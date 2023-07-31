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

// TODO: move everything in this file to pkg/api/rest
package meta

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Implements RESTScope interface
type restScope struct {
	name RESTScopeName
}

func (r *restScope) Name() RESTScopeName {
	return r.name
}

var RESTScopeNamespace = &restScope{
	name: RESTScopeNameNamespace,
}

var RESTScopeRoot = &restScope{
	name: RESTScopeNameRoot,
}

// DefaultRESTMapper exposes mappings between the types defined in a
// runtime.Scheme. It assumes that all types defined the provided scheme
// can be mapped with the provided MetadataAccessor and Codec interfaces.
//
// The resource name of a Kind is defined as the lowercase,
// English-plural version of the Kind string.
// When converting from resource to Kind, the singular version of the
// resource name is also accepted for convenience.
//
// TODO: Only accept plural for some operations for increased control?
// (`get pod bar` vs `get pods bar`)
type DefaultRESTMapper struct {
	defaultGroupVersions []schema.GroupVersion

	resourceToKind       map[schema.GroupVersionResource]schema.GroupVersionKind
	kindToPluralResource map[schema.GroupVersionKind]schema.GroupVersionResource
	kindToScope          map[schema.GroupVersionKind]RESTScope
	singularToPlural     map[schema.GroupVersionResource]schema.GroupVersionResource
	pluralToSingular     map[schema.GroupVersionResource]schema.GroupVersionResource
}

func (m *DefaultRESTMapper) String() string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("DefaultRESTMapper{kindToPluralResource=%v}", m.kindToPluralResource)
}

var _ RESTMapper = &DefaultRESTMapper{}

// NewDefaultRESTMapper initializes a mapping between Kind and APIVersion
// to a resource name and back based on the objects in a runtime.Scheme
// and the Kubernetes API conventions. Takes a group name, a priority list of the versions
// to search when an object has no default version (set empty to return an error),
// and a function that retrieves the correct metadata for a given version.
func NewDefaultRESTMapper(defaultGroupVersions []schema.GroupVersion) *DefaultRESTMapper {
	resourceToKind := make(map[schema.GroupVersionResource]schema.GroupVersionKind)
	kindToPluralResource := make(map[schema.GroupVersionKind]schema.GroupVersionResource)
	kindToScope := make(map[schema.GroupVersionKind]RESTScope)
	singularToPlural := make(map[schema.GroupVersionResource]schema.GroupVersionResource)
	pluralToSingular := make(map[schema.GroupVersionResource]schema.GroupVersionResource)
	// TODO: verify name mappings work correctly when versions differ

	return &DefaultRESTMapper{
		resourceToKind:       resourceToKind,
		kindToPluralResource: kindToPluralResource,
		kindToScope:          kindToScope,
		defaultGroupVersions: defaultGroupVersions,
		singularToPlural:     singularToPlural,
		pluralToSingular:     pluralToSingular,
	}
}

func (m *DefaultRESTMapper) Add(kind schema.GroupVersionKind, scope RESTScope) {
	plural, singular := UnsafeGuessKindToResource(kind)
	m.AddSpecific(kind, plural, singular, scope)
}

// DefaultRESTMapper里singularToPlural[singular] = plural
// DefaultRESTMapper里pluralToSingular[plural] = singular
// DefaultRESTMapper里resourceToKind[singular] = kind
// DefaultRESTMapper里resourceToKind[plural] = kind
// DefaultRESTMapper里kindToPluralResource[kind] = plural
// DefaultRESTMapper里kindToScope[kind] = scope
func (m *DefaultRESTMapper) AddSpecific(kind schema.GroupVersionKind, plural, singular schema.GroupVersionResource, scope RESTScope) {
	m.singularToPlural[singular] = plural
	m.pluralToSingular[plural] = singular

	m.resourceToKind[singular] = kind
	m.resourceToKind[plural] = kind

	m.kindToPluralResource[kind] = plural
	m.kindToScope[kind] = scope
}

// unpluralizedSuffixes is a list of resource suffixes that are the same plural and singular
// This is only is only necessary because some bits of code are lazy and don't actually use the RESTMapper like they should.
// TODO eliminate this so that different callers can correctly map to resources.  This probably means updating all
// callers to use the RESTMapper they mean.
var unpluralizedSuffixes = []string{
	"endpoints",
}

// UnsafeGuessKindToResource converts Kind to a resource name.
// Broken. This method only "sort of" works when used outside of this package.  It assumes that Kinds and Resources match
// and they aren't guaranteed to do so.
// 如果kind里面的Kind为空，则返回plural GroupVersionResource和singular GroupVersionResource为空
// 否则 singular GroupVersionResource为kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
// plural为
// kind为"Endpoints"，则跟singular一样
// "singularName"为s结尾，则Resource为singularName加"es"，kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
// "singularName"为y结尾，则Resource为singularName里的"y"变成"ies"，kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
// 其他情况，Resource为singularName加"s"，kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
func UnsafeGuessKindToResource(kind schema.GroupVersionKind) ( /*plural*/ schema.GroupVersionResource /*singular*/, schema.GroupVersionResource) {
	kindName := kind.Kind
	if len(kindName) == 0 {
		return schema.GroupVersionResource{}, schema.GroupVersionResource{}
	}
	singularName := strings.ToLower(kindName)
	singular := kind.GroupVersion().WithResource(singularName)

	for _, skip := range unpluralizedSuffixes {
		// kind为"Endpoints"，则返回singular, singular
		if strings.HasSuffix(singularName, skip) {
			return singular, singular
		}
	}

	switch string(singularName[len(singularName)-1]) {
	case "s":
		// "singularName"为s结尾，则Resource为singularName加"es"，kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
		return kind.GroupVersion().WithResource(singularName + "es"), singular
	case "y":
		// "singularName"为y结尾，则Resource为singularName里的"y"变成"ies"，kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
		return kind.GroupVersion().WithResource(strings.TrimSuffix(singularName, "y") + "ies"), singular
	}

	// Resource为singularName加"s"，kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
	return kind.GroupVersion().WithResource(singularName + "s"), singular
}

// ResourceSingularizer implements RESTMapper
// It converts a resource name from plural to singular (e.g., from pods to pod)
func (m *DefaultRESTMapper) ResourceSingularizer(resourceType string) (string, error) {
	partialResource := schema.GroupVersionResource{Resource: resourceType}
	resources, err := m.ResourcesFor(partialResource)
	if err != nil {
		return resourceType, err
	}

	singular := schema.GroupVersionResource{}
	for _, curr := range resources {
		currSingular, ok := m.pluralToSingular[curr]
		if !ok {
			continue
		}
		if singular.Empty() {
			singular = currSingular
			continue
		}

		if currSingular.Resource != singular.Resource {
			return resourceType, fmt.Errorf("multiple possible singular resources (%v) found for %v", resources, resourceType)
		}
	}

	if singular.Empty() {
		return resourceType, fmt.Errorf("no singular of resource %v has been defined", resourceType)
	}

	return singular.Resource, nil
}

// coerceResourceForMatching makes the resource lower case and converts internal versions to unspecified (legacy behavior)
// 确保resource.Resource是小写，如果resource.Version是"__internal"，则变成空""
func coerceResourceForMatching(resource schema.GroupVersionResource) schema.GroupVersionResource {
	resource.Resource = strings.ToLower(resource.Resource)
	if resource.Version == runtime.APIVersionInternal {
		resource.Version = ""
	}

	return resource
}

// 首先对输入的input schema.GroupVersionResource进行规整化（确保input.Resource是小写，如果input.Version是"__internal"，则变成空""）
// 然后从m.pluralToSingular查找，至少resource匹配的plural，越多匹配的放在前面
// 然后对所有匹配的GroupVersionResource进行排序
func (m *DefaultRESTMapper) ResourcesFor(input schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	// 确保input.Resource是小写，如果input.Version是"__internal"，则变成空""
	resource := coerceResourceForMatching(input)

	hasResource := len(resource.Resource) > 0
	hasGroup := len(resource.Group) > 0
	hasVersion := len(resource.Version) > 0

	if !hasResource {
		return nil, fmt.Errorf("a resource must be present, got: %v", resource)
	}

	ret := []schema.GroupVersionResource{}
	switch {
	case hasGroup && hasVersion:
		// fully qualified.  Find the exact match
		for plural, singular := range m.pluralToSingular {
			// resource是singular类型，则将plural append到ret
			if singular == resource {
				ret = append(ret, plural)
				break
			}
			// resource是plural类型，则plural append到ret
			if plural == resource {
				ret = append(ret, plural)
				break
			}
		}

	// 只有group情况
	case hasGroup:
		// given a group, prefer an exact match.  If you don't find one, resort to a prefix match on group
		foundExactMatch := false
		requestedGroupResource := resource.GroupResource()
		// 既然没有version，那么只要比较group和resource就行了
		for plural, singular := range m.pluralToSingular {
			// group resource是singular.GroupResource()，则将plural append到ret
			if singular.GroupResource() == requestedGroupResource {
				foundExactMatch = true
				ret = append(ret, plural)
			}
			//  group resource是plural.GroupResource()，则将plural append到ret
			if plural.GroupResource() == requestedGroupResource {
				foundExactMatch = true
				ret = append(ret, plural)
			}
		}

		// if you didn't find an exact match, match on group prefixing. This allows storageclass.storage to match
		// storageclass.storage.k8s.io
		// 在m.pluralToSingular里没有找到group resource一样的，则进行group前缀匹配
		if !foundExactMatch {
			for plural, singular := range m.pluralToSingular {
				// plural.Group没有requestedGroupResource.Group前缀，则跳过
				if !strings.HasPrefix(plural.Group, requestedGroupResource.Group) {
					continue
				}
				// resource是singular.Resource，则将plural append到ret
				if singular.Resource == requestedGroupResource.Resource {
					ret = append(ret, plural)
				}
				// resource是plural.Resource，则将plural append到ret
				if plural.Resource == requestedGroupResource.Resource {
					ret = append(ret, plural)
				}
			}

		}

	// 只有version情况
	case hasVersion:
		// 既然没有group，那就比较version和resource
		for plural, singular := range m.pluralToSingular {
			// resource.Version和resource.Resource和singular的Version和Resource相等，则将plural append到ret
			if singular.Version == resource.Version && singular.Resource == resource.Resource {
				ret = append(ret, plural)
			}
			// resource.Version和resource.Resource和plural的Version和Resource相等，则将plural append到ret
			if plural.Version == resource.Version && plural.Resource == resource.Resource {
				ret = append(ret, plural)
			}
		}

	// 即没有group和version，那么就只能比较resource匹配
	default:
		for plural, singular := range m.pluralToSingular {
			// resource跟singular.Resource一样，则将plural append到ret
			if singular.Resource == resource.Resource {
				ret = append(ret, plural)
			}
			// resource跟plural.Resource一样，则将plural append到ret
			if plural.Resource == resource.Resource {
				ret = append(ret, plural)
			}
		}
	}

	// 都没有找到
	if len(ret) == 0 {
		return nil, &NoResourceMatchError{PartialResource: resource}
	}

	// group version resource一样，则j排前面
	// 只有group version一样的情况下，按照resource排序
	// GroupVersion不一样，那么比较i和j的GroupVersion在o.sortOrder里的位置，在o.sortOrder里的位置前面的排在前面。
	// 如果都不在o.sortOrder里，则还是原本的顺序。如果i在o.sortOrder里j不在，则i排在前面。如果j在o.sortOrder里，i不在，则i排在前面（这里由于上面ret里都是group version一样，所以不会存在这个bug问题）
	sort.Sort(resourceByPreferredGroupVersion{ret, m.defaultGroupVersions})
	return ret, nil
}

func (m *DefaultRESTMapper) ResourceFor(resource schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	resources, err := m.ResourcesFor(resource)
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	if len(resources) == 1 {
		return resources[0], nil
	}

	return schema.GroupVersionResource{}, &AmbiguousResourceError{PartialResource: resource, MatchingResources: resources}
}

func (m *DefaultRESTMapper) KindsFor(input schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	resource := coerceResourceForMatching(input)

	hasResource := len(resource.Resource) > 0
	hasGroup := len(resource.Group) > 0
	hasVersion := len(resource.Version) > 0

	if !hasResource {
		return nil, fmt.Errorf("a resource must be present, got: %v", resource)
	}

	ret := []schema.GroupVersionKind{}
	switch {
	// fully qualified.  Find the exact match
	case hasGroup && hasVersion:
		kind, exists := m.resourceToKind[resource]
		if exists {
			ret = append(ret, kind)
		}

	case hasGroup:
		foundExactMatch := false
		requestedGroupResource := resource.GroupResource()
		for currResource, currKind := range m.resourceToKind {
			if currResource.GroupResource() == requestedGroupResource {
				foundExactMatch = true
				ret = append(ret, currKind)
			}
		}

		// if you didn't find an exact match, match on group prefixing. This allows storageclass.storage to match
		// storageclass.storage.k8s.io
		if !foundExactMatch {
			for currResource, currKind := range m.resourceToKind {
				if !strings.HasPrefix(currResource.Group, requestedGroupResource.Group) {
					continue
				}
				if currResource.Resource == requestedGroupResource.Resource {
					ret = append(ret, currKind)
				}
			}

		}

	case hasVersion:
		for currResource, currKind := range m.resourceToKind {
			if currResource.Version == resource.Version && currResource.Resource == resource.Resource {
				ret = append(ret, currKind)
			}
		}

	default:
		for currResource, currKind := range m.resourceToKind {
			if currResource.Resource == resource.Resource {
				ret = append(ret, currKind)
			}
		}
	}

	if len(ret) == 0 {
		return nil, &NoResourceMatchError{PartialResource: input}
	}

	sort.Sort(kindByPreferredGroupVersion{ret, m.defaultGroupVersions})
	return ret, nil
}

func (m *DefaultRESTMapper) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	kinds, err := m.KindsFor(resource)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	if len(kinds) == 1 {
		return kinds[0], nil
	}

	return schema.GroupVersionKind{}, &AmbiguousResourceError{PartialResource: resource, MatchingKinds: kinds}
}

type kindByPreferredGroupVersion struct {
	list      []schema.GroupVersionKind
	sortOrder []schema.GroupVersion
}

func (o kindByPreferredGroupVersion) Len() int      { return len(o.list) }
func (o kindByPreferredGroupVersion) Swap(i, j int) { o.list[i], o.list[j] = o.list[j], o.list[i] }
func (o kindByPreferredGroupVersion) Less(i, j int) bool {
	lhs := o.list[i]
	rhs := o.list[j]
	if lhs == rhs {
		return false
	}

	if lhs.GroupVersion() == rhs.GroupVersion() {
		return lhs.Kind < rhs.Kind
	}

	// otherwise, the difference is in the GroupVersion, so we need to sort with respect to the preferred order
	lhsIndex := -1
	rhsIndex := -1

	for i := range o.sortOrder {
		if o.sortOrder[i] == lhs.GroupVersion() {
			lhsIndex = i
		}
		if o.sortOrder[i] == rhs.GroupVersion() {
			rhsIndex = i
		}
	}

	if rhsIndex == -1 {
		return true
	}

	return lhsIndex < rhsIndex
}

type resourceByPreferredGroupVersion struct {
	list      []schema.GroupVersionResource
	sortOrder []schema.GroupVersion
}

func (o resourceByPreferredGroupVersion) Len() int      { return len(o.list) }
func (o resourceByPreferredGroupVersion) Swap(i, j int) { o.list[i], o.list[j] = o.list[j], o.list[i] }
func (o resourceByPreferredGroupVersion) Less(i, j int) bool {
	lhs := o.list[i]
	rhs := o.list[j]
	// group version resource一样，则j排前面
	if lhs == rhs {
		return false
	}

	// group version一样的情况下，按照resource排序
	if lhs.GroupVersion() == rhs.GroupVersion() {
		return lhs.Resource < rhs.Resource
	}

	// otherwise, the difference is in the GroupVersion, so we need to sort with respect to the preferred order
	// GroupVersion不一样，那么比较i和j的GroupVersion在o.sortOrder里的位置，在o.sortOrder里的位置前面的排在前面。
	// 如果都不在o.sortOrder里，则还是原本的顺序。如果i在o.sortOrder里j不在，则i排在前面。如果j在o.sortOrder里，i不在，则i排在前面（这里存在bug）
	lhsIndex := -1
	rhsIndex := -1

	// 统计i和j的GroupVersion在o.sortOrder里的位置
	for i := range o.sortOrder {
		if o.sortOrder[i] == lhs.GroupVersion() {
			lhsIndex = i
		}
		if o.sortOrder[i] == rhs.GroupVersion() {
			rhsIndex = i
		}
	}

	if rhsIndex == -1 {
		return true
	}

	return lhsIndex < rhsIndex
}

// RESTMapping returns a struct representing the resource path and conversion interfaces a
// RESTClient should use to operate on the provided group/kind in order of versions. If a version search
// order is not provided, the search order provided to DefaultRESTMapper will be used to resolve which
// version should be used to access the named group/kind.
// 从m.kindToPluralResource里查找第一个存在的group version kind
// 如果没有提供versions，或者version没有在m.kindToPluralResource里，或version为"__internal"
// 则使用m.defaultGroupVersions查找group与gk的group一样，将所有version与gk组成group version kind
// 根据这个group version kind，从m.kindToPluralResource查找GroupVersionResource，从m.kindToScope查找scope。最后组成RESTMapping
// 返回第一个RESTMapping
func (m *DefaultRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*RESTMapping, error) {
	// 从m.kindToPluralResource里查找第一个存在的group version kind
	// 如果没有提供versions，或者version没有在m.kindToPluralResource里，或version为"__internal"
	// 则使用m.defaultGroupVersions查找group与gk的group一样，将所有version与gk组成group version kind
	// 根据这个group version kind，从m.kindToPluralResource查找GroupVersionResource，从m.kindToScope查找scope。最后组成RESTMapping
	mappings, err := m.RESTMappings(gk, versions...)
	if err != nil {
		return nil, err
	}
	if len(mappings) == 0 {
		return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	// since we rely on RESTMappings method
	// take the first match and return to the caller
	// as this was the existing behavior.
	return mappings[0], nil
}

// RESTMappings returns the RESTMappings for the provided group kind. If a version search order
// is not provided, the search order provided to DefaultRESTMapper will be used.
// 如果有提供versions，则遍历所有的version，与gk group kind进行组合成group version kind
// 从m.kindToPluralResource里查找第一个存在的group version kind
// 如果没有提供versions，或者version没有在m.kindToPluralResource里，或version为"__internal"
// 则使用m.defaultGroupVersions查找group与gk的group一样，将所有version与gk组成group version kind
// 根据这个group version kind，从m.kindToPluralResource查找GroupVersionResource，从m.kindToScope查找scope。最后组成RESTMapping
func (m *DefaultRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) ([]*RESTMapping, error) {
	mappings := make([]*RESTMapping, 0)
	potentialGVK := make([]schema.GroupVersionKind, 0)
	hadVersion := false

	// Pick an appropriate version
	// 如果有提供versions，则遍历所有的version，与gk group kind进行组合成group version kind
	// 只挑选第一个m.kindToPluralResource里存在的group version kind，将group version kind append到potentialGVK
	for _, version := range versions {
		// 忽略空的version和version是"__internal"
		if len(version) == 0 || version == runtime.APIVersionInternal {
			continue
		}
		currGVK := gk.WithVersion(version)
		hadVersion = true
		if _, ok := m.kindToPluralResource[currGVK]; ok {
			potentialGVK = append(potentialGVK, currGVK)
			break
		}
	}
	// Use the default preferred versions
	// 如果没有提供versions，或者version没有在m.kindToPluralResource里，或version为"__internal"
	// 则使用m.defaultGroupVersions查找group与gk的group一样，将所有version与gk组成group version kind
	if !hadVersion && len(potentialGVK) == 0 {
		for _, gv := range m.defaultGroupVersions {
			if gv.Group != gk.Group {
				continue
			}
			potentialGVK = append(potentialGVK, gk.WithVersion(gv.Version))
		}
	}

	// 还是没找到，返回错误
	if len(potentialGVK) == 0 {
		return nil, &NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}

	for _, gvk := range potentialGVK {
		//Ensure we have a REST mapping
		res, ok := m.kindToPluralResource[gvk]
		if !ok {
			continue
		}

		// Ensure we have a REST scope
		scope, ok := m.kindToScope[gvk]
		if !ok {
			return nil, fmt.Errorf("the provided version %q and kind %q cannot be mapped to a supported scope", gvk.GroupVersion(), gvk.Kind)
		}

		mappings = append(mappings, &RESTMapping{
			Resource:         res,
			GroupVersionKind: gvk,
			Scope:            scope,
		})
	}

	if len(mappings) == 0 {
		return nil, &NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: gk.Group, Resource: gk.Kind}}
	}
	return mappings, nil
}

// MaybeResetRESTMapper calls Reset() on the mapper if it is a ResettableRESTMapper.
func MaybeResetRESTMapper(mapper RESTMapper) bool {
	m, ok := mapper.(ResettableRESTMapper)
	if ok {
		m.Reset()
	}
	return ok
}
