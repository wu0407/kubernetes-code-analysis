// +build linux

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

package kuberuntime

import (
	"time"

	cgroupfs "github.com/opencontainers/runc/libcontainer/cgroups/fs"
	v1 "k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/qos"
)

// applyPlatformSpecificContainerConfig applies platform specific configurations to runtimeapi.ContainerConfig.
// 生成container的*runtimeapi.LinuxContainerConfig，包括SecurityContext和Resources（CpuPeriod、CpuQuota、CpuShares、MemoryLimitInBytes、OomScoreAdj、CpusetCpus（没有设置）、CpusetMems（没有设置）、HugepageLimits）
func (m *kubeGenericRuntimeManager) applyPlatformSpecificContainerConfig(config *runtimeapi.ContainerConfig, container *v1.Container, pod *v1.Pod, uid *int64, username string, nsTarget *kubecontainer.ContainerID) error {
	config.Linux = m.generateLinuxContainerConfig(container, pod, uid, username, nsTarget)
	return nil
}

// generateLinuxContainerConfig generates linux container config for kubelet runtime v1.
// 生成container的*runtimeapi.LinuxContainerConfig，包括SecurityContext和Resources（CpuPeriod、CpuQuota、CpuShares、MemoryLimitInBytes、OomScoreAdj、CpusetCpus（没有设置）、CpusetMems（没有设置）、HugepageLimits）
func (m *kubeGenericRuntimeManager) generateLinuxContainerConfig(container *v1.Container, pod *v1.Pod, uid *int64, username string, nsTarget *kubecontainer.ContainerID) *runtimeapi.LinuxContainerConfig {
	lc := &runtimeapi.LinuxContainerConfig{
		Resources:       &runtimeapi.LinuxContainerResources{},
		// 根据pod和container生成*runtimeapi.LinuxContainerSecurityContext
		// 不包含container.SecurityContext里的WindowsOptions、RunAsNonRoot
		SecurityContext: m.determineEffectiveSecurityContext(pod, container, uid, username),
	}

	// pid的命名空间的模式是container模式，且nsTarget不为nil（这个container为ephemeralContainer且配置TargetContainerName）
	// 则设置pid模式为target模式，且设置TargetId
	if nsTarget != nil && lc.SecurityContext.NamespaceOptions.Pid == runtimeapi.NamespaceMode_CONTAINER {
		lc.SecurityContext.NamespaceOptions.Pid = runtimeapi.NamespaceMode_TARGET
		lc.SecurityContext.NamespaceOptions.TargetId = nsTarget.ID
	}

	// set linux container resources
	// 这里没有处理cpuset
	var cpuShares int64
	cpuRequest := container.Resources.Requests.Cpu()
	cpuLimit := container.Resources.Limits.Cpu()
	memoryLimit := container.Resources.Limits.Memory().Value()
	// 计算container的OOMScoreAdjust值
	// 1. pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，则OOMScoreAdjust值为-998
	// 2. "Guaranteed"的pod，OOMScoreAdjust值为-998
	// 3. "BestEffort"的pod，OOMScoreAdjust值为1000
	// 4. "Burstable"的pod的container，算法是
	// 先计算1000-(1000*{memory request}/{node节点的memory capcity})的值
	// 这个值小于2，则oomScoreAdjust设置为2
	// 这个值等于1000，则oomScoreAdjust调整为999
	// 其他情况oomScoreAdjust为这个值
	oomScoreAdj := int64(qos.GetContainerOOMScoreAdjust(pod, container,
		int64(m.machineInfo.MemoryCapacity)))
	// If request is not specified, but limit is, we want request to default to limit.
	// API server does this for new containers, but we repeat this logic in Kubelet
	// for containers running on existing Kubernetes clusters.
	// cpu资源的request为0，但是limit不为0，则cpushare使用cpu资源的limit来计算
	// 一个cpu等于1024 cpu_share，最小的cpu_share为2
	if cpuRequest.IsZero() && !cpuLimit.IsZero() {
		cpuShares = milliCPUToShares(cpuLimit.MilliValue())
	} else {
		// if cpuRequest.Amount is nil, then milliCPUToShares will return the minimal number
		// of CPU shares.
		// 一个cpu等于1024 cpu_share，最小的cpu_share为2
		cpuShares = milliCPUToShares(cpuRequest.MilliValue())
	}
	lc.Resources.CpuShares = cpuShares
	// 设置memory资源的limit不为0，则设置lc.Resources.MemoryLimitInBytes，即没有memory资源的limit，则不设置lc.Resources.MemoryLimitInBytes
	if memoryLimit != 0 {
		lc.Resources.MemoryLimitInBytes = memoryLimit
	}
	// Set OOM score of the container based on qos policy. Processes in lower-priority pods should
	// be killed first if the system runs out of memory.
	lc.Resources.OomScoreAdj = oomScoreAdj

	// 内核支持cpu cfs quota
	if m.cpuCFSQuota {
		// if cpuLimit.Amount is nil, then the appropriate default value is returned
		// to allow full usage of cpu resource.
		// 未启用"CustomCPUCFSQuotaPeriod"，则period固定为100ms
		cpuPeriod := int64(quotaPeriod)
		if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUCFSQuotaPeriod) {
			cpuPeriod = int64(m.cpuCFSQuotaPeriod.Duration / time.Microsecond)
		}
		// quota = milliCPU * period/1000，最小的quota为1000（1ms）
		cpuQuota := milliCPUToQuota(cpuLimit.MilliValue(), cpuPeriod)
		lc.Resources.CpuQuota = cpuQuota
		lc.Resources.CpuPeriod = cpuPeriod
	}

	// 返回hugepage资源的limit里系统支持的pagesize，和它的limit值（container resource里的limit值，不是系统支持的最大值）
	lc.Resources.HugepageLimits = GetHugepageLimitsFromResources(container.Resources)

	return lc
}

// GetHugepageLimitsFromResources returns limits of each hugepages from resources.
// 返回hugepage资源的limit里系统支持的pagesize，和它的limit值（container resource里的limit值，不是系统支持的最大值）
func GetHugepageLimitsFromResources(resources v1.ResourceRequirements) []*runtimeapi.HugepageLimit {
	var hugepageLimits []*runtimeapi.HugepageLimit

	// For each page size, limit to 0.
	// 遍历系统支持的hugepage类型列表
	for _, pageSize := range cgroupfs.HugePageSizes {
		hugepageLimits = append(hugepageLimits, &runtimeapi.HugepageLimit{
			PageSize: pageSize,
			Limit:    uint64(0),
		})
	}

	requiredHugepageLimits := map[string]uint64{}
	for resourceObj, amountObj := range resources.Limits {
		// 忽略resourceObj不包含"hugepages-"前缀，即跳过非hugepage资源
		if !v1helper.IsHugePageResourceName(resourceObj) {
			continue
		}

		// 返回hugepage的pagesize
		pageSize, err := v1helper.HugePageSizeFromResourceName(resourceObj)
		if err != nil {
			klog.Warningf("Failed to get hugepage size from resource name: %v", err)
			continue
		}

		// 转成存储格式（"B", "KB", "MB", "GB", "TB", "PB"）
		sizeString, err := v1helper.HugePageUnitSizeFromByteSize(pageSize.Value())
		if err != nil {
			klog.Warningf("pageSize is invalid: %v", err)
			continue
		}
		requiredHugepageLimits[sizeString] = uint64(amountObj.Value())
	}

	// 遍历系统支持的hugepage类型列表，只返回hugepage limit里支持的pagesize，不支持pagesize不返回
	for _, hugepageLimit := range hugepageLimits {
		if limit, exists := requiredHugepageLimits[hugepageLimit.PageSize]; exists {
			hugepageLimit.Limit = limit
		}
	}

	return hugepageLimits
}
