/*
Copyright 2017 The Kubernetes Authors.

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

package cpumanager

import (
	"fmt"
	"math"
	"sync"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/containermap"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/status"
)

// ActivePodsFunc is a function that returns a list of pods to reconcile.
type ActivePodsFunc func() []*v1.Pod

type runtimeService interface {
	UpdateContainerResources(id string, resources *runtimeapi.LinuxContainerResources) error
}

type policyName string

// cpuManagerStateFileName is the file name where cpu manager stores its state
const cpuManagerStateFileName = "cpu_manager_state"

// Manager interface provides methods for Kubelet to manage pod cpus.
type Manager interface {
	// Start is called during Kubelet initialization.
	Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error

	// Called to trigger the allocation of CPUs to a container. This must be
	// called at some point prior to the AddContainer() call for a container,
	// e.g. at pod admission time.
	Allocate(pod *v1.Pod, container *v1.Container) error

	// AddContainer is called between container create and container start
	// so that initial CPU affinity settings can be written through to the
	// container runtime before the first process begins to execute.
	AddContainer(p *v1.Pod, c *v1.Container, containerID string) error

	// RemoveContainer is called after Kubelet decides to kill or delete a
	// container. After this call, the CPU manager stops trying to reconcile
	// that container and any CPUs dedicated to the container are freed.
	RemoveContainer(containerID string) error

	// State returns a read-only interface to the internal CPU manager state.
	State() state.Reader

	// GetTopologyHints implements the topologymanager.HintProvider Interface
	// and is consulted to achieve NUMA aware resource alignment among this
	// and other resource controllers.
	GetTopologyHints(*v1.Pod, *v1.Container) map[string][]topologymanager.TopologyHint
}

type manager struct {
	sync.Mutex
	policy Policy

	// reconcilePeriod is the duration between calls to reconcileState.
	reconcilePeriod time.Duration

	// state allows pluggable CPU assignment policies while sharing a common
	// representation of state for the system to inspect and reconcile.
	state state.State

	// containerRuntime is the container runtime service interface needed
	// to make UpdateContainerResources() calls against the containers.
	containerRuntime runtimeService

	// activePods is a method for listing active pods on the node
	// so all the containers can be updated in the reconciliation loop.
	activePods ActivePodsFunc

	// podStatusProvider provides a method for obtaining pod statuses
	// and the containerID of their containers
	podStatusProvider status.PodStatusProvider

	// containerMap provides a mapping from (pod, container) -> containerID
	// for all containers a pod
	// runtime里的所有容器，容器id与对应的pod uid和container name
	// 并且在初始化之后，会同步activepods返回的pod
	containerMap containermap.ContainerMap

	topology *topology.CPUTopology

	nodeAllocatableReservation v1.ResourceList

	// sourcesReady provides the readiness of kubelet configuration sources such as apiserver update readiness.
	// We use it to determine when we can purge inactive pods from checkpointed state.
	sourcesReady config.SourcesReady

	// stateFileDirectory holds the directory where the state file for checkpoints is held.
	stateFileDirectory string
}

var _ Manager = &manager{}

type sourcesReadyStub struct{}

func (s *sourcesReadyStub) AddSource(source string) {}
func (s *sourcesReadyStub) AllReady() bool          { return true }

// NewManager creates new cpu manager based on provided policy
func NewManager(cpuPolicyName string, reconcilePeriod time.Duration, machineInfo *cadvisorapi.MachineInfo, numaNodeInfo topology.NUMANodeInfo, specificCPUs cpuset.CPUSet, nodeAllocatableReservation v1.ResourceList, stateFileDirectory string, affinity topologymanager.Store) (Manager, error) {
	var topo *topology.CPUTopology
	var policy Policy

	switch policyName(cpuPolicyName) {

	case PolicyNone:
		policy = NewNonePolicy()

	case PolicyStatic:
		var err error
		// 获取cpu拓扑--多少线程数、物理cpu、插槽数、各个线程cpu信息
		topo, err = topology.Discover(machineInfo, numaNodeInfo)
		if err != nil {
			return nil, err
		}
		// 比如&{2 2 1 map[0:{0 0 0} 1:{0 0 1}]}
		klog.Infof("[cpumanager] detected CPU topology: %v", topo)
		reservedCPUs, ok := nodeAllocatableReservation[v1.ResourceCPU]
		if !ok {
			// The static policy cannot initialize without this information.
			return nil, fmt.Errorf("[cpumanager] unable to determine reserved CPU resources for static policy")
		}
		if reservedCPUs.IsZero() {
			// The static policy requires this to be nonzero. Zero CPU reservation
			// would allow the shared pool to be completely exhausted. At that point
			// either we would violate our guarantee of exclusivity or need to evict
			// any pod that has at least one container that requires zero CPUs.
			// See the comments in policy_static.go for more details.
			return nil, fmt.Errorf("[cpumanager] the static policy requires systemreserved.cpu + kubereserved.cpu to be greater than zero")
		}

		// Take the ceiling of the reservation, since fractional CPUs cannot be
		// exclusively allocated.
		// 保留的cpu向上取整
		reservedCPUsFloat := float64(reservedCPUs.MilliValue()) / 1000
		numReservedCPUs := int(math.Ceil(reservedCPUsFloat))
		policy, err = NewStaticPolicy(topo, numReservedCPUs, specificCPUs, affinity)
		if err != nil {
			return nil, fmt.Errorf("new static policy error: %v", err)
		}

	default:
		return nil, fmt.Errorf("unknown policy: \"%s\"", cpuPolicyName)
	}

	manager := &manager{
		policy:                     policy,
		reconcilePeriod:            reconcilePeriod,
		topology:                   topo,
		nodeAllocatableReservation: nodeAllocatableReservation,
		stateFileDirectory:         stateFileDirectory,
	}
	manager.sourcesReady = &sourcesReadyStub{}
	return manager, nil
}

// 进行容器的垃圾回收，不存在的容器分配的cpu回收，containerMap列表清理（让容器的cpu分配状态里容器与activePods一致）
// 创建checkpoint和stateMemory来保存当前的分配策略、分配状态
// static policy会校验当前分配状态是否合法
// 创建一个goroutine周期性（默认10s）周期同步m.state中容器的cpuset到容器的cgroup设置，会同步activepods返回的pod的容器集合，清理不存在的容器的cpu分配
func (m *manager) Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady, podStatusProvider status.PodStatusProvider, containerRuntime runtimeService, initialContainers containermap.ContainerMap) error {
	klog.Infof("[cpumanager] starting with %s policy", m.policy.Name())
	klog.Infof("[cpumanager] reconciling every %v", m.reconcilePeriod)
	// 通常是kl.sourcesReady
	m.sourcesReady = sourcesReady
	// 通常是kl.GetActivePods
	m.activePods = activePods
	// 通常是kl.statusManager
	m.podStatusProvider = podStatusProvider
	// kl.runtimeService
	m.containerRuntime = containerRuntime
	// 所有runtime里的container，返回ContainerMap（容器id与对应的容器name和pod的uid）
	m.containerMap = initialContainers

	// m.stateFileDirectory是kubelet root dir
	// 创建一个checkpoints和stateMemory（保存cpu分配策略、容器分配的cpu、默认的cpuset），checkpoints存在，则读取恢复状态，否则写入现在的状态
	stateImpl, err := state.NewCheckpointState(m.stateFileDirectory, cpuManagerStateFileName, m.policy.Name(), m.containerMap)
	if err != nil {
		klog.Errorf("[cpumanager] could not initialize checkpoint manager: %v, please drain node and remove policy state file", err)
		return err
	}
	m.state = stateImpl

	// static policy
	// 校验stateMemory中的总的cpu数量与物理cpu是否一致
	// 校验stateMemory中获得assignments（已经分配的cpu）与stateMemory中的DefaultCPUset（共享的cpu集合）是否有重叠
	// 校验保留的cpu是否在stateMemory中的DefaultCPUset（共享的cpu集合）里
	// 校验已经有cpu分配了，则stateMemory中的DefaultCPUset不能为空。stateMemory中的DefaultCPUset和stateMemory中的assignments都为空，则设置stateMemory中的DefaultCPUset为所有cpu
	err = m.policy.Start(m.state)
	if err != nil {
		klog.Errorf("[cpumanager] policy start error: %v", err)
		return err
	}

	if m.policy.Name() == string(PolicyNone) {
		return nil
	}
	// Periodically call m.reconcileState() to continue to keep the CPU sets of
	// all pods in sync with and guaranteed CPUs handed out among them.
	// reconcilePeriod默认为10s
	// 启动goroutine，周期同步m.state中容器的cpuset到容器的cgroup设置（调用cri更新容器的cpuset cgroup），移除不存在的container，让container列表（m.state.assignments和m.containerMap）与m.activePods()一致
	go wait.Until(func() { m.reconcileState() }, m.reconcilePeriod, wait.NeverStop)
	return nil
}

func (m *manager) Allocate(p *v1.Pod, c *v1.Container) error {
	// Garbage collect any stranded resources before allocating CPUs.
	// 从m.state.cache.assignments(已分配cpu的集合)中找到container，这个container不在activePods（pod manager中维护pod列表）中（container的pod不存在或pod下面没有这个container）
	// 从m.state.cache.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
	// 在m.containerMap（运行时中获得）中移除这个container
	m.removeStaleState()

	m.Lock()
	defer m.Unlock()

	// Call down into the policy to assign this container CPUs if required.
	// policy为static policy，为Guaranteed的pod且container request的cpu为整数的container，分配cpu
	err := m.policy.Allocate(m.state, p, c)
	if err != nil {
		klog.Errorf("[cpumanager] Allocate error: %v", err)
		return err
	}

	return nil
}

// 调用cri更新容器的cpuset cgroup，如果发生错误则移除分配的container（从m.state.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合），m.containerMap中移除这个container）
func (m *manager) AddContainer(p *v1.Pod, c *v1.Container, containerID string) error {
	m.Lock()
	// Get the CPUs assigned to the container during Allocate()
	// (or fall back to the default CPUSet if none were assigned).
	// 获得container的cpuset（先从assignments中获取容器的分配cpu，没有找到，则返回defaultCPUSet--共享cpu集合）
	cpus := m.state.GetCPUSetOrDefault(string(p.UID), c.Name)
	m.Unlock()

	if !cpus.IsEmpty() {
		// 调用cri更新容器的cpuset cgroup
		err := m.updateContainerCPUSet(containerID, cpus)
		if err != nil {
			klog.Errorf("[cpumanager] AddContainer error: error updating CPUSet for container (pod: %s, container: %s, container id: %s, err: %v)", p.Name, c.Name, containerID, err)
			m.Lock()
			// 从m.state.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
			// m.containerMap中移除这个container
			err := m.policyRemoveContainerByRef(string(p.UID), c.Name)
			if err != nil {
				klog.Errorf("[cpumanager] AddContainer rollback state error: %v", err)
			}
			m.Unlock()
		}
		return err
	}

	klog.V(5).Infof("[cpumanager] update container resources is skipped due to cpu set is empty")
	return nil
}

// 从m.state.cache.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
// m.containerMap中移除这个container
func (m *manager) RemoveContainer(containerID string) error {
	m.Lock()
	defer m.Unlock()

	// 从m.state.cache.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
	// m.containerMap中移除这个container
	err := m.policyRemoveContainerByID(containerID)
	if err != nil {
		klog.Errorf("[cpumanager] RemoveContainer error: %v", err)
		return err
	}

	return nil
}

// 从m.state.cache.assignments移除container所占有的cpu，并在stateMemory中defaultCPUSet添加这个cpu集合（共享的cpu集合）
// containerMap中移除这个container
func (m *manager) policyRemoveContainerByID(containerID string) error {
	// 从containerMap中获得这个容器的podUID、containerName
	podUID, containerName, err := m.containerMap.GetContainerRef(containerID)
	if err != nil {
		return nil
	}

	// 从m.state.cache.assignments移除container所占有的cpu，并在stateMemory中defaultCPUSet添加这个cpu集合（共享的cpu集合） 
	err = m.policy.RemoveContainer(m.state, podUID, containerName)
	if err == nil {
		m.containerMap.RemoveByContainerID(containerID)
	}

	return err
}

// 从m.state.cache.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
// m.containerMap中移除这个container
func (m *manager) policyRemoveContainerByRef(podUID string, containerName string) error {
	// 从m.state.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
	err := m.policy.RemoveContainer(m.state, podUID, containerName)
	if err == nil {
		// 根据podUID和containerName从ContainerMap取得container id
		// ContainerMap中移除这个container id
		m.containerMap.RemoveByContainerRef(podUID, containerName)
	}

	return err
}

func (m *manager) State() state.Reader {
	return m.state
}

func (m *manager) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded resources before providing TopologyHints
	// 从m.state.cache.assignments(已分配cpu的集合)中找到container，这个container为不在activePods（pod manager中维护pod列表）中（container的pod不存在或pod下面没有这个container）
	// 从m.state.cache.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
	// 在m.containerMap（运行时runtime中获得container）中移除这个container
	m.removeStaleState()
	// Delegate to active policy
	// policy为static，则为Guaranteed的pod且container request的cpu为整数的container，分配cpu
	return m.policy.GetTopologyHints(m.state, pod, container)
}

type reconciledContainer struct {
	podName       string
	containerName string
	containerID   string
}

// 从m.state.cache.assignments(已分配cpu的集合)中找到container，这个container为不在activePods（pod manager中维护pod列表）中（container的pod不存在或pod下面没有这个container）
// 从m.state.cache.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
// 在m.containerMap（运行时runtime中获得container）中移除这个container
func (m *manager) removeStaleState() {
	// Only once all sources are ready do we attempt to remove any stale state.
	// This ensures that the call to `m.activePods()` below will succeed with
	// the actual active pods list.
	// m.sourcesReady.seenSources源已经注册且都提供了至少一个pod
	// m.sourcesReady.seenSources在kl.syncloop中会添加
	if !m.sourcesReady.AllReady() {
		return
	}

	// We grab the lock to ensure that no new containers will grab CPUs while
	// executing the code below. Without this lock, its possible that we end up
	// removing state that is newly added by an asynchronous call to
	// AddContainer() during the execution of this code.
	m.Lock()
	defer m.Unlock()

	// Get the list of active pods.
	// 返回所有pods中（普通pod和static pod），pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除，或pod被删除且至少一个container为running
	activePods := m.activePods()

	// Build a list of (podUID, containerName) pairs for all containers in all active Pods.
	activeContainers := make(map[string]map[string]struct{})
	for _, pod := range activePods {
		activeContainers[string(pod.UID)] = make(map[string]struct{})
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			activeContainers[string(pod.UID)][container.Name] = struct{}{}
		}
	}

	// Loop through the CPUManager state. Remove any state for containers not
	// in the `activeContainers` list built above.
	// 移除不在activeContainers
	assignments := m.state.GetCPUAssignments()
	for podUID := range assignments {
		for containerName := range assignments[podUID] {
			if _, ok := activeContainers[podUID][containerName]; !ok {
				klog.Errorf("[cpumanager] removeStaleState: removing (pod %s, container: %s)", podUID, containerName)
				// 从m.state.assignments移除container所占有的cpu，并在m.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合）
				// m.containerMap中移除这个container
				err := m.policyRemoveContainerByRef(podUID, containerName)
				if err != nil {
					klog.Errorf("[cpumanager] removeStaleState: failed to remove (pod %s, container %s), error: %v)", podUID, containerName, err)
				}
			}
		}
	}
}

// 启动goroutine，周期同步m.state中容器的cpuset到容器的cgroup设置（调用cri更新容器的cpuset cgroup），移除不存在的container，让container列表（m.state.cache.assignments和m.containerMap）与m.activePods()一致
func (m *manager) reconcileState() (success []reconciledContainer, failure []reconciledContainer) {
	success = []reconciledContainer{}
	failure = []reconciledContainer{}

	// 移除不存在的container，让container列表与m.activePods()一致
	// m.state.cache.assignments移除container所分配的cpu，并把这个cpu添加到共享cpu集合（m.state（stateMemory）中defaultCPUSet）
	// m.containerMap中移除这个container
	m.removeStaleState()
	// 调用cri更新所有容器的cpuset，container在m.state.cache.assignments中cpuset就是分配的cpu，否则就是m.state.defaultCPUSet(共享cpu)
	for _, pod := range m.activePods() {
		pstatus, ok := m.podStatusProvider.GetPodStatus(pod.UID)
		if !ok {
			klog.Warningf("[cpumanager] reconcileState: skipping pod; status not found (pod: %s)", pod.Name)
			failure = append(failure, reconciledContainer{pod.Name, "", ""})
			continue
		}

		allContainers := pod.Spec.InitContainers
		allContainers = append(allContainers, pod.Spec.Containers...)
		for _, container := range allContainers {
			// 从pod status中找不到container name的容器id，则跳过这个container
			containerID, err := findContainerIDByName(&pstatus, container.Name)
			if err != nil {
				klog.Warningf("[cpumanager] reconcileState: skipping container; ID not found in pod status (pod: %s, container: %s, error: %v)", pod.Name, container.Name, err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, ""})
				continue
			}

			// 从pod status中找不到container name的container status，则跳过这个container
			cstatus, err := findContainerStatusByName(&pstatus, container.Name)
			if err != nil {
				klog.Warningf("[cpumanager] reconcileState: skipping container; container status not found in pod status (pod: %s, container: %s, error: %v)", pod.Name, container.Name, err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, ""})
				continue
			}

			// 容器状态为waiting或没有状态，则跳过这个container
			if cstatus.State.Waiting != nil ||
				(cstatus.State.Waiting == nil && cstatus.State.Running == nil && cstatus.State.Terminated == nil) {
				klog.Warningf("[cpumanager] reconcileState: skipping container; container still in the waiting state (pod: %s, container: %s, error: %v)", pod.Name, container.Name, err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, ""})
				continue
			}

			// 容器状态为Terminated，则跳过这个container
			if cstatus.State.Terminated != nil {
				// The container is terminated but we can't call m.RemoveContainer()
				// here because it could remove the allocated cpuset for the container
				// which may be in the process of being restarted.  That would result
				// in the container losing any exclusively-allocated CPUs that it
				// was allocated.
				// containerMap中是否存在这个容器
				_, _, err := m.containerMap.GetContainerRef(containerID)
				if err == nil {
					// containerMap中存在这个容器，则记录这个日志
					klog.Warningf("[cpumanager] reconcileState: ignoring terminated container (pod: %s, container id: %s)", pod.Name, containerID)
				}
				continue
			}

			// Once we make it here we know we have a running container.
			// Idempotently add it to the containerMap incase it is missing.
			// This can happen after a kubelet restart, for example.
			// 强制重新添加container到containerMap，为了防止机器重启了，runtime里没有这个容器
			m.containerMap.Add(string(pod.UID), container.Name, containerID)

			// 获得container的cpuset（先从assignments中获取容器的分配cpu，没有找到，则返回defaultCPUSet--共享cpu集合）
			cset := m.state.GetCPUSetOrDefault(string(pod.UID), container.Name)
			if cset.IsEmpty() {
				// NOTE: This should not happen outside of tests.
				klog.Infof("[cpumanager] reconcileState: skipping container; assigned cpuset is empty (pod: %s, container: %s)", pod.Name, container.Name)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, containerID})
				continue
			}

			klog.V(4).Infof("[cpumanager] reconcileState: updating container (pod: %s, container: %s, container id: %s, cpuset: \"%v\")", pod.Name, container.Name, containerID, cset)
			// 同步m.state中容器的cpuset到容器的cgroup设置（调用cri更新容器的cpuset cgroup）
			err = m.updateContainerCPUSet(containerID, cset)
			if err != nil {
				klog.Errorf("[cpumanager] reconcileState: failed to update container (pod: %s, container: %s, container id: %s, cpuset: \"%v\", error: %v)", pod.Name, container.Name, containerID, cset, err)
				failure = append(failure, reconciledContainer{pod.Name, container.Name, containerID})
				continue
			}
			success = append(success, reconciledContainer{pod.Name, container.Name, containerID})
		}
	}
	return success, failure
}

// 从pod status中找到container name的容器id
func findContainerIDByName(status *v1.PodStatus, name string) (string, error) {
	allStatuses := status.InitContainerStatuses
	allStatuses = append(allStatuses, status.ContainerStatuses...)
	for _, container := range allStatuses {
		if container.Name == name && container.ContainerID != "" {
			cid := &kubecontainer.ContainerID{}
			// 根据container.ContainerID分析runtime Type和containerid，比如docker://123ef5433，则runtime Type为docker，id为123ef5433
			err := cid.ParseString(container.ContainerID)
			if err != nil {
				return "", err
			}
			return cid.ID, nil
		}
	}
	return "", fmt.Errorf("unable to find ID for container with name %v in pod status (it may not be running)", name)
}

// 从pod status中找到container name的container status
func findContainerStatusByName(status *v1.PodStatus, name string) (*v1.ContainerStatus, error) {
	for _, status := range append(status.InitContainerStatuses, status.ContainerStatuses...) {
		if status.Name == name {
			return &status, nil
		}
	}
	return nil, fmt.Errorf("unable to find status for container with name %v in pod status (it may not be running)", name)
}

// 调用cri更新容器的cpuset cgroup
func (m *manager) updateContainerCPUSet(containerID string, cpus cpuset.CPUSet) error {
	// TODO: Consider adding a `ResourceConfigForContainer` helper in
	// helpers_linux.go similar to what exists for pods.
	// It would be better to pass the full container resources here instead of
	// this patch-like partial resources.
	return m.containerRuntime.UpdateContainerResources(
		containerID,
		&runtimeapi.LinuxContainerResources{
			CpusetCpus: cpus.String(),
		})
}
