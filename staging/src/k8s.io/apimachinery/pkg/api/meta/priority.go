/*
Copyright 2016 The Kubernetes Authors.

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

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	AnyGroup    = "*"
	AnyVersion  = "*"
	AnyResource = "*"
	AnyKind     = "*"
)

var (
	_ ResettableRESTMapper = PriorityRESTMapper{}
)

// PriorityRESTMapper is a wrapper for automatically choosing a particular Resource or Kind
// when multiple matches are possible
type PriorityRESTMapper struct {
	// Delegate is the RESTMapper to use to locate all the Kind and Resource matches
	Delegate RESTMapper

	// ResourcePriority is a list of priority patterns to apply to matching resources.
	// The list of all matching resources is narrowed based on the patterns until only one remains.
	// A pattern with no matches is skipped.  A pattern with more than one match uses its
	// matches as the list to continue matching against.
	ResourcePriority []schema.GroupVersionResource

	// KindPriority is a list of priority patterns to apply to matching kinds.
	// The list of all matching kinds is narrowed based on the patterns until only one remains.
	// A pattern with no matches is skipped.  A pattern with more than one match uses its
	// matches as the list to continue matching against.
	KindPriority []schema.GroupVersionKind
}

func (m PriorityRESTMapper) String() string {
	return fmt.Sprintf("PriorityRESTMapper{\n\t%v\n\t%v\n\t%v\n}", m.ResourcePriority, m.KindPriority, m.Delegate)
}

// ResourceFor finds all resources, then passes them through the ResourcePriority patterns to find a single matching hit.
func (m PriorityRESTMapper) ResourceFor(partiallySpecifiedResource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	originalGVRs, originalErr := m.Delegate.ResourcesFor(partiallySpecifiedResource)
	if originalErr != nil && len(originalGVRs) == 0 {
		return schema.GroupVersionResource{}, originalErr
	}
	if len(originalGVRs) == 1 {
		return originalGVRs[0], originalErr
	}

	remainingGVRs := append([]schema.GroupVersionResource{}, originalGVRs...)
	for _, pattern := range m.ResourcePriority {
		matchedGVRs := []schema.GroupVersionResource{}
		for _, gvr := range remainingGVRs {
			if resourceMatches(pattern, gvr) {
				matchedGVRs = append(matchedGVRs, gvr)
			}
		}

		switch len(matchedGVRs) {
		case 0:
			// if you have no matches, then nothing matched this pattern just move to the next
			continue
		case 1:
			// one match, return
			return matchedGVRs[0], originalErr
		default:
			// more than one match, use the matched hits as the list moving to the next pattern.
			// this way you can have a series of selection criteria
			remainingGVRs = matchedGVRs
		}
	}

	return schema.GroupVersionResource{}, &AmbiguousResourceError{PartialResource: partiallySpecifiedResource, MatchingResources: originalGVRs}
}

// KindFor finds all kinds, then passes them through the KindPriority patterns to find a single matching hit.
func (m PriorityRESTMapper) KindFor(partiallySpecifiedResource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	originalGVKs, originalErr := m.Delegate.KindsFor(partiallySpecifiedResource)
	if originalErr != nil && len(originalGVKs) == 0 {
		return schema.GroupVersionKind{}, originalErr
	}
	if len(originalGVKs) == 1 {
		return originalGVKs[0], originalErr
	}

	remainingGVKs := append([]schema.GroupVersionKind{}, originalGVKs...)
	for _, pattern := range m.KindPriority {
		matchedGVKs := []schema.GroupVersionKind{}
		for _, gvr := range remainingGVKs {
			if kindMatches(pattern, gvr) {
				matchedGVKs = append(matchedGVKs, gvr)
			}
		}

		switch len(matchedGVKs) {
		case 0:
			// if you have no matches, then nothing matched this pattern just move to the next
			continue
		case 1:
			// one match, return
			return matchedGVKs[0], originalErr
		default:
			// more than one match, use the matched hits as the list moving to the next pattern.
			// this way you can have a series of selection criteria
			remainingGVKs = matchedGVKs
		}
	}

	return schema.GroupVersionKind{}, &AmbiguousResourceError{PartialResource: partiallySpecifiedResource, MatchingKinds: originalGVKs}
}

func resourceMatches(pattern schema.GroupVersionResource, resource schema.GroupVersionResource) bool {
	if pattern.Group != AnyGroup && pattern.Group != resource.Group {
		return false
	}
	if pattern.Version != AnyVersion && pattern.Version != resource.Version {
		return false
	}
	if pattern.Resource != AnyResource && pattern.Resource != resource.Resource {
		return false
	}

	return true
}

func kindMatches(pattern schema.GroupVersionKind, kind schema.GroupVersionKind) bool {
	if pattern.Group != AnyGroup && pattern.Group != kind.Group {
		return false
	}
	if pattern.Version != AnyVersion && pattern.Version != kind.Version {
		return false
	}
	if pattern.Kind != AnyKind && pattern.Kind != kind.Kind {
		return false
	}

	return true
}

// 先从调用m.Delegate.RESTMappings，获得[]*RESTMapping
// 优先匹配用户提供的version和group
func (m PriorityRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (mapping *RESTMapping, err error) {
	// 这里的m.Delegate是MultiRESTMapper（在staging\src\k8s.io\apimachinery\pkg\api\meta\multirestmapper.go）
	// 遍历所有的RESTMapper，查找所有匹配的RESTMapping，如果只存在一个则返回，否则返回错误
	mappings, originalErr := m.Delegate.RESTMappings(gk, versions...)
	if originalErr != nil && len(mappings) == 0 {
		return nil, originalErr
	}

	// any versions the user provides take priority
	priorities := m.KindPriority
	// 如果多个version，则每个version和group、AnyKind组合排前面
	if len(versions) > 0 {
		priorities = make([]schema.GroupVersionKind, 0, len(m.KindPriority)+len(versions))
		for _, version := range versions {
			gv := schema.GroupVersion{
				Version: version,
				Group:   gk.Group,
			}
			priorities = append(priorities, gv.WithKind(AnyKind))
		}
		priorities = append(priorities, m.KindPriority...)
	}

	remaining := append([]*RESTMapping{}, mappings...)
	for _, pattern := range priorities {
		var matching []*RESTMapping
		for _, m := range remaining {
			if kindMatches(pattern, m.GroupVersionKind) {
				matching = append(matching, m)
			}
		}

		switch len(matching) {
		case 0:
			// if you have no matches, then nothing matched this pattern just move to the next
			continue
		case 1:
			// one match, return
			return matching[0], originalErr
		default:
			// more than one match, use the matched hits as the list moving to the next pattern.
			// this way you can have a series of selection criteria
			remaining = matching
		}
	}
	// 没有任何匹配priorities里的group version kind，且只有一个RESTMapping
	if len(remaining) == 1 {
		return remaining[0], originalErr
	}

	// 多个RESTMapping，且匹配priorities里的多个group version kind
	var kinds []schema.GroupVersionKind
	for _, m := range mappings {
		kinds = append(kinds, m.GroupVersionKind)
	}
	return nil, &AmbiguousKindError{PartialKind: gk.WithVersion(""), MatchingKinds: kinds}
}

func (m PriorityRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	return m.Delegate.RESTMappings(gk, versions...)
}

func (m PriorityRESTMapper) ResourceSingularizer(resource string) (singular string, err error) {
	return m.Delegate.ResourceSingularizer(resource)
}

// 对MultiRESTMapper里所有RESTMapper执行ResourcesFor(resource)，对返回的结果进行去重
func (m PriorityRESTMapper) ResourcesFor(partiallySpecifiedResource schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	// 对MultiRESTMapper里所有RESTMapper执行ResourcesFor(resource)，对返回的结果进行去重
	return m.Delegate.ResourcesFor(partiallySpecifiedResource)
}

func (m PriorityRESTMapper) KindsFor(partiallySpecifiedResource schema.GroupVersionResource) (gvk []schema.GroupVersionKind, err error) {
	return m.Delegate.KindsFor(partiallySpecifiedResource)
}

func (m PriorityRESTMapper) Reset() {
	MaybeResetRESTMapper(m.Delegate)
}
