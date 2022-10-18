// +build !windows

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

package stats

import (
	"k8s.io/klog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cm"
)

// 返回nodeconfig里的KubeletCgroupsName、RuntimeCgroupsName、SystemCgroupsName和pod cgroup root（比如"/kubepods.slice"）
// 这些container最近的cpu和memory的使用情况，Accelerators状态（gpu）、UserDefinedMetrics（kubelet没有）、最近的网卡状态
// 其中statsapi.ContainerStats里Logs, Rootfs为nil，并将Name（cgroup路径）改为对应的类型名
func (sp *summaryProviderImpl) GetSystemContainersStats(nodeConfig cm.NodeConfig, podStats []statsapi.PodStats, updateStats bool) (stats []statsapi.ContainerStats) {
	systemContainers := map[string]struct {
		name             string
		forceStatsUpdate bool
		startTime        metav1.Time
	}{
		// KubeletCgroupsName默认为空
		// "kubelet"
		statsapi.SystemContainerKubelet: {name: nodeConfig.KubeletCgroupsName, forceStatsUpdate: false, startTime: sp.kubeletCreationTime},
		// RuntimeCgroupsName默认为空
		// "runtime"
		statsapi.SystemContainerRuntime: {name: nodeConfig.RuntimeCgroupsName, forceStatsUpdate: false},
		// SystemCgroupsName默认为空
		// "misc"
		statsapi.SystemContainerMisc:    {name: nodeConfig.SystemCgroupsName, forceStatsUpdate: false},
		// systemd为cgroup driver，默认返回为"/kubepods.slice"
		// cgroupfs为cgroup driver，则返回"/kubepods"
		// "pods"
		statsapi.SystemContainerPods:    {name: sp.provider.GetPodCgroupRoot(), forceStatsUpdate: updateStats},
	}
	for sys, cont := range systemContainers {
		// skip if cgroup name is undefined (not all system containers are required)
		if cont.name == "" {
			continue
		}
		// 返回cont.name容器最近的cpu和memory的使用情况，Accelerators状态（gpu）、UserDefinedMetrics（kubelet没有）、最近的网卡状态
		s, _, err := sp.provider.GetCgroupStats(cont.name, cont.forceStatsUpdate)
		if err != nil {
			klog.Errorf("Failed to get system container stats for %q: %v", cont.name, err)
			continue
		}
		// System containers don't have a filesystem associated with them.
		s.Logs, s.Rootfs = nil, nil
		s.Name = sys

		// if we know the start time of a system container, use that instead of the start time provided by cAdvisor
		if !cont.startTime.IsZero() {
			s.StartTime = cont.startTime
		}
		stats = append(stats, *s)
	}

	return stats
}

// 返回nodeConfig里的KubeletCgroupsName、RuntimeCgroupsName、SystemCgroupsName和pod cgroup root（比如"/kubepods.slice"）
// 如果上面类别中在nodeConfig未定义或为""，则忽略
// 这些container最近的cpu和memory的使用情况，并将Name（cgroup路径）改为对应的类型名
func (sp *summaryProviderImpl) GetSystemContainersCPUAndMemoryStats(nodeConfig cm.NodeConfig, podStats []statsapi.PodStats, updateStats bool) (stats []statsapi.ContainerStats) {
	systemContainers := map[string]struct {
		name             string
		forceStatsUpdate bool
		startTime        metav1.Time
	}{
		// KubeletCgroupsName默认为空
		// "kubelet"
		statsapi.SystemContainerKubelet: {name: nodeConfig.KubeletCgroupsName, forceStatsUpdate: false, startTime: sp.kubeletCreationTime},
		// RuntimeCgroupsName默认为空
		// "runtime"
		statsapi.SystemContainerRuntime: {name: nodeConfig.RuntimeCgroupsName, forceStatsUpdate: false},
		// SystemCgroupsName默认为空
		// "misc"
		statsapi.SystemContainerMisc:    {name: nodeConfig.SystemCgroupsName, forceStatsUpdate: false},
		// systemd为cgroup driver，默认返回为"/kubepods.slice"
		// cgroupfs为cgroup driver，则返回"/kubepods"
		// "pods"
		statsapi.SystemContainerPods:    {name: sp.provider.GetPodCgroupRoot(), forceStatsUpdate: updateStats},
	}
	for sys, cont := range systemContainers {
		// skip if cgroup name is undefined (not all system containers are required)
		if cont.name == "" {
			continue
		}
		// 从cadvisor中，如果cont.forceStatsUpdate为true，则等待container的housekeeping（更新监控数据）完成后，否则从memoryCache直接获取。
		// 返回container的StartTime（创建时间）、最后的cpu和memory的使用情况
		s, err := sp.provider.GetCgroupCPUAndMemoryStats(cont.name, cont.forceStatsUpdate)
		if err != nil {
			klog.Errorf("Failed to get system container stats for %q: %v", cont.name, err)
			continue
		}
		s.Name = sys

		// if we know the start time of a system container, use that instead of the start time provided by cAdvisor
		if !cont.startTime.IsZero() {
			s.StartTime = cont.startTime
		}
		stats = append(stats, *s)
	}

	return stats
}
