/*
Copyright 2014 The Kubernetes Authors.

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

package kubelet

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
)

// OnCompleteFunc is a function that is invoked when an operation completes.
// If err is non-nil, the operation did not complete successfully.
type OnCompleteFunc func(err error)

// PodStatusFunc is a function that is invoked to generate a pod status.
type PodStatusFunc func(pod *v1.Pod, podStatus *kubecontainer.PodStatus) v1.PodStatus

// KillPodOptions are options when performing a pod update whose update type is kill.
type KillPodOptions struct {
	// PodStatusFunc is the function to invoke to set pod status in response to a kill request.
	PodStatusFunc PodStatusFunc
	// PodTerminationGracePeriodSecondsOverride is optional override to use if a pod is being killed as part of kill operation.
	PodTerminationGracePeriodSecondsOverride *int64
}

// UpdatePodOptions is an options struct to pass to a UpdatePod operation.
type UpdatePodOptions struct {
	// pod to update
	Pod *v1.Pod
	// the mirror pod for the pod to update, if it is a static pod
	MirrorPod *v1.Pod
	// the type of update (create, update, sync, kill)
	UpdateType kubetypes.SyncPodType
	// optional callback function when operation completes
	// this callback is not guaranteed to be completed since a pod worker may
	// drop update requests if it was fulfilling a previous request.  this is
	// only guaranteed to be invoked in response to a kill pod request which is
	// always delivered.
	OnCompleteFunc OnCompleteFunc
	// if update type is kill, use the specified options to kill the pod.
	KillPodOptions *KillPodOptions
}

// PodWorkers is an abstract interface for testability.
type PodWorkers interface {
	UpdatePod(options *UpdatePodOptions)
	ForgetNonExistingPodWorkers(desiredPods map[types.UID]sets.Empty)
	ForgetWorker(uid types.UID)
}

// syncPodOptions provides the arguments to a SyncPod operation.
type syncPodOptions struct {
	// the mirror pod for the pod to sync, if it is a static pod
	mirrorPod *v1.Pod
	// pod to sync
	pod *v1.Pod
	// the type of update (create, update, sync)
	updateType kubetypes.SyncPodType
	// the current status
	podStatus *kubecontainer.PodStatus
	// if update type is kill, use the specified options to kill the pod.
	killPodOptions *KillPodOptions
}

// the function to invoke to perform a sync.
type syncPodFnType func(options syncPodOptions) error

const (
	// jitter factor for resyncInterval
	workerResyncIntervalJitterFactor = 0.5

	// jitter factor for backOffPeriod and backOffOnTransientErrorPeriod
	workerBackOffPeriodJitterFactor = 0.5

	// backoff period when transient error occurred.
	backOffOnTransientErrorPeriod = time.Second
)

type podWorkers struct {
	// Protects all per worker fields.
	podLock sync.Mutex

	// Tracks all running per-pod goroutines - per-pod goroutine will be
	// processing updates received through its corresponding channel.
	podUpdates map[types.UID]chan UpdatePodOptions
	// Track the current state of per-pod goroutines.
	// Currently all update request for a given pod coming when another
	// update of this pod is being processed are ignored.
	// 每个uid的goroutine 状态，是否处理了chan里的消息，正在处理为true，已经处理完为false
	isWorking map[types.UID]bool
	// Tracks the last undelivered work item for this pod - a work item is
	// undelivered if it comes in while the worker is working.
	// worker goroutine，只有一个buffer，为了保证生产者不hang住，在worker正在处理时候，将要生产的消息保存在lastUndeliveredWorkUpdate里，代表等待发送到chan中
	// 这样可以保证后续其他生产UpdatePodOptions，出现堆积时候，worker只处理最后的一个UpdatePodOptions，新的一个UpdatePodOptions覆盖老的，但是UpdateType为kubetypes.SyncPodKill不会被覆盖，后续的生产UpdatePodOptions会被丢弃
	lastUndeliveredWorkUpdate map[types.UID]UpdatePodOptions

	workQueue queue.WorkQueue

	// This function is run to sync the desired stated of pod.
	// NOTE: This function has to be thread-safe - it can be called for
	// different pods at the same time.
	syncPodFn syncPodFnType

	// The EventRecorder to use
	recorder record.EventRecorder

	// backOffPeriod is the duration to back off when there is a sync error.
	backOffPeriod time.Duration

	// resyncInterval is the duration to wait until the next sync.
	resyncInterval time.Duration

	// podCache stores kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache
}

func newPodWorkers(syncPodFn syncPodFnType, recorder record.EventRecorder, workQueue queue.WorkQueue,
	resyncInterval, backOffPeriod time.Duration, podCache kubecontainer.Cache) *podWorkers {
	return &podWorkers{
		podUpdates:                map[types.UID]chan UpdatePodOptions{},
		isWorking:                 map[types.UID]bool{},
		lastUndeliveredWorkUpdate: map[types.UID]UpdatePodOptions{},
		syncPodFn:                 syncPodFn,
		recorder:                  recorder,
		workQueue:                 workQueue,
		resyncInterval:            resyncInterval,
		backOffPeriod:             backOffPeriod,
		podCache:                  podCache,
	}
}

// 消费podUpdates中的消息，执行(kl *Kubelet) syncPod
// 执行完成后执行消息里的回调函数OnCompleteFunc
// 检测缓冲里是否消息
// 将uid执行错误加入到p.workQueue中并设置uid的过期时间
// 检查暂存器p.lastUndeliveredWorkUpdate[uid]是否有UpdatePodOptions
// 如果有，将p.lastUndeliveredWorkUpdate[uid]里的UpdatePodOptions发送到chan中，让worker处理
// 如果没有，则设置worker的状态p.isWorking[uid]为false，代表未在工作中（已经处理完chan中的UpdatePodOptions）
func (p *podWorkers) managePodLoop(podUpdates <-chan UpdatePodOptions) {
	var lastSyncTime time.Time
	for update := range podUpdates {
		err := func() error {
			podUID := update.Pod.UID
			// This is a blocking call that would return only if the cache
			// has an entry for the pod that is newer than minRuntimeCache
			// Time. This ensures the worker doesn't start syncing until
			// after the cache is at least newer than the finished time of
			// the previous sync.
			// 尝试从缓存中获取pod的runtime status缓存，成功则直接返回pod的runtime status和是否inspect pod发生错误
			// 否则添加一个订阅记录到p.podCache.subscribers[id]，等待chan中有数据，然后返回pod的runtime status和是否inspect pod发生错误
			status, err := p.podCache.GetNewerThan(podUID, lastSyncTime)
			if err != nil {
				// This is the legacy event thrown by manage pod loop
				// all other events are now dispatched from syncPodFn
				p.recorder.Eventf(update.Pod, v1.EventTypeWarning, events.FailedSync, "error determining status: %v", err)
				return err
			}
			// p.syncPodFn是(kl *Kubelet) syncPod
			err = p.syncPodFn(syncPodOptions{
				mirrorPod:      update.MirrorPod,
				pod:            update.Pod,
				podStatus:      status,
				killPodOptions: update.KillPodOptions,
				updateType:     update.UpdateType,
			})
			lastSyncTime = time.Now()
			return err
		}()
		// notify the call-back function if the operation succeeded or not
		if update.OnCompleteFunc != nil {
			update.OnCompleteFunc(err)
		}
		if err != nil {
			// IMPORTANT: we do not log errors here, the syncPodFn is responsible for logging errors
			klog.Errorf("Error syncing pod %s (%q), skipping: %v", update.Pod.UID, format.Pod(update.Pod), err)
		}
		// 将uid的执行错误加入到p.workQueue中并设置uid的过期时间
		// 检查暂存器p.lastUndeliveredWorkUpdate[uid]是否有UpdatePodOptions
		// 如果有，将p.lastUndeliveredWorkUpdate[uid]里的UpdatePodOptions发送到chan中，让worker处理
		// 如果没有，则设置worker的状态p.isWorking[uid]为false，代表未在工作中（已经处理完chan中的UpdatePodOptions）
		p.wrapUp(update.Pod.UID, err)
	}
}

// Apply the new setting to the specified pod.
// If the options provide an OnCompleteFunc, the function is invoked if the update is accepted.
// Update requests are ignored if a kill pod request is pending.
// 确保每个pod uid有一个goroutine来处理UpdatePodOptions
// 根据goroutine的工作状态，来处理UpdatePodOptions。如果goroutine不在工作中，则UpdatePodOptions投递到chan中（让goroutine处理）。如果goroutine在工作中则，将UpdatePodOption放到暂存器中（等待goroutine处理完当前消息后，再来处理）。
func (p *podWorkers) UpdatePod(options *UpdatePodOptions) {
	pod := options.Pod
	uid := pod.UID
	var podUpdates chan UpdatePodOptions
	var exists bool

	p.podLock.Lock()
	defer p.podLock.Unlock()
	// 每个pod uid会创建一个chan，和启动一个goroutine来读取这个chan里的UpdatePodOptions，进行处理
	if podUpdates, exists = p.podUpdates[uid]; !exists {
		// We need to have a buffer here, because checkForUpdates() method that
		// puts an update into channel is called from the same goroutine where
		// the channel is consumed. However, it is guaranteed that in such case
		// the channel is empty, so buffer of size 1 is enough.
		podUpdates = make(chan UpdatePodOptions, 1)
		p.podUpdates[uid] = podUpdates

		// Creating a new pod worker either means this is a new pod, or that the
		// kubelet just restarted. In either case the kubelet is willing to believe
		// the status of the pod for the first pod worker sync. See corresponding
		// comment in syncPod.
		go func() {
			defer runtime.HandleCrash()
			// 消费podUpdates中的消息，执行(kl *Kubelet) syncPod
			// 执行完成后执行消息里的回调函数OnCompleteFunc
			// 检测缓冲里是否消息
			// 将uid执行错误加入到p.workQueue中并设置uid的过期时间
			// 检查暂存器p.lastUndeliveredWorkUpdate[uid]是否有UpdatePodOptions
			// 如果有，将p.lastUndeliveredWorkUpdate[uid]里的UpdatePodOptions发送到chan中，让worker处理
			// 如果没有，则设置worker的状态p.isWorking[uid]为false，代表未在工作中（已经处理完chan中的UpdatePodOptions）
			p.managePodLoop(podUpdates)
		}()
	}
	// 是否将消息投递到chan中，还是暂时缓冲起来，等goroutine（p.managePodLoop方法）自己处理完（p.wrapUp方法）会去检测缓冲里是否消息，有消息则进行消费

	// 如果之前pod的goroutine处理完chan中的UpdatePodOptions或pod.UID不在p.isWorking里，则设置p.isWorking[pod.UID]为true，并发送option给chan
	if !p.isWorking[pod.UID] {
		p.isWorking[pod.UID] = true
		podUpdates <- *options
	} else {
		// if a request to kill a pod is pending, we do not let anything overwrite that request.
		// 如果之前的pod的goroutine未处理完chan中的UpdatePodOptions，则覆盖之前p.lastUndeliveredWorkUpdate里保存的pod对应的UpdatePodOptions
		// 之前p.lastUndeliveredWorkUpdate里保存的pod对应的UpdatePodOptions的UpdateType为kubetypes.SyncPodKill不会被覆盖，则这个UpdatePodOptions就丢弃
		update, found := p.lastUndeliveredWorkUpdate[pod.UID]
		if !found || update.UpdateType != kubetypes.SyncPodKill {
			p.lastUndeliveredWorkUpdate[pod.UID] = *options
		}
	}
}

func (p *podWorkers) removeWorker(uid types.UID) {
	if ch, ok := p.podUpdates[uid]; ok {
		close(ch)
		delete(p.podUpdates, uid)
		// If there is an undelivered work update for this pod we need to remove it
		// since per-pod goroutine won't be able to put it to the already closed
		// channel when it finishes processing the current work update.
		delete(p.lastUndeliveredWorkUpdate, uid)
	}
}
func (p *podWorkers) ForgetWorker(uid types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	p.removeWorker(uid)
}

func (p *podWorkers) ForgetNonExistingPodWorkers(desiredPods map[types.UID]sets.Empty) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	for key := range p.podUpdates {
		if _, exists := desiredPods[key]; !exists {
			p.removeWorker(key)
		}
	}
}

// 将uid执行错误加入到p.workQueue中并设置uid的过期时间
// 检查暂存器p.lastUndeliveredWorkUpdate[uid]是否有UpdatePodOptions
// 如果有，将p.lastUndeliveredWorkUpdate[uid]里的UpdatePodOptions发送到chan中，让worker处理
// 如果没有，则设置worker的状态p.isWorking[uid]为false，代表未在工作中（已经处理完chan中的UpdatePodOptions）
func (p *podWorkers) wrapUp(uid types.UID, syncErr error) {
	// Requeue the last update if the last sync returned error.
	switch {
	case syncErr == nil:
		// No error; requeue at the regular resync interval.
		// 把uid加入到queue中并设置uid的过期时间
		p.workQueue.Enqueue(uid, wait.Jitter(p.resyncInterval, workerResyncIntervalJitterFactor))
	case strings.Contains(syncErr.Error(), NetworkNotReadyErrorMsg):
		// Network is not ready; back off for short period of time and retry as network might be ready soon.
		// 错误里包含"network is not ready"
		// 把uid加入到queue中并设置uid的过期时间
		p.workQueue.Enqueue(uid, wait.Jitter(backOffOnTransientErrorPeriod, workerBackOffPeriodJitterFactor))
	default:
		// Error occurred during the sync; back off and then retry.
		// 把uid加入到queue中并设置uid的过期时间
		p.workQueue.Enqueue(uid, wait.Jitter(p.backOffPeriod, workerBackOffPeriodJitterFactor))
	}
	// 检查p.lastUndeliveredWorkUpdate[uid]是否有UpdatePodOptions，有的话，将p.lastUndeliveredWorkUpdate[uid]里的UpdatePodOptions发送到chan中，让worker处理
	// 否则，则设置worker的状态p.isWorking[uid]为false，代表未在工作中（已经处理完chan中的UpdatePodOptions）
	p.checkForUpdates(uid)
}

// 检查p.lastUndeliveredWorkUpdate[uid]是否有UpdatePodOptions，有的话，将p.lastUndeliveredWorkUpdate[uid]里的UpdatePodOptions发送到chan中，让worker处理
// 否则，则设置worker的状态p.isWorking[uid]为false，代表未在工作中（已经处理完chan中的UpdatePodOptions）
func (p *podWorkers) checkForUpdates(uid types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if workUpdate, exists := p.lastUndeliveredWorkUpdate[uid]; exists {
		p.podUpdates[uid] <- workUpdate
		delete(p.lastUndeliveredWorkUpdate, uid)
	} else {
		p.isWorking[uid] = false
	}
}

// killPodNow returns a KillPodFunc that can be used to kill a pod.
// It is intended to be injected into other modules that need to kill a pod.
// 返回利用pod worker机制来移除pod（停止pod里所有container和sandbox）函数
func killPodNow(podWorkers PodWorkers, recorder record.EventRecorder) eviction.KillPodFunc {
	return func(pod *v1.Pod, status v1.PodStatus, gracePeriodOverride *int64) error {
		// determine the grace period to use when killing the pod
		// gracePeriod默认为0
		// 参数里有设置gracePeriodOverride，则设置为参数里gracePeriodOverride值，否则pod.Spec.TerminationGracePeriodSeconds有则设置这个值
		gracePeriod := int64(0)
		if gracePeriodOverride != nil {
			gracePeriod = *gracePeriodOverride
		} else if pod.Spec.TerminationGracePeriodSeconds != nil {
			gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
		}

		// we timeout and return an error if we don't get a callback within a reasonable time.
		// the default timeout is relative to the grace period (we settle on 10s to wait for kubelet->runtime traffic to complete in sigkill)
		// timeout为1.5个gracePeriod，最小的timeout为10s
		timeout := int64(gracePeriod + (gracePeriod / 2))
		minTimeout := int64(10)
		if timeout < minTimeout {
			timeout = minTimeout
		}
		timeoutDuration := time.Duration(timeout) * time.Second

		// open a channel we block against until we get a result
		type response struct {
			err error
		}
		ch := make(chan response, 1)
		podWorkers.UpdatePod(&UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
			OnCompleteFunc: func(err error) {
				ch <- response{err: err}
			},
			KillPodOptions: &KillPodOptions{
				// 这个用于更新kl.statusManager里的pod的status
				PodStatusFunc: func(p *v1.Pod, podStatus *kubecontainer.PodStatus) v1.PodStatus {
					return status
				},
				// PodTerminationGracePeriodSecondsOverride为调用cri超时时间（在runtimeService里，默认为2分钟+gracePeriod），如果runtime为dockershim，这个gracePeriod超时时间会传给docker作为docker stop的graceful时间
				PodTerminationGracePeriodSecondsOverride: gracePeriodOverride,
			},
		})

		// wait for either a response, or a timeout
		select {
		// 等待podWorker里的kl.syncPod执行完，返回err可能为nil
		case r := <-ch:
			return r.err
		// timeout为1.5个gracePeriod，最小的timeout为10s
		case <-time.After(timeoutDuration):
			recorder.Eventf(pod, v1.EventTypeWarning, events.ExceededGracePeriod, "Container runtime did not kill the pod within specified grace period.")
			return fmt.Errorf("timeout waiting to kill pod")
		}
	}
}
