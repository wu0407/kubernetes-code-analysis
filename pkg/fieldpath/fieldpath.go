/*
Copyright 2015 The Kubernetes Authors.

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

package fieldpath

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
)

// FormatMap formats map[string]string to a string.
// 将map[string]string转成一行一行的{key}={value}
func FormatMap(m map[string]string) (fmtStr string) {
	// output with keys in sorted order to provide stable output
	keys := sets.NewString()
	for key := range m {
		keys.Insert(key)
	}
	for _, key := range keys.List() {
		fmtStr += fmt.Sprintf("%v=%q\n", key, m[key])
	}
	fmtStr = strings.TrimSuffix(fmtStr, "\n")

	return
}

// ExtractFieldPathAsString extracts the field from the given object
// and returns it as a string.  The object must be a pointer to an
// API type.
// 解析出fieldPath在obj中的值
// fieldPath包含中括号只支持"metadata.annotations" "metadata.labels"
// fieldPath不包含中括号，支持"metadata.annotations" "metadata.labels" "metadata.name" "metadata.namespace" "metadata.uid"
func ExtractFieldPathAsString(obj interface{}, fieldPath string) (string, error) {
	accessor, err := meta.Accessor(obj)
	if err != nil {
		return "", err
	}

	// 解析fieldPath，返回路径、和key（fieldPath包含中括号，即map类型的key）、和是否为map类型
	// 如果fieldPath包含中括号
	if path, subscript, ok := SplitMaybeSubscriptedPath(fieldPath); ok {
		switch path {
		case "metadata.annotations":
			// 验证subscript是否符合规范，不符合规范返回错误
			if errs := validation.IsQualifiedName(strings.ToLower(subscript)); len(errs) != 0 {
				return "", fmt.Errorf("invalid key subscript in %s: %s", fieldPath, strings.Join(errs, ";"))
			}
			// 返回metadata.annotations[subscript]值
			return accessor.GetAnnotations()[subscript], nil
		case "metadata.labels":
			// 验证subscript是否符合规范，不符合规范返回错误
			if errs := validation.IsQualifiedName(subscript); len(errs) != 0 {
				return "", fmt.Errorf("invalid key subscript in %s: %s", fieldPath, strings.Join(errs, ";"))
			}
			// 返回metadata.labels[subscript]值
			return accessor.GetLabels()[subscript], nil
		default:
			return "", fmt.Errorf("fieldPath %q does not support subscript", fieldPath)
		}
	}

	// fieldPath不包含中括号
	switch fieldPath {
	case "metadata.annotations":
		// 将annotations转成一行一行的{key}={value}
		return FormatMap(accessor.GetAnnotations()), nil
	case "metadata.labels":
		// 将labels转成一行一行的{key}={value}
		return FormatMap(accessor.GetLabels()), nil
	case "metadata.name":
		return accessor.GetName(), nil
	case "metadata.namespace":
		return accessor.GetNamespace(), nil
	case "metadata.uid":
		return string(accessor.GetUID()), nil
	}

	return "", fmt.Errorf("unsupported fieldPath: %v", fieldPath)
}

// SplitMaybeSubscriptedPath checks whether the specified fieldPath is
// subscripted, and
//  - if yes, this function splits the fieldPath into path and subscript, and
//    returns (path, subscript, true).
//  - if no, this function returns (fieldPath, "", false).
//
// Example inputs and outputs:
//  - "metadata.annotations['myKey']" --> ("metadata.annotations", "myKey", true)
//  - "metadata.annotations['a[b]c']" --> ("metadata.annotations", "a[b]c", true)
//  - "metadata.labels['']"           --> ("metadata.labels", "", true)
//  - "metadata.labels"               --> ("metadata.labels", "", false)
// 解析fieldPath，返回路径、和key（如果fieldPath包含中括号，即map类型里key）、和是否为map类型
func SplitMaybeSubscriptedPath(fieldPath string) (string, string, bool) {
	// 没有"']"后缀，直接返回fieldPath，""，false
	if !strings.HasSuffix(fieldPath, "']") {
		return fieldPath, "", false
	}
	s := strings.TrimSuffix(fieldPath, "']")
	parts := strings.SplitN(s, "['", 2)
	// fieldPath不包含"['"
	if len(parts) < 2 {
		return fieldPath, "", false
	}
	// "['"为前缀
	if len(parts[0]) == 0 {
		return fieldPath, "", false
	}
	// 路径和key
	return parts[0], parts[1], true
}
