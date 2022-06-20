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
	"path"
	"sort"
	"strings"

	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/leaky"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/kubernetes/pkg/kubelet/status"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// cadvisorStatsProvider implements the containerStatsProvider interface by
// getting the container stats from cAdvisor. This is needed by
// integrations which do not provide stats from CRI. See
// `pkg/kubelet/cadvisor/util.go#UsingLegacyCadvisorStats` for the logic for
// determining which integrations do not provide stats from CRI.
type cadvisorStatsProvider struct {
	// cadvisor is used to get the stats of the cgroup for the containers that
	// are managed by pods.
	cadvisor cadvisor.Interface
	// resourceAnalyzer is used to get the volume stats of the pods.
	resourceAnalyzer stats.ResourceAnalyzer
	// imageService is used to get the stats of the image filesystem.
	imageService kubecontainer.ImageService
	// statusProvider is used to get pod metadata
	statusProvider status.PodStatusProvider
}

// newCadvisorStatsProvider returns a containerStatsProvider that provides
// container stats from cAdvisor.
func newCadvisorStatsProvider(
	cadvisor cadvisor.Interface,
	resourceAnalyzer stats.ResourceAnalyzer,
	imageService kubecontainer.ImageService,
	statusProvider status.PodStatusProvider,
) containerStatsProvider {
	return &cadvisorStatsProvider{
		cadvisor:         cadvisor,
		resourceAnalyzer: resourceAnalyzer,
		imageService:     imageService,
		statusProvider:   statusProvider,
	}
}

// ListPodStats returns the stats of all the pod-managed containers.
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
func (p *cadvisorStatsProvider) ListPodStats() ([]statsapi.PodStats, error) {
	// Gets node root filesystem information and image filesystem stats, which
	// will be used to populate the available and capacity bytes/inodes in
	// container stats.
	// 返回kubelet root目录所在挂载设备的最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode使用情况）
	rootFsInfo, err := p.cadvisor.RootFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs info: %v", err)
	}
	// 获得镜像保存的磁盘设备最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode使用情况），磁盘可能有多个
	imageFsInfo, err := p.cadvisor.ImagesFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get imageFs info: %v", err)
	}
	// 从cadvisor的memoryCache获得2个最近"/"容器状态，返回容器的最近两个v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态），包含所有子容器的ContainerInfo
	infos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info from cadvisor: %v", err)
	}
	// removeTerminatedContainerInfo will also remove pod level cgroups, so save the infos into allInfos first
	allInfos := infos
	// 对containerInfo进行过滤
	// 忽略掉不是pod name和pod namespace都有
	// 如果pod里container名字有多个cgroup（容器），则只取最晚创建时间且cpu有使用数值且有memory RSS数值
	// pod里container名字只有一个cgroup（容器），则保留
	infos = removeTerminatedContainerInfo(infos)
	// Map each container to a pod and update the PodStats with container data.
	podToStats := map[statsapi.PodReference]*statsapi.PodStats{}
	for key, cinfo := range infos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		// cgroup里有".mount"后缀，忽略
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		// 忽略掉不是pod name和pod namespace都有
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		// 根据label获得pod name和pod namespace和pod uid，生成statsapi.PodReference
		ref := buildPodRef(cinfo.Spec.Labels)

		// Lookup the PodStats for the pod using the PodRef. If none exists,
		// initialize a new entry.
		podStats, found := podToStats[ref]
		if !found {
			podStats = &statsapi.PodStats{PodRef: ref}
			podToStats[ref] = podStats
		}

		// Update the PodStats entry with the stats from the container by
		// adding it to podStats.Containers.
		// 获得labels["io.kubernetes.container.name"]值
		containerName := kubetypes.GetContainerName(cinfo.Spec.Labels)
		// container name为"POD"，获取最近网卡状态设置为podStats.Network
		if containerName == leaky.PodInfraContainerName {
			// Special case for infrastructure container which is hidden from
			// the user and has network stats.
			// 第一个参数"pod:{pod namespace}_{pod name}"没有使用
			// cadvisorInfoToNetworkStats返回最近的网卡状态
			podStats.Network = cadvisorInfoToNetworkStats("pod:"+ref.Namespace+"_"+ref.Name, &cinfo)
		} else {
			// cadvisorInfoToContainerStats返回最近的cpu和memory的使用情况，容器日志所在文件系统的状态，容器根文件系统的使用状态，Accelerators状态、UserDefinedMetrics

			// 其中StartTime为info.Spec.CreationTime
			// 其中CPU和memory为info中cpu和memory使用情况
			// Logs里的文件系统大小、使用量、inode总数、inode剩余量、inode使用量为rootFsInfo中的，UsedBytes（容器的读写层的存储使用量）为cinfo中最近一个cstat.Filesystem里TotalUsageBytes - BaseUsageBytes
			// Rootfs里文件系统大小、使用量、inode总数、inode剩余量使用imageFsInfo中的，UsedBytes为cinfo中最近一个cstat.BaseUsageBytes（docker存储的目录使用量），InodesUsed为cinfo中最近一个cstat.InodeUsage
			// 添加普通container的状态到podStats.Containers
			podStats.Containers = append(podStats.Containers, *cadvisorInfoToContainerStats(containerName, &cinfo, &rootFsInfo, &imageFsInfo))
		}
	}

	// Add each PodStats to the result.
	result := make([]statsapi.PodStats, 0, len(podToStats))
	for _, podStats := range podToStats {
		// Lookup the volume stats for each pod.
		podUID := types.UID(podStats.PodRef.UID)
		var ephemeralStats []statsapi.VolumeStats
		// 从缓存中获取PodVolumeStats（pod各个volume的获取目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free）
		// 状态分为两类EphemeralVolumes（EmptyDir且EmptyDir底层存储不是内存、ConfigMap、GitRepo）和PersistentVolumes
		if vstats, found := p.resourceAnalyzer.GetPodVolumeStats(podUID); found {
			ephemeralStats = make([]statsapi.VolumeStats, len(vstats.EphemeralVolumes))
			copy(ephemeralStats, vstats.EphemeralVolumes)
			// 将vstats.EphemeralVolumes和vstats.PersistentVolumes合并，赋值给podStats.VolumeStats
			podStats.VolumeStats = append(vstats.EphemeralVolumes, vstats.PersistentVolumes...)
		}
		// 返回statsapi.FsStats
		// 其中AvailableBytes为rootFsInfo.Available
		// 其中CapacityBytes为rootFsInfo.Capacity
		// 其中InodesFree为rootFsInfo.InodesFree
		// 其中Inodes为rootFsInfo.Inodes
		// 其中Time为这些里面取最晚的时间，rootFsInfo.Timestamp、所有container里container.Rootfs.Time最晚的，所有ephemeralStats里volume.FsStats.Time最晚的
		// 其中UsedBytes这些里面相加，所有container.Rootfs.UsedBytes相加、所有container.Logs.UsedBytes相加、所有ephemeralStats里volume.FsStats.UsedBytes相加
		// 其中InodesUsed为这些相加，所有container.Rootfs.InodesUsed相加、所有ephemeralStats里volume.InodesUsed相加
		podStats.EphemeralStorage = calcEphemeralStorage(podStats.Containers, ephemeralStats, &rootFsInfo, nil, false)
		// Lookup the pod-level cgroup's CPU and memory stats
		// 从allInfos里找到pod cgroup的cadvisorapiv2.ContainerInfo监控信息
		podInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
		if podInfo != nil {
			// 从podInfo里的最后一个ContainerStats，获取最后的cpu和memory的使用情况
			cpu, memory := cadvisorInfoToCPUandMemoryStats(podInfo)
			podStats.CPU = cpu
			podStats.Memory = memory
		}

		// 从p.statusProvider.podStatuses缓存中获取uid的相关的pod的status，（如果是mirror pod uid则通过static pod uid来查找）
		status, found := p.statusProvider.GetPodStatus(podUID)
		// podStats.StartTime为pod status中StartTime
		if found && status.StartTime != nil && !status.StartTime.IsZero() {
			podStats.StartTime = *status.StartTime
			// only append stats if we were able to get the start time of the pod
			result = append(result, *podStats)
		}
	}

	return result, nil
}

// ListPodStatsAndUpdateCPUNanoCoreUsage updates the cpu nano core usage for
// the containers and returns the stats for all the pod-managed containers.
// For cadvisor, cpu nano core usages are pre-computed and cached, so this
// function simply calls ListPodStats.
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
func (p *cadvisorStatsProvider) ListPodStatsAndUpdateCPUNanoCoreUsage() ([]statsapi.PodStats, error) {
	return p.ListPodStats()
}

// ListPodCPUAndMemoryStats returns the cpu and memory stats of all the pod-managed containers.
func (p *cadvisorStatsProvider) ListPodCPUAndMemoryStats() ([]statsapi.PodStats, error) {
	infos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to get container info from cadvisor: %v", err)
	}
	// removeTerminatedContainerInfo will also remove pod level cgroups, so save the infos into allInfos first
	allInfos := infos
	infos = removeTerminatedContainerInfo(infos)
	// Map each container to a pod and update the PodStats with container data.
	podToStats := map[statsapi.PodReference]*statsapi.PodStats{}
	for key, cinfo := range infos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		ref := buildPodRef(cinfo.Spec.Labels)

		// Lookup the PodStats for the pod using the PodRef. If none exists,
		// initialize a new entry.
		podStats, found := podToStats[ref]
		if !found {
			podStats = &statsapi.PodStats{PodRef: ref}
			podToStats[ref] = podStats
		}

		// Update the PodStats entry with the stats from the container by
		// adding it to podStats.Containers.
		containerName := kubetypes.GetContainerName(cinfo.Spec.Labels)
		if containerName == leaky.PodInfraContainerName {
			// Special case for infrastructure container which is hidden from
			// the user and has network stats.
			podStats.StartTime = metav1.NewTime(cinfo.Spec.CreationTime)
		} else {
			podStats.Containers = append(podStats.Containers, *cadvisorInfoToContainerCPUAndMemoryStats(containerName, &cinfo))
		}
	}

	// Add each PodStats to the result.
	result := make([]statsapi.PodStats, 0, len(podToStats))
	for _, podStats := range podToStats {
		podUID := types.UID(podStats.PodRef.UID)
		// Lookup the pod-level cgroup's CPU and memory stats
		podInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
		if podInfo != nil {
			cpu, memory := cadvisorInfoToCPUandMemoryStats(podInfo)
			podStats.CPU = cpu
			podStats.Memory = memory
		}
		result = append(result, *podStats)
	}

	return result, nil
}

// ImageFsStats returns the stats of the filesystem for storing images.
// 返回镜像保存的磁盘设备的磁盘大小、可用大小、镜像占用空间总大小、inode剩余数量、inode总量、inode使用量
func (p *cadvisorStatsProvider) ImageFsStats() (*statsapi.FsStats, error) {
	// 获得镜像保存的磁盘设备最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表、inode状态），磁盘可能有多个
	imageFsInfo, err := p.cadvisor.ImagesFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get imageFs info: %v", err)
	}
	// 获得node节点上所有镜像的总大小
	imageStats, err := p.imageService.ImageStats()
	if err != nil || imageStats == nil {
		return nil, fmt.Errorf("failed to get image stats: %v", err)
	}

	var imageFsInodesUsed *uint64
	if imageFsInfo.Inodes != nil && imageFsInfo.InodesFree != nil {
		imageFsIU := *imageFsInfo.Inodes - *imageFsInfo.InodesFree
		imageFsInodesUsed = &imageFsIU
	}

	return &statsapi.FsStats{
		Time:           metav1.NewTime(imageFsInfo.Timestamp),
		AvailableBytes: &imageFsInfo.Available,
		CapacityBytes:  &imageFsInfo.Capacity,
		// 镜像总大小
		UsedBytes:      &imageStats.TotalStorageBytes,
		InodesFree:     imageFsInfo.InodesFree,
		Inodes:         imageFsInfo.Inodes,
		InodesUsed:     imageFsInodesUsed,
	}, nil
}

// ImageFsDevice returns name of the device where the image filesystem locates,
// e.g. /dev/sda1.
// 获得镜像保存的磁盘设备名
func (p *cadvisorStatsProvider) ImageFsDevice() (string, error) {
	// 获得镜像保存的磁盘设备最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表），磁盘可能有多个
	imageFsInfo, err := p.cadvisor.ImagesFsInfo()
	if err != nil {
		return "", err
	}
	return imageFsInfo.Device, nil
}

// buildPodRef returns a PodReference that identifies the Pod managing cinfo
// 根据label获得pod name和pod namespace和pod uid，生成statsapi.PodReference
func buildPodRef(containerLabels map[string]string) statsapi.PodReference {
	// 获得labels["io.kubernetes.pod.name"]值
	podName := kubetypes.GetPodName(containerLabels)
	// 获得labels["io.kubernetes.pod.namespace"]值
	podNamespace := kubetypes.GetPodNamespace(containerLabels)
	// 获得labels["io.kubernetes.pod.uid"]值
	podUID := kubetypes.GetPodUID(containerLabels)
	return statsapi.PodReference{Name: podName, Namespace: podNamespace, UID: podUID}
}

// isPodManagedContainer returns true if the cinfo container is managed by a Pod
// 是否即有pod name和pod namespace
func isPodManagedContainer(cinfo *cadvisorapiv2.ContainerInfo) bool {
	// 获得labels["io.kubernetes.pod.name"]值
	podName := kubetypes.GetPodName(cinfo.Spec.Labels)
	// 获得labels["io.kubernetes.pod.namespace"]值
	podNamespace := kubetypes.GetPodNamespace(cinfo.Spec.Labels)
	managed := podName != "" && podNamespace != ""
	// podName和podNamespace至少有一个为空，且podName跟podNamespace不相等（即只有一个为空）
	if !managed && podName != podNamespace {
		klog.Warningf(
			"Expect container to have either both podName (%s) and podNamespace (%s) labels, or neither.",
			podName, podNamespace)
	}
	return managed
}

// getCadvisorPodInfoFromPodUID returns a pod cgroup information by matching the podUID with its CgroupName identifier base name
// 从infos里找到pod cgroup的cadvisorapiv2.ContainerInfo监控信息
func getCadvisorPodInfoFromPodUID(podUID types.UID, infos map[string]cadvisorapiv2.ContainerInfo) *cadvisorapiv2.ContainerInfo {
	for key, info := range infos {
		// cgroup路径有".slice"后缀，key为最后一个解析出来的路径
		if cm.IsSystemdStyleName(key) {
			// Convert to internal cgroup name and take the last component only.
			// name比如为"/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podeb424a44_7004_429d_9925_fbfbc69a7749.slice"
			// 返回["kubepods", "besteffort", "podeb424a44-7004-429d-9925-fbfbc69a7749"]
			internalCgroupName := cm.ParseSystemdToCgroupName(key)
			key = internalCgroupName[len(internalCgroupName)-1]
		} else {
			// Take last component only.
			// 其他为最后一个路径名
			key = path.Base(key)
		}
		// key为字符串"pod"+{pod uid}，说明找到了
		if cm.GetPodCgroupNameSuffix(podUID) == key {
			return &info
		}
	}
	return nil
}

// removeTerminatedContainerInfo returns the specified containerInfo but with
// the stats of the terminated containers removed.
//
// A ContainerInfo is considered to be of a terminated container if it has an
// older CreationTime and zero CPU instantaneous and memory RSS usage.
// 对containerInfo进行过滤
// 忽略掉不是pod name和pod namespace都有
// 如果pod里container名字有多个cgroup（容器），则只取最晚创建时间且cpu有使用数值且有memory RSS数值
// pod里container名字只有一个cgroup（容器），则保留
func removeTerminatedContainerInfo(containerInfo map[string]cadvisorapiv2.ContainerInfo) map[string]cadvisorapiv2.ContainerInfo {
	cinfoMap := make(map[containerID][]containerInfoWithCgroup)
	for key, cinfo := range containerInfo {
		// 不是pod name和pod namespace都有（pod name和pod namespace至少有一个为空），则忽略
		if !isPodManagedContainer(&cinfo) {
			continue
		}
		cinfoID := containerID{
			// 根据label获得pod name和pod namespace和pod uid，生成statsapi.PodReference
			podRef:        buildPodRef(cinfo.Spec.Labels),
			// 获得labels["io.kubernetes.container.name"]值
			containerName: kubetypes.GetContainerName(cinfo.Spec.Labels),
		}
		cinfoMap[cinfoID] = append(cinfoMap[cinfoID], containerInfoWithCgroup{
			cinfo:  cinfo,
			cgroup: key,
		})
	}
	result := make(map[string]cadvisorapiv2.ContainerInfo)
	for _, refs := range cinfoMap {
		// pod里container名字只有一个cgroup（容器），则保存到result
		if len(refs) == 1 {
			result[refs[0].cgroup] = refs[0].cinfo
			continue
		}
		// pod里container名字有多个cgroup（容器）
		// 根据创建时间从早到晚进行排序，如果时间相同，后面的cpu有使用数值且有memory RSS数值，则顺序不变
		sort.Sort(ByCreationTime(refs))
		for i := len(refs) - 1; i >= 0; i-- {
			// 使用最晚创建时间且cpu有使用数值且有memory RSS数值
			if hasMemoryAndCPUInstUsage(&refs[i].cinfo) {
				result[refs[i].cgroup] = refs[i].cinfo
				break
			}
		}
	}
	return result
}

// ByCreationTime implements sort.Interface for []containerInfoWithCgroup based
// on the cinfo.Spec.CreationTime field.
type ByCreationTime []containerInfoWithCgroup

func (a ByCreationTime) Len() int      { return len(a) }
func (a ByCreationTime) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByCreationTime) Less(i, j int) bool {
	// 如果创建时间一样，如果后面的cpu有使用数值且有memory RSS数值，则返回true，否则返回false
	if a[i].cinfo.Spec.CreationTime.Equal(a[j].cinfo.Spec.CreationTime) {
		// There shouldn't be two containers with the same name and/or the same
		// creation time. However, to make the logic here robust, we break the
		// tie by moving the one without CPU instantaneous or memory RSS usage
		// to the beginning.
		// 返回true情况为cpu有使用数值且有memory RSS数值
		return hasMemoryAndCPUInstUsage(&a[j].cinfo)
	}
	return a[i].cinfo.Spec.CreationTime.Before(a[j].cinfo.Spec.CreationTime)
}

// containerID is the identity of a container in a pod.
type containerID struct {
	podRef        statsapi.PodReference
	containerName string
}

// containerInfoWithCgroup contains the ContainerInfo and its cgroup name.
type containerInfoWithCgroup struct {
	cinfo  cadvisorapiv2.ContainerInfo
	cgroup string
}

// hasMemoryAndCPUInstUsage returns true if the specified container info has
// both non-zero CPU instantaneous usage and non-zero memory RSS usage, and
// false otherwise.
// 返回true情况为cpu有使用数值且有memory RSS数值
func hasMemoryAndCPUInstUsage(info *cadvisorapiv2.ContainerInfo) bool {
	// 没有cpu或没有memory，则返回false（exits的容器，cgroup目录是不存在的，所以Spec.HasCpu或Spec.HasMemory返回false）
	if !info.Spec.HasCpu || !info.Spec.HasMemory {
		return false
	}
	// 返回info.Stats里的最后一个ContainerStats
	cstat, found := latestContainerStats(info)
	// 没有ContainerStats或ContainerStats为空
	if !found {
		return false
	}
	// 没有cpu监控数据
	if cstat.CpuInst == nil {
		return false
	}
	return cstat.CpuInst.Usage.Total != 0 && cstat.Memory.RSS != 0
}

// 从cadvisor的memoryCache获得2个最近"/"容器状态，返回容器的最近两个v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态），包含所有子容器的ContainerInfo
func getCadvisorContainerInfo(ca cadvisor.Interface) (map[string]cadvisorapiv2.ContainerInfo, error) {
	// 从cadvisor的memoryCache获得2个最近容器状态，返回容器的最近两个v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态），包含所有子容器的ContainerInfo
	infos, err := ca.ContainerInfoV2("/", cadvisorapiv2.RequestOptions{
		IdType:    cadvisorapiv2.TypeName,
		Count:     2, // 2 samples are needed to compute "instantaneous" CPU
		Recursive: true,
	})
	if err != nil {
		// "/"容器存在，说明获取子容器的ContainerInfo信息，出现错误
		if _, ok := infos["/"]; ok {
			// If the failure is partial, log it and return a best-effort
			// response.
			klog.Errorf("Partial failure issuing cadvisor.ContainerInfoV2: %v", err)
		} else {
			return nil, fmt.Errorf("failed to get root cgroup stats: %v", err)
		}
	}
	return infos, nil
}
