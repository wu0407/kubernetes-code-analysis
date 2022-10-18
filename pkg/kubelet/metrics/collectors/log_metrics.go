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

package collectors

import (
	"k8s.io/component-base/metrics"
	"k8s.io/klog"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
)

var (
	descLogSize = metrics.NewDesc(
		"kubelet_container_log_filesystem_used_bytes",
		"Bytes used by the container's logs on the filesystem.",
		[]string{
			"uid",
			"namespace",
			"pod",
			"container",
		}, nil,
		metrics.ALPHA,
		"",
	)
)

type logMetricsCollector struct {
	metrics.BaseStableCollector

	podStats func() ([]statsapi.PodStats, error)
}

// Check if logMetricsCollector implements necessary interface
var _ metrics.StableCollector = &logMetricsCollector{}

// NewLogMetricsCollector implements the metrics.StableCollector interface and
// exposes metrics about container's log volume size.
func NewLogMetricsCollector(podStats func() ([]statsapi.PodStats, error)) metrics.StableCollector {
	return &logMetricsCollector{
		podStats: podStats,
	}
}

// DescribeWithStability implements the metrics.StableCollector interface.
func (c *logMetricsCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- descLogSize
}

// CollectWithStability implements the metrics.StableCollector interface.
func (c *logMetricsCollector) CollectWithStability(ch chan<- metrics.Metric) {
	// c.podStats默认为kl.StatsProvider.ListPodStats
	// 如果是kl.StatsProvider为cadvisorStatsProvider，则
	//   返回所有pod的监控状态
	//   其中Network为labels["io.kubernetes.container.name"]为"POD"的容器的网卡状态
	//   其中Containers为pod里所有普通container的状态
	//   其中VolumeStats为pod各个volume的获取目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free。状态分为两类EphemeralVolumes（EmptyDir且EmptyDir底层存储不是内存、ConfigMap、GitRepo）和PersistentVolumes
	//   EphemeralStorage
	//      其中AvailableBytes为rootFsInfo.Available
	//      其中CapacityBytes为rootFsInfo.Capacity
	//      其中InodesFree为rootFsInfo.InodesFree
	//      其中Inodes为rootFsInfo.Inodes
	//      其中Time为这些里面取最晚的时间，rootFsInfo.Timestamp、所有container里container.Rootfs.Time最晚的，所有ephemeralStats里volume.FsStats.Time最晚的
	//      其中UsedBytes这些里面相加，所有container.Rootfs.UsedBytes相加、所有container.Logs.UsedBytes相加、所有ephemeralStats里volume.FsStats.UsedBytes相加
	//      其中InodesUsed为这些相加，所有container.Rootfs.InodesUsed相加、所有ephemeralStats里volume.InodesUsed相加
	//   PersistentVolumes
	//     由各个驱动类型实现
	//   CPU和Memory为pod cgroup监控信息
	//   StartTime为pod status里的StartTime
	podStats, err := c.podStats()
	if err != nil {
		klog.Errorf("failed to get pod stats: %v", err)
		return
	}

	for _, ps := range podStats {
		for _, c := range ps.Containers {
			if c.Logs != nil && c.Logs.UsedBytes != nil {
				ch <- metrics.NewLazyConstMetric(
					descLogSize,
					metrics.GaugeValue,
					float64(*c.Logs.UsedBytes),
					ps.PodRef.UID,
					ps.PodRef.Namespace,
					ps.PodRef.Name,
					c.Name,
				)
			}
		}
	}
}
