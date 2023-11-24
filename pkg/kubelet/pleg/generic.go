/*
Copyright 2015 The Kubernetes Authors.

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

package pleg

import (
	"fmt"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

// GenericPLEG is an extremely simple generic PLEG that relies solely on
// periodic listing to discover container changes. It should be used
// as temporary replacement for container runtimes do not support a proper
// event generator yet.
//
// Note that GenericPLEG assumes that a container would not be created,
// terminated, and garbage collected within one relist period. If such an
// incident happens, GenenricPLEG would miss all events regarding this
// container. In the case of relisting failure, the window may become longer.
// Note that this assumption is not unique -- many kubelet internal components
// rely on terminated containers as tombstones for bookkeeping purposes. The
// garbage collector is implemented to work with such situations. However, to
// guarantee that kubelet can handle missing container events, it is
// recommended to set the relist period short and have an auxiliary, longer
// periodic sync in kubelet as the safety net.
type GenericPLEG struct {
	// The period for relisting.
	relistPeriod time.Duration
	// The container runtime.
	runtime kubecontainer.Runtime
	// The channel from which the subscriber listens events.
	eventChannel chan *PodLifecycleEvent
	// The internal cache for pod/container information.
	podRecords podRecords
	// Time of the last relisting.
	relistTime atomic.Value
	// Cache for storing the runtime states required for syncing pods.
	cache kubecontainer.Cache
	// For testability.
	clock clock.Clock
	// Pods that failed to have their status retrieved during a relist. These pods will be
	// retried during the next relisting.
	podsToReinspect map[types.UID]*kubecontainer.Pod
}

// plegContainerState has a one-to-one mapping to the
// kubecontainer.ContainerState except for the non-existent state. This state
// is introduced here to complete the state transition scenarios.
type plegContainerState string

const (
	plegContainerRunning     plegContainerState = "running"
	plegContainerExited      plegContainerState = "exited"
	plegContainerUnknown     plegContainerState = "unknown"
	plegContainerNonExistent plegContainerState = "non-existent"

	// The threshold needs to be greater than the relisting period + the
	// relisting time, which can vary significantly. Set a conservative
	// threshold to avoid flipping between healthy and unhealthy.
	relistThreshold = 3 * time.Minute
)

// 转换kubecontainer.ContainerState为plegContainerState
func convertState(state kubecontainer.ContainerState) plegContainerState {
	switch state {
	case kubecontainer.ContainerStateCreated:
		// kubelet doesn't use the "created" state yet, hence convert it to "unknown".
		return plegContainerUnknown
	case kubecontainer.ContainerStateRunning:
		return plegContainerRunning
	case kubecontainer.ContainerStateExited:
		return plegContainerExited
	case kubecontainer.ContainerStateUnknown:
		return plegContainerUnknown
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", state))
	}
}

type podRecord struct {
	old     *kubecontainer.Pod
	current *kubecontainer.Pod
}

type podRecords map[types.UID]*podRecord

// NewGenericPLEG instantiates a new GenericPLEG object and return it.
func NewGenericPLEG(runtime kubecontainer.Runtime, channelCapacity int,
	relistPeriod time.Duration, cache kubecontainer.Cache, clock clock.Clock) PodLifecycleEventGenerator {
	return &GenericPLEG{
		relistPeriod: relistPeriod,
		runtime:      runtime,
		eventChannel: make(chan *PodLifecycleEvent, channelCapacity),
		podRecords:   make(podRecords),
		cache:        cache,
		clock:        clock,
	}
}

// Watch returns a channel from which the subscriber can receive PodLifecycleEvent
// events.
// TODO: support multiple subscribers.
// 返回g.eventChannel
func (g *GenericPLEG) Watch() chan *PodLifecycleEvent {
	return g.eventChannel
}

// Start spawns a goroutine to relist periodically.
func (g *GenericPLEG) Start() {
	// 默认1秒钟执行一次
	// 从runtime中获得所有运行和不在运行的pod
	// 将pods更新到g.podRecords里每个uid的current
	// 遍历g.podRecords里的pod，遍历pod里的普通container和sandbox，根据老的和新的container状态生成PodLifecycleEvent
	// 遍历所有PodLifecycleEvent：
	//    如果启用podStatus缓存，则更新pod的podStatus缓存
	//    更新pid的g.podRecords：
	//       id不在podRecords中，直接返回
	//       如果r.current为nil，则从podRecords中移除这个id（因为pod已经被删除了），直接返回
	//       否则，将r.old设置成r.current，r.current置为nil
	//   发送event到g.eventChannel，如果g.eventChannel满了，则丢弃这个event
	go wait.Until(g.relist, g.relistPeriod, wait.NeverStop)
}

// Healthy check if PLEG work properly.
// relistThreshold is the maximum interval between two relist.
func (g *GenericPLEG) Healthy() (bool, error) {
	relistTime := g.getRelistTime()
	if relistTime.IsZero() {
		return false, fmt.Errorf("pleg has yet to be successful")
	}
	// Expose as metric so you can alert on `time()-pleg_last_seen_seconds > nn`
	metrics.PLEGLastSeen.Set(float64(relistTime.Unix()))
	elapsed := g.clock.Since(relistTime)
	if elapsed > relistThreshold {
		return false, fmt.Errorf("pleg was last seen active %v ago; threshold is %v", elapsed, relistThreshold)
	}
	return true, nil
}

// 根据老的和新的container状态生成PodLifecycleEvent
func generateEvents(podID types.UID, cid string, oldState, newState plegContainerState) []*PodLifecycleEvent {
	// 状态没有变化，直接返回nil
	if newState == oldState {
		return nil
	}

	klog.V(4).Infof("GenericPLEG: %v/%v: %v -> %v", podID, cid, oldState, newState)
	switch newState {
	//新状态为"running"，则生成"ContainerStarted"的PodLifecycleEvent
	case plegContainerRunning:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerStarted, Data: cid}}
	// 新状态为"exited"，则生成"ContainerDied"的PodLifecycleEvent
	case plegContainerExited:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}}
	// 新状态为"unknown"，则生成"ContainerChanged"的PodLifecycleEvent
	case plegContainerUnknown:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerChanged, Data: cid}}
	// 新状态为"non-existent"
	// 如果老的状态为"exited"，则生成"ContainerRemoved"的PodLifecycleEvent
	// 否则生成"ContainerDied"和"ContainerRemoved"的PodLifecycleEvent（说明container莫名其妙的消失，有可能被手动移除，也有可能瞬间停止移除，中间状态没有捕获到）
	case plegContainerNonExistent:
		switch oldState {
		case plegContainerExited:
			// We already reported that the container died before.
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerRemoved, Data: cid}}
		default:
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}, {ID: podID, Type: ContainerRemoved, Data: cid}}
		}
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", newState))
	}
}

// 获得g.relistTime中的时间
func (g *GenericPLEG) getRelistTime() time.Time {
	val := g.relistTime.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

// 更新g.relistTime中的时间
func (g *GenericPLEG) updateRelistTime(timestamp time.Time) {
	g.relistTime.Store(timestamp)
}

// relist queries the container runtime for list of pods/containers, compare
// with the internal pods/containers, and generates events accordingly.
// 从runtime中获得所有运行和不在运行的pod
// 将pods更新到g.podRecords里每个uid的current
// 遍历g.podRecords里的pod，遍历pod里的普通container和sandbox，根据老的和新的container状态生成PodLifecycleEvent
// 遍历所有PodLifecycleEvent：
//    如果启用podStatus缓存，则更新pod的podStatus缓存
//    更新pid的g.podRecords：
//       id不在podRecords中，直接返回
//       如果r.current为nil，则从podRecords中移除这个id（因为pod已经被删除了），直接返回
//       否则，将r.old设置成r.current，r.current置为nil
//   发送event到g.eventChannel，如果g.eventChannel满了，则丢弃这个event
func (g *GenericPLEG) relist() {
	klog.V(5).Infof("GenericPLEG: Relisting")

	if lastRelistTime := g.getRelistTime(); !lastRelistTime.IsZero() {
		metrics.PLEGRelistInterval.Observe(metrics.SinceInSeconds(lastRelistTime))
	}

	timestamp := g.clock.Now()
	defer func() {
		metrics.PLEGRelistDuration.Observe(metrics.SinceInSeconds(timestamp))
	}()

	// Get all the pods.
	// 从runtime中获得所有运行和不在运行的pod
	podList, err := g.runtime.GetPods(true)
	if err != nil {
		klog.Errorf("GenericPLEG: Unable to retrieve pods: %v", err)
		return
	}

	// 更新g.relistTime中的时间
	g.updateRelistTime(timestamp)

	pods := kubecontainer.Pods(podList)
	// update running pod and container count
	updateRunningPodAndContainerMetrics(pods)
	// 将pods更新到g.podRecords里每个uid的current
	g.podRecords.setCurrent(pods)

	// Compare the old and the current pods, and generate events.
	eventsByPodID := map[types.UID][]*PodLifecycleEvent{}
	for pid := range g.podRecords {
		// 返回id的old字段值（记录）
		oldPod := g.podRecords.getOld(pid)
		// 返回id的current字段值（记录）
		pod := g.podRecords.getCurrent(pid)
		// Get all containers in the old and the new pod.
		// 返回oldPod和新pod里所有container包括普通container和sandbox
		allContainers := getContainersFromPods(oldPod, pod)

		for _, container := range allContainers {
			// 根据老的和新的container状态生成PodLifecycleEvent
			events := computeEvents(oldPod, pod, &container.ID)
			for _, e := range events {
				// 将e append到eventsByPodID[e.ID]
				updateEvents(eventsByPodID, e)
			}
		}
	}

	var needsReinspection map[types.UID]*kubecontainer.Pod
	// g.cache不为nil返回true
	// 默认启用
	if g.cacheEnabled() {
		needsReinspection = make(map[types.UID]*kubecontainer.Pod)
	}

	// If there are events associated with a pod, we should update the
	// podCache.
	// 遍历所有event
	for pid, events := range eventsByPodID {
		// 返回g.podRecords里pid的current字段值（记录）
		pod := g.podRecords.getCurrent(pid)
		// g.cache不为nil返回true
		// 默认启用
		if g.cacheEnabled() {
			// updateCache() will inspect the pod and update the cache. If an
			// error occurs during the inspection, we want PLEG to retry again
			// in the next relist. To achieve this, we do not update the
			// associated podRecord of the pod, so that the change will be
			// detect again in the next relist.
			// TODO: If many pods changed during the same relist period,
			// inspecting the pod and getting the PodStatus to update the cache
			// serially may take a while. We should be aware of this and
			// parallelize if needed.
			// pod为nil，从g.cache.pods删除这个id记录。（pod不在podRecord的current字段里，说明pod消失了）
			// pod不为nil，则从runtime中获得podStatus，并更新podStatus缓存
			// 添加id的data到添加到g.cache.pods中，并通知关注这个id的订阅者，发送缓存（g.cache.pods）中的id的data，到订阅者的chan中。
			if err := g.updateCache(pod, pid); err != nil {
				// Rely on updateCache calling GetPodStatus to log the actual error.
				klog.V(4).Infof("PLEG: Ignoring events for pod %s/%s: %v", pod.Name, pod.Namespace, err)

				// make sure we try to reinspect the pod during the next relisting
				// 更新podStatus缓存发生错误，将pod写入needsReinspection名单里
				needsReinspection[pid] = pod

				// 更新podStatus缓存发生错误，则继续下一个pod event
				continue
			} else {
				// this pod was in the list to reinspect and we did so because it had events, so remove it
				// from the list (we don't want the reinspection code below to inspect it a second time in
				// this relist execution)
				// 成功更新podStatus缓存，从g.podsToReinspect中移除这个pod
				delete(g.podsToReinspect, pid)
			}
		}
		// Update the internal storage and send out the events.
		// id不在podRecords中，直接返回
		// 如果r.current为nil，则从podRecords中移除这个id（因为pod已经被删除了），直接返回
		// 否则，将r.old设置成r.current，r.current置为nil
		g.podRecords.update(pid)
		// 发送event到g.eventChannel，如果g.eventChannel满了，则丢弃这个event
		for i := range events {
			// Filter out events that are not reliable and no other components use yet.
			// 忽略"ContainerChanged"类型的event（container status变为unknown）
			if events[i].Type == ContainerChanged {
				continue
			}
			select {
			case g.eventChannel <- events[i]:
			default:
				metrics.PLEGDiscardEvents.Inc()
				klog.Error("event channel is full, discard this relist() cycle event")
			}
		}
	}

	// g.cache不为nil返回true
	// 默认启用
	if g.cacheEnabled() {
		// reinspect any pods that failed inspection during the previous relist
		// 需要重新inspect列表不为空，则重新从runtime中获得podStatus，更新podStatus缓存
		if len(g.podsToReinspect) > 0 {
			klog.V(5).Infof("GenericPLEG: Reinspecting pods that previously failed inspection")
			for pid, pod := range g.podsToReinspect {
				// pod为nil，从g.cache.pods删除这个id记录
				// pod不为nil，则从runtime中获得podStatus，并更新podStatus缓存
				// 添加id的data到添加到g.cache.pods中，并通知关注这个id的订阅者，发送缓存（g.cache.pods）中的id的data，到订阅者的chan中。
				if err := g.updateCache(pod, pid); err != nil {
					// Rely on updateCache calling GetPodStatus to log the actual error.
					klog.V(5).Infof("PLEG: pod %s/%s failed reinspection: %v", pod.Name, pod.Namespace, err)
					// 还是更新缓存中发生错误，则添加pod到needsReinspection
					needsReinspection[pid] = pod
				}
			}
		}

		// Update the cache timestamp.  This needs to happen *after*
		// all pods have been properly updated in the cache.
		// 更新g.cache.timestamp为timestamp，并发生data到timestamp满足订阅者需要（还未过期）的订阅者的chan中
		g.cache.UpdateTime(timestamp)
	}

	// make sure we retain the list of pods that need reinspecting the next time relist is called
	// 更新g.podsToReinspect为needsReinspection
	g.podsToReinspect = needsReinspection
}

// 返回pods里所有container包括普通container和sandbox
func getContainersFromPods(pods ...*kubecontainer.Pod) []*kubecontainer.Container {
	cidSet := sets.NewString()
	var containers []*kubecontainer.Container
	for _, p := range pods {
		if p == nil {
			continue
		}
		for _, c := range p.Containers {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}
		// Update sandboxes as containers
		// TODO: keep track of sandboxes explicitly.
		for _, c := range p.Sandboxes {
			cid := string(c.ID.ID)
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}

	}
	return containers
}

// 根据老的和新的container状态生成PodLifecycleEvent
func computeEvents(oldPod, newPod *kubecontainer.Pod, cid *kubecontainer.ContainerID) []*PodLifecycleEvent {
	var pid types.UID
	// pod的uid优先选择oldPod
	if oldPod != nil {
		pid = oldPod.ID
	} else if newPod != nil {
		pid = newPod.ID
	}
	// 依次从oldPod中的container和sandbox中找到container id为cid的container，并将kubecontainer.ContainerState转换为plegContainerState
	oldState := getContainerState(oldPod, cid)
	// 依次从newPod中的container和sandbox中找到container id为cid的container，并将kubecontainer.ContainerState转换为plegContainerState
	newState := getContainerState(newPod, cid)
	// 根据老的和新的container状态生成PodLifecycleEvent
	return generateEvents(pid, cid.ID, oldState, newState)
}

// g.cache不为nil返回true
func (g *GenericPLEG) cacheEnabled() bool {
	return g.cache != nil
}

// getPodIP preserves an older cached status' pod IP if the new status has no pod IPs
// and its sandboxes have exited
// 如果status中有ip，则返回status中的ip
// status中没有ip，如果从缓存（g.cache.pods）中的获取podStatus发生错误，或缓存中的status里没有ip，则返回nil
// 没有发生错误，且缓存中oldStatus里有ip，如果status.SandboxStatuses中有ready的sandbox，则返回status的ip列表（为空）
// status中没有ready的sandbox，返回缓存中oldStatus里的ip列表
func (g *GenericPLEG) getPodIPs(pid types.UID, status *kubecontainer.PodStatus) []string {
	// 如果status中有ip，则返回status中的ip
	if len(status.IPs) != 0 {
		return status.IPs
	}

	// status中没有ip

	// 返回缓存（g.cache.pods）中的id的data里status，如果不在缓存中，则返回空的数据（只有pod id）
	oldStatus, err := g.cache.Get(pid)
	// 如果发生错误，或缓存中的status里没有ip，则返回nil
	if err != nil || len(oldStatus.IPs) == 0 {
		return nil
	}

	// 没有发生错误，且缓存中oldStatus里有ip

	// 如果status.SandboxStatuses中有ready的sandbox，则返回status的ip列表（为空）
	for _, sandboxStatus := range status.SandboxStatuses {
		// If at least one sandbox is ready, then use this status update's pod IP
		if sandboxStatus.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			return status.IPs
		}
	}

	// For pods with no ready containers or sandboxes (like exited pods)
	// use the old status' pod IP
	// status中没有ready的sandbox，返回缓存中oldStatus里的ip列表
	return oldStatus.IPs
}

// pod为nil，从g.cache.pods删除这个id记录
// pod不为nil，则从runtime中获得podStatus，并更新podStatus缓存
// 添加id的data到添加到g.cache.pods中，并通知关注这个id的订阅者，发送缓存（g.cache.pods）中的id的data，到订阅者的chan中。
func (g *GenericPLEG) updateCache(pod *kubecontainer.Pod, pid types.UID) error {
	// pod不在podRecord的current字段里，说明pod消失了
	if pod == nil {
		// The pod is missing in the current relist. This means that
		// the pod has no visible (active or inactive) containers.
		klog.V(4).Infof("PLEG: Delete status for pod %q", string(pid))
		// 从g.cache.pods删除这个id记录
		g.cache.Delete(pid)
		return nil
	}
	timestamp := g.clock.Now()
	// TODO: Consider adding a new runtime method
	// GetPodStatus(pod *kubecontainer.Pod) so that Docker can avoid listing
	// all containers again.
	// 返回pod的uid、name、namespace、以及所有sandbox状态、ips、所有容器状态
	status, err := g.runtime.GetPodStatus(pod.ID, pod.Name, pod.Namespace)
	klog.V(4).Infof("PLEG: Write status for %s/%s: %#v (err: %v)", pod.Name, pod.Namespace, status, err)
	if err == nil {
		// Preserve the pod IP across cache updates if the new IP is empty.
		// When a pod is torn down, kubelet may race with PLEG and retrieve
		// a pod status after network teardown, but the kubernetes API expects
		// the completed pod's IP to be available after the pod is dead.
		//
		// 如果status中有ip，则返回status中的ip（status.IPs没有变化）
		// status中没有ip，如果从缓存（g.cache.pods）中的获取podStatus发生错误，或缓存中的status里没有ip，则返回nil（status.IPs没有变化）
		// 没有发生错误，且缓存中oldStatus里有ip，如果status.SandboxStatuses中有ready的sandbox，则返回status的ip列表（为空）（status.IPs没有变化）
		// status中没有ready的sandbox，返回缓存中oldStatus里的ip列表（说明PLEG从runtime中获取pod列表，到runtime中GetPodStatus，之间pod被删除了）（status.IPs有变化）
		status.IPs = g.getPodIPs(pid, status)
	}

	// 更新podStatus缓存
	// 添加id的data到添加到g.cache.pods中，并通知关注这个id的订阅者，发送缓存（g.cache.pods）中的id的data，到订阅者的chan中。
	g.cache.Set(pod.ID, status, err, timestamp)
	return err
}

func updateEvents(eventsByPodID map[types.UID][]*PodLifecycleEvent, e *PodLifecycleEvent) {
	if e == nil {
		return
	}
	eventsByPodID[e.ID] = append(eventsByPodID[e.ID], e)
}

// 依次从pod中的container和sandbox中找到container id为cid的container，并将kubecontainer.ContainerState转换为plegContainerState
func getContainerState(pod *kubecontainer.Pod, cid *kubecontainer.ContainerID) plegContainerState {
	// Default to the non-existent state.
	state := plegContainerNonExistent
	if pod == nil {
		return state
	}
	// pod中container id为cid的container
	c := pod.FindContainerByID(*cid)
	if c != nil {
		// 转换kubecontainer.ContainerState为plegContainerState
		return convertState(c.State)
	}
	// Search through sandboxes too.
	// 在pod中的container没有找到container，尝试从pod中的sandbox里找到container
	c = pod.FindSandboxByID(*cid)
	if c != nil {
		// 转换kubecontainer.ContainerState为plegContainerState
		return convertState(c.State)
	}

	return state
}

func updateRunningPodAndContainerMetrics(pods []*kubecontainer.Pod) {
	// Set the number of running pods in the parameter
	// 更新多少个pod在runtime中的metrics
	metrics.RunningPodCount.Set(float64(len(pods)))
	// intermediate map to store the count of each "container_state"
	containerStateCount := make(map[string]int)

	for _, pod := range pods {
		containers := pod.Containers
		for _, container := range containers {
			// update the corresponding "container_state" in map to set value for the gaugeVec metrics
			containerStateCount[string(container.State)]++
		}
	}
	// 更新各种container运行状态的container数量metrics
	for key, value := range containerStateCount {
		metrics.RunningContainerCount.WithLabelValues(key).Set(float64(value))
	}
}

// 返回id的old字段值（记录）
func (pr podRecords) getOld(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.old
}

// 返回id的current字段值（记录）
func (pr podRecords) getCurrent(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.current
}

// 将pods更新到pr里每个uid的current
func (pr podRecords) setCurrent(pods []*kubecontainer.Pod) {
	// 重置每个uid的current记录为nil
	for i := range pr {
		pr[i].current = nil
	}
	// 将pods更新为current记录
	for _, pod := range pods {
		if r, ok := pr[pod.ID]; ok {
			r.current = pod
		} else {
			pr[pod.ID] = &podRecord{current: pod}
		}
	}
}

// id不在podRecords中，直接返回
// 如果r.current为nil，则从podRecords中移除这个id（因为pod已经被删除了），直接返回
// 否则，将r.old设置成r.current，r.current置为nil
func (pr podRecords) update(id types.UID) {
	r, ok := pr[id]
	// 不在podRecords中，直接返回
	if !ok {
		return
	}
	pr.updateInternal(id, r)
}

// 如果r.current为nil，则从podRecords中移除这个id（因为pod已经被删除了），直接返回
// 否则，将r.old设置成r.current，r.current置为nil
func (pr podRecords) updateInternal(id types.UID, r *podRecord) {
	// 如果r.current为nil，则从podRecords中移除这个id（因为pod已经被删除了），直接返回
	if r.current == nil {
		// Pod no longer exists; delete the entry.
		delete(pr, id)
		return
	}
	// 将r.old设置成r.current
	r.old = r.current
	// r.current置为nil
	r.current = nil
}
