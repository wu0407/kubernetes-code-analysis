/*
Copyright 2019 The Kubernetes Authors.

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

package stats

import (
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/klog"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
)

// NodeResourceMetric describes a metric for the node
type NodeResourceMetric struct {
	Desc    *metrics.Desc
	ValueFn func(stats.NodeStats) (*float64, time.Time)
}

func (n *NodeResourceMetric) desc() *metrics.Desc {
	return n.Desc
}

// ContainerResourceMetric describes a metric for containers
type ContainerResourceMetric struct {
	Desc    *metrics.Desc
	ValueFn func(stats.ContainerStats) (*float64, time.Time)
}

func (n *ContainerResourceMetric) desc() *metrics.Desc {
	return n.Desc
}

// ResourceMetricsConfig specifies which metrics to collect and export
type ResourceMetricsConfig struct {
	NodeMetrics      []NodeResourceMetric
	ContainerMetrics []ContainerResourceMetric
}

// NewPrometheusResourceMetricCollector returns a metrics.StableCollector which exports resource metrics
func NewPrometheusResourceMetricCollector(provider SummaryProvider, config ResourceMetricsConfig) metrics.StableCollector {
	return &resourceMetricCollector{
		provider: provider,
		config:   config,
		// 返回metrics.Desc
		errors: metrics.NewDesc("scrape_error",
			"1 if there was an error while getting container metrics, 0 otherwise",
			nil,
			nil,
			metrics.ALPHA,
			"1.18.0"),
	}
}

type resourceMetricCollector struct {
	metrics.BaseStableCollector

	provider SummaryProvider
	config   ResourceMetricsConfig
	errors   *metrics.Desc
}

var _ metrics.StableCollector = &resourceMetricCollector{}

// DescribeWithStability implements metrics.StableCollector
// 发送rc.config.NodeMetrics和rc.config.ContainerMetrics的desc和rc.errors到ch
func (rc *resourceMetricCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- rc.errors

	for _, metric := range rc.config.NodeMetrics {
		ch <- metric.desc()
	}
	for _, metric := range rc.config.ContainerMetrics {
		ch <- metric.desc()
	}
}

// CollectWithStability implements metrics.StableCollector
// Since new containers are frequently created and removed, using the Gauge would
// leak metric collectors for containers or pods that no longer exist.  Instead, implement
// custom collector in a way that only collects metrics for active containers.
// 发送rc.config.NodeMetrics里的metrics和rc.config.ContainerMetrics且带了时间戳，到ch
func (rc *resourceMetricCollector) CollectWithStability(ch chan<- metrics.Metric) {
	var errorCount float64
	defer func() {
		// 如果metrics不是隐藏的，则返回prometheus.constMetric，否则返回nil
		// 发送"scrape_error"到ch
		ch <- metrics.NewLazyConstMetric(rc.errors, metrics.GaugeValue, errorCount)
	}()
	// 返回node的cpu和memory监控状态，nodeConfig（ContainerManager）里的定义的cgroup类别的cpu和memory监控状态和所有pod的cpu和memory监控状态和所有pod的container的cpu、内存监控状态
	summary, err := rc.provider.GetCPUAndMemoryStats()
	if err != nil {
		errorCount = 1
		klog.Warningf("Error getting summary for resourceMetric prometheus endpoint: %v", err)
		return
	}

	// 遍历每个NodeMetrics，执行ValueFn，获得node的cpu和memory数值
	// 发送带有时间戳的metrics到ch
	for _, metric := range rc.config.NodeMetrics {
		if value, timestamp := metric.ValueFn(summary.Node); value != nil {
			// 发送带有时间戳的metrics到ch
			ch <- metrics.NewLazyMetricWithTimestamp(timestamp,
				metrics.NewLazyConstMetric(metric.desc(), metrics.GaugeValue, *value))
		}
	}

	// 遍历每个pod下的container，发送带有时间戳的metrics到ch
	for _, pod := range summary.Pods {
		for _, container := range pod.Containers {
			for _, metric := range rc.config.ContainerMetrics {
				if value, timestamp := metric.ValueFn(container); value != nil {
					ch <- metrics.NewLazyMetricWithTimestamp(timestamp,
						metrics.NewLazyConstMetric(metric.desc(), metrics.GaugeValue, *value, container.Name, pod.PodRef.Name, pod.PodRef.Namespace))
				}
			}
		}
	}
}
