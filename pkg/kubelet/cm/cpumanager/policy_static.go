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

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/state"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpuset"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
)

// PolicyStatic is the name of the static policy
const PolicyStatic policyName = "static"

// staticPolicy is a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
//
// This policy allocates CPUs exclusively for a container if all the following
// conditions are met:
//
// - The pod QoS class is Guaranteed.
// - The CPU request is a positive integer.
//
// The static policy maintains the following sets of logical CPUs:
//
// - SHARED: Burstable, BestEffort, and non-integral Guaranteed containers
//   run here. Initially this contains all CPU IDs on the system. As
//   exclusive allocations are created and destroyed, this CPU set shrinks
//   and grows, accordingly. This is stored in the state as the default
//   CPU set.
//
// - RESERVED: A subset of the shared pool which is not exclusively
//   allocatable. The membership of this pool is static for the lifetime of
//   the Kubelet. The size of the reserved pool is
//   ceil(systemreserved.cpu + kubereserved.cpu).
//   Reserved CPUs are taken topologically starting with lowest-indexed
//   physical core, as reported by cAdvisor.
//
// - ASSIGNABLE: Equal to SHARED - RESERVED. Exclusive CPUs are allocated
//   from this pool.
//
// - EXCLUSIVE ALLOCATIONS: CPU sets assigned exclusively to one container.
//   These are stored as explicit assignments in the state.
//
// When an exclusive allocation is made, the static policy also updates the
// default cpuset in the state abstraction. The CPU manager's periodic
// reconcile loop takes care of rewriting the cpuset in cgroupfs for any
// containers that may be running in the shared pool. For this reason,
// applications running within exclusively-allocated containers must tolerate
// potentially sharing their allocated CPUs for up to the CPU manager
// reconcile period.
type staticPolicy struct {
	// cpu socket topology
	topology *topology.CPUTopology
	// set of CPUs that is not available for exclusive assignment
	reserved cpuset.CPUSet
	// topology manager reference to get container Topology affinity
	affinity topologymanager.Store
	// set of CPUs to reuse across allocations in a pod
	cpusToReuse map[string]cpuset.CPUSet
}

// Ensure staticPolicy implements Policy interface
var _ Policy = &staticPolicy{}

// NewStaticPolicy returns a CPU manager policy that does not change CPU
// assignments for exclusively pinned guaranteed containers after the main
// container process starts.
func NewStaticPolicy(topology *topology.CPUTopology, numReservedCPUs int, reservedCPUs cpuset.CPUSet, affinity topologymanager.Store) (Policy, error) {
	allCPUs := topology.CPUDetails.CPUs()
	var reserved cpuset.CPUSet
	// 如果已经指定保留cpu的列表，通过命令行--reserved-cpus指定
	if reservedCPUs.Size() > 0 {
		reserved = reservedCPUs
	} else {
		// takeByTopology allocates CPUs associated with low-numbered cores from
		// allCPUs.
		//
		// For example: Given a system with 8 CPUs available and HT enabled,
		// if numReservedCPUs=2, then reserved={0,4}
		// 
		// 4 CPUs available and HT enabled
		// topology=&{NumCPUs:4 NumCores:2 NumSockets:1 CPUDetails:map[0:{NUMANodeID:0 SocketID:0 CoreID:0} 1:{NUMANodeID:0 SocketID:0 CoreID:1} 2:{NUMANodeID:0 SocketID:0 CoreID:0} 3:{NUMANodeID:0 SocketID:0 CoreID:1}]}
		// topo感知分配
		// 优先id从小到大
		// 优先同一socket
		// 优先同一物理核心
		// 剩下cpu 亲和已分配的socket和物理核心
		reserved, _ = takeByTopology(topology, allCPUs, numReservedCPUs)
	}

	if reserved.Size() != numReservedCPUs {
		err := fmt.Errorf("[cpumanager] unable to reserve the required amount of CPUs (size of %s did not equal %d)", reserved, numReservedCPUs)
		return nil, err
	}

	klog.Infof("[cpumanager] reserved %d CPUs (\"%s\") not available for exclusive assignment", reserved.Size(), reserved)

	return &staticPolicy{
		topology:    topology,
		reserved:    reserved,
		affinity:    affinity,
		cpusToReuse: make(map[string]cpuset.CPUSet),
	}, nil
}

func (p *staticPolicy) Name() string {
	return string(PolicyStatic)
}

func (p *staticPolicy) Start(s state.State) error {
	if err := p.validateState(s); err != nil {
		klog.Errorf("[cpumanager] static policy invalid state: %v, please drain node and remove policy state file", err)
		return err
	}
	return nil
}

// 校验stateMemory中的总的cpu数量与物理cpu是否一致
// 校验stateMemory中获得assignments（已经分配的cpu）与stateMemory中的DefaultCPUset（共享的cpu集合）是否有重叠
// 校验保留的cpu是否在stateMemory中的DefaultCPUset（共享的cpu集合）里
// 校验已经有cpu分配了，则stateMemory中的DefaultCPUset不能为空。stateMemory中的DefaultCPUset和stateMemory中的assignments都为空，则设置checkpoint和stateMemory中的DefaultCPUset为所有cpu
func (p *staticPolicy) validateState(s state.State) error {
	// 从stateMemory中获得assignments--已经分配的cpu集合
	tmpAssignments := s.GetCPUAssignments()
	// 从stateMemory中获得defaultCPUSet--所有非整数cpu的guaranteed类型pod，共享的cpu集合
	tmpDefaultCPUset := s.GetDefaultCPUSet()

	// Default cpuset cannot be empty when assignments exist
	// 已经有cpu分配了，则tmpDefaultCPUset不能为空
	// 如果没有cpu分配且tmpDefaultCPUset为空，则设置checkpoint和stateMemory中defaultCPUSet为所有cpu（线程或逻辑）。这个一般发生在kubelet启动时候且kubelet的root目录下cpu_manager_state文件不存在
	if tmpDefaultCPUset.IsEmpty() {
		if len(tmpAssignments) != 0 {
			return fmt.Errorf("default cpuset cannot be empty")
		}
		// state is empty initialize
		allCPUs := p.topology.CPUDetails.CPUs()
		s.SetDefaultCPUSet(allCPUs)
		return nil
	}

	// State has already been initialized from file (is not empty)
	// 1. Check if the reserved cpuset is not part of default cpuset because:
	// - kube/system reserved have changed (increased) - may lead to some containers not being able to start
	// - user tampered with file
	// 当p.reserved（保留的cpu）不是tmpDefaultCPUset（共享的cpu集合）的子集，直接返回错误。这个发生在修改了保留cpu的配置（有cpu不在defaultCPUSet中--已经分配出去，由于cpu分配出去，DefaultCPUset就会改变），然后重启了kubelet
	if !p.reserved.Intersection(tmpDefaultCPUset).Equals(p.reserved) {
		return fmt.Errorf("not all reserved cpus: \"%s\" are present in defaultCpuSet: \"%s\"",
			p.reserved.String(), tmpDefaultCPUset.String())
	}

	// 2. Check if state for static policy is consistent
	// 已经分配的cpu不能在共享cpu集合中
	for pod := range tmpAssignments {
		for container, cset := range tmpAssignments[pod] {
			// None of the cpu in DEFAULT cset should be in s.assignments
			// 已经分配的cpu在tmpDefaultCPUset里（所有非整数cpu的guaranteed类型pod，共享的cpu集合）
			if !tmpDefaultCPUset.Intersection(cset).IsEmpty() {
				return fmt.Errorf("pod: %s, container: %s cpuset: \"%s\" overlaps with default cpuset \"%s\"",
					pod, container, cset.String(), tmpDefaultCPUset.String())
			}
		}
	}

	// 3. It's possible that the set of available CPUs has changed since
	// the state was written. This can be due to for example
	// offlining a CPU when kubelet is not running. If this happens,
	// CPU manager will run into trouble when later it tries to
	// assign non-existent CPUs to containers. Validate that the
	// topology that was received during CPU manager startup matches with
	// the set of CPUs stored in the state.
	// kubelet停止运行，cpu硬件发生变化（比如更换cpu），然后再启动kubelet
	// 检查所有的cpu(tmpAssignments和tmpDefaultCPUset)是否与当前的硬件的cpu一样
	totalKnownCPUs := tmpDefaultCPUset.Clone()
	tmpCPUSets := []cpuset.CPUSet{}
	for pod := range tmpAssignments {
		for _, cset := range tmpAssignments[pod] {
			tmpCPUSets = append(tmpCPUSets, cset)
		}
	}
	totalKnownCPUs = totalKnownCPUs.UnionAll(tmpCPUSets)
	if !totalKnownCPUs.Equals(p.topology.CPUDetails.CPUs()) {
		return fmt.Errorf("current set of available CPUs \"%s\" doesn't match with CPUs in state \"%s\"",
			p.topology.CPUDetails.CPUs().String(), totalKnownCPUs.String())
	}

	return nil
}

// assignableCPUs returns the set of unassigned CPUs minus the reserved set.
// s.DefaultCPUSet(未分配或共享的）的cpu减去p.reserved（保留的cpu）
func (p *staticPolicy) assignableCPUs(s state.State) cpuset.CPUSet {
	return s.GetDefaultCPUSet().Difference(p.reserved)
}

// 更新cpu重用cpusToReuse，只保留当前的pod uid的重用cpuset， 如果是init container当前的pod uid的重用cpuset添加cpuset，如果是非init container则当前的pod uid的重用cpuset移除这个cpuset
// 只有init container分配的cpu才能被同一pod的所有container重用
func (p *staticPolicy) updateCPUsToReuse(pod *v1.Pod, container *v1.Container, cset cpuset.CPUSet) {
	// If pod entries to m.cpusToReuse other than the current pod exist, delete them.
	// 删除cpusToReuse中，非当前pod uid的cpuset
	// 清空p.cpusToReuse，只保留当前的pod uid
	for podUID := range p.cpusToReuse {
		if podUID != string(pod.UID) {
			delete(p.cpusToReuse, podUID)
		}
	}
	// If no cpuset exists for cpusToReuse by this pod yet, create one.
	if _, ok := p.cpusToReuse[string(pod.UID)]; !ok {
		p.cpusToReuse[string(pod.UID)] = cpuset.NewCPUSet()
	}
	// Check if the container is an init container.
	// If so, add its cpuset to the cpuset of reusable CPUs for any new allocations.
	// 如果是init container，则它分配的cpu是可以重用的，添加到cpusToReuse，然后返回
	for _, initContainer := range pod.Spec.InitContainers {
		if container.Name == initContainer.Name {
			p.cpusToReuse[string(pod.UID)] = p.cpusToReuse[string(pod.UID)].Union(cset)
			return
		}
	}
	// Otherwise it is an app container.
	// Remove its cpuset from the cpuset of reusable CPUs for any new allocations.
	// 如果是非init container，则它的分配的cpu从cpusToReuse中移除
	p.cpusToReuse[string(pod.UID)] = p.cpusToReuse[string(pod.UID)].Difference(cset)
}

func (p *staticPolicy) Allocate(s state.State, pod *v1.Pod, container *v1.Container) error {
	// 判断pod qos是不是Guaranteed，返回container的cpu的resuest
	if numCPUs := p.guaranteedCPUs(pod, container); numCPUs != 0 {
		klog.Infof("[cpumanager] static policy: Allocate (pod: %s, container: %s)", pod.Name, container.Name)
		// container belongs in an exclusively allocated pool

		// 判断container是否cpu已分配（container是否在s.cache.assignments有记录）
		if cpuset, ok := s.GetCPUSet(string(pod.UID), container.Name); ok {
			// 更新cpu重用cpusToReuse，只保留当前的pod uid的重用cpuset，init container则cpusToReuse增加这些cpuset，否则移除这些cpuset
			p.updateCPUsToReuse(pod, container, cpuset)
			klog.Infof("[cpumanager] static policy: container already present in state, skipping (pod: %s, container: %s)", pod.Name, container.Name)
			return nil
		}

		// Call Topology Manager to get the aligned socket affinity across all hint providers.
		// 暂时不懂Topology Manager怎么运转的，待修改
		hint := p.affinity.GetAffinity(string(pod.UID), container.Name)
		klog.Infof("[cpumanager] Pod %v, Container %v Topology Affinity is: %v", pod.UID, container.Name, hint)

		// Allocate CPUs according to the NUMA affinity contained in the hint.
		// 根据numaAffinity进行分配cpu，并更新checkpoint和stateMemory的defaultCPUSet（共享cpu）--移除已经分配的cpu
		cpuset, err := p.allocateCPUs(s, numCPUs, hint.NUMANodeAffinity, p.cpusToReuse[string(pod.UID)])
		if err != nil {
			klog.Errorf("[cpumanager] unable to allocate %d CPUs (pod: %s, container: %s, error: %v)", numCPUs, pod.Name, container.Name, err)
			return err
		}
		// 更新checkpoint和stateMemory的assignments(已分配cpu)
		s.SetCPUSet(string(pod.UID), container.Name, cpuset)
		// 更新cpu重用cpusToReuse，只保留当前的pod uid的重用cpuset，init container则cpusToReuse增加这些cpuset，否则移除这些cpuset
		p.updateCPUsToReuse(pod, container, cpuset)

	}
	// container belongs in the shared pool (nothing to do; use default cpuset)
	return nil
}

// 从s.cache.assignments移除container所占有的cpu，并在checkpoint和stateMemory中defaultCPUSet添加这个cpu集合（共享的cpu集合）
func (p *staticPolicy) RemoveContainer(s state.State, podUID string, containerName string) error {
	klog.Infof("[cpumanager] static policy: RemoveContainer (pod: %s, container: %s)", podUID, containerName)
	// 获得container的cpu分配
	if toRelease, ok := s.GetCPUSet(podUID, containerName); ok {
		// 从s.cache.assignments（已分配cpu集合）中移除podUID下的这个container，如果podUID下的分配cpu集合为空，删除这个podUID
		s.Delete(podUID, containerName)
		// Mutate the shared pool, adding released cpus.
		// defaultCPUSet添加这个cpu集合
		s.SetDefaultCPUSet(s.GetDefaultCPUSet().Union(toRelease))
	}
	return nil
}

// 根据numaAffinity进行分配cpu，并更新checkpoint和stateMemory的defaultCPUSet（共享cpu）--移除已经分配的cpu
func (p *staticPolicy) allocateCPUs(s state.State, numCPUs int, numaAffinity bitmask.BitMask, reusableCPUs cpuset.CPUSet) (cpuset.CPUSet, error) {
	klog.Infof("[cpumanager] allocateCpus: (numCPUs: %d, socket: %v)", numCPUs, numaAffinity)

	// 可分配cpu = s.DefaultCPUSet(未分配或共享的）的cpu减去p.reserved（保留的cpu）
	// 可以分配的cpu集合为 可分配cpu加上可重用p.cpusToReuse[pod uid]（reusableCPUs）
	assignableCPUs := p.assignableCPUs(s).Union(reusableCPUs)

	// If there are aligned CPUs in numaAffinity, attempt to take those first.
	// 如果有numaAffinity，则从assignableCPUs且在numaAffinity的cpu集合中分配cpu
	result := cpuset.NewCPUSet()
	if numaAffinity != nil {
		alignedCPUs := cpuset.NewCPUSet()
		for _, numaNodeID := range numaAffinity.GetBits() {
			alignedCPUs = alignedCPUs.Union(assignableCPUs.Intersection(p.topology.CPUDetails.CPUsInNUMANodes(numaNodeID)))
		}

		// 在numa节点里的可分配cpu（alignedCPUs）大于需要的cpu（numCPUs），则以需要的numCPUs为准
		// 在numa节点里的可分配cpu（alignedCPUs）小于需要的cpu（numCPUs），则以需要的alignedCPUs为准
		numAlignedToAlloc := alignedCPUs.Size()
		if numCPUs < numAlignedToAlloc {
			numAlignedToAlloc = numCPUs
		}

		// 分配cpu
		alignedCPUs, err := takeByTopology(p.topology, alignedCPUs, numAlignedToAlloc)
		if err != nil {
			return cpuset.NewCPUSet(), err
		}

		// alignedCPUs转成result，感觉这里多余了，直接赋值就行
		result = result.Union(alignedCPUs)
	}

	// Get any remaining CPUs from what's leftover after attempting to grab aligned ones.
	// 分配剩余需要的cpu（numaaffinity分配的cpu数量不满足需要cpu数量），从可分配的cpu（assignableCPUs）中排除已经分配的cpu，进行分配
	remainingCPUs, err := takeByTopology(p.topology, assignableCPUs.Difference(result), numCPUs-result.Size())
	if err != nil {
		return cpuset.NewCPUSet(), err
	}
	result = result.Union(remainingCPUs)

	// Remove allocated CPUs from the shared CPUSet.
	// checkpoint和stateMemory更新defaultCPUSet（共享cpu）--移除已经分配的cpu
	s.SetDefaultCPUSet(s.GetDefaultCPUSet().Difference(result))

	klog.Infof("[cpumanager] allocateCPUs: returning \"%v\"", result)
	return result, nil
}

func (p *staticPolicy) guaranteedCPUs(pod *v1.Pod, container *v1.Container) int {
	// 判断pod qos是不是Guaranteed
	if v1qos.GetPodQOS(pod) != v1.PodQOSGuaranteed {
		return 0
	}
	cpuQuantity := container.Resources.Requests[v1.ResourceCPU]
	// 必须是整数不能有浮点数，有浮点数cpuQuantity.MilliValue()会进行向上取整
	if cpuQuantity.Value()*1000 != cpuQuantity.MilliValue() {
		return 0
	}
	// Safe downcast to do for all systems with < 2.1 billion CPUs.
	// Per the language spec, `int` is guaranteed to be at least 32 bits wide.
	// https://golang.org/ref/spec#Numeric_types
	return int(cpuQuantity.Value())
}

func (p *staticPolicy) GetTopologyHints(s state.State, pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	// If there are no CPU resources requested for this container, we do not
	// generate any topology hints.
	if _, ok := container.Resources.Requests[v1.ResourceCPU]; !ok {
		return nil
	}

	// Get a count of how many guaranteed CPUs have been requested.
	requested := p.guaranteedCPUs(pod, container)

	// If there are no guaranteed CPUs being requested, we do not generate
	// any topology hints. This can happen, for example, because init
	// containers don't have to have guaranteed CPUs in order for the pod
	// to still be in the Guaranteed QOS tier.
	if requested == 0 {
		return nil
	}

	// Short circuit to regenerate the same hints if there are already
	// guaranteed CPUs allocated to the Container. This might happen after a
	// kubelet restart, for example.
	if allocated, exists := s.GetCPUSet(string(pod.UID), container.Name); exists {
		if allocated.Size() != requested {
			klog.Errorf("[cpumanager] CPUs already allocated to (pod %v, container %v) with different number than request: requested: %d, allocated: %d", string(pod.UID), container.Name, requested, allocated.Size())
			return map[string][]topologymanager.TopologyHint{
				string(v1.ResourceCPU): {},
			}
		}
		klog.Infof("[cpumanager] Regenerating TopologyHints for CPUs already allocated to (pod %v, container %v)", string(pod.UID), container.Name)
		return map[string][]topologymanager.TopologyHint{
			string(v1.ResourceCPU): p.generateCPUTopologyHints(allocated, requested),
		}
	}

	// Get a list of available CPUs.
	available := p.assignableCPUs(s)

	// Generate hints.
	cpuHints := p.generateCPUTopologyHints(available, requested)
	klog.Infof("[cpumanager] TopologyHints generated for pod '%v', container '%v': %v", pod.Name, container.Name, cpuHints)

	return map[string][]topologymanager.TopologyHint{
		string(v1.ResourceCPU): cpuHints,
	}
}

// generateCPUtopologyHints generates a set of TopologyHints given the set of
// available CPUs and the number of CPUs being requested.
//
// It follows the convention of marking all hints that have the same number of
// bits set as the narrowest matching NUMANodeAffinity with 'Preferred: true', and
// marking all others with 'Preferred: false'.
func (p *staticPolicy) generateCPUTopologyHints(availableCPUs cpuset.CPUSet, request int) []topologymanager.TopologyHint {
	// Initialize minAffinitySize to include all NUMA Nodes.
	minAffinitySize := p.topology.CPUDetails.NUMANodes().Size()
	// Initialize minSocketsOnMinAffinity to include all Sockets.
	minSocketsOnMinAffinity := p.topology.CPUDetails.Sockets().Size()

	// Iterate through all combinations of socket bitmask and build hints from them.
	hints := []topologymanager.TopologyHint{}
	bitmask.IterateBitMasks(p.topology.CPUDetails.NUMANodes().ToSlice(), func(mask bitmask.BitMask) {
		// First, update minAffinitySize and minSocketsOnMinAffinity for the
		// current request size.
		cpusInMask := p.topology.CPUDetails.CPUsInNUMANodes(mask.GetBits()...).Size()
		socketsInMask := p.topology.CPUDetails.SocketsInNUMANodes(mask.GetBits()...).Size()
		if cpusInMask >= request && mask.Count() < minAffinitySize {
			minAffinitySize = mask.Count()
			if socketsInMask < minSocketsOnMinAffinity {
				minSocketsOnMinAffinity = socketsInMask
			}
		}

		// Then check to see if we have enough CPUs available on the current
		// socket bitmask to satisfy the CPU request.
		numMatching := 0
		for _, c := range availableCPUs.ToSlice() {
			if mask.IsSet(p.topology.CPUDetails[c].NUMANodeID) {
				numMatching++
			}
		}

		// If we don't, then move onto the next combination.
		if numMatching < request {
			return
		}

		// Otherwise, create a new hint from the socket bitmask and add it to the
		// list of hints.  We set all hint preferences to 'false' on the first
		// pass through.
		hints = append(hints, topologymanager.TopologyHint{
			NUMANodeAffinity: mask,
			Preferred:        false,
		})
	})

	// Loop back through all hints and update the 'Preferred' field based on
	// counting the number of bits sets in the affinity mask and comparing it
	// to the minAffinitySize. Only those with an equal number of bits set (and
	// with a minimal set of sockets) will be considered preferred.
	for i := range hints {
		if hints[i].NUMANodeAffinity.Count() == minAffinitySize {
			nodes := hints[i].NUMANodeAffinity.GetBits()
			numSockets := p.topology.CPUDetails.SocketsInNUMANodes(nodes...).Size()
			if numSockets == minSocketsOnMinAffinity {
				hints[i].Preferred = true
			}
		}
	}

	return hints
}
