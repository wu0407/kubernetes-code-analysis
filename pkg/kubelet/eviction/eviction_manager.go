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

package eviction

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"k8s.io/klog"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	apiv1resource "k8s.io/kubernetes/pkg/api/v1/resource"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/features"
	statsapi "k8s.io/kubernetes/pkg/kubelet/apis/stats/v1alpha1"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

const (
	podCleanupTimeout  = 30 * time.Second
	podCleanupPollFreq = time.Second
)

const (
	// signalEphemeralContainerFsLimit is amount of storage available on filesystem requested by the container
	signalEphemeralContainerFsLimit string = "ephemeralcontainerfs.limit"
	// signalEphemeralPodFsLimit is amount of storage available on filesystem requested by the pod
	signalEphemeralPodFsLimit string = "ephemeralpodfs.limit"
	// signalEmptyDirFsLimit is amount of storage available on filesystem requested by an emptyDir
	signalEmptyDirFsLimit string = "emptydirfs.limit"
)

// managerImpl implements Manager
type managerImpl struct {
	//  used to track time
	clock clock.Clock
	// config is how the manager is configured
	config Config
	// the function to invoke to kill a pod
	killPodFunc KillPodFunc
	// the function to get the mirror pod by a given statid pod
	mirrorPodFunc MirrorPodFunc
	// the interface that knows how to do image gc
	imageGC ImageGC
	// the interface that knows how to do container gc
	containerGC ContainerGC
	// protects access to internal state
	sync.RWMutex
	// node conditions are the set of conditions present
	nodeConditions []v1.NodeConditionType
	// captures when a node condition was last observed based on a threshold being met
	nodeConditionsLastObservedAt nodeConditionsObservedAt
	// nodeRef is a reference to the node
	nodeRef *v1.ObjectReference
	// used to record events about the node
	recorder record.EventRecorder
	// used to measure usage stats on system
	summaryProvider stats.SummaryProvider
	// records when a threshold was first observed
	thresholdsFirstObservedAt thresholdsObservedAt
	// records the set of thresholds that have been met (including graceperiod) but not yet resolved
	thresholdsMet []evictionapi.Threshold
	// signalToRankFunc maps a resource to ranking function for that resource.
	signalToRankFunc map[evictionapi.Signal]rankFunc
	// signalToNodeReclaimFuncs maps a resource to an ordered list of functions that know how to reclaim that resource.
	signalToNodeReclaimFuncs map[evictionapi.Signal]nodeReclaimFuncs
	// last observations from synchronize
	lastObservations signalObservations
	// dedicatedImageFs indicates if imagefs is on a separate device from the rootfs
	dedicatedImageFs *bool
	// thresholdNotifiers is a list of memory threshold notifiers which each notify for a memory eviction threshold
	thresholdNotifiers []ThresholdNotifier
	// thresholdsLastUpdated is the last time the thresholdNotifiers were updated.
	thresholdsLastUpdated time.Time
	// etcHostsPath is a function that will get the etc-hosts file's path for a pod given its UID
	etcHostsPath func(podUID types.UID) string
}

// ensure it implements the required interface
var _ Manager = &managerImpl{}

// NewManager returns a configured Manager and an associated admission handler to enforce eviction configuration.
func NewManager(
	summaryProvider stats.SummaryProvider,
	config Config,
	killPodFunc KillPodFunc,
	mirrorPodFunc MirrorPodFunc,
	imageGC ImageGC,
	containerGC ContainerGC,
	recorder record.EventRecorder,
	nodeRef *v1.ObjectReference,
	clock clock.Clock,
	etcHostsPath func(types.UID) string,
) (Manager, lifecycle.PodAdmitHandler) {
	manager := &managerImpl{
		clock:                        clock,
		killPodFunc:                  killPodFunc,
		mirrorPodFunc:                mirrorPodFunc,
		imageGC:                      imageGC,
		containerGC:                  containerGC,
		config:                       config,
		recorder:                     recorder,
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
		dedicatedImageFs:             nil,
		thresholdNotifiers:           []ThresholdNotifier{},
		etcHostsPath:                 etcHostsPath,
	}
	return manager, manager
}

// Admit rejects a pod if its not safe to admit for node stability.
// 通过admit条件（下列条件满足一个）
// 1. 所有资源没有达到evict阈值，则直接通过admit
// 2. pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，则直接通过admit
// 3. 只有一个"MemoryPressure" condition，qos不为"BestEffort"的pod，则直接通过admit
// 4. 只有一个"MemoryPressure" condition，如果"BestEffort"的pod能够容忍taint "node.kubernetes.io/memory-pressure"、effect为"NoSchedule"，则直接通过admit
func (m *managerImpl) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	m.RLock()
	defer m.RUnlock()
	// 所有资源没有达到evict阈值，则直接通过admit
	if len(m.nodeConditions) == 0 {
		return lifecycle.PodAdmitResult{Admit: true}
	}
	// Admit Critical pods even under resource pressure since they are required for system stability.
	// https://github.com/kubernetes/kubernetes/issues/40573 has more details.
	// pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，则直接通过admit
	if kubelettypes.IsCriticalPod(attrs.Pod) {
		return lifecycle.PodAdmitResult{Admit: true}
	}

	// Conditions other than memory pressure reject all pods
	// 是否只有一个"MemoryPressure" condition
	nodeOnlyHasMemoryPressureCondition := hasNodeCondition(m.nodeConditions, v1.NodeMemoryPressure) && len(m.nodeConditions) == 1
	// 只有一个"MemoryPressure" condition，qos不为"BestEffort"的pod，则直接通过admit
	if nodeOnlyHasMemoryPressureCondition {
		notBestEffort := v1.PodQOSBestEffort != v1qos.GetPodQOS(attrs.Pod)
		// qos不为"BestEffort"的pod，则直接通过admit
		if notBestEffort {
			return lifecycle.PodAdmitResult{Admit: true}
		}

		// When node has memory pressure, check BestEffort Pod's toleration:
		// admit it if tolerates memory pressure taint, fail for other tolerations, e.g. DiskPressure.
		// 如果"BestEffort"的pod能够容忍taint "node.kubernetes.io/memory-pressure"、effect为"NoSchedule"，则直接通过admit
		if v1helper.TolerationsTolerateTaint(attrs.Pod.Spec.Tolerations, &v1.Taint{
			Key:    v1.TaintNodeMemoryPressure,
			Effect: v1.TaintEffectNoSchedule,
		}) {
			return lifecycle.PodAdmitResult{Admit: true}
		}
	}

	// reject pods when under memory pressure (if pod is best effort), or if under disk pressure.
	// 其他情况"BestEffort"的pod，admit不通过
	klog.Warningf("Failed to admit pod %s - node has conditions: %v", format.Pod(attrs.Pod), m.nodeConditions)
	return lifecycle.PodAdmitResult{
		Admit:   false,
		Reason:  Reason,
		Message: fmt.Sprintf(nodeConditionMessageFmt, m.nodeConditions),
	}
}

// Start starts the control loop to observe and response to low compute resources.
// 1. 如果启用KernelMemcgNotification，则为"memory.available"或"allocatableMemory.available"阈值类型，各创建一个notifier，消费notifier.events里的消息，添加notifier到m.thresholdNotifiers，执行第二个步骤
// 2. 启动一个goroutine，每隔monitoringInterval时间
// 2.1. 执行检测是否达到驱逐阈值，执行pod的驱逐。完成驱逐后，等待30s，每1s执行检测所有pod资源是否回收完毕，如果都回收完毕则直接返回。
// 2.2. 如果启用KernelMemcgNotification，每个notifier，停止老的goroutine并启动一个goroutine，利用epoll和cgroup目录（/sys/fs/cgroup/memory或/sys/fs/cgroup/memory/kubepods.slice）下cgroup.event_control，循环每次等待10s时间，如果有期望事件发生，则发送消息给m.thresholdNotifiers[*].events
// 2.3. memory阈值类型是"allocatableMemory.available"，监听"/sys/fs/cgroup/memory/kubepods.slice"下"memory.usage_in_bytes"文件
// 2.4. memory阈值类型是"memory.available"，监听"/sys/fs/cgroup/memory"下"memory.usage_in_bytes"文件
func (m *managerImpl) Start(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc, podCleanedUpFunc PodCleanedUpFunc, monitoringInterval time.Duration) {
	thresholdHandler := func(message string) {
		klog.Infof(message)
		// 1. 为不同的evictionapi.Signal生成排序函数，为不同的evictionapi.Signal生成资源回收函数
		// 2. 从m.summaryProvider中获得node状态和所有pod的状态监控信息
		// 3. 如果启用m.config.KernelMemcgNotification，且离最新阈值更新时间已经过去10s，则执行阈值更新
		//    memory阈值类型是"allocatableMemory.available"，监听"/sys/fs/cgroup/memory/kubepods.slice"下"memory.usage_in_bytes"文件
		//    memory阈值类型是"memory.available"，监听"/sys/fs/cgroup/memory"下"memory.usage_in_bytes"文件
		//    停止老的goroutine，启动一个goroutine，利用epoll和cgroup目录（/sys/fs/cgroup/memory或/sys/fs/cgroup/memory/kubepods.slice）下cgroup.event_control，循环每次等待10s时间，如果有期望事件发生，则发送消息给m.thresholdNotifiers[*].events
		//    "memory.usage_in_bytes"文件内容的值，在达到capacity - eviction_hard + inactive_file值时候，会生成事件。
		// 4. 过滤出所有需要保留资源值大于可用资源值（到达evict阈值）且已经过了gracePeriod的类型的阈值类型（包括之前已经达到阈值且已经过了gracePeriod的类型，现在还是超过阈值）
		// 5. pod层面资源evicted，发生驱逐的pod列表不为空，则返回
		//    所有发生驱逐的pod列表（满足下面三种任意一个条件）
		//    5.1. pod里EmptyDir类型的volume的使用值超出了设置的限制（有显式的设置pod.Spec.Volumes[i].VolumeSource.EmptyDir.SizeLimit）
		//    5.2. pod里所有container的（logs或rootfs加logs）的总磁盘使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量，或加上pod的etcHost文件大小，大于pod总的"ephemeral-storage"类型资源limit，进行驱逐且优雅停止时间为0
		//    5.3. pod里所有container里只要有一个container磁盘使用量大于container的"ephemeral-storage"类型资源limit，则进行pod驱逐而且优雅停止时间为0
		//    镜像保存的磁盘设备和kubelet root目录所在挂载设备一致，则container磁盘使用量为logs的磁盘使用量加上rootfs的磁盘使用量
		//    镜像保存的磁盘设备和kubelet root目录所在挂载设备不一致，则container磁盘使用量为logs的磁盘使用量
		// 6. node层面资源evicted
		//    6.1. 从所有达到阈值且经过gracePeriod的类型的阈值类型里取第一个阈值类型，驱逐类型为"memory.available"和"allocatableMemory.available"排在前面，在signalToResource中没有资源类型排在最后面
		//    6.2. 执行这个阈值类型对应资源回收函数，尝试回收对应资源。执行回收后，未达到驱逐阈值则直接返回
		//    6.3. 执行阈值类型对应的pod排序函数，对pod进行排序
		//    6.4. 驱逐第一个非（pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000）pod
		m.synchronize(diskInfoProvider, podFunc)
	}
	// m.config.KernelMemcgNotification默认为false
	if m.config.KernelMemcgNotification {
		for _, threshold := range m.config.Thresholds {
			// "memory.available"或"allocatableMemory.available"
			if threshold.Signal == evictionapi.SignalMemoryAvailable || threshold.Signal == evictionapi.SignalAllocatableMemoryAvailable {
				// m.config.PodCgroupRoot默认为"/kubepods.slice"(cgroup driver为systemd)
				notifier, err := NewMemoryThresholdNotifier(threshold, m.config.PodCgroupRoot, &CgroupNotifierFactory{}, thresholdHandler)
				if err != nil {
					klog.Warningf("eviction manager: failed to create memory threshold notifier: %v", err)
				} else {
					// 启动goroutine，消费notifier.events里的消息，执行thresholdHandler（即m.synchronize）
					// notifier.events里有消息代表cgroup里的"memory.usage_in_bytes"文件内容的值，超过了阈值
					go notifier.Start()
					m.thresholdNotifiers = append(m.thresholdNotifiers, notifier)
				}
			}
		}
	}
	// start the eviction manager monitoring
	go func() {
		for {
			if evictedPods := m.synchronize(diskInfoProvider, podFunc); evictedPods != nil {
				klog.Infof("eviction manager: pods %s evicted, waiting for pod to be cleaned up", format.Pods(evictedPods))
				// 等待30s，每1s执行检测所有pod资源是否回收完毕，如果都回收完毕则直接返回
				m.waitForPodsCleanup(podCleanedUpFunc, evictedPods)
			} else {
				time.Sleep(monitoringInterval)
			}
		}
	}()
}

// IsUnderMemoryPressure returns true if the node is under memory pressure.
func (m *managerImpl) IsUnderMemoryPressure() bool {
	m.RLock()
	defer m.RUnlock()
	return hasNodeCondition(m.nodeConditions, v1.NodeMemoryPressure)
}

// IsUnderDiskPressure returns true if the node is under disk pressure.
func (m *managerImpl) IsUnderDiskPressure() bool {
	m.RLock()
	defer m.RUnlock()
	return hasNodeCondition(m.nodeConditions, v1.NodeDiskPressure)
}

// IsUnderPIDPressure returns true if the node is under PID pressure.
func (m *managerImpl) IsUnderPIDPressure() bool {
	m.RLock()
	defer m.RUnlock()
	return hasNodeCondition(m.nodeConditions, v1.NodePIDPressure)
}

// synchronize is the main control loop that enforces eviction thresholds.
// Returns the pod that was killed, or nil if no pod was killed.
// 1. 为不同的evictionapi.Signal生成排序函数，为不同的evictionapi.Signal生成资源回收函数
// 2. 从m.summaryProvider中获得node状态和所有pod的状态监控信息
// 3. 如果启用m.config.KernelMemcgNotification，且离最新阈值更新时间已经过去10s，则执行阈值更新
//    memory阈值类型是"allocatableMemory.available"，监听"/sys/fs/cgroup/memory/kubepods.slice"下"memory.usage_in_bytes"文件
//    memory阈值类型是"memory.available"，监听"/sys/fs/cgroup/memory"下"memory.usage_in_bytes"文件
//    停止老的goroutine，启动一个goroutine，利用epoll和cgroup目录（/sys/fs/cgroup/memory或/sys/fs/cgroup/memory/kubepods.slice）下cgroup.event_control，循环每次等待10s时间，如果有期望事件发生，则发送消息给m.thresholdNotifiers.events
//    "memory.usage_in_bytes"文件内容的值，在达到capacity - eviction_hard + inactive_file值时候，会生成事件。
// 4. 过滤出所有需要保留资源值大于可用资源值（到达evict阈值）且已经过了gracePeriod的类型的阈值类型（包括之前已经达到阈值且已经过了gracePeriod的类型，现在还是超过阈值）
// 5. pod层面资源evicted，发生驱逐的pod列表不为空，则返回
//    所有发生驱逐的pod列表（满足下面三种任意一个条件）
//    5.1. pod里EmptyDir类型的volume的使用值超出了设置的限制（有显式的设置pod.Spec.Volumes[i].VolumeSource.EmptyDir.SizeLimit）
//    5.2. pod里所有container的（logs或rootfs加logs）的总磁盘使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量，或加上pod的etcHost文件大小，大于pod总的"ephemeral-storage"类型资源limit，进行驱逐且优雅停止时间为0
//    5.3. pod里所有container里只要有一个container磁盘使用量大于container的"ephemeral-storage"类型资源limit，则进行pod驱逐而且优雅停止时间为0
//    镜像保存的磁盘设备和kubelet root目录所在挂载设备一致，则container磁盘使用量为logs的磁盘使用量加上rootfs的磁盘使用量
//    镜像保存的磁盘设备和kubelet root目录所在挂载设备不一致，则container磁盘使用量为logs的磁盘使用量
// 6. node层面资源evicted
//    6.1. 从所有达到阈值且经过gracePeriod的类型的阈值类型里取第一个阈值类型，驱逐类型为"memory.available"和"allocatableMemory.available"排在前面，在signalToResource中没有资源类型排在最后面
//    6.2. 执行这个阈值类型对应资源回收函数，尝试回收对应资源。执行回收后，未达到驱逐阈值则直接返回
//    6.3. 执行阈值类型对应的pod排序函数，对pod进行排序
//    6.4. 驱逐第一个非（pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000）pod
func (m *managerImpl) synchronize(diskInfoProvider DiskInfoProvider, podFunc ActivePodsFunc) []*v1.Pod {
	// if we have nothing to do, just return
	thresholds := m.config.Thresholds
	// 没有设置驱逐阈值且没有启用"LocalStorageCapacityIsolation"，直接返回nil
	if len(thresholds) == 0 && !utilfeature.DefaultFeatureGate.Enabled(features.LocalStorageCapacityIsolation) {
		return nil
	}

	klog.V(3).Infof("eviction manager: synchronize housekeeping")
	// build the ranking functions (if not yet known)
	// TODO: have a function in cadvisor that lets us know if global housekeeping has completed
	if m.dedicatedImageFs == nil {
		// 镜像保存的磁盘设备是否kubelet root目录所在挂载设备不一致
		hasImageFs, ok := diskInfoProvider.HasDedicatedImageFs()
		if ok != nil {
			return nil
		}
		// 设置m.dedicatedImageFs
		m.dedicatedImageFs = &hasImageFs
		// 为不同的evictionapi.Signal生成排序函数
		m.signalToRankFunc = buildSignalToRankFunc(hasImageFs)
		// 如果有专门的image fs，则"nodefs.available"和"nodefs.inodesFree"没有资源回收函数，"imagefs.available"和"imagefs.inodesFree"回收函数为containerGC.DeleteAllUnusedContainers（移除所有未使用容器）和imageGC.DeleteUnusedImages（删除无用的镜像）
		// 如果没有专门的image fs，则"nodefs.available"和"nodefs.inodesFree"和"imagefs.available"和"imagefs.inodesFree"回收函数为containerGC.DeleteAllUnusedContainers（移除所有未使用容器）和imageGC.DeleteUnusedImages（删除无用的镜像）
		m.signalToNodeReclaimFuncs = buildSignalToNodeReclaimFuncs(m.imageGC, m.containerGC, hasImageFs)
	}

	activePods := podFunc()
	updateStats := true
	// 返回node状态和所有pod的状态监控信息
	summary, err := m.summaryProvider.Get(updateStats)
	if err != nil {
		klog.Errorf("eviction manager: failed to get summary stats: %v", err)
		return nil
	}

	// 离最新更新时间已经过去10s
	if m.clock.Since(m.thresholdsLastUpdated) > notifierRefreshInterval {
		// m.thresholdsLastUpdated更新为现在 
		m.thresholdsLastUpdated = m.clock.Now()
		for _, notifier := range m.thresholdNotifiers {
			// memory阈值类型是"allocatableMemory.available"，监听"/sys/fs/cgroup/memory/kubepods.slice"下"memory.usage_in_bytes"文件
			// memory阈值类型是"memory.available"，监听"/sys/fs/cgroup/memory"下"memory.usage_in_bytes"文件
			// 启动一个goroutine，利用epoll和cgroup目录（/sys/fs/cgroup/memory或/sys/fs/cgroup/memory/kubepods.slice）下cgroup.event_control，循环每次等待10s时间，如果有期望事件发生，则发送消息给m.thresholdNotifiers.events
			// "memory.usage_in_bytes"文件内容的值，在达到capacity - eviction_hard + inactive_file值时候，会生成事件。
			// capacity和inactive_file是实时值，所以这里会停止老的goroutine，创建新的goroutine使用新计算出阈值(新的capacity和inactive_file)
			if err := notifier.UpdateThreshold(summary); err != nil {
				klog.Warningf("eviction manager: failed to update %s: %v", notifier.Description(), err)
			}
		}
	}

	// make observations and get a function to derive pod usage stats relative to those observations.
	// 从summary.Node获得各个阈值类型的available值和capacity值和time
	// 利用summary.Pods生成通过pod查找statsapi.PodStats的函数
	observations, statsFunc := makeSignalObservations(summary)
	debugLogObservations("observations", observations)

	// determine the set of thresholds met independent of grace period
	// 返回所有需要保留资源值大于可用资源值（到达evict阈值）的阈值类型
	thresholds = thresholdsMet(thresholds, observations, false)
	debugLogThresholdsWithObservation("thresholds - ignoring grace period", thresholds, observations)

	// determine the set of thresholds previously met that have not yet satisfied the associated min-reclaim
	// m.thresholdsMet之前已经达到阈值且已经过了gracePeriod的类型，现在还是超过阈值，则跟现在到达阈值的列表thresholds进行聚合去掉重复的。
	if len(m.thresholdsMet) > 0 {
		// 之前已经达到阈值且已经过了gracePeriod的类型，现在还是超过阈值
		// thresholdsMet函数，返回所有需要保留资源值大于可用资源值（到达evict阈值）的阈值类型
		// 如果阈值类型里定义了MinReclaim，则保留资源需要加上MinReclaim
		thresholdsNotYetResolved := thresholdsMet(m.thresholdsMet, observations, true)
		// 将thresholds和thresholdsNotYetResolved进行合并，去掉重复的。
		thresholds = mergeThresholds(thresholds, thresholdsNotYetResolved)
	}
	debugLogThresholdsWithObservation("thresholds - reclaim not satisfied", thresholds, observations)

	// track when a threshold was first observed
	now := m.clock.Now()
	// thresholds有threshold不在m.thresholdsFirstObservedAt里，则添加到result且值为now。如果在lastObservedAt里，则添加到result且值为lastObservedAt里的值，并返回result
	// 都在m.thresholdsFirstObservedAt和thresholds，以m.thresholdsFirstObservedAt为准，m.thresholdsFirstObservedAt不在thresholds里的不会记录在result里
	thresholdsFirstObservedAt := thresholdsFirstObservedAt(thresholds, m.thresholdsFirstObservedAt, now)

	// the set of node conditions that are triggered by currently observed thresholds
	// 获得thresholds里signal（阈值类型）对应的nodeCondition
	nodeConditions := nodeConditions(thresholds)
	if len(nodeConditions) > 0 {
		klog.V(3).Infof("eviction manager: node conditions - observed: %v", nodeConditions)
	}

	// track when a node condition was last observed
	// 将nodeConditions添加到results里且value为now， m.nodeConditionsLastObservedAt里的condition不在results里，则添加到result里，返回result
	// 即将nodeConditions和m.nodeConditionsLastObservedAt进行聚合，并且以nodeConditions里为准
	nodeConditionsLastObservedAt := nodeConditionsLastObservedAt(nodeConditions, m.nodeConditionsLastObservedAt, now)

	// node conditions report true if it has been observed within the transition period window
	// 返回nodeCondition发生时间未超过m.config.PressureTransitionPeriod的condition列表
	// 比如某一类型的condition已经没有了，但是还是在m.nodeConditionsLastObservedAt里（老的condition依然会存在nodeConditionsLastObservedAt里），但是它的发现时间是没有更新的，所以这里要判断在m.config.PressureTransitionPeriod里的condition，才是有效的。即这个类型的condition消除的时间最长为m.config.PressureTransitionPeriod，这个是防止condition恢复后，又触发evict，condition状态出现抖动不稳定。
	nodeConditions = nodeConditionsObservedSince(nodeConditionsLastObservedAt, m.config.PressureTransitionPeriod, now)
	if len(nodeConditions) > 0 {
		klog.V(3).Infof("eviction manager: node conditions - transition period not met: %v", nodeConditions)
	}

	// determine the set of thresholds we need to drive eviction behavior (i.e. all grace periods are met)
	// 所有达到阈值的类型发生时间超过阈值的GracePeriod的阈值类型
	thresholds = thresholdsMetGracePeriod(thresholdsFirstObservedAt, now)
	debugLogThresholdsWithObservation("thresholds - grace periods satisfied", thresholds, observations)

	// update internal state
	m.Lock()
	// 用于admit
	m.nodeConditions = nodeConditions
	m.thresholdsFirstObservedAt = thresholdsFirstObservedAt
	// 用于admit
	m.nodeConditionsLastObservedAt = nodeConditionsLastObservedAt
	m.thresholdsMet = thresholds

	// determine the set of thresholds whose stats have been updated since the last sync
	// thresholds里所有，阈值类型有监控数据，且监控数据时间或数据发生变化的驱逐类型
	thresholds = thresholdsUpdatedStats(thresholds, observations, m.lastObservations)
	debugLogThresholdsWithObservation("thresholds - updated stats", thresholds, observations)

	m.lastObservations = observations
	m.Unlock()

	// pod层面资源evicted
	// evict pods if there is a resource usage violation from local volume temporary storage
	// If eviction happens in localStorageEviction function, skip the rest of eviction action
	// 启用"LocalStorageCapacityIsolation"功能，且发生pod磁盘资源超出限制的驱逐，则返回所有被驱逐pod
	if utilfeature.DefaultFeatureGate.Enabled(features.LocalStorageCapacityIsolation) {
		// 返回所有发生驱逐的pod列表（满足下面三种任意一个条件）
		// 1. pod里EmptyDir类型的volume的使用值超出了设置的限制（有显式的设置pod.Spec.Volumes[i].VolumeSource.EmptyDir.SizeLimit）
		// 2. pod里所有container的（logs或rootfs加logs）的总磁盘使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量，或加上pod的etcHost文件大小，大于pod总的"ephemeral-storage"类型资源limit，进行驱逐且优雅停止时间为0
		// 3. pod里所有container里只要有一个container磁盘使用量大于container的"ephemeral-storage"类型资源limit，则进行pod驱逐而且优雅停止时间为0
		// 镜像保存的磁盘设备和kubelet root目录所在挂载设备一致，则container磁盘使用量为logs的磁盘使用量加上rootfs的磁盘使用量
		// 镜像保存的磁盘设备和kubelet root目录所在挂载设备不一致，则container磁盘使用量为logs的磁盘使用量
		if evictedPods := m.localStorageEviction(summary, activePods); len(evictedPods) > 0 {
			return evictedPods
		}
	}

	// node层面资源evicted

	// 达到阈值的类型发生时间超过阈值的GracePeriod，且监控数据时间或数据发生变化的阈值类型为空，直接返回
	if len(thresholds) == 0 {
		klog.V(3).Infof("eviction manager: no resources are starved")
		return nil
	}

	// 有达到阈值的类型发生时间超过阈值的GracePeriod，且监控数据时间或数据发生变化的阈值类型

	// rank the thresholds by eviction priority
	// 驱逐类型为"memory.available"和"allocatableMemory.available"排在前面，在signalToResource中没有资源类型排在最后面
	sort.Sort(byEvictionPriority(thresholds))
	// 返回thresholds中第一个在signalToResource（Signal与对应相关资源）、对应的资源名称和是否在signalToResource中
	thresholdToReclaim, resourceToReclaim, foundAny := getReclaimableThreshold(thresholds)
	if !foundAny {
		return nil
	}
	klog.Warningf("eviction manager: attempting to reclaim %v", resourceToReclaim)

	// record an event about the resources we are now attempting to reclaim via eviction
	m.recorder.Eventf(m.nodeRef, v1.EventTypeWarning, "EvictionThresholdMet", "Attempting to reclaim %s", resourceToReclaim)

	// check if there are node-level resources we can reclaim to reduce pressure before evicting end-user pods.
	// 执行thresholdToReclaim.Signal对应的回收资源函数，执行后所有需要保留资源值小于可用资源值（未达到驱逐阈值），代表成功回收资源，并且从达到阈值变成未达到驱逐阈值，直接返回
	if m.reclaimNodeLevelResources(thresholdToReclaim.Signal, resourceToReclaim) {
		klog.Infof("eviction manager: able to reduce %v pressure without evicting pods.", resourceToReclaim)
		return nil
	}

	// 尝试回收资源后，还是达到了资源驱逐阈值
	klog.Infof("eviction manager: must evict pod(s) to reclaim %v", resourceToReclaim)

	// rank the pods for eviction
	rank, ok := m.signalToRankFunc[thresholdToReclaim.Signal]
	if !ok {
		klog.Errorf("eviction manager: no ranking function for signal %s", thresholdToReclaim.Signal)
		return nil
	}

	// the only candidates viable for eviction are those pods that had anything running.
	if len(activePods) == 0 {
		klog.Errorf("eviction manager: eviction thresholds have been met, but no pods are active to evict")
		return nil
	}

	// rank the running pods for eviction for the specified resource
	// 根据之前设置的算法，对pod进行排序
	rank(activePods, statsFunc)

	klog.Infof("eviction manager: pods ranked for eviction: %s", format.Pods(activePods))

	//record age of metrics for met thresholds that we are using for evictions.
	for _, t := range thresholds {
		timeObserved := observations[t.Signal].time
		if !timeObserved.IsZero() {
			metrics.EvictionStatsAge.WithLabelValues(string(t.Signal)).Observe(metrics.SinceInSeconds(timeObserved.Time))
		}
	}

	// we kill at most a single pod during each eviction interval
	for i := range activePods {
		pod := activePods[i]
		// 如果是hard evict，则gracePeriodOverride为0
		gracePeriodOverride := int64(0)
		// 如果是soft evict，则gracePeriodOverride为MaxPodGracePeriodSeconds
		if !isHardEvictionThreshold(thresholdToReclaim) {
			gracePeriodOverride = m.config.MaxPodGracePeriodSeconds
		}
		// 返回"The node was low on resource: {resourceToReclaim}. Container {container name} was using {usage}, which exceeds its request of {request}. "
		// annotations["offending_containers"]为所有使用量大于request的container
		// annotations["offending_containers_usage"]为所有使用量大于request的container的资源使用量
		// annotations["starved_resource"]为资源类型
		message, annotations := evictionMessage(resourceToReclaim, pod, statsFunc)
		// 对pod进行驱逐，pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，不进行驱逐
		// 利用pod worker机制来移除pod（停止pod里所有container和sandbox）
		if m.evictPod(pod, gracePeriodOverride, message, annotations) {
			metrics.Evictions.WithLabelValues(string(thresholdToReclaim.Signal)).Inc()
			return []*v1.Pod{pod}
		}
	}
	klog.Infof("eviction manager: unable to evict any pods from the node")
	return nil
}

// 等待30s，每1s执行检测所有pod资源是否回收完毕，如果都回收完毕则直接返回
func (m *managerImpl) waitForPodsCleanup(podCleanedUpFunc PodCleanedUpFunc, pods []*v1.Pod) {
	// 30s
	timeout := m.clock.NewTimer(podCleanupTimeout)
	defer timeout.Stop()
	// 1s
	ticker := m.clock.NewTicker(podCleanupPollFreq)
	defer ticker.Stop()
	for {
		select {
		case <-timeout.C():
			klog.Warningf("eviction manager: timed out waiting for pods %s to be cleaned up", format.Pods(pods))
			return
		case <-ticker.C():
			for i, pod := range pods {
				// 有pod资源未回收完毕，则等待下一个ticker
				if !podCleanedUpFunc(pod) {
					break
				}
				// 最后一个pod
				if i == len(pods)-1 {
					klog.Infof("eviction manager: pods %s successfully cleaned up", format.Pods(pods))
					return
				}
			}
		}
	}
}

// reclaimNodeLevelResources attempts to reclaim node level resources.  returns true if thresholds were satisfied and no pod eviction is required.
// 执行signalToReclaim对应的回收资源函数，返回执行后是否所有需要保留资源值小于可用资源值（未达到驱逐阈值）
func (m *managerImpl) reclaimNodeLevelResources(signalToReclaim evictionapi.Signal, resourceToReclaim v1.ResourceName) bool {
	nodeReclaimFuncs := m.signalToNodeReclaimFuncs[signalToReclaim]
	for _, nodeReclaimFunc := range nodeReclaimFuncs {
		// attempt to reclaim the pressured resource.
		// 执行回收资源函数
		if err := nodeReclaimFunc(); err != nil {
			klog.Warningf("eviction manager: unexpected error when attempting to reduce %v pressure: %v", resourceToReclaim, err)
		}

	}
	// signal有对应回收函数
	if len(nodeReclaimFuncs) > 0 {
		// node状态和所有pod的状态监控信息
		summary, err := m.summaryProvider.Get(true)
		if err != nil {
			klog.Errorf("eviction manager: failed to get summary stats after resource reclaim: %v", err)
			return false
		}

		// make observations and get a function to derive pod usage stats relative to those observations.
		// 从summary.Node获得各个阈值类型的available和capacity和time
		observations, _ := makeSignalObservations(summary)
		debugLogObservations("observations after resource reclaim", observations)

		// determine the set of thresholds met independent of grace period
		// 返回所有需要保留资源值大于可用资源值（到达evict阈值）的类型的阈值设置
		// 保留资源不需要加上MinReclaim
		thresholds := thresholdsMet(m.config.Thresholds, observations, false)
		debugLogThresholdsWithObservation("thresholds after resource reclaim - ignoring grace period", thresholds, observations)

		// 没有超过阈值，直接返回
		if len(thresholds) == 0 {
			return true
		}
	}
	return false
}

// localStorageEviction checks the EmptyDir volume usage for each pod and determine whether it exceeds the specified limit and needs
// to be evicted. It also checks every container in the pod, if the container overlay usage exceeds the limit, the pod will be evicted too.
// 返回所有发生驱逐的pod列表（满足下面三种任意一个条件）
// 1. pod里EmptyDir类型的volume的使用值超出了设置的限制（有显式的设置pod.Spec.Volumes[i].VolumeSource.EmptyDir.SizeLimit）
// 2. pod里所有container的（logs或rootfs加logs）的总磁盘使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量，或加上pod的etcHost文件大小，大于pod总的"ephemeral-storage"类型资源limit，进行驱逐且优雅停止时间为0
// 3. pod里所有container里只要有一个container磁盘使用量大于container的"ephemeral-storage"类型资源limit，则进行pod驱逐而且优雅停止时间为0
// 镜像保存的磁盘设备和kubelet root目录所在挂载设备一致，则container磁盘使用量为logs的磁盘使用量加上rootfs的磁盘使用量
// 镜像保存的磁盘设备和kubelet root目录所在挂载设备不一致，则container磁盘使用量为logs的磁盘使用量
func (m *managerImpl) localStorageEviction(summary *statsapi.Summary, pods []*v1.Pod) []*v1.Pod {
	// 通过pod查找statsapi.PodStats的函数
	statsFunc := cachedStatsFunc(summary.Pods)
	evicted := []*v1.Pod{}
	for _, pod := range pods {
		podStats, ok := statsFunc(pod)
		// 没有这个pod的监控数据（说明数据还未更新），跳过
		if !ok {
			continue
		}

		// 驱逐pod里EmptyDir类型的volume的使用值超出了设置的限制（有显式的设置pod.Spec.Volumes[i].VolumeSource.EmptyDir.SizeLimit）的pod，返回是否发生了驱逐
		if m.emptyDirLimitEviction(podStats, pod) {
			evicted = append(evicted, pod)
			continue
		}

		// 所有container的（logs或rootfs加logs）的总磁盘使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量，或加上pod的etcHost文件大小，大于pod总的"ephemeral-storage"类型资源limit，进行驱逐且优雅停止时间为0
		if m.podEphemeralStorageLimitEviction(podStats, pod) {
			evicted = append(evicted, pod)
			continue
		}

		// pod里所有container里只要有一个container磁盘使用量大于container的"ephemeral-storage"类型资源limit，则进行pod驱逐而且优雅停止时间为0
		// 镜像保存的磁盘设备和kubelet root目录所在挂载设备一致，则container磁盘使用量为logs的磁盘使用量加上rootfs的磁盘使用量
		// 镜像保存的磁盘设备和kubelet root目录所在挂载设备不一致，则container磁盘使用量为logs的磁盘使用量
		if m.containerEphemeralStorageLimitEviction(podStats, pod) {
			evicted = append(evicted, pod)
		}
	}

	return evicted
}

// 驱逐EmptyDir类型的volume的使用值超出了设置的限制（有显式的设置pod.Spec.Volumes[i].VolumeSource.EmptyDir.SizeLimit）的pod，返回是否发生了驱逐
func (m *managerImpl) emptyDirLimitEviction(podStats statsapi.PodStats, pod *v1.Pod) bool {
	podVolumeUsed := make(map[string]*resource.Quantity)
	for _, volume := range podStats.VolumeStats {
		podVolumeUsed[volume.Name] = resource.NewQuantity(int64(*volume.UsedBytes), resource.BinarySI)
	}
	for i := range pod.Spec.Volumes {
		source := &pod.Spec.Volumes[i].VolumeSource
		if source.EmptyDir != nil {
			size := source.EmptyDir.SizeLimit
			used := podVolumeUsed[pod.Spec.Volumes[i].Name]
			// EmptyDir类型的volume的使用值超出了设置的限制（有显式的设置）
			if used != nil && size != nil && size.Sign() == 1 && used.Cmp(*size) > 0 {
				// the emptyDir usage exceeds the size limit, evict the pod
				// 对超出限制的pod进行驱逐（pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，不进行驱逐）
				// 利用pod worker机制来移除pod（停止pod里所有container和sandbox）
				// 生成"Usage of EmptyDir volume {pod.Spec.Volumes[i].Name} exceeds the limit '{size}'. "的pod相关的event
				if m.evictPod(pod, 0, fmt.Sprintf(emptyDirMessageFmt, pod.Spec.Volumes[i].Name, size.String()), nil) {
					metrics.Evictions.WithLabelValues(signalEmptyDirFsLimit).Inc()
					return true
				}
				return false
			}
		}
	}

	return false
}

// 所有container的（logs或rootfs加logs）的总磁盘使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量，或加上pod的etcHost文件大小，大于pod总的"ephemeral-storage"类型资源limit，进行驱逐
func (m *managerImpl) podEphemeralStorageLimitEviction(podStats statsapi.PodStats, pod *v1.Pod) bool {
	// 统计pod的各种资源的limit和request
	_, podLimits := apiv1resource.PodRequestsAndLimits(pod)
	// 是否设置"ephemeral-storage" limit
	_, found := podLimits[v1.ResourceEphemeralStorage]
	// 没有limit，直接返回
	if !found {
		return false
	}

	podEphemeralStorageTotalUsage := &resource.Quantity{}
	var fsStatsSet []fsStatsType
	// 镜像保存的磁盘设备和kubelet root目录所在挂载设备不一致
	if *m.dedicatedImageFs {
		// "logs"和"localVolumeSource"
		fsStatsSet = []fsStatsType{fsStatsLogs, fsStatsLocalVolumeSource}
	} else {
		// "root"和"logs"和"localVolumeSource"
		fsStatsSet = []fsStatsType{fsStatsRoot, fsStatsLogs, fsStatsLocalVolumeSource}
	}
	// m.etcHostPath返回，默认为"/var/lib/kubelet/pods/{pod uid}/etc-hosts"
	// 所有container的（logs或rootfs加logs）的总磁盘使用量和inodes使用量，或加上volume（GitRepo、不为"Memory"的EmptyDir、configMap、DownwardAPI）的总磁盘使用量和inodes使用量，或加上pod的etcHost文件大小和inode使用里（为1），即包含了"ephemeral-storage"和"inodes"资源的使用量
	podEphemeralUsage, err := podLocalEphemeralStorageUsage(podStats, pod, fsStatsSet, m.etcHostsPath(pod.UID))
	if err != nil {
		klog.Errorf("eviction manager: error getting pod disk usage %v", err)
		return false
	}

	// pod总的"ephemeral-storage"类型资源使用
	podEphemeralStorageTotalUsage.Add(podEphemeralUsage[v1.ResourceEphemeralStorage])
	// pod总的"ephemeral-storage"类型资源limit
	podEphemeralStorageLimit := podLimits[v1.ResourceEphemeralStorage]
	// pod总的"ephemeral-storage"类型资源使用大于pod总的"ephemeral-storage"类型资源limit
	if podEphemeralStorageTotalUsage.Cmp(podEphemeralStorageLimit) > 0 {
		// the total usage of pod exceeds the total size limit of containers, evict the pod
		// 对pod进行驱逐，pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，不进行驱逐
		// 利用pod worker机制来移除pod（停止pod里所有container和sandbox）
		// 生成"Pod ephemeral local storage usage exceeds the total limit of containers {podEphemeralStorageLimit}. "的pod相关event
		// 而且优雅停止时间为0
		if m.evictPod(pod, 0, fmt.Sprintf(podEphemeralStorageMessageFmt, podEphemeralStorageLimit.String()), nil) {
			metrics.Evictions.WithLabelValues(signalEphemeralPodFsLimit).Inc()
			return true
		}
		return false
	}
	return false
}

// pod里所有container里只要有一个container磁盘使用量大于container的"ephemeral-storage"类型资源limit，则进行pod驱逐而且优雅停止时间为0
// 镜像保存的磁盘设备和kubelet root目录所在挂载设备一致，则container磁盘使用量为logs的磁盘使用量加上rootfs的磁盘使用量
// 镜像保存的磁盘设备和kubelet root目录所在挂载设备不一致，则container磁盘使用量为logs的磁盘使用量
func (m *managerImpl) containerEphemeralStorageLimitEviction(podStats statsapi.PodStats, pod *v1.Pod) bool {
	thresholdsMap := make(map[string]*resource.Quantity)
	for _, container := range pod.Spec.Containers {
		// container的"ephemeral-storage"类型资源limit
		ephemeralLimit := container.Resources.Limits.StorageEphemeral()
		if ephemeralLimit != nil && ephemeralLimit.Value() != 0 {
			thresholdsMap[container.Name] = ephemeralLimit
		}
	}

	for _, containerStat := range podStats.Containers {
		// 将containerStat.Logs.UsedBytes转城resource.Quantity
		containerUsed := diskUsage(containerStat.Logs)
		// 镜像保存的磁盘设备和kubelet root目录所在挂载设备一致，则container磁盘使用量为logs的磁盘使用量加上rootfs的磁盘使用量
		if !*m.dedicatedImageFs {
			containerUsed.Add(*diskUsage(containerStat.Rootfs))
		}

		// container磁盘使用量大于container的"ephemeral-storage"类型资源limit，则进行pod驱逐而且优雅停止时间为0
		if ephemeralStorageThreshold, ok := thresholdsMap[containerStat.Name]; ok {
			if ephemeralStorageThreshold.Cmp(*containerUsed) < 0 {
				// 对pod进行驱逐，pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，不进行驱逐
				// 利用pod worker机制来移除pod（停止pod里所有container和sandbox）
				// 生成"Container {container name} exceeded its local ephemeral storage limit {ephemeralStorageThreshold}. "的pod事件
				// 而且优雅停止时间为0
				if m.evictPod(pod, 0, fmt.Sprintf(containerEphemeralStorageMessageFmt, containerStat.Name, ephemeralStorageThreshold.String()), nil) {
					metrics.Evictions.WithLabelValues(signalEphemeralContainerFsLimit).Inc()
					return true
				}
				return false
			}
		}
	}
	return false
}

// 对pod进行驱逐，pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，不进行驱逐
// 利用pod worker机制来移除pod（停止pod里所有container和sandbox）
func (m *managerImpl) evictPod(pod *v1.Pod, gracePeriodOverride int64, evictMsg string, annotations map[string]string) bool {
	// If the pod is marked as critical and static, and support for critical pod annotations is enabled,
	// do not evict such pods. Static pods are not re-admitted after evictions.
	// https://github.com/kubernetes/kubernetes/issues/40573 has more details.
	// pod是static pod或mirror pod，或pod设置了优先级且优先级大于2000000000，返回true。其他情况返回false
	if kubelettypes.IsCriticalPod(pod) {
		klog.Errorf("eviction manager: cannot evict a critical pod %s", format.Pod(pod))
		return false
	}
	status := v1.PodStatus{
		Phase:   v1.PodFailed,
		Message: evictMsg,
		Reason:  Reason,
	}
	// record that we are evicting the pod
	m.recorder.AnnotatedEventf(pod, annotations, v1.EventTypeWarning, Reason, evictMsg)
	// this is a blocking call and should only return when the pod and its containers are killed.
	// m.killPodFunc实现在pkg\kubelet\pod_workers.go里killPodNow
	// 返回利用pod worker机制来移除pod（停止pod里所有container和sandbox）函数
	err := m.killPodFunc(pod, status, &gracePeriodOverride)
	if err != nil {
		klog.Errorf("eviction manager: pod %s failed to evict %v", format.Pod(pod), err)
	} else {
		klog.Infof("eviction manager: pod %s is evicted successfully", format.Pod(pod))
	}
	return true
}
