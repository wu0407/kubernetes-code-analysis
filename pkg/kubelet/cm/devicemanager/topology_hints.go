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

package devicemanager

import (
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
)

// GetTopologyHints implements the TopologyManager HintProvider Interface which
// ensures the Device Manager is consulted when Topology Aware Hints for each
// container are created.
// 为container需要的device plugin资源生成可以分配device的TopologyHint
func (m *ManagerImpl) GetTopologyHints(pod *v1.Pod, container *v1.Container) map[string][]topologymanager.TopologyHint {
	// Garbage collect any stranded device resources before providing TopologyHints
	// 移除m.podDevices中已经terminated的pod，更新m.podDevices（pod与pod各个container分配的资源）和m.allocatedDevices（已经分配资源和device id）
	m.UpdateAllocatedDevices()

	// Loop through all device resources and generate TopologyHints for them..
	deviceHints := make(map[string][]topologymanager.TopologyHint)
	for resourceObj, requestedObj := range container.Resources.Limits {
		resource := string(resourceObj)
		requested := int(requestedObj.Value())

		// Only consider resources associated with a device plugin.
		// resource在m.healthyDevices（registered healthy devices）里或m.allocatedDevices（allocated devices）里
		if m.isDevicePluginResource(resource) {
			// Only consider devices that actually container topology information.
			// resource未定义了Topology，则设置deviceHints里的值为nil（这个resource没有对应的topologymanager.TopologyHint），继续下一个resource
			if aligned := m.deviceHasTopologyAlignment(resource); !aligned {
				klog.Infof("[devicemanager] Resource '%v' does not have a topology preference", resource)
				deviceHints[resource] = nil
				continue
			}

			// Short circuit to regenerate the same hints if there are already
			// devices allocated to the Container. This might happen after a
			// kubelet restart, for example.
			// 已经分配给container的resource的所有device id
			allocated := m.podDevices.containerDevices(string(pod.UID), container.Name, resource)
			// 已经给container分配resource的device
			if allocated.Len() > 0 {
				// 已经分配的resource的device id跟container.Resources.Limit需要的不一致，则设置deviceHints里的值为空topologymanager.TopologyHint，继续下一个resource
				if allocated.Len() != requested {
					klog.Errorf("[devicemanager] Resource '%v' already allocated to (pod %v, container %v) with different number than request: requested: %d, allocated: %d", resource, string(pod.UID), container.Name, requested, allocated.Len())
					deviceHints[resource] = []topologymanager.TopologyHint{}
					continue
				}
				klog.Infof("[devicemanager] Regenerating TopologyHints for resource '%v' already allocated to (pod %v, container %v)", resource, string(pod.UID), container.Name)
				// 已经分配的resource的device id数量跟container.Resources.Limit需要的一致，则生成device TopologyHints，继续下一个resource
				// 尝试不同数量的numaNodes，从allocated（已经分配的device）里查找能够匹配numanodes的device。如果能够满足request需要的数量，则把当前mask（numa对应的mask）且数字Preferred为false，添加到deviceHints[resource]。
				//  尝试不同数量的numaNodes，从resource里所有已经注册的device查找能够匹配numanodes的device，且能够满足request需要的数量，找到最小满足需要的mask（numanodes）位数
				// deviceHints[resource]中mask（numa对应的mask）位数与最小满足需要的mask（numanodes）位数一样，则修改这个hint的Preferred为true
				deviceHints[resource] = m.generateDeviceTopologyHints(resource, allocated, requested)
				continue
			}

			// 未给container分配resource的device

			// Get the list of available devices, for which TopologyHints should be generated.
			// m.healthyDevices的resource的device里未使用的device集合
			available := m.getAvailableDevices(resource)
			// 可用的device数量小于需要的数量，则设置deviceHints里的值为空topologymanager.TopologyHint，继续下一个resource
			if available.Len() < requested {
				klog.Errorf("[devicemanager] Unable to generate topology hints: requested number of devices unavailable for '%s': requested: %d, available: %d", resource, requested, available.Len())
				deviceHints[resource] = []topologymanager.TopologyHint{}
				continue
			}

			// Generate TopologyHints for this resource given the current
			// request size and the list of available devices.
			// 可用的device数量大于等于需要的数量，则生成device TopologyHints
			// 尝试不同数量的numaNodes，从available（可以分配的device）里查找能够匹配numanodes的device。如果能够满足request需要的数量，则把当前mask（numa对应的mask）且数字Preferred为false，添加到deviceHints[resource]。
			//  尝试不同数量的numaNodes，从resource里所有已经注册的device查找能够匹配numanodes的device，且能够满足request需要的数量，找到最小满足需要的mask（numanodes）位数
			// deviceHints[resource]中mask（numa对应的mask）位数与最小满足需要的mask（numanodes）位数一样，则修改这个hint的Preferred为true
			deviceHints[resource] = m.generateDeviceTopologyHints(resource, available, requested)
		}
	}

	return deviceHints
}

// resource是否定义了Topology
func (m *ManagerImpl) deviceHasTopologyAlignment(resource string) bool {
	// If any device has Topology set, we assume they care about alignment.
	for device := range m.allDevices[resource] {
		if m.allDevices[resource][device].Topology != nil {
			return true
		}
	}
	return false
}

// m.healthyDevices的resource的device里排除已经分配的resource的device（m.allocatedDevices[resource]）
// 即 m.healthyDevices的resource的device里未使用的device集合
func (m *ManagerImpl) getAvailableDevices(resource string) sets.String {
	// Strip all devices in use from the list of healthy ones.
	return m.healthyDevices[resource].Difference(m.allocatedDevices[resource])
}

// 尝试不同数量的numaNodes，从devices里查找能够匹配numanodes的device。如果能够满足request需要的数量，则把当前mask（numa对应的mask）且数字Preferred为false，添加到hints。
// 尝试不同数量的numaNodes，从resource里所有已经注册的device查找能够匹配numanodes的device，且能够满足request需要的数量，找到最小满足需要的mask（numanodes）位数
// hints中mask（numa对应的mask）位数与最小满足需要的mask（numanodes）位数一样，则修改这个hint的Preferred为true
func (m *ManagerImpl) generateDeviceTopologyHints(resource string, devices sets.String, request int) []topologymanager.TopologyHint {
	// Initialize minAffinitySize to include all NUMA Nodes
	// 节点里cpu有多少个numaNode
	minAffinitySize := len(m.numaNodes)

	// Iterate through all combinations of NUMA Nodes and build hints from them.
	hints := []topologymanager.TopologyHint{}
	// 依次从m.numaNodes中按顺序取1个item、2个item、3个item、直到所有item，执行callback
	// 尝试不同数量的numaNodes，从devices里查找能够匹配numanodes的device。如果能够满足需要的数量，则把当前mask（numa对应的mask）且设置Preferred为false，添加到hints。当resource里所有device里匹配mask的device数量（总的可用的）大于等于需要数量，会修改最小的mask（numanodes）位数（所有mask里能够匹配出的device数量满足需要的里，最小mask位数），初始最小的mask（numanodes）位数为numa数量。
	bitmask.IterateBitMasks(m.numaNodes, func(mask bitmask.BitMask) {
		// First, update minAffinitySize for the current request size.
		devicesInMask := 0
		// 遍历resource里所有已经注册的device，统计device里多少个device匹配mask
		for _, device := range m.allDevices[resource] {
			// device没有Topology，则跳过
			if device.Topology == nil {
				continue
			}
			// 统计device的Topology.Nodes只要有一个node在mask中，则devicesInMask加1
			for _, node := range device.Topology.Nodes {
				// node.ID是在mask中
				if mask.IsSet(int(node.ID)) {
					devicesInMask++
					break
				}
			}
		}
		// resource里所有device里匹配mask的device数量（总的可用的）大于等于需要数量，且mask的位数小于minAffinitySize（第一次递归执行是节点numa数量），则minAffinitySize为mask的位数
		if devicesInMask >= request && mask.Count() < minAffinitySize {
			minAffinitySize = mask.Count()
		}

		// Then check to see if we have enough devices available on the current
		// NUMA Node combination to satisfy the device request.
		numMatching := 0
		// 统计devices（已经分配给container的device或可分配给container的device）里多少个device匹配mask
		for d := range devices {
			// 忽略devices（已经分配给container的device或可分配给container的device）里没有Topology的device
			if m.allDevices[resource][d].Topology == nil {
				continue
			}
			// devices（已经分配给container的device或可分配给container的device）里device的Topology.Nodes的中有一个node在mask里，则numMatching加1，继续下一个device
			for _, node := range m.allDevices[resource][d].Topology.Nodes {
				if mask.IsSet(int(node.ID)) {
					numMatching++
					break
				}
			}
		}

		// If we don't, then move onto the next combination.
		// devices（已经分配给container的device或可分配给container的device）里匹配mask的device数量小于需要的数量，直接返回
		if numMatching < request {
			return
		}

		// devices（已经分配给container的device或可分配给container的device）里匹配mask的device数量大于等于需要的数量

		// Otherwise, create a new hint from the NUMA mask and add it to the
		// list of hints.  We set all hint preferences to 'false' on the first
		// pass through.
		// 这个mask结果（Preferred为false）添加到hints里
		hints = append(hints, topologymanager.TopologyHint{
			NUMANodeAffinity: mask,
			Preferred:        false,
		})
	})

	// Loop back through all hints and update the 'Preferred' field based on
	// counting the number of bits sets in the affinity mask and comparing it
	// to the minAffinity. Only those with an equal number of bits set will be
	// considered preferred.
	// 所有mask里能够匹配出的device数量满足需要的里，最小mask位数对应的hint，修改Preferred为true
	for i := range hints {
		if hints[i].NUMANodeAffinity.Count() == minAffinitySize {
			hints[i].Preferred = true
		}
	}

	return hints
}
