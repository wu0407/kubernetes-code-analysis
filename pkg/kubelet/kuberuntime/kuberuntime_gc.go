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

package kuberuntime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// containerGC is the manager of garbage collection.
type containerGC struct {
	client           internalapi.RuntimeService
	manager          *kubeGenericRuntimeManager
	podStateProvider podStateProvider
}

// NewContainerGC creates a new containerGC.
func newContainerGC(client internalapi.RuntimeService, podStateProvider podStateProvider, manager *kubeGenericRuntimeManager) *containerGC {
	return &containerGC{
		client:           client,
		manager:          manager,
		podStateProvider: podStateProvider,
	}
}

// containerGCInfo is the internal information kept for containers being considered for GC.
type containerGCInfo struct {
	// The ID of the container.
	id string
	// The name of the container.
	name string
	// Creation time for the container.
	createTime time.Time
	// If true, the container is in unknown state. Garbage collector should try
	// to stop containers before removal.
	unknown bool
}

// sandboxGCInfo is the internal information kept for sandboxes being considered for GC.
type sandboxGCInfo struct {
	// The ID of the sandbox.
	id string
	// Creation time for the sandbox.
	createTime time.Time
	// If true, the sandbox is ready or still has containers.
	active bool
}

// evictUnit is considered for eviction as units of (UID, container name) pair.
type evictUnit struct {
	// UID of the pod.
	uid types.UID
	// Name of the container in the pod.
	name string
}

type containersByEvictUnit map[evictUnit][]containerGCInfo
type sandboxesByPodUID map[types.UID][]sandboxGCInfo

// NumContainers returns the number of containers in this map.
// 总的container数量
func (cu containersByEvictUnit) NumContainers() int {
	num := 0
	for key := range cu {
		num += len(cu[key])
	}
	return num
}

// NumEvictUnits returns the number of pod in this map.
func (cu containersByEvictUnit) NumEvictUnits() int {
	return len(cu)
}

// Newest first.
type byCreated []containerGCInfo

func (a byCreated) Len() int           { return len(a) }
func (a byCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// Newest first.
type sandboxByCreated []sandboxGCInfo

func (a sandboxByCreated) Len() int           { return len(a) }
func (a sandboxByCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a sandboxByCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// enforceMaxContainersPerEvictUnit enforces MaxPerPodContainer for each evictUnit.
// 从evictUnits列表里每个evictUnit保留MaxContainers数量的container，先移除早创建的老的container，evictUnits更新为剩下保留的container containerGCInfo
func (cgc *containerGC) enforceMaxContainersPerEvictUnit(evictUnits containersByEvictUnit, MaxContainers int) {
	for key := range evictUnits {
		toRemove := len(evictUnits[key]) - MaxContainers

		if toRemove > 0 {
			// 从evictUnits[key]列表移除toRemove数量的container，先移除早创建的老的container，返回剩下保留的container containerGCInfo
			evictUnits[key] = cgc.removeOldestN(evictUnits[key], toRemove)
		}
	}
}

// removeOldestN removes the oldest toRemove containers and returns the resulting slice.
// 从containers列表移除toRemove数量的container，先移除早创建的老的container，返回剩下保留的container containerGCInfo
func (cgc *containerGC) removeOldestN(containers []containerGCInfo, toRemove int) []containerGCInfo {
	// Remove from oldest to newest (last to first).
	numToKeep := len(containers) - toRemove
	// 从后面的item开始，即先移除早创建的老的container
	for i := len(containers) - 1; i >= numToKeep; i-- {
		// container处于unknown状态
		if containers[i].unknown {
			// Containers in known state could be running, we should try
			// to stop it before removal.
			id := kubecontainer.ContainerID{
				Type: cgc.manager.runtimeName,
				ID:   containers[i].id,
			}
			message := "Container is in unknown state, try killing it before removal"
			// 停止container
			// 1. 执行prestophook
			// 2. 调用runtime停止container
			// 3. 删除掉m.containerRefManager里containerIDToRef里有关这个container id的记录
			if err := cgc.manager.killContainer(nil, id, containers[i].name, message, nil); err != nil {
				klog.Errorf("Failed to stop container %q: %v", containers[i].id, err)
				continue
			}
		}
		// 先释放container ID在cgc.manager.internalLifecycle.topologyManager.podTopologyHints分配资源
		// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
		// 调用runtime移除容器
		if err := cgc.manager.removeContainer(containers[i].id); err != nil {
			klog.Errorf("Failed to remove container %q: %v", containers[i].id, err)
		}
	}

	// Assume we removed the containers so that we're not too aggressive.
	return containers[:numToKeep]
}

// removeOldestNSandboxes removes the oldest inactive toRemove sandboxes and
// returns the resulting slice.
// 先移除toRemove数量老的早创建的sandbox，且active为false（不为running且没有container属于它）
func (cgc *containerGC) removeOldestNSandboxes(sandboxes []sandboxGCInfo, toRemove int) {
	// Remove from oldest to newest (last to first).
	numToKeep := len(sandboxes) - toRemove
	for i := len(sandboxes) - 1; i >= numToKeep; i-- {
		if !sandboxes[i].active {
			cgc.removeSandbox(sandboxes[i].id)
		}
	}
}

// removeSandbox removes the sandbox by sandboxID.
// 移除sandbox容器
func (cgc *containerGC) removeSandbox(sandboxID string) {
	klog.V(4).Infof("Removing sandbox %q", sandboxID)
	// In normal cases, kubelet should've already called StopPodSandbox before
	// GC kicks in. To guard against the rare cases where this is not true, try
	// stopping the sandbox before removing it.
	// 调用cri，执行StopPodSandbox，停止sandbox容器，释放容器ip
	if err := cgc.client.StopPodSandbox(sandboxID); err != nil {
		klog.Errorf("Failed to stop sandbox %q before removing: %v", sandboxID, err)
		return
	}
	// 调用cri，执行RemovePodSandbox
	// 如果runtime为dockershim
	// 获取所有属于sandbox的普通容器
	// 先移除所有属于sandbox的普通容器（先移除kubelet创建的日志软链，然后再执行docker container rm移除容器）
	// 再移除sandbox容器，类似docker rm -v -f {container id}
	// 移除"/var/lib/dockershim/sandbox/{sandbox id}"文件
	if err := cgc.client.RemovePodSandbox(sandboxID); err != nil {
		klog.Errorf("Failed to remove sandbox %q: %v", sandboxID, err)
	}
}

// evictableContainers gets all containers that are evictable. Evictable containers are: not running
// and created more than MinAge ago.
// 获取可以驱逐的container，包括非running container且container createdAt时间距离现在已经经过minAge。对同一个evictUnits里的container进行排序（createdAt时间比较晚的排在前面）
func (cgc *containerGC) evictableContainers(minAge time.Duration) (containersByEvictUnit, error) {
	// 获取runtime里所有container（包括running和非running的）
	containers, err := cgc.manager.getKubeletContainers(true)
	if err != nil {
		return containersByEvictUnit{}, err
	}

	evictUnits := make(containersByEvictUnit)
	newestGCTime := time.Now().Add(-minAge)
	for _, container := range containers {
		// Prune out running containers.
		// 过滤掉正在running的container
		if container.State == runtimeapi.ContainerState_CONTAINER_RUNNING {
			continue
		}

		createdAt := time.Unix(0, container.CreatedAt)
		// 排除掉container createdAt时间距离现在还没经过minAge
		if newestGCTime.Before(createdAt) {
			continue
		}

		// 获取pod name、pod namespace、podUID、pod里面的container name
		labeledInfo := getContainerInfoFromLabels(container.Labels)
		containerInfo := containerGCInfo{
			id:         container.Id,
			name:       container.Metadata.Name,
			createTime: createdAt,
			unknown:    container.State == runtimeapi.ContainerState_CONTAINER_UNKNOWN,
		}
		key := evictUnit{
			uid:  labeledInfo.PodUID,
			name: containerInfo.name,
		}
		evictUnits[key] = append(evictUnits[key], containerInfo)
	}

	// Sort the containers by age.
	for uid := range evictUnits {
		// createdAt时间比较晚的排在前面
		sort.Sort(byCreated(evictUnits[uid]))
	}

	return evictUnits, nil
}

// evict all containers that are evictable
// 获取非running container且container createdAt时间距离现在已经经过minAge的container
// allSourcesReady为true，则移除所有可以驱逐的container
// 根据gcPolicy.MaxPerPodContainer，移除每个pod里老的可以移除的container
// 根据gcPolicy.MaxContainers，移除pod里或所有container里老的可以移除的container
func (cgc *containerGC) evictContainers(gcPolicy kubecontainer.ContainerGCPolicy, allSourcesReady bool, evictTerminatedPods bool) error {
	// Separate containers by evict units.
	// 获取可以驱逐的container，包括非running container且container createdAt时间距离现在已经经过minAge。对同一个evictUnits里的container进行排序（createdAt时间比较晚的排在前面，比较早的排在后面）
	evictUnits, err := cgc.evictableContainers(gcPolicy.MinAge)
	if err != nil {
		return err
	}

	// Remove deleted pod containers if all sources are ready.
	// allSourcesReady为true，则移除所有可以驱逐的container
	if allSourcesReady {
		for key, unit := range evictUnits {
			// IsPodDeleted返回为true的情况（或的关系）
			// pod manager里没有发现uid的pod
			// pod的Phase为"Failed"且reason为"Evicted"
			// pod被删除且所有container的state都处于Terminated或Waiting
			// 
			// IsPodTerminated返回为true情况（或的关系）
			// pod manager里没有发现uid的pod
			// pod的Phase为"Failed"或"Succeeded"
			// pod被删除且所有container都处于Terminated或Waiting
			// 
			// pod删除了或pod处于Terminated状态且evictTerminatedPods为true
			if cgc.podStateProvider.IsPodDeleted(key.uid) || (cgc.podStateProvider.IsPodTerminated(key.uid) && evictTerminatedPods) {
				// 移除所有container
				cgc.removeOldestN(unit, len(unit)) // Remove all.
				// 从evictUnits中移除
				delete(evictUnits, key)
			}
		}
	}

	// Enforce max containers per evict unit.
	// pod最多dead container数量大于等于0
	if gcPolicy.MaxPerPodContainer >= 0 {
		// 从evictUnits列表里每个evictUnit保留gcPolicy.MaxPerPodContainer数量的container，先移除早创建的老的container，返回剩下保留的container containerGCInfo
		cgc.enforceMaxContainersPerEvictUnit(evictUnits, gcPolicy.MaxPerPodContainer)
	}

	// Enforce max total number of containers.
	// 定义了总的最多dead container数量，且现有总的container数量大于gcPolicy.MaxContainers
	if gcPolicy.MaxContainers >= 0 && evictUnits.NumContainers() > gcPolicy.MaxContainers {
		// Leave an equal number of containers per evict unit (min: 1).
		// 计算平均每个evictUnit保留多少个container，最小值为1
		numContainersPerEvictUnit := gcPolicy.MaxContainers / evictUnits.NumEvictUnits()
		if numContainersPerEvictUnit < 1 {
			numContainersPerEvictUnit = 1
		}
		// 从evictUnits列表里每个evictUnit保留numContainersPerEvictUnit数量的container，先移除早创建的老的container，evictUnits更新为剩下保留的container containerGCInfo
		cgc.enforceMaxContainersPerEvictUnit(evictUnits, numContainersPerEvictUnit)

		// If we still need to evict, evict oldest first.
		// 总的container数量还是大于gcPolicy.MaxContainers，则对所有container进行排序（createdAt时间比较晚的排在前面），然后保留gcPolicy.MaxContainerscontainer
		numContainers := evictUnits.NumContainers()
		if numContainers > gcPolicy.MaxContainers {
			flattened := make([]containerGCInfo, 0, numContainers)
			for key := range evictUnits {
				flattened = append(flattened, evictUnits[key]...)
			}
			// createdAt时间比较晚的排在前面
			sort.Sort(byCreated(flattened))

			// 从flattened列表移除超出gcPolicy.MaxContainers数量的container，先移除早创建的老的container，返回剩下保留的container containerGCInfo
			cgc.removeOldestN(flattened, numContainers-gcPolicy.MaxContainers)
		}
	}
	return nil
}

// evictSandboxes remove all evictable sandboxes. An evictable sandbox must
// meet the following requirements:
//   1. not in ready state
//   2. contains no containers.
//   3. belong to a non-existent (i.e., already removed) pod, or is not the
//      most recently created sandbox for the pod.
// 如果pod删除了或pod处于Terminated状态且evictTerminatedPods为true，则移除所有不为running且没有container属于它的sandbox容器
// 如果pod没有被删除了，且pod不处于Terminated状态或pod处于Terminated状态且evictTerminatedPods为false，则保留pod的最近一个sanbox容器（移除其他老的、不为running且没有container属于它的pod）
func (cgc *containerGC) evictSandboxes(evictTerminatedPods bool) error {
	// 获取runtime里所有container（包括running和非running的）
	containers, err := cgc.manager.getKubeletContainers(true)
	if err != nil {
		return err
	}

	// 获得runtime里所有的sanbox容器（包括running和非running的）
	sandboxes, err := cgc.manager.getKubeletSandboxes(true)
	if err != nil {
		return err
	}

	// collect all the PodSandboxId of container
	sandboxIDs := sets.NewString()
	for _, container := range containers {
		sandboxIDs.Insert(container.PodSandboxId)
	}

	sandboxesByPod := make(sandboxesByPodUID)
	for _, sandbox := range sandboxes {
		podUID := types.UID(sandbox.Metadata.Uid)
		sandboxInfo := sandboxGCInfo{
			id:         sandbox.Id,
			createTime: time.Unix(0, sandbox.CreatedAt),
		}

		// Set ready sandboxes to be active.
		if sandbox.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			sandboxInfo.active = true
		}

		// Set sandboxes that still have containers to be active.
		// sandbox不为running状态，但是container中所属于的sandbox列表，包含这个sandbox
		if sandboxIDs.Has(sandbox.Id) {
			sandboxInfo.active = true
		}

		sandboxesByPod[podUID] = append(sandboxesByPod[podUID], sandboxInfo)
	}

	// Sort the sandboxes by age.
	for uid := range sandboxesByPod {
		// createdAt时间比较晚的排在前面
		// 对每个uid的sandbox列表进行排序
		sort.Sort(sandboxByCreated(sandboxesByPod[uid]))
	}

	for podUID, sandboxes := range sandboxesByPod {
		// IsPodDeleted返回为true的情况（或的关系）
		// pod manager里没有发现uid的pod
		// pod的Phase为"Failed"且reason为"Evicted"
		// pod被删除且所有container的state都处于Terminated或Waiting
		// 
		// IsPodTerminated返回为true情况（或的关系）
		// pod manager里没有发现uid的pod
		// pod的Phase为"Failed"或"Succeeded"
		// pod被删除且所有container都处于Terminated或Waiting
		// 
		// pod删除了或pod处于Terminated状态且evictTerminatedPods为true
		if cgc.podStateProvider.IsPodDeleted(podUID) || (cgc.podStateProvider.IsPodTerminated(podUID) && evictTerminatedPods) {
			// Remove all evictable sandboxes if the pod has been removed.
			// Note that the latest dead sandbox is also removed if there is
			// already an active one.
			// 移除pod里所有sandbox container，且active为false（不为running且没有container属于它）
			cgc.removeOldestNSandboxes(sandboxes, len(sandboxes))
		} else {
			// Keep latest one if the pod still exists.
			// 如果pod没有被删除了，且pod不处于Terminated状态或pod处于Terminated状态且evictTerminatedPods为false，则保留pod的最近一个sanbox容器（移除其他老的、不为running且没有container属于它的pod）
			cgc.removeOldestNSandboxes(sandboxes, len(sandboxes)-1)
		}
	}
	return nil
}

// evictPodLogsDirectories evicts all evictable pod logs directories. Pod logs directories
// are evictable if there are no corresponding pods.
// allSourcesReady为true，且pod（pod uid从目录名中提取）不为删除状态，则递归移除目录"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"
// "/var/log/containers/*.log"文件，如果链接的目标文件不存在，则移除这个链接
func (cgc *containerGC) evictPodLogsDirectories(allSourcesReady bool) error {
	osInterface := cgc.manager.osInterface
	if allSourcesReady {
		// Only remove pod logs directories when all sources are ready.
		// "/var/log/pods"
		dirs, err := osInterface.ReadDir(podLogsRootDirectory)
		if err != nil {
			return fmt.Errorf("failed to read podLogsRootDirectory %q: %v", podLogsRootDirectory, err)
		}
		for _, dir := range dirs {
			name := dir.Name()
			// 从目录名"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"解析出pod uid
			podUID := parsePodUIDFromLogsDirectory(name)
			// IsPodDeleted返回为true的情况（或的关系）
			// pod manager里没有发现uid的pod
			// pod的Phase为"Failed"且reason为"Evicted"
			// pod被删除且所有container的state都处于Terminated或Waiting
			
			// 忽略pod不为删除状态
			if !cgc.podStateProvider.IsPodDeleted(podUID) {
				continue
			}
			// 递归移除目录"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"
			err := osInterface.RemoveAll(filepath.Join(podLogsRootDirectory, name))
			if err != nil {
				klog.Errorf("Failed to remove pod logs directory %q: %v", name, err)
			}
		}
	}

	// Remove dead container log symlinks.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	// "/var/log/containers/*.log"
	logSymlinks, _ := osInterface.Glob(filepath.Join(legacyContainerLogsDir, fmt.Sprintf("*.%s", legacyLogSuffix)))
	for _, logSymlink := range logSymlinks {
		// 链接的目标文件不存在，则移除这个链接
		if _, err := osInterface.Stat(logSymlink); os.IsNotExist(err) {
			err := osInterface.Remove(logSymlink)
			if err != nil {
				klog.Errorf("Failed to remove container log dead symlink %q: %v", logSymlink, err)
			}
		}
	}
	return nil
}

// GarbageCollect removes dead containers using the specified container gc policy.
// Note that gc policy is not applied to sandboxes. Sandboxes are only removed when they are
// not ready and containing no containers.
//
// GarbageCollect consists of the following steps:
// * gets evictable containers which are not active and created more than gcPolicy.MinAge ago.
// * removes oldest dead containers for each pod by enforcing gcPolicy.MaxPerPodContainer.
// * removes oldest dead containers by enforcing gcPolicy.MaxContainers.
// * gets evictable sandboxes which are not ready and contains no containers.
// * removes evictable sandboxes.
// 移除可以移除的container
// 获取非running container且container createdAt时间距离现在已经经过minAge的container
// allSourcesReady为true，则移除所有可以驱逐的container
// 根据gcPolicy.MaxPerPodContainer，移除每个pod里老的可以移除的container
// 根据gcPolicy.MaxContainers，移除pod里或所有container里老的可以移除的container
//
// 移除可以移除的sandbox
// 如果pod删除了或pod处于Terminated状态且evictTerminatedPods为true，则移除所有不为running且没有container属于它的sandbox容器
// 如果pod没有被删除了，且pod不处于Terminated状态或pod处于Terminated状态且evictTerminatedPods为false，则保留pod的最近一个sanbox容器
//
// 移除pod日志目录
// allSourcesReady为true，且pod（pod uid从目录名中提取）不为删除状态，则递归移除目录"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"
// "/var/log/containers/*.log"文件，如果链接的目标文件不存在，则移除这个链接
func (cgc *containerGC) GarbageCollect(gcPolicy kubecontainer.ContainerGCPolicy, allSourcesReady bool, evictTerminatedPods bool) error {
	errors := []error{}
	// Remove evictable containers
	// 获取非running container且container createdAt时间距离现在已经经过minAge的container
	// allSourcesReady为true，则移除所有可以驱逐的container
	// 根据gcPolicy.MaxPerPodContainer，移除每个pod里老的可以移除的container
	// 根据gcPolicy.MaxContainers，移除pod里或所有container里老的可以移除的container
	if err := cgc.evictContainers(gcPolicy, allSourcesReady, evictTerminatedPods); err != nil {
		errors = append(errors, err)
	}

	// Remove sandboxes with zero containers
	// 如果pod删除了或pod处于Terminated状态且evictTerminatedPods为true，则移除所有不为running且没有container属于它的sandbox容器
	// 如果pod没有被删除了，且pod不处于Terminated状态或pod处于Terminated状态且evictTerminatedPods为false，则保留pod的最近一个sanbox容器
	if err := cgc.evictSandboxes(evictTerminatedPods); err != nil {
		errors = append(errors, err)
	}

	// Remove pod sandbox log directory
	// allSourcesReady为true，且pod（pod uid从目录名中提取）不为删除状态，则递归移除目录"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"
	// "/var/log/containers/*.log"文件，如果链接的目标文件不存在，则移除这个链接
	if err := cgc.evictPodLogsDirectories(allSourcesReady); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.NewAggregate(errors)
}
