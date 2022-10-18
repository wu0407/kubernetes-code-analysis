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

package collectors

import (
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/klog"
	summary "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
)

var (
	nodeCPUUsageDesc = metrics.NewDesc("node_cpu_usage_seconds_total",
		"Cumulative cpu time consumed by the node in core-seconds",
		nil,
		nil,
		metrics.ALPHA,
		"")

	nodeMemoryUsageDesc = metrics.NewDesc("node_memory_working_set_bytes",
		"Current working set of the node in bytes",
		nil,
		nil,
		metrics.ALPHA,
		"")

	containerCPUUsageDesc = metrics.NewDesc("container_cpu_usage_seconds_total",
		"Cumulative cpu time consumed by the container in core-seconds",
		[]string{"container", "pod", "namespace"},
		nil,
		metrics.ALPHA,
		"")

	containerMemoryUsageDesc = metrics.NewDesc("container_memory_working_set_bytes",
		"Current working set of the container in bytes",
		[]string{"container", "pod", "namespace"},
		nil,
		metrics.ALPHA,
		"")

	resouceScrapeResultDesc = metrics.NewDesc("scrape_error",
		"1 if there was an error while getting container metrics, 0 otherwise",
		nil,
		nil,
		metrics.ALPHA,
		"")
)

// NewResourceMetricsCollector returns a metrics.StableCollector which exports resource metrics
func NewResourceMetricsCollector(provider stats.SummaryProvider) metrics.StableCollector {
	return &resourceMetricsCollector{
		provider: provider,
	}
}

type resourceMetricsCollector struct {
	metrics.BaseStableCollector

	provider stats.SummaryProvider
}

// Check if resourceMetricsCollector implements necessary interface
var _ metrics.StableCollector = &resourceMetricsCollector{}

// DescribeWithStability implements metrics.StableCollector
func (rc *resourceMetricsCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- nodeCPUUsageDesc
	ch <- nodeMemoryUsageDesc
	ch <- containerCPUUsageDesc
	ch <- containerMemoryUsageDesc
	ch <- resouceScrapeResultDesc
}

// CollectWithStability implements metrics.StableCollector
// Since new containers are frequently created and removed, using the Gauge would
// leak metric collectors for containers or pods that no longer exist.  Instead, implement
// custom collector in a way that only collects metrics for active containers.
func (rc *resourceMetricsCollector) CollectWithStability(ch chan<- metrics.Metric) {
	var errorCount float64
	defer func() {
		// 如果metrics不是隐藏的，则返回prometheus.constMetric，否则返回nil
		ch <- metrics.NewLazyConstMetric(resouceScrapeResultDesc, metrics.GaugeValue, errorCount)
	}()
	// 返回node的cpu和memory监控状态，nodeConfig（ContainerManager）里的定义的cgroup类别的cpu和memory监控状态和所有pod的cpu和memory监控状态和所有pod的container的cpu、内存监控状态
	statsSummary, err := rc.provider.GetCPUAndMemoryStats()
	if err != nil {
		errorCount = 1
		klog.Warningf("Error getting summary for resourceMetric prometheus endpoint: %v", err)
		return
	}

	// 根据s.CPU.UsageCoreNanoSeconds和s.CPU.Time.Time生成带有时间戳的metrics，将metrics发送到ch
	rc.collectNodeCPUMetrics(ch, statsSummary.Node)
	// 根据s.Memory.WorkingSetBytes和s.Memory.Time.Time生成带有时间戳的metrics，将metrics发送到ch
	rc.collectNodeMemoryMetrics(ch, statsSummary.Node)

	// 遍历所有pod里的container
	for _, pod := range statsSummary.Pods {
		for _, container := range pod.Containers {
			// 根据s.CPU.Time.Time和s.CPU.UsageCoreNanoSeconds生成带有时间戳的metrics，将metrics发送到ch~
			rc.collectContainerCPUMetrics(ch, pod, container)
			// 根据s.Memory.Time.Time和s.Memory.WorkingSetBytes生成带有时间戳的metrics，将metrics发送到ch
			rc.collectContainerMemoryMetrics(ch, pod, container)
		}
	}
}

// 根据s.CPU.UsageCoreNanoSeconds和s.CPU.Time.Time生成带有时间戳的metrics，将metrics发送到ch
func (rc *resourceMetricsCollector) collectNodeCPUMetrics(ch chan<- metrics.Metric, s summary.NodeStats) {
	if s.CPU == nil {
		return
	}

	ch <- metrics.NewLazyMetricWithTimestamp(s.CPU.Time.Time,
		// 如果metrics不是隐藏的，则返回prometheus.constMetric，否则返回nil
		metrics.NewLazyConstMetric(nodeCPUUsageDesc, metrics.CounterValue, float64(*s.CPU.UsageCoreNanoSeconds)/float64(time.Second)))
}

// 根据s.Memory.WorkingSetBytes和s.Memory.Time.Time生成带有时间戳的metrics，将metrics发送到ch
func (rc *resourceMetricsCollector) collectNodeMemoryMetrics(ch chan<- metrics.Metric, s summary.NodeStats) {
	if s.Memory == nil {
		return
	}

	ch <- metrics.NewLazyMetricWithTimestamp(s.Memory.Time.Time,
		metrics.NewLazyConstMetric(nodeMemoryUsageDesc, metrics.GaugeValue, float64(*s.Memory.WorkingSetBytes)))
}

// 根据s.CPU.Time.Time和s.CPU.UsageCoreNanoSeconds生成带有时间戳的metrics，将metrics发送到ch
func (rc *resourceMetricsCollector) collectContainerCPUMetrics(ch chan<- metrics.Metric, pod summary.PodStats, s summary.ContainerStats) {
	if s.CPU == nil {
		return
	}

	ch <- metrics.NewLazyMetricWithTimestamp(s.CPU.Time.Time,
		metrics.NewLazyConstMetric(containerCPUUsageDesc, metrics.CounterValue,
			float64(*s.CPU.UsageCoreNanoSeconds)/float64(time.Second), s.Name, pod.PodRef.Name, pod.PodRef.Namespace))
}

// 根据s.Memory.Time.Time和s.Memory.WorkingSetBytes生成带有时间戳的metrics，将metrics发送到ch
func (rc *resourceMetricsCollector) collectContainerMemoryMetrics(ch chan<- metrics.Metric, pod summary.PodStats, s summary.ContainerStats) {
	if s.Memory == nil {
		return
	}

	ch <- metrics.NewLazyMetricWithTimestamp(s.Memory.Time.Time,
		metrics.NewLazyConstMetric(containerMemoryUsageDesc, metrics.GaugeValue,
			float64(*s.Memory.WorkingSetBytes), s.Name, pod.PodRef.Name, pod.PodRef.Namespace))
}
