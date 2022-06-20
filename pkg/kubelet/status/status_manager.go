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

package status

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	clientset "k8s.io/client-go/kubernetes"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	statusutil "k8s.io/kubernetes/pkg/util/pod"
)

// A wrapper around v1.PodStatus that includes a version to enforce that stale pod statuses are
// not sent to the API server.
type versionedPodStatus struct {
	status v1.PodStatus
	// Monotonically increasing version number (per pod).
	version uint64
	// Pod name & namespace, for sending updates to API server.
	podName      string
	podNamespace string
}

type podStatusSyncRequest struct {
	podUID types.UID
	status versionedPodStatus
}

// Updates pod statuses in apiserver. Writes only when new status has changed.
// All methods are thread-safe.
type manager struct {
	kubeClient clientset.Interface
	podManager kubepod.Manager
	// Map from pod UID to sync status of the corresponding pod.
	// 如果是mirror pod，则代表static pod status
	// key只能是static pod和普通pod的uid
	podStatuses      map[types.UID]versionedPodStatus
	podStatusesLock  sync.RWMutex
	podStatusChannel chan podStatusSyncRequest
	// Map from (mirror) pod UID to latest status version successfully sent to the API server.
	// apiStatusVersions must only be accessed from the sync thread.
	// 将podStatuses字段更新到apiserver后，再更新apiStatusVersions
	// 所以podStatuses字段里的version有可能跟apiStatusVersions不一样
	// static pod的key是mirror pod的uid，普通pod的key是自己uid转成MirrorPodUID类型
	//  (mirror) pod UID和最后发送到apiserver的版本（versionedPodStatus字段里的version）
	// apiserver里的pod，只有普通pod和mirror pod
	apiStatusVersions map[kubetypes.MirrorPodUID]uint64
	podDeletionSafety PodDeletionSafetyProvider
}

// PodStatusProvider knows how to provide status for a pod. It's intended to be used by other components
// that need to introspect status.
type PodStatusProvider interface {
	// GetPodStatus returns the cached status for the provided pod UID, as well as whether it
	// was a cache hit.
	GetPodStatus(uid types.UID) (v1.PodStatus, bool)
}

// PodDeletionSafetyProvider provides guarantees that a pod can be safely deleted.
type PodDeletionSafetyProvider interface {
	// A function which returns true if the pod can safely be deleted
	PodResourcesAreReclaimed(pod *v1.Pod, status v1.PodStatus) bool
}

// Manager is the Source of truth for kubelet pod status, and should be kept up-to-date with
// the latest v1.PodStatus. It also syncs updates back to the API server.
type Manager interface {
	PodStatusProvider

	// Start the API server status sync loop.
	Start()

	// SetPodStatus caches updates the cached status for the given pod, and triggers a status update.
	SetPodStatus(pod *v1.Pod, status v1.PodStatus)

	// SetContainerReadiness updates the cached container status with the given readiness, and
	// triggers a status update.
	SetContainerReadiness(podUID types.UID, containerID kubecontainer.ContainerID, ready bool)

	// SetContainerStartup updates the cached container status with the given startup, and
	// triggers a status update.
	SetContainerStartup(podUID types.UID, containerID kubecontainer.ContainerID, started bool)

	// TerminatePod resets the container status for the provided pod to terminated and triggers
	// a status update.
	TerminatePod(pod *v1.Pod)

	// RemoveOrphanedStatuses scans the status cache and removes any entries for pods not included in
	// the provided podUIDs.
	RemoveOrphanedStatuses(podUIDs map[types.UID]bool)
}

const syncPeriod = 10 * time.Second

// NewManager returns a functional Manager.
func NewManager(kubeClient clientset.Interface, podManager kubepod.Manager, podDeletionSafety PodDeletionSafetyProvider) Manager {
	return &manager{
		kubeClient:        kubeClient,
		podManager:        podManager,
		podStatuses:       make(map[types.UID]versionedPodStatus),
		podStatusChannel:  make(chan podStatusSyncRequest, 1000), // Buffer up to 1000 statuses
		apiStatusVersions: make(map[kubetypes.MirrorPodUID]uint64),
		podDeletionSafety: podDeletionSafety,
	}
}

// isPodStatusByKubeletEqual returns true if the given pod statuses are equal when non-kubelet-owned
// pod conditions are excluded.
// This method normalizes the status before comparing so as to make sure that meaningless
// changes will be ignored.
// 比较status是否一样，condition只比较kubelet condition（PodScheduled、Ready、Initialized、Unschedulable、ContainersReady），非kubelet condition不比较
func isPodStatusByKubeletEqual(oldStatus, status *v1.PodStatus) bool {
	oldCopy := oldStatus.DeepCopy()
	for _, c := range status.Conditions {
		// 非kubelet condition不比较（比如readiness gate的condition），只比较kubelet condition--PodScheduled、Ready、Initialized、Unschedulable、ContainersReady
		if kubetypes.PodConditionByKubelet(c.Type) {
			_, oc := podutil.GetPodCondition(oldCopy, c.Type)
			if oc == nil || oc.Status != c.Status || oc.Message != c.Message || oc.Reason != c.Reason {
				return false
			}
		}
	}
	oldCopy.Conditions = status.Conditions
	return apiequality.Semantic.DeepEqual(oldCopy, status)
}

// 启动一个go routine
// 1. 消费m.podStatusChannel执行m.syncPod方法（同步给定的status到api server和如果pod可以进行删除则执行最终的pod删除）
// 2. 每10s执行清空m.podStatusChannel，执行m.syncBatch（清理m.apiStatusVersion中不需要的uid，找出需要执行m.syncPod的uid，然后执行m.syncPod）
func (m *manager) Start() {
	// Don't start the status manager if we don't have a client. This will happen
	// on the master, where the kubelet is responsible for bootstrapping the pods
	// of the master components.
	if m.kubeClient == nil {
		klog.Infof("Kubernetes client is nil, not starting status manager.")
		return
	}

	klog.Info("Starting to sync pod status with apiserver")
	//lint:ignore SA1015 Ticker can link since this is only called once and doesn't handle termination.
	syncTicker := time.Tick(syncPeriod)
	// syncPod and syncBatch share the same go routine to avoid sync races.
	go wait.Forever(func() {
		for {
			select {
			case syncRequest := <-m.podStatusChannel:
				klog.V(5).Infof("Status Manager: syncing pod: %q, with status: (%d, %v) from podStatusChannel",
					syncRequest.podUID, syncRequest.status.version, syncRequest.status.status)
				// 同步给定的status到api server，以下条件都要满足
				// 1. pod的status真的要更新
				// 2. pod uid没有发生了变化
				// 这里还处理了pod已经可以被删除情况，在api server中删除pod（设置GracePeriodSeconds为0进行删除）
				m.syncPod(syncRequest.podUID, syncRequest.status)
			case <-syncTicker:
				klog.V(5).Infof("Status Manager: syncing batch")
				// remove any entries in the status channel since the batch will handle them
				// 清空m.podStatusChannel
				for i := len(m.podStatusChannel); i > 0; i-- {
					<-m.podStatusChannel
				}
				// 1. 清理m.apiStatusVersion中不需要的uid（uid从m.podStatuses删除，但是在m.apiStatusVersions没有删除且uid的pod不是mirror pod）
				// 2. 遍历m.podStatuses查找出需要执行m.syncPod方法的pod uid（m.podStatuses和m.apiStatusVersions里是不一致的，或m.podStatuses和m.apiStatusVersions里是一致的，但是跟pod manager里（等于apiserver里）的不一样）
				// 3. 执行m.syncPod进行同步给定的status到api server和pod可以进行删除则执行最终的pod删除
				m.syncBatch()
			}
		}
	}, 0)
}

// 从m.podStatuses缓存中获取uid的相关的pod的status，（如果是mirror pod uid则通过static pod uid来查找）
func (m *manager) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	// 通过pod原始的uid（如果是mirror pod uid则通过static pod uid来查找）来查找pod status
	status, ok := m.podStatuses[types.UID(m.podManager.TranslatePodUID(uid))]
	return status.status, ok
}

// 将status规整化后，与内部的m.podStatuses进行比较，发生变化，则发送到m.podStatusChannel进行更新或等待批量更新
func (m *manager) SetPodStatus(pod *v1.Pod, status v1.PodStatus) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	// pod.Status里有非kubelet的condition，则记录错误
	for _, c := range pod.Status.Conditions {
		if !kubetypes.PodConditionByKubelet(c.Type) {
			klog.Errorf("Kubelet is trying to update pod condition %q for pod %q. "+
				"But it is not owned by kubelet.", string(c.Type), format.Pod(pod))
		}
	}
	// Make sure we're caching a deep copy.
	status = *status.DeepCopy()

	// Force a status update if deletion timestamp is set. This is necessary
	// because if the pod is in the non-running state, the pod worker still
	// needs to be able to trigger an update and/or deletion.
	m.updateStatusInternal(pod, status, pod.DeletionTimestamp != nil)
}

func (m *manager) SetContainerReadiness(podUID types.UID, containerID kubecontainer.ContainerID, ready bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		klog.V(4).Infof("Pod %q has been deleted, no need to update readiness", string(podUID))
		return
	}

	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		klog.Warningf("Container readiness changed before pod has synced: %q - %q",
			format.Pod(pod), containerID.String())
		return
	}

	// Find the container to update.
	containerStatus, _, ok := findContainerStatus(&oldStatus.status, containerID.String())
	if !ok {
		klog.Warningf("Container readiness changed for unknown container: %q - %q",
			format.Pod(pod), containerID.String())
		return
	}

	if containerStatus.Ready == ready {
		klog.V(4).Infof("Container readiness unchanged (%v): %q - %q", ready,
			format.Pod(pod), containerID.String())
		return
	}

	// Make sure we're not updating the cached version.
	status := *oldStatus.status.DeepCopy()
	containerStatus, _, _ = findContainerStatus(&status, containerID.String())
	containerStatus.Ready = ready

	// updateConditionFunc updates the corresponding type of condition
	updateConditionFunc := func(conditionType v1.PodConditionType, condition v1.PodCondition) {
		conditionIndex := -1
		for i, condition := range status.Conditions {
			if condition.Type == conditionType {
				conditionIndex = i
				break
			}
		}
		if conditionIndex != -1 {
			status.Conditions[conditionIndex] = condition
		} else {
			klog.Warningf("PodStatus missing %s type condition: %+v", conditionType, status)
			status.Conditions = append(status.Conditions, condition)
		}
	}
	updateConditionFunc(v1.PodReady, GeneratePodReadyCondition(&pod.Spec, status.Conditions, status.ContainerStatuses, status.Phase))
	updateConditionFunc(v1.ContainersReady, GenerateContainersReadyCondition(&pod.Spec, status.ContainerStatuses, status.Phase))
	m.updateStatusInternal(pod, status, false)
}

func (m *manager) SetContainerStartup(podUID types.UID, containerID kubecontainer.ContainerID, started bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		klog.V(4).Infof("Pod %q has been deleted, no need to update startup", string(podUID))
		return
	}

	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		klog.Warningf("Container startup changed before pod has synced: %q - %q",
			format.Pod(pod), containerID.String())
		return
	}

	// Find the container to update.
	containerStatus, _, ok := findContainerStatus(&oldStatus.status, containerID.String())
	if !ok {
		klog.Warningf("Container startup changed for unknown container: %q - %q",
			format.Pod(pod), containerID.String())
		return
	}

	if containerStatus.Started != nil && *containerStatus.Started == started {
		klog.V(4).Infof("Container startup unchanged (%v): %q - %q", started,
			format.Pod(pod), containerID.String())
		return
	}

	// Make sure we're not updating the cached version.
	status := *oldStatus.status.DeepCopy()
	containerStatus, _, _ = findContainerStatus(&status, containerID.String())
	containerStatus.Started = &started

	m.updateStatusInternal(pod, status, false)
}

func findContainerStatus(status *v1.PodStatus, containerID string) (containerStatus *v1.ContainerStatus, init bool, ok bool) {
	// Find the container to update.
	for i, c := range status.ContainerStatuses {
		if c.ContainerID == containerID {
			return &status.ContainerStatuses[i], false, true
		}
	}

	for i, c := range status.InitContainerStatuses {
		if c.ContainerID == containerID {
			return &status.InitContainerStatuses[i], true, true
		}
	}

	return nil, false, false

}

// 设置所有非Terminated或非Waiting的container status为Terminated状态
// 如果pod status发生变化，则更新podStatuses字段中的pod状态，并添加需要更新的status到podStatusChannel
// 如果podStatusChannel满了，则直接放弃，等待周期性syncBatch进行同步
func (m *manager) TerminatePod(pod *v1.Pod) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	// ensure that all containers have a terminated state - because we do not know whether the container
	// was successful, always report an error
	oldStatus := &pod.Status
	if cachedStatus, ok := m.podStatuses[pod.UID]; ok {
		oldStatus = &cachedStatus.status
	}
	status := *oldStatus.DeepCopy()
	for i := range status.ContainerStatuses {
		// container已经处在Terminated和Waiting状态，则直接忽略
		if status.ContainerStatuses[i].State.Terminated != nil || status.ContainerStatuses[i].State.Waiting != nil {
			continue
		}
		status.ContainerStatuses[i].State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				Reason:   "ContainerStatusUnknown",
				Message:  "The container could not be located when the pod was terminated",
				ExitCode: 137,
			},
		}
	}
	for i := range status.InitContainerStatuses {
		// container已经处在Terminated和Waiting状态，则直接忽略
		if status.InitContainerStatuses[i].State.Terminated != nil || status.InitContainerStatuses[i].State.Waiting != nil {
			continue
		}
		status.InitContainerStatuses[i].State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				Reason:   "ContainerStatusUnknown",
				Message:  "The container could not be located when the pod was terminated",
				ExitCode: 137,
			},
		}
	}

	// 提供的status与老的status进行比较，发生更新则进行处理，返回是否触发了更新status操作：
	// status有变化，则更新新的pod status到m.podStatuses字段，并添加需要更新的status到m.podStatusChannel
	// 如果podStatusChannel满了，则直接放弃，等待周期性syncBatch进行同步
	m.updateStatusInternal(pod, status, true)
}

// checkContainerStateTransition ensures that no container is trying to transition
// from a terminated to non-terminated state, which is illegal and indicates a
// logical error in the kubelet.
// restart policy为"Always"，则允许从terminated to non-terminated state
// restart policy为"OnFailure"，则允许从ExitCode不为0的Terminated状态到non-terminated state
// 其他情况则不允许从terminated to non-terminated state
func checkContainerStateTransition(oldStatuses, newStatuses []v1.ContainerStatus, restartPolicy v1.RestartPolicy) error {
	// If we should always restart, containers are allowed to leave the terminated state
	if restartPolicy == v1.RestartPolicyAlways {
		return nil
	}
	for _, oldStatus := range oldStatuses {
		// Skip any container that wasn't terminated
		// 跳过不处于Terminated的container，只检测Terminated状态
		if oldStatus.State.Terminated == nil {
			continue
		}
		// Skip any container that failed but is allowed to restart
		if oldStatus.State.Terminated.ExitCode != 0 && restartPolicy == v1.RestartPolicyOnFailure {
			continue
		}
		for _, newStatus := range newStatuses {
			if oldStatus.Name == newStatus.Name && newStatus.State.Terminated == nil {
				return fmt.Errorf("terminated container %v attempted illegal transition to non-terminated state", newStatus.Name)
			}
		}
	}
	return nil
}

// updateStatusInternal updates the internal status cache, and queues an update to the api server if
// necessary. Returns whether an update was triggered.
// This method IS NOT THREAD SAFE and must be called from a locked function.
//
// 提供的status与老的status进行比较，发生更新则进行处理，返回是否将uid和新的status发送到m.podStatusChannel（是否触发了更新status操作）：
//   如果m.podStatuses中找到pod，则提供的status与缓存中的podStatuses.status进行比较。否则没有找到则下一步
//   如果是static pod，则从pod manager中取mirror pod，来获得老的pod status进行比较。否则没有找到则下一步
//   与提供的pod status进行比较
// status有变化，则保存新的pod status到m.podStatuses字段，并添加需要更新的status到m.podStatusChannel（m.Start()会启动一个goroutine会消费这个chan，进行status更新）
// 如果m.podStatusChannel满了，则直接放弃，等待周期性m.syncBatch进行同步
func (m *manager) updateStatusInternal(pod *v1.Pod, status v1.PodStatus, forceUpdate bool) bool {
	var oldStatus v1.PodStatus
	cachedStatus, isCached := m.podStatuses[pod.UID]
	if isCached {
		oldStatus = cachedStatus.status
		// 根据static pod获取mirror pod
	} else if mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod); ok {
		oldStatus = mirrorPod.Status
	} else {
		oldStatus = pod.Status
	}

	// Check for illegal state transition in containers
	// 检查是否允许container status由terminated to non-terminated，不允许则返回错误
	// restart policy为"Always"，则允许从terminated to non-terminated state
	// restart policy为"OnFailure"，则允许从ExitCode不为0的Terminated状态到non-terminated state
	// 其他情况则不允许从terminated to non-terminated state
	if err := checkContainerStateTransition(oldStatus.ContainerStatuses, status.ContainerStatuses, pod.Spec.RestartPolicy); err != nil {
		klog.Errorf("Status update on pod %v/%v aborted: %v", pod.Namespace, pod.Name, err)
		return false
	}
	if err := checkContainerStateTransition(oldStatus.InitContainerStatuses, status.InitContainerStatuses, pod.Spec.RestartPolicy); err != nil {
		klog.Errorf("Status update on pod %v/%v aborted: %v", pod.Namespace, pod.Name, err)
		return false
	}

	// Set ContainersReadyCondition.LastTransitionTime.
	// 新PodStatus中没有v1.ContainersReady的condition就不更新
	// v1.ContainersReady的condition.Status没有发生变化，lastTransitionTime为oldCondition.LastTransitionTime，不需要更新
	// 其他情况，更新v1.ContainersReady的condition的lastTransitionTime为现在时间
	updateLastTransitionTime(&status, &oldStatus, v1.ContainersReady)

	// Set ReadyCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.PodReady)

	// Set InitializedCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.PodInitialized)

	// Set PodScheduledCondition.LastTransitionTime.
	updateLastTransitionTime(&status, &oldStatus, v1.PodScheduled)

	// ensure that the start time does not change across updates.
	if oldStatus.StartTime != nil && !oldStatus.StartTime.IsZero() {
		status.StartTime = oldStatus.StartTime
	} else if status.StartTime.IsZero() {
		// if the status has no start time, we need to set an initial time
		now := metav1.Now()
		status.StartTime = &now
	}

	// status里的时间转成RFC3339（秒级精度）
	// 对container status按照container名字字母排序
	// initcontainer status按照pod的里initcontainer出现顺序排序
	normalizeStatus(pod, &status)
	// The intent here is to prevent concurrent updates to a pod's status from
	// clobbering each other so the phase of a pod progresses monotonically.
	// 比较status是否一样，condition只比较kubelet condition（PodScheduled、Ready、Initialized、Unschedulable、ContainersReady）
	// 已经在m.podStatuses.status缓存中的，且kubelet管理的condition一样，且不强制更新，则返回false
	if isCached && isPodStatusByKubeletEqual(&cachedStatus.status, &status) && !forceUpdate {
		klog.V(3).Infof("Ignoring same status for pod %q, status: %+v", format.Pod(pod), status)
		return false // No new status.
	}

	newStatus := versionedPodStatus{
		status:       status,
		version:      cachedStatus.version + 1,
		podName:      pod.Name,
		podNamespace: pod.Namespace,
	}
	m.podStatuses[pod.UID] = newStatus

	select {
	case m.podStatusChannel <- podStatusSyncRequest{pod.UID, newStatus}:
		klog.V(5).Infof("Status Manager: adding pod: %q, with status: (%d, %v) to podStatusChannel",
			pod.UID, newStatus.version, newStatus.status)
		return true
	default:
		// Let the periodic syncBatch handle the update if the channel is full.
		// We can't block, since we hold the mutex lock.
		klog.V(4).Infof("Skipping the status update for pod %q for now because the channel is full; status: %+v",
			format.Pod(pod), status)
		return false
	}
}

// updateLastTransitionTime updates the LastTransitionTime of a pod condition.
// 新PodStatus中没有conditionType的condition就不更新
// conditionType的condition.Status没有发生变化，不需要更新
// 其他情况，更新conditionType的condition的lastTransitionTime为现在时间
func updateLastTransitionTime(status, oldStatus *v1.PodStatus, conditionType v1.PodConditionType) {
	_, condition := podutil.GetPodCondition(status, conditionType)
	if condition == nil {
		return
	}
	// Need to set LastTransitionTime.
	lastTransitionTime := metav1.Now()
	_, oldCondition := podutil.GetPodCondition(oldStatus, conditionType)
	if oldCondition != nil && condition.Status == oldCondition.Status {
		lastTransitionTime = oldCondition.LastTransitionTime
	}
	condition.LastTransitionTime = lastTransitionTime
}

// deletePodStatus simply removes the given pod from the status cache.
func (m *manager) deletePodStatus(uid types.UID) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	delete(m.podStatuses, uid)
}

// TODO(filipg): It'd be cleaner if we can do this without signal from user.
func (m *manager) RemoveOrphanedStatuses(podUIDs map[types.UID]bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	for key := range m.podStatuses {
		if _, ok := podUIDs[key]; !ok {
			klog.V(5).Infof("Removing %q from status map.", key)
			delete(m.podStatuses, key)
		}
	}
}

// syncBatch syncs pods statuses with the apiserver.
// 1. 清理m.apiStatusVersion中不需要的uid（uid从m.podStatuses删除，但是在m.apiStatusVersions没有删除且uid的pod不是mirror pod）
// 2. 遍历m.podStatuses查找出需要执行m.syncPod方法的pod uid（m.podStatuses和m.apiStatusVersions里是不一致的，或m.podStatuses和m.apiStatusVersions里是一致的，但是跟pod manager里（等于apiserver里）的不一样）
// 3. 执行m.syncPod进行同步给定的status到api server和pod可以进行删除，则执行最终的pod删除
func (m *manager) syncBatch() {
	var updatedStatuses []podStatusSyncRequest
	// 返回podManager里的
	// 所有普通pod uid对应mirror pod uid的podToMirror（static pod没有对应的mirror pod则对应的mirror pod uid值为""）和
	// mirror pod uid对应static pod uid的mirrorToPod
	podToMirror, mirrorToPod := m.podManager.GetUIDTranslations()
	func() { // Critical section
		m.podStatusesLock.RLock()
		defer m.podStatusesLock.RUnlock()

		// Clean up orphaned versions.
		for uid := range m.apiStatusVersions {
			_, hasPod := m.podStatuses[types.UID(uid)]
			_, hasMirror := mirrorToPod[uid]
			// 即不在m.podStatuses（在m.syncPod方法中从m.podStatuses删除，但是在m.apiStatusVersions没有删除），且不是mirror pod
			// 则从m.apiStatusVersions删除这个uid
			if !hasPod && !hasMirror {
				delete(m.apiStatusVersions, uid)
			}
		}

		// 遍历m.podStatuses查找出需要执行m.syncPod方法的pod uid
		// 先执行m.needsUpdate，判断versionedPodStatus的版本跟内部的m.apiStatusVersions里的版本进行比较，如果m.apiStatusVersions里的版本比较落后，那么直接放到需要执行m.syncPod方法的列表里。说明pod在status manager内部的m.podStatuses和m.apiStatusVersions里是不一致的
		// 否则再执行m.needsReconcile，判断在pod manager（等效于apiserver中）中的pod status和m.podStatuses里的status是否一致，不一致则从m.apiStatusVersions里删除这个pod的uid，然后放到需要执行m.syncPod方法的列表里。说明pod在status manager内部的m.podStatuses和m.apiStatusVersions里是一致的，还需要跟pod manager（等效于apiserver中）中的pod status进行比较。
		for uid, status := range m.podStatuses {
			// 将uid转成kubetypes.MirrorPodUID，用在m.needsUpdate方法（在m.apiStatusVersions里查找version）和删除m.apiStatusVersions里的条目
			syncedUID := kubetypes.MirrorPodUID(uid)
			if mirrorUID, ok := podToMirror[kubetypes.ResolvedPodUID(uid)]; ok {
				// m.podManager.GetUIDTranslations()里返回的podToMirror里，如果static pod没有mirror pod，则mirrorUID为空
				// 那么直接忽略
				if mirrorUID == "" {
					klog.V(5).Infof("Static pod %q (%s/%s) does not have a corresponding mirror pod; skipping", uid, status.podName, status.podNamespace)
					continue
				}
				// 如果是static pod，则syncedUID为mirror pod的kubetypes.MirrorPodUID，因为m.needsUpdate方法里会从m.apiStatusVersions里面取状态，需要用到mirror pod的kubetypes.MirrorPodUID
				syncedUID = mirrorUID
			}
			// 需要更新满足下面任何一个条件：
			// 1. 状态已经过时-- apiStatusVersions状态过时
			// 2. podManager里有相应pod（普通pod和static pod）且pod能够被删除
			// 即static pod只要满足：状态已经过时--apiStatusVersions状态过时
			// m.needsUpdate会拿status versionedPodStatus的版本跟内部的m.apiStatusVersions里的版本进行比较和判断pod能否进行最终删除
			if m.needsUpdate(types.UID(syncedUID), status) {
				updatedStatuses = append(updatedStatuses, podStatusSyncRequest{uid, status})

			// m.needsReconcile会拿pod manager中的pod status（apiserver中的status）与status.status进行比较
			// pod manager里的pod的status（apiserver中的status）与这里的status不一致
			} else if m.needsReconcile(uid, status.status) {
				// Delete the apiStatusVersions here to force an update on the pod status
				// In most cases the deleted apiStatusVersions here should be filled
				// soon after the following syncPod() [If the syncPod() sync an update
				// successfully].
				// 
				// 删除m.apiStatusVersions--下面执行的m.syncPod()里的m.needsUpdate(uid, status)就为true，就能继续执行
				delete(m.apiStatusVersions, syncedUID)
				updatedStatuses = append(updatedStatuses, podStatusSyncRequest{uid, status})
			}
		}
	}()

	for _, update := range updatedStatuses {
		klog.V(5).Infof("Status Manager: syncPod in syncbatch. pod UID: %q", update.podUID)
		// 同步给定的status到api server，以下条件都要满足
		// 1. pod的status真的要更新
		// 2. pod uid没有发生了变化
		// 
		// 这里还处理了pod已经可以被最终删除情况，在api server中删除pod（设置GracePeriodSeconds为0进行删除）
		m.syncPod(update.podUID, update.status)
	}
}

// syncPod syncs the given status with the API server. The caller must not hold the lock.
// 同步给定的status到api server，以下条件都要满足
// 1. pod的status真的要更新
// 2. pod uid没有发生了变化
// 
// 这里还处理了pod已经可以被删除情况，在api server中删除pod（设置GracePeriodSeconds为0进行删除）
func (m *manager) syncPod(uid types.UID, status versionedPodStatus) {
	// pod需要更新必须满足下面一个条件：
	// 1. pod状态新发现或pod状态过时，pod uid在m.apiStatusVersions中没有找到，或versionedPodStatus的版本大于m.apiStatusVersions
	// 2. podManager里有相应pod（普通pod和static pod）且pod能够被删除
	// 
	// pod状态不需要更新
	if !m.needsUpdate(uid, status) {
		klog.V(1).Infof("Status for pod %q is up-to-date; skipping", uid)
		return
	}

	// TODO: make me easier to express from client code
	// 获取普通pod或mirror pod
	pod, err := m.kubeClient.CoreV1().Pods(status.podNamespace).Get(context.TODO(), status.podName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		klog.V(3).Infof("Pod %q does not exist on the server", format.PodDesc(status.podName, status.podNamespace, uid))
		// If the Pod is deleted the status will be cleared in
		// RemoveOrphanedStatuses, so we just ignore the update here.
		return
	}
	if err != nil {
		klog.Warningf("Failed to get status for pod %q: %v", format.PodDesc(status.podName, status.podNamespace, uid), err)
		return
	}

	// apiserver中没有static pod，只有mirror pod，而提供的uid是static pod uid或普通pod uid，所以（mirror pod uid返回static pod的uid）
	// 根据uid从pod manager中获取普通的pod的uid（mirror pod uid返回static pod的uid）
	translatedUID := m.podManager.TranslatePodUID(pod.UID)
	// Type convert original uid just for the purpose of comparison.
	// uid是kubelet内部function能够识别的uid--非mirror pod的uid
	// 发生uid变化（apiserver获取到的pod的uid（转成非mirror pod的uid）与提供不一样）
	if len(translatedUID) > 0 && translatedUID != kubetypes.ResolvedPodUID(uid) {
		klog.V(2).Infof("Pod %q was deleted and then recreated, skipping status update; old UID %q, new UID %q", format.Pod(pod), uid, translatedUID)
		// pod uid发生了变化（pod先被删除后又被创建），从m.podStatuses中删除uid
		m.deletePodStatus(uid)
		return
	}

	oldStatus := pod.Status.DeepCopy()
	newPod, patchBytes, unchanged, err := statusutil.PatchPodStatus(m.kubeClient, pod.Namespace, pod.Name, pod.UID, *oldStatus, mergePodStatus(*oldStatus, status.status))
	klog.V(3).Infof("Patch status for pod %q with %q", format.Pod(pod), patchBytes)
	if err != nil {
		klog.Warningf("Failed to update status for pod %q: %v", format.Pod(pod), err)
		return
	}
	if unchanged {
		klog.V(3).Infof("Status for pod %q is up-to-date: (%d)", format.Pod(pod), status.version)
	} else {
		klog.V(3).Infof("Status for pod %q updated successfully: (%d, %+v)", format.Pod(pod), status.version, status.status)
		pod = newPod
	}

	// 这里保存的是apiserver里的mirror pod或mirror pod的uid
	// 更新m.apiStatusVersions的值为versionedPodStatus的version值
	m.apiStatusVersions[kubetypes.MirrorPodUID(pod.UID)] = status.version

	// We don't handle graceful deletion of mirror pods.
	// 判断pod是否能够被删除
	// 能够删除需同时满足
	// 1. pod的DeletionTimestamp不为nil
	// 2. 不是mirror pod
	// 3. container不再运行且没有volume挂载和pod的cgroup目录在所有cgroup子系统必须都不存在
	if m.canBeDeleted(pod, status.status) {
		deleteOptions := metav1.DeleteOptions{
			GracePeriodSeconds: new(int64),
			// Use the pod UID as the precondition for deletion to prevent deleting a
			// newly created pod with the same name and namespace.
			Preconditions: metav1.NewUIDPreconditions(string(pod.UID)),
		}
		err = m.kubeClient.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, deleteOptions)
		if err != nil {
			klog.Warningf("Failed to delete status for pod %q: %v", format.Pod(pod), err)
			return
		}
		klog.V(3).Infof("Pod %q fully terminated and removed from etcd", format.Pod(pod))
		//pod已经被删除，则从m.podStatuses中删除uid，这里没有从m.apiStatusVersions中删除uid, 而是在m.syncBatch()中统一删除
		m.deletePodStatus(uid)
	}
}

// needsUpdate returns whether the status is stale for the given pod UID.
// This method is not thread safe, and must only be accessed by the sync thread.
// pod需要更新必须满足下面一个条件：
// 1. pod状态新发现或pod状态过时，pod uid在m.apiStatusVersions中没有找到，或versionedPodStatus的版本大于m.apiStatusVersions
// 2. podManager里有相应pod（普通pod和static pod）且pod能够被删除
func (m *manager) needsUpdate(uid types.UID, status versionedPodStatus) bool {
	// 如果提供的uid是static，则在m.apiStatusVersions中一定找不到，ok一定是false，直接返回true
	latest, ok := m.apiStatusVersions[kubetypes.MirrorPodUID(uid)]
	// pod uid在m.apiStatusVersions中没有查找到，或versionedPodStatus的版本大于m.apiStatusVersions，则返回true
	if !ok || latest < status.version {
		return true
	}

	// 到这里只能是mirror pod或普通pod
	// mirror pod的GetPodByUID(uid)一定是不存在，return false
	// 根据uid返回非mirror pod（普通pod和static pod）
	pod, ok := m.podManager.GetPodByUID(uid)
	if !ok {
		// mirror pod或普通pod不存在，则
		return false
	}
	// 到这里只能是普通pod
	// 能够删除需同时满足
	// 1. pod的DeletionTimestamp不为nil
	// 2. 不是mirror pod
	// 3. container不再运行且没有volume挂载和pod的cgroup目录在所有cgroup子系统必须都不存在
	return m.canBeDeleted(pod, status.status)
}

// 判断pod是否能够被删除
// 能够删除需同时满足
// 1. pod的DeletionTimestamp不为nil
// 2. 不是mirror pod
// 3. container不再运行且没有volume挂载和pod的cgroup目录在所有cgroup子系统必须都不存在
func (m *manager) canBeDeleted(pod *v1.Pod, status v1.PodStatus) bool {
	if pod.DeletionTimestamp == nil || kubetypes.IsMirrorPod(pod) {
		return false
	}
	// pod资源已经被回收，下面条件全都要满足
	// 提供的status里的container都不再运行
	// kl.podCache里没有container运行状态
	// pod在node上没有已挂载的volume
	// 如果启用了CgroupsPerQOS，则pod的cgroup路径在所有cgroup子系统必须都不存在
	return m.podDeletionSafety.PodResourcesAreReclaimed(pod, status)
}

// needsReconcile compares the given status with the status in the pod manager (which
// in fact comes from apiserver), returns whether the status needs to be reconciled with
// the apiserver. Now when pod status is inconsistent between apiserver and kubelet,
// kubelet should forcibly send an update to reconcile the inconsistence, because kubelet
// should be the source of truth of pod status.
// NOTE(random-liu): It's simpler to pass in mirror pod uid and get mirror pod by uid, but
// now the pod manager only supports getting mirror pod by static pod, so we have to pass
// static pod uid here.
// TODO(random-liu): Simplify the logic when mirror pod manager is added.
// pod manger里获得的pod status（apiserver中的status）与给定的status进行比较，返回是否要进行reconcile
func (m *manager) needsReconcile(uid types.UID, status v1.PodStatus) bool {
	// The pod could be a static pod, so we should translate first.
	// 根据uid返回非mirror pod（普通pod和static pod）
	pod, ok := m.podManager.GetPodByUID(uid)
	if !ok {
		klog.V(4).Infof("Pod %q has been deleted, no need to reconcile", string(uid))
		return false
	}
	// If the pod is a static pod, we should check its mirror pod, because only status in mirror pod is meaningful to us.
	// pod的annotation["kubernetes.io/config.source"]值是api
	if kubetypes.IsStaticPod(pod) {
		// 根据static pod获取mirror pod
		mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod)
		if !ok {
			klog.V(4).Infof("Static pod %q has no corresponding mirror pod, no need to reconcile", format.Pod(pod))
			return false
		}
		pod = mirrorPod
	}

	podStatus := pod.Status.DeepCopy()
	// status里的时间转成RFC3339（秒级精度），对container status按照container名字字母排序，initcontainer status按照pod的里initcontainer出现顺序排序
	normalizeStatus(pod, podStatus)

	// 比较pod manger里获得的pod status（apiserver中的status）与给定的status
	// 比较status是否一样，condition只比较kubelet condition（PodScheduled、Ready、Initialized、Unschedulable、ContainersReady），非kubelet condition不比较
	if isPodStatusByKubeletEqual(podStatus, &status) {
		// If the status from the source is the same with the cached status,
		// reconcile is not needed. Just return.
		return false
	}
	klog.V(3).Infof("Pod status is inconsistent with cached status for pod %q, a reconciliation should be triggered:\n %s", format.Pod(pod),
		diff.ObjectDiff(podStatus, &status))

	return true
}

// normalizeStatus normalizes nanosecond precision timestamps in podStatus
// down to second precision (*RFC339NANO* -> *RFC3339*). This must be done
// before comparing podStatus to the status returned by apiserver because
// apiserver does not support RFC339NANO.
// Related issue #15262/PR #15263 to move apiserver to RFC339NANO is closed.
// status里的时间转成RFC3339（秒级精度）
// 对container status按照container名字字母排序
// initcontainer status按照pod的里initcontainer出现顺序排序
func normalizeStatus(pod *v1.Pod, status *v1.PodStatus) *v1.PodStatus {
	bytesPerStatus := kubecontainer.MaxPodTerminationMessageLogLength
	if containers := len(pod.Spec.Containers) + len(pod.Spec.InitContainers); containers > 0 {
		bytesPerStatus = bytesPerStatus / containers
	}
	normalizeTimeStamp := func(t *metav1.Time) {
		*t = t.Rfc3339Copy()
	}
	normalizeContainerState := func(c *v1.ContainerState) {
		if c.Running != nil {
			normalizeTimeStamp(&c.Running.StartedAt)
		}
		if c.Terminated != nil {
			normalizeTimeStamp(&c.Terminated.StartedAt)
			normalizeTimeStamp(&c.Terminated.FinishedAt)
			// Terminated.Message超出部分截掉，所有container总的最大12k，每个container平均分配上限
			if len(c.Terminated.Message) > bytesPerStatus {
				c.Terminated.Message = c.Terminated.Message[:bytesPerStatus]
			}
		}
	}

	if status.StartTime != nil {
		normalizeTimeStamp(status.StartTime)
	}
	for i := range status.Conditions {
		condition := &status.Conditions[i]
		normalizeTimeStamp(&condition.LastProbeTime)
		normalizeTimeStamp(&condition.LastTransitionTime)
	}

	// update container statuses
	for i := range status.ContainerStatuses {
		cstatus := &status.ContainerStatuses[i]
		normalizeContainerState(&cstatus.State)
		normalizeContainerState(&cstatus.LastTerminationState)
	}
	// Sort the container statuses, so that the order won't affect the result of comparison
	// 根据container的name进行正排序
	sort.Sort(kubetypes.SortedContainerStatuses(status.ContainerStatuses))

	// update init container statuses
	for i := range status.InitContainerStatuses {
		cstatus := &status.InitContainerStatuses[i]
		normalizeContainerState(&cstatus.State)
		normalizeContainerState(&cstatus.LastTerminationState)
	}
	// Sort the container statuses, so that the order won't affect the result of comparison
	// 根据init container在pod中的顺序对init container的status进行排序
	kubetypes.SortInitContainerStatuses(pod, status.InitContainerStatuses)
	return status
}

// mergePodStatus merges oldPodStatus and newPodStatus where pod conditions
// not owned by kubelet is preserved from oldPodStatus
// 保留oldPodStatus非kubelet condition（比如readiness gate），其他以newPodStatus为准
func mergePodStatus(oldPodStatus, newPodStatus v1.PodStatus) v1.PodStatus {
	podConditions := []v1.PodCondition{}
	for _, c := range oldPodStatus.Conditions {
		if !kubetypes.PodConditionByKubelet(c.Type) {
			podConditions = append(podConditions, c)
		}
	}

	for _, c := range newPodStatus.Conditions {
		if kubetypes.PodConditionByKubelet(c.Type) {
			podConditions = append(podConditions, c)
		}
	}
	newPodStatus.Conditions = podConditions
	return newPodStatus
}

// NeedToReconcilePodReadiness returns if the pod "Ready" condition need to be reconcile
func NeedToReconcilePodReadiness(pod *v1.Pod) bool {
	if len(pod.Spec.ReadinessGates) == 0 {
		return false
	}
	podReadyCondition := GeneratePodReadyCondition(&pod.Spec, pod.Status.Conditions, pod.Status.ContainerStatuses, pod.Status.Phase)
	i, curCondition := podutil.GetPodConditionFromList(pod.Status.Conditions, v1.PodReady)
	// Only reconcile if "Ready" condition is present and Status or Message is not expected
	if i >= 0 && (curCondition.Status != podReadyCondition.Status || curCondition.Message != podReadyCondition.Message) {
		return true
	}
	return false
}
