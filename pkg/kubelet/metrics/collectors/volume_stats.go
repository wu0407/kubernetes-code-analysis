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
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/component-base/metrics"
	stats "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	kubeletmetrics "k8s.io/kubernetes/pkg/kubelet/metrics"
	serverstats "k8s.io/kubernetes/pkg/kubelet/server/stats"
)

var (
	volumeStatsCapacityBytesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsCapacityBytesKey),
		"Capacity in bytes of the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsAvailableBytesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsAvailableBytesKey),
		"Number of available bytes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsUsedBytesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsUsedBytesKey),
		"Number of used bytes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsInodesDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsInodesKey),
		"Maximum number of inodes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsInodesFreeDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsInodesFreeKey),
		"Number of free inodes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
	volumeStatsInodesUsedDesc = metrics.NewDesc(
		metrics.BuildFQName("", kubeletmetrics.KubeletSubsystem, kubeletmetrics.VolumeStatsInodesUsedKey),
		"Number of used inodes in the volume",
		[]string{"namespace", "persistentvolumeclaim"}, nil,
		metrics.ALPHA, "",
	)
)

type volumeStatsCollector struct {
	metrics.BaseStableCollector

	statsProvider serverstats.Provider
}

// Check if volumeStatsCollector implements necessary interface
var _ metrics.StableCollector = &volumeStatsCollector{}

// NewVolumeStatsCollector creates a volume stats metrics.StableCollector.
func NewVolumeStatsCollector(statsProvider serverstats.Provider) metrics.StableCollector {
	return &volumeStatsCollector{statsProvider: statsProvider}
}

// DescribeWithStability implements the metrics.StableCollector interface.
func (collector *volumeStatsCollector) DescribeWithStability(ch chan<- *metrics.Desc) {
	ch <- volumeStatsCapacityBytesDesc
	ch <- volumeStatsAvailableBytesDesc
	ch <- volumeStatsUsedBytesDesc
	ch <- volumeStatsInodesDesc
	ch <- volumeStatsInodesFreeDesc
	ch <- volumeStatsInodesUsedDesc
}

// CollectWithStability implements the metrics.StableCollector interface.
func (collector *volumeStatsCollector) CollectWithStability(ch chan<- metrics.Metric) {
	// 如果是collector.statsProvider最终为cadvisorStatsProvider，则
	// 返回所有pod的监控状态
	// 其中Network为labels["io.kubernetes.container.name"]为"POD"的容器的网卡状态
	// 其中Containers为pod里所有普通container的状态
	// 其中VolumeStats为pod各个volume的获取目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free。状态分为两类EphemeralVolumes（EmptyDir且EmptyDir底层存储不是内存、ConfigMap、GitRepo）和PersistentVolumes
	// EphemeralStorage
	//    其中AvailableBytes为rootFsInfo.Available
	//    其中CapacityBytes为rootFsInfo.Capacity
	//    其中InodesFree为rootFsInfo.InodesFree
	//    其中Inodes为rootFsInfo.Inodes
	//    其中Time为这些里面取最晚的时间，rootFsInfo.Timestamp、所有container里container.Rootfs.Time最晚的，所有ephemeralStats里volume.FsStats.Time最晚的
	//    其中UsedBytes这些里面相加，所有container.Rootfs.UsedBytes相加、所有container.Logs.UsedBytes相加、所有ephemeralStats里volume.FsStats.UsedBytes相加
	//    其中InodesUsed为这些相加，所有container.Rootfs.InodesUsed相加、所有ephemeralStats里volume.InodesUsed相加
	// PersistentVolumes
	//   由各个驱动类型实现
	// CPU和Memory为pod cgroup监控信息
	// StartTime为pod status里的StartTime
	podStats, err := collector.statsProvider.ListPodStats()
	if err != nil {
		return
	}
	addGauge := func(desc *metrics.Desc, pvcRef *stats.PVCReference, v float64, lv ...string) {
		lv = append([]string{pvcRef.Namespace, pvcRef.Name}, lv...)

		// 如果metrics不是隐藏的，则返回prometheus.constMetric，否则返回nil
		// 发送metrics到ch
		ch <- metrics.NewLazyConstMetric(desc, metrics.GaugeValue, v, lv...)
	}
	allPVCs := sets.String{}
	for _, podStat := range podStats {
		// 跳过没有volume 状态数据的pod
		if podStat.VolumeStats == nil {
			continue
		}
		for _, volumeStat := range podStat.VolumeStats {
			pvcRef := volumeStat.PVCRef
			if pvcRef == nil {
				// ignore if no PVC reference
				continue
			}
			pvcUniqStr := pvcRef.Namespace + "/" + pvcRef.Name
			if allPVCs.Has(pvcUniqStr) {
				// ignore if already collected
				continue
			}
			// 将volumeStatsCapacityBytesDesc发送到ch
			addGauge(volumeStatsCapacityBytesDesc, pvcRef, float64(*volumeStat.CapacityBytes))
			addGauge(volumeStatsAvailableBytesDesc, pvcRef, float64(*volumeStat.AvailableBytes))
			addGauge(volumeStatsUsedBytesDesc, pvcRef, float64(*volumeStat.UsedBytes))
			addGauge(volumeStatsInodesDesc, pvcRef, float64(*volumeStat.Inodes))
			addGauge(volumeStatsInodesFreeDesc, pvcRef, float64(*volumeStat.InodesFree))
			addGauge(volumeStatsInodesUsedDesc, pvcRef, float64(*volumeStat.InodesUsed))
			allPVCs.Insert(pvcUniqStr)
		}
	}
}
