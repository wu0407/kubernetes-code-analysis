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

package mergepatch

import (
	"fmt"
	"reflect"

	"github.com/davecgh/go-spew/spew"
	"sigs.k8s.io/yaml"
)

// PreconditionFunc asserts that an incompatible change is not present within a patch.
type PreconditionFunc func(interface{}) bool

// RequireKeyUnchanged returns a precondition function that fails if the provided key
// is present in the patch (indicating that its value has changed).
// 返回函数，key在patch中，key的值被改变；key不在patch中，key的值没有被改变
func RequireKeyUnchanged(key string) PreconditionFunc {
	return func(patch interface{}) bool {
		patchMap, ok := patch.(map[string]interface{})
		if !ok {
			return true
		}

		// The presence of key means that its value has been changed, so the test fails.
		_, ok = patchMap[key]
		return !ok
	}
}

// RequireMetadataKeyUnchanged creates a precondition function that fails
// if the metadata.key is present in the patch (indicating its value
// has changed).
func RequireMetadataKeyUnchanged(key string) PreconditionFunc {
	return func(patch interface{}) bool {
		patchMap, ok := patch.(map[string]interface{})
		if !ok {
			return true
		}
		patchMap1, ok := patchMap["metadata"]
		if !ok {
			return true
		}
		patchMap2, ok := patchMap1.(map[string]interface{})
		if !ok {
			return true
		}
		_, ok = patchMap2[key]
		return !ok
	}
}

func ToYAMLOrError(v interface{}) string {
	y, err := toYAML(v)
	if err != nil {
		return err.Error()
	}

	return y
}

func toYAML(v interface{}) (string, error) {
	y, err := yaml.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("yaml marshal failed:%v\n%v\n", err, spew.Sdump(v))
	}

	return string(y), nil
}

// HasConflicts returns true if the left and right JSON interface objects overlap with
// different values in any key. All keys are required to be strings. Since patches of the
// same Type have congruent keys, this is valid for multiple patch types. This method
// supports JSON merge patch semantics.
//
// NOTE: Numbers with different types (e.g. int(0) vs int64(0)) will be detected as conflicts.
//
//	Make sure the unmarshaling of left and right are consistent (e.g. use the same library).
// 返回false的条件（或的关系）
// 1. left和right是相同类型，且如果是map[string]interface{}类型，则存在相同key的value必须不存在冲突（即满足这里的所有个条件）。
// 2. left和right是相同类型，且如果是[]interface{}，则必须两边的长度大小不一样，且两边相同index的每一项都不存在冲突
// 3. left和right是相同类型，且如果是其他简单类型（string, float64, bool, int64, nil），则值必须相等
// 比如'{"a": null}'和'{"a": "b"}'就存在冲突
func HasConflicts(left, right interface{}) (bool, error) {
	switch typedLeft := left.(type) {
	case map[string]interface{}:
		switch typedRight := right.(type) {
		// left和right都是map[string]interface{}类型
		case map[string]interface{}:
			// 遍历left，检测left与right相同的key，是否有冲突
			// 返回是否存在冲突和错误
			for key, leftValue := range typedLeft {
				rightValue, ok := typedRight[key]
				// 有key不一样，继续
				if !ok {
					continue
				}
				if conflict, err := HasConflicts(leftValue, rightValue); err != nil || conflict {
					return conflict, err
				}
			}

			return false, nil
		// right是其他类型，说明right与left的类型不一样（存在冲突），返回true, nil
		default:
			return true, nil
		}
	case []interface{}:
		switch typedRight := right.(type) {
		// left和right都是[]interface{}类型
		case []interface{}:
			// 两边的长度大小不一样，说明存在冲突，返回true, nil
			if len(typedLeft) != len(typedRight) {
				return true, nil
			}

			// 两边长度一样
			// 遍历left，检测两边的每一项是否存在冲突
			// 返回是否存在冲突和错误
			for i := range typedLeft {
				if conflict, err := HasConflicts(typedLeft[i], typedRight[i]); err != nil || conflict {
					return conflict, err
				}
			}

			return false, nil
		// right是其他类型，说明right与left的类型不一样（存在冲突），返回true, nil
		default:
			return true, nil
		}
	// 其他简单类型（string, float64, bool, int64, nil），判断值是否相等
	// 不相等，则返回true, nil。相等，则返回false, nil
	case string, float64, bool, int64, nil:
		return !reflect.DeepEqual(left, right), nil
	// 其他类型返回错误
	default:
		return true, fmt.Errorf("unknown type: %v", reflect.TypeOf(left))
	}
}
