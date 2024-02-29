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
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
)

// OnCompleteFunc is a function that is invoked when an operation completes.
// If err is non-nil, the operation did not complete successfully.
type OnCompleteFunc func(err error)

// PodStatusFunc is a function that is invoked to override the pod status when a pod is killed.
type PodStatusFunc func(podStatus *v1.PodStatus)

// KillPodOptions are options when performing a pod update whose update type is kill.
type KillPodOptions struct {
	// CompletedCh is closed when the kill request completes (syncTerminatingPod has completed
	// without error) or if the pod does not exist, or if the pod has already terminated. This
	// could take an arbitrary amount of time to be closed, but is never left open once
	// CouldHaveRunningContainers() returns false.
	CompletedCh chan<- struct{}
	// Evict is true if this is a pod triggered eviction - once a pod is evicted some resources are
	// more aggressively reaped than during normal pod operation (stopped containers).
	Evict bool
	// PodStatusFunc is invoked (if set) and overrides the status of the pod at the time the pod is killed.
	// The provided status is populated from the latest state.
	PodStatusFunc PodStatusFunc
	// PodTerminationGracePeriodSecondsOverride is optional override to use if a pod is being killed as part of kill operation.
	PodTerminationGracePeriodSecondsOverride *int64
}

// UpdatePodOptions is an options struct to pass to a UpdatePod operation.
type UpdatePodOptions struct {
	// The type of update (create, update, sync, kill).
	UpdateType kubetypes.SyncPodType
	// StartTime is an optional timestamp for when this update was created. If set,
	// when this update is fully realized by the pod worker it will be recorded in
	// the PodWorkerDuration metric.
	StartTime time.Time
	// Pod to update. Required.
	Pod *v1.Pod
	// MirrorPod is the mirror pod if Pod is a static pod. Optional when UpdateType
	// is kill or terminated.
	MirrorPod *v1.Pod
	// RunningPod is a runtime pod that is no longer present in config. Required
	// if Pod is nil, ignored if Pod is set.
	RunningPod *kubecontainer.Pod
	// KillPodOptions is used to override the default termination behavior of the
	// pod or to update the pod status after an operation is completed. Since a
	// pod can be killed for multiple reasons, PodStatusFunc is invoked in order
	// and later kills have an opportunity to override the status (i.e. a preemption
	// may be later turned into an eviction).
	KillPodOptions *KillPodOptions
}

// PodWorkType classifies the three phases of pod lifecycle - setup (sync),
// teardown of containers (terminating), cleanup (terminated).
type PodWorkType int

const (
	// SyncPodWork is when the pod is expected to be started and running.
	SyncPodWork PodWorkType = iota
	// TerminatingPodWork is when the pod is no longer being set up, but some
	// containers may be running and are being torn down.
	TerminatingPodWork
	// TerminatedPodWork indicates the pod is stopped, can have no more running
	// containers, and any foreground cleanup can be executed.
	TerminatedPodWork
)

// PodWorkType classifies the status of pod as seen by the pod worker - setup (sync),
// teardown of containers (terminating), cleanup (terminated), or recreated with the
// same UID (kill -> create while terminating)
type PodWorkerState int

const (
	// SyncPod is when the pod is expected to be started and running.
	SyncPod PodWorkerState = iota
	// TerminatingPod is when the pod is no longer being set up, but some
	// containers may be running and are being torn down.
	TerminatingPod
	// TerminatedPod indicates the pod is stopped, can have no more running
	// containers, and any foreground cleanup can be executed.
	TerminatedPod
	// TerminatedAndRecreatedPod indicates that after the pod was terminating a
	// request to recreate the pod was received. The pod is terminated and can
	// now be restarted by sending a create event to the pod worker.
	TerminatedAndRecreatedPod
)

// podWork is the internal changes
type podWork struct {
	// WorkType is the type of sync to perform - sync (create), terminating (stop
	// containers), terminated (clean up and write status).
	WorkType PodWorkType

	// Options contains the data to sync.
	Options UpdatePodOptions
}

// PodWorkers is an abstract interface for testability.
type PodWorkers interface {
	// UpdatePod notifies the pod worker of a change to a pod, which will then
	// be processed in FIFO order by a goroutine per pod UID. The state of the
	// pod will be passed to the syncPod method until either the pod is marked
	// as deleted, it reaches a terminal phase (Succeeded/Failed), or the pod
	// is evicted by the kubelet. Once that occurs the syncTerminatingPod method
	// will be called until it exits successfully, and after that all further
	// UpdatePod() calls will be ignored for that pod until it has been forgotten
	// due to significant time passing. A pod that is terminated will never be
	// restarted.
	UpdatePod(options UpdatePodOptions)
	// SyncKnownPods removes workers for pods that are not in the desiredPods set
	// and have been terminated for a significant period of time. Once this method
	// has been called once, the workers are assumed to be fully initialized and
	// subsequent calls to ShouldPodContentBeRemoved on unknown pods will return
	// true. It returns a map describing the state of each known pod worker.
	SyncKnownPods(desiredPods []*v1.Pod) map[types.UID]PodWorkerState

	// IsPodKnownTerminated returns true if the provided pod UID is known by the pod
	// worker to be terminated. If the pod has been force deleted and the pod worker
	// has completed termination this method will return false, so this method should
	// only be used to filter out pods from the desired set such as in admission.
	//
	// Intended for use by the kubelet config loops, but not subsystems, which should
	// use ShouldPod*().
	IsPodKnownTerminated(uid types.UID) bool
	// CouldHaveRunningContainers returns true before the pod workers have synced,
	// once the pod workers see the pod (syncPod could be called), and returns false
	// after the pod has been terminated (running containers guaranteed stopped).
	//
	// Intended for use by the kubelet config loops, but not subsystems, which should
	// use ShouldPod*().
	CouldHaveRunningContainers(uid types.UID) bool
	// IsPodTerminationRequested returns true when pod termination has been requested
	// until the termination completes and the pod is removed from config. This should
	// not be used in cleanup loops because it will return false if the pod has already
	// been cleaned up - use ShouldPodContainersBeTerminating instead. Also, this method
	// may return true while containers are still being initialized by the pod worker.
	//
	// Intended for use by the kubelet sync* methods, but not subsystems, which should
	// use ShouldPod*().
	IsPodTerminationRequested(uid types.UID) bool

	// ShouldPodContainersBeTerminating returns false before pod workers have synced,
	// or once a pod has started terminating. This check is similar to
	// ShouldPodRuntimeBeRemoved but is also true after pod termination is requested.
	//
	// Intended for use by subsystem sync loops to avoid performing background setup
	// after termination has been requested for a pod. Callers must ensure that the
	// syncPod method is non-blocking when their data is absent.
	ShouldPodContainersBeTerminating(uid types.UID) bool
	// ShouldPodRuntimeBeRemoved returns true if runtime managers within the Kubelet
	// should aggressively cleanup pod resources that are not containers or on disk
	// content, like attached volumes. This is true when a pod is not yet observed
	// by a worker after the first sync (meaning it can't be running yet) or after
	// all running containers are stopped.
	// TODO: Once pod logs are separated from running containers, this method should
	// be used to gate whether containers are kept.
	//
	// Intended for use by subsystem sync loops to know when to start tearing down
	// resources that are used by running containers. Callers should ensure that
	// runtime content they own is not required for post-termination - for instance
	// containers are required in docker to preserve pod logs until after the pod
	// is deleted.
	ShouldPodRuntimeBeRemoved(uid types.UID) bool
	// ShouldPodContentBeRemoved returns true if resource managers within the Kubelet
	// should aggressively cleanup all content related to the pod. This is true
	// during pod eviction (when we wish to remove that content to free resources)
	// as well as after the request to delete a pod has resulted in containers being
	// stopped (which is a more graceful action). Note that a deleting pod can still
	// be evicted.
	//
	// Intended for use by subsystem sync loops to know when to start tearing down
	// resources that are used by non-deleted pods. Content is generally preserved
	// until deletion+removal_from_etcd or eviction, although garbage collection
	// can free content when this method returns false.
	ShouldPodContentBeRemoved(uid types.UID) bool
	// IsPodForMirrorPodTerminatingByFullName returns true if a static pod with the
	// provided pod name is currently terminating and has yet to complete. It is
	// intended to be used only during orphan mirror pod cleanup to prevent us from
	// deleting a terminating static pod from the apiserver before the pod is shut
	// down.
	IsPodForMirrorPodTerminatingByFullName(podFullname string) bool
}

// the function to invoke to perform a sync (reconcile the kubelet state to the desired shape of the pod)
type syncPodFnType func(ctx context.Context, updateType kubetypes.SyncPodType, pod *v1.Pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error)

// the function to invoke to terminate a pod (ensure no running processes are present)
type syncTerminatingPodFnType func(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, runningPod *kubecontainer.Pod, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error

// the function to invoke to cleanup a pod that is terminated
type syncTerminatedPodFnType func(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error

const (
	// jitter factor for resyncInterval
	workerResyncIntervalJitterFactor = 0.5

	// jitter factor for backOffPeriod and backOffOnTransientErrorPeriod
	workerBackOffPeriodJitterFactor = 0.5

	// backoff period when transient error occurred.
	backOffOnTransientErrorPeriod = time.Second
)

// podSyncStatus tracks per-pod transitions through the three phases of pod
// worker sync (setup, terminating, terminated).
type podSyncStatus struct {
	// ctx is the context that is associated with the current pod sync.
	ctx context.Context
	// cancelFn if set is expected to cancel the current sync*Pod operation.
	cancelFn context.CancelFunc
	// working is true if a pod worker is currently in a sync method.
	working bool
	// fullname of the pod
	fullname string

	// syncedAt is the time at which the pod worker first observed this pod.
	syncedAt time.Time
	// terminatingAt is set once the pod is requested to be killed - note that
	// this can be set before the pod worker starts terminating the pod, see
	// terminating.
	terminatingAt time.Time
	// startedTerminating is true once the pod worker has observed the request to
	// stop a pod (exited syncPod and observed a podWork with WorkType
	// TerminatingPodWork). Once this is set, it is safe for other components
	// of the kubelet to assume that no other containers may be started.
	startedTerminating bool
	// deleted is true if the pod has been marked for deletion on the apiserver
	// or has no configuration represented (was deleted before).
	deleted bool
	// gracePeriod is the requested gracePeriod once terminatingAt is nonzero.
	gracePeriod int64
	// evicted is true if the kill indicated this was an eviction (an evicted
	// pod can be more aggressively cleaned up).
	evicted bool
	// terminatedAt is set once the pod worker has completed a successful
	// syncTerminatingPod call and means all running containers are stopped.
	terminatedAt time.Time
	// finished is true once the pod worker completes for a pod
	// (syncTerminatedPod exited with no errors) until SyncKnownPods is invoked
	// to remove the pod. A terminal pod (Succeeded/Failed) will have
	// termination status until the pod is deleted.
	finished bool
	// restartRequested is true if the pod worker was informed the pod is
	// expected to exist (update type of create, update, or sync) after
	// it has been killed. When known pods are synced, any pod that is
	// terminated and has restartRequested will have its history cleared.
	restartRequested bool
	// notifyPostTerminating will be closed once the pod transitions to
	// terminated. After the pod is in terminated state, nothing should be
	// added to this list.
	notifyPostTerminating []chan<- struct{}
	// statusPostTerminating is a list of the status changes associated
	// with kill pod requests. After the pod is in terminated state, nothing
	// should be added to this list. The worker will execute the last function
	// in this list on each termination attempt.
	statusPostTerminating []PodStatusFunc
}

// 返回s.working的值
func (s *podSyncStatus) IsWorking() bool              { return s.working }
// s.terminatingAt不为0值返回true
func (s *podSyncStatus) IsTerminationRequested() bool { return !s.terminatingAt.IsZero() }
// 返回s.startedTerminating的值
func (s *podSyncStatus) IsTerminationStarted() bool   { return s.startedTerminating }
// s.terminatedAt不为0值返回true
func (s *podSyncStatus) IsTerminated() bool           { return !s.terminatedAt.IsZero() }
// 返回s.finished的值
func (s *podSyncStatus) IsFinished() bool             { return s.finished }
// 返回s.evicted的值
func (s *podSyncStatus) IsEvicted() bool              { return s.evicted }
// 返回s.deleted的值
func (s *podSyncStatus) IsDeleted() bool              { return s.deleted }

// podWorkers keeps track of operations on pods and ensures each pod is
// reconciled with the container runtime and other subsystems. The worker
// also tracks which pods are in flight for starting, which pods are
// shutting down but still have running containers, and which pods have
// terminated recently and are guaranteed to have no running containers.
//
// A pod passed to a pod worker is either being synced (expected to be
// running), terminating (has running containers but no new containers are
// expected to start), terminated (has no running containers but may still
// have resources being consumed), or cleaned up (no resources remaining).
// Once a pod is set to be "torn down" it cannot be started again for that
// UID (corresponding to a delete or eviction) until:
//
//  1. The pod worker is finalized (syncTerminatingPod and
//     syncTerminatedPod exit without error sequentially)
//  2. The SyncKnownPods method is invoked by kubelet housekeeping and the pod
//     is not part of the known config.
//
// Pod workers provide a consistent source of information to other kubelet
// loops about the status of the pod and whether containers can be
// running. The ShouldPodContentBeRemoved() method tracks whether a pod's
// contents should still exist, which includes non-existent pods after
// SyncKnownPods() has been called once (as per the contract, all existing
// pods should be provided via UpdatePod before SyncKnownPods is invoked).
// Generally other sync loops are expected to separate "setup" and
// "teardown" responsibilities and the information methods here assist in
// each by centralizing that state. A simple visualization of the time
// intervals involved might look like:
//
// ---|                                         = kubelet config has synced at least once
// -------|                                  |- = pod exists in apiserver config
// --------|                  |---------------- = CouldHaveRunningContainers() is true
//
//	^- pod is observed by pod worker  .
//	.                                 .
//
// ----------|       |------------------------- = syncPod is running
//
//	. ^- pod worker loop sees change and invokes syncPod
//	. .                               .
//
// --------------|                     |------- = ShouldPodContainersBeTerminating() returns true
// --------------|                     |------- = IsPodTerminationRequested() returns true (pod is known)
//
//	. .   ^- Kubelet evicts pod       .
//	. .                               .
//
// -------------------|       |---------------- = syncTerminatingPod runs then exits without error
//
//	        . .        ^ pod worker loop exits syncPod, sees pod is terminating,
//					 . .          invokes syncTerminatingPod
//	        . .                               .
//
// ---|    |------------------|              .  = ShouldPodRuntimeBeRemoved() returns true (post-sync)
//
//	.                ^ syncTerminatingPod has exited successfully
//	.                               .
//
// ----------------------------|       |------- = syncTerminatedPod runs then exits without error
//
//	.                         ^ other loops can tear down
//	.                               .
//
// ------------------------------------|  |---- = status manager is waiting for PodResourcesAreReclaimed()
//
//	.                         ^     .
//
// ----------|                               |- = status manager can be writing pod status
//
//	^ status manager deletes pod because no longer exists in config
//
// Other components in the Kubelet can request a termination of the pod
// via the UpdatePod method or the killPodNow wrapper - this will ensure
// the components of the pod are stopped until the kubelet is restarted
// or permanently (if the phase of the pod is set to a terminal phase
// in the pod status change).
type podWorkers struct {
	// Protects all per worker fields.
	podLock sync.Mutex
	// podsSynced is true once the pod worker has been synced at least once,
	// which means that all working pods have been started via UpdatePod().
	podsSynced bool
	// Tracks all running per-pod goroutines - per-pod goroutine will be
	// processing updates received through its corresponding channel.
	podUpdates map[types.UID]chan podWork
	// Tracks the last undelivered work item for this pod - a work item is
	// undelivered if it comes in while the worker is working.
	lastUndeliveredWorkUpdate map[types.UID]podWork
	// Tracks by UID the termination status of a pod - syncing, terminating,
	// terminated, and evicted.
	podSyncStatuses map[types.UID]*podSyncStatus
	// Tracks all uids for started static pods by full name
	startedStaticPodsByFullname map[string]types.UID
	// Tracks all uids for static pods that are waiting to start by full name
	waitingToStartStaticPodsByFullname map[string][]types.UID

	workQueue queue.WorkQueue

	// This function is run to sync the desired state of pod.
	// NOTE: This function has to be thread-safe - it can be called for
	// different pods at the same time.

	syncPodFn            syncPodFnType
	syncTerminatingPodFn syncTerminatingPodFnType
	syncTerminatedPodFn  syncTerminatedPodFnType

	// workerChannelFn is exposed for testing to allow unit tests to impose delays
	// in channel communication. The function is invoked once each time a new worker
	// goroutine starts.
	workerChannelFn func(uid types.UID, in chan podWork) (out <-chan podWork)

	// The EventRecorder to use
	recorder record.EventRecorder

	// backOffPeriod is the duration to back off when there is a sync error.
	backOffPeriod time.Duration

	// resyncInterval is the duration to wait until the next sync.
	resyncInterval time.Duration

	// podCache stores kubecontainer.PodStatus for all pods.
	podCache kubecontainer.Cache
}

func newPodWorkers(
	syncPodFn syncPodFnType,
	syncTerminatingPodFn syncTerminatingPodFnType,
	syncTerminatedPodFn syncTerminatedPodFnType,
	recorder record.EventRecorder,
	workQueue queue.WorkQueue,
	resyncInterval, backOffPeriod time.Duration,
	podCache kubecontainer.Cache,
) PodWorkers {
	return &podWorkers{
		podSyncStatuses:                    map[types.UID]*podSyncStatus{},
		podUpdates:                         map[types.UID]chan podWork{},
		lastUndeliveredWorkUpdate:          map[types.UID]podWork{},
		startedStaticPodsByFullname:        map[string]types.UID{},
		waitingToStartStaticPodsByFullname: map[string][]types.UID{},
		syncPodFn:                          syncPodFn,
		syncTerminatingPodFn:               syncTerminatingPodFn,
		syncTerminatedPodFn:                syncTerminatedPodFn,
		recorder:                           recorder,
		workQueue:                          workQueue,
		resyncInterval:                     resyncInterval,
		backOffPeriod:                      backOffPeriod,
		podCache:                           podCache,
	}
}

func (p *podWorkers) IsPodKnownTerminated(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return status.IsTerminated()
	}
	// if the pod is not known, we return false (pod worker is not aware of it)
	return false
}

// 如果uid在p.podSyncStatuses里，返回podWorker是否不处在terminated状态
// 否则在"至少一个pod worker执行了syncPod，即已经有pod通过UpdatePod()启动"，返回false，否则返回true
func (p *podWorkers) CouldHaveRunningContainers(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return !status.IsTerminated()
	}
	// once all pods are synced, any pod without sync status is known to not be running.
	return !p.podsSynced
}

// 如果uid在p.podSyncStatuses里，返回podWorker是否处在terminating状态
// 否则，返回false
func (p *podWorkers) IsPodTerminationRequested(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		// the pod may still be setting up at this point.
		return status.IsTerminationRequested()
	}
	// an unknown pod is considered not to be terminating (use ShouldPodContainersBeTerminating in
	// cleanup loops to avoid failing to cleanup pods that have already been removed from config)
	return false
}

func (p *podWorkers) ShouldPodContainersBeTerminating(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		// we wait until the pod worker goroutine observes the termination, which means syncPod will not
		// be executed again, which means no new containers can be started
		return status.IsTerminationStarted()
	}
	// once we've synced, if the pod isn't known to the workers we should be tearing them
	// down
	return p.podsSynced
}

// 如果uid在p.podSyncStatuses里，返回是否terminated状态
// 否则在"至少一个pod worker执行了syncPod，即已经有pod通过UpdatePod()启动"，返回true，否则返回false
func (p *podWorkers) ShouldPodRuntimeBeRemoved(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return status.IsTerminated()
	}
	// a pod that hasn't been sent to the pod worker yet should have no runtime components once we have
	// synced all content.
	return p.podsSynced
}

// 如果uid在p.podSyncStatuses里，则当pod是被驱逐或"pod被删除且处于terminated状态"，返回true，否则返回false
// 否则在"至少一个pod worker执行了syncPod，即已经有pod通过UpdatePod()启动"，返回true，否则返回false
func (p *podWorkers) ShouldPodContentBeRemoved(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return status.IsEvicted() || (status.IsDeleted() && status.IsTerminated())
	}
	// a pod that hasn't been sent to the pod worker yet should have no content on disk once we have
	// synced all content.
	return p.podsSynced
}

// podFullName在p.startedStaticPodsByFullname里且对应的uid在 p.podSyncStatuses，且状态为terminating，则返回true
// 其他返回false
func (p *podWorkers) IsPodForMirrorPodTerminatingByFullName(podFullName string) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	uid, started := p.startedStaticPodsByFullname[podFullName]
	if !started {
		return false
	}
	status, exists := p.podSyncStatuses[uid]
	if !exists {
		return false
	}
	if !status.IsTerminationRequested() || status.IsTerminated() {
		return false
	}

	return true
}

// 运行的container数量为0，且Read状态的sandbox数量为0，返回true
func isPodStatusCacheTerminal(status *kubecontainer.PodStatus) bool {
	runningContainers := 0
	runningSandboxes := 0
	for _, container := range status.ContainerStatuses {
		if container.State == kubecontainer.ContainerStateRunning {
			runningContainers++
		}
	}
	for _, sb := range status.SandboxStatuses {
		if sb.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			runningSandboxes++
		}
	}
	return runningContainers == 0 && runningSandboxes == 0
}

// UpdatePod carries a configuration change or termination state to a pod. A pod is either runnable,
// terminating, or terminated, and will transition to terminating if deleted on the apiserver, it is
// discovered to have a terminal phase (Succeeded or Failed), or if it is evicted by the kubelet.
func (p *podWorkers) UpdatePod(options UpdatePodOptions) {
	// handle when the pod is an orphan (no config) and we only have runtime status by running only
	// the terminating part of the lifecycle
	pod := options.Pod
	var isRuntimePod bool
	// options.RunningPod不为nil且options.Pod为nil
	//   如果options.UpdateType不为SyncPodKill，则直接返回
	//   否则重新设置options.Pod为options.RunningPod转成api的Pod
	// options.RunningPod不为nil且options.Pod不为nil，则设置options.RunningPod为nil
	if options.RunningPod != nil {
		// options.RunningPod不为nil且options.Pod为nil
		if options.Pod == nil {
			// 内部Pod类型转成api的Pod
			pod = options.RunningPod.ToAPIPod()
			// options.UpdateType不为SyncPodKill，则直接返回
			if options.UpdateType != kubetypes.SyncPodKill {
				klog.InfoS("Pod update is ignored, runtime pods can only be killed", "pod", klog.KObj(pod), "podUID", pod.UID)
				return
			}
			// 重新设置options.Pod
			options.Pod = pod
			isRuntimePod = true
		} else {
			// options.RunningPod不为nil且options.Pod不为nil，则设置options.RunningPod为nil
			options.RunningPod = nil
			klog.InfoS("Pod update included RunningPod which is only valid when Pod is not specified", "pod", klog.KObj(options.Pod), "podUID", options.Pod.UID)
		}
	}
	uid := pod.UID

	p.podLock.Lock()
	defer p.podLock.Unlock()

	// decide what to do with this pod - we are either setting it up, tearing it down, or ignoring it
	now := time.Now()
	status, ok := p.podSyncStatuses[uid]
	// pod第一次被sync（在p.podSyncStatuses里没有记录）
	if !ok {
		klog.V(4).InfoS("Pod is being synced for the first time", "pod", klog.KObj(pod), "podUID", pod.UID)
		status = &podSyncStatus{
			syncedAt: now,
			// {pod name}_{pod namespace}
			fullname: kubecontainer.GetPodFullName(pod),
		}
		// if this pod is being synced for the first time, we need to make sure it is an active pod
		// pod不是runtimePod（options.RunningPod不为nil且options.Pod为nil），且pod的phase为failed或succeed，则检查pod是否已经处于终止状态
		if !isRuntimePod && (pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded) {
			// check to see if the pod is not running and the pod is terminal.
			// If this succeeds then record in the podWorker that it is terminated.
			// 从缓存中获得pod的运行时状态（有运行时状态数据）
			if statusCache, err := p.podCache.Get(pod.UID); err == nil {
				// 运行的container数量为0，且Read状态的sandbox数量为0
				if isPodStatusCacheTerminal(statusCache) {
					// podWorker最终的状态（代表podWorkers已经处理完成）
					status = &podSyncStatus{
						terminatedAt:       now,
						terminatingAt:      now,
						syncedAt:           now,
						startedTerminating: true,
						finished:           true,
						fullname:           kubecontainer.GetPodFullName(pod),
					}
				}
			}
		}
		p.podSyncStatuses[uid] = status
	}

	// if an update is received that implies the pod should be running, but we are already terminating a pod by
	// that UID, assume that two pods with the same UID were created in close temporal proximity (usually static
	// pod but it's possible for an apiserver to extremely rarely do something similar) - flag the sync status
	// to indicate that after the pod terminates it should be reset to "not running" to allow a subsequent add/update
	// to start the pod worker again
	// status.terminatingAt不为0值，且options.UpdateType为kubetypes.SyncPodCreate（代表pod要生成容器），则设置status.restartRequested为true，然后返回
	// 补充：
	// (kl *Kubelet) syncLoopIteration里houseKeeping（每2秒执行一次）会调用kubelet.HandlePodCleanups--》SyncKnownPods里会判断status.restartRequested为true话，且pod worker已完成，则执行清理
	// 从p.podSyncStatuses中删除uid
	// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
	// 并从p.lastUndeliveredWorkUpdate移除uid
	// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
	if status.IsTerminationRequested() {
		if options.UpdateType == kubetypes.SyncPodCreate {
			status.restartRequested = true
			klog.V(4).InfoS("Pod is terminating but has been requested to restart with same UID, will be reconciled later", "pod", klog.KObj(pod), "podUID", pod.UID)
			return
		}
	}

	// once a pod is terminated by UID, it cannot reenter the pod worker (until the UID is purged by housekeeping)
	// podWorker已经处理完成，直接返回
	if status.IsFinished() {
		klog.V(4).InfoS("Pod is finished processing, no further updates", "pod", klog.KObj(pod), "podUID", pod.UID)
		return
	}

	// check for a transition to terminating
	var becameTerminating bool
	// podWorker不处于正在terminating状态
	// 下面这几种情况设置status.terminatingAt为now，代表即将开始关闭容器（进入terminating状态）
	// 1. options.RunningPod不为nil且options.Pod为nil
	// 2. pod.DeletionTimestamp不为nil
	// 3. pod的phase为failed或succeed
	// 4. options.UpdateType为kubetypes.SyncPodKill
	if !status.IsTerminationRequested() {
		switch {
		// options.RunningPod不为nil且options.Pod为nil
		case isRuntimePod:
			klog.V(4).InfoS("Pod is orphaned and must be torn down", "pod", klog.KObj(pod), "podUID", pod.UID)
			status.deleted = true
			status.terminatingAt = now
			becameTerminating = true
		case pod.DeletionTimestamp != nil:
			klog.V(4).InfoS("Pod is marked for graceful deletion, begin teardown", "pod", klog.KObj(pod), "podUID", pod.UID)
			status.deleted = true
			status.terminatingAt = now
			becameTerminating = true
		case pod.Status.Phase == v1.PodFailed, pod.Status.Phase == v1.PodSucceeded:
			klog.V(4).InfoS("Pod is in a terminal phase (success/failed), begin teardown", "pod", klog.KObj(pod), "podUID", pod.UID)
			status.terminatingAt = now
			becameTerminating = true
		case options.UpdateType == kubetypes.SyncPodKill:
			if options.KillPodOptions != nil && options.KillPodOptions.Evict {
				klog.V(4).InfoS("Pod is being evicted by the kubelet, begin teardown", "pod", klog.KObj(pod), "podUID", pod.UID)
				status.evicted = true
			} else {
				klog.V(4).InfoS("Pod is being removed by the kubelet, begin teardown", "pod", klog.KObj(pod), "podUID", pod.UID)
			}
			status.terminatingAt = now
			becameTerminating = true
		}
	}

	// once a pod is terminating, all updates are kills and the grace period can only decrease
	var workType PodWorkType
	var wasGracePeriodShortened bool
	switch {
	// podWorker已经完成了terminating处于terminated状态
	case status.IsTerminated():
		// A terminated pod may still be waiting for cleanup - if we receive a runtime pod kill request
		// due to housekeeping seeing an older cached version of the runtime pod simply ignore it until
		// after the pod worker completes.
		// options.RunningPod不为nil且options.Pod为nil，直接返回
		if isRuntimePod {
			klog.V(3).InfoS("Pod is waiting for termination, ignoring runtime-only kill until after pod worker is fully terminated", "pod", klog.KObj(pod), "podUID", pod.UID)
			return
		}

		workType = TerminatedPodWork

		if options.KillPodOptions != nil {
			if ch := options.KillPodOptions.CompletedCh; ch != nil {
				close(ch)
			}
		}
		options.KillPodOptions = nil

	// podWorker处于正在terminating状态
	case status.IsTerminationRequested():
		workType = TerminatingPodWork
		if options.KillPodOptions == nil {
			options.KillPodOptions = &KillPodOptions{}
		}

		if ch := options.KillPodOptions.CompletedCh; ch != nil {
			status.notifyPostTerminating = append(status.notifyPostTerminating, ch)
		}
		if fn := options.KillPodOptions.PodStatusFunc; fn != nil {
			status.statusPostTerminating = append(status.statusPostTerminating, fn)
		}

		// 当status.gracePeriod（只计算不为0情况，为0认为没有设置）、pod.DeletionGracePeriodSeconds、options.PodTerminationGracePeriodSecondsOverride都设置了取最小的
		// 上面的都没有设置，则gracePeriod为*pod.Spec.TerminationGracePeriodSeconds
		// gracePeriod为0，则设置为1
		// 返回最后的gracePeriod，和判断是否"status.gracePeriod不为0且最后的gracePeriod和status.gracePeriod不一样（返回true）"
		gracePeriod, gracePeriodShortened := calculateEffectiveGracePeriod(status, pod, options.KillPodOptions)

		// 是否“status.gracePeriod不为0，但是gracePeriod被其他设置覆盖（pod.DeletionGracePeriodSeconds、options.PodTerminationGracePeriodSecondsOverride）成更小的”
		wasGracePeriodShortened = gracePeriodShortened
		status.gracePeriod = gracePeriod
		// always set the grace period for syncTerminatingPod so we don't have to recalculate,
		// will never be zero.
		options.KillPodOptions.PodTerminationGracePeriodSecondsOverride = &gracePeriod

	default:
		workType = SyncPodWork

		// KillPodOptions is not valid for sync actions outside of the terminating phase
		if options.KillPodOptions != nil {
			if ch := options.KillPodOptions.CompletedCh; ch != nil {
				close(ch)
			}
			options.KillPodOptions = nil
		}
	}

	// the desired work we want to be performing
	work := podWork{
		WorkType: workType,
		Options:  options,
	}

	// start the pod worker goroutine if it doesn't exist
	// 判断pod uid对应的chan是否存在
	podUpdates, exists := p.podUpdates[uid]
	if !exists {
		// We need to have a buffer here, because checkForUpdates() method that
		// puts an update into channel is called from the same goroutine where
		// the channel is consumed. However, it is guaranteed that in such case
		// the channel is empty, so buffer of size 1 is enough.
		podUpdates = make(chan podWork, 1)
		p.podUpdates[uid] = podUpdates

		// ensure that static pods start in the order they are received by UpdatePod
		// pod是static pod
		if kubetypes.IsStaticPod(pod) {
			p.waitingToStartStaticPodsByFullname[status.fullname] =
				append(p.waitingToStartStaticPodsByFullname[status.fullname], uid)
		}

		// allow testing of delays in the pod update channel
		var outCh <-chan podWork
		if p.workerChannelFn != nil {
			// 包装一下chan，让它可以记录延迟
			outCh = p.workerChannelFn(uid, podUpdates)
		} else {
			outCh = podUpdates
		}

		// Creating a new pod worker either means this is a new pod, or that the
		// kubelet just restarted. In either case the kubelet is willing to believe
		// the status of the pod for the first pod worker sync. See corresponding
		// comment in syncPod.
		go func() {
			defer runtime.HandleCrash()
			p.managePodLoop(outCh)
		}()
	}

	// dispatch a request to the pod worker if none are running
	// podWorker不在工作状态，则设置为工作状态（status.working为true），然后发送podWork到chan，直接返回
	if !status.IsWorking() {
		status.working = true
		podUpdates <- work
		return
	}

	// capture the maximum latency between a requested update and when the pod
	// worker observes it
	// pod uid在p.lastUndeliveredWorkUpdate已经存在最后未处理的podWork
	if undelivered, ok := p.lastUndeliveredWorkUpdate[pod.UID]; ok {
		// track the max latency between when a config change is requested and when it is realized
		// NOTE: this undercounts the latency when multiple requests are queued, but captures max latency
		// 已经存在未处理的podWork的Options.StartTime早于现在的podWork.Options.StartTime，则现在的podWork.Options.StartTime更新为已经存在未处理的podWork的Options.StartTime（为了记录最长的delay时间）
		if !undelivered.Options.StartTime.IsZero() && undelivered.Options.StartTime.Before(work.Options.StartTime) {
			work.Options.StartTime = undelivered.Options.StartTime
		}
	}

	// always sync the most recent data
	p.lastUndeliveredWorkUpdate[pod.UID] = work

	// podWorker从不是terminating状态转为terminating状态或者"status.gracePeriod不为0，但是gracePeriod被其他设置覆盖（pod.DeletionGracePeriodSeconds、options.PodTerminationGracePeriodSecondsOverride）成更小的"，且设置了status.cancelFn，则执行status.cancelFn
	// 即podWorker在工作中导致现在的PodWork未处理，且如果podWorker从不是terminating状态转为terminating状态或者"status.gracePeriod不为0，但是gracePeriod被其他设置覆盖（pod.DeletionGracePeriodSeconds、options.PodTerminationGracePeriodSecondsOverride）成更小的"，则执行取消context操作
	if (becameTerminating || wasGracePeriodShortened) && status.cancelFn != nil {
		klog.V(3).InfoS("Cancelling current pod sync", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", work.WorkType)
		status.cancelFn()
		return
	}
}

// calculateEffectiveGracePeriod sets the initial grace period for a newly terminating pod or allows a
// shorter grace period to be provided, returning the desired value.
// 当status.gracePeriod（只计算不为0情况，为0认为没有设置）、pod.DeletionGracePeriodSeconds、options.PodTerminationGracePeriodSecondsOverride都设置了取最小的
// 上面的都没有设置，则gracePeriod为*pod.Spec.TerminationGracePeriodSeconds
// gracePeriod为0，则设置为1
// 返回最后的gracePeriod，和判断是否"status.gracePeriod不为0且最后的gracePeriod和status.gracePeriod不一样（返回true）"
func calculateEffectiveGracePeriod(status *podSyncStatus, pod *v1.Pod, options *KillPodOptions) (int64, bool) {
	// enforce the restriction that a grace period can only decrease and track whatever our value is,
	// then ensure a calculated value is passed down to lower levels
	gracePeriod := status.gracePeriod
	// this value is bedrock truth - the apiserver owns telling us this value calculated by apiserver
	// pod设置了DeletionGracePeriodSeconds，且status.gracePeriod为0或DeletionGracePeriodSeconds小于status.gracePeriod，则gracePeriod为DeletionGracePeriodSeconds
	if override := pod.DeletionGracePeriodSeconds; override != nil {
		if gracePeriod == 0 || *override < gracePeriod {
			gracePeriod = *override
		}
	}
	// we allow other parts of the kubelet (namely eviction) to request this pod be terminated faster
	// 当设置了options.PodTerminationGracePeriodSecondsOverride，且gracePeriod为0或options.PodTerminationGracePeriodSecondsOverride小于gracePeriod
	if options != nil {
		if override := options.PodTerminationGracePeriodSecondsOverride; override != nil {
			if gracePeriod == 0 || *override < gracePeriod {
				gracePeriod = *override
			}
		}
	}

	// 上面简单总结为，当status.gracePeriod（只计算不为0情况，为0认为没有设置）、pod.DeletionGracePeriodSeconds、options.PodTerminationGracePeriodSecondsOverride都设置了取最小的
	// 那个没有设置就忽略

	// make a best effort to default this value to the pod's desired intent, in the event
	// the kubelet provided no requested value (graceful termination?)
	// 上面的都没有设置，则gracePeriod为*pod.Spec.TerminationGracePeriodSeconds
	if gracePeriod == 0 && pod.Spec.TerminationGracePeriodSeconds != nil {
		gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
	}
	// no matter what, we always supply a grace period of 1
	if gracePeriod < 1 {
		gracePeriod = 1
	}
	return gracePeriod, status.gracePeriod != 0 && status.gracePeriod != gracePeriod
}

// allowPodStart tries to start the pod and returns true if allowed, otherwise
// it requeues the pod and returns false. If the pod will never be able to start
// because data is missing, or the pod was terminated before start, canEverStart
// is false.
// pod不是staticPod，则返回true，true
// pod是static pod
//   pod不在p.podSyncStatuses里，则返回false，false
//   podWorker处于Terminating状态，则返回false，false
//   根据p.startedStaticPodsByFullname和waitingToStartStaticPodsByFullname，判断static pod不能启动，将uid加入p.workQueue，等待下次kl.syncLoopIteration里syncCh触发执行，同时设置status.working为false，返回false，true
//   其他情况返回true，true
func (p *podWorkers) allowPodStart(pod *v1.Pod) (canStart bool, canEverStart bool) {
	// pod不是staticPod，则返回true，true
	if !kubetypes.IsStaticPod(pod) {
		// TODO: Do we want to allow non-static pods with the same full name?
		// Note that it may disable the force deletion of pods.
		return true, true
	}
	p.podLock.Lock()
	defer p.podLock.Unlock()
	// pod不在p.podSyncStatuses里，则返回false，false
	status, ok := p.podSyncStatuses[pod.UID]
	if !ok {
		klog.ErrorS(nil, "Pod sync status does not exist, the worker should not be running", "pod", klog.KObj(pod), "podUID", pod.UID)
		return false, false
	}
	// podWorker处于Terminating状态，则返回false，false
	if status.IsTerminationRequested() {
		return false, false
	}
	// 下面几种情况返回true
	// 1. static pod的fullname已经在p.startedStaticPodsByFullname（已经启动的static pod）里，且保存的uid等于现在的uid
	// 2. fullname对应p.waitingToStartStaticPodsByFullname里列表为空
	// 3. 遍历waitingToStartStaticPodsByFullname列表中uid，第一个不是"等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态"等待启动的uid为现在的uid
	// 如果遍历过程中第一个不是"等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态"等待启动的uid为现在的uid
	//   如果剩余uid列表不为空，则将static的fullname从p.waitingToStartStaticPodsByFullname移除，否则更新p.waitingToStartStaticPodsByFullname[fullname]为剩余未遍历uid
	//   将static的fullname和对应的uid保存到p.startedStaticPodsByFullname
	// fullname对应p.waitingToStartStaticPodsByFullname里列表为空，将static的fullname和对应的uid保存到p.startedStaticPodsByFullname
	// 如果遍历过程中第一个不是"等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态"等待启动的uid不为现在的uid，则则更新p.waitingToStartStaticPodsByFullname[fullname]为当前的等待的uid到最后未遍历的uid，然后返回false
	//
	// 如果不能启动，则将uid加入p.workQueue，等待下次kl.syncLoopIteration里syncCh触发执行，同时设置status.working为false，返回false，true
	if !p.allowStaticPodStart(status.fullname, pod.UID) {
		p.workQueue.Enqueue(pod.UID, wait.Jitter(p.backOffPeriod, workerBackOffPeriodJitterFactor))
		status.working = false
		return false, true
	}
	return true, true
}

// allowStaticPodStart tries to start the static pod and returns true if
// 1. there are no other started static pods with the same fullname
// 2. the uid matches that of the first valid static pod waiting to start
// 下面几种情况返回true
// 1. static pod的fullname已经在p.startedStaticPodsByFullname（已经启动的static pod）里，且保存的uid等于现在的uid
// 2. fullname对应p.waitingToStartStaticPodsByFullname里列表为空
// 3. 遍历waitingToStartStaticPodsByFullname列表中uid，第一个不是"等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态"等待启动的uid为现在的uid
// 如果遍历过程中第一个不是"等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态"等待启动的uid为现在的uid
//   如果剩余uid列表不为空，则将static的fullname从p.waitingToStartStaticPodsByFullname移除，否则更新p.waitingToStartStaticPodsByFullname[fullname]为剩余未遍历uid
//   将static的fullname和对应的uid保存到p.startedStaticPodsByFullname
// fullname对应p.waitingToStartStaticPodsByFullname里列表为空，将static的fullname和对应的uid保存到p.startedStaticPodsByFullname
// 如果遍历过程中第一个不是"等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态"等待启动的uid不为现在的uid，则则更新p.waitingToStartStaticPodsByFullname[fullname]为当前的等待的uid到最后未遍历的uid，然后返回false
func (p *podWorkers) allowStaticPodStart(fullname string, uid types.UID) bool {
	startedUID, started := p.startedStaticPodsByFullname[fullname]
	// static pod的fullname已经在p.startedStaticPodsByFullname（已经启动的static pod）里，则返回保存的uid是否等于现在的uid
	if started {
		return startedUID == uid
	}

	waitingPods := p.waitingToStartStaticPodsByFullname[fullname]
	// TODO: This is O(N) with respect to the number of updates to static pods
	// with overlapping full names, and ideally would be O(1).
	// 遍历waitingToStartStaticPodsByFullname列表中uid
	// 如果是"等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态"，则跳过
	// 如果遇到启动的uid不为现在的uid，则更新p.waitingToStartStaticPodsByFullname[fullname]为移除了前面的uid，然后返回false
	// 遇到这个等待启动的uid为现在的uid，终止循环
	for i, waitingUID := range waitingPods {
		// has pod already terminated or been deleted?
		status, ok := p.podSyncStatuses[waitingUID]
		// 等待启动的uid不在p.podSyncStatuses，或处于Terminating状态，或处于Terminated状态，则跳过
		if !ok || status.IsTerminationRequested() || status.IsTerminated() {
			continue
		}
		// another pod is next in line
		// 等待启动的uid不为现在的uid，则更新p.waitingToStartStaticPodsByFullname[fullname]为移除了前面的uid，然后返回false
		if waitingUID != uid {
			p.waitingToStartStaticPodsByFullname[fullname] = waitingPods[i:]
			return false
		}
		// we are up next, remove ourselves
		// 这个等待启动的uid为现在的uid，则waitingPods为剩余uid列表
		waitingPods = waitingPods[i+1:]
		break
	}
	// 剩余的uid列表不为空，则更新p.waitingToStartStaticPodsByFullname为剩余的uid列表
	if len(waitingPods) != 0 {
		p.waitingToStartStaticPodsByFullname[fullname] = waitingPods
	} else {
		// 剩余uid列表不为空，则将static的fullname从p.waitingToStartStaticPodsByFullname移除
		delete(p.waitingToStartStaticPodsByFullname, fullname)
	}
	// 将static的fullname和对应的uid保存到p.startedStaticPodsByFullname
	p.startedStaticPodsByFullname[fullname] = uid
	return true
}

func (p *podWorkers) managePodLoop(podUpdates <-chan podWork) {
	var lastSyncTime time.Time
	var podStarted bool
	for update := range podUpdates {
		pod := update.Options.Pod

		// Decide whether to start the pod. If the pod was terminated prior to the pod being allowed
		// to start, we have to clean it up and then exit the pod worker loop.
		// 一旦pod能启动，则podStarted为true，就不走这个逻辑
		if !podStarted {
			// pod不是staticPod，则返回true，true
			// pod是static pod
			//   pod不在p.podSyncStatuses里，则返回false，false
			//   podWorker处于Terminating状态，则返回false，false
			//   根据p.startedStaticPodsByFullname和waitingToStartStaticPodsByFullname，判断static pod不能启动，将uid加入p.workQueue，等待下次kl.syncLoopIteration里syncCh触发执行，同时设置status.working为false，返回false，true
			//   其他情况返回true，true
			canStart, canEverStart := p.allowPodStart(pod)
			// pod不是永远不能启动，执行最后的状态设置动作，然后返回
			if !canEverStart {
				// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
				// 并从p.lastUndeliveredWorkUpdate移除uid
				// 如果有p.podSyncStatuses里uid对应的status，则设置status.finished为true，status.working为false，status.terminatedAt为time.Now()
				// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
				p.completeUnstartedTerminated(pod)
				if start := update.Options.StartTime; !start.IsZero() {
					metrics.PodWorkerDuration.WithLabelValues("terminated").Observe(metrics.SinceInSeconds(start))
				}
				klog.V(4).InfoS("Processing pod event done", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", update.WorkType)
				return
			}
			// pod暂时不能启动，则等待chan下一个podWork
			if !canStart {
				klog.V(4).InfoS("Pod cannot start yet", "pod", klog.KObj(pod), "podUID", pod.UID)
				continue
			}
			podStarted = true
		}

		klog.V(4).InfoS("Processing pod event", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", update.WorkType)
		var isTerminal bool
		err := func() error {
			// The worker is responsible for ensuring the sync method sees the appropriate
			// status updates on resyncs (the result of the last sync), transitions to
			// terminating (no wait), or on terminated (whatever the most recent state is).
			// Only syncing and terminating can generate pod status changes, while terminated
			// pods ensure the most recent status makes it to the api server.
			var status *kubecontainer.PodStatus
			var err error
			switch {
			case update.Options.RunningPod != nil:
				// when we receive a running pod, we don't need status at all
			default:
				// wait until we see the next refresh from the PLEG via the cache (max 2s)
				// TODO: this adds ~1s of latency on all transitions from sync to terminating
				//  to terminated, and on all termination retries (including evictions). We should
				//  improve latency by making the the pleg continuous and by allowing pod status
				//  changes to be refreshed when key events happen (killPod, sync->terminating).
				//  Improving this latency also reduces the possibility that a terminated
				//  container's status is garbage collected before we have a chance to update the
				//  API server (thus losing the exit code).
				status, err = p.podCache.GetNewerThan(pod.UID, lastSyncTime)
			}
			if err != nil {
				// This is the legacy event thrown by manage pod loop all other events are now dispatched
				// from syncPodFn
				p.recorder.Eventf(pod, v1.EventTypeWarning, events.FailedSync, "error determining status: %v", err)
				return err
			}

			// uid不在p.podSyncStatuses中则返回nil
			// 如果没有context或context已经取消，则创建一个新的context，返回这个context
			// 否则，返回status.ctx
			ctx := p.contextForWorker(pod.UID)

			// Take the appropriate action (illegal phases are prevented by UpdatePod)
			switch {
			// work类型为TerminatedPodWork
			case update.WorkType == TerminatedPodWork:
				// p.syncTerminatedPodFn实现在pkg\kubelet\kubelet.go里(kl *Kubelet) syncTerminatedPod
				err = p.syncTerminatedPodFn(ctx, pod, status)

			// work类型为TerminatingPodWork
			case update.WorkType == TerminatingPodWork:
				var gracePeriod *int64
				// (p *podWorkers)UpdatePod中保证update.Options.KillPodOptions不为nil
				if opt := update.Options.KillPodOptions; opt != nil {
					gracePeriod = opt.PodTerminationGracePeriodSecondsOverride
				}
				// uid不在p.podSyncStatuses，则返回nil
				// 处于Terminating状态，但是status.startedTerminating为false，则status.startedTerminating设置为true（代表正在执行terminating）
				// 如果有status.statusPostTerminating，则返回status.statusPostTerminating列表里最后一个，否则返回nil
				podStatusFn := p.acknowledgeTerminating(pod)

				// p.syncTerminatingPodFn实现在pkg\kubelet\kubelet.go里(kl *Kubelet) syncTerminatingPod
				err = p.syncTerminatingPodFn(ctx, pod, status, update.Options.RunningPod, gracePeriod, podStatusFn)

			default:
				// p.syncPodFn实现在pkg\kubelet\kubelet.go里(kl *Kubelet) syncPod
				isTerminal, err = p.syncPodFn(ctx, update.Options.UpdateType, pod, update.Options.MirrorPod, status)
			}

			lastSyncTime = time.Now()
			return err
		}()

		var phaseTransition bool
		switch {
		case err == context.Canceled:
			// when the context is cancelled we expect an update to already be queued
			klog.V(2).InfoS("Sync exited with context cancellation error", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", update.WorkType)

		case err != nil:
			// we will queue a retry
			klog.ErrorS(err, "Error syncing pod, skipping", "pod", klog.KObj(pod), "podUID", pod.UID)

		case update.WorkType == TerminatedPodWork:
			// we can shut down the worker
			// 已经执行完syncTerminatedPodFn，则执行最后状态设置
			// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
			// 并从p.lastUndeliveredWorkUpdate移除uid
			// 如果pod.UID存在p.podSyncStatuses中，则设置status.finished为true，status.working为false
			// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
			p.completeTerminated(pod)
			if start := update.Options.StartTime; !start.IsZero() {
				metrics.PodWorkerDuration.WithLabelValues("terminated").Observe(metrics.SinceInSeconds(start))
			}
			klog.V(4).InfoS("Processing pod event done", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", update.WorkType)
			return

		case update.WorkType == TerminatingPodWork:
			// pods that don't exist in config don't need to be terminated, garbage collection will cover them
			// 存在update.Options.RunningPod（孤儿pod），则podWorker不需要切换到terminated状态
			if update.Options.RunningPod != nil {
				// 如果pod.UID存在p.podSyncStatuses中，则设置status.terminatedAt为time.Now()，status.finished为true，status.working为false
				// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
				// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
				// 并从p.lastUndeliveredWorkUpdate移除uid
				p.completeTerminatingRuntimePod(pod)
				if start := update.Options.StartTime; !start.IsZero() {
					metrics.PodWorkerDuration.WithLabelValues(update.Options.UpdateType.String()).Observe(metrics.SinceInSeconds(start))
				}
				klog.V(4).InfoS("Processing pod event done", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", update.WorkType)
				return
			}
			// otherwise we move to the terminating phase
			// 如果pod.UID存在p.podSyncStatuses中，设置status.terminatedAt为time.Now()，关闭status.notifyPostTerminating里的所有chan，设置status.notifyPostTerminating为nil，设置status.statusPostTerminating为nil
			// 向p.lastUndeliveredWorkUpdate里添加pod.UID和对应的podWork（WorkType为TerminatedPodWork），代表下一次循环执行Terminated
			p.completeTerminating(pod)
			phaseTransition = true

		// syncPod返回pod为terminal
		case isTerminal:
			// if syncPod indicated we are now terminal, set the appropriate pod status to move to terminating
			klog.V(4).InfoS("Pod is terminal", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", update.WorkType)
			// 如果pod.UID存在p.podSyncStatuses中，如果status.terminatingAt为0值，则设置status.terminatingAt为time.Now()，同时设置status.startedTerminating为true（代表terminating正在执行，告诉其他组件pod不会生成新的container）
			// 向p.lastUndeliveredWorkUpdate里添加pod.UID和对应的podWork（WorkType为TerminatingPodWork），代表下一次循环执行Terminating
			p.completeSync(pod)
			phaseTransition = true
		}

		// queue a retry if necessary, then put the next event in the channel if any
		// 加入p.workQueue队列（让下次(kl *Kubelet) syncLoopIteration里syncCh触发执行（每1秒触发一次），从p.workQueue读取出过期的uid重新执行UpdatePod）
		// 1. 阶段转换（terminating到terminated，sync到terminating），立即加入p.workQueue
		// 2. 没有错误，加入队列但是等待[60s, 90s]
		// 3. 错误包含"network is not ready"，加入队列但是等待[1s, 1.5s]
		// 4. 发生其他错误，加入队列并等待[10s, 15s]
		// 消费并清空p.lastUndeliveredWorkUpdate[uid]，或直接设置设置uid在p.podSyncStatuses里的status.working为false
		// 存在还未投递的podWork（uid在p.lastUndeliveredWorkUpdate中），则将未投递的podWork发生到uid对应的chan，然后从p.lastUndeliveredWorkUpdate中移除这个uid
		// 否则设置uid在p.podSyncStatuses里的status.working为false（下次执行UpdatePod，直接将podWork发送到chan中，而不是添加到p.lastUndeliveredWorkUpdate）
		p.completeWork(pod, phaseTransition, err)
		if start := update.Options.StartTime; !start.IsZero() {
			metrics.PodWorkerDuration.WithLabelValues(update.Options.UpdateType.String()).Observe(metrics.SinceInSeconds(start))
		}
		klog.V(4).InfoS("Processing pod event done", "pod", klog.KObj(pod), "podUID", pod.UID, "updateType", update.WorkType)
	}
}

// acknowledgeTerminating sets the terminating flag on the pod status once the pod worker sees
// the termination state so that other components know no new containers will be started in this
// pod. It then returns the status function, if any, that applies to this pod.
// uid不在p.podSyncStatuses，则返回nil
// 处于Terminating状态，但是status.startedTerminating为false，则status.startedTerminating设置为true（代表正在执行terminating，告诉其他组件pod不会生成新的container）
// 如果有status.statusPostTerminating，则返回status.statusPostTerminating列表里最后一个，否则返回nil
func (p *podWorkers) acknowledgeTerminating(pod *v1.Pod) PodStatusFunc {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	status, ok := p.podSyncStatuses[pod.UID]
	if !ok {
		return nil
	}

	// 处于Terminating状态，但是status.startedTerminating为false，则status.startedTerminating设置为true（代表正在执行terminating）
	if !status.terminatingAt.IsZero() && !status.startedTerminating {
		klog.V(4).InfoS("Pod worker has observed request to terminate", "pod", klog.KObj(pod), "podUID", pod.UID)
		status.startedTerminating = true
	}

	// 返回status.statusPostTerminating列表里最后一个
	if l := len(status.statusPostTerminating); l > 0 {
		return status.statusPostTerminating[l-1]
	}
	return nil
}

// completeSync is invoked when syncPod completes successfully and indicates the pod is now terminal and should
// be terminated. This happens when the natural pod lifecycle completes - any pod which is not RestartAlways
// exits. Unnatural completions, such as evictions, API driven deletion or phase transition, are handled by
// UpdatePod.
// 如果pod.UID存在p.podSyncStatuses中，如果status.terminatingAt为0值，则设置status.terminatingAt为time.Now()，同时设置status.startedTerminating为true（代表terminating正在执行，告诉其他组件pod不会生成新的container）
// 向p.lastUndeliveredWorkUpdate里添加pod.UID和对应的podWork（WorkType为TerminatingPodWork），代表下一次循环执行Terminating
func (p *podWorkers) completeSync(pod *v1.Pod) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod indicated lifecycle completed naturally and should now terminate", "pod", klog.KObj(pod), "podUID", pod.UID)

	if status, ok := p.podSyncStatuses[pod.UID]; ok {
		if status.terminatingAt.IsZero() {
			status.terminatingAt = time.Now()
		} else {
			klog.V(4).InfoS("Pod worker attempted to set terminatingAt twice, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		status.startedTerminating = true
	}

	p.lastUndeliveredWorkUpdate[pod.UID] = podWork{
		WorkType: TerminatingPodWork,
		Options: UpdatePodOptions{
			Pod: pod,
		},
	}
}

// completeTerminating is invoked when syncTerminatingPod completes successfully, which means
// no container is running, no container will be started in the future, and we are ready for
// cleanup.  This updates the termination state which prevents future syncs and will ensure
// other kubelet loops know this pod is not running any containers.
// 如果pod.UID存在p.podSyncStatuses中，设置status.terminatedAt为time.Now()，关闭status.notifyPostTerminating里的所有chan，设置status.notifyPostTerminating为nil，设置status.statusPostTerminating为nil
// 向p.lastUndeliveredWorkUpdate里添加pod.UID和对应的podWork（WorkType为TerminatedPodWork），代表下一次循环执行Terminated
func (p *podWorkers) completeTerminating(pod *v1.Pod) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod terminated all containers successfully", "pod", klog.KObj(pod), "podUID", pod.UID)

	if status, ok := p.podSyncStatuses[pod.UID]; ok {
		if status.terminatingAt.IsZero() {
			klog.V(4).InfoS("Pod worker was terminated but did not have terminatingAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		status.terminatedAt = time.Now()
		for _, ch := range status.notifyPostTerminating {
			close(ch)
		}
		status.notifyPostTerminating = nil
		status.statusPostTerminating = nil
	}

	p.lastUndeliveredWorkUpdate[pod.UID] = podWork{
		WorkType: TerminatedPodWork,
		Options: UpdatePodOptions{
			Pod: pod,
		},
	}
}

// completeTerminatingRuntimePod is invoked when syncTerminatingPod completes successfully,
// which means an orphaned pod (no config) is terminated and we can exit. Since orphaned
// pods have no API representation, we want to exit the loop at this point
// cleanup.  This updates the termination state which prevents future syncs and will ensure
// other kubelet loops know this pod is not running any containers.
// 如果pod.UID存在p.podSyncStatuses中，则设置status.terminatedAt为time.Now()，status.finished为true，status.working为false
// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
// 并从p.lastUndeliveredWorkUpdate移除uid
func (p *podWorkers) completeTerminatingRuntimePod(pod *v1.Pod) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod terminated all orphaned containers successfully and worker can now stop", "pod", klog.KObj(pod), "podUID", pod.UID)

	// 如果pod.UID存在p.podSyncStatuses中，则设置status.terminatedAt为time.Now()，status.finished为true，status.working为false
	if status, ok := p.podSyncStatuses[pod.UID]; ok {
		if status.terminatingAt.IsZero() {
			klog.V(4).InfoS("Pod worker was terminated but did not have terminatingAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		status.terminatedAt = time.Now()
		status.finished = true
		status.working = false

		if p.startedStaticPodsByFullname[status.fullname] == pod.UID {
			delete(p.startedStaticPodsByFullname, status.fullname)
		}
	}

	p.cleanupPodUpdates(pod.UID)
}

// completeTerminated is invoked after syncTerminatedPod completes successfully and means we
// can stop the pod worker. The pod is finalized at this point.
// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
// 并从p.lastUndeliveredWorkUpdate移除uid
// 如果pod.UID存在p.podSyncStatuses中，则设置status.finished为true，status.working为false
// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
func (p *podWorkers) completeTerminated(pod *v1.Pod) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod is complete and the worker can now stop", "pod", klog.KObj(pod), "podUID", pod.UID)

	// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
	// 并从p.lastUndeliveredWorkUpdate移除uid
	p.cleanupPodUpdates(pod.UID)

	// 如果pod.UID存在p.podSyncStatuses中，则设置status.finished为true，status.working为false
	// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
	if status, ok := p.podSyncStatuses[pod.UID]; ok {
		if status.terminatingAt.IsZero() {
			klog.V(4).InfoS("Pod worker is complete but did not have terminatingAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		if status.terminatedAt.IsZero() {
			klog.V(4).InfoS("Pod worker is complete but did not have terminatedAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		status.finished = true
		status.working = false

		if p.startedStaticPodsByFullname[status.fullname] == pod.UID {
			delete(p.startedStaticPodsByFullname, status.fullname)
		}
	}
}

// completeUnstartedTerminated is invoked if a pod that has never been started receives a termination
// signal before it can be started.
// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
// 并从p.lastUndeliveredWorkUpdate移除uid
// 如果有p.podSyncStatuses里uid对应的status，则设置status.finished为true，status.working为false，status.terminatedAt为time.Now()
// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
func (p *podWorkers) completeUnstartedTerminated(pod *v1.Pod) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod never started and the worker can now stop", "pod", klog.KObj(pod), "podUID", pod.UID)

	// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
	// 并从p.lastUndeliveredWorkUpdate移除uid
	p.cleanupPodUpdates(pod.UID)

	if status, ok := p.podSyncStatuses[pod.UID]; ok {
		// 没有设置terminatingAt
		if status.terminatingAt.IsZero() {
			klog.V(4).InfoS("Pod worker is complete but did not have terminatingAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		// 设置了terminatedAt
		if !status.terminatedAt.IsZero() {
			klog.V(4).InfoS("Pod worker is complete and had terminatedAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
		}
		status.finished = true
		status.working = false
		status.terminatedAt = time.Now()

		if p.startedStaticPodsByFullname[status.fullname] == pod.UID {
			delete(p.startedStaticPodsByFullname, status.fullname)
		}
	}
}

// completeWork requeues on error or the next sync interval and then immediately executes any pending
// work.
// 加入p.workQueue队列（让下次(kl *Kubelet) syncLoopIteration里syncCh触发执行，从p.workQueue读取出过期的uid重新执行UpdatePod）
// 1. 阶段转换，立即加入p.workQueue
// 2. 没有错误，加入队列但是等待[60s, 90s]
// 3. 错误包含"network is not ready"，加入队列但是等待[1s, 1.5s]
// 4. 发生其他错误，加入队列并等待[10s, 15s]
// 消费并清空p.lastUndeliveredWorkUpdate[uid]，或直接设置设置uid在p.podSyncStatuses里的status.working为false
// 存在还未投递的podWork（uid在p.lastUndeliveredWorkUpdate中），则将未投递的podWork发生到uid对应的chan，然后从p.lastUndeliveredWorkUpdate中移除这个uid
// 否则设置uid在p.podSyncStatuses里的status.working为false（下次执行UpdatePod，直接将podWork发送到chan中，而不是添加到p.lastUndeliveredWorkUpdate）
func (p *podWorkers) completeWork(pod *v1.Pod, phaseTransition bool, syncErr error) {
	// Requeue the last update if the last sync returned error.
	switch {
	// 阶段转换，立即加入p.workQueue
	case phaseTransition:
		p.workQueue.Enqueue(pod.UID, 0)
	// 没有错误，加入队列但是等待[60s, 90s]
	case syncErr == nil:
		// No error; requeue at the regular resync interval.
		// p.resyncInterval默认为1分钟
		p.workQueue.Enqueue(pod.UID, wait.Jitter(p.resyncInterval, workerResyncIntervalJitterFactor))
	// 错误包含"network is not ready"，加入队列但是等待[1s, 1.5s]
	case strings.Contains(syncErr.Error(), NetworkNotReadyErrorMsg):
		// Network is not ready; back off for short period of time and retry as network might be ready soon.
		p.workQueue.Enqueue(pod.UID, wait.Jitter(backOffOnTransientErrorPeriod, workerBackOffPeriodJitterFactor))
	// 发生其他错误，加入队列并等待[10s, 15s]
	default:
		// Error occurred during the sync; back off and then retry.
		// p.backOffPeriod为10s
		p.workQueue.Enqueue(pod.UID, wait.Jitter(p.backOffPeriod, workerBackOffPeriodJitterFactor))
	}
	// 存在还未投递的podWork（uid在p.lastUndeliveredWorkUpdate中），则将未投递的podWork发生到uid对应的chan，然后从p.lastUndeliveredWorkUpdate中移除这个uid
	// 否则设置uid在p.podSyncStatuses里的status.working为false（下次执行UpdatePod，直接将podWork发送到chan中，而不是添加到p.lastUndeliveredWorkUpdate）
	p.completeWorkQueueNext(pod.UID)
}

// completeWorkQueueNext holds the lock and either queues the next work item for the worker or
// clears the working status.
// 存在还未投递的podWork（uid在p.lastUndeliveredWorkUpdate中），则将未投递的podWork发生到uid对应的chan，然后从p.lastUndeliveredWorkUpdate中移除这个uid
// 否则设置uid在p.podSyncStatuses里的status.working为false（下次执行UpdatePod，直接将podWork发送到chan中，而不是添加到p.lastUndeliveredWorkUpdate）
func (p *podWorkers) completeWorkQueueNext(uid types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	// 存在还未投递的podWork，则将未投递的podWork发生到uid对应的chan，然后从p.lastUndeliveredWorkUpdate中移除这个uid
	// 否则设置uid在p.podSyncStatuses里的status.working为false（下次执行UpdatePod，直接将podWork发送到chan中，而不是添加到p.lastUndeliveredWorkUpdate）
	if workUpdate, exists := p.lastUndeliveredWorkUpdate[uid]; exists {
		p.podUpdates[uid] <- workUpdate
		delete(p.lastUndeliveredWorkUpdate, uid)
	} else {
		p.podSyncStatuses[uid].working = false
	}
}

// contextForWorker returns or initializes the appropriate context for a known
// worker. If the current context is expired, it is reset. If no worker is
// present, no context is returned.
// uid不在p.podSyncStatuses中则返回nil
// 如果没有context或context已经取消，则创建一个新的context，返回这个context
// 否则，返回status.ctx
func (p *podWorkers) contextForWorker(uid types.UID) context.Context {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	status, ok := p.podSyncStatuses[uid]
	if !ok {
		return nil
	}
	// 没有context或context已经取消，则创建一个新的context
	if status.ctx == nil || status.ctx.Err() == context.Canceled {
		status.ctx, status.cancelFn = context.WithCancel(context.Background())
	}
	return status.ctx
}

// SyncKnownPods will purge any fully terminated pods that are not in the desiredPods
// list, which means SyncKnownPods must be called in a threadsafe manner from calls
// to UpdatePods for new pods. It returns a map of known workers that are not finished
// with a value of SyncPodTerminated, SyncPodKill, or SyncPodSync depending on whether
// the pod is terminated, terminating, or syncing.
// 根据pod uid在p.podSyncStatuses里的status，返回所有uid和对应的PodWorkerState
// 设置p.podsSynced为true
// p.podSyncStatuses中的uid不在desiredPods中（pod被删除了），或需要重启（status状态为terminating，UpdatePods里收到UpdateType为kubetypes.SyncPodCreate请求）且pod worker已完成
// 则执行下面
// 从p.podSyncStatuses中删除uid
// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
// 并从p.lastUndeliveredWorkUpdate移除uid
// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
func (p *podWorkers) SyncKnownPods(desiredPods []*v1.Pod) map[types.UID]PodWorkerState {
	workers := make(map[types.UID]PodWorkerState)
	known := make(map[types.UID]struct{})
	for _, pod := range desiredPods {
		known[pod.UID] = struct{}{}
	}

	p.podLock.Lock()
	defer p.podLock.Unlock()

	// 至少一个pod worker执行了syncPod，即已经有pod通过UpdatePod()启动
	p.podsSynced = true
	for uid, status := range p.podSyncStatuses {
		// p.podSyncStatuses中的uid不在known中（pod被删除了），或需要重启（status状态为terminating，UpdatePods里收到UpdateType为kubetypes.SyncPodCreate请求）
		if _, exists := known[uid]; !exists || status.restartRequested {
			// uid不在p.podSyncStatuses里直接返回
			// pod worker还未完成（完成了意味着podWorker需要被移除），直接返回
			// 从p.podSyncStatuses中删除uid
			// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
			// 并从p.lastUndeliveredWorkUpdate移除uid
			// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
			p.removeTerminatedWorker(uid)
		}
		switch {
		// 处于Terminated状态
		case !status.terminatedAt.IsZero():
			if status.restartRequested {
				workers[uid] = TerminatedAndRecreatedPod
			} else {
				workers[uid] = TerminatedPod
			}
		// 处于terminating
		case !status.terminatingAt.IsZero():
			workers[uid] = TerminatingPod
		// sync状态
		default:
			workers[uid] = SyncPod
		}
	}
	return workers
}

// removeTerminatedWorker cleans up and removes the worker status for a worker
// that has reached a terminal state of "finished" - has successfully exited
// syncTerminatedPod. This "forgets" a pod by UID and allows another pod to be
// recreated with the same UID.
// uid不在p.podSyncStatuses里直接返回
// pod worker还未完成（完成了意味着podWorker需要被移除），直接返回
// 从p.podSyncStatuses中删除uid
// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
// 并从p.lastUndeliveredWorkUpdate移除uid
// 如果p.startedStaticPodsByFullname[status.fullname]跟pod的uid一样，则从p.startedStaticPodsByFullname移除status.fullname
func (p *podWorkers) removeTerminatedWorker(uid types.UID) {
	status, ok := p.podSyncStatuses[uid]
	// uid不在p.podSyncStatuses里直接返回
	if !ok {
		// already forgotten, or forgotten too early
		klog.V(4).InfoS("Pod worker has been requested for removal but is not a known pod", "podUID", uid)
		return
	}

	// pod worker还未完成（完成了意味着podWorker需要被移除）
	if !status.finished {
		klog.V(4).InfoS("Pod worker has been requested for removal but is still not fully terminated", "podUID", uid)
		return
	}

	if status.restartRequested {
		klog.V(4).InfoS("Pod has been terminated but another pod with the same UID was created, remove history to allow restart", "podUID", uid)
	} else {
		klog.V(4).InfoS("Pod has been terminated and is no longer known to the kubelet, remove all history", "podUID", uid)
	}
	// 从p.podSyncStatuses中删除uid
	delete(p.podSyncStatuses, uid)
	// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
	// 并从p.lastUndeliveredWorkUpdate移除uid
	p.cleanupPodUpdates(uid)

	if p.startedStaticPodsByFullname[status.fullname] == uid {
		delete(p.startedStaticPodsByFullname, status.fullname)
	}
}

// killPodNow returns a KillPodFunc that can be used to kill a pod.
// It is intended to be injected into other modules that need to kill a pod.
func killPodNow(podWorkers PodWorkers, recorder record.EventRecorder) eviction.KillPodFunc {
	return func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, statusFn func(*v1.PodStatus)) error {
		// determine the grace period to use when killing the pod
		// gracePeriod设置顺序是
		// gracePeriodOverride、pod.Spec.TerminationGracePeriodSeconds、int64(0)
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
		ch := make(chan struct{}, 1)
		// 重新执行kl.podWorkers.UpdatePod，触发重新执行podWork（event类型为SyncPodKill）（执行terminating）
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
			KillPodOptions: &KillPodOptions{
				CompletedCh:                              ch,
				Evict:                                    isEvicted,
				PodStatusFunc:                            statusFn,
				PodTerminationGracePeriodSecondsOverride: gracePeriodOverride,
			},
		})

		// wait for either a response, or a timeout
		select {
		case <-ch:
			return nil
		case <-time.After(timeoutDuration):
			recorder.Eventf(pod, v1.EventTypeWarning, events.ExceededGracePeriod, "Container runtime did not kill the pod within specified grace period.")
			return fmt.Errorf("timeout waiting to kill pod")
		}
	}
}

// cleanupPodUpdates closes the podUpdates channel and removes it from
// podUpdates map so that the corresponding pod worker can stop. It also
// removes any undelivered work. This method must be called holding the
// pod lock.
// 关闭p.podUpdates中uid对应的chan，并从p.podUpdates中移除uid
// 并从p.lastUndeliveredWorkUpdate移除uid
func (p *podWorkers) cleanupPodUpdates(uid types.UID) {
	if ch, ok := p.podUpdates[uid]; ok {
		close(ch)
	}
	delete(p.podUpdates, uid)
	delete(p.lastUndeliveredWorkUpdate, uid)
}
