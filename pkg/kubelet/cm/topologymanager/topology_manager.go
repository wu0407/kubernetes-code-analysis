/*
Copyright 2019 The Kubernetes Authors.

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

package topologymanager

import (
	"fmt"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/klog"
	cputopology "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
)

const (
	// maxAllowableNUMANodes specifies the maximum number of NUMA Nodes that
	// the TopologyManager supports on the underlying machine.
	//
	// At present, having more than this number of NUMA Nodes will result in a
	// state explosion when trying to enumerate possible NUMAAffinity masks and
	// generate hints for them. As such, if more NUMA Nodes than this are
	// present on a machine and the TopologyManager is enabled, an error will
	// be returned and the TopologyManager will not be loaded.
	maxAllowableNUMANodes = 8
)

//Manager interface provides methods for Kubelet to manage pod topology hints
type Manager interface {
	//Manager implements pod admit handler interface
	lifecycle.PodAdmitHandler
	//Adds a hint provider to manager to indicate the hint provider
	//wants to be consoluted when making topology hints
	AddHintProvider(HintProvider)
	//Adds pod to Manager for tracking
	AddContainer(pod *v1.Pod, containerID string) error
	//Removes pod from Manager tracking
	RemoveContainer(containerID string) error
	//Interface for storing pod topology hints
	Store
}

type manager struct {
	mutex sync.Mutex
	//The list of components registered with the Manager
	hintProviders []HintProvider
	//Mapping of a Pods mapping of Containers and their TopologyHints
	//Indexed by PodUID to ContainerName
	podTopologyHints map[string]map[string]TopologyHint
	//Mapping of PodUID to ContainerID for Adding/Removing Pods from PodTopologyHints mapping
	// container id 对应的pod uid
	podMap map[string]string
	//Topology Manager Policy
	policy Policy
}

// HintProvider is an interface for components that want to collaborate to
// achieve globally optimal concrete resource alignment with respect to
// NUMA locality.
type HintProvider interface {
	// GetTopologyHints returns a map of resource names to a list of possible
	// concrete resource allocations in terms of NUMA locality hints. Each hint
	// is optionally marked "preferred" and indicates the set of NUMA nodes
	// involved in the hypothetical allocation. The topology manager calls
	// this function for each hint provider, and merges the hints to produce
	// a consensus "best" hint. The hint providers may subsequently query the
	// topology manager to influence actual resource assignment.
	GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]TopologyHint
	// Allocate triggers resource allocation to occur on the HintProvider after
	// all hints have been gathered and the aggregated Hint is available via a
	// call to Store.GetAffinity().
	Allocate(pod *v1.Pod, container *v1.Container) error
}

//Store interface is to allow Hint Providers to retrieve pod affinity
type Store interface {
	GetAffinity(podUID string, containerName string) TopologyHint
}

//TopologyHint is a struct containing the NUMANodeAffinity for a Container
type TopologyHint struct {
	NUMANodeAffinity bitmask.BitMask
	// Preferred is set to true when the NUMANodeAffinity encodes a preferred
	// allocation for the Container. It is set to false otherwise.
	Preferred bool
}

// IsEqual checks if TopologyHint are equal
func (th *TopologyHint) IsEqual(topologyHint TopologyHint) bool {
	if th.Preferred == topologyHint.Preferred {
		if th.NUMANodeAffinity == nil || topologyHint.NUMANodeAffinity == nil {
			return th.NUMANodeAffinity == topologyHint.NUMANodeAffinity
		}
		return th.NUMANodeAffinity.IsEqual(topologyHint.NUMANodeAffinity)
	}
	return false
}

// LessThan checks if TopologyHint `a` is less than TopologyHint `b`
// this means that either `a` is a preferred hint and `b` is not
// or `a` NUMANodeAffinity attribute is narrower than `b` NUMANodeAffinity attribute.
func (th *TopologyHint) LessThan(other TopologyHint) bool {
	if th.Preferred != other.Preferred {
		return th.Preferred == true
	}
	return th.NUMANodeAffinity.IsNarrowerThan(other.NUMANodeAffinity)
}

var _ Manager = &manager{}

//NewManager creates a new TopologyManager based on provided policy
func NewManager(numaNodeInfo cputopology.NUMANodeInfo, topologyPolicyName string) (Manager, error) {
	klog.Infof("[topologymanager] Creating topology manager with %s policy", topologyPolicyName)

	var numaNodes []int
	for node := range numaNodeInfo {
		numaNodes = append(numaNodes, node)
	}

	if topologyPolicyName != PolicyNone && len(numaNodes) > maxAllowableNUMANodes {
		return nil, fmt.Errorf("unsupported on machines with more than %v NUMA Nodes", maxAllowableNUMANodes)
	}

	var policy Policy
	switch topologyPolicyName {

	case PolicyNone:
		policy = NewNonePolicy()

	case PolicyBestEffort:
		policy = NewBestEffortPolicy(numaNodes)

	case PolicyRestricted:
		policy = NewRestrictedPolicy(numaNodes)

	case PolicySingleNumaNode:
		policy = NewSingleNumaNodePolicy(numaNodes)

	default:
		return nil, fmt.Errorf("unknown policy: \"%s\"", topologyPolicyName)
	}

	var hp []HintProvider
	pth := make(map[string]map[string]TopologyHint)
	pm := make(map[string]string)
	manager := &manager{
		hintProviders:    hp,
		podTopologyHints: pth,
		podMap:           pm,
		policy:           policy,
	}

	return manager, nil
}

func (m *manager) GetAffinity(podUID string, containerName string) TopologyHint {
	return m.podTopologyHints[podUID][containerName]
}

// 遍历所有的hintProviders生成container的TopologyHint
// device manager：为container需要的device plugin资源生成可以分配device的TopologyHint
// cpuManager：如果为static policy，则为container分配需要的cpu，生成TopologyHint。如果为none policy，则不做任何操作
func (m *manager) accumulateProvidersHints(pod *v1.Pod, container *v1.Container) (providersHints []map[string][]TopologyHint) {
	// Loop through all hint providers and save an accumulated list of the
	// hints returned by each hint provider.
	for _, provider := range m.hintProviders {
		// Get the TopologyHints from a provider.
		// device manager在pkg\kubelet\cm\devicemanager\topology_hints.go
		// 为container需要的device plugin资源生成可以分配device的TopologyHint
		//
		// cpuManager
		// 如果为static policy，则为Guaranteed的pod且container request的cpu为整数的container分配需要的cpu，生成TopologyHint
		// 如果为none policy，则不做任何操作
		hints := provider.GetTopologyHints(pod, container)
		providersHints = append(providersHints, hints)
		klog.Infof("[topologymanager] TopologyHints for pod '%v', container '%v': %v", pod.Name, container.Name, hints)
	}
	return providersHints
}

// 从注册的device plugins，分配container的limit中所有需要的资源
// cpu manager，policy为static policy，为Guaranteed的pod且container request的cpu为整数的container，分配cpu
func (m *manager) allocateAlignedResources(pod *v1.Pod, container *v1.Container) error {
	for _, provider := range m.hintProviders {
		// 从注册的device plugins，分配container的limit中所有需要的资源
		// policy为static policy，为Guaranteed的pod且container request的cpu为整数的container，分配cpu
		err := provider.Allocate(pod, container)
		if err != nil {
			return err
		}
	}
	return nil
}

// Collect Hints from hint providers and pass to policy to retrieve the best one.
func (m *manager) calculateAffinity(pod *v1.Pod, container *v1.Container) (TopologyHint, bool) {
	// 遍历所有的hintProviders生成container的TopologyHint
	// device manager：为container需要的device plugin资源生成可以分配device的TopologyHint
	// cpuManager：如果为static policy，则为container分配需要的cpu，生成TopologyHint。如果为none policy，则不做任何操作
	providersHints := m.accumulateProvidersHints(pod, container)
	// 如果为none policy，则返回空的TopologyHint和true
	// 
	// 如果为"best-effort" policy
	// 遍历providersHints（所有provider的TopologyHint），输出filteredProvidersHints（类型为[][]TopologyHint）
	//   如果provider没有TopologyHint，则默认为[]TopologyHint{{nil, true}}
	//   如果资源没有TopologyHint，则这个资源的TopologyHint，默认为[]TopologyHint{{nil, true}}
	//   如果资源有TopologyHint且为空（不能跟NUMA affinities），则为[]TopologyHint{{nil, false}}
	// 从filteredProvidersHints二维[]TopologyHint中，每个[]TopologyHint取一个TopologyHint，进行组合，生成新的[]TopologyHint，然后调用callback
	// callback是，每一组[]TopologyHint，进行（跟defaultAffinity（亲和所有numaNode）跟所有的TopologyHint进行与计算）聚合，在所有聚合的结果，选出最适合的TopologyHint。
	// 筛选规则：
	//   如果TopologyHint的位的值为1的数量相等，则取TopologyHint的NUMANodeAffinity值较小的TopologyHint。
	//   如果TopologyHint的NUMANodeAffinity位的值为1的数量不相等，则TopologyHint的NUMANodeAffinity位的值为1的数量最小的TopologyHint
	//   优先选择，聚合的结果TopologyHint的Preferred是true
	// admit一直返回true
	// 
	// 如果为"restricted" policy
	// 筛选规则与"best-effort"一样，但是admit为筛选的结果TopologyHint的Preferred的值
	// 
	// 如果为"single-numa-node" policy
	// 遍历providersHints（所有provider的TopologyHint），输出filteredHints（类型为[][]TopologyHint）
	//   如果provider没有TopologyHint，则默认为[]TopologyHint{{nil, true}}
	//   如果资源没有TopologyHint，则这个资源的TopologyHint，默认为[]TopologyHint{{nil, true}}
	//   如果资源有TopologyHint且为空（不能跟NUMA affinities），则为[]TopologyHint{{nil, false}}
	// filteredHints里每个[]TopologyHint里的TopologyHint进行过滤，输出过滤后的singleNumaHints(类型[][]TopologyHint)。
	// 过滤规则：
	//   保留TopologyHint，NUMANodeAffinity为nil，且Preferred为true。即资源没有TopologyHint
	//   保留TopologyHint，NUMANodeAffinity不为nil，且NUMANodeAffinity位的值为1的数量为1（只亲和一个numa），且Preferred为true
	//   不保留其他TopologyHint
	// 从singleNumaHints二维[]TopologyHint中，每个[]TopologyHint取一个TopologyHint，进行组合，生成新的[]TopologyHint，然后调用callback
	// callback是，每一组[]TopologyHint，进行（跟defaultAffinity（亲和所有numaNode）跟所有的TopologyHint进行与计算）聚合，在所有聚合的结果，选出最适合的TopologyHint。
	// 筛选规则：
	//   如果TopologyHint的位的值为1的数量相等，则取TopologyHint的NUMANodeAffinity值较小的TopologyHint。
	//   如果TopologyHint的NUMANodeAffinity位的值为1的数量不相等，则TopologyHint的NUMANodeAffinity位的值为1的数量最小的TopologyHint
	//   优先选择，聚合的结果TopologyHint的Preferred是true
	// 如果筛选结果bestHint的NUMANodeAffinity（最佳的NUMANodeAffinity）为默认的defaultAffinity（亲和所有numaNode），则筛选结果bestHint的NUMANodeAffinity为nil
	// admit为聚合的结果TopologyHint的Preferred的值
	// 简单概括，就是比"restricted"，多加一道筛选（保留没有亲和和只亲和一个numaNode）和结果NUMANodeAffinity为亲和所有numaNode，则重置NUMANodeAffinity为nil
	bestHint, admit := m.policy.Merge(providersHints)
	klog.Infof("[topologymanager] ContainerTopologyHint: %v", bestHint)
	return bestHint, admit
}

func (m *manager) AddHintProvider(h HintProvider) {
	// 目前hintprovider有cm.deviceManager、cm.cpuManager
	m.hintProviders = append(m.hintProviders, h)
}

// 添加container id到m.podMap
func (m *manager) AddContainer(pod *v1.Pod, containerID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.podMap[containerID] = string(pod.UID)
	return nil
}

// 清理container ID在m.podTopologyHints分配资源
func (m *manager) RemoveContainer(containerID string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	klog.Infof("[topologymanager] RemoveContainer - Container ID: %v", containerID)
	podUIDString := m.podMap[containerID]
	// 从m.podMap中移除
	delete(m.podMap, containerID)
	// 从m.podTopologyHints[podUIDString]中移除containerID，如果m.podTopologyHints[podUIDString]长度为0，则将podUIDString从m.podTopologyHints中移除
	if _, exists := m.podTopologyHints[podUIDString]; exists {
		delete(m.podTopologyHints[podUIDString], containerID)
		if len(m.podTopologyHints[podUIDString]) == 0 {
			delete(m.podTopologyHints, podUIDString)
		}
	}

	return nil
}

// policy为"none"，行为跟pkg\kubelet\cm\container_manager_linux.go里的resourceAllocator一样
//   成功为pod里所有普通container和init container，分配container的limit中所有需要device plugins的资源，且成功分配cpu（如果policy为static policy，成功为Guaranteed的pod且container request的cpu为整数的container，分配cpu），则admit通过
// policy为"best-effort"、"restricted"、"single-numa-node"
// 遍历所有的hintProviders（cpu manager和device manager）生成container的各个资源的TopologyHint列表
// 从各个资源的TopologyHint列表中挑出一个TopologyHint，进行组合成[]TopologyHint，然后跟default Affinity（亲和所有numaNode）进行与计算。
// 在所有组合中，根据各个policy类型，挑选出最合适的TopologyHint。
// admit结果：
// policy为"best-effort"，则admit通过
// policy为"restricted"、"single-numa-node"，则为最合适TopologyHint的Preferred的值
func (m *manager) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	klog.Infof("[topologymanager] Topology Admit Handler")
	pod := attrs.Pod

	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		// policy为"none"，行为跟pkg\kubelet\cm\container_manager_linux.go里的resourceAllocator一样
		if m.policy.Name() == PolicyNone {
			// 从注册的device plugins，分配container的limit中所有需要的资源
			// policy为static policy，为Guaranteed的pod且container request的cpu为整数的container，分配cpu
			err := m.allocateAlignedResources(pod, &container)
			if err != nil {
				return lifecycle.PodAdmitResult{
					Message: fmt.Sprintf("Allocate failed due to %v, which is unexpected", err),
					Reason:  "UnexpectedAdmissionError",
					Admit:   false,
				}
			}
			continue
		}

		// 遍历所有的hintProviders（cpu manager和device manager）生成container的各个资源的TopologyHint列表
		// 根据policy类型，筛选出最佳的TopologyHint
		result, admit := m.calculateAffinity(pod, &container)
		if !admit {
			return lifecycle.PodAdmitResult{
				Message: "Resources cannot be allocated with Topology locality",
				Reason:  "TopologyAffinityError",
				Admit:   false,
			}
		}

		klog.Infof("[topologymanager] Topology Affinity for (pod: %v container: %v): %v", pod.UID, container.Name, result)
		if m.podTopologyHints[string(pod.UID)] == nil {
			m.podTopologyHints[string(pod.UID)] = make(map[string]TopologyHint)
		}
		// 将container的TopologyHint添加到m.podTopologyHints
		m.podTopologyHints[string(pod.UID)][container.Name] = result

		// 从注册的device plugins，分配container的limit中所有需要的资源
		// cpu manager，policy为static policy，为Guaranteed的pod且container request的cpu为整数的container，分配cpu
		err := m.allocateAlignedResources(pod, &container)
		if err != nil {
			return lifecycle.PodAdmitResult{
				Message: fmt.Sprintf("Allocate failed due to %v, which is unexpected", err),
				Reason:  "UnexpectedAdmissionError",
				Admit:   false,
			}
		}
	}

	return lifecycle.PodAdmitResult{Admit: true}
}
