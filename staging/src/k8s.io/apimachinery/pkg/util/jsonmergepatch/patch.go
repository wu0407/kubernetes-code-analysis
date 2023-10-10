/*
Copyright 2017 The Kubernetes Authors.

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

package jsonmergepatch

import (
	"fmt"
	"reflect"

	"github.com/evanphx/json-patch"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/mergepatch"
)

// Create a 3-way merge patch based-on JSON merge patch.
// Calculate addition-and-change patch between current and modified.
// Calculate deletion patch between original and modified.
// origin变为modified的patch，只考虑字段值为"null"，即移除字段
// current变为modified的patch，只考虑字段增加和更新
// 对于kubectl apply（client apply）
// origin是以前的文件内容、modified是现在文件内容（加上现在文件内容的annotation["kubectl.kubernetes.io/last-applied-configuration"]）、current为apiserver中的内容
// 移除的字段（值为null），只看以前文件和当文件内容的patch
// 增加和更新字段，只看apiserver和当前的文件内容的patch
// 同时会判断是否满足preconditions，不满足返回错误
func CreateThreeWayJSONMergePatch(original, modified, current []byte, fns ...mergepatch.PreconditionFunc) ([]byte, error) {
	if len(original) == 0 {
		original = []byte(`{}`)
	}
	if len(modified) == 0 {
		modified = []byte(`{}`)
	}
	if len(current) == 0 {
		current = []byte(`{}`)
	}

	// 如果为kubectl场景，apiserver里的对象与现在文件内容（加上现在文件内容的annotation["kubectl.kubernetes.io/last-applied-configuration"]）进行比较
	// annotation["kubectl.kubernetes.io/last-applied-configuration"]的值替换为modified里的值（现在文件的内容）
	// 生成将current变为modified的json merge patch
	addAndChangePatch, err := jsonpatch.CreateMergePatch(current, modified)
	if err != nil {
		return nil, err
	}
	// Only keep addition and changes
	// 删除null值、空值保留，其他值保留。
	// 返回过滤后的json byte数组和过滤后的map对象和错误
	// apiserver中的对象跟现在文件的对比后，更新和新增字段。
	addAndChangePatch, addAndChangePatchObj, err := keepOrDeleteNullInJsonPatch(addAndChangePatch, false)
	if err != nil {
		return nil, err
	}

	// 以前的文件内容与现在的文件内容（加上现在文件内容的annotation["kubectl.kubernetes.io/last-applied-configuration"]）进行比较
	// 生成将original变为modified的json merge patch
	deletePatch, err := jsonpatch.CreateMergePatch(original, modified)
	if err != nil {
		return nil, err
	}
	// Only keep deletion
	// 保留null值、空值和其他值删除
	// 以前文件跟现在文件比较后，现在文件中消失的字段（即字段值为null）
	deletePatch, deletePatchObj, err := keepOrDeleteNullInJsonPatch(deletePatch, true)
	if err != nil {
		return nil, err
	}

	// 返回false的条件（或的关系）
	// 1. addAndChangePatchObj和deletePatchObj是相同类型，且如果是map[string]interface{}类型，则存在相同key的value必须不存在冲突（即满足下面的所有个条件）。
	// 2. addAndChangePatchObj和deletePatchObj是相同类型，且如果是[]interface{}，则必须两边的长度大小不一样，且两边相同index的每一项都不存在冲突
	// 3. addAndChangePatchObj和deletePatchObj是相同类型，且如果是其他简单类型（string, float64, bool, int64, nil），则值必须相等
	// 同一字段同时进行更新和移除，或同时增加和移除，就会出现冲突，比如'{"a": null}'和'{"a": "b"}'就存在冲突
	hasConflicts, err := mergepatch.HasConflicts(addAndChangePatchObj, deletePatchObj)
	if err != nil {
		return nil, err
	}
	if hasConflicts {
		return nil, mergepatch.NewErrConflict(mergepatch.ToYAMLOrError(addAndChangePatchObj), mergepatch.ToYAMLOrError(deletePatchObj))
	}
	// 将addAndChangePatch应用到deletePatchObj中，即将deletePatch和addAndChangePatch进行合成
	patch, err := jsonpatch.MergePatch(deletePatch, addAndChangePatch)
	if err != nil {
		return nil, err
	}

	var patchMap map[string]interface{}
	err = json.Unmarshal(patch, &patchMap)
	if err != nil {
		return nil, fmt.Errorf("Failed to unmarshal patch for precondition check: %s", patch)
	}
	// 是否满足条件
	meetPreconditions, err := meetPreconditions(patchMap, fns...)
	if err != nil {
		return nil, err
	}
	// 不满足条件，返回错误
	if !meetPreconditions {
		return nil, mergepatch.NewErrPreconditionFailed(patchMap)
	}

	return patch, nil
}

// keepOrDeleteNullInJsonPatch takes a json-encoded byte array and a boolean.
// It returns a filtered object and its corresponding json-encoded byte array.
// It is a wrapper of func keepOrDeleteNullInObj
// keepNull为true，则保留null值、空值和其他值删除。keepNull为false，则删除null值、空值保留，其他值保留。
// 返回过滤后的json byte数组和过滤后的map对象和错误
func keepOrDeleteNullInJsonPatch(patch []byte, keepNull bool) ([]byte, map[string]interface{}, error) {
	var patchMap map[string]interface{}
	err := json.Unmarshal(patch, &patchMap)
	if err != nil {
		return nil, nil, err
	}
	// keepNull为true，则保留null值、空值和其他值删除。keepNull为false，则删除null值、空值保留，其他值保留。
	filteredMap, err := keepOrDeleteNullInObj(patchMap, keepNull)
	if err != nil {
		return nil, nil, err
	}
	o, err := json.Marshal(filteredMap)
	return o, filteredMap, err
}

// keepOrDeleteNullInObj will keep only the null value and delete all the others,
// if keepNull is true. Otherwise, it will delete all the null value and keep the others.
// keepNull为true，则保留null值、空值和其他值删除。keepNull为false，则删除null值、空值保留，其他值保留。
func keepOrDeleteNullInObj(m map[string]interface{}, keepNull bool) (map[string]interface{}, error) {
	filteredMap := make(map[string]interface{})
	var err error
	for key, val := range m {
		switch {
		// key值是null的情况，且keepNull为true，则保留（保存到filteredMap中），否则删除
		case keepNull && val == nil:
			filteredMap[key] = nil
		// key值不为null的情况
		case val != nil:
			switch typedVal := val.(type) {
			// key值是map对象的情况
			case map[string]interface{}:
				// Explicitly-set empty maps are treated as values instead of empty patches
				// map为空对象的情况，如果keepNull为false，则保存key值到filteredMap中
				if len(typedVal) == 0 {
					if !keepNull {
						filteredMap[key] = typedVal
					}
					continue
				}

				// map对象不为空的情况，递归调用keepOrDeleteNullInObj，将返回的map对象保存到filteredMap中
				var filteredSubMap map[string]interface{}
				filteredSubMap, err = keepOrDeleteNullInObj(typedVal, keepNull)
				if err != nil {
					return nil, err
				}

				// If the returned filtered submap was empty, this is an empty patch for the entire subdict, so the key
				// should not be set
				// 如果不是嵌套为空的对象，则保存key值到filteredMap中。嵌套为空的对象，不保存key值到filteredMap中
				if len(filteredSubMap) != 0 {
					filteredMap[key] = filteredSubMap
				}

			// key值是list、string、float64, bool, int64、nil对象的情况
			case []interface{}, string, float64, bool, int64, nil:
				// Lists are always replaced in Json, no need to check each entry in the list.
				// 如果keepNull为false，则保存key值到filteredMap中
				if !keepNull {
					filteredMap[key] = val
				}
			default:
				return nil, fmt.Errorf("unknown type: %v", reflect.TypeOf(typedVal))
			}
		}
	}
	return filteredMap, nil
}

func meetPreconditions(patchObj map[string]interface{}, fns ...mergepatch.PreconditionFunc) (bool, error) {
	// Apply the preconditions to the patch, and return an error if any of them fail.
	for _, fn := range fns {
		if !fn(patchObj) {
			return false, fmt.Errorf("precondition failed for: %v", patchObj)
		}
	}
	return true, nil
}
