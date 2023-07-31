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

package scale

import (
	"fmt"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	scalescheme "k8s.io/client-go/scale/scheme"
	scaleappsint "k8s.io/client-go/scale/scheme/appsint"
	scaleappsv1beta1 "k8s.io/client-go/scale/scheme/appsv1beta1"
	scaleappsv1beta2 "k8s.io/client-go/scale/scheme/appsv1beta2"
	scaleautoscaling "k8s.io/client-go/scale/scheme/autoscalingv1"
	scaleextint "k8s.io/client-go/scale/scheme/extensionsint"
	scaleext "k8s.io/client-go/scale/scheme/extensionsv1beta1"
)

// PreferredResourceMapper determines the preferred version of a resource to scale
type PreferredResourceMapper interface {
	// ResourceFor takes a partial resource and returns the preferred resource.
	ResourceFor(resource schema.GroupVersionResource) (preferredResource schema.GroupVersionResource, err error)
}

// Ensure a RESTMapper satisfies the PreferredResourceMapper interface
var _ PreferredResourceMapper = meta.RESTMapper(nil)

// ScaleKindResolver knows about the relationship between
// resources and the GroupVersionKind of their scale subresources.
type ScaleKindResolver interface {
	// ScaleForResource returns the GroupVersionKind of the
	// scale subresource for the given GroupVersionResource.
	ScaleForResource(resource schema.GroupVersionResource) (scaleVersion schema.GroupVersionKind, err error)
}

// discoveryScaleResolver is a ScaleKindResolver that uses
// a DiscoveryInterface to associate resources with their
// scale-kinds
type discoveryScaleResolver struct {
	discoveryClient discovery.ServerResourcesInterface
}

// 请求"/api/v1"或"/apis/{group}/{Version}" 返回metav1.APIResourceList
// 根据这个list遍历所有的resource，resource.Name是带有"/"，且按"/"进行分割，前半部分为inputRes.Resource，后半部分为"scale"
// 如果resource.Group和resource.Version都不为空，则groupVersion使用resource的GroupVersionKind，否则使用inputRes.GroupVersion和resource的Kind
func (r *discoveryScaleResolver) ScaleForResource(inputRes schema.GroupVersionResource) (scaleVersion schema.GroupVersionKind, err error) {
	// 请求"/api/v1"或"/apis/{group}/{Version}" 返回metav1.APIResourceList
	groupVerResources, err := r.discoveryClient.ServerResourcesForGroupVersion(inputRes.GroupVersion().String())
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("unable to fetch discovery information for %s: %v", inputRes.String(), err)
	}

	// 遍历所有resource
	for _, resource := range groupVerResources.APIResources {
		// 对resource名字，按"/"进行分割
		resourceParts := strings.SplitN(resource.Name, "/", 2)
		// 不为2个分段，或第一部分不为inputRes.Resource，或第二部分不为"scale"，则跳过
		if len(resourceParts) != 2 || resourceParts[0] != inputRes.Resource || resourceParts[1] != "scale" {
			// skip non-scale resources, or scales for resources that we're not looking for
			continue
		}

		scaleGV := inputRes.GroupVersion()
		// 如果resource.Group和resource.Version都不为空，则groupVersion使用resource的resource.Group和resource.Version
		if resource.Group != "" && resource.Version != "" {
			scaleGV = schema.GroupVersion{
				Group:   resource.Group,
				Version: resource.Version,
			}
		}

		return scaleGV.WithKind(resource.Kind), nil
	}

	return schema.GroupVersionKind{}, fmt.Errorf("could not find scale subresource for %s in discovery information", inputRes.String())
}

// cachedScaleKindResolver is a ScaleKindResolver that caches results
// from another ScaleKindResolver, re-fetching on cache misses.
type cachedScaleKindResolver struct {
	base ScaleKindResolver

	cache map[schema.GroupVersionResource]schema.GroupVersionKind
	mu    sync.RWMutex
}

// 查找resource schema.GroupVersionResource对应的schema.GroupVersionKind
// 先从缓存中查找，如果找到直接返回
// 否则
// 请求"/api/v1"或"/apis/{group}/{Version}" 返回metav1.APIResourceList
// 根据这个list遍历所有的resource，resource.Name是带有"/"，且按"/"进行分割，前半部分为inputRes.Resource，后半部分为"scale"
// 如果resource.Group和resource.Version都不为空，则groupVersion使用resource的GroupVersionKind，否则使用inputRes.GroupVersion和resource的Kind
// 保存这个对应关系到缓存中
func (r *cachedScaleKindResolver) ScaleForResource(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	r.mu.RLock()
	// 从缓存中查找，如果找到直接返回
	gvk, isCached := r.cache[resource]
	r.mu.RUnlock()
	if isCached {
		return gvk, nil
	}

	// we could have multiple fetches of the same resources, but that's probably
	// better than limiting to only one reader at once (mu.Mutex),
	// or blocking checks for other resources while we fetch
	// (mu.Lock before fetch).
	// 请求"/api/v1"或"/apis/{group}/{Version}" 返回metav1.APIResourceList
	// 根据这个list遍历所有的resource，resource.Name是带有"/"，且按"/"进行分割，前半部分为inputRes.Resource，后半部分为"scale"
	// 如果resource.Group和resource.Version都不为空，则groupVersion使用resource的GroupVersionKind，否则使用inputRes.GroupVersion和resource的Kind
	gvk, err := r.base.ScaleForResource(resource)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[resource] = gvk

	return gvk, nil
}

// NewDiscoveryScaleKindResolver creates a new ScaleKindResolver which uses information from the given
// disovery client to resolve the correct Scale GroupVersionKind for different resources.
func NewDiscoveryScaleKindResolver(client discovery.ServerResourcesInterface) ScaleKindResolver {
	base := &discoveryScaleResolver{
		discoveryClient: client,
	}

	return &cachedScaleKindResolver{
		base:  base,
		cache: make(map[schema.GroupVersionResource]schema.GroupVersionKind),
	}
}

// ScaleConverter knows how to convert between external scale versions.
type ScaleConverter struct {
	scheme            *runtime.Scheme
	codecs            serializer.CodecFactory
	internalVersioner runtime.GroupVersioner
}

// NewScaleConverter creates a new ScaleConverter for converting between
// Scales in autoscaling/v1 and extensions/v1beta1.
func NewScaleConverter() *ScaleConverter {
	scheme := runtime.NewScheme()
	utilruntime.Must(scaleautoscaling.AddToScheme(scheme))
	utilruntime.Must(scalescheme.AddToScheme(scheme))
	utilruntime.Must(scaleext.AddToScheme(scheme))
	utilruntime.Must(scaleextint.AddToScheme(scheme))
	utilruntime.Must(scaleappsint.AddToScheme(scheme))
	utilruntime.Must(scaleappsv1beta1.AddToScheme(scheme))
	utilruntime.Must(scaleappsv1beta2.AddToScheme(scheme))

	return &ScaleConverter{
		scheme: scheme,
		codecs: serializer.NewCodecFactory(scheme),
		internalVersioner: runtime.NewMultiGroupVersioner(
			// {Group: "autoscaling", Version: "__internal"}
			scalescheme.SchemeGroupVersion,
			schema.GroupKind{Group: scaleext.GroupName, Kind: "Scale"},
			schema.GroupKind{Group: scaleautoscaling.GroupName, Kind: "Scale"},
			schema.GroupKind{Group: scaleappsv1beta1.GroupName, Kind: "Scale"},
			schema.GroupKind{Group: scaleappsv1beta2.GroupName, Kind: "Scale"},
		),
	}
}

// Scheme returns the scheme used by this scale converter.
func (c *ScaleConverter) Scheme() *runtime.Scheme {
	return c.scheme
}

func (c *ScaleConverter) Codecs() serializer.CodecFactory {
	return c.codecs
}

func (c *ScaleConverter) ScaleVersions() []schema.GroupVersion {
	return []schema.GroupVersion{
		// {Group: "autoscaling", Version: "v1"}
		scaleautoscaling.SchemeGroupVersion,
		// {Group: "autoscaling", Version: "__internal"}
		scalescheme.SchemeGroupVersion,
		// {Group: "extensions", Version: "v1beta1"}
		scaleext.SchemeGroupVersion,
		// {Group: "extensions", Version: "__internal"}
		scaleextint.SchemeGroupVersion,
		// {Group: "apps", Version: "__internal"}
		scaleappsint.SchemeGroupVersion,
		// {Group: "apps", Version: "v1beta1"}
		scaleappsv1beta1.SchemeGroupVersion,
		// {Group: "apps", Version: "v1beta2"}
		scaleappsv1beta2.SchemeGroupVersion,
	}
}

// ConvertToVersion converts the given *external* input object to the given output *external* output group-version.
func (c *ScaleConverter) ConvertToVersion(in runtime.Object, outVersion schema.GroupVersion) (runtime.Object, error) {
	// 先转成内部的版本（优先顺序{Group: "autoscaling", Version: "__internal"}，{Group: "extensions", Kind: "Scale"}，{Group: "autoscaling", Kind: "Scale"}，{Group: "apps", Kind: "Scale"}）
	scaleInt, err := c.scheme.ConvertToVersion(in, c.internalVersioner)
	if err != nil {
		return nil, err
	}

	// 再将内部版本转成外部版本
	return c.scheme.ConvertToVersion(scaleInt, outVersion)
}
