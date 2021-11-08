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
	"time"

	cadvisorapiv1 "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
)

// defaultNetworkInterfaceName is used for collectng network stats.
// This logic relies on knowledge of the container runtime implementation and
// is not reliable.
const defaultNetworkInterfaceName = "eth0"

// 从info.Stats里的最后一个ContainerStats，获取最后的cpu和memory的使用情况
func cadvisorInfoToCPUandMemoryStats(info *cadvisorapiv2.ContainerInfo) (*statsapi.CPUStats, *statsapi.MemoryStats) {
	// 返回info.Stats里的最后一个ContainerStats
	cstat, found := latestContainerStats(info)
	if !found {
		return nil, nil
	}
	var cpuStats *statsapi.CPUStats
	var memoryStats *statsapi.MemoryStats
	cpuStats = &statsapi.CPUStats{
		Time:                 metav1.NewTime(cstat.Timestamp),
		UsageNanoCores:       uint64Ptr(0),
		UsageCoreNanoSeconds: uint64Ptr(0),
	}
	if info.Spec.HasCpu {
		if cstat.CpuInst != nil {
			// CPU core-nanoseconds per second，cpu使用率
			cpuStats.UsageNanoCores = &cstat.CpuInst.Usage.Total
		}
		if cstat.Cpu != nil {
			cpuStats.UsageCoreNanoSeconds = &cstat.Cpu.Usage.Total
		}
	}
	if info.Spec.HasMemory && cstat.Memory != nil {
		pageFaults := cstat.Memory.ContainerData.Pgfault
		majorPageFaults := cstat.Memory.ContainerData.Pgmajfault
		memoryStats = &statsapi.MemoryStats{
			Time:            metav1.NewTime(cstat.Timestamp),
			UsageBytes:      &cstat.Memory.Usage,
			WorkingSetBytes: &cstat.Memory.WorkingSet,
			RSSBytes:        &cstat.Memory.RSS,
			PageFaults:      &pageFaults,
			MajorPageFaults: &majorPageFaults,
		}
		// availableBytes = memory limit (if known) - workingset
		// info.Spec.Memory.Limit不大于2^62次方
		if !isMemoryUnlimited(info.Spec.Memory.Limit) {
			availableBytes := info.Spec.Memory.Limit - cstat.Memory.WorkingSet
			memoryStats.AvailableBytes = &availableBytes
		}
	} else {
		memoryStats = &statsapi.MemoryStats{
			Time:            metav1.NewTime(cstat.Timestamp),
			WorkingSetBytes: uint64Ptr(0),
		}
	}
	return cpuStats, memoryStats
}

// cadvisorInfoToContainerStats returns the statsapi.ContainerStats converted
// from the container and filesystem info.
// 返回最近的cpu和memory的使用情况，容器日志所在文件系统的状态，容器根文件系统的使用状态，Accelerators状态、UserDefinedMetrics
func cadvisorInfoToContainerStats(name string, info *cadvisorapiv2.ContainerInfo, rootFs, imageFs *cadvisorapiv2.FsInfo) *statsapi.ContainerStats {
	result := &statsapi.ContainerStats{
		StartTime: metav1.NewTime(info.Spec.CreationTime),
		Name:      name,
	}
	// 返回info.Stats里的最后一个ContainerStats
	cstat, found := latestContainerStats(info)
	if !found {
		return result
	}

	// 从info.Stats里的最后一个ContainerStats，获取最后的cpu和memory的使用情况
	cpu, memory := cadvisorInfoToCPUandMemoryStats(info)
	result.CPU = cpu
	result.Memory = memory

	if rootFs != nil {
		// The container logs live on the node rootfs device
		// 返回文件系统状态信息（大小、使用量、inode总数、inode剩余量、inode使用量）
		result.Logs = buildLogsStats(cstat, rootFs)
	}

	if imageFs != nil {
		// The container rootFs lives on the imageFs devices (which may not be the node root fs)
		// 返回文件系统状态信息（大小、使用量、inode总数、inode剩余量）
		result.Rootfs = buildRootfsStats(cstat, imageFs)
	}

	cfs := cstat.Filesystem
	if cfs != nil {
		if cfs.BaseUsageBytes != nil {
			if result.Rootfs != nil {
				rootfsUsage := *cfs.BaseUsageBytes
				result.Rootfs.UsedBytes = &rootfsUsage
			}
			if cfs.TotalUsageBytes != nil && result.Logs != nil {
				logsUsage := *cfs.TotalUsageBytes - *cfs.BaseUsageBytes
				result.Logs.UsedBytes = &logsUsage
			}
		}
		if cfs.InodeUsage != nil && result.Rootfs != nil {
			rootInodes := *cfs.InodeUsage
			result.Rootfs.InodesUsed = &rootInodes
		}
	}

	for _, acc := range cstat.Accelerators {
		result.Accelerators = append(result.Accelerators, statsapi.AcceleratorStats{
			Make:        acc.Make,
			Model:       acc.Model,
			ID:          acc.ID,
			MemoryTotal: acc.MemoryTotal,
			MemoryUsed:  acc.MemoryUsed,
			DutyCycle:   acc.DutyCycle,
		})
	}

	result.UserDefinedMetrics = cadvisorInfoToUserDefinedMetrics(info)

	return result
}

// cadvisorInfoToContainerCPUAndMemoryStats returns the statsapi.ContainerStats converted
// from the container and filesystem info.
func cadvisorInfoToContainerCPUAndMemoryStats(name string, info *cadvisorapiv2.ContainerInfo) *statsapi.ContainerStats {
	result := &statsapi.ContainerStats{
		StartTime: metav1.NewTime(info.Spec.CreationTime),
		Name:      name,
	}

	cpu, memory := cadvisorInfoToCPUandMemoryStats(info)
	result.CPU = cpu
	result.Memory = memory

	return result
}

// cadvisorInfoToNetworkStats returns the statsapi.NetworkStats converted from
// the container info from cadvisor.
// 返回最近的网卡状态
func cadvisorInfoToNetworkStats(name string, info *cadvisorapiv2.ContainerInfo) *statsapi.NetworkStats {
	if !info.Spec.HasNetwork {
		return nil
	}
	// 返回info.Stats里的最后一个ContainerStats
	cstat, found := latestContainerStats(info)
	if !found {
		return nil
	}

	if cstat.Network == nil {
		return nil
	}

	iStats := statsapi.NetworkStats{
		Time: metav1.NewTime(cstat.Timestamp),
	}

	for i := range cstat.Network.Interfaces {
		inter := cstat.Network.Interfaces[i]
		iStat := statsapi.InterfaceStats{
			Name:     inter.Name,
			RxBytes:  &inter.RxBytes,
			RxErrors: &inter.RxErrors,
			TxBytes:  &inter.TxBytes,
			TxErrors: &inter.TxErrors,
		}

		if inter.Name == defaultNetworkInterfaceName {
			iStats.InterfaceStats = iStat
		}

		iStats.Interfaces = append(iStats.Interfaces, iStat)
	}

	return &iStats
}

// cadvisorInfoToUserDefinedMetrics returns the statsapi.UserDefinedMetric
// converted from the container info from cadvisor.
func cadvisorInfoToUserDefinedMetrics(info *cadvisorapiv2.ContainerInfo) []statsapi.UserDefinedMetric {
	type specVal struct {
		ref     statsapi.UserDefinedMetricDescriptor
		valType cadvisorapiv1.DataType
		time    time.Time
		value   float64
	}
	udmMap := map[string]*specVal{}
	for _, spec := range info.Spec.CustomMetrics {
		udmMap[spec.Name] = &specVal{
			ref: statsapi.UserDefinedMetricDescriptor{
				Name:  spec.Name,
				Type:  statsapi.UserDefinedMetricType(spec.Type),
				Units: spec.Units,
			},
			valType: spec.Format,
		}
	}
	for _, stat := range info.Stats {
		for name, values := range stat.CustomMetrics {
			specVal, ok := udmMap[name]
			if !ok {
				klog.Warningf("spec for custom metric %q is missing from cAdvisor output. Spec: %+v, Metrics: %+v", name, info.Spec, stat.CustomMetrics)
				continue
			}
			for _, value := range values {
				// Pick the most recent value
				if value.Timestamp.Before(specVal.time) {
					continue
				}
				specVal.time = value.Timestamp
				specVal.value = value.FloatValue
				if specVal.valType == cadvisorapiv1.IntType {
					specVal.value = float64(value.IntValue)
				}
			}
		}
	}
	var udm []statsapi.UserDefinedMetric
	for _, specVal := range udmMap {
		udm = append(udm, statsapi.UserDefinedMetric{
			UserDefinedMetricDescriptor: specVal.ref,
			Time:                        metav1.NewTime(specVal.time),
			Value:                       specVal.value,
		})
	}
	return udm
}

// latestContainerStats returns the latest container stats from cadvisor, or nil if none exist
// 返回info.Stats里的最后一个ContainerStats
func latestContainerStats(info *cadvisorapiv2.ContainerInfo) (*cadvisorapiv2.ContainerStats, bool) {
	stats := info.Stats
	if len(stats) < 1 {
		return nil, false
	}
	latest := stats[len(stats)-1]
	if latest == nil {
		return nil, false
	}
	return latest, true
}

func isMemoryUnlimited(v uint64) bool {
	// Size after which we consider memory to be "unlimited". This is not
	// MaxInt64 due to rounding by the kernel.
	// TODO: cadvisor should export this https://github.com/google/cadvisor/blob/master/metrics/prometheus.go#L596
	const maxMemorySize = uint64(1 << 62)

	return v > maxMemorySize
}

// getCgroupInfo returns the information of the container with the specified
// containerName from cadvisor.
// 等待所有的container的housekeeping（更新监控数据）完成后，从cadvisor的memoryCache获得2个最近容器状态，返回容器的最近两个容器的v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态）
func getCgroupInfo(cadvisor cadvisor.Interface, containerName string, updateStats bool) (*cadvisorapiv2.ContainerInfo, error) {
	var maxAge *time.Duration
	if updateStats {
		age := 0 * time.Second
		maxAge = &age
	}
	// 等待所有的container的housekeeping（更新监控数据）完成后，从cadvisor的memoryCache获得2个最近容器状态，返回容器的最近两个v2.ContainerInfo包括ContainerSpec（包括各种（是否有cpu、内存、网络、blkio、pid等）属性）和ContainerStats（容器的监控状态）
	infoMap, err := cadvisor.ContainerInfoV2(containerName, cadvisorapiv2.RequestOptions{
		IdType:    cadvisorapiv2.TypeName,
		Count:     2, // 2 samples are needed to compute "instantaneous" CPU
		Recursive: false,
		MaxAge:    maxAge,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get container info for %q: %v", containerName, err)
	}
	if len(infoMap) != 1 {
		return nil, fmt.Errorf("unexpected number of containers: %v", len(infoMap))
	}
	info := infoMap[containerName]
	return &info, nil
}

// getCgroupStats returns the latest stats of the container having the
// specified containerName from cadvisor.
func getCgroupStats(cadvisor cadvisor.Interface, containerName string, updateStats bool) (*cadvisorapiv2.ContainerStats, error) {
	info, err := getCgroupInfo(cadvisor, containerName, updateStats)
	if err != nil {
		return nil, err
	}
	stats, found := latestContainerStats(info)
	if !found {
		return nil, fmt.Errorf("failed to get latest stats from container info for %q", containerName)
	}
	return stats, nil
}

// 返回文件系统状态信息
func buildLogsStats(cstat *cadvisorapiv2.ContainerStats, rootFs *cadvisorapiv2.FsInfo) *statsapi.FsStats {
	fsStats := &statsapi.FsStats{
		Time:           metav1.NewTime(cstat.Timestamp),
		AvailableBytes: &rootFs.Available,
		CapacityBytes:  &rootFs.Capacity,
		InodesFree:     rootFs.InodesFree,
		Inodes:         rootFs.Inodes,
	}

	if rootFs.Inodes != nil && rootFs.InodesFree != nil {
		logsInodesUsed := *rootFs.Inodes - *rootFs.InodesFree
		fsStats.InodesUsed = &logsInodesUsed
	}
	return fsStats
}

// 返回文件系统状态信息
func buildRootfsStats(cstat *cadvisorapiv2.ContainerStats, imageFs *cadvisorapiv2.FsInfo) *statsapi.FsStats {
	return &statsapi.FsStats{
		Time:           metav1.NewTime(cstat.Timestamp),
		AvailableBytes: &imageFs.Available,
		CapacityBytes:  &imageFs.Capacity,
		InodesFree:     imageFs.InodesFree,
		Inodes:         imageFs.Inodes,
	}
}

func getUint64Value(value *uint64) uint64 {
	if value == nil {
		return 0
	}

	return *value
}

func uint64Ptr(i uint64) *uint64 {
	return &i
}

func calcEphemeralStorage(containers []statsapi.ContainerStats, volumes []statsapi.VolumeStats, rootFsInfo *cadvisorapiv2.FsInfo,
	podLogStats *statsapi.FsStats, isCRIStatsProvider bool) *statsapi.FsStats {
	result := &statsapi.FsStats{
		Time:           metav1.NewTime(rootFsInfo.Timestamp),
		AvailableBytes: &rootFsInfo.Available,
		CapacityBytes:  &rootFsInfo.Capacity,
		InodesFree:     rootFsInfo.InodesFree,
		Inodes:         rootFsInfo.Inodes,
	}
	for _, container := range containers {
		addContainerUsage(result, &container, isCRIStatsProvider)
	}
	for _, volume := range volumes {
		result.UsedBytes = addUsage(result.UsedBytes, volume.FsStats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, volume.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &volume.FsStats.Time)
	}
	if podLogStats != nil {
		result.UsedBytes = addUsage(result.UsedBytes, podLogStats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, podLogStats.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &podLogStats.Time)
	}
	return result
}

func addContainerUsage(stat *statsapi.FsStats, container *statsapi.ContainerStats, isCRIStatsProvider bool) {
	if rootFs := container.Rootfs; rootFs != nil {
		stat.Time = maxUpdateTime(&stat.Time, &rootFs.Time)
		stat.InodesUsed = addUsage(stat.InodesUsed, rootFs.InodesUsed)
		stat.UsedBytes = addUsage(stat.UsedBytes, rootFs.UsedBytes)
		if logs := container.Logs; logs != nil {
			stat.UsedBytes = addUsage(stat.UsedBytes, logs.UsedBytes)
			// We have accurate container log inode usage for CRI stats provider.
			if isCRIStatsProvider {
				stat.InodesUsed = addUsage(stat.InodesUsed, logs.InodesUsed)
			}
			stat.Time = maxUpdateTime(&stat.Time, &logs.Time)
		}
	}
}

func maxUpdateTime(first, second *metav1.Time) metav1.Time {
	if first.Before(second) {
		return *second
	}
	return *first
}

func addUsage(first, second *uint64) *uint64 {
	if first == nil {
		return second
	} else if second == nil {
		return first
	}
	total := *first + *second
	return &total
}
