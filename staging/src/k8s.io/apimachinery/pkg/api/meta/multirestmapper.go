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

package meta

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
)

var (
	_ ResettableRESTMapper = MultiRESTMapper{}
)

// MultiRESTMapper is a wrapper for multiple RESTMappers.
type MultiRESTMapper []RESTMapper

func (m MultiRESTMapper) String() string {
	nested := make([]string, 0, len(m))
	for _, t := range m {
		currString := fmt.Sprintf("%v", t)
		splitStrings := strings.Split(currString, "\n")
		nested = append(nested, strings.Join(splitStrings, "\n\t"))
	}

	return fmt.Sprintf("MultiRESTMapper{\n\t%s\n}", strings.Join(nested, "\n\t"))
}

// ResourceSingularizer converts a REST resource name from plural to singular (e.g., from pods to pod)
// This implementation supports multiple REST schemas and return the first match.
func (m MultiRESTMapper) ResourceSingularizer(resource string) (singular string, err error) {
	for _, t := range m {
		singular, err = t.ResourceSingularizer(resource)
		if err == nil {
			return
		}
	}
	return
}

// 对MultiRESTMapper里所有RESTMapper执行ResourcesFor(resource)，对返回的结果进行去重
func (m MultiRESTMapper) ResourcesFor(resource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	allGVRs := []schema.GroupVersionResource{}
	for _, t := range m {
		// t如果是*DefaultRESTMapper在staging\src\k8s.io\apimachinery\pkg\api\meta\restmapper.go
		// 首先对输入的input schema.GroupVersionResource进行规整化（确保input.Resource是小写，如果input.Version是"__internal"，则变成空""）
		// 然后从m.pluralToSingular查找，至少resource匹配的plural，越多匹配的放在前面
		// 然后对所有匹配的GroupVersionResource进行排序
		gvrs, err := t.ResourcesFor(resource)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}

		// walk the existing values to de-dup
		// 对MultiRESTMapper里所有RESTMapper的ResourcesFor(resource)返回结果进行去重
		for _, curr := range gvrs {
			found := false
			for _, existing := range allGVRs {
				if curr == existing {
					found = true
					break
				}
			}

			if !found {
				allGVRs = append(allGVRs, curr)
			}
		}
	}

	if len(allGVRs) == 0 {
		return nil, &NoResourceMatchError{PartialResource: resource}
	}

	return allGVRs, nil
}

func (m MultiRESTMapper) KindsFor(resource schema.GroupVersionResource) (gvk []schema.GroupVersionKind, err error) {
	allGVKs := []schema.GroupVersionKind{}
	for _, t := range m {
		gvks, err := t.KindsFor(resource)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, err
		}

		// walk the existing values to de-dup
		for _, curr := range gvks {
			found := false
			for _, existing := range allGVKs {
				if curr == existing {
					found = true
					break
				}
			}

			if !found {
				allGVKs = append(allGVKs, curr)
			}
		}
	}

	if len(allGVKs) == 0 {
		return nil, &NoResourceMatchError{PartialResource: resource}
	}

	return allGVKs, nil
}

// 对m MultiRESTMapper里所有RESTMapper执行ResourcesFor(resource)，对返回的结果进行去重
// 如果返回多个结果，则返回错误，否则返回这个结果
func (m MultiRESTMapper) ResourceFor(resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	// 对m MultiRESTMapper里所有RESTMapper执行ResourcesFor(resource)，对返回的结果进行去重
	resources, err := m.ResourcesFor(resource)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	if len(resources) == 1 {
		return resources[0], nil
	}

	return schema.GroupVersionResource{}, &AmbiguousResourceError{PartialResource: resource, MatchingResources: resources}
}

func (m MultiRESTMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	kinds, err := m.KindsFor(resource)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	if len(kinds) == 1 {
		return kinds[0], nil
	}

	return schema.GroupVersionKind{}, &AmbiguousResourceError{PartialResource: resource, MatchingKinds: kinds}
}

// RESTMapping provides the REST mapping for the resource based on the
// kind and version. This implementation supports multiple REST schemas and
// return the first match.
// 遍历所有的RESTMapper，查找所有匹配的RESTMapping，如果只存在一个则返回，否则返回错误
func (m MultiRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	allMappings := []*RESTMapping{}
	errors := []error{}

	// 遍历所有的RESTMapper
	for _, t := range m {
		// DefaultRESTMapper（staging\src\k8s.io\apimachinery\pkg\api\meta\restmapper.go）
		// 从t.kindToPluralResource里查找第一个存在的group version kind
		// 如果没有提供versions，或者version没有在m.kindToPluralResource里，或version为"__internal"
		// 则使用t.defaultGroupVersions查找group与gk的group一样，将所有version与gk组成group version kind
		// 根据这个group version kind，从t.kindToPluralResource查找GroupVersionResource，从t.kindToScope查找scope。最后组成RESTMapping
		// 返回第一个RESTMapping
		currMapping, err := t.RESTMapping(gk, versions...)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			errors = append(errors, err)
			continue
		}

		allMappings = append(allMappings, currMapping)
	}

	// if we got exactly one mapping, then use it even if other requested failed
	if len(allMappings) == 1 {
		return allMappings[0], nil
	}
	// 如果超过一个匹配，返回错误
	if len(allMappings) > 1 {
		var kinds []schema.GroupVersionKind
		for _, m := range allMappings {
			kinds = append(kinds, m.GroupVersionKind)
		}
		return nil, &AmbiguousKindError{PartialKind: gk.WithVersion(""), MatchingKinds: kinds}
	}
	if len(errors) > 0 {
		return nil, utilerrors.NewAggregate(errors)
	}
	return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
}

// RESTMappings returns all possible RESTMappings for the provided group kind, or an error
// if the type is not recognized.
func (m MultiRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	var allMappings []*RESTMapping
	var errors []error

	for _, t := range m {
		// 如果t是DefaultRESTMapper（staging\src\k8s.io\apimachinery\pkg\api\meta\restmapper.go）
		// 如果有提供versions，则遍历所有的version，与gk group kind进行组合成group version kind
		// 从m.kindToPluralResource里查找第一个存在的group version kind
		// 如果没有提供versions，或者version没有在m.kindToPluralResource里，或version为"__internal"
		// 则使用m.defaultGroupVersions查找group与gk的group一样，将所有version与gk组成group version kind
		// 根据这个group version kind，从m.kindToPluralResource查找GroupVersionResource，从m.kindToScope查找scope。最后组成RESTMapping
		currMappings, err := t.RESTMappings(gk, versions...)
		// ignore "no match" errors, but any other error percolates back up
		if IsNoMatchError(err) {
			continue
		}
		if err != nil {
			errors = append(errors, err)
			continue
		}
		allMappings = append(allMappings, currMappings...)
	}
	if len(errors) > 0 {
		return nil, utilerrors.NewAggregate(errors)
	}
	if len(allMappings) == 0 {
		return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	return allMappings, nil
}

func (m MultiRESTMapper) Reset() {
	for _, t := range m {
		MaybeResetRESTMapper(t)
	}
}
