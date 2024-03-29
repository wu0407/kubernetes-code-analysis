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

package cm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubefeatures "k8s.io/kubernetes/pkg/features"
)

const (
	// Taken from lmctfy https://github.com/google/lmctfy/blob/master/lmctfy/controllers/cpu_controller.cc
	MinShares     = 2
	SharesPerCPU  = 1024
	MilliCPUToCPU = 1000

	// 100000 is equivalent to 100ms
	QuotaPeriod    = 100000
	MinQuotaPeriod = 1000
)

// MilliCPUToQuota converts milliCPU to CFS quota and period values.
// quota = milliCPU * period/1000, 未启用"CustomCPUCFSQuotaPeriod"，则period固定为100ms。最小的quota为1000（1ms）
func MilliCPUToQuota(milliCPU int64, period int64) (quota int64) {
	// CFS quota is measured in two values:
	//  - cfs_period_us=100ms (the amount of time to measure usage across given by period)
	//  - cfs_quota=20ms (the amount of cpu time allowed to be used across a period)
	// so in the above example, you are limited to 20% of a single CPU
	// for multi-cpu environments, you just scale equivalent amounts
	// see https://www.kernel.org/doc/Documentation/scheduler/sched-bwc.txt for details

	if milliCPU == 0 {
		return
	}

	// 未启用"CustomCPUCFSQuotaPeriod"，则period固定为100ms
	if !utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUCFSQuotaPeriod) {
		period = QuotaPeriod
	}

	// we then convert your milliCPU to a value normalized over a period
	quota = (milliCPU * period) / MilliCPUToCPU

	// quota needs to be a minimum of 1ms.
	if quota < MinQuotaPeriod {
		quota = MinQuotaPeriod
	}
	return
}

// MilliCPUToShares converts the milliCPU to CFS shares.
// 一个cpu等于1024 cpu_share，最小的cpu_share为2
func MilliCPUToShares(milliCPU int64) uint64 {
	if milliCPU == 0 {
		// Docker converts zero milliCPU to unset, which maps to kernel default
		// for unset: 1024. Return 2 here to really match kernel default for
		// zero milliCPU.
		return MinShares
	}
	// Conceptually (milliCPU / milliCPUToCPU) * sharesPerCPU, but factored to improve rounding.
	shares := (milliCPU * SharesPerCPU) / MilliCPUToCPU
	if shares < MinShares {
		return MinShares
	}
	return uint64(shares)
}

// HugePageLimits converts the API representation to a map
// from huge page size (in bytes) to huge page limit (in bytes).
// 返回hugepage页大小与limit大小
func HugePageLimits(resourceList v1.ResourceList) map[int64]int64 {
	hugePageLimits := map[int64]int64{}
	for k, v := range resourceList {
		// 判断是否有"hugepages-"前缀
		if v1helper.IsHugePageResourceName(k) {
			// 获得hugepage的页大小
			pageSize, _ := v1helper.HugePageSizeFromResourceName(k)
			if value, exists := hugePageLimits[pageSize.Value()]; exists {
				hugePageLimits[pageSize.Value()] = value + v.Value()
			} else {
				hugePageLimits[pageSize.Value()] = v.Value()
			}
		}
	}
	return hugePageLimits
}

// ResourceConfigForPod takes the input pod and outputs the cgroup resource config.
// 计算pod里所有container的各个类型的资源总和
// cpu request对应的是cgroup里的cpu_share，一个cpu等于1024 cpu_share，最小的cpu_share为2
// cpu limit对应的是cgroup里的cfs_quota，quota = milliCPU * period/1000, 未启用"CustomCPUCFSQuotaPeriod"，则period固定为100ms。最小的quota为1000（1ms）
// "Guaranteed" pod有HugePageLimit、CpuShares、CpuQuota、CpuPeriod、CpuPeriod、Memory
// "Burstable" pod有HugePageLimit、CpuShares、（pod的所有container都设置了cpu limit，才设置CpuQuota和CpuPeriod）、（"Burstable"的pod的所有container都设置了memory limit，才设置MemoryLimits）
// "Besteffort"的pod，只设置CpuShares、HugePageLimit
func ResourceConfigForPod(pod *v1.Pod, enforceCPULimits bool, cpuPeriod uint64) *ResourceConfig {
	// sum requests and limits.
	// 统计pod的各种资源的limit和request
	reqs, limits := resource.PodRequestsAndLimits(pod)

	cpuRequests := int64(0)
	cpuLimits := int64(0)
	memoryLimits := int64(0)
	if request, found := reqs[v1.ResourceCPU]; found {
		cpuRequests = request.MilliValue()
	}
	if limit, found := limits[v1.ResourceCPU]; found {
		cpuLimits = limit.MilliValue()
	}
	if limit, found := limits[v1.ResourceMemory]; found {
		memoryLimits = limit.Value()
	}

	// convert to CFS values
	// 一个cpu等于1024 cpu_share，最小的cpu_share为2
	// request对应的是cpu_share
	cpuShares := MilliCPUToShares(cpuRequests)
	// quota = milliCPU * period/1000, 未启用"CustomCPUCFSQuotaPeriod"，则period固定为100ms。最小的quota为1000（1ms）
	// limit对应的是cfs_quota
	cpuQuota := MilliCPUToQuota(cpuLimits, int64(cpuPeriod))

	// track if limits were applied for each resource.
	memoryLimitsDeclared := true
	cpuLimitsDeclared := true
	// map hugepage pagesize (bytes) to limits (bytes)
	hugePageLimits := map[int64]int64{}
	for _, container := range pod.Spec.Containers {
		// 只要有一个container没有设置cpu limit，则cpuLimitsDeclared为false
		if container.Resources.Limits.Cpu().IsZero() {
			cpuLimitsDeclared = false
		}
		// 只要有一个container没有设置memory limit，则memoryLimitsDeclared为false
		if container.Resources.Limits.Memory().IsZero() {
			memoryLimitsDeclared = false
		}
		// 返回hugepage页大小与limit大小
		containerHugePageLimits := HugePageLimits(container.Resources.Requests)
		for k, v := range containerHugePageLimits {
			if value, exists := hugePageLimits[k]; exists {
				hugePageLimits[k] = value + v
			} else {
				hugePageLimits[k] = v
			}
		}
	}

	// quota is not capped when cfs quota is disabled
	// enforceCPULimits默认为true
	if !enforceCPULimits {
		cpuQuota = int64(-1)
	}

	// determine the qos class
	qosClass := v1qos.GetPodQOS(pod)

	// build the result
	result := &ResourceConfig{}
	if qosClass == v1.PodQOSGuaranteed {
		result.CpuShares = &cpuShares
		result.CpuQuota = &cpuQuota
		result.CpuPeriod = &cpuPeriod
		result.Memory = &memoryLimits
	} else if qosClass == v1.PodQOSBurstable {
		result.CpuShares = &cpuShares
		// "Burstable"的pod的所有container都设置了cpu limit，才设置CpuQuota和CpuPeriod
		if cpuLimitsDeclared {
			result.CpuQuota = &cpuQuota
			result.CpuPeriod = &cpuPeriod
		}
		// "Burstable"的pod的所有container都设置了memory limit，才设置Memory
		if memoryLimitsDeclared {
			result.Memory = &memoryLimits
		}
	} else {
		// "Besteffort"的pod，只设置CpuShares
		shares := uint64(MinShares)
		result.CpuShares = &shares
	}
	// 所有qos class的pod都会设置HugePageLimit
	result.HugePageLimit = hugePageLimits
	return result
}

// GetCgroupSubsystems returns information about the mounted cgroup subsystems
// Mounts：获得所有cgroup子系统的Mountpoint、Root、对应的Subsystems列表
// MountPoints：子系统和挂载点信息
func GetCgroupSubsystems() (*CgroupSubsystems, error) {
	// get all cgroup mounts.
	// 从/proc/self/mountinfo中获得所有cgroup子系统的Mountpoint、Root、对应的Subsystems列表
	allCgroups, err := libcontainercgroups.GetCgroupMounts(true)
	if err != nil {
		return &CgroupSubsystems{}, err
	}
	if len(allCgroups) == 0 {
		return &CgroupSubsystems{}, fmt.Errorf("failed to find cgroup mounts")
	}
	mountPoints := make(map[string]string, len(allCgroups))
	for _, mount := range allCgroups {
		for _, subsystem := range mount.Subsystems {
			mountPoints[subsystem] = mount.Mountpoint
		}
	}
	return &CgroupSubsystems{
		Mounts:      allCgroups,
		MountPoints: mountPoints,
	}, nil
}

// getCgroupProcs takes a cgroup directory name as an argument
// reads through the cgroup's procs file and returns a list of tgid's.
// It returns an empty list if a procs file doesn't exists
// 获取"{dir}/cgroup.procs"里的文件内容里的所有pid
func getCgroupProcs(dir string) ([]int, error) {
	procsFile := filepath.Join(dir, "cgroup.procs")
	f, err := os.Open(procsFile)
	if err != nil {
		if os.IsNotExist(err) {
			// The procsFile does not exist, So no pids attached to this directory
			return []int{}, nil
		}
		return nil, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	out := []int{}
	for s.Scan() {
		if t := s.Text(); t != "" {
			pid, err := strconv.Atoi(t)
			if err != nil {
				return nil, fmt.Errorf("unexpected line in %v; could not convert to pid: %v", procsFile, err)
			}
			out = append(out, pid)
		}
	}
	return out, nil
}

// GetPodCgroupNameSuffix returns the last element of the pod CgroupName identifier
// 返回字符串"pod"+{pod uid}
func GetPodCgroupNameSuffix(podUID types.UID) string {
	return podCgroupNamePrefix + string(podUID)
}

// NodeAllocatableRoot returns the literal cgroup path for the node allocatable cgroup
func NodeAllocatableRoot(cgroupRoot, cgroupDriver string) string {
	root := ParseCgroupfsToCgroupName(cgroupRoot)
	nodeAllocatableRoot := NewCgroupName(root, defaultNodeAllocatableCgroupName)
	if libcontainerCgroupManagerType(cgroupDriver) == libcontainerSystemd {
		return nodeAllocatableRoot.ToSystemd()
	}
	return nodeAllocatableRoot.ToCgroupfs()
}

// GetKubeletContainer returns the cgroup the kubelet will use
func GetKubeletContainer(kubeletCgroups string) (string, error) {
	if kubeletCgroups == "" {
		cont, err := getContainer(os.Getpid())
		if err != nil {
			return "", err
		}
		return cont, nil
	}
	return kubeletCgroups, nil
}

// GetRuntimeContainer returns the cgroup used by the container runtime
func GetRuntimeContainer(containerRuntime, runtimeCgroups string) (string, error) {
	if containerRuntime == "docker" {
		cont, err := getContainerNameForProcess(dockerProcessName, dockerPidFile)
		if err != nil {
			return "", fmt.Errorf("failed to get container name for docker process: %v", err)
		}
		return cont, nil
	}
	return runtimeCgroups, nil
}
