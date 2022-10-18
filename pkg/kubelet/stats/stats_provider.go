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

package stats

import (
	"fmt"

	cadvisorapiv1 "github.com/google/cadvisor/info/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	internalapi "k8s.io/cri-api/pkg/apis"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/stats/pidlimit"
	"k8s.io/kubernetes/pkg/kubelet/status"
)

// NewCRIStatsProvider returns a StatsProvider that provides the node stats
// from cAdvisor and the container stats from CRI.
func NewCRIStatsProvider(
	cadvisor cadvisor.Interface,
	resourceAnalyzer stats.ResourceAnalyzer,
	podManager kubepod.Manager,
	runtimeCache kubecontainer.RuntimeCache,
	runtimeService internalapi.RuntimeService,
	imageService internalapi.ImageManagerService,
	logMetricsService LogMetricsService,
	osInterface kubecontainer.OSInterface,
) *StatsProvider {
	return newStatsProvider(cadvisor, podManager, runtimeCache, newCRIStatsProvider(cadvisor, resourceAnalyzer,
		runtimeService, imageService, logMetricsService, osInterface))
}

// NewCadvisorStatsProvider returns a containerStatsProvider that provides both
// the node and the container stats from cAdvisor.
func NewCadvisorStatsProvider(
	cadvisor cadvisor.Interface,
	resourceAnalyzer stats.ResourceAnalyzer,
	podManager kubepod.Manager,
	runtimeCache kubecontainer.RuntimeCache,
	imageService kubecontainer.ImageService,
	statusProvider status.PodStatusProvider,
) *StatsProvider {
	return newStatsProvider(cadvisor, podManager, runtimeCache, newCadvisorStatsProvider(cadvisor, resourceAnalyzer, imageService, statusProvider))
}

// newStatsProvider returns a new StatsProvider that provides node stats from
// cAdvisor and the container stats using the containerStatsProvider.
func newStatsProvider(
	cadvisor cadvisor.Interface,
	podManager kubepod.Manager,
	runtimeCache kubecontainer.RuntimeCache,
	containerStatsProvider containerStatsProvider,
) *StatsProvider {
	return &StatsProvider{
		cadvisor:               cadvisor,
		podManager:             podManager,
		runtimeCache:           runtimeCache,
		containerStatsProvider: containerStatsProvider,
	}
}

// StatsProvider provides the stats of the node and the pod-managed containers.
type StatsProvider struct {
	cadvisor     cadvisor.Interface
	podManager   kubepod.Manager
	runtimeCache kubecontainer.RuntimeCache
	containerStatsProvider
	rlimitStatsProvider
}

// containerStatsProvider is an interface that provides the stats of the
// containers managed by pods.
type containerStatsProvider interface {
	ListPodStats() ([]statsapi.PodStats, error)
	ListPodStatsAndUpdateCPUNanoCoreUsage() ([]statsapi.PodStats, error)
	ListPodCPUAndMemoryStats() ([]statsapi.PodStats, error)
	ImageFsStats() (*statsapi.FsStats, error)
	ImageFsDevice() (string, error)
}

type rlimitStatsProvider interface {
	RlimitStats() (*statsapi.RlimitStats, error)
}

// RlimitStats returns base information about process count
// 返回最大pid数量和当前的进程数量
// MaxPID最大的pid数量是从/proc/sys/kernel/pid_max获取
// NumOfRunningProcesses为系统运行的进程数
// Time为现在时间
func (p *StatsProvider) RlimitStats() (*statsapi.RlimitStats, error) {
	return pidlimit.Stats()
}

// GetCgroupStats returns the stats of the cgroup with the cgroupName. Note that
// this function doesn't generate filesystem stats.
// 返回容器最近的cpu和memory的使用情况，Accelerators状态（gpu）、UserDefinedMetrics（kubelet没有）、最近的网卡状态
func (p *StatsProvider) GetCgroupStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, *statsapi.NetworkStats, error) {
	// updateStats为true，则等待所有的container的housekeeping（更新监控数据）完成后。否则从memoryCache直接获取
	// 从cadvisor的memoryCache获得2个最近容器状态，返回容器的最近两个容器的v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态）
	info, err := getCgroupInfo(p.cadvisor, cgroupName, updateStats)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get cgroup stats for %q: %v", cgroupName, err)
	}
	// Rootfs and imagefs doesn't make sense for raw cgroup.
	// 返回容器最近的cpu和memory的使用情况，Accelerators状态（gpu）、UserDefinedMetrics（kubelet没有）
	s := cadvisorInfoToContainerStats(cgroupName, info, nil, nil)
	// 返回容器最近的网卡状态
	n := cadvisorInfoToNetworkStats(cgroupName, info)
	return s, n, nil
}

// GetCgroupCPUAndMemoryStats returns the CPU and memory stats of the cgroup with the cgroupName. Note that
// this function doesn't generate filesystem stats.
// 从cadvisor中，如果updateStats为true，则等待container的housekeeping（更新监控数据）完成后，否则从memoryCache直接获取。
// 返回container的StartTime（创建时间）、最后的cpu和memory的使用情况
func (p *StatsProvider) GetCgroupCPUAndMemoryStats(cgroupName string, updateStats bool) (*statsapi.ContainerStats, error) {
	// updateStats为true，则等待所有的container的housekeeping（更新监控数据）完成后。否则从memoryCache直接获取
	// 从cadvisor的memoryCache获得2个最近容器状态，返回容器的最近两个容器的v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态）
	info, err := getCgroupInfo(p.cadvisor, cgroupName, updateStats)
	if err != nil {
		return nil, fmt.Errorf("failed to get cgroup stats for %q: %v", cgroupName, err)
	}
	// Rootfs and imagefs doesn't make sense for raw cgroup.
	// 从info中解析出container的StartTime（创建时间）、最后的cpu和memory的使用情况
	s := cadvisorInfoToContainerCPUAndMemoryStats(cgroupName, info)
	return s, nil
}

// RootFsStats returns the stats of the node root filesystem.
// 返回kubelet root目录所在挂载设备的最近状态（状态的时间、磁盘大小、可用大小、使用量、inode剩余量、inode总量、inode使用情况）
func (p *StatsProvider) RootFsStats() (*statsapi.FsStats, error) {
	// 返回kubelet root目录所在挂载设备的最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode使用情况）
	rootFsInfo, err := p.cadvisor.RootFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs info: %v", err)
	}

	var nodeFsInodesUsed *uint64
	if rootFsInfo.Inodes != nil && rootFsInfo.InodesFree != nil {
		nodeFsIU := *rootFsInfo.Inodes - *rootFsInfo.InodesFree
		nodeFsInodesUsed = &nodeFsIU
	}

	// Get the root container stats's timestamp, which will be used as the
	// imageFs stats timestamp.  Dont force a stats update, as we only want the timestamp.
	// 从cadvisor中获得"/"容器的最近一个ContainerStats（容器的监控状态cpu、内存、网络、blkio、pid等）
	rootStats, err := getCgroupStats(p.cadvisor, "/", false)
	if err != nil {
		return nil, fmt.Errorf("failed to get root container stats: %v", err)
	}

	return &statsapi.FsStats{
		Time:           metav1.NewTime(rootStats.Timestamp),
		AvailableBytes: &rootFsInfo.Available,
		CapacityBytes:  &rootFsInfo.Capacity,
		UsedBytes:      &rootFsInfo.Usage,
		InodesFree:     rootFsInfo.InodesFree,
		Inodes:         rootFsInfo.Inodes,
		InodesUsed:     nodeFsInodesUsed,
	}, nil
}

// GetContainerInfo returns stats (from cAdvisor) for a container.
// 根据podFullName、containerName、podUID和req，返回info.ContainerInfo（包括在req.start到req.end时间范围内，最多req.maxStats个ContainerStats）
func (p *StatsProvider) GetContainerInfo(podFullName string, podUID types.UID, containerName string, req *cadvisorapiv1.ContainerInfoRequest) (*cadvisorapiv1.ContainerInfo, error) {
	// Resolve and type convert back again.
	// We need the static pod UID but the kubecontainer API works with types.UID.
	// 首先尝试将uid转成MirrorPodUID，然后在p.podManager.translationByUID中查找static uid
	// 如果未找到，则返回原始的uid
	podUID = types.UID(p.podManager.TranslatePodUID(podUID))

	// 缓存未过期，则返回缓存中（r.pods）的pod
	// 缓存过期了，则从runtime中获得所有的running pod，并更新r.pods和r.cacheTime
	pods, err := p.runtimeCache.GetPods()
	if err != nil {
		return nil, err
	}
	// podFullName不为空，则从根据podFullName查找pod
	// 否则，根据pod uid查找pod
	pod := kubecontainer.Pods(pods).FindPod(podFullName, podUID)
	// 根据containerName在pod.Containers中查找container
	container := pod.FindContainerByName(containerName)
	if container == nil {
		return nil, kubecontainer.ErrContainerNotFound
	}

	// 根据containerName和query，返回info.ContainerInfo（包括在req.start到req.end时间范围内，最多req.maxStats个ContainerStats）
	ci, err := p.cadvisor.DockerContainer(container.ID.ID, req)
	if err != nil {
		return nil, err
	}
	return &ci, nil
}

// GetRawContainerInfo returns the stats (from cadvisor) for a non-Kubernetes
// container.
// 从cadvisor中获得containerName的containerInfo，包含在req.start到req.end时间范围内，最多req.maxStats个ContainerStats
// 如果subcontainers为true，返回containerName和所有子container的containerInfo
func (p *StatsProvider) GetRawContainerInfo(containerName string, req *cadvisorapiv1.ContainerInfoRequest, subcontainers bool) (map[string]*cadvisorapiv1.ContainerInfo, error) {
	if subcontainers {
		// 返回containerName和所有子container的info.ContainerInfo（在req.start到req.end时间范围内，最多req.maxStats个ContainerStats）集合
		return p.cadvisor.SubcontainerInfo(containerName, req)
	}
	// 根据containerName和query，返回info.ContainerInfo（在req.start到req.end时间范围内，最多req.maxStats个ContainerStats）
	containerInfo, err := p.cadvisor.ContainerInfo(containerName, req)
	if err != nil {
		return nil, err
	}
	return map[string]*cadvisorapiv1.ContainerInfo{
		containerInfo.Name: containerInfo,
	}, nil
}

// HasDedicatedImageFs returns true if a dedicated image filesystem exists for storing images.
// 镜像保存的磁盘设备是否kubelet root目录所在挂载设备不一致
func (p *StatsProvider) HasDedicatedImageFs() (bool, error) {
	// 如果是cadvisor，获得镜像保存的磁盘设备名
	device, err := p.containerStatsProvider.ImageFsDevice()
	if err != nil {
		return false, err
	}
	// 返回kubelet root目录所在挂载设备的最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode使用情况）
	rootFsInfo, err := p.cadvisor.RootFsInfo()
	if err != nil {
		return false, err
	}
	// 镜像保存的磁盘设备是否kubelet root目录所在挂载设备不一致
	return device != rootFsInfo.Device, nil
}
