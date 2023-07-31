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

package restmapper

import (
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"k8s.io/klog/v2"
)

// APIGroupResources is an API group with a mapping of versions to
// resources.
type APIGroupResources struct {
	Group metav1.APIGroup
	// A mapping of version string to a slice of APIResources for
	// that version.
	VersionedResources map[string][]metav1.APIResource
}

// NewDiscoveryRESTMapper returns a PriorityRESTMapper based on the discovered
// groups and resources passed in.
func NewDiscoveryRESTMapper(groupResources []*APIGroupResources) meta.RESTMapper {
	unionMapper := meta.MultiRESTMapper{}

	var groupPriority []string
	// /v1 is special.  It should always come first
	// {Group: "", Version: "v1", Resource: "*"}排resourcePriority前面
	resourcePriority := []schema.GroupVersionResource{{Group: "", Version: "v1", Resource: meta.AnyResource}}
	// {Group: "", Version: "v1", Kind: "*"}排kindPriority前面
	kindPriority := []schema.GroupVersionKind{{Group: "", Version: "v1", Kind: meta.AnyKind}}

	for _, group := range groupResources {
		groupPriority = append(groupPriority, group.Group.Name)

		// Make sure the preferred version comes first
		if len(group.Group.PreferredVersion.Version) != 0 {
			preferred := group.Group.PreferredVersion.Version
			if _, ok := group.VersionedResources[preferred]; ok {
				// preferred Version  {Group: group.Group.Name, Version: group.Group.PreferredVersion.Version, Resource: "*"}排在resourcePriority第二
				resourcePriority = append(resourcePriority, schema.GroupVersionResource{
					Group:    group.Group.Name,
					Version:  group.Group.PreferredVersion.Version,
					Resource: meta.AnyResource,
				})

				// preferred Version  {Group: group.Group.Name, Version: group.Group.PreferredVersion.Version, Kind: "*"}排在kindPriority第二
				kindPriority = append(kindPriority, schema.GroupVersionKind{
					Group:   group.Group.Name,
					Version: group.Group.PreferredVersion.Version,
					Kind:    meta.AnyKind,
				})
			}
		}

		// 遍历这个group下的所有版本
		for _, discoveryVersion := range group.Group.Versions {
			// 版本下面的所有resource
			resources, ok := group.VersionedResources[discoveryVersion.Version]
			if !ok {
				continue
			}

			// Add non-preferred versions after the preferred version, in case there are resources that only exist in those versions
			if discoveryVersion.Version != group.Group.PreferredVersion.Version {
				// 不是PreferredVersion，{Group: group.Group.Name, Version: discoveryVersion.Version, Resource: "*"}排在resourcePriority后面
				resourcePriority = append(resourcePriority, schema.GroupVersionResource{
					Group:    group.Group.Name,
					Version:  discoveryVersion.Version,
					Resource: meta.AnyResource,
				})

				// 不是PreferredVersion，{Group: group.Group.Name, Version: discoveryVersion.Version, Kind: "*"}排在kindPriority后面
				kindPriority = append(kindPriority, schema.GroupVersionKind{
					Group:   group.Group.Name,
					Version: discoveryVersion.Version,
					Kind:    meta.AnyKind,
				})
			}

			gv := schema.GroupVersion{Group: group.Group.Name, Version: discoveryVersion.Version}
			versionMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})

			// 遍历这个版本下的所有metav1.APIResource
			for _, resource := range resources {
				scope := meta.RESTScopeNamespace
				if !resource.Namespaced {
					scope = meta.RESTScopeRoot
				}

				// if we have a slash, then this is a subresource and we shouldn't create mappings for those.
				if strings.Contains(resource.Name, "/") {
					continue
				}

				plural := gv.WithResource(resource.Name)
				singular := gv.WithResource(resource.SingularName)
				// this is for legacy resources and servers which don't list singular forms.  For those we must still guess.
				// /api/v1（k8s.io/api/core/v1）下返回结果里"singularName"为空
				if len(resource.SingularName) == 0 {
					// 如果kind里面的Kind为空，则返回plural GroupVersionResource和singular GroupVersionResource为空
					// 否则 singular GroupVersionResource为kind schema.GroupVersionKind里的Kind变成小写，然后组装成GroupVersionResource
					_, singular = meta.UnsafeGuessKindToResource(gv.WithKind(resource.Kind))
				}

				// versionMapper里singularToPlural[singular] = plural
				// versionMapper里pluralToSingular[plural] = singular
				// versionMapper里resourceToKind[singular] = kind（Kind变小写）
				// versionMapper里resourceToKind[plural] = kind（Kind变小写）
				// versionMapper里kindToPluralResource[kind（Kind变小写）] = plural
				// versionMapper里kindToScope[kind（Kind变小写）] = scope
				versionMapper.AddSpecific(gv.WithKind(strings.ToLower(resource.Kind)), plural, singular, scope)
				// versionMapper里singularToPlural[singular] = plural
				// versionMapper里pluralToSingular[plural] = singular
				// versionMapper里resourceToKind[singular] = kind（Kind变大写）
				// versionMapper里resourceToKind[plural] = kind（Kind变大写）
				// versionMapper里kindToPluralResource[kind（Kind变大写）] = plural
				// versionMapper里kindToScope[kind（Kind变大写）] = scope

				// 最后为
				// versionMapper里singularToPlural[singular] = plural
				// versionMapper里pluralToSingular[plural] = singular
				// versionMapper里resourceToKind[singular] = kind（Kind变大写）
				// versionMapper里resourceToKind[plural] = kind（Kind变大写）
				// versionMapper里kindToPluralResource[kind（Kind变小写）] = plural
				// versionMapper里kindToScope[kind（Kind变小写）] = scope
				// versionMapper里kindToPluralResource[kind（Kind变大写）] = plural
				// versionMapper里kindToScope[kind（Kind变大写）] = scope
				versionMapper.AddSpecific(gv.WithKind(resource.Kind), plural, singular, scope)
				// TODO this is producing unsafe guesses that don't actually work, but it matches previous behavior
				// 这里的内部singular为groupVersionKind（kind加"list"）
				// plural为groupVersionKind（kind加"lists"）
				// versionMapper里singularToPlural[singular（kind加"list"）] = plural（groupVersionKind（kind加"lists"））
				// versionMapper里pluralToSingular[plural（groupVersionKind（kind加"lists"））] = singular（kind加"list"）
				// versionMapper里resourceToKind[singular（kind加"list"）] = kind（Kind加"Lists"）
				// versionMapper里resourceToKind[plural（groupVersionKind（kind加"lists"）] = kind（Kind加"Lists"）
				// versionMapper里kindToPluralResource[kind（Kind加"Lists"））] = plural（groupVersionKind（kind加"lists"）
				// versionMapper里kindToScope[kind（Kind加"Lists"）] = scope
				versionMapper.Add(gv.WithKind(resource.Kind+"List"), scope)
			}
			// TODO why is this type not in discovery (at least for "v1")
			// 这里的内部singular为groupVersionKind（kind为"list"），scope为meta.RESTScopeRoot
			// plural为groupVersionKind（kind为"lists"）
			// versionMapper里singularToPlural[singular（groupVersionKind（kind为"list"））] = plural（plural为groupVersionKind（kind为"lists"））
			// versionMapper里pluralToSingular[plural（plural为groupVersionKind（kind为"lists"））] = singular（groupVersionKind（kind为"list"）
			// versionMapper里resourceToKind[singular] = kind（Kind为groupVersion（Kind为"List"））
			// versionMapper里resourceToKind[plural（plural为groupVersionKind（kind为"lists"））] = kind（Kind为groupVersion（Kind为"List"））
			// versionMapper里kindToPluralResource[kind（Kind为groupVersion（Kind为"List"））] = plural（plural为groupVersionKind（kind为"lists"））
			// versionMapper里kindToScope[kind（Kind为groupVersion（Kind为"List"））] = scope
			versionMapper.Add(gv.WithKind("List"), meta.RESTScopeRoot)
			unionMapper = append(unionMapper, versionMapper)
		}
	}

	// 最后添加任意version、任意resource {Group: group, Version: "*", Resource: "*"}在resourcePriority最后
	// 添加任意version、任意kind {Group: group, Version: "*", Kind: "*"}在kindPriority最后
	for _, group := range groupPriority {
		resourcePriority = append(resourcePriority, schema.GroupVersionResource{
			Group:    group,
			Version:  meta.AnyVersion,
			Resource: meta.AnyResource,
		})
		kindPriority = append(kindPriority, schema.GroupVersionKind{
			Group:   group,
			Version: meta.AnyVersion,
			Kind:    meta.AnyKind,
		})
	}

	return meta.PriorityRESTMapper{
		// 所有group version kind对应的resourceToKind（map）、kindToPluralResource（map）、kindToScope（map）、defaultGroupVersions（group version列表）、singularToPlural（map）、pluralToSingular（map）
		Delegate:         unionMapper,
		// 所有group version resource列表（顺序代表优先级）
		ResourcePriority: resourcePriority,
		// 所有group version kind列表（顺序代表优先级）
		KindPriority:     kindPriority,
	}
}

// GetAPIGroupResources uses the provided discovery client to gather
// discovery information and populate a slice of APIGroupResources.
func GetAPIGroupResources(cl discovery.DiscoveryInterface) ([]*APIGroupResources, error) {
	// 先从discovery缓存目录（.kube/cache/discovery/{server addr去除"https://"后非点号特殊字符处理成下划线}）中，读取"servergroups.json"，如果成功进行返回
	// 否则请求"/api"和"/apis"，将返回数据进行聚合成metav1.APIGroupList，然后把数据写到discovery缓存目录中
	// 然后根据这个metav1.APIGroupList里的Groups列表
	// 并发的获取apiGroups对应的map[schema.GroupVersion]*metav1.APIResourceList和map[schema.GroupVersion]error对应的错误
	// 传入的DiscoveryInterface，如果是CachedDiscoveryClient
	// 先从缓存目录（~/.kube/discovery/{group}/{version}/）下的"serverresources.json"读取并解析出metav1.APIResourceList，如果成功，则返回
	// 否则 请求"/api/v1"或"/apis/{group}/{Version}" 返回metav1.APIResourceList，然后将返回写入缓存文件中
	// 最后进行数据处理，返回[]*metav1.APIGroup, []*metav1.APIResourceList, error
	gs, rs, err := cl.ServerGroupsAndResources()
	if rs == nil || gs == nil {
		return nil, err
		// TODO track the errors and update callers to handle partial errors.
	}
	rsm := map[string]*metav1.APIResourceList{}
	for _, r := range rs {
		rsm[r.GroupVersion] = r
	}

	var result []*APIGroupResources
	for _, group := range gs {
		groupResources := &APIGroupResources{
			Group:              *group,
			// 这个group下面所有version和对应的metav1.APIResource
			VersionedResources: make(map[string][]metav1.APIResource),
		}
		for _, version := range group.Versions {
			resources, ok := rsm[version.GroupVersion]
			if !ok {
				continue
			}
			groupResources.VersionedResources[version.Version] = resources.APIResources
		}
		result = append(result, groupResources)
	}
	return result, nil
}

// DeferredDiscoveryRESTMapper is a RESTMapper that will defer
// initialization of the RESTMapper until the first mapping is
// requested.
type DeferredDiscoveryRESTMapper struct {
	initMu   sync.Mutex
	delegate meta.RESTMapper
	cl       discovery.CachedDiscoveryInterface
}

// NewDeferredDiscoveryRESTMapper returns a
// DeferredDiscoveryRESTMapper that will lazily query the provided
// client for discovery information to do REST mappings.
func NewDeferredDiscoveryRESTMapper(cl discovery.CachedDiscoveryInterface) *DeferredDiscoveryRESTMapper {
	return &DeferredDiscoveryRESTMapper{
		cl: cl,
	}
}

func (d *DeferredDiscoveryRESTMapper) getDelegate() (meta.RESTMapper, error) {
	d.initMu.Lock()
	defer d.initMu.Unlock()

	if d.delegate != nil {
		return d.delegate, nil
	}

	// 通过discovery.CachedDiscoveryInterface获得[]*APIGroupResources（group与group下面的所有version和对应的metav1.APIResource）
	groupResources, err := GetAPIGroupResources(d.cl)
	if err != nil {
		return nil, err
	}

	// 生成meta.PriorityRESTMapper
	d.delegate = NewDiscoveryRESTMapper(groupResources)
	return d.delegate, nil
}

// Reset resets the internally cached Discovery information and will
// cause the next mapping request to re-discover.
func (d *DeferredDiscoveryRESTMapper) Reset() {
	klog.V(5).Info("Invalidating discovery information")

	d.initMu.Lock()
	defer d.initMu.Unlock()

	d.cl.Invalidate()
	d.delegate = nil
}

// KindFor takes a partial resource and returns back the single match.
// It returns an error if there are multiple matches.
func (d *DeferredDiscoveryRESTMapper) KindFor(resource schema.GroupVersionResource) (gvk schema.GroupVersionKind, err error) {
	del, err := d.getDelegate()
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	gvk, err = del.KindFor(resource)
	if err != nil && !d.cl.Fresh() {
		d.Reset()
		gvk, err = d.KindFor(resource)
	}
	return
}

// KindsFor takes a partial resource and returns back the list of
// potential kinds in priority order.
func (d *DeferredDiscoveryRESTMapper) KindsFor(resource schema.GroupVersionResource) (gvks []schema.GroupVersionKind, err error) {
	del, err := d.getDelegate()
	if err != nil {
		return nil, err
	}
	gvks, err = del.KindsFor(resource)
	if len(gvks) == 0 && !d.cl.Fresh() {
		d.Reset()
		gvks, err = d.KindsFor(resource)
	}
	return
}

// ResourceFor takes a partial resource and returns back the single
// match. It returns an error if there are multiple matches.
func (d *DeferredDiscoveryRESTMapper) ResourceFor(input schema.GroupVersionResource) (gvr schema.GroupVersionResource, err error) {
	del, err := d.getDelegate()
	if err != nil {
		return schema.GroupVersionResource{}, err
	}
	// del为PriorityRESTMapper在staging\src\k8s.io\apimachinery\pkg\api\meta\priority.go
	// 对MultiRESTMapper里所有RESTMapper执行ResourcesFor(resource)，对返回的结果进行去重
	gvr, err = del.ResourceFor(input)
	if err != nil && !d.cl.Fresh() {
		d.Reset()
		gvr, err = d.ResourceFor(input)
	}
	return
}

// ResourcesFor takes a partial resource and returns back the list of
// potential resource in priority order.
func (d *DeferredDiscoveryRESTMapper) ResourcesFor(input schema.GroupVersionResource) (gvrs []schema.GroupVersionResource, err error) {
	del, err := d.getDelegate()
	if err != nil {
		return nil, err
	}
	gvrs, err = del.ResourcesFor(input)
	if len(gvrs) == 0 && !d.cl.Fresh() {
		d.Reset()
		gvrs, err = d.ResourcesFor(input)
	}
	return
}

// RESTMapping identifies a preferred resource mapping for the
// provided group kind.
func (d *DeferredDiscoveryRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (m *meta.RESTMapping, err error) {
	del, err := d.getDelegate()
	if err != nil {
		return nil, err
	}
	m, err = del.RESTMapping(gk, versions...)
	if err != nil && !d.cl.Fresh() {
		d.Reset()
		// 进行重试（重新执行自己）
		m, err = d.RESTMapping(gk, versions...)
	}
	return
}

// RESTMappings returns the RESTMappings for the provided group kind
// in a rough internal preferred order. If no kind is found, it will
// return a NoResourceMatchError.
func (d *DeferredDiscoveryRESTMapper) RESTMappings(gk schema.GroupKind, versions ...string) (ms []*meta.RESTMapping, err error) {
	del, err := d.getDelegate()
	if err != nil {
		return nil, err
	}
	ms, err = del.RESTMappings(gk, versions...)
	if len(ms) == 0 && !d.cl.Fresh() {
		d.Reset()
		ms, err = d.RESTMappings(gk, versions...)
	}
	return
}

// ResourceSingularizer converts a resource name from plural to
// singular (e.g., from pods to pod).
func (d *DeferredDiscoveryRESTMapper) ResourceSingularizer(resource string) (singular string, err error) {
	del, err := d.getDelegate()
	if err != nil {
		return resource, err
	}
	singular, err = del.ResourceSingularizer(resource)
	if err != nil && !d.cl.Fresh() {
		d.Reset()
		singular, err = d.ResourceSingularizer(resource)
	}
	return
}

func (d *DeferredDiscoveryRESTMapper) String() string {
	del, err := d.getDelegate()
	if err != nil {
		return fmt.Sprintf("DeferredDiscoveryRESTMapper{%v}", err)
	}
	return fmt.Sprintf("DeferredDiscoveryRESTMapper{\n\t%v\n}", del)
}

// Make sure it satisfies the interface
var _ meta.ResettableRESTMapper = &DeferredDiscoveryRESTMapper{}
