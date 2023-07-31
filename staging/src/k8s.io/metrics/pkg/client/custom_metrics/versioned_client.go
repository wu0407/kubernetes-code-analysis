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

package custom_metrics

import (
	"context"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	cmint "k8s.io/metrics/pkg/apis/custom_metrics"
	"k8s.io/metrics/pkg/apis/custom_metrics/v1beta1"
	"k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
	"k8s.io/metrics/pkg/client/custom_metrics/scheme"
)

var versionConverter = NewMetricConverter()

type customMetricsClient struct {
	client  rest.Interface
	version schema.GroupVersion
	mapper  meta.RESTMapper
}

// NewForVersion returns a new CustomMetricsClient for a particular api version.
func NewForVersion(client rest.Interface, mapper meta.RESTMapper, version schema.GroupVersion) CustomMetricsClient {
	return &customMetricsClient{
		client:  client,
		version: version,
		mapper:  mapper,
	}
}

// NewForVersionForConfig returns a new CustomMetricsClient for a particular api version and base configuration.
func NewForVersionForConfig(c *rest.Config, mapper meta.RESTMapper, version schema.GroupVersion) (CustomMetricsClient, error) {
	configShallowCopy := *c
	if configShallowCopy.RateLimiter == nil && configShallowCopy.QPS > 0 {
		if configShallowCopy.Burst <= 0 {
			return nil, fmt.Errorf("burst is required to be greater than 0 when RateLimiter is not set and QPS is set to greater than 0")
		}
		configShallowCopy.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(configShallowCopy.QPS, configShallowCopy.Burst)
	}
	configShallowCopy.APIPath = "/apis"
	if configShallowCopy.UserAgent == "" {
		configShallowCopy.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	configShallowCopy.GroupVersion = &version
	configShallowCopy.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	client, err := rest.RESTClientFor(&configShallowCopy)
	if err != nil {
		return nil, err
	}

	return NewForVersion(client, mapper, version), nil
}

func (c *customMetricsClient) RootScopedMetrics() MetricsInterface {
	return &rootScopedMetrics{c}
}

func (c *customMetricsClient) NamespacedMetrics(namespace string) MetricsInterface {
	return &namespacedMetrics{
		client:    c,
		namespace: namespace,
	}
}

// qualResourceForKind returns the string format of a qualified group resource for the specified GroupKind
// 从c.mapper（meta.RESTMapper）获得groupKind对应的*meta.RESTMapping，在从*meta.RESTMapping中获得GroupResource，然后转成string（{Resource} + "." + {Group}或{Resource}）
func (c *customMetricsClient) qualResourceForKind(groupKind schema.GroupKind) (string, error) {
	mapping, err := c.mapper.RESTMapping(groupKind)
	if err != nil {
		return "", fmt.Errorf("unable to map kind %s to resource: %v", groupKind.String(), err)
	}

	gr := mapping.Resource.GroupResource()
	return gr.String(), nil
}

type rootScopedMetrics struct {
	client *customMetricsClient
}

// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
func (m *rootScopedMetrics) getForNamespace(namespace string, metricName string, metricSelector labels.Selector) (*v1beta2.MetricValue, error) {
	// 先将*cmint.MetricListOptions转换为"__internal"内部版本，然后内部版本转成m.client.version版本
	params, err := versionConverter.ConvertListOptionsToVersion(&cmint.MetricListOptions{
		MetricLabelSelector: metricSelector.String(),
	}, m.client.version)
	if err != nil {
		return nil, err
	}

	result := m.client.client.Get().
		Resource("metrics").
		Namespace(namespace).
		Name(metricName).
		VersionedParams(params, scheme.ParameterCodec).
		Do(context.TODO())

	// 将rest.Result转成gv（{Group: "custom.metrics.k8s.io", Version: "v1beta2"}）版本的runtime.Object
	metricObj, err := versionConverter.ConvertResultToVersion(result, v1beta2.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}

	var res *v1beta2.MetricValueList
	var ok bool
	if res, ok = metricObj.(*v1beta2.MetricValueList); !ok {
		return nil, fmt.Errorf("the custom metrics API server didn't return MetricValueList, the type is %v", reflect.TypeOf(metricObj))
	}
	if len(res.Items) != 1 {
		return nil, fmt.Errorf("the custom metrics API server returned %v results when we asked for exactly one", len(res.Items))
	}

	return &res.Items[0], nil
}

// 如果groupKind为{Kind: "Namespace", Group: ""}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{name}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
// 否则，访问"/apis/custom.metrics.k8s.io/v1beta2/{resource}/{name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
func (m *rootScopedMetrics) GetForObject(groupKind schema.GroupKind, name string, metricName string, metricSelector labels.Selector) (*v1beta2.MetricValue, error) {
	// handle namespace separately
	if groupKind.Kind == "Namespace" && groupKind.Group == "" {
		// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{name}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
		return m.getForNamespace(name, metricName, metricSelector)
	}

	resourceName, err := m.client.qualResourceForKind(groupKind)
	if err != nil {
		return nil, err
	}

	params, err := versionConverter.ConvertListOptionsToVersion(&cmint.MetricListOptions{
		MetricLabelSelector: metricSelector.String(),
	}, m.client.version)
	if err != nil {
		return nil, err
	}

	result := m.client.client.Get().
		Resource(resourceName).
		Name(name).
		SubResource(metricName).
		VersionedParams(params, scheme.ParameterCodec).
		Do(context.TODO())

	metricObj, err := versionConverter.ConvertResultToVersion(result, v1beta2.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}

	var res *v1beta2.MetricValueList
	var ok bool
	if res, ok = metricObj.(*v1beta2.MetricValueList); !ok {
		return nil, fmt.Errorf("the custom metrics API server didn't return MetricValueList, the type is %v", reflect.TypeOf(metricObj))
	}
	if len(res.Items) != 1 {
		return nil, fmt.Errorf("the custom metrics API server returned %v results when we asked for exactly one", len(res.Items))
	}

	return &res.Items[0], nil
}

// 访问"/apis/custom.metrics.k8s.io/v1beta2/{resource}/*/{metricName}?labelSelector={selector}&metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList
func (m *rootScopedMetrics) GetForObjects(groupKind schema.GroupKind, selector labels.Selector, metricName string, metricSelector labels.Selector) (*v1beta2.MetricValueList, error) {
	// we can't wildcard-fetch for namespaces
	if groupKind.Kind == "Namespace" && groupKind.Group == "" {
		return nil, fmt.Errorf("cannot fetch metrics for multiple namespaces at once")
	}

	// 从c.mapper（meta.RESTMapper）获得groupKind对应的*meta.RESTMapping，在从*meta.RESTMapping中获得GroupResource，然后转成string
	resourceName, err := m.client.qualResourceForKind(groupKind)
	if err != nil {
		return nil, err
	}

	// 先将*cmint.MetricListOptions转换为"__internal"内部版本，然后内部版本转成m.client.version版本
	params, err := versionConverter.ConvertListOptionsToVersion(&cmint.MetricListOptions{
		LabelSelector:       selector.String(),
		MetricLabelSelector: metricSelector.String(),
	}, m.client.version)
	if err != nil {
		return nil, err
	}

	result := m.client.client.Get().
		Resource(resourceName).
		Name(v1beta1.AllObjects).
		SubResource(metricName).
		VersionedParams(params, scheme.ParameterCodec).
		Do(context.TODO())

	// 将rest.Result转成gv（{Group: "custom.metrics.k8s.io", Version: "v1beta2"}）版本的runtime.Object
	metricObj, err := versionConverter.ConvertResultToVersion(result, v1beta2.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}

	var res *v1beta2.MetricValueList
	var ok bool
	if res, ok = metricObj.(*v1beta2.MetricValueList); !ok {
		return nil, fmt.Errorf("the custom metrics API server didn't return MetricValueList, the type is %v", reflect.TypeOf(metricObj))
	}
	return res, nil
}

type namespacedMetrics struct {
	client    *customMetricsClient
	namespace string
}

// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/{resource}/{name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
func (m *namespacedMetrics) GetForObject(groupKind schema.GroupKind, name string, metricName string, metricSelector labels.Selector) (*v1beta2.MetricValue, error) {
	// 从c.mapper（meta.RESTMapper）获得groupKind对应的*meta.RESTMapping，在从*meta.RESTMapping中获得GroupResource，然后转成string
	resourceName, err := m.client.qualResourceForKind(groupKind)
	if err != nil {
		return nil, err
	}

	// 先将*cmint.MetricListOptions转换为"__internal"内部版本，然后内部版本转成m.client.version版本
	params, err := versionConverter.ConvertListOptionsToVersion(&cmint.MetricListOptions{
		MetricLabelSelector: metricSelector.String(),
	}, m.client.version)
	if err != nil {
		return nil, err
	}

	result := m.client.client.Get().
		Resource(resourceName).
		Namespace(m.namespace).
		Name(name).
		SubResource(metricName).
		VersionedParams(params, scheme.ParameterCodec).
		Do(context.TODO())

	// 将rest.Result转成gv（{Group: "custom.metrics.k8s.io", Version: "v1beta2"}）版本的runtime.Object
	metricObj, err := versionConverter.ConvertResultToVersion(result, v1beta2.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}

	var res *v1beta2.MetricValueList
	var ok bool
	if res, ok = metricObj.(*v1beta2.MetricValueList); !ok {
		return nil, fmt.Errorf("the custom metrics API server didn't return MetricValueList, the type is %v", reflect.TypeOf(metricObj))
	}
	if len(res.Items) != 1 {
		return nil, fmt.Errorf("the custom metrics API server returned %v results when we asked for exactly one", len(res.Items))
	}

	return &res.Items[0], nil
}

// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/{resource}/*/{metricName}?labelSelector={selector}&metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList
func (m *namespacedMetrics) GetForObjects(groupKind schema.GroupKind, selector labels.Selector, metricName string, metricSelector labels.Selector) (*v1beta2.MetricValueList, error) {
	// 从c.mapper（meta.RESTMapper）获得groupKind对应的*meta.RESTMapping，在从*meta.RESTMapping中获得GroupResource，然后转成string
	resourceName, err := m.client.qualResourceForKind(groupKind)
	if err != nil {
		return nil, err
	}

	// 先将*cmint.MetricListOptions转换为"__internal"内部版本，然后内部版本转成m.client.version版本
	params, err := versionConverter.ConvertListOptionsToVersion(&cmint.MetricListOptions{
		LabelSelector:       selector.String(),
		MetricLabelSelector: metricSelector.String(),
	}, m.client.version)
	if err != nil {
		return nil, err
	}

	result := m.client.client.Get().
		Resource(resourceName).
		Namespace(m.namespace).
		Name(v1beta1.AllObjects).
		SubResource(metricName).
		VersionedParams(params, scheme.ParameterCodec).
		Do(context.TODO())

	// 将rest.Result转成gv（{Group: "custom.metrics.k8s.io", Version: "v1beta2"}）版本的runtime.Object
	metricObj, err := versionConverter.ConvertResultToVersion(result, v1beta2.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}

	var res *v1beta2.MetricValueList
	var ok bool
	if res, ok = metricObj.(*v1beta2.MetricValueList); !ok {
		return nil, fmt.Errorf("the custom metrics API server didn't return MetricValueList, the type is %v", reflect.TypeOf(metricObj))
	}
	return res, nil
}
