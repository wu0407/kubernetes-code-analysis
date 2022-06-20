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

package eviction

import (
	"fmt"
	"time"

	"k8s.io/klog"

	"k8s.io/apimachinery/pkg/api/resource"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
)

const (
	memoryUsageAttribute = "memory.usage_in_bytes"
	// this prevents constantly updating the memcg notifier if synchronize
	// is run frequently.
	notifierRefreshInterval = 10 * time.Second
)

type memoryThresholdNotifier struct {
	threshold  evictionapi.Threshold
	cgroupPath string
	events     chan struct{}
	factory    NotifierFactory
	handler    func(string)
	notifier   CgroupNotifier
}

var _ ThresholdNotifier = &memoryThresholdNotifier{}

// NewMemoryThresholdNotifier creates a ThresholdNotifier which is designed to respond to the given threshold.
// UpdateThreshold must be called once before the threshold will be active.
func NewMemoryThresholdNotifier(threshold evictionapi.Threshold, cgroupRoot string, factory NotifierFactory, handler func(string)) (ThresholdNotifier, error) {
	// Mounts：获得所有cgroup子系统的Mountpoint、Root、对应的Subsystems列表
	// MountPoints：子系统和挂载点信息
	cgroups, err := cm.GetCgroupSubsystems()
	if err != nil {
		return nil, err
	}
	cgpath, found := cgroups.MountPoints["memory"]
	if !found || len(cgpath) == 0 {
		return nil, fmt.Errorf("memory cgroup mount point not found")
	}
	// 阈值类型是"allocatableMemory.available"
	if isAllocatableEvictionThreshold(threshold) {
		// for allocatable thresholds, point the cgroup notifier at the allocatable cgroup
		cgpath += cgroupRoot
	}
	return &memoryThresholdNotifier{
		threshold:  threshold,
		cgroupPath: cgpath,
		events:     make(chan struct{}),
		handler:    handler,
		factory:    factory,
	}, nil
}

// 消费m.events里的消息，执行m.handler
func (m *memoryThresholdNotifier) Start() {
	klog.Infof("eviction manager: created %s", m.Description())
	for range m.events {
		m.handler(fmt.Sprintf("eviction manager: %s crossed", m.Description()))
	}
}

// memory阈值类型是"allocatableMemory.available"，监听"/sys/fs/cgroup/memory/kubepods.slice"下"memory.usage_in_bytes"文件
// memory阈值类型是"memory.available"，监听"/sys/fs/cgroup/memory"下"memory.usage_in_bytes"文件
// 启动一个goroutine，利用epoll和cgroup目录（/sys/fs/cgroup/memory或/sys/fs/cgroup/memory/kubepods.slice）下cgroup.event_control，循环每次等待10s时间，如果有期望事件发生，则发送消息给m.events
// "memory.usage_in_bytes"文件内容的值，在达到capacity - eviction_hard + inactive_file值时候，会生成事件。
// capacity和inactive_file是实时值，所以这里会停止老的goroutine，创建新的goroutine使用新计算出阈值
func (m *memoryThresholdNotifier) UpdateThreshold(summary *statsapi.Summary) error {
	memoryStats := summary.Node.Memory
	// 阈值类型是"allocatableMemory.available"，则memoryStats为cgroup "/kubepods.slice"的监控数据
	if isAllocatableEvictionThreshold(m.threshold) {
		// 从summary.Node.SystemContainers获得name为"pods"的statsapi.ContainerStats（默认为"/kubepods.slice"容器的监控数据）
		allocatableContainer, err := getSysContainer(summary.Node.SystemContainers, statsapi.SystemContainerPods)
		if err != nil {
			return err
		}
		memoryStats = allocatableContainer.Memory
	}
	if memoryStats == nil || memoryStats.UsageBytes == nil || memoryStats.WorkingSetBytes == nil || memoryStats.AvailableBytes == nil {
		return fmt.Errorf("summary was incomplete.  Expected MemoryStats and all subfields to be non-nil, but got %+v", memoryStats)
	}
	// Set threshold on usage to capacity - eviction_hard + inactive_file,
	// since we want to be notified when working_set = capacity - eviction_hard
	// 使用working_set来衡量是否达到阈值，working_set =（UsageBytes - inactive_file）= capacity - eviction_hard，但是系统是使用UsageBytes，所以当UsageBytes= capacity - eviction_hard + inactive_file
	// 
	// inactiveFile为UsageBytes减去WorkingSetBytes，因为在vendor\github.com\google\cadvisor\container\libcontainer\handler.go里WorkingSet就等于UsageBytes减去inactiveFile
	inactiveFile := resource.NewQuantity(int64(*memoryStats.UsageBytes-*memoryStats.WorkingSetBytes), resource.BinarySI)
	// AvailableBytes为limit减去workingSetBytes，所以limit（capacity）为AvailableBytes加WorkingSetBytes
	capacity := resource.NewQuantity(int64(*memoryStats.AvailableBytes+*memoryStats.WorkingSetBytes), resource.BinarySI)
	// 保留多少memory
	evictionThresholdQuantity := evictionapi.GetThresholdQuantity(m.threshold.Value, capacity)
	memcgThreshold := capacity.DeepCopy()
	memcgThreshold.Sub(*evictionThresholdQuantity)
	memcgThreshold.Add(*inactiveFile)

	klog.V(3).Infof("eviction manager: setting %s to %s\n", m.Description(), memcgThreshold.String())
	if m.notifier != nil {
		m.notifier.Stop()
	}
	// 监听"/sys/fs/cgroup/memory/kubepods.slice"下"memory.usage_in_bytes"文件内容的值大小，超过了memcgThreshold.Value()事件
	newNotifier, err := m.factory.NewCgroupNotifier(m.cgroupPath, memoryUsageAttribute, memcgThreshold.Value())
	if err != nil {
		return err
	}
	m.notifier = newNotifier
	// 启动一个goroutine，利用epoll，循环每次等待10s时间，如果有期望事件发生，则发送消息给m.events
	go m.notifier.Start(m.events)
	return nil
}

func (m *memoryThresholdNotifier) Description() string {
	var hard, allocatable string
	// m.threshold的GracePeriod为0
	if isHardEvictionThreshold(m.threshold) {
		hard = "hard "
	} else {
		hard = "soft "
	}
	// 阈值类型是不是"allocatableMemory.available"
	if isAllocatableEvictionThreshold(m.threshold) {
		allocatable = "allocatable "
	}
	return fmt.Sprintf("%s%smemory eviction threshold", hard, allocatable)
}

var _ NotifierFactory = &CgroupNotifierFactory{}

// CgroupNotifierFactory knows how to make CgroupNotifiers which integrate with the kernel
type CgroupNotifierFactory struct{}

// NewCgroupNotifier implements the NotifierFactory interface
func (n *CgroupNotifierFactory) NewCgroupNotifier(path, attribute string, threshold int64) (CgroupNotifier, error) {
	return NewCgroupNotifier(path, attribute, threshold)
}
