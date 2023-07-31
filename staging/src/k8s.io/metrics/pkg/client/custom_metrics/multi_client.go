/*
Copyright 2018 The Kubernetes Authors.

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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"k8s.io/metrics/pkg/apis/custom_metrics/v1beta2"
)

// AvailableAPIsGetter knows how to fetch and cache the preferred custom metrics API version,
// and invalidate that cache when asked.
type AvailableAPIsGetter interface {
	PreferredVersion() (schema.GroupVersion, error)
	Invalidate()
}

// PeriodicallyInvalidate periodically invalidates the preferred version cache until
// told to stop.
func PeriodicallyInvalidate(cache AvailableAPIsGetter, interval time.Duration, stopCh <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cache.Invalidate()
		case <-stopCh:
			return
		}
	}
}

// NewForConfig creates a new custom metrics client which delegates to a client which
// uses the preferred api version.
func NewForConfig(baseConfig *rest.Config, mapper meta.RESTMapper, availableAPIs AvailableAPIsGetter) CustomMetricsClient {
	return &multiClient{
		clients:       make(map[schema.GroupVersion]CustomMetricsClient),
		availableAPIs: availableAPIs,

		newClient: func(ver schema.GroupVersion) (CustomMetricsClient, error) {
			return NewForVersionForConfig(rest.CopyConfig(baseConfig), mapper, ver)
		},
	}
}

// multiClient is a CustomMetricsClient that can work with *any* metrics API version.
type multiClient struct {
	newClient     func(schema.GroupVersion) (CustomMetricsClient, error)
	clients       map[schema.GroupVersion]CustomMetricsClient
	availableAPIs AvailableAPIsGetter
	mu            sync.RWMutex
}

// getPreferredClient returns a custom metrics client of the preferred api version.
func (c *multiClient) getPreferredClient() (CustomMetricsClient, error) {
	// 实现在staging\src\k8s.io\metrics\pkg\client\custom_metrics\discovery.go里（*apiVersionsFromDiscovery）
	// 使用discoveryClient从apiserver获取"custom.metrics.k8s.io"对应的metav1.APIGroup信息
	// 首先判断，apiGroup.PreferredVersion.GroupVersion在现在支持的"custom.metrics.k8s.io"版本中，存在直接返回
	// 否则从apiGroup.Versions中寻找第一个在现在支持的"custom.metrics.k8s.io"版本中的GroupVersion
	// 这里是{Group: "custom.metrics.k8s.io", Version: "v1beta2"}
	pref, err := c.availableAPIs.PreferredVersion()
	if err != nil {
		return nil, err
	}

	// 从缓存中获取prefer groupVersion对应client
	c.mu.RLock()
	client, present := c.clients[pref]
	c.mu.RUnlock()
	// 缓存中存在，则返回
	if present {
		return client, nil
	}

	// 缓存中不存在，则进行生成client
	c.mu.Lock()
	defer c.mu.Unlock()
	client, err = c.newClient(pref)
	if err != nil {
		return nil, err
	}
	// 保存client到缓存中
	c.clients[pref] = client

	return client, nil
}

func (c *multiClient) RootScopedMetrics() MetricsInterface {
	return &multiClientInterface{clients: c}
}

func (c *multiClient) NamespacedMetrics(namespace string) MetricsInterface {
	return &multiClientInterface{
		clients:   c,
		namespace: &namespace,
	}
}

type multiClientInterface struct {
	clients   *multiClient
	namespace *string
}

func (m *multiClientInterface) GetForObject(groupKind schema.GroupKind, name string, metricName string, metricSelector labels.Selector) (*v1beta2.MetricValue, error) {
	// 先从缓存中获得这个prefer groupVersion对应client，如果获取不到则生成一个client（在staging\src\k8s.io\metrics\pkg\client\custom_metrics\versioned_client.go里customMetricsClient）
	client, err := m.clients.getPreferredClient()
	if err != nil {
		return nil, err
	}
	if m.namespace == nil {
		// 如果groupKind为{Kind: "Namespace", Group: ""}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{name}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
		// 否则，访问"/apis/custom.metrics.k8s.io/v1beta2/{resource}/{name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
		return client.RootScopedMetrics().GetForObject(groupKind, name, metricName, metricSelector)
	} else {
		// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/{resource}/{name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]
		return client.NamespacedMetrics(*m.namespace).GetForObject(groupKind, name, metricName, metricSelector)
	}
}

func (m *multiClientInterface) GetForObjects(groupKind schema.GroupKind, selector labels.Selector, metricName string, metricSelector labels.Selector) (*v1beta2.MetricValueList, error) {
	client, err := m.clients.getPreferredClient()
	if err != nil {
		return nil, err
	}
	if m.namespace == nil {
		// 访问"/apis/custom.metrics.k8s.io/v1beta2/{resource}/*/{metricName}?labelSelector={selector}&metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList
		return client.RootScopedMetrics().GetForObjects(groupKind, selector, metricName, metricSelector)
	} else {
		// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/{resource}/*/{metricName}?labelSelector={selector}&metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList
		return client.NamespacedMetrics(*m.namespace).GetForObjects(groupKind, selector, metricName, metricSelector)
	}
}
