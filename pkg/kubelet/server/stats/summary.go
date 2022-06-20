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

package stats

import (
	"fmt"

	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/util"
)

// SummaryProvider provides summaries of the stats from Kubelet.
type SummaryProvider interface {
	// Get provides a new Summary with the stats from Kubelet,
	// and will update some stats if updateStats is true
	Get(updateStats bool) (*statsapi.Summary, error)
	// GetCPUAndMemoryStats provides a new Summary with the CPU and memory stats from Kubelet,
	GetCPUAndMemoryStats() (*statsapi.Summary, error)
}

// summaryProviderImpl implements the SummaryProvider interface.
type summaryProviderImpl struct {
	// kubeletCreationTime is the time at which the summaryProvider was created.
	kubeletCreationTime metav1.Time
	// systemBootTime is the time at which the system was started
	systemBootTime metav1.Time

	provider Provider
}

var _ SummaryProvider = &summaryProviderImpl{}

// NewSummaryProvider returns a SummaryProvider using the stats provided by the
// specified statsProvider.
func NewSummaryProvider(statsProvider Provider) SummaryProvider {
	kubeletCreationTime := metav1.Now()
	// 开机时间点
	bootTime, err := util.GetBootTime()
	if err != nil {
		// bootTime will be zero if we encounter an error getting the boot time.
		klog.Warningf("Error getting system boot time.  Node metrics will have an incorrect start time: %v", err)
	}

	return &summaryProviderImpl{
		kubeletCreationTime: kubeletCreationTime,
		systemBootTime:      metav1.NewTime(bootTime),
		provider:            statsProvider,
	}
}

// 返回node状态和所有pod的状态监控信息
func (sp *summaryProviderImpl) Get(updateStats bool) (*statsapi.Summary, error) {
	// TODO(timstclair): Consider returning a best-effort response if any of
	// the following errors occur.
	// 当kl.kubeClient为nil，从kl.initialNode手动生成的初始的node对象，否则从informer中获取本机的node对象
	node, err := sp.provider.GetNode()
	if err != nil {
		return nil, fmt.Errorf("failed to get node info: %v", err)
	}
	// kl.containerManager.NodeConfig
	nodeConfig := sp.provider.GetNodeConfig()
	// 实现在pkg\kubelet\stats\stats_provider.go
	// 返回"/"容器最近的cpu和memory的使用情况，Accelerators状态（gpu）、UserDefinedMetrics（kubelet没有）、最近的网卡状态
	rootStats, networkStats, err := sp.provider.GetCgroupStats("/", updateStats)
	if err != nil {
		return nil, fmt.Errorf("failed to get root cgroup stats: %v", err)
	}
	// 返回kubelet root目录所在挂载设备的最近状态（状态的时间、磁盘大小、可用大小、使用量、inode剩余量、inode总量、inode使用情况） 
	rootFsStats, err := sp.provider.RootFsStats()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs stats: %v", err)
	}
	// 返回镜像保存的磁盘设备的磁盘大小、可用大小、镜像占用空间总大小、inode剩余数量、inode总量、inode使用量
	imageFsStats, err := sp.provider.ImageFsStats()
	if err != nil {
		return nil, fmt.Errorf("failed to get imageFs stats: %v", err)
	}
	var podStats []statsapi.PodStats
	if updateStats {
		// provider为cadvisorStatsProvider
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
		// CPU和Memory为pod cgroup监控信息
		// StartTime为pod status里的StartTime
		podStats, err = sp.provider.ListPodStatsAndUpdateCPUNanoCoreUsage()
	} else {
		// provider为cadvisorStatsProvider，跟ListPodStatsAndUpdateCPUNanoCoreUsage()一样
		podStats, err = sp.provider.ListPodStats()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list pod stats: %v", err)
	}

	// 返回最大pid数量和当前的进程数量
	// MaxPID最大的pid数量是从/proc/sys/kernel/pid_max获取
	// NumOfRunningProcesses为系统运行的进程数
	// Time为现在时间
	rlimit, err := sp.provider.RlimitStats()
	if err != nil {
		return nil, fmt.Errorf("failed to get rlimit stats: %v", err)
	}

	nodeStats := statsapi.NodeStats{
		NodeName:         node.Name,
		CPU:              rootStats.CPU,
		Memory:           rootStats.Memory,
		Network:          networkStats,
		StartTime:        sp.systemBootTime,
		Fs:               rootFsStats,
		Runtime:          &statsapi.RuntimeStats{ImageFs: imageFsStats},
		Rlimit:           rlimit,
		// 返回nodeconfig里的KubeletCgroupsName、RuntimeCgroupsName、SystemCgroupsName和pod cgroup root（比如"/kubepods.slice"）
		// 这些container最近的cpu和memory的使用情况，Accelerators状态（gpu）、UserDefinedMetrics（kubelet没有）、最近的网卡状态
		// 其中statsapi.ContainerStats里Logs, Rootfs为nil，并将Name（cgroup路径）改为对应的类型名
		// podStats没有使用
		SystemContainers: sp.GetSystemContainersStats(nodeConfig, podStats, updateStats),
	}
	summary := statsapi.Summary{
		Node: nodeStats,
		Pods: podStats,
	}
	return &summary, nil
}

func (sp *summaryProviderImpl) GetCPUAndMemoryStats() (*statsapi.Summary, error) {
	// TODO(timstclair): Consider returning a best-effort response if any of
	// the following errors occur.
	node, err := sp.provider.GetNode()
	if err != nil {
		return nil, fmt.Errorf("failed to get node info: %v", err)
	}
	nodeConfig := sp.provider.GetNodeConfig()
	rootStats, err := sp.provider.GetCgroupCPUAndMemoryStats("/", false)
	if err != nil {
		return nil, fmt.Errorf("failed to get root cgroup stats: %v", err)
	}

	podStats, err := sp.provider.ListPodCPUAndMemoryStats()
	if err != nil {
		return nil, fmt.Errorf("failed to list pod stats: %v", err)
	}

	nodeStats := statsapi.NodeStats{
		NodeName:         node.Name,
		CPU:              rootStats.CPU,
		Memory:           rootStats.Memory,
		StartTime:        rootStats.StartTime,
		SystemContainers: sp.GetSystemContainersCPUAndMemoryStats(nodeConfig, podStats, false),
	}
	summary := statsapi.Summary{
		Node: nodeStats,
		Pods: podStats,
	}
	return &summary, nil
}
