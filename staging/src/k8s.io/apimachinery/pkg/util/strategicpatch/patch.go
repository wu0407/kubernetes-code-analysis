/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package strategicpatch

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/klog/v2"
)

// An alternate implementation of JSON Merge Patch
// (https://tools.ietf.org/html/rfc7386) which supports the ability to annotate
// certain fields with metadata that indicates whether the elements of JSON
// lists should be merged or replaced.
//
// For more information, see the PATCH section of docs/devel/api-conventions.md.
//
// Some of the content of this package was borrowed with minor adaptations from
// evanphx/json-patch and openshift/origin.

const (
	directiveMarker  = "$patch"
	deleteDirective  = "delete"
	replaceDirective = "replace"
	mergeDirective   = "merge"

	retainKeysStrategy = "retainKeys"

	deleteFromPrimitiveListDirectivePrefix = "$deleteFromPrimitiveList"
	retainKeysDirective                    = "$" + retainKeysStrategy
	setElementOrderDirectivePrefix         = "$setElementOrder"
)

// JSONMap is a representations of JSON object encoded as map[string]interface{}
// where the children can be either map[string]interface{}, []interface{} or
// primitive type).
// Operating on JSONMap representation is much faster as it doesn't require any
// json marshaling and/or unmarshaling operations.
type JSONMap map[string]interface{}

type DiffOptions struct {
	// SetElementOrder determines whether we generate the $setElementOrder parallel list.
	SetElementOrder bool
	// IgnoreChangesAndAdditions indicates if we keep the changes and additions in the patch.
	IgnoreChangesAndAdditions bool
	// IgnoreDeletions indicates if we keep the deletions in the patch.
	IgnoreDeletions bool
	// We introduce a new value retainKeys for patchStrategy.
	// It indicates that all fields needing to be preserved must be
	// present in the `retainKeys` list.
	// And the fields that are present will be merged with live object.
	// All the missing fields will be cleared when patching.
	BuildRetainKeysDirective bool
}

type MergeOptions struct {
	// MergeParallelList indicates if we are merging the parallel list.
	// We don't merge parallel list when calling mergeMap() in CreateThreeWayMergePatch()
	// which is called client-side.
	// We merge parallel list iff when calling mergeMap() in StrategicMergeMapPatch()
	// which is called server-side
	MergeParallelList bool
	// IgnoreUnmatchedNulls indicates if we should process the unmatched nulls.
	IgnoreUnmatchedNulls bool
}

// The following code is adapted from github.com/openshift/origin/pkg/util/jsonmerge.
// Instead of defining a Delta that holds an original, a patch and a set of preconditions,
// the reconcile method accepts a set of preconditions as an argument.

// CreateTwoWayMergePatch creates a patch that can be passed to StrategicMergePatch from an original
// document and a modified document, which are passed to the method as json encoded content. It will
// return a patch that yields the modified document when applied to the original document, or an error
// if either of the two documents is invalid.
func CreateTwoWayMergePatch(original, modified []byte, dataStruct interface{}, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	return CreateTwoWayMergePatchUsingLookupPatchMeta(original, modified, schema, fns...)
}

func CreateTwoWayMergePatchUsingLookupPatchMeta(
	original, modified []byte, schema LookupPatchMeta, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	originalMap := map[string]interface{}{}
	if len(original) > 0 {
		if err := json.Unmarshal(original, &originalMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	modifiedMap := map[string]interface{}{}
	if len(modified) > 0 {
		if err := json.Unmarshal(modified, &modifiedMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	patchMap, err := CreateTwoWayMergeMapPatchUsingLookupPatchMeta(originalMap, modifiedMap, schema, fns...)
	if err != nil {
		return nil, err
	}

	return json.Marshal(patchMap)
}

// CreateTwoWayMergeMapPatch creates a patch from an original and modified JSON objects,
// encoded JSONMap.
// The serialized version of the map can then be passed to StrategicMergeMapPatch.
func CreateTwoWayMergeMapPatch(original, modified JSONMap, dataStruct interface{}, fns ...mergepatch.PreconditionFunc) (JSONMap, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	return CreateTwoWayMergeMapPatchUsingLookupPatchMeta(original, modified, schema, fns...)
}

func CreateTwoWayMergeMapPatchUsingLookupPatchMeta(original, modified JSONMap, schema LookupPatchMeta, fns ...mergepatch.PreconditionFunc) (JSONMap, error) {
	diffOptions := DiffOptions{
		SetElementOrder: true,
	}
	patchMap, err := diffMaps(original, modified, schema, diffOptions)
	if err != nil {
		return nil, err
	}

	// Apply the preconditions to the patch, and return an error if any of them fail.
	for _, fn := range fns {
		if !fn(patchMap) {
			return nil, mergepatch.NewErrPreconditionFailed(patchMap)
		}
	}

	return patchMap, nil
}

// Returns a (recursive) strategic merge patch that yields modified when applied to original.
// Including:
// - Adding fields to the patch present in modified, missing from original
// - Setting fields to the patch present in modified and original with different values
// - Delete fields present in original, missing from modified through
// - IFF map field - set to nil in patch
// - IFF list of maps && merge strategy - use deleteDirective for the elements
// - IFF list of primitives && merge strategy - use parallel deletion list
// - IFF list of maps or primitives with replace strategy (default) - set patch value to the value in modified
// - Build $retainKeys directive for fields with retainKeys patch strategy
// 先进行遍历modified
//   modified中的key不在original中（说明是增加字段），如果IgnoreChangesAndAdditions为false（不忽略更新和增加字段），则将key和对应的modifiedValue保存到patch
//   modified中的key在original中
//     当key是"$patch"，且originalValue和modifiedValue都是string，（如果不都是string，则返回错误）
//       如果originalValue和modifiedValue不相等，则添加key为"$patch" value为modifiedValue到patch，继续下一个key
//     当originalValue和modifiedValue不是同一类型，即类型发生了变化
//       不忽略更新和增加，则将key和对应modifiedValue到patch中，继续下一个key
//     当originalValue和modifiedValue是同一类型
//       originalValue和modifiedValue是map[string]interface{}类型
//         如果"x-kubernetes-patch-strategy"里包含"replace"，且如果不忽略改变和增加字段，则添加key和对应的modifiedValue到patch中
//         否则，继续对（key的值）的map[string]interface{}进行diff（就是递归调用diffMaps），如果originalValue和modifiedValue不相等（有差异），则添加key和对应patchValue到patch中
//       originalValue和modifiedValue是[]interface{}类型
//         如果"x-kubernetes-patch-strategy"里包含"merge"，则根据"x-kubernetes-patch-merge-key"进行merge
//           original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则没有任何patch，不需要继续执行
//           original为空（说明只有增加字段）且不忽略更新和增加字段，则patchList为modified，nil的删除的列表，setOrderList为nil，不需要继续执行
//           slice元素类型为map
//             根据map的key为mergeKey的值，进行map排序
//             根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item，获得增加或更新的列表，移除的列表
//             元素为map[string]interface{}
//             SetElementOrder为true，且（
//               不忽略更新增加字段且（存在差异（patchList大于0）或顺序不一样（不存在差异））
//               或（不忽略删除（一定忽略更新增加字段）且存在差异（patchList大于0））
//             ）
//             setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值，按照modified里顺序
//             获得patchList（更新增加和删除列表（按照modified中mergeKey对应的相同值的顺序），删除的元素在最后面），nil的删除的列表，setOrderList
//           slice元素类型为slice，返回错误
//           slice元素为其他类型（元素类型是Primitive）
//             将两个list按照元素的%v的字符排序
//             比较两个list，返回两个list，第一个是patchList（增加的元素），第二个是deleteList（删除的元素）
//             按照modified中的顺序，来排序patchList
//             SetElementOrder为true且
//               不忽略删除字段，且有删除的元素（len(deleteList)大于0）
//               或不忽略更新增加字段，且original和modified不相同
//             则setOrderList为modified
//             获得patchList（按照modified中的顺序），deleteList（删除的元素），setOrderList
//          将key为key，value为patchList添加到patch（patch[key] = patchList）
//          当元素类型是Primitive，将key为"$deleteFromPrimitiveList/{key}"，value为deleteList添加到patch
//          将key为"$setElementOrder/{key}"，value为setOrderList
//        x-kubernetes-patch-strategy"里不包含"merge"，则使用替换策略，即modifiedValue替换originalValue
//            不忽略更新和增加字段，且original和modified不相等，添加key和value为modified到patch
//       其他类型Primitive
//         不忽略更新和增加字段，且original和modified不相等，添加key和value为modified到patch
// diffOptions不忽略删除，则遍历original里的所有key，如果key不在modified，则设置patch里的key为key，value为nil（说明是删除字段）
// retainKeysList保存的是modified里值不为nil的key（且BuildRetainKeysDirective为true），它是用来处理删除字段
// retainKeysList不为空，且（original和modified存在差异，或original里有key的值不为nil，且不在modified里）
//   retainKeysList按照元素的%v的字符排序，然后保存到patch["$retainKeys"]
// 返回patch
func diffMaps(original, modified map[string]interface{}, schema LookupPatchMeta, diffOptions DiffOptions) (map[string]interface{}, error) {
	patch := map[string]interface{}{}

	// This will be used to build the $retainKeys directive sent in the patch
	retainKeysList := make([]interface{}, 0, len(modified))

	// Compare each value in the modified map against the value in the original map
	for key, modifiedValue := range modified {
		// Get the underlying type for pointers
		// 这个会在map[string]interface{}里的interface也是map[string]interface{}，即map[string]interface{}里嵌套了map[string]interface{}。
		// 或map[string]interface{}里的interface是[]interface{}
		// 且"x-kubernetes-patch-strategy"里包含"retainKeys"，设置为true
		// 如果BuildRetainKeysDirective为true，且modifiedValue不为nil（这个key不是要删除的），则将key append到retainKeysList
		if diffOptions.BuildRetainKeysDirective && modifiedValue != nil {
			retainKeysList = append(retainKeysList, key)
		}

		originalValue, ok := original[key]
		// modified中的key不在original中（说明是增加字段）
		if !ok {
			// Key was added, so add to patch
			// 如果IgnoreChangesAndAdditions为false（不忽略更新和增加字段），则将key和对应的modifiedValue保存到patch
			if !diffOptions.IgnoreChangesAndAdditions {
				patch[key] = modifiedValue
			}
			continue
		}

		// modified中的key在original中

		// The patch may have a patch directive
		// TODO: figure out if we need this. This shouldn't be needed by apply. When would the original map have patch directives in it?
		// 当key是"$patch"，且originalValue和modifiedValue都是string，（如果不都是string，则返回错误）
		// 如果originalValue和modifiedValue不相等，则添加key为"$patch" value为modifiedValue到patch，返回true, nil
		// 否则返回false, nil
		foundDirectiveMarker, err := handleDirectiveMarker(key, originalValue, modifiedValue, patch)
		if err != nil {
			return nil, err
		}
		// 当key是"$patch"，则继续下一个key
		if foundDirectiveMarker {
			continue
		}

		// originalValue和modifiedValue不是同一类型，即类型发生了变化
		if reflect.TypeOf(originalValue) != reflect.TypeOf(modifiedValue) {
			// Types have changed, so add to patch
			// 不忽略更新和增加，则将key和对应modifiedValue到patch中
			if !diffOptions.IgnoreChangesAndAdditions {
				patch[key] = modifiedValue
			}
			continue
		}

		// Types are the same, so compare values
		// originalValue和modifiedValue是同一类型
		switch originalValueTyped := originalValue.(type) {
		// originalValue和modifiedValue是map[string]interface{}类型
		case map[string]interface{}:
			modifiedValueTyped := modifiedValue.(map[string]interface{})
			// 从schema.schema中查找key字段对应的openapi.Schema，再从openapi.Schema里extensions里获得的"x-kubernetes-patch-strategy"的值
			// 如果"x-kubernetes-patch-strategy"里包含"replace"，且如果不忽略改变和增加字段，则添加key和对应的modifiedValue到patch中
			// 否则，继续对（key的值）的map[string]interface{}进行diff（就是递归调用diffMaps），如果originalValue和modifiedValue不相等（有差异），则添加key和对应patchValue到patch中
			err = handleMapDiff(key, originalValueTyped, modifiedValueTyped, patch, schema, diffOptions)
		// originalValue和modifiedValue是[]interface{}类型
		case []interface{}:
			modifiedValueTyped := modifiedValue.([]interface{})
			// 从schema（group version kind对应的schema）中获得key字段对应的LookupPatchMeta（用于查找key内部的字段或元素类型对应的Schema）和PatchMeta
			// 根据PatchMeta里的"x-kubernetes-patch-strategy"里是否包含"merge"，进行不同的diff
			// 如果"x-kubernetes-patch-strategy"里包含"merge"，则根据"x-kubernetes-patch-merge-key"进行merge
			//   original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则返回nil
			//   original为空（说明只有增加字段）且不忽略更新和增加字段，则返回modified，nil, nil, nil
			//   slice元素类型为map
			//     根据map的key为mergeKey的值，进行map排序
			//     根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item，获得增加或更新的列表，移除的列表
			//     元素为map[string]interface{}
			//     SetElementOrder为true，且（
			//       不忽略更新增加字段且（存在差异（patchList大于0）或顺序不一样（不存在差异））
			//       或（不忽略删除（一定忽略更新增加字段）且存在差异（patchList大于0））
			//     ）
			//     setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值，按照modified里顺序
			//     获得patchList（更新增加和删除列表（按照modified中mergeKey对应的相同值的顺序），删除的元素在最后面），nil的删除的列表，setOrderList
			//   slice元素类型为slice，返回错误
			//   slice元素为其他类型（元素类型是Primitive）
			//     将两个list按照元素的%v的字符排序
			//     比较两个list，返回两个list，第一个是patchList（增加的元素），第二个是deleteList（删除的元素）
			//     按照modified中的顺序，来排序patchList
			//     SetElementOrder为true且
			//       不忽略删除字段，且有删除的元素（len(deleteList)大于0）
			//       或不忽略更新增加字段，且original和modified不相同
			//     则setOrderList为modified
			//     获得patchList（按照modified中的顺序），deleteList（删除的元素），setOrderList
			// 将key为key，value为patchList添加到patch（patch[key] = patchList）
			// 当元素类型是Primitive，将key为"$deleteFromPrimitiveList/{key}"，value为deleteList添加到patch
			// 将key为"$setElementOrder/{key}"，value为setOrderList
			// "x-kubernetes-patch-strategy"里不包含"merge"，则使用替换策略，即modifiedValue替换originalValue
			//    忽略更新和增加字段，则直接返回
			//    original和modified相等，则返回
			//    否则，添加key和value为modified到patch
			err = handleSliceDiff(key, originalValueTyped, modifiedValueTyped, patch, schema, diffOptions)
		default:
			// 其他类型Primitive
			// 忽略更新和增加字段，则直接返回
			// original和modified相等，则返回
			// 否则，添加key为key和value为modified到patch
			replacePatchFieldIfNotEqual(key, originalValue, modifiedValue, patch, diffOptions)
		}
		if err != nil {
			return nil, err
		}
	}

	// diffOptions不忽略删除，且
	// 将遍历original里的所有key，如果key不在modified，则设置patch里的key为key，value为nil（说明是删除字段）
	// 因为上面是按照modified进行遍历的，所以可能存在original但是不在modified的key
	updatePatchIfMissing(original, modified, patch, diffOptions)
	// Insert the retainKeysList iff there are values present in the retainKeysList and
	// either of the following is true:
	// - the patch is not empty
	// - there are additional field in original that need to be cleared
	// retainKeysList不为空，且（original和modified存在差异，或original里有key的值不为nil，且不在modified里）
	// retainKeysList保存的是modified里值不为nil的key，它是用来处理删除字段
	if len(retainKeysList) > 0 &&
		// original里有key的值不为nil，且不在modified里，则返回true
		// original里有key的值为nil，且不在modified里，就相当于original里没有这个key
		(len(patch) > 0 || hasAdditionalNewField(original, modified)) {
		// retainKeysList按照元素的%v的字符排序
		// 保存到patch["$retainKeys"]
		patch[retainKeysDirective] = sortScalars(retainKeysList)
	}
	return patch, nil
}

// handleDirectiveMarker handles how to diff directive marker between 2 objects
// 当key是"$patch"，且originalValue和modifiedValue都是string，（如果不都是string，则返回错误）
// 如果originalValue和modifiedValue不相等，则添加key为"$patch" value为modifiedValue到patch，返回true, nil
// 否则返回false, nil
func handleDirectiveMarker(key string, originalValue, modifiedValue interface{}, patch map[string]interface{}) (bool, error) {
	// 如果key是"$patch"
	if key == directiveMarker {
		// originalValue必须是string
		originalString, ok := originalValue.(string)
		if !ok {
			return false, fmt.Errorf("invalid value for special key: %s", directiveMarker)
		}
		// modifiedValue必须是string
		modifiedString, ok := modifiedValue.(string)
		if !ok {
			return false, fmt.Errorf("invalid value for special key: %s", directiveMarker)
		}
		// 如果value不一样，则添加key为"$patch" value为modifiedValue到patch
		if modifiedString != originalString {
			patch[directiveMarker] = modifiedValue
		}
		return true, nil
	}
	return false, nil
}

// handleMapDiff diff between 2 maps `originalValueTyped` and `modifiedValue`,
// puts the diff in the `patch` associated with `key`
// key is the key associated with originalValue and modifiedValue.
// originalValue, modifiedValue are the old and new value respectively.They are both maps
// patch is the patch map that contains key and the updated value, and it is the parent of originalValue, modifiedValue
// diffOptions contains multiple options to control how we do the diff.
// 从schema.schema中查找key字段对应的openapi.Schema，再从openapi.Schema里extensions里获得的"x-kubernetes-patch-strategy"的值
// 如果"x-kubernetes-patch-strategy"里包含"replace"，且如果不忽略改变和增加字段，则添加key和对应的modifiedValue到patch中
// 否则，继续对下一层级（key的值）的map[string]interface{}进行diff，如果originalValue和modifiedValue不相等（有差异），则添加key和对应patchValue到patch中
func handleMapDiff(key string, originalValue, modifiedValue, patch map[string]interface{},
	schema LookupPatchMeta, diffOptions DiffOptions) error {
	// 这里的schema是PatchMetaFromOpenAPI（在staging\src\k8s.io\apimachinery\pkg\util\strategicpatch\meta.go）
	// 从schema.schema中查找key字段对应的openapi.Schema，设置为返回的第一个字段(PatchMetaFromOpenAPI{Schema: {key字段对应的openapi.Schema}})
	// 从openapi.Schema里extensions里获得的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，设置返回的第二个字段PatchMeta的patchMergeKey和patchStrategies字段
	subschema, patchMeta, err := schema.LookupPatchMetadataForStruct(key)

	if err != nil {
		// We couldn't look up metadata for the field
		// If the values are identical, this doesn't matter, no patch is needed
		if reflect.DeepEqual(originalValue, modifiedValue) {
			return nil
		}
		// Otherwise, return the error
		return err
	}
	// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
	// 当strategies的数量大于等于3个，则返回错误
	// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
	// 目前已见到的"x-kubernetes-patch-strategy"的字段值为"merge"、"replace"、"retainKeys"、"merge,retainKeys"
	retainKeys, patchStrategy, err := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
	if err != nil {
		return err
	}
	diffOptions.BuildRetainKeysDirective = retainKeys
	switch patchStrategy {
	// The patch strategic from metadata tells us to replace the entire object instead of diffing it
	// 如果"x-kubernetes-patch-strategy"里包含"replace"，且如果不忽略改变和增加字段，则添加key和对应的modifiedValue到patch中
	case replaceDirective:
		if !diffOptions.IgnoreChangesAndAdditions {
			patch[key] = modifiedValue
		}
	// "x-kubernetes-patch-strategy"里不包含"replace"（比如"x-kubernetes-patch-strategy"为空，或只包含"retainKeys"，或只包含非"retainKeys"）
	default:
		// 继续对下一层级（key的值）的map[string]interface{}进行diff
		patchValue, err := diffMaps(originalValue, modifiedValue, subschema, diffOptions)
		if err != nil {
			return err
		}
		// Maps were not identical, use provided patch value
		// originalValue和modifiedValue不相等（有差异），则添加key和对应patchValue到patch中
		if len(patchValue) > 0 {
			patch[key] = patchValue
		}
	}
	return nil
}

// handleSliceDiff diff between 2 slices `originalValueTyped` and `modifiedValue`,
// puts the diff in the `patch` associated with `key`
// key is the key associated with originalValue and modifiedValue.
// originalValue, modifiedValue are the old and new value respectively.They are both slices
// patch is the patch map that contains key and the updated value, and it is the parent of originalValue, modifiedValue
// diffOptions contains multiple options to control how we do the diff.
// 从schema（group version kind对应的schema）中获得key字段对应的LookupPatchMeta（用于查找key内部的字段或元素类型对应的Schema）和PatchMeta
// 根据PatchMeta里的"x-kubernetes-patch-strategy"里是否包含"merge"，进行不同的diff
// 如果"x-kubernetes-patch-strategy"里包含"merge"，则根据"x-kubernetes-patch-merge-key"进行merge
//   original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则返回nil
//   original为空（说明只有增加字段）且不忽略更新和增加字段，则返回modified，nil, nil, nil
//   slice元素类型为map
//     根据map的key为mergeKey的值，进行map排序
//     根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item，获得增加或更新的列表，移除的列表
//     元素为map[string]interface{}
//     SetElementOrder为true，且（
//       不忽略更新增加字段且（存在差异（patchList大于0）或顺序不一样（不存在差异））
//       或（不忽略删除（一定忽略更新增加字段）且存在差异（patchList大于0））
//     ）
//     setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值，按照modified里顺序
//     获得patchList（更新增加和删除列表（按照modified中mergeKey对应的相同值的顺序），删除的元素在最后面），nil的删除的列表，setOrderList
//   slice元素类型为slice，返回错误
//   slice元素为其他类型（元素类型是Primitive）
//     将两个list按照元素的%v的字符排序
//     比较两个list，返回两个list，第一个是patchList（增加的元素），第二个是deleteList（删除的元素）
//     按照modified中的顺序，来排序patchList
//     SetElementOrder为true且
//       不忽略删除字段，且有删除的元素（len(deleteList)大于0）
//       或不忽略更新增加字段，且original和modified不相同
//     则setOrderList为modified
//     获得patchList（按照modified中的顺序），deleteList（删除的元素），setOrderList
//    将key为key，value为patchList添加到patch（patch[key] = patchList）
//    当元素类型是Primitive，将key为"$deleteFromPrimitiveList/{key}"，value为deleteList添加到patch
//    将key为"$setElementOrder/{key}"，value为setOrderList
// "x-kubernetes-patch-strategy"里不包含"merge"，则使用替换策略，即modifiedValue替换originalValue
//    忽略更新和增加字段，则直接返回
//    original和modified相等，则返回
//    否则，添加key和value为modified到patch
func handleSliceDiff(key string, originalValue, modifiedValue []interface{}, patch map[string]interface{},
	schema LookupPatchMeta, diffOptions DiffOptions) error {
	// 这里的schema是PatchMetaFromOpenAPI（在staging\src\k8s.io\apimachinery\pkg\util\strategicpatch\meta.go）
	// 返回slice的item类型Schema，组装成第一个返回值PatchMetaFromOpenAPI{Schema: sliceItem.subschema}
	// s.Schema类型是proto.*Kind，则调用sliceItem.VisitKind(s.Schema)
	//     从s.Schema中查找sliceItem.key字段对应Schema
	//     从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成第二个返回值PatchMeta
	// 否则，第二个参数为PatchMeta{}（空的PatchMeta）
	subschema, patchMeta, err := schema.LookupPatchMetadataForSlice(key)
	if err != nil {
		// We couldn't look up metadata for the field
		// If the values are identical, this doesn't matter, no patch is needed
		if reflect.DeepEqual(originalValue, modifiedValue) {
			return nil
		}
		// Otherwise, return the error
		return err
	}
	// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
	// 当strategies的数量大于等于3个，则返回错误
	// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
	retainKeys, patchStrategy, err := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
	if err != nil {
		return err
	}
	switch patchStrategy {
	// Merge the 2 slices using mergePatchKey
	// "x-kubernetes-patch-strategy"里包含"merge"
	case mergeDirective:
		diffOptions.BuildRetainKeysDirective = retainKeys
		// original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则返回nil
		// original为空（说明只有增加字段）且不忽略更新和增加字段，则返回modified，nil, nil, nil
		// slice元素类型为map
		//   根据map的key为mergeKey的值，进行map排序
		//   根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item，获得增加或更新的列表，移除的列表
		//   元素为map[string]interface{}
		//   SetElementOrder为true，且（
		//     不忽略更新增加字段且（存在差异（patchList大于0）或顺序不一样（不存在差异））
		//     或（不忽略删除（一定忽略更新增加字段）且存在差异（patchList大于0））
		//   ）
		//   setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值，按照modified里顺序
		//   返回patch（更新增加和删除列表（按照modified中mergeKey对应的相同值的顺序），删除的元素在最后面），nil的删除的列表，setOrderList
		// slice元素类型为slice，返回错误
		// slice元素为其他类型
		//   将两个list按照元素的%v的字符排序
		//   比较两个list，返回两个list，第一个是patchList（增加的元素），第二个是deleteList（删除的元素）
		//   按照modified中的顺序，来排序patchList
		//   SetElementOrder为true且
		//     不忽略删除字段，且有删除的元素（len(deleteList)大于0）
		//     或不忽略更新增加字段，且original和modified不相同
		//   则setOrderList为modified
		//  返回patchList（按照modified中的顺序），deleteList（删除的元素），setOrderList
		addList, deletionList, setOrderList, err := diffLists(originalValue, modifiedValue, subschema, patchMeta.GetPatchMergeKey(), diffOptions)
		if err != nil {
			return err
		}
		if len(addList) > 0 {
			patch[key] = addList
		}
		// generate a parallel list for deletion
		// len(deletionList)大于0，只有元素类型是Primitive
		if len(deletionList) > 0 {
			// "$deleteFromPrimitiveList/{key}"
			parallelDeletionListKey := fmt.Sprintf("%s/%s", deleteFromPrimitiveListDirectivePrefix, key)
			patch[parallelDeletionListKey] = deletionList
		}
		if len(setOrderList) > 0 {
			// "$setElementOrder/{key}"
			parallelSetOrderListKey := fmt.Sprintf("%s/%s", setElementOrderDirectivePrefix, key)
			patch[parallelSetOrderListKey] = setOrderList
		}
	// "x-kubernetes-patch-strategy"里不包含"merge"
	// 比如"x-kubernetes-patch-strategy": "retainKeys"
	// "x-kubernetes-patch-strategy": "replace"
	// 或没有"x-kubernetes-patch-strategy"
	default:
		// 忽略更新和增加字段，则直接返回
		// original和modified相等，则返回
		// 否则，添加key和value为modified到patch
		replacePatchFieldIfNotEqual(key, originalValue, modifiedValue, patch, diffOptions)
	}
	return nil
}

// replacePatchFieldIfNotEqual updates the patch if original and modified are not deep equal
// if diffOptions.IgnoreChangesAndAdditions is false.
// original is the old value, maybe either the live cluster object or the last applied configuration
// modified is the new value, is always the users new config
// 忽略更新和增加字段，则直接返回
// original和modified相等，则返回
// 否则，添加key和value为modified到patch
func replacePatchFieldIfNotEqual(key string, original, modified interface{},
	patch map[string]interface{}, diffOptions DiffOptions) {
	// 忽略更新和增加字段，则直接返回
	if diffOptions.IgnoreChangesAndAdditions {
		// Ignoring changes - do nothing
		return
	}
	// original和modified相等，则返回
	if reflect.DeepEqual(original, modified) {
		// Contents are identical - do nothing
		return
	}
	// Create a patch to replace the old value with the new one
	patch[key] = modified
}

// updatePatchIfMissing iterates over `original` when ignoreDeletions is false.
// Clear the field whose key is not present in `modified`.
// original is the old value, maybe either the live cluster object or the last applied configuration
// modified is the new value, is always the users new config
// diffOptions忽略删除，直接返回
// 将遍历original里的所有key，如果key不在modified，则设置patch里的key为key，value为nil
func updatePatchIfMissing(original, modified, patch map[string]interface{}, diffOptions DiffOptions) {
	// 忽略删除，直接返回
	if diffOptions.IgnoreDeletions {
		// Ignoring deletion - do nothing
		return
	}
	// Add nils for deleted values
	// original里的key不在modified，则设置patch里的key为key，value为nil
	for key := range original {
		if _, found := modified[key]; !found {
			patch[key] = nil
		}
	}
}

// validateMergeKeyInLists checks if each map in the list has the mentryerge key.
// 遍历lists，在每个list元素中查找是否包含mergeKey
// list元素不是map[string]interface{}，返回错误
// 所有list的元素不包含mergeKey，返回错误
// 所有list的元素包含mergeKey，返回nil
func validateMergeKeyInLists(mergeKey string, lists ...[]interface{}) error {
	for _, list := range lists {
		for _, item := range list {
			m, ok := item.(map[string]interface{})
			if !ok {
				return mergepatch.ErrBadArgType(m, item)
			}
			if _, ok = m[mergeKey]; !ok {
				return mergepatch.ErrNoMergeKey(m, mergeKey)
			}
		}
	}
	return nil
}

// normalizeElementOrder sort `patch` list by `patchOrder` and sort `serverOnly` list by `serverOrder`.
// Then it merges the 2 sorted lists.
// It guarantee the relative order in the patch list and in the serverOnly list is kept.
// `patch` is a list of items in the patch, and `serverOnly` is a list of items in the live object.
// `patchOrder` is the order we want `patch` list to have and
// `serverOrder` is the order we want `serverOnly` list to have.
// kind is the kind of each item in the lists `patch` and `serverOnly`.
// 先将patch按照patchOrder进行排序，serverOnly按照serverOrder进行排序
// 然后将两个排序后的list进行合并
// 插入的顺序是（元素类型是map[string]interface{}）按照在serverOrder中mergeKey对应的一样值的位置，或（元素类型是其他类型）在serverOrder中位置
// 如果都在serverOrder里，则谁在前面，进行先插入
// 否则，patch元素先插入。
func normalizeElementOrder(patch, serverOnly, patchOrder, serverOrder []interface{}, mergeKey string, kind reflect.Kind) ([]interface{}, error) {
	// kind为map
	//   将patch里不删除的（key为"$patch"且value为"delete"以外的元素）元素先进行排序（按照patchOrder中mergeKey对应的相同值的顺序），然后将删除的元素（包含key为"$patch"且value为"delete"）添加到后面（按照原来的顺序）
	// kind为其他类型
	//   按照patchOrder中的顺序，来排序patch
	patch, err := normalizeSliceOrder(patch, patchOrder, mergeKey, kind)
	if err != nil {
		return nil, err
	}
	klog.Infof("after normalize patch: %v, patchOrder: %v", patch, patchOrder)
	serverOnly, err = normalizeSliceOrder(serverOnly, serverOrder, mergeKey, kind)
	if err != nil {
		return nil, err
	}
	klog.Infof("after normalize serverOnly: %v, serverOrder: %v", serverOnly, serverOrder)
	// 将serverOnly和patch进行合并
	// 插入的顺序是（元素类型是map[string]interface{}）按照在serverOrder中mergeKey对应的一样值的位置，或（元素类型是其他类型）在serverOrder中位置
	// 如果都在在serverOrder里，则谁在前面，进行插入
	// 否则，patch元素先插入。
	all := mergeSortedSlice(serverOnly, patch, serverOrder, mergeKey, kind)

	return all, nil
}

// mergeSortedSlice merges the 2 sorted lists by serverOrder with best effort.
// It will insert each item in `left` list to `right` list. In most cases, the 2 lists will be interleaved.
// The relative order of left and right are guaranteed to be kept.
// They have higher precedence than the order in the live list.
// The place for a item in `left` is found by:
// scan from the place of last insertion in `right` to the end of `right`,
// the place is before the first item that is greater than the item we want to insert.
// example usage: using server-only items as left and patch items as right. We insert server-only items
// to patch list. We use the order of live object as record for comparison.
// 将left和right进行合并
// 插入的顺序是（元素类型是map[string]interface{}）按照在serverOrder中mergeKey对应的一样值的位置，或（元素类型是其他类型）在serverOrder中位置
// 如果都在在serverOrder里，则谁在前面，谁先插入
// 否则，right先插入。
func mergeSortedSlice(left, right, serverOrder []interface{}, mergeKey string, kind reflect.Kind) []interface{} {
	// Returns if l is less than r, and if both have been found.
	// If l and r both present and l is in front of r, l is less than r.
	less := func(l, r interface{}) (bool, bool) {
		// kind是map类型
		//   遍历serverOrder中元素（map[string]interface{}）查找mergeKey对应的值，与l（map[string]interface{}）中查找mergeKey对应一样的值，则返回这时的下标
		//   如果没有找到值一样的，则返回-1
		// kind是其他类型
		//   遍历serverOrder中元素，找到与l值一样的元素的下标，如果找不到则返回-1
		li := index(serverOrder, l, mergeKey, kind)
		ri := index(serverOrder, r, mergeKey, kind)
		// 两个都在serverOrder里，则比较两个位置，返回l位置是否小于r的位置，true（都在serverOrder里）
		if li >= 0 && ri >= 0 {
			return li < ri, true
		} else {
			// 两个不都在serverOrder里，返回false，false
			return false, false
		}
	}

	// left and right should be non-overlapping.
	size := len(left) + len(right)
	i, j := 0, 0
	s := make([]interface{}, size, size)

	for k := 0; k < size; k++ {
		// left已经遍历完，且right未遍历完
		if i >= len(left) && j < len(right) {
			// have items left in `right` list
			// 保存right元素
			s[k] = right[j]
			j++
		// left未遍历完，且right已经遍历完
		} else if j >= len(right) && i < len(left) {
			// have items left in `left` list
			// 保存left元素
			s[k] = left[i]
			i++
		} else {
			// compare them if i and j are both in bound
			// left和right都未遍历完
			// 比较两个元素
			less, foundBoth := less(left[i], right[j])
			// 两个都在serverOrder里，且left[i]在right[j]前面
			if foundBoth && less {
				// 保存left元素
				s[k] = left[i]
				i++
			} else {
				// 否则right元素
				s[k] = right[j]
				j++
			}
		}
	}
	return s
}

// index returns the index of the item in the given items, or -1 if it doesn't exist
// l must NOT be a slice of slices, this should be checked before calling.
// kind是map类型
//   遍历l中元素（map[string]interface{}）查找mergeKey对应的值，与valToLookUp中（map[string]interface{}）查找mergeKey对应的值一样，则返回这时的下标
//   如果没有找到值一样的，则返回-1
// kind是其他类型
//   遍历l中元素，找到与valToLookUp值一样的元素的下标，如果找不到则返回-1
func index(l []interface{}, valToLookUp interface{}, mergeKey string, kind reflect.Kind) int {
	var getValFn func(interface{}) interface{}
	// Get the correct `getValFn` based on item `kind`.
	// It should return the value of merge key for maps and
	// return the item for other kinds.
	switch kind {
	case reflect.Map:
		// 返回mergeKey对应的值
		getValFn = func(item interface{}) interface{} {
			typedItem, ok := item.(map[string]interface{})
			if !ok {
				return nil
			}
			val := typedItem[mergeKey]
			return val
		}
	default:
		getValFn = func(item interface{}) interface{} {
			return item
		}
	}

	for i, v := range l {
		if getValFn(valToLookUp) == getValFn(v) {
			return i
		}
	}
	return -1
}

// extractToDeleteItems takes a list and
// returns 2 lists: one contains items that should be kept and the other contains items to be deleted.
// 第二个返回值 slice里存在key为"$patch"且value为"delete"，则这个元素（map[string]interface{}）为要被删除的
// 第一个返回值 剩下的为需要保留的
func extractToDeleteItems(l []interface{}) ([]interface{}, []interface{}, error) {
	var nonDelete, toDelete []interface{}
	for _, v := range l {
		m, ok := v.(map[string]interface{})
		if !ok {
			return nil, nil, mergepatch.ErrBadArgType(m, v)
		}

		// key为"$patch"是否存在m里
		directive, foundDirective := m[directiveMarker]
		// 存在且value为"delete"，则将v添加toDelete
		if foundDirective && directive == deleteDirective {
			toDelete = append(toDelete, v)
		} else {
			// 不存在，或value不为"delete"
			nonDelete = append(nonDelete, v)
		}
	}
	return nonDelete, toDelete, nil
}

// normalizeSliceOrder sort `toSort` list by `order`
// kind为map
//   将toSort里不删除的（key为"$patch"且value为"delete"以外的元素）元素先进行排序（按照order中mergeKey对应的相同值的顺序），然后将删除的元素（包含key为"$patch"且value为"delete"）添加到后面（按照原来的顺序）
// kind为其他类型
//   按照order中的顺序，来排序toSort
func normalizeSliceOrder(toSort, order []interface{}, mergeKey string, kind reflect.Kind) ([]interface{}, error) {
	var toDelete []interface{}
	if kind == reflect.Map {
		// make sure each item in toSort, order has merge key
		// 遍历toSort, order，在每个元素中查找是否包含mergeKey
		// list不是map[string]interface{}，返回错误
		// 所有list不包含mergeKey，返回错误
		// 所有list包含mergeKey，返回nil
		err := validateMergeKeyInLists(mergeKey, toSort, order)
		if err != nil {
			return nil, err
		}
		// slice里的key为"$patch"且value为"delete"，则这个元素（map[string]interface{}）为要被删除的
		// 剩下的为需要保留的
		toSort, toDelete, err = extractToDeleteItems(toSort)
		if err != nil {
			return nil, err
		}
	}

	// 按照order中的顺序，来排序toSort
	sort.SliceStable(toSort, func(i, j int) bool {
		// 遍历order中元素（map[string]interface{}）查找mergeKey对应的值，如果与toSort[i]中查找mergeKey对应的值一样，则返回这时的下标
		// 如果没有找到值一样的，则返回-1
		if ii := index(order, toSort[i], mergeKey, kind); ii >= 0 {
			if ij := index(order, toSort[j], mergeKey, kind); ij >= 0 {
				return ii < ij
			}
		}
		return true
	})
	toSort = append(toSort, toDelete...)
	return toSort, nil
}

// Returns a (recursive) strategic merge patch, a parallel deletion list if necessary and
// another list to set the order of the list
// Only list of primitives with merge strategy will generate a parallel deletion list.
// These two lists should yield modified when applied to original, for lists with merge semantics.
// original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则返回nil
// original为空（说明只有增加字段）且不忽略更新和增加字段，则返回modified，nil, nil, nil
// slice元素类型为map
//   根据map的key为mergeKey的值，进行map排序
//   根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item
//   返回增加或更新的列表，移除的列表
//   元素为map[string]interface{}
//   SetElementOrder为true，且（
//     不忽略更新增加字段且（存在差异（patchList大于0）或顺序不一样（不存在差异））
//     或（不忽略删除（一定忽略更新增加字段）且存在差异（patchList大于0））
//   ）
//   setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值，按照modified里顺序
//   返回patch（更新增加和删除列表（按照modified中mergeKey对应的相同值的顺序），删除的元素在最后面），nil的删除的列表，setOrderList
// slice元素类型为slice，返回错误
// slice元素为其他类型
//   将两个list按照元素的%v的字符排序
//   比较两个list，返回两个list，第一个是patchList（增加的元素），第二个是deleteList（删除的元素）
//   按照modified中的顺序，来排序patchList
//   SetElementOrder为true且
//     不忽略删除字段，且有删除的元素（len(deleteList)大于0）
//     或不忽略更新增加字段，且original和modified不相同
//   则setOrderList为modified
//  返回patchList（按照modified中的顺序），deleteList（删除的元素），setOrderList
func diffLists(original, modified []interface{}, schema LookupPatchMeta, mergeKey string, diffOptions DiffOptions) ([]interface{}, []interface{}, []interface{}, error) {
	if len(original) == 0 {
		// Both slices are empty - do nothing
		// original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则返回nil
		if len(modified) == 0 || diffOptions.IgnoreChangesAndAdditions {
			return nil, nil, nil, nil
		}

		// Old slice was empty - add all elements from the new slice
		// original为空（说明只有增加字段）且不忽略更新和增加字段，则返回modified
		return modified, nil, nil, nil
	}

	// 检测original和modified的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误
	// 否则返回slice内部元素类型
	elementType, err := sliceElementType(original, modified)
	if err != nil {
		return nil, nil, nil, err
	}

	var patchList, deleteList, setOrderList []interface{}
	kind := elementType.Kind()
	switch kind {
	// slice元素类型为map
	case reflect.Map:
		// 根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item
		// 返回增加或更新的列表，移除的列表
		// 元素为map[string]interface{}
		patchList, deleteList, err = diffListsOfMaps(original, modified, schema, mergeKey, diffOptions)
		if err != nil {
			return nil, nil, nil, err
		}
		// 将patchList里不删除的（key为"$patch"且value为"delete"以外的元素）元素先进行排序（按照modified中mergeKey对应的相同值的顺序），然后将删除的元素（包含key为"$patch"且value为"delete"）添加到后面（按照原来的顺序）
		patchList, err = normalizeSliceOrder(patchList, modified, mergeKey, kind)
		if err != nil {
			return nil, nil, nil, err
		}
		// 长度一样，且相同index的每个元素里的mergeKey的值一样，返回true
		// 否则返回false
		orderSame, err := isOrderSame(original, modified, mergeKey)
		if err != nil {
			return nil, nil, nil, err
		}
		// append the deletions to the end of the patch list.
		patchList = append(patchList, deleteList...)
		deleteList = nil
		// generate the setElementOrder list when there are content changes or order changes
		// SetElementOrder为true，且（
		//   不忽略更新增加字段且（存在差异（patchList大于0）或顺序不一样（不存在差异））
		//   或（不忽略删除（一定忽略更新增加字段）且存在差异（patchList大于0））
		// ）
		// setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值，按照modified里顺序
		if diffOptions.SetElementOrder &&
			((!diffOptions.IgnoreChangesAndAdditions && (len(patchList) > 0 || !orderSame)) ||
				(!diffOptions.IgnoreDeletions && len(patchList) > 0)) {
			// Generate a list of maps that each item contains only the merge key.
			setOrderList = make([]interface{}, len(modified))
			for i, v := range modified {
				typedV := v.(map[string]interface{})
				// setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值（只有一个key mergeKey）
				setOrderList[i] = map[string]interface{}{
					mergeKey: typedV[mergeKey],
				}
			}
		}
	// 不支持嵌套slice类型，返回错误
	case reflect.Slice:
		// Lists of Lists are not permitted by the api
		return nil, nil, nil, mergepatch.ErrNoListOfLists
	default:
		// 将两个list按照元素的%v的字符排序
		// 比较两个list，返回两个list，第一个是addList（增加的元素），第二个是deleteList（删除的元素）
		patchList, deleteList, err = diffListsOfScalars(original, modified, diffOptions)
		if err != nil {
			return nil, nil, nil, err
		}
		// 按照modified中的顺序，来排序patchList
		// 比如original为[1,3]，modified为[1,4,3,2]，原来的patchList为[2,4]（先排序在比较），改回原来的顺序为[4,2]
		patchList, err = normalizeSliceOrder(patchList, modified, mergeKey, kind)
		// generate the setElementOrder list when there are content changes or order changes
		// SetElementOrder为true且
		//   不忽略删除字段，且有删除的元素（len(deleteList)大于0）
		//   或不忽略更新增加字段，且original和modified不相同
		// 则setOrderList为modified
		if diffOptions.SetElementOrder && ((!diffOptions.IgnoreDeletions && len(deleteList) > 0) ||
			(!diffOptions.IgnoreChangesAndAdditions && !reflect.DeepEqual(original, modified))) {
			setOrderList = modified
		}
	}
	return patchList, deleteList, setOrderList, err
}

// isOrderSame checks if the order in a list has changed
// 长度一样，且相同index的每个元素里的mergeKey的值一样，返回true
// 否则返回false
func isOrderSame(original, modified []interface{}, mergeKey string) (bool, error) {
	if len(original) != len(modified) {
		return false, nil
	}
	for i, modifiedItem := range modified {
		// mergeKey为空，则比较original[i]和modifiedItem是否相等
		// mergeKey不为空，则比较mergeKey在original[i]和modifiedItem中的值是否相等
		// original[i]和modifiedItem不是map[string]interface{}，返回错误
		// original[i]和modifiedItem没有包含mergeKey，返回错误
		equal, err := mergeKeyValueEqual(original[i], modifiedItem, mergeKey)
		if err != nil || !equal {
			return equal, err
		}
	}
	return true, nil
}

// diffListsOfScalars returns 2 lists, the first one is addList and the second one is deletionList.
// Argument diffOptions.IgnoreChangesAndAdditions controls if calculate addList. true means not calculate.
// Argument diffOptions.IgnoreDeletions controls if calculate deletionList. true means not calculate.
// original may be changed, but modified is guaranteed to not be changed
// 将两个list按照元素的%v的字符排序
// 比较两个list，返回两个list，第一个是addList，第二个是deleteList
func diffListsOfScalars(original, modified []interface{}, diffOptions DiffOptions) ([]interface{}, []interface{}, error) {
	modifiedCopy := make([]interface{}, len(modified))
	copy(modifiedCopy, modified)
	// Sort the scalars for easier calculating the diff
	// 按照元素的%v的字符排序
	originalScalars := sortScalars(original)
	modifiedScalars := sortScalars(modifiedCopy)

	originalIndex, modifiedIndex := 0, 0
	addList := []interface{}{}
	deletionList := []interface{}{}

	for {
		originalInBounds := originalIndex < len(originalScalars)
		modifiedInBounds := modifiedIndex < len(modifiedScalars)
		// 都遍历完
		if !originalInBounds && !modifiedInBounds {
			break
		}
		// we need to compare the string representation of the scalar,
		// because the scalar is an interface which doesn't support either < or >
		// And that's how func sortScalars compare scalars.
		var originalString, modifiedString string
		var originalValue, modifiedValue interface{}
		// original未遍历完
		if originalInBounds {
			originalValue = originalScalars[originalIndex]
			originalString = fmt.Sprintf("%v", originalValue)
		}
		// modified未遍历完
		if modifiedInBounds {
			modifiedValue = modifiedScalars[modifiedIndex]
			modifiedString = fmt.Sprintf("%v", modifiedValue)
		}

		// 至少有一个未遍历完
		// 返回移除的元素originalString（originalString小于modifiedString）（originalString在前面，modifiedString在后面，说明元素删除），增加的元素modifiedString（originalString大于modifiedString，originalString在后面，modifiedString在前面）
		// 两个返回值中必有一个是nil，即只能有移除，增加，没有增加没有移除
		// 都遍历完(这里不会发生，上面已经判断了)，则originalV和modifiedV都为nil
		originalV, modifiedV := compareListValuesAtIndex(originalInBounds, modifiedInBounds, originalString, modifiedString)
		switch {
		// 两个元素的值相同
		case originalV == nil && modifiedV == nil:
			originalIndex++
			modifiedIndex++
		// originalValue被移除了
		// 如果不忽略移除字段或元素，则将originalValue添加到deletionList
		case originalV != nil && modifiedV == nil:
			if !diffOptions.IgnoreDeletions {
				deletionList = append(deletionList, originalValue)
			}
			originalIndex++
		// 增加了modifiedValue
		// 如果不忽略更新和增加字段，则将modifiedValue添加到addList
		case originalV == nil && modifiedV != nil:
			if !diffOptions.IgnoreChangesAndAdditions {
				addList = append(addList, modifiedValue)
			}
			modifiedIndex++
		default:
			return nil, nil, fmt.Errorf("Unexpected returned value from compareListValuesAtIndex: %v and %v", originalV, modifiedV)
		}
	}

	// 对deletionList进行去重
	return addList, deduplicateScalars(deletionList), nil
}

// If first return value is non-nil, list1 contains an element not present in list2
// If second return value is non-nil, list2 contains an element not present in list1
// 至少有一个未遍历完
// 返回移除的元素list1Value（list1小于list2）（list1在前面，list2在后面，说明元素删除），增加的元素list2Value（list1大于list2，list1在后面，list2在前面）
// 两个返回值中必有一个是nil，即只能有移除，增加，没有增加没有移除
// 都遍历完了，返回nil, nil
func compareListValuesAtIndex(list1Inbounds, list2Inbounds bool, list1Value, list2Value string) (interface{}, interface{}) {
	bothInBounds := list1Inbounds && list2Inbounds
	switch {
	// scalars are identical
	// 都未遍历完，且值一样，则返回nil
	case bothInBounds && list1Value == list2Value:
		return nil, nil
	// only list2 is in bound
	// list1已经遍历完，list2未遍历完
	case !list1Inbounds:
		fallthrough
	// list2 has additional scalar
	// 都未遍历完，且值不一样，list1大于list2（list1在后面，list2在前面），说明增加元素
	// 比如说original为[1,10]，modified为[1,2,3,10]，现在对比list1 10和list2 3
	// 返回增加的list2Value
	case bothInBounds && list1Value > list2Value:
		return nil, list2Value
	// only original is in bound
	// list1未遍历完，list2已经遍历完
	case !list2Inbounds:
		fallthrough
	// original has additional scalar
	// 都未遍历完，且值不一样，list1小于list2（list1在前面，list2在后面），说明元素删除
	// 比如original为[1,2,3,10]，modified为[1,10]，现在对比list1 2和list2 10
	// 返回移除的list1Value
	case bothInBounds && list1Value < list2Value:
		return list1Value, nil
	// 都遍历完了
	default:
		return nil, nil
	}
}

// diffListsOfMaps takes a pair of lists and
// returns a (recursive) strategic merge patch list contains additions and changes and
// a deletion list contains deletions
// 根据mergeKey进行diff，mergeKey相同的两个元素认为是同一个item
// 返回增加或更新的列表，移除的列表
// 元素为map[string]interface{}
func diffListsOfMaps(original, modified []interface{}, schema LookupPatchMeta, mergeKey string, diffOptions DiffOptions) ([]interface{}, []interface{}, error) {
	patch := make([]interface{}, 0, len(modified))
	deletionList := make([]interface{}, 0, len(original))

	// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误
	// 默认slice里的元素类型都是一样的
	// 如果slice内部元素类型不是map类型，则对slice进行去重并按照元素的%v的字符排序，并返回
	// 否则slice内部的元素是map类型
	//    如果递归调用过来的（recurse为true），则对map[string]interface）的每一个value进行排序处理
	//        key是"$retainKeys"，且value为slice，则value按照元素的%v的字符排序（value不为slice返回错误）
	//        key包含前缀"$deleteFromPrimitiveList"，且value是slice类型，则value按照元素的%v的字符排序（value不为slice返回错误）
	//        key包含"$setElementOrder"前缀，value必须是slice类型（value不为slice返回错误）
	//        key不是"$patch"
	//          如果value类型为map[string]interface{}，则继续对嵌套的内部map做排序处理
	//          value类型为[]interface{}，则继续进行对元素递归排序
	//          value是其他类型，则不变
	//        key是剩下情况（"$patch"），则不变
	//    如果不是递归调用过来的（recurse为true），则map的每个value不进行排序
	// 根据map的key为mergeKey的值，进行map排序
	originalSorted, err := sortMergeListsByNameArray(original, schema, mergeKey, false)
	if err != nil {
		return nil, nil, err
	}
	modifiedSorted, err := sortMergeListsByNameArray(modified, schema, mergeKey, false)
	if err != nil {
		return nil, nil, err
	}

	originalIndex, modifiedIndex := 0, 0
	for {
		originalInBounds := originalIndex < len(originalSorted)
		modifiedInBounds := modifiedIndex < len(modifiedSorted)
		bothInBounds := originalInBounds && modifiedInBounds
		// 都已经遍历完
		if !originalInBounds && !modifiedInBounds {
			break
		}

		var originalElementMergeKeyValueString, modifiedElementMergeKeyValueString string
		var originalElementMergeKeyValue, modifiedElementMergeKeyValue interface{}
		var originalElement, modifiedElement map[string]interface{}
		// origin还未遍历完
		if originalInBounds {
			// 返回originalSorted[originalIndex]，和originalSorted[originalIndex]里的key为mergeKey对应的值
			// originalSorted[originalIndex]（originalSorted元素类型）一定是map[string]interface{}
			originalElement, originalElementMergeKeyValue, err = getMapAndMergeKeyValueByIndex(originalIndex, mergeKey, originalSorted)
			if err != nil {
				return nil, nil, err
			}
			// originalElement里的key为mergeKey对应的值打印成"%v"
			originalElementMergeKeyValueString = fmt.Sprintf("%v", originalElementMergeKeyValue)
		}
		// modified还未遍历完
		if modifiedInBounds {
			// 返回modifiedSorted[modifiedIndex]，和modifiedSorted[modifiedIndex]里的key为mergeKey对应的值
			// modifiedSorted[modifiedIndex]（modifiedSorted元素类型）一定是map[string]interface{}
			modifiedElement, modifiedElementMergeKeyValue, err = getMapAndMergeKeyValueByIndex(modifiedIndex, mergeKey, modifiedSorted)
			if err != nil {
				return nil, nil, err
			}
			// modifiedElement里的key为mergeKey对应的值打印成"%v"
			modifiedElementMergeKeyValueString = fmt.Sprintf("%v", modifiedElementMergeKeyValue)
		}

		switch {
		// 都未遍历完，且originalElement里的key为mergeKey对应的值与modifiedElement里的key为mergeKey对应的值一样
		case bothInBounds && ItemMatchesOriginalAndModifiedSlice(originalElementMergeKeyValueString, modifiedElementMergeKeyValueString):
			// Merge key values are equal, so recurse
			// 继续比较map[string]interface{}的各个key和value
			patchValue, err := diffMaps(originalElement, modifiedElement, schema, diffOptions)
			if err != nil {
				return nil, nil, err
			}
			// mergeKey对应的值一样，还是存在差异，则设置patchValue[mergeKey]为modifiedElementMergeKeyValue（modifiedElement里的key为mergeKey对应的值一样）
			// 由于key为mergeKey对应的值一样，所以继续比较map里的元素生成的patchValue里不会有mergeKey，所以这里补上
			if len(patchValue) > 0 {
				patchValue[mergeKey] = modifiedElementMergeKeyValue
				patch = append(patch, patchValue)
			}
			originalIndex++
			modifiedIndex++
		// only modified is in bound
		// original已经遍历完，modified未遍历完
		case !originalInBounds:
			fallthrough
		// modified has additional map
		// 都未遍历完，且originalElement里的key为mergeKey对应的值与modifiedElement里的key为mergeKey对应的值不一样，且originalElement里的key为mergeKey对应的值打印成"%v"比modifiedElement里的key为mergeKey对应的值打印成"%v"大（即modifiedElement在前面，originalElement在后面）
		case bothInBounds && ItemAddedToModifiedSlice(originalElementMergeKeyValueString, modifiedElementMergeKeyValueString):
			// 不忽略更新和增加字段，则添加modifiedElement到patch
			// 说明是增加新字段
			if !diffOptions.IgnoreChangesAndAdditions {
				patch = append(patch, modifiedElement)
			}
			// 比如说original为[1,10],modified为[1,2,3,10]，现在对比10和3
			modifiedIndex++
		// only original is in bound
		// original未遍历完，modified已经遍历完
		case !modifiedInBounds:
			fallthrough
		// original has additional map
		// 都未遍历完，且originalElement里的key为mergeKey对应的值与modifiedElement里的key为mergeKey对应的值不一样，且originalElement里的key为mergeKey对应的值打印成"%v"比modifiedElement里的key为mergeKey对应的值打印成"%v"小（即modifiedElement在后面面，originalElement在前面）
		case bothInBounds && ItemRemovedFromModifiedSlice(originalElementMergeKeyValueString, modifiedElementMergeKeyValueString):
			// 不忽略更新和增加字段，则添加map[string]interface{}{mergeKey: mergeKeyValue, "$patch": "delete"}到deletionList
			// 相当于originalElement被移除了
			if !diffOptions.IgnoreDeletions {
				// Item was deleted, so add delete directive
				// 生成map[string]interface{}{mergeKey: mergeKeyValue, "$patch": "delete"}
				// 添加到deletionList
				deletionList = append(deletionList, CreateDeleteDirective(mergeKey, originalElementMergeKeyValue))
			}
			// 比如说original为[1,2,3,10]，modified为[1,10]，现在对比2和10
			originalIndex++
		}
	}

	return patch, deletionList, nil
}

// getMapAndMergeKeyValueByIndex return a map in the list and its merge key value given the index of the map.
// 返回listOfMaps[index]，和listOfMaps[index]里key为mergeKey对应的值
// listOfMaps[index]（listOfMaps元素类型）一定是map[string]interface{}
func getMapAndMergeKeyValueByIndex(index int, mergeKey string, listOfMaps []interface{}) (map[string]interface{}, interface{}, error) {
	m, ok := listOfMaps[index].(map[string]interface{})
	if !ok {
		// slice元素不是map[string]interface{}，返回错误
		return nil, nil, mergepatch.ErrBadArgType(m, listOfMaps[index])
	}

	// 获得mergeKey对应的值
	val, ok := m[mergeKey]
	if !ok {
		return nil, nil, mergepatch.ErrNoMergeKey(m, mergeKey)
	}
	return m, val, nil
}

// StrategicMergePatch applies a strategic merge patch. The patch and the original document
// must be json encoded content. A patch can be created from an original and a modified document
// by calling CreateStrategicMergePatch.
func StrategicMergePatch(original, patch []byte, dataStruct interface{}) ([]byte, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	return StrategicMergePatchUsingLookupPatchMeta(original, patch, schema)
}

func StrategicMergePatchUsingLookupPatchMeta(original, patch []byte, schema LookupPatchMeta) ([]byte, error) {
	originalMap, err := handleUnmarshal(original)
	if err != nil {
		return nil, err
	}
	patchMap, err := handleUnmarshal(patch)
	if err != nil {
		return nil, err
	}

	result, err := StrategicMergeMapPatchUsingLookupPatchMeta(originalMap, patchMap, schema)
	if err != nil {
		return nil, err
	}

	return json.Marshal(result)
}

func handleUnmarshal(j []byte) (map[string]interface{}, error) {
	if j == nil {
		j = []byte("{}")
	}

	m := map[string]interface{}{}
	err := json.Unmarshal(j, &m)
	if err != nil {
		return nil, mergepatch.ErrBadJSONDoc
	}
	return m, nil
}

// StrategicMergeMapPatch applies a strategic merge patch. The original and patch documents
// must be JSONMap. A patch can be created from an original and modified document by
// calling CreateTwoWayMergeMapPatch.
// Warning: the original and patch JSONMap objects are mutated by this function and should not be reused.
func StrategicMergeMapPatch(original, patch JSONMap, dataStruct interface{}) (JSONMap, error) {
	schema, err := NewPatchMetaFromStruct(dataStruct)
	if err != nil {
		return nil, err
	}

	// We need the go struct tags `patchMergeKey` and `patchStrategy` for fields that support a strategic merge patch.
	// For native resources, we can easily figure out these tags since we know the fields.

	// Because custom resources are decoded as Unstructured and because we're missing the metadata about how to handle
	// each field in a strategic merge patch, we can't find the go struct tags. Hence, we can't easily  do a strategic merge
	// for custom resources. So we should fail fast and return an error.
	if _, ok := dataStruct.(*unstructured.Unstructured); ok {
		return nil, mergepatch.ErrUnsupportedStrategicMergePatchFormat
	}

	return StrategicMergeMapPatchUsingLookupPatchMeta(original, patch, schema)
}

func StrategicMergeMapPatchUsingLookupPatchMeta(original, patch JSONMap, schema LookupPatchMeta) (JSONMap, error) {
	mergeOptions := MergeOptions{
		MergeParallelList:    true,
		IgnoreUnmatchedNulls: true,
	}
	return mergeMap(original, patch, schema, mergeOptions)
}

// MergeStrategicMergeMapPatchUsingLookupPatchMeta merges strategic merge
// patches retaining `null` fields and parallel lists. If 2 patches change the
// same fields and the latter one will override the former one. If you don't
// want that happen, you need to run func MergingMapsHaveConflicts before
// merging these patches. Applying the resulting merged merge patch to a JSONMap
// yields the same as merging each strategic merge patch to the JSONMap in
// succession.
func MergeStrategicMergeMapPatchUsingLookupPatchMeta(schema LookupPatchMeta, patches ...JSONMap) (JSONMap, error) {
	mergeOptions := MergeOptions{
		MergeParallelList:    false,
		IgnoreUnmatchedNulls: false,
	}
	merged := JSONMap{}
	var err error
	for _, patch := range patches {
		merged, err = mergeMap(merged, patch, schema, mergeOptions)
		if err != nil {
			return nil, err
		}
	}
	return merged, nil
}

// handleDirectiveInMergeMap handles the patch directive when merging 2 maps.
// "$patch"的value为"replace"，将"$patch"从patch删除，直接返回patch
// "$patch"的value为"delete"，返回空的map[string]interface{}
// 其他情况，返回错误
func handleDirectiveInMergeMap(directive interface{}, patch map[string]interface{}) (map[string]interface{}, error) {
	// "$patch"的value为"replace"，将"$patch"从patch删除，直接返回patch
	if directive == replaceDirective {
		// If the patch contains "$patch: replace", don't merge it, just use the
		// patch directly. Later on, we can add a single level replace that only
		// affects the map that the $patch is in.
		// 将"$patch"从patch删除
		delete(patch, directiveMarker)
		return patch, nil
	}

	// "$patch"的value为"delete"，返回空的map[string]interface{}
	if directive == deleteDirective {
		// If the patch contains "$patch: delete", don't merge it, just return
		//  an empty map.
		return map[string]interface{}{}, nil
	}

	return nil, mergepatch.ErrBadPatchType(directive, patch)
}

// 如果item是map[string]interface{}，则如果有key为"$patch"，则返回true
// 否则返回false
func containsDirectiveMarker(item interface{}) bool {
	m, ok := item.(map[string]interface{})
	if ok {
		if _, foundDirectiveMarker := m[directiveMarker]; foundDirectiveMarker {
			return true
		}
	}
	return false
}

// mergeKey为空，则比较left和right是否相等
// mergeKey不为空，则比较mergeKey在left和right中的值是否相等
// mergeKey不为空，left和right不是map[string]interface{}，返回错误
// mergeKey不为空，left和right没有包含mergeKey，返回错误
func mergeKeyValueEqual(left, right interface{}, mergeKey string) (bool, error) {
	if len(mergeKey) == 0 {
		return left == right, nil
	}
	typedLeft, ok := left.(map[string]interface{})
	if !ok {
		return false, mergepatch.ErrBadArgType(typedLeft, left)
	}
	typedRight, ok := right.(map[string]interface{})
	if !ok {
		return false, mergepatch.ErrBadArgType(typedRight, right)
	}
	mergeKeyLeft, ok := typedLeft[mergeKey]
	if !ok {
		return false, mergepatch.ErrNoMergeKey(typedLeft, mergeKey)
	}
	mergeKeyRight, ok := typedRight[mergeKey]
	if !ok {
		return false, mergepatch.ErrNoMergeKey(typedRight, mergeKey)
	}
	return mergeKeyLeft == mergeKeyRight, nil
}

// extractKey trims the prefix and return the original key
func extractKey(s, prefix string) (string, error) {
	substrings := strings.SplitN(s, "/", 2)
	// s里没有"/"，或s为""，"/"前面不是prefix，则返回错误
	if len(substrings) <= 1 || substrings[0] != prefix {
		switch prefix {
		case deleteFromPrimitiveListDirectivePrefix:
			return "", mergepatch.ErrBadPatchFormatForPrimitiveList
		case setElementOrderDirectivePrefix:
			return "", mergepatch.ErrBadPatchFormatForSetElementOrderList
		default:
			return "", fmt.Errorf("fail to find unknown prefix %q in %s\n", prefix, s)
		}
	}
	return substrings[1], nil
}

// validatePatchUsingSetOrderList verifies:
// the relative order of any two items in the setOrderList list matches that in the patch list.
// the items in the patch list must be a subset or the same as the $setElementOrder list (deletions are ignored).
// 校验patchList里的元素的顺序跟在setOrderList里顺序是否一样
// setOrderList或patch list为空，则直接返回
// 如果元素类型是map[string]interface{}，则跳过key为"$patch"，即忽略删除的元素
// 如果mergeKey不为空，则元素类型一定是map[string]interface{}，则按照mergeKey的值作为比较
// mergeKey为空，则按照元素的值进行比较
func validatePatchWithSetOrderList(patchList, setOrderList interface{}, mergeKey string) error {
	typedSetOrderList, ok := setOrderList.([]interface{})
	if !ok {
		return mergepatch.ErrBadPatchFormatForSetElementOrderList
	}
	typedPatchList, ok := patchList.([]interface{})
	if !ok {
		return mergepatch.ErrBadPatchFormatForSetElementOrderList
	}
	// setOrderList或patch list为空，则直接返回
	if len(typedSetOrderList) == 0 || len(typedPatchList) == 0 {
		return nil
	}

	var nonDeleteList []interface{}
	var err error
	// mergeKey不为空
	if len(mergeKey) > 0 {
		// 第二个返回值 typedPatchList里存在key为"$patch"且value为"delete"，则这个元素（map[string]interface{}）为要被删除的
		// 第一个返回值 剩下的为需要保留的
		nonDeleteList, _, err = extractToDeleteItems(typedPatchList)
		if err != nil {
			return err
		}
	} else {
		// mergeKey为空
		nonDeleteList = typedPatchList
	}

	patchIndex, setOrderIndex := 0, 0
	// 遍历patch里的nonDeleteList，如果元素类型是map[string]interface{}，则跳过key为"$patch"
	// 根据是否有mergeKey，进行比较（patch元素是否在setOrderList里）
	//   mergeKey为空，则比较nonDeleteList[patchIndex]和typedSetOrderList[setOrderIndex]是否相等
	//   mergeKey不为空，则比较mergeKey在nonDeleteList[patchIndex]和typedSetOrderList[setOrderIndex]中的值是否相等
	for patchIndex < len(nonDeleteList) && setOrderIndex < len(typedSetOrderList) {
		// 如果item是map[string]interface{}，则如果有key为"$patch"，则返回true
		// 否则返回false
		// 跳过要被删除的元素
		if containsDirectiveMarker(nonDeleteList[patchIndex]) {
			patchIndex++
			continue
		}
		// mergeKey为空，则比较nonDeleteList[patchIndex]和typedSetOrderList[setOrderIndex]是否相等
		// mergeKey不为空，则比较mergeKey在nonDeleteList[patchIndex]和typedSetOrderList[setOrderIndex]中的值是否相等
		// mergeKey不为空，nonDeleteList[patchIndex]和typedSetOrderList[setOrderIndex]不是map[string]interface{}，返回错误
		// mergeKey不为空，nonDeleteList[patchIndex]和typedSetOrderList[setOrderIndex]任意一个没有包含mergeKey，返回错误
		mergeKeyEqual, err := mergeKeyValueEqual(nonDeleteList[patchIndex], typedSetOrderList[setOrderIndex], mergeKey)
		if err != nil {
			return err
		}
		// 找到相等的sedOrderList，则继续下一个patch，patchIndex++
		if mergeKeyEqual {
			patchIndex++
		}
		setOrderIndex++
	}
	// If patchIndex is inbound but setOrderIndex if out of bound mean there are items mismatching between the patch list and setElementOrder list.
	// the second check is a sanity check, and should always be true if the first is true.
	// nonDeleteList未遍历完，且setOrderList已经遍历完，说明patchList里有一部分顺序和SetOrderList不一样
	if patchIndex < len(nonDeleteList) && setOrderIndex >= len(typedSetOrderList) {
		return fmt.Errorf("The order in patch list:\n%v\n doesn't match %s list:\n%v\n", typedPatchList, setElementOrderDirectivePrefix, setOrderList)
	}
	return nil
}

// preprocessDeletionListForMerging preprocesses the deletion list.
// it returns shouldContinue, isDeletionList, noPrefixKey
// 返回值为应该继续，是否为删除列表，去掉前缀后剩下后半部分的内容
// 如果key包含前缀"$deleteFromPrimitiveList"，且mergeDeletionList为false，则直接设置original的key的值为patchVal，返回true, false, "", nil
// 如果key包含前缀"$deleteFromPrimitiveList"，且mergeDeletionList为true，从key中（格式"$deleteFromPrimitiveList/{originalKey}"）解析出originalKey，返回false, true, originalKey, err
// 否则，返回false, false, "", nil
func preprocessDeletionListForMerging(key string, original map[string]interface{},
	patchVal interface{}, mergeDeletionList bool) (bool, bool, string, error) {
	// If found a parallel list for deletion and we are going to merge the list,
	// overwrite the key to the original key and set flag isDeleteList
	// key是否包含前缀"$deleteFromPrimitiveList"
	foundParallelListPrefix := strings.HasPrefix(key, deleteFromPrimitiveListDirectivePrefix)
	// key包含前缀"$deleteFromPrimitiveList"
	if foundParallelListPrefix {
		// mergeDeletionList为false，则直接设置original的key的值为patchVal，返回true, false, "", nil
		if !mergeDeletionList {
			original[key] = patchVal
			return true, false, "", nil
		}
		// 从key中（格式"$deleteFromPrimitiveList/{originalKey}"）解析出originalKey，返回false, true, originalKey, err
		originalKey, err := extractKey(key, deleteFromPrimitiveListDirectivePrefix)
		return false, true, originalKey, err
	}
	return false, false, "", nil
}

// applyRetainKeysDirective looks for a retainKeys directive and applies to original
// - if no directive exists do nothing
// - if directive is found, clear keys in original missing from the directive list
// - validate that all keys present in the patch are present in the retainKeys directive
// note: original may be another patch request, e.g. applying the add+modified patch to the deletions patch. In this case it may have directives
// patch中没有"$retainKeys"，则直接返回
// 如果有"$retainKeys"，
//   从patch中移除"$retainKeys"
//   不启用MergeParallelList
//     original里有"$retainKeys"，且"$retainKeys"的值在original和patch中不一样，则返回错误
//     original里没有，则添加key为"$retainKeys"和value为patch中$retainKeys"的值到original，返回nil
//   启用MergeParallelList
//      验证"$retainKeys"的patch中的值是否为切片slice，如果不是切片则返回错误
//      patch里的key（跳过值是nil或key包含"$deleteFromPrimitiveList"前缀或包含"$setElementOrder"前缀）不在"$retainKeys"在patch中的值中，则返回错误
//      original里的key不在"$retainKeys"的patch中的值中，则将这个key从original删除
func applyRetainKeysDirective(original, patch map[string]interface{}, options MergeOptions) error {
	// patch中是否有"$retainKeys"
	retainKeysInPatch, foundInPatch := patch[retainKeysDirective]
	// 没有则直接返回
	if !foundInPatch {
		return nil
	}
	// cleanup the directive
	// 从patch中移除"$retainKeys"
	delete(patch, retainKeysDirective)

	// 不启用MergeParallelList
	// original里有"$retainKeys"，且"$retainKeys"的值在original和patch中不一样，则返回错误
	// original里没有，则添加key为"$retainKeys"和value为patch中$retainKeys"的值到original
	// 直接返回
	if !options.MergeParallelList {
		// If original is actually a patch, make sure the retainKeys directives are the same in both patches if present in both.
		// If not present in the original patch, copy from the modified patch.
		// original里是否包含"$retainKeys"
		retainKeysInOriginal, foundInOriginal := original[retainKeysDirective]
		// original里有，且"$retainKeys"的值在original和patch中不一样，则返回错误
		if foundInOriginal {
			// 比较"$retainKeys"的值在original和patch中是否一样
			// "$retainKeys"的值在original和patch中不一样，则返回错误
			if !reflect.DeepEqual(retainKeysInOriginal, retainKeysInPatch) {
				// This error actually should never happen.
				return fmt.Errorf("%v and %v are not deep equal: this may happen when calculating the 3-way diff patch", retainKeysInOriginal, retainKeysInPatch)
			}
		} else {
			// original里没有，则添加key为"$retainKeys"和value为patch中$retainKeys"的值
			original[retainKeysDirective] = retainKeysInPatch
		}
		return nil
	}

	// 启用MergeParallelList
	// "$retainKeys"的patch中的值是切片slice，如果不是切片则返回错误
	// retainKeysList保存的是modified里值不为nil的key
	retainKeysList, ok := retainKeysInPatch.([]interface{})
	if !ok {
		return mergepatch.ErrBadPatchFormatForRetainKeys
	}

	// validate patch to make sure all fields in the patch are present in the retainKeysList.
	// The map is used only as a set, the value is never referenced
	m := map[interface{}]struct{}{}
	for _, v := range retainKeysList {
		m[v] = struct{}{}
	}
	for k, v := range patch {
		// 值是nil或key包含"$deleteFromPrimitiveList"前缀或包含"$setElementOrder"前缀
		// 说明是删除的字段或元素和SetElementOrder，则继续下一个key
		if v == nil || strings.HasPrefix(k, deleteFromPrimitiveListDirectivePrefix) ||
			strings.HasPrefix(k, setElementOrderDirectivePrefix) {
			continue
		}
		// If there is an item present in the patch but not in the retainKeys list,
		// the patch is invalid.
		// patch里的key不在"$retainKeys"在patch中的值中，则返回错误
		if _, found := m[k]; !found {
			return mergepatch.ErrBadPatchFormatForRetainKeys
		}
	}

	// clear not present fields
	// original里的key不在"$retainKeys"的patch中的值中，则将这个key从original删除
	for k := range original {
		if _, found := m[k]; !found {
			delete(original, k)
		}
	}
	return nil
}

// mergePatchIntoOriginal processes $setElementOrder list.
// When not merging the directive, it will make sure $setElementOrder list exist only in original.
// When merging the directive, it will try to find the $setElementOrder list and
// its corresponding patch list, validate it and merge it.
// Then, sort them by the relative order in setElementOrder, patch list and live list.
// The precedence is $setElementOrder > order in patch list > order in live list.
// This function will delete the item after merging it to prevent process it again in the future.
// Ref: https://git.k8s.io/community/contributors/design-proposals/cli/preserve-order-in-strategic-merge-patch.md
// 处理patch中key格式为"$setElementOrder/{key}"
// 未启用MergeParallelList
//   original里也存在相同"$setElementOrder"前缀的key，且在original里的值和在patch里的值不一样，则返回错误
//   original里不存在相同"$setElementOrder"前缀的key，则添加key为key，value为key在patch里的值
// 从patch中删除这个key
// patch中"$setElementOrder/{key}"的value的值类型不为slice，则返回错误
// originalKey为从key格式"$setElementOrder/{key}"中提取出"/"后半部分的值
// 在original里存在这个原始key，如果值的类型不是slice，则返回错误
// 在patch中存在这个原始的key，如果值的类型不是slice，则返回错误
// 对original和patch原始key的元素进行merge（在original里存在这个原始key和在patch中存在这个原始的key，至少有一个为true。都不为true则跳过patch中的这个key）
//   在original里存在这个原始key，且在patch中不存在这个原始的key，则merged为originalFieldValue（original里这个原始key的值）
//   在original里不存在这个原始key，且在patch中存在这个原始key，则merged为patchFieldValue（patch里这个原始key的值）
//   在original里存在这个原始key，且在patch中存在这个原始key，则对originalFieldValue和patchFieldValue这两个值进行merge
//   mergeKey为空，将merged进行分类，patchItems（在typedSetElementOrderList部分）和serverOnlyItems（不在typedSetElementOrderList部分）
//   mergeKey不为空，将merged进行分类，patchItems（跟typedSetElementOrderList有相同mergeKey和对应mergeKeyValue）部分，serverOnlyItems（没有相同的mergeKey和对应mergeKeyValue部分）
//      merged和typedSetElementOrderList元素类型不是map[string]interface{}，返回错误
// 先将patchItems按照typedSetElementOrderList（patch中"$setElementOrder/{key}"的value的值）进行排序，serverOnlyItems按照originalFieldValue（original里原始key的值）进行排序
//   然后将两个排序后的list进行合并
//   插入的顺序是（元素类型是map[string]interface{}）按照在originalFieldValue中mergeKey对应的一样值的位置，或（元素类型是其他类型）在originalFieldValue中位置
//   如果都在originalFieldValue里，则谁在前面，进行先插入
//   否则，patchItems元素先插入。
// original的原始key的值设为上面合并的结果
// 从patch中移除原始key
func mergePatchIntoOriginal(original, patch map[string]interface{}, schema LookupPatchMeta, mergeOptions MergeOptions) error {
	klog.Infof("mergePatchIntoOriginal: original: %v, patch: %v, mergeOptions: %#v", original, patch, mergeOptions)
	for key, patchV := range patch {
		// Do nothing if there is no ordering directive
		// key格式为"$setElementOrder/{key}"
		// key的前缀不匹配"$setElementOrder"，则跳过
		if !strings.HasPrefix(key, setElementOrderDirectivePrefix) {
			continue
		}

		// 在merge策略下进行diff map slice里setElementOrderInPatch的值是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值（只有一个key mergeKey）
		// 在merge策略下进行diff Primitive slice里setElementOrderInPatch的值是modified，[]interface{}
		setElementOrderInPatch := patchV
		// Copies directive from the second patch (`patch`) to the first patch (`original`)
		// and checks they are equal and delete the directive in the second patch
		// 未启用MergeParallelList
		if !mergeOptions.MergeParallelList {
			setElementOrderListInOriginal, ok := original[key]
			// original里也存在相同"$setElementOrder"前缀的key，且在original里的值和在patch里的值不一样，则返回错误
			if ok {
				// check if the setElementOrder list in original and the one in patch matches
				if !reflect.DeepEqual(setElementOrderListInOriginal, setElementOrderInPatch) {
					return mergepatch.ErrBadPatchFormatForSetElementOrderList
				}
			} else {
				// move the setElementOrder list from patch to original
				// original里不存在相同"$setElementOrder"前缀的key，则添加key为key，value为key在patch里的值
				original[key] = setElementOrderInPatch
			}
		}
		// 删除这个key
		delete(patch, key)

		var (
			ok                                          bool
			originalFieldValue, patchFieldValue, merged []interface{}
			patchStrategy                               string
			patchMeta                                   PatchMeta
			subschema                                   LookupPatchMeta
		)
		typedSetElementOrderList, ok := setElementOrderInPatch.([]interface{})
		// value的值类型不为slice，则返回错误
		if !ok {
			return mergepatch.ErrBadArgType(typedSetElementOrderList, setElementOrderInPatch)
		}
		// Trim the setElementOrderDirectivePrefix to get the key of the list field in original.
		// 从key格式"$setElementOrder/{key}"中提取出"/"后半部分的值
		originalKey, err := extractKey(key, setElementOrderDirectivePrefix)
		if err != nil {
			return err
		}
		// try to find the list with `originalKey` in `original` and `modified` and merge them.
		originalList, foundOriginal := original[originalKey]
		patchList, foundPatch := patch[originalKey]
		// 在original里存在这个原始key，如果值的类型不是slice，则返回错误
		if foundOriginal {
			originalFieldValue, ok = originalList.([]interface{})
			if !ok {
				return mergepatch.ErrBadArgType(originalFieldValue, originalList)
			}
		}
		// 在patch中存在这个原始的key，如果值的类型不是slice，则返回错误
		if foundPatch {
			patchFieldValue, ok = patchList.([]interface{})
			if !ok {
				return mergepatch.ErrBadArgType(patchFieldValue, patchList)
			}
		}
		// 如果schema是PatchMetaFromOpenAPI
		// 返回slice的item类型Schema，组装成第一个返回值PatchMetaFromOpenAPI{Schema: sliceItem.subschema}
		// s.Schema类型是proto.*Kind，则调用sliceItem.VisitKind(s.Schema)
		//     从s.Schema中查找sliceItem.key字段对应Schema
		//     从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成第二个返回值PatchMeta
		// 否则，第二个参数为PatchMeta{}（空的PatchMeta）
		subschema, patchMeta, err = schema.LookupPatchMetadataForSlice(originalKey)
		if err != nil {
			return err
		}
		// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
		// 当strategies的数量大于等于3个，则返回错误
		// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
		_, patchStrategy, err = extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
		if err != nil {
			return err
		}
		// Check for consistency between the element order list and the field it applies to
		// 输入参数为patch中原始key的值，key在patch中的值，merge key
		// patchFieldValue或typedSetElementOrderList为空，则直接返回
		// 校验patchFieldValue（patch中原始key的值）里的元素的顺序跟在SetElementOrderList里顺序是否一样
		// 如果元素类型是map[string]interface{}，则跳过key为"$patch"，即忽略删除的元素
		// 如果mergeKey不为空，则元素类型一定是map[string]interface{}，则按照mergeKey的值作为比较
		// mergeKey为空，则按照元素的值进行比较
		// 返回错误代表顺序不一致
		err = validatePatchWithSetOrderList(patchFieldValue, typedSetElementOrderList, patchMeta.GetPatchMergeKey())
		if err != nil {
			return err
		}

		switch {
		// 在original里存在这个原始key，且在patch中不存在这个原始的key
		case foundOriginal && !foundPatch:
			// no change to list contents
			merged = originalFieldValue
		// 在original里不存在这个原始key，且在patch中存在这个原始key
		case !foundOriginal && foundPatch:
			// list was added
			merged = patchFieldValue
		// 在original里存在这个原始key，且在patch中存在这个原始key
		case foundOriginal && foundPatch:
			// 如果original和patch任意一个不是[]interface{}，返回错误
			// fieldPatchStrategy（patch策略）是"merge"
			// original和patch都为空，则不做任何操作
			// 元素类型不是map
			//   启用MergeParallelList且isDeleteList为true，从original中移除在patch中的元素，则返回移除后的[]interface{}
			//   不启用MergeParallelLis，或启用MergeParallelList且isDeleteList为false
			//     对original和patch进行合并，然后进行去重，赋值为merged
			//     将merged进行分类，在patch部分和不在patch部分
			//     先将patchItems（在patch部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//     然后将两个排序后的list进行合并
			//     合并规则：
			//        如果都在original里，按照在original中位置，谁在前面，进行插入。
			//        如果不都在original里，patchItems（在patch部分）进行插入
			//     返回合并后list
			// 元素是map
			//   mergeKey为空，返回错误
			//   进行"$patch"处理（删除或替换）：
			//     存在"$patch"，"$patch"的值为"delete"，则original为删除map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））和patch移除包含"$patch"的map之后的值
			//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
			//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
			//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
			//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
			//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//         然后将两个排序后的list进行合并
			//         合并规则：
			//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
			//            如果不都在original里，patchItems（在patch部分）进行插入
			//         返回合并后list
			//     "$patch"的值是"replace"，则original为patch移除包含"$patch"的map之后的值，patch设置为nil
			//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
			//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//         然后将两个排序后的list进行合并
			//         合并规则：
			//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
			//            如果不都在original里，patchItems（在patch部分）进行插入
			//         返回合并后list
			//      所有patch元素都不存在"$patch"，original为original, patch为（遍历patch重新append生成的）
			//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
			//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
			//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
			//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
			//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//         然后将两个排序后的list进行合并
			//         合并规则：
			//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
			//            如果不都在original里，patchItems（在patch部分）进行插入
			//         返回合并后list
			//      存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
			//      patch里的元素存在key "$patch"但是不存在key mergeKey，则返回错误
			// fieldPatchStrategy（patch策略）不是"merge"，返回patch（转成[]interface{}）
			merged, err = mergeSliceHandler(originalList, patchList, subschema,
				patchStrategy, patchMeta.GetPatchMergeKey(), false, mergeOptions)
			if err != nil {
				return err
			}
		// 在original里不存在这个原始key，且在patch中不存在这个原始key，则继续下一个元素
		case !foundOriginal && !foundPatch:
			continue
		}

		klog.Infof("merged: %#v", merged)
		// Split all items into patch items and server-only items and then enforce the order.
		var patchItems, serverOnlyItems []interface{}
		// mergeKey为空
		if len(patchMeta.GetPatchMergeKey()) == 0 {
			// Primitives doesn't need merge key to do partitioning.
			// 将merged进行分类，返回在typedSetElementOrderList部分和不在typedSetElementOrderList部分
			patchItems, serverOnlyItems = partitionPrimitivesByPresentInList(merged, typedSetElementOrderList)

		} else {
			// Maps need merge key to do partitioning.
			// mergeKey不为空
			// 将merged进行分类，返回跟typedSetElementOrderList有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
			// merged和typedSetElementOrderList元素类型不是map[string]interface{}，返回错误
			patchItems, serverOnlyItems, err = partitionMapsByPresentInList(merged, typedSetElementOrderList, patchMeta.GetPatchMergeKey())
			if err != nil {
				return err
			}
		}
		klog.Infof("patchItems: %v, serverOnlyItems: %v", patchItems, serverOnlyItems)

		// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误，嵌套slice，则返回错误
		// 否则返回slice内部元素类型
		elementType, err := sliceElementType(originalFieldValue, patchFieldValue)
		if err != nil {
			return err
		}
		kind := elementType.Kind()
		// normalize merged list
		// typedSetElementOrderList contains all the relative order in typedPatchList,
		// so don't need to use typedPatchList
		// 先将patchItems按照typedSetElementOrderList进行排序，serverOnlyItems按照originalFieldValue进行排序
		// 然后将两个排序后的list进行合并
		// 插入的顺序是（元素类型是map[string]interface{}）按照在originalFieldValue中mergeKey对应的一样值的位置，或（元素类型是其他类型）在originalFieldValue中位置
		// 如果都在originalFieldValue里，则谁在前面，进行先插入
		// 否则，patchItems元素先插入。
		both, err := normalizeElementOrder(patchItems, serverOnlyItems, typedSetElementOrderList, originalFieldValue, patchMeta.GetPatchMergeKey(), kind)
		if err != nil {
			return err
		}
		klog.Infof("both: %v", both)
		original[originalKey] = both
		// delete patch list from patch to prevent process again in the future
		delete(patch, originalKey)
	}
	return nil
}

// partitionPrimitivesByPresentInList partitions elements into 2 slices, the first containing items present in partitionBy, the other not.
// 将original进行分类，返回在partitionBy部分和不在partitionBy部分
func partitionPrimitivesByPresentInList(original, partitionBy []interface{}) ([]interface{}, []interface{}) {
	patch := make([]interface{}, 0, len(original))
	serverOnly := make([]interface{}, 0, len(original))
	inPatch := map[interface{}]bool{}
	for _, v := range partitionBy {
		inPatch[v] = true
	}
	for _, v := range original {
		if !inPatch[v] {
			serverOnly = append(serverOnly, v)
		} else {
			patch = append(patch, v)
		}
	}
	return patch, serverOnly
}

// partitionMapsByPresentInList partitions elements into 2 slices, the first containing items present in partitionBy, the other not.
// 将original进行分类，返回跟partitionBy有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
// original和partitionBy元素类型不是map[string]interface{}，返回错误
func partitionMapsByPresentInList(original, partitionBy []interface{}, mergeKey string) ([]interface{}, []interface{}, error) {
	patch := make([]interface{}, 0, len(original))
	serverOnly := make([]interface{}, 0, len(original))
	for _, v := range original {
		typedV, ok := v.(map[string]interface{})
		if !ok {
			return nil, nil, mergepatch.ErrBadArgType(typedV, v)
		}
		mergeKeyValue, foundMergeKey := typedV[mergeKey]
		// 不包含mergeKey，则返回错误
		if !foundMergeKey {
			return nil, nil, mergepatch.ErrNoMergeKey(typedV, mergeKey)
		}
		// 遍历partitionBy，如果存在mergeKey和mergeKeyValue，则返回这个元素map[string]interface{}，index，true，nil。否则返回nil, 0, false, nil
		// 元素类型不是map[string]interface{}，返回错误
		_, _, found, err := findMapInSliceBasedOnKeyValue(partitionBy, mergeKey, mergeKeyValue)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			serverOnly = append(serverOnly, v)
		} else {
			patch = append(patch, v)
		}
	}
	return patch, serverOnly, nil
}

// Merge fields from a patch map into the original map. Note: This may modify
// both the original map and the patch because getting a deep copy of a map in
// golang is highly non-trivial.
// flag mergeOptions.MergeParallelList controls if using the parallel list to delete or keeping the list.
// If patch contains any null field (e.g. field_1: null) that is not
// present in original, then to propagate it to the end result use
// mergeOptions.IgnoreUnmatchedNulls == false.
// patch中存在"$patch"
//   "$patch"的value为"replace"，将"$patch"从patch删除，直接返回patch
//   "$patch"的value为"delete"，返回空的map[string]interface{}
//   其他情况，返回错误
// patch中有"$retainKeys"
//   从patch中移除"$retainKeys"
//   不启用MergeParallelList
//     original里有"$retainKeys"，且"$retainKeys"的值在original和patch中不一样，则返回错误
//     original里没有"$retainKeys"，则添加key为"$retainKeys"和value为patch中$retainKeys"的值到original
//   启用MergeParallelList
//     验证"$retainKeys"的patch中的值是否为切片slice，如果不是切片则返回错误
//     验证patch里的key（跳过值是nil或key包含"$deleteFromPrimitiveList"前缀或包含"$setElementOrder"前缀）不在"$retainKeys"在patch中的值中，则返回错误
//     original里的key不在"$retainKeys"的patch中的值中，则将这个key从original删除
// 处理patch中key格式为"$setElementOrder/{key}"
//   未启用MergeParallelList
//     original里也存在相同"$setElementOrder"前缀的key，且在original里的值和在patch里的值不一样，则返回错误
//     original里不存在相同"$setElementOrder"前缀的key，则添加key为key，value为key在patch里的值
//   从patch中删除这个key
//   patch中"$setElementOrder/{key}"的value的值类型不为slice，则返回错误
//   originalKey为从key格式"$setElementOrder/{key}"中提取出"/"后半部分的值
//   在original里存在这个原始key，如果值的类型不是slice，则返回错误
//   对original和patch原始key的元素进行merge（在original里存在这个原始key和在patch中存在这个原始的key，至少有一个为true。都不为true则跳过patch中的这个key）
//   在original里存在这个原始key，且在patch中不存在这个原始的key，则merged为originalFieldValue（original里这个原始key的值）
//   在original里不存在这个原始key，且在patch中存在这个原始key，则merged为patchFieldValue（patch里这个原始key的值）
//   在original里存在这个原始key，且在patch中存在这个原始key，则对originalFieldValue和patchFieldValue这两个值进行merge
//   mergeKey为空，将merged进行分类，patchItems（在typedSetElementOrderList部分）和serverOnlyItems（不在typedSetElementOrderList部分）
//   mergeKey不为空，将merged进行分类，patchItems（跟typedSetElementOrderList有相同mergeKey和对应mergeKeyValue）部分，serverOnlyItems（没有相同的mergeKey和对应mergeKeyValue部分）
//     merged和typedSetElementOrderList元素类型不是map[string]interface{}，返回错误
//   先将patchItems按照typedSetElementOrderList（patch中"$setElementOrder/{key}"的value的值）进行排序，serverOnlyItems按照originalFieldValue（original里原始key的值）进行排序
//   然后将两个排序后的list进行合并
//     插入的顺序是（元素类型是map[string]interface{}）按照在originalFieldValue中mergeKey对应的一样值的位置，或（元素类型是其他类型）在originalFieldValue中位置
//     如果都在originalFieldValue里，则谁在前面，进行先插入
//     否则，patchItems元素先插入。
//   original的原始key的值设为上面合并的结果
//   从patch中移除原始key
// 处理patch里剩余部分（包含前缀"$deleteFromPrimitiveList"和其他）
// 遍历所有patch里key
//   如果key包含前缀"$deleteFromPrimitiveList"，且mergeOptions.MergeParallelList为false，则直接设置original的key的值为patchVal
//   （原来key格式"$deleteFromPrimitiveList/{originalKey}"解析出originalKey，则为originalKey。否则为原来的key
//   处理key对应的patchV是nil
//     则从original中删除这个key（原来key格式"$deleteFromPrimitiveList/{originalKey}"解析出originalKey，则为originalKey。否则为原来的key）
//       如果mergeOptions.IgnoreUnmatchedNulls为true，则这个key处理完
//   original里本来就没有这个key，且key不包含前缀"$deleteFromPrimitiveList"，则设置original的key值为patchV（说明增加字段），这个key处理完
//   original里本来就没有这个key，且key包含前缀"$deleteFromPrimitiveList"，这个key处理完（说明原来就要删除这个key，但是original里就没有这个key）
//   原来original里有这个key，key对应的patchV是nil且mergeOptions.IgnoreUnmatchedNulls为false，key被删除
//     key包含前缀"$deleteFromPrimitiveList"，这个key处理完
//     key不包含前缀"$deleteFromPrimitiveList"，则设置original的key值为patchV（说明明确设置original[key]为nil），这个key处理完
//   处理patchV不为nil
//     origin里原本有这个key，则需要对original[k]和patchV进行合并
//     key不包含前缀"$deleteFromPrimitiveList"（mergeOptions.MergeParallelList可以为true或false），正常的合并
//     key包含前缀"$deleteFromPrimitiveList"（mergeOptions.MergeParallelList一定为true），即original中有部分元素需要被移除
//     合并具体规则：
//       类型不一样
//         key不包含前缀"$deleteFromPrimitiveList"，则original的key值为patchV（说明类型改变了），这个key处理完
//         key包含前缀"$deleteFromPrimitiveList"，这个key处理完
//       都为map类型
//         将original和patch转成map[string]interface{}
//         如果任意一个不是map[string]interface{}，返回错误
//         patchStrategy不为"replace"，则进行map递归（mergeMap）的merge，返回merge后的map[string]interface{}
//         patchStrategy为"replace"，则返回typedPatch（直接替换）
//       都为slice类型
//          fieldPatchStrategy（patch策略）是"merge"
//            original和patch都为空，则不做任何操作
//            元素类型不是map
//              启用MergeParallelList且isDeleteList为true（key包含前缀"$deleteFromPrimitiveList"），从original中移除在patch中的元素，则返回移除后的[]interface{}
//              不启用MergeParallelLis，或启用MergeParallelList且isDeleteList为false（key不包含前缀"$deleteFromPrimitiveList"）
//                对original和patch进行append合并，然后进行去重，赋值为merged
//                将merged进行分类，在patch部分和不在patch部分
//                先将patchItems（在patch部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//                然后将两个排序后的list进行合并
//                合并规则：
//                   如果都在original里，按照在original中位置，谁在前面，进行插入。
//                   如果不都在original里，patchItems（在patch部分）进行插入
//                返回合并后list
//            元素是map
//              mergeKey为空，返回错误
//              进行"$patch"处理（删除或替换）：
//                存在"$patch"，"$patch"的值为"delete"，则original为删除了map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））和patch移除包含"$patch"的map之后的值
//                处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
//                    original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
//                    original里不存在mergeKey和对应的mergeValue，则将patch的元素map append到original（相当于增加新map）
//                  对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//                  先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//                  然后将两个排序后的list进行合并
//                  合并规则：
//                     如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//                     如果不都在original里，patchItems（在patch部分）进行插入
//                  返回合并后list
//              "$patch"的值是"replace"，则original为patch移除包含"$patch"的map之后的值，patch为nil
//                  对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//                  先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//                  然后将两个排序后的list进行合并
//                  合并规则：
//                     如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//                     如果不都在original里，patchItems（在patch部分）进行插入
//                  返回合并后list
//              所有patch元素都不存在"$patch"，original为original, patch为（遍历patch重新append生成的）
//                处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
//                  original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
//                  original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
//                对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//                先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//                然后将两个排序后的list进行合并
//                合并规则：
//                   如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//                   如果不都在original里，patchItems（在patch部分）进行插入
//                返回合并后list
//             存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
//             patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
//        fieldPatchStrategy（patch策略）不是"merge"，返回patch（转成[]interface{}）
//       其他类型，则original的key值为patchV（直接替换）
// 返回最后的original
func mergeMap(original, patch map[string]interface{}, schema LookupPatchMeta, mergeOptions MergeOptions) (map[string]interface{}, error) {
	// patch中存在"$patch"（根节点不会存在"$patch"）
	if v, ok := patch[directiveMarker]; ok {
		// "$patch"的value为"replace"，将"$patch"从patch删除，直接返回patch
		// "$patch"的value为"delete"，返回空的map[string]interface{}
		// 其他情况，返回错误
		return handleDirectiveInMergeMap(v, patch)
	}

	// nil is an accepted value for original to simplify logic in other places.
	// If original is nil, replace it with an empty map and then apply the patch.
	if original == nil {
		original = map[string]interface{}{}
	}

	// patch中没有"$retainKeys"，则直接返回
	// 如果有"$retainKeys"，
	//   从patch中移除"$retainKeys"
	//   不启用MergeParallelList
	//     original里有"$retainKeys"，且"$retainKeys"的值在original和patch中不一样，则返回错误
	//     original里没有，则添加key为"$retainKeys"和value为patch中$retainKeys"的值到original，返回nil
	//   启用MergeParallelList
	//      验证"$retainKeys"的patch中的值是否为切片slice，如果不是切片则返回错误
	//      patch里的key（跳过值是nil或key包含"$deleteFromPrimitiveList"前缀或包含"$setElementOrder"前缀）不在"$retainKeys"在patch中的值中，则返回错误
	//      original里的key不在"$retainKeys"的patch中的值中，则将这个key从original删除
	err := applyRetainKeysDirective(original, patch, mergeOptions)
	if err != nil {
		return nil, err
	}

	// Process $setElementOrder list and other lists sharing the same key.
	// When not merging the directive, it will make sure $setElementOrder list exist only in original.
	// When merging the directive, it will process $setElementOrder and its patch list together.
	// This function will delete the merged elements from patch so they will not be reprocessed
	// 处理patch中key格式为"$setElementOrder/{key}"
	// 未启用MergeParallelList
	//   original里也存在相同"$setElementOrder"前缀的key，且在original里的值和在patch里的值不一样，则返回错误
	//   original里不存在相同"$setElementOrder"前缀的key，则添加key为key，value为key在patch里的值
	// 从patch中删除这个key
	// patch中"$setElementOrder/{key}"的value的值类型不为slice，则返回错误
	// originalKey为从key格式"$setElementOrder/{key}"中提取出"/"后半部分的值
	// 在original里存在这个原始key，如果值的类型不是slice，则返回错误
	// 在patch中存在这个原始的key，如果值的类型不是slice，则返回错误
	// 对original和patch原始key的元素进行merge（在original里存在这个原始key和在patch中存在这个原始的key，至少有一个为true。都不为true则跳过patch中的这个key）
	//   在original里存在这个原始key，且在patch中不存在这个原始的key，则merged为originalFieldValue（original里这个原始key的值）
	//   在original里不存在这个原始key，且在patch中存在这个原始key，则merged为patchFieldValue（patch里这个原始key的值）
	//   在original里存在这个原始key，且在patch中存在这个原始key，则对originalFieldValue和patchFieldValue这两个值进行merge
	//   mergeKey为空，将merged进行分类，patchItems（在typedSetElementOrderList部分）和serverOnlyItems（不在typedSetElementOrderList部分）
	//   mergeKey不为空，将merged进行分类，patchItems（跟typedSetElementOrderList有相同mergeKey和对应mergeKeyValue）部分，serverOnlyItems（没有相同的mergeKey和对应mergeKeyValue部分）
	//      merged和typedSetElementOrderList元素类型不是map[string]interface{}，返回错误
	// 先将patchItems按照typedSetElementOrderList（patch中"$setElementOrder/{key}"的value的值）进行排序，serverOnlyItems按照originalFieldValue（original里原始key的值）进行排序
	//   然后将两个排序后的list进行合并
	//   插入的顺序是（元素类型是map[string]interface{}）按照在originalFieldValue中mergeKey对应的一样值的位置，或（元素类型是其他类型）在originalFieldValue中位置
	//   如果都在originalFieldValue里，则谁在前面，进行先插入
	//   否则，patchItems元素先插入。
	// original的原始key的值设为上面合并的结果
	// 从patch中移除原始key
	err = mergePatchIntoOriginal(original, patch, schema, mergeOptions)
	if err != nil {
		return nil, err
	}

	// Start merging the patch into the original.
	for k, patchV := range patch {
		// 返回值为应该跳过这个key，是否为删除列表，去掉前缀后剩下后半部分的内容
		// skipProcessing（应该跳过这个key）为true：key包含前缀"$deleteFromPrimitiveList"，且mergeOptions.MergeParallelList为false。
		// isDeleteList（删除列表）为true：key包含前缀"$deleteFromPrimitiveList"，且mergeOptions.MergeParallelList为true
		// noPrefixKey：去掉前缀后剩下后半部分的内容
		// 如果key包含前缀"$deleteFromPrimitiveList"，且mergeOptions.MergeParallelList为false，则直接设置original的key的值为patchVal，返回true, false, "", nil
		// 如果key包含前缀"$deleteFromPrimitiveList"，且mergeOptions.MergeParallelList为true，从key中（格式"$deleteFromPrimitiveList/{originalKey}"）解析出originalKey，返回false, true, originalKey, err
		// 否则，返回false, false, "", nil
		skipProcessing, isDeleteList, noPrefixKey, err := preprocessDeletionListForMerging(k, original, patchV, mergeOptions.MergeParallelList)
		if err != nil {
			return nil, err
		}
		// key包含前缀"$deleteFromPrimitiveList"且mergeOptions.MergeParallelList为false（skipProcessing为true），则继续下一个key
		if skipProcessing {
			continue
		}
		// key包含前缀"$deleteFromPrimitiveList"，且mergeOptions.MergeParallelList为true，从key中（格式"$deleteFromPrimitiveList/{originalKey}"）解析出originalKey
		if len(noPrefixKey) > 0 {
			k = noPrefixKey
		}

		// If the value of this key is null, delete the key if it exists in the
		// original. Otherwise, check if we want to preserve it or skip it.
		// Preserving the null value is useful when we want to send an explicit
		// delete to the API server.
		// key对应的patchV是nil，则从original中删除这个key
		// 如果mergeOptions.IgnoreUnmatchedNulls为true，则继续下一个key。
		if patchV == nil {
			delete(original, k)
			if mergeOptions.IgnoreUnmatchedNulls {
				continue
			}
		}

		_, ok := original[k]
		// original里没有这个key
		// （可能原来original里有这个key，key对应的patchV是nil且mergeOptions.IgnoreUnmatchedNulls为false，key被删除）
		// 或original里本来就没有这个key
		// 继续下一个key
		// （patchV为nil在这里处理完了，继续下一个key）
		if !ok {
			// key不包含前缀"$deleteFromPrimitiveList"（mergeOptions.MergeParallelList的值可以是true或false），则设置original的key值为patchV（说明增加字段，当patchV为nil时候且mergeOptions.IgnoreUnmatchedNulls为false，说明明确设置original[k]为nil）
			if !isDeleteList {
				// If it's not in the original document, just take the patch value.
				original[k] = patchV
			}
			// 继续下一个key
			continue
		}

		// 这里patchV一定不为nil
		// origin里原本有这个key，则需要对original[k]和patchV进行合并
		// key不包含前缀"$deleteFromPrimitiveList"（mergeOptions.MergeParallelList可以为true或false），正常的合并
		// key包含前缀"$deleteFromPrimitiveList"（mergeOptions.MergeParallelList一定为true），即original中有部分元素需要被移除

		originalType := reflect.TypeOf(original[k])
		patchType := reflect.TypeOf(patchV)
		// 类型不一样
		if originalType != patchType {
			// key不包含前缀"$deleteFromPrimitiveList"，则original的key值为patchV（说明类型改变了）
			if !isDeleteList {
				original[k] = patchV
			}
			continue
		}
		// If they're both maps or lists, recurse into the value.
		switch originalType.Kind() {
		// 都为map类型
		case reflect.Map:
			// 如果schema是PatchMetaFromOpenAPI
			// 从schema.schema中查找key字段对应的openapi.Schema，设置为返回的第一个字段(PatchMetaFromOpenAPI{Schema: {key字段对应的openapi.Schema}})
			// 获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，设置返回的第二个字段PatchMeta的patchMergeKey和patchStrategies字段
			subschema, patchMeta, err2 := schema.LookupPatchMetadataForStruct(k)
			if err2 != nil {
				return nil, err2
			}
			// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
			// 当strategies的数量大于等于3个，则返回错误
			// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
			_, patchStrategy, err2 := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
			if err2 != nil {
				return nil, err2
			}
			// 将original和patch转成map[string]interface{}
			// 如果任意一个不是map[string]interface{}，返回错误
			// patchStrategy不为"replace"，则进行map递归（mergeMap）的merge，返回merge后的map[string]interface{}
			// patchStrategy为"replace"，则返回typedPatch（直接替换）
			original[k], err = mergeMapHandler(original[k], patchV, subschema, patchStrategy, mergeOptions)
		// 都为slice类型
		case reflect.Slice:
			// 如果schema是PatchMetaFromOpenAPI
			// 返回slice的item类型Schema，组装成第一个返回值PatchMetaFromOpenAPI{Schema: sliceItem.subschema}
			// s.Schema类型是proto.*Kind，则调用sliceItem.VisitKind(s.Schema)
			//     从s.Schema中查找sliceItem.key字段对应Schema
			//     从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成第二个返回值PatchMeta
			// 否则，第二个参数为PatchMeta{}（空的PatchMeta）
			subschema, patchMeta, err2 := schema.LookupPatchMetadataForSlice(k)
			if err2 != nil {
				return nil, err2
			}
			// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
			// 当strategies的数量大于等于3个，则返回错误
			// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
			_, patchStrategy, err2 := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
			if err2 != nil {
				return nil, err2
			}
			// 如果original和patch任意一个不是[]interface{}，返回错误
			// fieldPatchStrategy（patch策略）是"merge"
			// original和patch都为空，则不做任何操作
			// 元素类型不是map
			//   启用MergeParallelList且isDeleteList为true，从original中移除在patch中的元素，则返回移除后的[]interface{}
			//   不启用MergeParallelLis，或启用MergeParallelList且isDeleteList为false
			//     对original和patch进行合并append，然后进行去重，赋值为merged
			//     将merged进行分类，在patch部分和不在patch部分
			//     先将patchItems（在patch部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//     然后将两个排序后的list进行合并
			//     合并规则：
			//        如果都在original里，按照在original中位置，谁在前面，进行插入。
			//        如果不都在original里，patchItems（在patch部分）进行插入
			//     返回合并后list
			// 元素是map
			//   mergeKey为空，返回错误
			//   进行"$patch"处理（删除或替换）：
			//     存在"$patch"，"$patch"的值为"delete"，则original为删除map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））和patch移除包含"$patch"的map之后的值
			//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
			//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
			//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
			//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
			//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//         然后将两个排序后的list进行合并
			//         合并规则：
			//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
			//            如果不都在original里，patchItems（在patch部分）进行插入
			//         返回合并后list
			//     "$patch"的值是"replace"，则original为patch移除包含"$patch"的map之后的值，patch为nil
			//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
			//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//         然后将两个排序后的list进行合并
			//         合并规则：
			//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
			//            如果不都在original里，patchItems（在patch部分）进行插入
			//         返回合并后list
			//      所有patch元素都不存在"$patch"，original为original, patch为（遍历patch重新append生成的）
			//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
			//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
			//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
			//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
			//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
			//         然后将两个排序后的list进行合并
			//         合并规则：
			//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
			//            如果不都在original里，patchItems（在patch部分）进行插入
			//         返回合并后list
			//      存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
			//      patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
			// fieldPatchStrategy（patch策略）不是"merge"，返回patch（转成[]interface{}）
			original[k], err = mergeSliceHandler(original[k], patchV, subschema, patchStrategy, patchMeta.GetPatchMergeKey(), isDeleteList, mergeOptions)
		// 其他类型，则original的key值为patchV（直接替换）
		default:
			original[k] = patchV
		}
		if err != nil {
			return nil, err
		}
	}
	return original, nil
}

// mergeMapHandler handles how to merge `patchV` whose key is `key` with `original` respecting
// fieldPatchStrategy and mergeOptions.
// 将original和patch转成map[string]interface{}
// 如果任意一个不是map[string]interface{}，返回错误
// fieldPatchStrategy不为"replace"，则进行map的merge，返回merge后的map[string]interface{}
// fieldPatchStrategy为"replace"，则返回typedPatch（直接替换）
func mergeMapHandler(original, patch interface{}, schema LookupPatchMeta,
	fieldPatchStrategy string, mergeOptions MergeOptions) (map[string]interface{}, error) {
	// 将original和patch转成map[string]interface{}
	// 如果任意一个不是map[string]interface{}，返回错误
	typedOriginal, typedPatch, err := mapTypeAssertion(original, patch)
	if err != nil {
		return nil, err
	}

	// fieldPatchStrategy不为"replace"，则进行map的merge
	if fieldPatchStrategy != replaceDirective {
		return mergeMap(typedOriginal, typedPatch, schema, mergeOptions)
	} else {
		// fieldPatchStrategy为"replace"，则返回typedPatch（直接替换）
		return typedPatch, nil
	}
}

// mergeSliceHandler handles how to merge `patchV` whose key is `key` with `original` respecting
// fieldPatchStrategy, fieldPatchMergeKey, isDeleteList and mergeOptions.
// 如果original和patch任意一个不是[]interface{}，返回错误
// fieldPatchStrategy（patch策略）是"merge"
// original和patch都为空，则不做任何操作
// 元素类型不是map
//   启用MergeParallelList且isDeleteList为true，从original中移除在patch中的元素，则返回移除后的[]interface{}
//   不启用MergeParallelLis，或启用MergeParallelList且isDeleteList为false
//     对original和patch进行合并，然后进行去重，赋值为merged
//     将merged进行分类，在patch部分和不在patch部分
//     先将patchItems（在patch部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//     然后将两个排序后的list进行合并
//     合并规则：
//        如果都在original里，按照在original中位置，谁在前面，进行插入。
//        如果不都在original里，patchItems（在patch部分）进行插入
//     返回合并后list
// 元素是map
//   mergeKey为空，返回错误
//   进行"$patch"处理（删除或替换）：
//     存在"$patch"，"$patch"的值为"delete"，则original为删除map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））和patch移除包含"$patch"的map之后的值
//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//         然后将两个排序后的list进行合并
//         合并规则：
//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//            如果不都在original里，patchItems（在patch部分）进行插入
//         返回合并后list
//     "$patch"的值是"replace"，则original为patch移除包含"$patch"的map之后的值，patch为nil
//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//         然后将两个排序后的list进行合并
//         合并规则：
//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//            如果不都在original里，patchItems（在patch部分）进行插入
//         返回合并后list
//      所有patch元素都不存在"$patch"，original为original, patch为（遍历patch重新append生成的）
//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//         然后将两个排序后的list进行合并
//         合并规则：
//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//            如果不都在original里，patchItems（在patch部分）进行插入
//         返回合并后list
//      存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
//      patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
// fieldPatchStrategy（patch策略）不是"merge"，返回patch（转成[]interface{}）
func mergeSliceHandler(original, patch interface{}, schema LookupPatchMeta,
	fieldPatchStrategy, fieldPatchMergeKey string, isDeleteList bool, mergeOptions MergeOptions) ([]interface{}, error) {
	// original和patch都转成[]interface{}
	// 如果original和patch任意一个不是[]interface{}，返回错误
	typedOriginal, typedPatch, err := sliceTypeAssertion(original, patch)
	if err != nil {
		return nil, err
	}

	// fieldPatchStrategy（patch策略）是"merge"
	if fieldPatchStrategy == mergeDirective {
		// original和patch都为空，则不做任何操作
		// 元素类型不是map
		//   启用MergeParallelList且isDeleteList为true，从original中移除在patch中的元素，则返回移除后的[]interface{}
		//   不启用MergeParallelLis，或启用MergeParallelList且isDeleteList为false
		//     对original和patch进行合并append，然后进行去重，赋值为merged
		//     将merged进行分类，在patch部分和不在patch部分
		//     先将patchItems（在patch部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
		//     然后将两个排序后的list进行合并
		//     合并规则：
		//        如果都在original里，按照在original中位置，谁在前面，进行插入。
		//        如果不都在original里，patchItems（在patch部分）进行插入
		//     返回合并后list
		// 元素是map
		//   mergeKey为空，返回错误
		//   进行"$patch"处理（删除或替换）：
		//     存在"$patch"，"$patch"的值为"delete"，则original为删除map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））和patch移除包含"$patch"的map之后的值
		//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
		//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
		//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
		//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
		//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
		//         然后将两个排序后的list进行合并
		//         合并规则：
		//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
		//            如果不都在original里，patchItems（在patch部分）进行插入
		//         返回合并后list
		//     "$patch"的值是"replace"，则original为patch移除包含"$patch"的map之后的值，patch为nil
		//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
		//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
		//         然后将两个排序后的list进行合并
		//         合并规则：
		//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
		//            如果不都在original里，patchItems（在patch部分）进行插入
		//         返回合并后list
		//      所有patch元素都不存在"$patch"，original为original, patch为（遍历patch重新append生成的）
		//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
		//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
		//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
		//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
		//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
		//         然后将两个排序后的list进行合并
		//         合并规则：
		//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
		//            如果不都在original里，patchItems（在patch部分）进行插入
		//         返回合并后list
		//      存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
		//      patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
		return mergeSlice(typedOriginal, typedPatch, schema, fieldPatchMergeKey, mergeOptions, isDeleteList)
	} else {
		// fieldPatchStrategy（patch策略）不是"merge"，返回patch（转成[]interface{}）
		return typedPatch, nil
	}
}

// Merge two slices together. Note: This may modify both the original slice and
// the patch because getting a deep copy of a slice in golang is highly
// non-trivial.
// original和patch都为空，则不做任何操作
// 元素类型不是map
//   启用MergeParallelList且isDeleteList为true，从original中移除在patch中的元素，则返回移除后的[]interface{}
//   不启用MergeParallelLis，或启用MergeParallelList且isDeleteList为false
//     对original和patch进行合并append，然后进行去重，赋值为merged
//     将merged进行分类，在patch部分和不在patch部分
//     先将patchItems（在patch部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//     然后将两个排序后的list进行合并
//     合并规则：
//        如果都在original里，按照在original中位置，谁在前面，进行插入。
//        如果不都在original里，patchItems（在patch部分）进行插入
//     返回合并后list
// 元素是map
//   mergeKey为空，返回错误
//   进行"$patch"处理（删除或替换）：
//     存在"$patch"，"$patch"的值为"delete"，则original为删除map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））和patch移除包含"$patch"的map之后的值
//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//         然后将两个排序后的list进行合并
//         合并规则：
//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//            如果不都在original里，patchItems（在patch部分）进行插入
//         返回合并后list
//     "$patch"的值是"replace"，则original为patch移除包含"$patch"的map之后的值，patch为nil
//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//         然后将两个排序后的list进行合并
//         合并规则：
//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//            如果不都在original里，patchItems（在patch部分）进行插入
//         返回合并后list
//      所有patch元素都不存在"$patch"，original为original, patch为（遍历patch重新append生成的）
//         处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
//           original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
//           original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
//         对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
//         先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
//         然后将两个排序后的list进行合并
//         合并规则：
//            如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
//            如果不都在original里，patchItems（在patch部分）进行插入
//         返回合并后list
//      存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
//      patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
func mergeSlice(original, patch []interface{}, schema LookupPatchMeta, mergeKey string, mergeOptions MergeOptions, isDeleteList bool) ([]interface{}, error) {
	// original和patch都为空，则不做任何操作
	if len(original) == 0 && len(patch) == 0 {
		return original, nil
	}

	// All the values must be of the same type, but not a list.
	// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误，嵌套slice，则返回错误
	// 否则返回slice内部元素类型
	t, err := sliceElementType(original, patch)
	if err != nil {
		return nil, err
	}

	var merged []interface{}
	kind := t.Kind()
	// If the elements are not maps, merge the slices of scalars.
	// 元素类型不是map
	if kind != reflect.Map {
		// 启用MergeParallelList且isDeleteList为true
		if mergeOptions.MergeParallelList && isDeleteList {
			// 从original中移除在patch中的元素，返回移除后的[]interface{}
			return deleteFromSlice(original, patch), nil
		}
		// Maybe in the future add a "concat" mode that doesn't
		// deduplicate.
		// 不启用MergeParallelLis，或启用MergeParallelList且isDeleteList为false
		both := append(original, patch...)
		// 返回不重复的slice
		merged = deduplicateScalars(both)

	} else {
		// 元素类型是map
		// mergeKey为空，返回错误
		if mergeKey == "" {
			return nil, fmt.Errorf("cannot merge lists without merge key for %s", schema.Name())
		}

		// 遍历patch []map[string]interface{}，对是否存在"$patch"进行不同的处理（处理删除和替换）
		// 存在"$patch"，"$patch"的值为"delete"，则返回删除map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））的original和patch移除包含"$patch"的map之后的值
		// "$patch"的值是"replace"，返回patch移除包含"$patch"的map之后的值，nil
		// patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
		// 存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
		// 所有patch元素都不存在"$patch"，返回original, patch（重新append生成的）
		original, patch, err = mergeSliceWithSpecialElements(original, patch, mergeKey)
		if err != nil {
			return nil, err
		}

		// 处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
		// 上面patch里元素map存在"$patch"的值是"replace"（因为上面处理后，这里的patch为nil），则这里merged就是上面的original
		// 
		// patch为nil，返回original
		// patch不为nil，遍历patch中元素map，不存在mergeKey，则返回错误
		// original元素里存在mergeKey和对应的mergeValue，则递归进行这个元素origin和patch的map聚合，替换original这个index元素的值
		// original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
		// 返回original
		merged, err = mergeSliceWithoutSpecialElements(original, patch, mergeKey, schema, mergeOptions)
		if err != nil {
			return nil, err
		}
	}

	// enforce the order
	var patchItems, serverOnlyItems []interface{}
	// mergeKey为空（元素类型一定不是map，如果mergeKey不为空，下面else里直接报错）
	if len(mergeKey) == 0 {
		// 将merged进行分类，返回在patch部分和不在patch部分
		patchItems, serverOnlyItems = partitionPrimitivesByPresentInList(merged, patch)
	} else {
		// 将merged进行分类，返回跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
		patchItems, serverOnlyItems, err = partitionMapsByPresentInList(merged, patch, mergeKey)
		if err != nil {
			return nil, err
		}
	}
	// 先将patchItems按照patch进行排序，serverOnlyItems按照original进行排序
	// 然后将两个排序后的list进行合并
	// 插入的顺序是（元素类型是map[string]interface{}）按照在original中mergeKey对应的一样值的位置，或（元素类型是其他类型）在original中位置
	// 如果都在original里，则谁在前面，进行先插入
	// 否则，patchItems元素先插入。
	return normalizeElementOrder(patchItems, serverOnlyItems, patch, original, mergeKey, kind)
}

// mergeSliceWithSpecialElements handles special elements with directiveMarker
// before merging the slices. It returns a updated `original` and a patch without special elements.
// original and patch must be slices of maps, they should be checked before calling this function.
// 遍历patch []map[string]interface{}，对是否存在"$patch"进行不同的处理
// 存在"$patch"，"$patch"的值为"delete"，则返回删除map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））的original和patch移除包含"$patch"的map之后的值
// "$patch"的值是"replace"，返回patch移除包含"$patch"的map之后的值，nil
// patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
// 存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
// 所有patch元素都不存在"$patch"，返回original, patch（重新append生成的）
func mergeSliceWithSpecialElements(original, patch []interface{}, mergeKey string) ([]interface{}, []interface{}, error) {
	patchWithoutSpecialElements := []interface{}{}
	replace := false
	for _, v := range patch {
		typedV := v.(map[string]interface{})
		// 是否存在"$patch"
		patchType, ok := typedV[directiveMarker]
		if !ok {
			// 元素不存在"$patch"，则将v append到patchWithoutSpecialElements
			patchWithoutSpecialElements = append(patchWithoutSpecialElements, v)
		} else {
			// 存在"$patch"
			switch patchType {
			// "$patch"的值为"delete"
			case deleteDirective:
				mergeValue, ok := typedV[mergeKey]
				// 存在mergeKey
				if ok {
					var err error
					// 从original里删除元素（元素存在mergeKey和对应的mergeValue），返回删除后的original
					original, err = deleteMatchingEntries(original, mergeKey, mergeValue)
					if err != nil {
						return nil, nil, err
					}
				} else {
					// 不存在mergeKey，直接返回错误
					return nil, nil, mergepatch.ErrNoMergeKey(typedV, mergeKey)
				}
			// "$patch"的值是"replace"
			case replaceDirective:
				replace = true
				// Continue iterating through the array to prune any other $patch elements.
			case mergeDirective:
				// "$patch"的值是"merge"返回错误
				return nil, nil, fmt.Errorf("merging lists cannot yet be specified in the patch")
			default:
				// "$patch"是其他值，返回错误
				return nil, nil, mergepatch.ErrBadPatchType(patchType, typedV)
			}
		}
	}
	// "$patch"的值是"replace"，返回patch移除"$patch"项之后的值，nil
	if replace {
		return patchWithoutSpecialElements, nil, nil
	}
	// $patch"的值为"delete"，返回删除元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））的original和patch移除包含"$patch"的map之后的值
	// 所有patch元素都不存在"$patch"，返回original, patch（重新append生成的）
	return original, patchWithoutSpecialElements, nil
}

// delete all matching entries (based on merge key) from a merging list
// 从original里删除元素（元素存在mergeKey和对应的mergeValue），返回删除后的original
func deleteMatchingEntries(original []interface{}, mergeKey string, mergeValue interface{}) ([]interface{}, error) {
	for {
		// 遍历original，如果存在mergeKey和对应的mergeValue，则返回这个元素map[string]interface{}，index，true，nil。否则返回nil, 0, false, nil
		// 元素类型不是map[string]interface{}，返回错误
		_, originalKey, found, err := findMapInSliceBasedOnKeyValue(original, mergeKey, mergeValue)
		if err != nil {
			return nil, err
		}

		if !found {
			break
		}
		// Delete the element at originalKey.
		original = append(original[:originalKey], original[originalKey+1:]...)
	}
	return original, nil
}

// mergeSliceWithoutSpecialElements merges slices with non-special elements.
// original and patch must be slices of maps, they should be checked before calling this function.
// patch为nil，返回original
// patch不为nil，遍历patch中元素map，不存在mergeKey，则返回错误
// original元素里存在mergeKey和对应的mergeValue，则递归进行这个元素origin和patch的map聚合，替换original这个index元素的值
// original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original
// 返回original
func mergeSliceWithoutSpecialElements(original, patch []interface{}, mergeKey string, schema LookupPatchMeta, mergeOptions MergeOptions) ([]interface{}, error) {
	for _, v := range patch {
		typedV := v.(map[string]interface{})
		mergeValue, ok := typedV[mergeKey]
		// 不存在mergeKey，返回错误
		if !ok {
			return nil, mergepatch.ErrNoMergeKey(typedV, mergeKey)
		}

		// If we find a value with this merge key value in original, merge the
		// maps. Otherwise append onto original.
		// 遍历original，如果存在mergeKey和mergeValue，则返回这个元素map[string]interface{}，index，true，nil。否则返回nil, 0, false, nil
		// 元素类型不是map[string]interface{}，返回错误
		originalMap, originalKey, found, err := findMapInSliceBasedOnKeyValue(original, mergeKey, mergeValue)
		if err != nil {
			return nil, err
		}

		// original里存在mergeKey和对应的mergeValue
		if found {
			var mergedMaps interface{}
			var err error
			// Merge into original.
			// 递归进行下一层map聚合
			mergedMaps, err = mergeMap(originalMap, typedV, schema, mergeOptions)
			if err != nil {
				return nil, err
			}

			original[originalKey] = mergedMaps
		} else {
			// original里不存在mergeKey和对应的mergeValue，则v append到original
			original = append(original, v)
		}
	}
	return original, nil
}

// deleteFromSlice uses the parallel list to delete the items in a list of scalars
// 从current中移除在toDelete中的元素，返回移除后的[]interface{}
func deleteFromSlice(current, toDelete []interface{}) []interface{} {
	toDeleteMap := map[interface{}]interface{}{}
	processed := make([]interface{}, 0, len(current))
	for _, v := range toDelete {
		toDeleteMap[v] = true
	}
	for _, v := range current {
		if _, found := toDeleteMap[v]; !found {
			processed = append(processed, v)
		}
	}
	return processed
}

// This method no longer panics if any element of the slice is not a map.
// 遍历m，如果存在key和value，则返回这个元素map[string]interface{}，index，true，nil。否则返回nil, 0, false, nil
// 元素类型不是map[string]interface{}，返回错误
func findMapInSliceBasedOnKeyValue(m []interface{}, key string, value interface{}) (map[string]interface{}, int, bool, error) {
	for k, v := range m {
		typedV, ok := v.(map[string]interface{})
		if !ok {
			return nil, 0, false, fmt.Errorf("value for key %v is not a map", k)
		}

		valueToMatch, ok := typedV[key]
		if ok && valueToMatch == value {
			return typedV, k, true, nil
		}
	}

	return nil, 0, false, nil
}

// This function takes a JSON map and sorts all the lists that should be merged
// by key. This is needed by tests because in JSON, list order is significant,
// but in Strategic Merge Patch, merge lists do not have significant order.
// Sorting the lists allows for order-insensitive comparison of patched maps.
func sortMergeListsByName(mapJSON []byte, schema LookupPatchMeta) ([]byte, error) {
	var m map[string]interface{}
	err := json.Unmarshal(mapJSON, &m)
	if err != nil {
		return nil, mergepatch.ErrBadJSONDoc
	}

	newM, err := sortMergeListsByNameMap(m, schema)
	if err != nil {
		return nil, err
	}

	return json.Marshal(newM)
}

// Function sortMergeListsByNameMap recursively sorts the merge lists by its mergeKey in a map.
// 对s（map[string]interface）的每一个value进行排序处理
// key是"$retainKeys"，且value为slice，则value按照元素的%v的字符排序（value不为slice返回错误）
// key包含前缀"$deleteFromPrimitiveList"，且value是slice类型，则value按照元素的%v的字符排序（value不为slice返回错误）
// key包含"$setElementOrder"前缀，value必须是slice类型（value不为slice返回错误）
// key不是"$patch"
//   如果value类型为map[string]interface{}，则继续对嵌套的内部map做排序处理
//   value类型为[]interface{}，则继续内部元素进行排序
//   value是其他类型，则不变
// key是剩下情况（"$patch"），则不变
func sortMergeListsByNameMap(s map[string]interface{}, schema LookupPatchMeta) (map[string]interface{}, error) {
	newS := map[string]interface{}{}
	for k, v := range s {
		// key是"$retainKeys"，且value为slice，则value按照元素的%v的字符排序
		if k == retainKeysDirective {
			// value不是slice返回错误
			typedV, ok := v.([]interface{})
			if !ok {
				return nil, mergepatch.ErrBadPatchFormatForRetainKeys
			}
			// 按照元素的%v的字符排序
			v = sortScalars(typedV)
		// key包含前缀"$deleteFromPrimitiveList"，且value是slice类型，则value按照元素的%v的字符排序
		} else if strings.HasPrefix(k, deleteFromPrimitiveListDirectivePrefix) {
			typedV, ok := v.([]interface{})
			if !ok {
				return nil, mergepatch.ErrBadPatchFormatForPrimitiveList
			}
			// 按照元素的%v的字符排序
			v = sortScalars(typedV)
		// key包含"$setElementOrder"前缀
		} else if strings.HasPrefix(k, setElementOrderDirectivePrefix) {
			_, ok := v.([]interface{})
			if !ok {
				// value不是slice类型返回错误
				return nil, mergepatch.ErrBadPatchFormatForSetElementOrderList
			}
		// key不是"$patch"
		} else if k != directiveMarker {
			// recurse for map and slice.
			switch typedV := v.(type) {
			// value类型为map[string]interface{}
			case map[string]interface{}:
				// 如果schema为PatchMetaFromOpenAPI
				// 从s.schema中查找key字段对应的openapi.Schema，设置为返回的第一个字段(PatchMetaFromOpenAPI{Schema: {key字段对应的openapi.Schema}})
				// 获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，设置返回的第二个字段PatchMeta的patchMergeKey和patchStrategies字段
				subschema, _, err := schema.LookupPatchMetadataForStruct(k)
				if err != nil {
					return nil, err
				}
				// 继续进行内部的map的各个value的排序
				v, err = sortMergeListsByNameMap(typedV, subschema)
				if err != nil {
					return nil, err
				}
			// value类型为[]interface{}
			case []interface{}:
				// 如果schema为PatchMetaFromOpenAPI
				// 返回slice的item类型Schema，组装成第一个返回值PatchMetaFromOpenAPI{Schema: sliceItem.subschema}
				// s.Schema类型是proto.*Kind，则调用sliceItem.VisitKind(s.Schema)
				//     从s.Schema中查找sliceItem.key字段对应Schema
				//     从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成第二个返回值PatchMeta
				// 否则，第二个参数为PatchMeta{}（空的PatchMeta）
				subschema, patchMeta, err := schema.LookupPatchMetadataForSlice(k)
				if err != nil {
					return nil, err
				}
				// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
				// 当strategies的数量大于等于3个，则返回错误
				// 当strategies的数量大于等于2个，且没有包含"retainKeys"，则返回错误
				_, patchStrategy, err := extractRetainKeysPatchStrategy(patchMeta.GetPatchStrategies())
				if err != nil {
					return nil, err
				}
				// "x-kubernetes-patch-strategy"的value里，其他非"retainKeys"值为"merge"
				if patchStrategy == mergeDirective {
					var err error
					// 继续对slice元素进行排序
					v, err = sortMergeListsByNameArray(typedV, subschema, patchMeta.GetPatchMergeKey(), true)
					if err != nil {
						return nil, err
					}
				}
			}
		}

		newS[k] = v
	}

	return newS, nil
}

// Function sortMergeListsByNameMap recursively sorts the merge lists by its mergeKey in an array.
// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误
// 默认slice里的元素类型都是一样的
// 如果slice内部元素类型不是map类型，则对slice进行去重并按照元素的%v的字符排序，并返回
// 否则slice内部的元素是map类型
//    如果递归调用过来的（recurse为true），则对map[string]interface）的每一个value进行排序处理
//        key是"$retainKeys"，且value为slice，则value按照元素的%v的字符排序（value不为slice返回错误）
//        key包含前缀"$deleteFromPrimitiveList"，且value是slice类型，则value按照元素的%v的字符排序（value不为slice返回错误）
//        key包含"$setElementOrder"前缀，value必须是slice类型（value不为slice返回错误）
//        key不是"$patch"
//          如果value类型为map[string]interface{}，则继续对嵌套的内部map做排序处理
//          value类型为[]interface{}，则继续进行对元素递归排序
//          value是其他类型，则不变
//    如果不是递归调用过来的（recurse为true），则map的每个value不进行排序
// 根据map的key为fieldName的值，进行map排序
func sortMergeListsByNameArray(s []interface{}, schema LookupPatchMeta, mergeKey string, recurse bool) ([]interface{}, error) {
	if len(s) == 0 {
		return s, nil
	}

	// We don't support lists of lists yet.
	// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误
	// 否则返回slice内部元素类型
	t, err := sliceElementType(s)
	if err != nil {
		return nil, err
	}

	// If the elements are not maps...
	// 如果slice内部元素类型不是map
	if t.Kind() != reflect.Map {
		// Sort the elements, because they may have been merged out of order.
		// 对slice进行去重并按照元素的%v的字符排序
		return deduplicateAndSortScalars(s), nil
	}

	// Elements are maps - if one of the keys of the map is a map or a
	// list, we may need to recurse into it.
	// slice内部的元素是map类型
	newS := []interface{}{}
	for _, elem := range s {
		// 递归调用过来的
		if recurse {
			typedElem := elem.(map[string]interface{})
			// 对typedElem（map[string]interface）的每一个value进行排序处理
			// key是"$retainKeys"，且value为slice，则value按照元素的%v的字符排序（value不为slice返回错误）
			// key包含前缀"$deleteFromPrimitiveList"，且value是slice类型，则value按照元素的%v的字符排序（value不为slice返回错误）
			// key包含"$setElementOrder"前缀，value必须是slice类型（value不为slice返回错误）
			// key不是"$patch"
			//   如果value类型为map[string]interface{}，则继续对嵌套的内部map做排序处理
			//   value类型为[]interface{}，则继续进行对元素递归排序
			//   value是其他类型，则不变
			// key是剩下情况（"$patch"），则不变
			newElem, err := sortMergeListsByNameMap(typedElem, schema)
			if err != nil {
				return nil, err
			}

			newS = append(newS, newElem)
		} else {
			// 不是递归调用过来
			newS = append(newS, elem)
		}
	}

	// Sort the maps.
	// newS的元素类型一定是map[string]interface{}
	// 根据map的key为fieldName的值，进行map排序
	newS = sortMapsBasedOnField(newS, mergeKey)
	return newS, nil
}

// m的元素类型一定是map[string]interface{}
// 根据map的key为fieldName的值，进行map排序
func sortMapsBasedOnField(m []interface{}, fieldName string) []interface{} {
	// m的元素类型一定是map[string]interface{}
	// 将[]interface{}转成[]map[string]interface{}
	mapM := mapSliceFromSlice(m)
	// 根据map的key为fieldName的值，进行map排序
	ss := SortableSliceOfMaps{mapM, fieldName}
	sort.Sort(ss)
	// 将[]map[string]interface{}转成[]interface{}
	newS := sliceFromMapSlice(ss.s)
	return newS
}

// 将[]interface{}转成[]map[string]interface{}
func mapSliceFromSlice(m []interface{}) []map[string]interface{} {
	newM := []map[string]interface{}{}
	for _, v := range m {
		vt := v.(map[string]interface{})
		newM = append(newM, vt)
	}

	return newM
}

// 将[]map[string]interface{}转成[]interface{}
func sliceFromMapSlice(s []map[string]interface{}) []interface{} {
	newS := []interface{}{}
	for _, v := range s {
		newS = append(newS, v)
	}

	return newS
}

type SortableSliceOfMaps struct {
	s []map[string]interface{}
	k string // key to sort on
}

func (ss SortableSliceOfMaps) Len() int {
	return len(ss.s)
}

// 根据map的key为k的值，进行map排序
func (ss SortableSliceOfMaps) Less(i, j int) bool {
	iStr := fmt.Sprintf("%v", ss.s[i][ss.k])
	jStr := fmt.Sprintf("%v", ss.s[j][ss.k])
	return sort.StringsAreSorted([]string{iStr, jStr})
}

func (ss SortableSliceOfMaps) Swap(i, j int) {
	tmp := ss.s[i]
	ss.s[i] = ss.s[j]
	ss.s[j] = tmp
}

// 对slice进行去重并按照元素的%v的字符排序
func deduplicateAndSortScalars(s []interface{}) []interface{} {
	// 返回不重复的slice
	s = deduplicateScalars(s)
	return sortScalars(s)
}

// 按照元素的%v的字符排序
func sortScalars(s []interface{}) []interface{} {
	ss := SortableSliceOfScalars{s}
	sort.Sort(ss)
	return ss.s
}

// 返回不重复的slice
func deduplicateScalars(s []interface{}) []interface{} {
	// Clever algorithm to deduplicate.
	length := len(s) - 1
	for i := 0; i < length; i++ {
		for j := i + 1; j <= length; j++ {
			if s[i] == s[j] {
				// 将最后一个元素设置为s[j]
				s[j] = s[length]
				// 设置s为原来的s去掉最后一个元素（最后一个元素已经在s[j]）
				s = s[0:length]
				// 长度减一
				length--
				// 重复执行index j的比较（这里j--，循环后j++）
				j--
			}
		}
	}

	return s
}

type SortableSliceOfScalars struct {
	s []interface{}
}

func (ss SortableSliceOfScalars) Len() int {
	return len(ss.s)
}

// 按照元素的%v的字符排序
func (ss SortableSliceOfScalars) Less(i, j int) bool {
	iStr := fmt.Sprintf("%v", ss.s[i])
	jStr := fmt.Sprintf("%v", ss.s[j])
	return sort.StringsAreSorted([]string{iStr, jStr})
}

func (ss SortableSliceOfScalars) Swap(i, j int) {
	tmp := ss.s[i]
	ss.s[i] = ss.s[j]
	ss.s[j] = tmp
}

// Returns the type of the elements of N slice(s). If the type is different,
// another slice or undefined, returns an error.
// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误，嵌套slice，则返回错误
// 否则返回slice内部元素类型
func sliceElementType(slices ...[]interface{}) (reflect.Type, error) {
	var prevType reflect.Type
	for _, s := range slices {
		// Go through elements of all given slices and make sure they are all the same type.
		for _, v := range s {
			// slice内部的元素的类型
			currentType := reflect.TypeOf(v)
			if prevType == nil {
				prevType = currentType
				// We don't support lists of lists yet.
				// 嵌套slice，则返回错误
				if prevType.Kind() == reflect.Slice {
					return nil, mergepatch.ErrNoListOfLists
				}
			} else {
				// slice内部元素类型不一样，则返回错误
				if prevType != currentType {
					return nil, fmt.Errorf("list element types are not identical: %v", fmt.Sprint(slices))
				}
				prevType = currentType
			}
		}
	}

	// slices为空或slice内部元素都为nil
	if prevType == nil {
		return nil, fmt.Errorf("no elements in any of the given slices")
	}

	return prevType, nil
}

// MergingMapsHaveConflicts returns true if the left and right JSON interface
// objects overlap with different values in any key. All keys are required to be
// strings. Since patches of the same Type have congruent keys, this is valid
// for multiple patch types. This method supports strategic merge patch semantics.
// 元素的类型不一样存在冲突
// map类型
//  有一个包含"$patch"，必须两个都包含"$patch"，且值一样
//  fieldPatchStrategy为"replace"不存在冲突
//  存在相同的非"$patch"和"$retainKeys"的key，则检测value是否存在冲突
// []interface{}类型
//   fieldPatchStrategy为"merge"只对map类型的检测冲突，非map不存在冲突
//     检测left和right里有相同mergeKey的value的元素是否存在冲突
//   fieldPatchStrategy不为"merge"
//     left和right的长度不一样，不存在冲突
//     然后进行比较相同的index的元素（非map类型的元素的list进行去重并按照%v字符排序）是否存在冲突
// Primitive类型（string, float64, bool, int64, nil），不相等存在冲突
func MergingMapsHaveConflicts(left, right map[string]interface{}, schema LookupPatchMeta) (bool, error) {
	return mergingMapFieldsHaveConflicts(left, right, schema, "", "")
}

// 如果left类型为map[string]interface{}
//   而right不为map[string]interface{}，说明类型不一样（冲突了），返回true, nil
//   没有都包含"$patch"，则返回true, nil，（说明一个进行删除或替换，另一个进行更新或增加，两个操作不一样）
//   left和right包含"$patch"，且value不一样，说明操作不一样，返回true, nil
//   fieldPatchStrategy为"replace"，则返回false, nil
//   fieldPatchStrategy不为"replace"
//     遍历left所有的key，跳过key为"$patch"和key为"$retainKeys"
//     right不存在相同的key，则跳过这个key
//     right存在相同的key
//       key在left的value类型为[]interface{}或map[string]interface{}，则从schema中获得patchStrategy和mergeKey
//       其他类型的patchStrategy和mergeKey都为空
//       对下一层leftValue和rightValue递归的进行进行mergingMapFieldsHaveConflicts，进行检测冲突
//         如果存在冲突，返回true, err
//         不存在冲突，继续下一个key
//     如果left和right有相同的key，且没有冲突，返回false，nil
//     如果left和right没有相同的key，返回false，nil
// 如果left类型为[]interface{}
//   而right不为[]interface{}，说明类型不一样（冲突了），返回true, nil
//   检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误，嵌套slice，则返回错误
//   如果fieldPatchStrategy为"merge"
//     元素类型不为map，则返回false，nil
//     检测left和right里有相同mergeKey的value的元素是否存在冲突，返回是否存在冲突
//   如果fieldPatchStrategy不为"merge"
//     left和right的长度不一样，返回true, nil
//     元素类型不为map，则对left和right分别进行去重并按照元素的%v的字符排序。元素类型为map的，不用排序（因为不知道按什么排序）
//     遍历left，进行left和right每个相同index的元素的冲突检测（递归执行mergingMapFieldsHaveConflicts）。如果存在冲突，则返回true, err
//     否则返回false，nil
// 如果left类型为Primitive类型（string, float64, bool, int64, nil），比较left和right的值是否相等，不相等返回true，nil。否则返回false，nil
// 其他类型返回错误
func mergingMapFieldsHaveConflicts(
	left, right interface{},
	schema LookupPatchMeta,
	fieldPatchStrategy, fieldPatchMergeKey string,
) (bool, error) {
	switch leftType := left.(type) {
	case map[string]interface{}:
		rightType, ok := right.(map[string]interface{})
		// left类型为map[string]interface{}，而right不为map[string]interface{}，说明类型不一样（冲突了），返回true, nil
		if !ok {
			return true, nil
		}
		leftMarker, okLeft := leftType[directiveMarker]
		rightMarker, okRight := rightType[directiveMarker]
		// if one or the other has a directive marker,
		// then we need to consider that before looking at the individual keys,
		// since a directive operates on the whole map.
		// left包含"$patch"，或right包含"$patch"
		if okLeft || okRight {
			// if one has a directive marker and the other doesn't,
			// then we have a conflict, since one is deleting or replacing the whole map,
			// and the other is doing things to individual keys.
			// 没有都包含"$patch"，则返回true, nil，（说明一个进行删除或替换，另一个进行更新或增加，两个操作不一样）
			if okLeft != okRight {
				return true, nil
			}
			// if they both have markers, but they are not the same directive,
			// then we have a conflict because they're doing different things to the map.
			// 俩个都包含"$patch"，且value不一样，说明操作不一样，返回true, nil
			if leftMarker != rightMarker {
				return true, nil
			}
		}
		// fieldPatchStrategy为"replace"，则返回false, nil
		if fieldPatchStrategy == replaceDirective {
			return false, nil
		}
		// Check the individual keys.
		// 遍历left所有的key，跳过key为"$patch"和key为"$retainKeys"
		// right不存在相同的key，则跳过这个key
		// right存在相同的key
		//   key在left的value类型为[]interface{}或map[string]interface{}，则从schema中获得patchStrategy和mergeKey
		//   其他类型的patchStrategy和mergeKey都为空
		//   对下一层leftValue和rightValue递归的进行进行mergingMapFieldsHaveConflicts，进行检测冲突
		//     如果存在冲突，返回true, err
		//     不存在冲突，继续下一个key
		// 如果left和right有相同的key，且没有冲突，返回false，nil
		// 如果left和right没有相同的key，返回false，nil
		return mapsHaveConflicts(leftType, rightType, schema)

	case []interface{}:
		rightType, ok := right.([]interface{})
		// left为[]interface{}，而right不为[]interface{}，说明类型不一样（冲突了），返回true, nil
		if !ok {
			return true, nil
		}
		// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误，嵌套slice，则返回错误
		// 如果fieldPatchStrategy为"merge"
		//   元素类型不为map，则返回false，nil
		//   检测left和right里有相同mergeKey的value的元素是否存在冲突，返回是否存在冲突
		// 如果fieldPatchStrategy不为"merge"
		//   left和right的长度不一样，返回true, nil
		//   元素类型不为map，则对left和right分别进行去重并按照元素的%v的字符排序。元素类型为map的，不用排序（因为不知道按什么排序）
		//   遍历left，进行left和right每个相同index的元素的冲突检测（递归执行mergingMapFieldsHaveConflicts）。如果存在冲突，则返回true, err
		//   否则返回false，nil
		return slicesHaveConflicts(leftType, rightType, schema, fieldPatchStrategy, fieldPatchMergeKey)
	case string, float64, bool, int64, nil:
		return !reflect.DeepEqual(left, right), nil
	default:
		return true, fmt.Errorf("unknown type: %v", reflect.TypeOf(left))
	}
}

// 遍历left所有的key，跳过key为"$patch"和key为"$retainKeys"
// right不存在相同的key，则跳过这个key
// right存在相同的key
//   key在left的value类型为[]interface{}或map[string]interface{}，则从schema中获得patchStrategy和mergeKey
//   其他类型的patchStrategy和mergeKey都为空
//   对下一层leftValue和rightValue递归的进行进行mergingMapFieldsHaveConflicts，进行检测冲突
//     如果存在冲突，返回true, err
//     不存在冲突，继续下一个key
// 如果left和right有相同的key，且没有冲突，返回false，nil
// 如果left和right没有相同的key，返回false，nil
func mapsHaveConflicts(typedLeft, typedRight map[string]interface{}, schema LookupPatchMeta) (bool, error) {
	for key, leftValue := range typedLeft {
		// key不为"$patch"，且key不为"$retainKeys"
		if key != directiveMarker && key != retainKeysDirective {
			// right存在相同的key
			if rightValue, ok := typedRight[key]; ok {
				var subschema LookupPatchMeta
				var patchMeta PatchMeta
				var patchStrategy string
				var err error
				switch leftValue.(type) {
				// key在left的value类型为[]interface{}
				case []interface{}:
					// 如果schema为PatchMetaFromOpenAPI
					// 返回slice的item类型Schema，组装成第一个返回值PatchMetaFromOpenAPI{Schema: sliceItem.subschema}
					// s.Schema类型是proto.*Kind，则调用sliceItem.VisitKind(s.Schema)
					//     从s.Schema中查找sliceItem.key字段对应Schema
					//     从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成第二个返回值PatchMeta
					// 否则，第二个参数为PatchMeta{}（空的PatchMeta）
					subschema, patchMeta, err = schema.LookupPatchMetadataForSlice(key)
					if err != nil {
						return true, err
					}
					// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
					// 当strategies的数量大于等于3个，则返回错误
					// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
					_, patchStrategy, err = extractRetainKeysPatchStrategy(patchMeta.patchStrategies)
					if err != nil {
						return true, err
					}
				// key在left的value类型为map[string]interface{}
				case map[string]interface{}:
					// 如果schema为PatchMetaFromOpenAPI
					// 从s.schema中查找key字段对应的openapi.Schema，设置为返回的第一个字段(PatchMetaFromOpenAPI{Schema: {key字段对应的openapi.Schema}})
					// 获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，设置返回的第二个字段PatchMeta的patchMergeKey和patchStrategies字段
					subschema, patchMeta, err = schema.LookupPatchMetadataForStruct(key)
					if err != nil {
						return true, err
					}
					// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
					// 当strategies的数量大于等于3个，则返回错误
					// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
					_, patchStrategy, err = extractRetainKeysPatchStrategy(patchMeta.patchStrategies)
					if err != nil {
						return true, err
					}
				}

				// 递归的进行进行mergingMapFieldsHaveConflicts，进行检测冲突
				if hasConflicts, err := mergingMapFieldsHaveConflicts(leftValue, rightValue,
					subschema, patchStrategy, patchMeta.GetPatchMergeKey()); hasConflicts {
					return true, err
				}
			}
		}
	}

	return false, nil
}

// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误，嵌套slice，则返回错误
// 如果fieldPatchStrategy为"merge"
//   元素类型不为map，则返回false，nil
//   （元素为map）检测left和right里有相同mergeKey的value的元素是否存在冲突，返回是否存在冲突
// 如果fieldPatchStrategy不为"merge"
//   left和right的长度不一样，返回true, nil
//   元素类型不为map，则对left和right分别进行去重并按照元素的%v的字符排序。元素类型为map的，不用排序（因为不知道按什么排序）
//   遍历left，进行left和right每个相同index的元素的冲突检测（递归执行mergingMapFieldsHaveConflicts）。如果存在冲突，则返回true, err
//   否则返回false，nil
func slicesHaveConflicts(
	typedLeft, typedRight []interface{},
	schema LookupPatchMeta,
	fieldPatchStrategy, fieldPatchMergeKey string,
) (bool, error) {
	// 检测每个slice的元素是否一样，不一样则返回错误。slices为空或元素都为nil，返回错误，嵌套slice，则返回错误
	// 否则返回slice内部元素类型
	elementType, err := sliceElementType(typedLeft, typedRight)
	if err != nil {
		return true, err
	}

	// fieldPatchStrategy为"merge"
	if fieldPatchStrategy == mergeDirective {
		// Merging lists of scalars have no conflicts by definition
		// So we only need to check further if the elements are maps
		// 元素类型不为map，则返回false，nil
		if elementType.Kind() != reflect.Map {
			return false, nil
		}

		// Build a map for each slice and then compare the two maps
		// 遍历slice，每个元素map[string]interface{}里提取出mergeKey和对应的mergeValue，key为mergeValue转成"%s"，value为这个元素（转成map[string]interface{}）保存到result map[string]interface{}，返回result
		// 返回所有slice里每个元素的mergeKey对应的mergeValue（转成"%s"格式）作为key，元素值（map[string]interface{}）为value组成的map[string]interface{}
		// 如果元素类型不是map[string]interface{}返回错误
		// 如果有一个元素不包含mergeKey，返回错误
		leftMap, err := sliceOfMapsToMapOfMaps(typedLeft, fieldPatchMergeKey)
		if err != nil {
			return true, err
		}

		rightMap, err := sliceOfMapsToMapOfMaps(typedRight, fieldPatchMergeKey)
		if err != nil {
			return true, err
		}

		// 遍历leftMap，如果leftMap和rightMap有相同的key，对key在leftMap和rightMap的value进行递归mergingMapFieldsHaveConflicts检测是否有冲突，存在冲突，返回true, err
		// 如果leftMap和rightMap有相同的key，且没有冲突，返回false，nil
		// 如果leftMap和rightMap没有相同的key，返回false，nil
		// 即检测left和right里有相同mergeKey的value的元素是否存在冲突
		return mapsOfMapsHaveConflicts(leftMap, rightMap, schema)
	}

	// Either we don't have type information, or these are non-merging lists
	// left和right的长度不一样，返回true, nil
	if len(typedLeft) != len(typedRight) {
		return true, nil
	}

	// Sort scalar slices to prevent ordering issues
	// We have no way to sort non-merging lists of maps
	// 元素类型不为map
	if elementType.Kind() != reflect.Map {
		// 对slice进行去重并按照元素的%v的字符排序
		typedLeft = deduplicateAndSortScalars(typedLeft)
		typedRight = deduplicateAndSortScalars(typedRight)
	}

	// Compare the slices element by element in order
	// This test will fail if the slices are not sorted
	// 遍历left，进行left和right每个相同index的元素的冲突检测（递归执行mergingMapFieldsHaveConflicts）。如果存在冲突，则返回true, err
	for i := range typedLeft {
		if hasConflicts, err := mergingMapFieldsHaveConflicts(typedLeft[i], typedRight[i], schema, "", ""); hasConflicts {
			return true, err
		}
	}

	return false, nil
}

// 遍历slice，每个元素map[string]interface{}里提取出mergeKey和对应的mergeValue，key为mergeValue转成"%s"，value为这个元素（转成map[string]interface{}）保存到result map[string]interface{}，返回result
// 返回所有slice里每个元素的mergeKey对应的mergeValue（转成"%s"格式）作为key，元素值（map[string]interface{}）为value组成的map[string]interface{}
// 如果元素类型不是map[string]interface{}返回错误
// 如果有一个元素不包含mergeKey，返回错误
func sliceOfMapsToMapOfMaps(slice []interface{}, mergeKey string) (map[string]interface{}, error) {
	result := make(map[string]interface{}, len(slice))
	for _, value := range slice {
		typedValue, ok := value.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("invalid element type in merging list:%v", slice)
		}

		mergeValue, ok := typedValue[mergeKey]
		if !ok {
			return nil, fmt.Errorf("cannot find merge key `%s` in merging list element:%v", mergeKey, typedValue)
		}

		result[fmt.Sprintf("%s", mergeValue)] = typedValue
	}

	return result, nil
}

// 遍历left，如果left和right有相同的key，对key在left和right的value进行递归mergingMapFieldsHaveConflicts检测是否有冲突，存在冲突，返回true, err
// 如果left和right有相同的key，且没有冲突，返回false，nil
// 如果left和right没有相同的key，返回false，nil
func mapsOfMapsHaveConflicts(typedLeft, typedRight map[string]interface{}, schema LookupPatchMeta) (bool, error) {
	for key, leftValue := range typedLeft {
		if rightValue, ok := typedRight[key]; ok {
			if hasConflicts, err := mergingMapFieldsHaveConflicts(leftValue, rightValue, schema, "", ""); hasConflicts {
				return true, err
			}
		}
	}

	return false, nil
}

// CreateThreeWayMergePatch reconciles a modified configuration with an original configuration,
// while preserving any changes or deletions made to the original configuration in the interim,
// and not overridden by the current configuration. All three documents must be passed to the
// method as json encoded content. It will return a strategic merge patch, or an error if any
// of the documents is invalid, or if there are any preconditions that fail against the modified
// configuration, or, if overwrite is false and there are conflicts between the modified and current
// configurations. Conflicts are defined as keys changed differently from original to modified
// than from original to current. In other words, a conflict occurs if modified changes any key
// in a way that is different from how it is changed in current (e.g., deleting it, changing its
// value). We also propagate values fields that do not exist in original but are explicitly
// defined in modified.
// 对于kubectl apply（client apply）
// origin是以前的文件内容、modified是现在文件内容（加上现在文件内容的annotation["kubectl.kubernetes.io/last-applied-configuration"]）、current为apiserver中的内容
// 将original，modified，current进行json.Unmarshal为map[string]interface{}
// 对currentMap和modifiedMap进行diff，计算出所有增加和更新的Patch
// 对originalMap和modifiedMap进行diff，计算出所有删除的patch
// 然后将两个patch进行合并，生成最终的patch
// 执行fns，验证patch是否符合条件
// 如果overwrite为false，则检验是否存在冲突，现在存在的差异和最终patch存在冲突（两个对同一个key同时进行修改）。
//   对originalMap和currentMap进行diff，计算出现在存在的差异
//   对最终的patch和现在存在的差异进行比较，是否存在冲突
//   存在冲突返回错误
//   比如original "{k: a}", current为"{k: b}"， modified为"{k: c}"
//   其中"{k: b}"是非kubectl apply修改的，而现在基于original修改成"{k: c}"，会存在冲突
// 返回最终的patch（json.marshal转成[]byte）
func CreateThreeWayMergePatch(original, modified, current []byte, schema LookupPatchMeta, overwrite bool, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	originalMap := map[string]interface{}{}
	if len(original) > 0 {
		if err := json.Unmarshal(original, &originalMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	modifiedMap := map[string]interface{}{}
	if len(modified) > 0 {
		if err := json.Unmarshal(modified, &modifiedMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	currentMap := map[string]interface{}{}
	if len(current) > 0 {
		if err := json.Unmarshal(current, &currentMap); err != nil {
			return nil, mergepatch.ErrBadJSONDoc
		}
	}

	// The patch is the difference from current to modified without deletions, plus deletions
	// from original to modified. To find it, we compute deletions, which are the deletions from
	// original to modified, and delta, which is the difference from current to modified without
	// deletions, and then apply delta to deletions as a patch, which should be strictly additive.
	deltaMapDiffOptions := DiffOptions{
		IgnoreDeletions: true,
		SetElementOrder: true,
	}
	// 先进行遍历modified
	//   modified中的key不在currentMap中（说明是增加字段），则将key和对应的modifiedValue保存到patch
	//   modified中的key在currentMap中
	//     当key是"$patch"，且currentMapValue和modifiedValue都是string，（如果不都是string，则返回错误）
	//       如果currentMapValue和modifiedValue不相等，则添加key为"$patch" value为modifiedValue到patch，继续下一个key
	//     当currentMapValue和modifiedValue不是同一类型，即类型发生了变化，将key和对应modifiedValue到patch中，继续下一个key
	//     当currentMapValue和modifiedValue是同一类型
	//       currentMapValue和modifiedValue是map[string]interface{}类型
	//         如果"x-kubernetes-patch-strategy"里包含"replace"，则添加key和对应的modifiedValue到patch中
	//         否则，继续对（key的值）的map[string]interface{}进行diff（就是递归调用diffMaps），如果currentMapValue和modifiedValue不相等（有差异），则添加key和对应patchValue到patch中
	//       currentMapValue和modifiedValue是[]interface{}类型
	//         如果"x-kubernetes-patch-strategy"里包含"merge"，则根据"x-kubernetes-patch-merge-key"进行merge
	//           currentMap为空（说明只有增加字段），则patchList为modified，deleteList为nil, setOrderList为nil, nil
	//           slice元素类型为map
	//             根据map的key为mergeKey的值，进行map排序
	//             根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item，获得增加或更新的列表，移除的列表（为空）
	//             元素为map[string]interface{}
	//             SetElementOrder为true，且（
	//              （存在差异（patchList大于0）或顺序不一样（不存在差异））
	//             ）
	//             setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值（每个对应一个map，且map里长度为1），按照modified里顺序
	//             获得patchList（更新增加列表（按照modified中mergeKey对应的相同值的顺序）），nil的删除的列表，setOrderList
	//           slice元素类型为slice，返回错误
	//           slice元素为其他类型（元素类型是Primitive）
	//             将两个list按照元素的%v的字符排序
	//             比较两个list，返回两个list，第一个是patchList（增加的元素），第二个是deleteList（删除的元素，为空）
	//             按照modified中的顺序，来排序patchList
	//             SetElementOrder为true且currentMap和modified不相同，则setOrderList为modified
	//             获得patchList（按照modified中的顺序），deleteList（删除的元素），setOrderList
	//          将key为key，value为patchList添加到patch（patch[key] = patchList）
	//          将key为"$setElementOrder/{key}"，value为setOrderList
	//        x-kubernetes-patch-strategy"里不包含"merge"，则使用替换策略，即modifiedValue替换currentMapValue
	//            currentMap和modified不相等，添加key和value为modified到patch
	//       其他类型Primitive
	//         currentMap和modified不相等，添加key和value为modified到patch
	// 返回patch
	deltaMap, err := diffMaps(currentMap, modifiedMap, schema, deltaMapDiffOptions)
	if err != nil {
		return nil, err
	}
	deletionsMapDiffOptions := DiffOptions{
		SetElementOrder:           true,
		IgnoreChangesAndAdditions: true,
	}
	// 先进行遍历modified
	//   modified中的key在original中
	//     当key是"$patch"，且originalValue和modifiedValue都是string，（如果不都是string，则返回错误）
	//       如果originalValue和modifiedValue不相等，则添加key为"$patch" value为modifiedValue到patch，继续下一个key
	//     当originalValue和modifiedValue不是同一类型，即类型发生了变化，继续下一个key
	//     当originalValue和modifiedValue是同一类型
	//       originalValue和modifiedValue是map[string]interface{}类型
	//         如果"x-kubernetes-patch-strategy"里不包含"replace"，
	//         继续对（key的值）的map[string]interface{}进行diff（就是递归调用diffMaps），如果originalValue和modifiedValue不相等（存在删除字段），则添加key和对应patchValue到patch中
	//       originalValue和modifiedValue是[]interface{}类型
	//         如果"x-kubernetes-patch-strategy"里包含"merge"，则根据"x-kubernetes-patch-merge-key"进行merge
	//           original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则patchList、deleteList、setOrderList都为nil
	//           slice元素类型为map
	//             根据map的key为mergeKey的值，进行map排序
	//             根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item，获得patchList（增加或更新的列表为空）移除的列表
	//             元素为map[string]interface{}
	//             SetElementOrder为true，且（不忽略删除（一定忽略更新增加字段）且存在差异（patchList大于0）
	//             setOrderList（其实是[]map[string]interface{}）保存所有modified里每个元素的mergeKey和对应的值（每个对应一个map，且map里长度为1），按照modified里顺序
	//             获得patchList（删除的元素），nil的删除的列表，setOrderList
	//           slice元素类型为slice，返回错误
	//           slice元素为其他类型（元素类型是Primitive）
	//             将两个list按照元素的%v的字符排序
	//             比较两个list，返回两个list，第一个是patchList（增加的元素，这里为空），第二个是deleteList（删除的元素）
	//             按照modified中的顺序，来排序patchList（这里为空）
	//             SetElementOrder为true且不忽略删除字段，且有删除的元素（len(deleteList)大于0），则setOrderList为modified
	//             获得patchList（按照modified中的顺序，这里为空），deleteList（删除的元素），setOrderList
	//          将key为key，value为patchList添加到patch（patch[key] = patchList）（只有元素类型为map[string]interface且存在删除）
	//          将key为"$deleteFromPrimitiveList/{key}"，value为deleteList添加到patch（只有元素类型是Primitive）
	//          将key为"$setElementOrder/{key}"，value为setOrderList
	//        x-kubernetes-patch-strategy"里不包含"merge"，则这里没有任何patch（删除元素）
	//       其他类型Primitive
	//         这里没有任何patch（删除元素）
	// diffOptions不忽略删除，则遍历original里的所有key，如果key不在modified，则设置patch里的key为key，value为nil（说明是删除字段）
	// 返回patch
	deletionsMap, err := diffMaps(originalMap, modifiedMap, schema, deletionsMapDiffOptions)
	if err != nil {
		return nil, err
	}

	mergeOptions := MergeOptions{}
	// patch中存在"$patch"
	//   "$patch"的value为"replace"，将"$patch"从patch删除，直接返回patch
	//   "$patch"的value为"delete"，返回空的map[string]interface{}
	//   其他情况，返回错误
	// patch中有"$retainKeys"（这里的情况是不会有的）
	//   从patch中移除"$retainKeys"
	//   不启用MergeParallelList
	//     original里有"$retainKeys"，且"$retainKeys"的值在original和patch中不一样，则返回错误
	//     original里没有"$retainKeys"，则添加key为"$retainKeys"和value为patch中$retainKeys"的值到original
	// 处理patch中key格式为"$setElementOrder/{key}"
	//   未启用MergeParallelList
	//     original里也存在相同"$setElementOrder"前缀的key，且在original里的值和在patch里的值不一样，则返回错误
	//     original里不存在相同"$setElementOrder"前缀的key，则添加key为key，value为key在patch里的值
	//   从patch中删除这个key
	//   patch中"$setElementOrder/{key}"的value的值类型不为slice，则返回错误
	//   originalKey为从key格式"$setElementOrder/{key}"中提取出"/"后半部分的值
	//   在original里存在这个原始key，如果值的类型不是slice，则返回错误
	//   对original和patch原始key的元素进行merge（在original里存在这个原始key和在patch中存在这个原始的key，至少有一个为true。都不为true则跳过patch中的这个key）
	//   在original里存在这个原始key，且在patch中不存在这个原始的key，则merged为originalFieldValue（original里这个原始key的值）
	//   在original里不存在这个原始key，且在patch中存在这个原始key，则merged为patchFieldValue（patch里这个原始key的值）
	//   在original里存在这个原始key，且在patch中存在这个原始key，则对originalFieldValue和patchFieldValue这两个值进行merge
	//   mergeKey为空，将merged进行分类，patchItems（在typedSetElementOrderList部分）和serverOnlyItems（不在typedSetElementOrderList部分）
	//   mergeKey不为空，将merged进行分类，patchItems（跟typedSetElementOrderList有相同mergeKey和对应mergeKeyValue）部分，serverOnlyItems（没有相同的mergeKey和对应mergeKeyValue部分）
	//     merged和typedSetElementOrderList元素类型不是map[string]interface{}，返回错误
	//   先将patchItems按照typedSetElementOrderList（patch中"$setElementOrder/{key}"的value的值）进行排序，serverOnlyItems按照originalFieldValue（original里原始key的值）进行排序
	//   然后将两个排序后的list进行合并
	//     插入的顺序是（元素类型是map[string]interface{}）按照在originalFieldValue中mergeKey对应的一样值的位置，或（元素类型是其他类型）在originalFieldValue中位置
	//     如果都在originalFieldValue里，则谁在前面，进行先插入
	//     否则，patchItems元素先插入。
	//   original的原始key的值设为上面合并的结果
	//   从patch中移除原始key
	// 处理patch里剩余部分（包含前缀"$deleteFromPrimitiveList"和其他）
	// 遍历所有patch里key
	//   如果key包含前缀"$deleteFromPrimitiveList"，且mergeOptions.MergeParallelList为false，则直接设置original的key的值为patchVal。（当前场景不会出现，因为patch不会有key包含前缀"$deleteFromPrimitiveList"）
	//   （原来key格式"$deleteFromPrimitiveList/{originalKey}"解析出originalKey，则为originalKey。否则为原来的key
	//   处理key对应的patchV是nil
	//     则从original中删除这个key（原来key格式"$deleteFromPrimitiveList/{originalKey}"解析出originalKey，则为originalKey。否则为原来的key）
	//   original里本来就没有这个key，且key不包含前缀"$deleteFromPrimitiveList"，则设置original的key值为patchV（说明增加字段），这个key处理完
	//   original里本来就没有这个key，且key包含前缀"$deleteFromPrimitiveList"，这个key处理完（说明原来就要删除这个key，但是original里就没有这个key）
	//   原来original里有这个key，key对应的patchV是nil且mergeOptions.IgnoreUnmatchedNulls为false，original的key被删除
	//     key包含前缀"$deleteFromPrimitiveList"，这个key处理完
	//     key不包含前缀"$deleteFromPrimitiveList"，则设置original的key值为patchV（说明明确设置original[key]为nil），这个key处理完
	//   处理patchV不为nil
	//     origin里原本有这个key，则需要对original[k]和patchV进行合并
	//     key不包含前缀"$deleteFromPrimitiveList"（mergeOptions.MergeParallelList可以为true或false），正常的合并
	//     key包含前缀"$deleteFromPrimitiveList"（mergeOptions.MergeParallelList一定为true），即original中有部分元素需要被移除
	//     合并具体规则：
	//       类型不一样
	//         key不包含前缀"$deleteFromPrimitiveList"，则original的key值为patchV（说明类型改变了），这个key处理完
	//         key包含前缀"$deleteFromPrimitiveList"，这个key处理完
	//       都为map类型
	//         将original和patch转成map[string]interface{}
	//         如果任意一个不是map[string]interface{}，返回错误
	//         patchStrategy不为"replace"，则进行map递归（mergeMap）的merge，返回merge后的map[string]interface{}
	//         patchStrategy为"replace"，则返回typedPatch（直接替换）
	//       都为slice类型
	//          fieldPatchStrategy（patch策略）是"merge"
	//            original和patch都为空，则不做任何操作
	//            元素类型不是map
	//              不启用MergeParallelList，或启用MergeParallelList且isDeleteList为false（key不包含前缀"$deleteFromPrimitiveList"）
	//                对original和patch进行append合并，然后进行去重，赋值为merged
	//                将merged进行分类，在patch部分和不在patch部分
	//                先将patchItems（在patch部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
	//                然后将两个排序后的list进行合并
	//                合并规则：
	//                   如果都在original里，按照在original中位置，谁在前面，进行插入。
	//                   如果不都在original里，patchItems（在patch部分）进行插入
	//                返回合并后list
	//            元素是map
	//              mergeKey为空，返回错误
	//              进行"$patch"处理（删除或替换）：
	//                存在"$patch"，"$patch"的值为"delete"，则original为删除了map元素（元素存在mergeKey和对应的mergeValue（patch中mergeKey值））和patch移除包含"$patch"的map之后的值
	//                处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
	//                    original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
	//                    original里不存在mergeKey和对应的mergeValue，则将patch的元素map append到original（相当于增加新map）
	//                  对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
	//                  先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
	//                  然后将两个排序后的list进行合并
	//                  合并规则：
	//                     如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
	//                     如果不都在original里，patchItems（在patch部分）进行插入
	//                  返回合并后list
	//              "$patch"的值是"replace"，则original为patch移除包含"$patch"的map之后的值，patch为nil
	//                  对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
	//                  先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
	//                  然后将两个排序后的list进行合并
	//                  合并规则：
	//                     如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
	//                     如果不都在original里，patchItems（在patch部分）进行插入
	//                  返回合并后list
	//              所有patch元素都不存在"$patch"，original为original, patch为（遍历patch重新append生成的）
	//                处理patch里元素map没有"$patch"，但是存在mergeKey。（map聚合或增加map）
	//                  original元素里存在mergeKey和对应的mergeValue，则递归进行这个origin和patch的元素map聚合，聚合值替换original这个index元素的值
	//                  original里不存在mergeKey和对应的mergeValue，则patch的元素map append到original（相当于增加新map）
	//                对original进行分类，跟patch有相同mergeKey和对应mergeKeyValue，没有相同的mergeKey和对应mergeKeyValue部分
	//                先将patchItems（在patch有相同mergeKey和对应mergeKeyValue部分）按照patch进行排序，serverOnlyItems（不在patch部分）按照original进行排序
	//                然后将两个排序后的list进行合并
	//                合并规则：
	//                   如果都在original里，按照在original中mergeKey对应的一样值的位置，谁在前面，进行插入
	//                   如果不都在original里，patchItems（在patch部分）进行插入
	//                返回合并后list
	//             存在"$patch"，"$patch"的值不是"replace"或"delete"，返回错误
	//             patch里的元素存在key "$patch"但是这个元素不存在key mergeKey，则返回错误
	//        fieldPatchStrategy（patch策略）不是"merge"，返回patch（转成[]interface{}）
	//       其他类型，则original的key值为patchV（直接替换）
	// 返回最后的original
	patchMap, err := mergeMap(deletionsMap, deltaMap, schema, mergeOptions)
	if err != nil {
		return nil, err
	}

	// Apply the preconditions to the patch, and return an error if any of them fail.
	for _, fn := range fns {
		if !fn(patchMap) {
			return nil, mergepatch.NewErrPreconditionFailed(patchMap)
		}
	}

	// If overwrite is false, and the patch contains any keys that were changed differently,
	// then return a conflict error.
	// 检测key被其他地方改了
	// 比如original "{k: a}", current为"{k: b}"， modified为"{k: c}"
	// 其中"{k: b}"是非kubectl apply修改的，而现在基于original修改成"{k: c}"，会存在冲突
	if !overwrite {
		changeMapDiffOptions := DiffOptions{}
		// originalMap和currentMap进行对比，获得以前的改变（即包含增加和更新和删除，没有"$setElementOrder/{key}"）
		// 先进行遍历modified
		//   modified中的key不在original中（说明是增加字段），如果IgnoreChangesAndAdditions为false（不忽略更新和增加字段），则将key和对应的modifiedValue保存到patch
		//   modified中的key在original中
		//     当key是"$patch"，且originalValue和modifiedValue都是string，（如果不都是string，则返回错误）
		//       如果originalValue和modifiedValue不相等，则添加key为"$patch" value为modifiedValue到patch，继续下一个key
		//     当originalValue和modifiedValue不是同一类型，即类型发生了变化
		//       不忽略更新和增加，则将key和对应modifiedValue到patch中，继续下一个key
		//     当originalValue和modifiedValue是同一类型
		//       originalValue和modifiedValue是map[string]interface{}类型
		//         如果"x-kubernetes-patch-strategy"里包含"replace"，且如果不忽略改变和增加字段，则添加key和对应的modifiedValue到patch中
		//         否则，继续对（key的值）的map[string]interface{}进行diff（就是递归调用diffMaps），如果originalValue和modifiedValue不相等（有差异），则添加key和对应patchValue到patch中
		//       originalValue和modifiedValue是[]interface{}类型
		//         如果"x-kubernetes-patch-strategy"里包含"merge"，则根据"x-kubernetes-patch-merge-key"进行merge
		//           original和modified都为空，或original为空（说明只有增加字段）且忽略更新和增加字段，则没有任何patch，不需要继续执行
		//           original为空（说明只有增加字段）且不忽略更新和增加字段，则patchList为modified，nil的删除的列表，setOrderList为nil，不需要继续执行
		//           slice元素类型为map
		//             根据map的key为mergeKey的值，进行map排序
		//             根据mergeKey进行diff，mergeKey对应的值相同的两个元素认为是同一个item，获得增加或更新的列表，移除的列表
		//             元素为map[string]interface{}
		//             获得patchList（更新增加和删除列表（按照modified中mergeKey对应的相同值的顺序），删除的元素在最后面），nil的删除的列表，setOrderList（这里为空，因为SetElementOrder为false）
		//           slice元素类型为slice，返回错误
		//           slice元素为其他类型（元素类型是Primitive）
		//             将两个list按照元素的%v的字符排序
		//             比较两个list，返回两个list，第一个是patchList（增加的元素），第二个是deleteList（删除的元素）
		//             按照modified中的顺序，来排序patchList
		//             获得patchList（按照modified中的顺序），deleteList（删除的元素），setOrderList（这里为空，因为SetElementOrder为false）
		//          将key为key，value为patchList添加到patch（patch[key] = patchList）
		//          当元素类型是Primitive，将key为"$deleteFromPrimitiveList/{key}"，value为deleteList添加到patch
		//        x-kubernetes-patch-strategy"里不包含"merge"，则使用替换策略，即modifiedValue替换originalValue
		//            不忽略更新和增加字段，且original和modified不相等，添加key和value为modified到patch
		//       其他类型Primitive
		//         不忽略更新和增加字段，且original和modified不相等，添加key和value为modified到patch
		// diffOptions不忽略删除，则遍历original里的所有key，如果key不在modified，则设置patch里的key为key，value为nil（说明是删除字段）
		// 返回patch
		changedMap, err := diffMaps(originalMap, currentMap, schema, changeMapDiffOptions)
		klog.Infof("diffMaps: originalMap: %#v\ncurrentMap: %#v\nchangedMap %#v",  originalMap, currentMap, changedMap)
		if err != nil {
			return nil, err
		}

		// 元素的类型不一样存在冲突
		// map类型
		//  有一个包含"$patch"，必须两个都包含"$patch"，且值一样
		//  fieldPatchStrategy为"replace"不存在冲突
		//  存在相同的非"$patch"和"$retainKeys"的key，则检测value是否存在冲突
		// []interface{}类型
		//   fieldPatchStrategy为"merge"只对map类型的检测冲突，非map不存在冲突
		//     检测left和right里有相同mergeKey的value的元素是否存在冲突
		//   fieldPatchStrategy不为"merge"
		//     left和right的长度不一样，不存在冲突
		//     然后进行比较相同的index的元素（非map类型的元素的list进行去重并按照%v字符排序）是否存在冲突
		// Primitive类型（string, float64, bool, int64, nil），不相等存在冲突
		hasConflicts, err := MergingMapsHaveConflicts(patchMap, changedMap, schema)
		klog.InfoS("MergingMapsHaveConflicts result", "patchMap", patchMap, "changedMap", changedMap, "hasConflicts", hasConflicts)
		if err != nil {
			return nil, err
		}

		if hasConflicts {
			return nil, mergepatch.NewErrConflict(mergepatch.ToYAMLOrError(patchMap), mergepatch.ToYAMLOrError(changedMap))
		}
	}

	return json.Marshal(patchMap)
}

func ItemAddedToModifiedSlice(original, modified string) bool { return original > modified }

func ItemRemovedFromModifiedSlice(original, modified string) bool { return original < modified }

func ItemMatchesOriginalAndModifiedSlice(original, modified string) bool { return original == modified }

// 生成map[string]interface{}{mergeKey: mergeKeyValue, "$patch": "delete"}
func CreateDeleteDirective(mergeKey string, mergeKeyValue interface{}) map[string]interface{} {
	return map[string]interface{}{mergeKey: mergeKeyValue, directiveMarker: deleteDirective}
}

// 将original和patch转成map[string]interface{}
// 如果任意一个不是map[string]interface{}，返回错误
func mapTypeAssertion(original, patch interface{}) (map[string]interface{}, map[string]interface{}, error) {
	typedOriginal, ok := original.(map[string]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedOriginal, original)
	}
	typedPatch, ok := patch.(map[string]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedPatch, patch)
	}
	return typedOriginal, typedPatch, nil
}

// original和patch都转成[]interface{}
// 如果original和patch任意一个不是[]interface{}，返回错误
func sliceTypeAssertion(original, patch interface{}) ([]interface{}, []interface{}, error) {
	typedOriginal, ok := original.([]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedOriginal, original)
	}
	typedPatch, ok := patch.([]interface{})
	if !ok {
		return nil, nil, mergepatch.ErrBadArgType(typedPatch, patch)
	}
	return typedOriginal, typedPatch, nil
}

// extractRetainKeysPatchStrategy process patch strategy, which is a string may contains multiple
// patch strategies separated by ",". It returns a boolean var indicating if it has
// retainKeys strategies and a string for the other strategy.
// 返回strategies的值是否包含"retainKeys"，其他非"retainKeys"值
// 当strategies的数量大于等于3个，则返回错误
// 当strategies的数量大于等于2个，没有包含"retainKeys"，则返回错误
func extractRetainKeysPatchStrategy(strategies []string) (bool, string, error) {
	switch len(strategies) {
	case 0:
		return false, "", nil
	case 1:
		singleStrategy := strategies[0]
		switch singleStrategy {
		// "retainKeys"
		case retainKeysStrategy:
			return true, "", nil
		default:
			return false, singleStrategy, nil
		}
	case 2:
		switch {
		// 第一个值为"retainKeys"，则返回true，第二个值，nil
		case strategies[0] == retainKeysStrategy:
			return true, strategies[1], nil
		// 第二个值为"retainKeys"，则返回true，第一个值，nil
		case strategies[1] == retainKeysStrategy:
			return true, strategies[0], nil
		default:
			// 没有包含"retainKeys"，则返回false, "", 错误
			return false, "", fmt.Errorf("unexpected patch strategy: %v", strategies)
		}
	default:
		return false, "", fmt.Errorf("unexpected patch strategy: %v", strategies)
	}
}

// hasAdditionalNewField returns if original map has additional key with non-nil value than modified.
// original里有key的值不为nil，且不在modified里，则返回true
func hasAdditionalNewField(original, modified map[string]interface{}) bool {
	for k, v := range original {
		if v == nil {
			continue
		}
		if _, found := modified[k]; !found {
			return true
		}
	}
	return false
}
