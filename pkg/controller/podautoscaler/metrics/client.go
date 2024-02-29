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

package metrics

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	autoscaling "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	customapi "k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	resourceclient "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	customclient "k8s.io/metrics/pkg/client/custom_metrics"
	externalclient "k8s.io/metrics/pkg/client/external_metrics"
)

const (
	metricServerDefaultMetricWindow = time.Minute
)

func NewRESTMetricsClient(resourceClient resourceclient.PodMetricsesGetter, customClient customclient.CustomMetricsClient, externalClient externalclient.ExternalMetricsClient) MetricsClient {
	return &restMetricsClient{
		&resourceMetricsClient{resourceClient},
		&customMetricsClient{customClient},
		&externalMetricsClient{externalClient},
	}
}

// restMetricsClient is a client which supports fetching
// metrics from both the resource metrics API and the
// custom metrics API.
type restMetricsClient struct {
	*resourceMetricsClient
	*customMetricsClient
	*externalMetricsClient
}

// resourceMetricsClient implements the resource-metrics-related parts of MetricsClient,
// using data from the resource metrics API.
type resourceMetricsClient struct {
	client resourceclient.PodMetricsesGetter
}

// GetResourceMetric gets the given resource metric (and an associated oldest timestamp)
// for all pods matching the specified selector in the given namespace
// 根据namespace和selector list所有podMetrics资源
// 如果container不为空，则从所有metrics.Items里（metricsapi.podMetrics的Containers里）找到resource类型的metric，保存到PodMetricsInfo
// 如果container为空，则遍历rawMetrics（[]metricsapi.podMetrics）里的所有Containers的resource之和，保存在PodMetricsInfo（pod名称对应的PodMetric）
// 返回PodMetricsInfo和第一个metric的Timestamp
func (c *resourceMetricsClient) GetResourceMetric(ctx context.Context, resource v1.ResourceName, namespace string, selector labels.Selector, container string) (PodMetricsInfo, time.Time, error) {
	// 访问/apis/metrics.k8s.io/v1beta1/namespaces/{namespace}/pods
	// 或namespace为空/apis/metrics.k8s.io/v1beta1/pods 
	metrics, err := c.client.PodMetricses(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("unable to fetch metrics from resource metrics API: %v", err)
	}

	if len(metrics.Items) == 0 {
		return nil, time.Time{}, fmt.Errorf("no metrics returned from resource metrics API")
	}
	var res PodMetricsInfo
	if container != "" {
		// 从所有metrics.Items里（metricsapi.podMetrics的Containers里）找到resource类型的metric，保存到PodMetricsInfo
		res, err = getContainerMetrics(metrics.Items, resource, container)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("failed to get container metrics: %v", err)
		}
	} else {
		// 遍历rawMetrics（[]metricsapi.podMetrics）里的所有Containers的resource之和，保存在PodMetricsInfo（pod名称对应的PodMetric）
		res = getPodMetrics(metrics.Items, resource)
	}
	timestamp := metrics.Items[0].Timestamp.Time
	return res, timestamp, nil
}

// 从所有rawMetrics（[]metricsapi.podMetrics）里的Containers里找到resource类型的metric，保存到PodMetricsInfo
func getContainerMetrics(rawMetrics []metricsapi.PodMetrics, resource v1.ResourceName, container string) (PodMetricsInfo, error) {
	res := make(PodMetricsInfo, len(rawMetrics))
	for _, m := range rawMetrics {
		containerFound := false
		for _, c := range m.Containers {
			if c.Name == container {
				containerFound = true
				if val, resFound := c.Usage[resource]; resFound {
					res[m.Name] = PodMetric{
						Timestamp: m.Timestamp.Time,
						Window:    m.Window.Duration,
						Value:     val.MilliValue(),
					}
				}
				break
			}
		}
		if !containerFound {
			return nil, fmt.Errorf("container %s not present in metrics for pod %s/%s", container, m.Namespace, m.Name)
		}
	}
	return res, nil
}

// 遍历rawMetrics（[]metricsapi.podMetrics）里的所有Containers的resource之和，保存在PodMetricsInfo（pod名称对应的PodMetric）
func getPodMetrics(rawMetrics []metricsapi.PodMetrics, resource v1.ResourceName) PodMetricsInfo {
	res := make(PodMetricsInfo, len(rawMetrics))
	for _, m := range rawMetrics {
		podSum := int64(0)
		missing := len(m.Containers) == 0
		for _, c := range m.Containers {
			resValue, found := c.Usage[resource]
			if !found {
				missing = true
				klog.V(2).Infof("missing resource metric %v for %s/%s", resource, m.Namespace, m.Name)
				break
			}
			podSum += resValue.MilliValue()
		}
		if !missing {
			res[m.Name] = PodMetric{
				Timestamp: m.Timestamp.Time,
				Window:    m.Window.Duration,
				Value:     podSum,
			}
		}
	}
	return res
}

// customMetricsClient implements the custom-metrics-related parts of MetricsClient,
// using data from the custom metrics API.
type customMetricsClient struct {
	client customclient.CustomMetricsClient
}

// GetRawMetric gets the given metric (and an associated oldest timestamp)
// for all pods matching the specified selector in the given namespace
// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/pod/{name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList转成PodMetricsInfo，第一个metric的timestamp
func (c *customMetricsClient) GetRawMetric(metricName string, namespace string, selector labels.Selector, metricSelector labels.Selector) (PodMetricsInfo, time.Time, error) {
	// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/pod/*/{metricName}?labelSelector={selector}&metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList
	metrics, err := c.client.NamespacedMetrics(namespace).GetForObjects(schema.GroupKind{Kind: "Pod"}, selector, metricName, metricSelector)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("unable to fetch metrics from custom metrics API: %v", err)
	}

	if len(metrics.Items) == 0 {
		return nil, time.Time{}, fmt.Errorf("no metrics returned from custom metrics API")
	}

	res := make(PodMetricsInfo, len(metrics.Items))
	for _, m := range metrics.Items {
		// 默认为1分钟
		window := metricServerDefaultMetricWindow
		// metric有WindowSeconds
		if m.WindowSeconds != nil {
			window = time.Duration(*m.WindowSeconds) * time.Second
		}
		res[m.DescribedObject.Name] = PodMetric{
			Timestamp: m.Timestamp.Time,
			Window:    window,
			Value:     int64(m.Value.MilliValue()),
		}

		m.Value.MilliValue()
	}

	timestamp := metrics.Items[0].Timestamp.Time

	return res, timestamp, nil
}

// GetObjectMetric gets the given metric (and an associated timestamp) for the given
// object in the given namespace
// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
func (c *customMetricsClient) GetObjectMetric(metricName string, namespace string, objectRef *autoscaling.CrossVersionObjectReference, metricSelector labels.Selector) (int64, time.Time, error) {
	// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
	gvk := schema.FromAPIVersionAndKind(objectRef.APIVersion, objectRef.Kind)
	var metricValue *customapi.MetricValue
	var err error
	// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}
	if gvk.Kind == "Namespace" && gvk.Group == "" {
		// handle namespace separately
		// NB: we ignore namespace name here, since CrossVersionObjectReference isn't
		// supposed to allow you to escape your namespace
		// 如果groupKind为{Kind: "Namespace", Group: ""}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
		metricValue, err = c.client.RootScopedMetrics().GetForObject(gvk.GroupKind(), namespace, metricName, metricSelector)
	} else {
		// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{gvk.GroupKind()对应resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
		metricValue, err = c.client.NamespacedMetrics(namespace).GetForObject(gvk.GroupKind(), objectRef.Name, metricName, metricSelector)
	}

	if err != nil {
		return 0, time.Time{}, fmt.Errorf("unable to fetch metrics from custom metrics API: %v", err)
	}

	return metricValue.Value.MilliValue(), metricValue.Timestamp.Time, nil
}

// externalMetricsClient implements the external metrics related parts of MetricsClient,
// using data from the external metrics API.
type externalMetricsClient struct {
	client externalclient.ExternalMetricsClient
}

// GetExternalMetric gets all the values of a given external metric
// that match the specified selector.
// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{namespace}/{metricName}?labelSelector={selector}"
// 返回所有metrics值列表和第一个metric的timestamp
func (c *externalMetricsClient) GetExternalMetric(metricName, namespace string, selector labels.Selector) ([]int64, time.Time, error) {
	// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{namespace}/{metricName}?labelSelector={selector}"
	metrics, err := c.client.NamespacedMetrics(namespace).List(metricName, selector)
	if err != nil {
		return []int64{}, time.Time{}, fmt.Errorf("unable to fetch metrics from external metrics API: %v", err)
	}

	if len(metrics.Items) == 0 {
		return nil, time.Time{}, fmt.Errorf("no metrics returned from external metrics API")
	}

	res := make([]int64, 0)
	for _, m := range metrics.Items {
		res = append(res, m.Value.MilliValue())
	}
	timestamp := metrics.Items[0].Timestamp.Time
	return res, timestamp, nil
}
