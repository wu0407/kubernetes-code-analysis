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
	"context"
	"fmt"

	autoscaling "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	restclient "k8s.io/client-go/rest"
)

var scaleConverter = NewScaleConverter()
var codecs = serializer.NewCodecFactory(scaleConverter.Scheme())
var parameterScheme = runtime.NewScheme()
var dynamicParameterCodec = runtime.NewParameterCodec(parameterScheme)

var versionV1 = schema.GroupVersion{Version: "v1"}

func init() {
	metav1.AddToGroupVersion(parameterScheme, versionV1)
}

// scaleClient is an implementation of ScalesGetter
// which makes use of a RESTMapper and a generic REST
// client to support an discoverable resource.
// It behaves somewhat similarly to the dynamic ClientPool,
// but is more specifically scoped to Scale.
type scaleClient struct {
	mapper PreferredResourceMapper

	apiPathResolverFunc dynamic.APIPathResolverFunc
	scaleKindResolver   ScaleKindResolver
	clientBase          restclient.Interface
}

// NewForConfig creates a new ScalesGetter which resolves kinds
// to resources using the given RESTMapper, and API paths using
// the given dynamic.APIPathResolverFunc.
func NewForConfig(cfg *restclient.Config, mapper PreferredResourceMapper, resolver dynamic.APIPathResolverFunc, scaleKindResolver ScaleKindResolver) (ScalesGetter, error) {
	// so that the RESTClientFor doesn't complain
	cfg.GroupVersion = &schema.GroupVersion{}

	cfg.NegotiatedSerializer = codecs.WithoutConversion()
	if len(cfg.UserAgent) == 0 {
		cfg.UserAgent = restclient.DefaultKubernetesUserAgent()
	}

	client, err := restclient.RESTClientFor(cfg)
	if err != nil {
		return nil, err
	}

	return New(client, mapper, resolver, scaleKindResolver), nil
}

// New creates a new ScalesGetter using the given client to make requests.
// The GroupVersion on the client is ignored.
func New(baseClient restclient.Interface, mapper PreferredResourceMapper, resolver dynamic.APIPathResolverFunc, scaleKindResolver ScaleKindResolver) ScalesGetter {
	return &scaleClient{
		mapper: mapper,

		apiPathResolverFunc: resolver,
		scaleKindResolver:   scaleKindResolver,
		clientBase:          baseClient,
	}
}

// apiPathFor returns the absolute api path for the given GroupVersion
// group为空，则返回"/api/{group}/{version}"
// 否则返回"/apis/{group}/{version}"
func (c *scaleClient) apiPathFor(groupVer schema.GroupVersion) string {
	// we need to set the API path based on GroupVersion (defaulting to the legacy path if none is set)
	// TODO: we "cheat" here since the API path really only depends on group ATM, but this should
	// *probably* take GroupVersionResource and not GroupVersionKind.
	// c.apiPathResolverFunc实现在LegacyAPIPathResolverFunc staging\src\k8s.io\client-go\dynamic\interface.go
	apiPath := c.apiPathResolverFunc(groupVer.WithKind(""))
	if apiPath == "" {
		apiPath = "/api"
	}

	// 返回"/api/{group}/{version}"或"/apis/{group}/{version}"
	return restclient.DefaultVersionedAPIPath(apiPath, groupVer)
}

// pathAndVersionFor returns the appropriate base path and the associated full GroupVersionResource
// for the given GroupResource
// 获取resource资源的对应的访问apiserver路径前缀和对应的schema.GroupVersionResource
func (c *scaleClient) pathAndVersionFor(resource schema.GroupResource) (string, schema.GroupVersionResource, error) {
	// c.mapper实现在staging\src\k8s.io\client-go\restmapper\discovery.go里的DeferredDiscoveryRESTMapper
	// 最终执行在DefaultRESTMapper staging\src\k8s.io\apimachinery\pkg\api\meta\restmapper.go
	//   首先对输入的input schema.GroupVersionResource进行规整化（确保input.Resource是小写，如果input.Version是"__internal"，则变成空""）
	//   然后从m.pluralToSingular查找，至少resource匹配的plural，越多匹配的放在前面
	//   然后对所有匹配的GroupVersionResource进行排序
	// 然后实现在MultiRESTMapper staging\src\k8s.io\apimachinery\pkg\api\meta\multirestmapper.go
	//   对MultiRESTMapper里所有RESTMapper执行ResourcesFor(resource)，对返回的结果进行去重。如果返回多个结果，则返回错误，否则返回这个结果
	//   目前的scale资源在group里只有一个version所以不会有多个（所以这里不会返回错误）。
	// 在上层的实现在PriorityRESTMapper staging\src\k8s.io\apimachinery\pkg\api\meta\priority.go
	// 最上层的实现在在staging\src\k8s.io\client-go\restmapper\discovery.go里的DeferredDiscoveryRESTMapper
	gvr, err := c.mapper.ResourceFor(resource.WithVersion(""))
	if err != nil {
		return "", gvr, fmt.Errorf("unable to get full preferred group-version-resource for %s: %v", resource.String(), err)
	}

	groupVer := gvr.GroupVersion()

	// group为空，则返回"/api/{group}/{version}"
	// 否则返回"/apis/{group}/{version}"
	return c.apiPathFor(groupVer), gvr, nil
}

// namespacedScaleClient is an ScaleInterface for fetching
// Scales in a given namespace.
type namespacedScaleClient struct {
	client    *scaleClient
	namespace string
}

// convertToScale converts the response body to autoscaling/v1.Scale
// 将result转成autoscaling/v1.Scale
func convertToScale(result *restclient.Result) (*autoscaling.Scale, error) {
	scaleBytes, err := result.Raw()
	if err != nil {
		return nil, err
	}
	decoder := scaleConverter.codecs.UniversalDecoder(scaleConverter.ScaleVersions()...)
	rawScaleObj, err := runtime.Decode(decoder, scaleBytes)
	if err != nil {
		return nil, err
	}

	// convert whatever this is to autoscaling/v1.Scale
	// 转成autoscaling/v1.Scale
	scaleObj, err := scaleConverter.ConvertToVersion(rawScaleObj, autoscaling.SchemeGroupVersion)
	if err != nil {
		return nil, fmt.Errorf("received an object from a /scale endpoint which was not convertible to autoscaling Scale: %v", err)
	}

	return scaleObj.(*autoscaling.Scale), nil
}

func (c *scaleClient) Scales(namespace string) ScaleInterface {
	return &namespacedScaleClient{
		client:    c,
		namespace: namespace,
	}
}

func (c *namespacedScaleClient) Get(ctx context.Context, resource schema.GroupResource, name string, opts metav1.GetOptions) (*autoscaling.Scale, error) {
	// Currently, a /scale endpoint can return different scale types.
	// Until we have support for the alternative API representations proposal,
	// we need to deal with accepting different API versions.
	// In practice, this is autoscaling/v1.Scale and extensions/v1beta1.Scale

	// 获取resource资源的对应的访问apiserver路径前缀和对应的schema.GroupVersionResource
	path, gvr, err := c.client.pathAndVersionFor(resource)
	if err != nil {
		return nil, fmt.Errorf("unable to get client for %s: %v", resource.String(), err)
	}

	result := c.client.clientBase.Get().
		AbsPath(path).
		NamespaceIfScoped(c.namespace, c.namespace != "").
		Resource(gvr.Resource).
		Name(name).
		SubResource("scale").
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Do(ctx)
	if err := result.Error(); err != nil {
		return nil, err
	}

	return convertToScale(&result)
}

// 比如传入的resource为{Group: "apps", Resource: "deployments"}
// 执行PUT "/apis/apps/v1/namespaces/{c.namespace}/deployments/{scale.Name}/scale?{opts}"
func (c *namespacedScaleClient) Update(ctx context.Context, resource schema.GroupResource, scale *autoscaling.Scale, opts metav1.UpdateOptions) (*autoscaling.Scale, error) {
	// 获取resource资源的对应的访问apiserver路径前缀和对应的schema.GroupVersionResource
	// 比如传入的resource为{Group: "apps", Resource: "deployments/scale"}，返回的"/apis/apps/v1",{Group: "apps", Resource: "deployments/scale", "Version": "v1"}
	path, gvr, err := c.client.pathAndVersionFor(resource)
	if err != nil {
		return nil, fmt.Errorf("unable to get client for %s: %v", resource.String(), err)
	}

	// Currently, a /scale endpoint can receive and return different scale types.
	// Until we have support for the alternative API representations proposal,
	// we need to deal with sending and accepting different API versions.

	// figure out what scale we actually need here
	// c.client.scaleKindResolver在staging\src\k8s.io\client-go\scale\util.go里的cachedScaleKindResolver
	// 查找resource schema.GroupVersionResource对应的schema.GroupVersionKind
	// 先从缓存中查找，如果找到直接返回
	// 否则
	// 请求"/api/v1"或"/apis/{group}/{Version}" 返回metav1.APIResourceList
	// 根据这个list遍历所有的resource，resource.Name是带有"/"，且按"/"进行分割，前半部分为inputRes.Resource，后半部分为"scale"
	// 如果resource.Group和resource.Version都不为空，则groupVersion使用resource的GroupVersionKind，否则使用gvr.GroupVersion和resource的Kind
	// 保存这个对应关系到缓存中
	// 比如传入的resource为{Group: "apps", Resource: "deployments"}，返回的groupVersionKind为{group":"autoscaling","version":"v1","kind":"Scale"}
	desiredGVK, err := c.client.scaleKindResolver.ScaleForResource(gvr)
	if err != nil {
		return nil, fmt.Errorf("could not find proper group-version for scale subresource of %s: %v", gvr.String(), err)
	}

	// convert this to whatever this endpoint wants
	// 转成desiredGVK.GroupVersion()的scale
	scaleUpdate, err := scaleConverter.ConvertToVersion(scale, desiredGVK.GroupVersion())
	if err != nil {
		return nil, fmt.Errorf("could not convert scale update to external Scale: %v", err)
	}
	encoder := scaleConverter.codecs.LegacyCodec(desiredGVK.GroupVersion())
	scaleUpdateBytes, err := runtime.Encode(encoder, scaleUpdate)
	if err != nil {
		return nil, fmt.Errorf("could not encode scale update to external Scale: %v", err)
	}

	result := c.client.clientBase.Put().
		AbsPath(path).
		NamespaceIfScoped(c.namespace, c.namespace != "").
		Resource(gvr.Resource).
		Name(scale.Name).
		SubResource("scale").
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Body(scaleUpdateBytes).
		Do(ctx)
	if err := result.Error(); err != nil {
		// propagate "raw" error from the API
		// this allows callers to interpret underlying Reason field
		// for example: errors.IsConflict(err)
		return nil, err
	}

	// 将result转成autoscaling/v1.Scale
	return convertToScale(&result)
}

func (c *namespacedScaleClient) Patch(ctx context.Context, gvr schema.GroupVersionResource, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*autoscaling.Scale, error) {
	groupVersion := gvr.GroupVersion()
	result := c.client.clientBase.Patch(pt).
		AbsPath(c.client.apiPathFor(groupVersion)).
		NamespaceIfScoped(c.namespace, c.namespace != "").
		Resource(gvr.Resource).
		Name(name).
		SubResource("scale").
		SpecificallyVersionedParams(&opts, dynamicParameterCodec, versionV1).
		Body(data).
		Do(ctx)
	if err := result.Error(); err != nil {
		return nil, err
	}

	return convertToScale(&result)
}
