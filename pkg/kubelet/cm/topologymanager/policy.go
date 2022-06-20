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
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
)

// Policy interface for Topology Manager Pod Admit Result
type Policy interface {
	// Returns Policy Name
	Name() string
	// Returns a merged TopologyHint based on input from hint providers
	// and a Pod Admit Handler Response based on hints and policy type
	Merge(providersHints []map[string][]TopologyHint) (TopologyHint, bool)
}

// Merge a TopologyHints permutation to a single hint by performing a bitwise-AND
// of their affinity masks. The hint shall be preferred if all hits in the permutation
// are preferred.
// 遍历permutation将每个TopologyHint跟numaNodes进行（与计算）聚合
// NUMANodeAffinity为nil，那么就是亲和所有numaNode
func mergePermutation(numaNodes []int, permutation []TopologyHint) TopologyHint {
	// Get the NUMANodeAffinity from each hint in the permutation and see if any
	// of them encode unpreferred allocations.
	preferred := true
	defaultAffinity, _ := bitmask.NewBitMask(numaNodes...)
	var numaAffinities []bitmask.BitMask
	for _, hint := range permutation {
		// Only consider hints that have an actual NUMANodeAffinity set.
		// NUMANodeAffinity为nil，那么就是亲和所有numaNode
		if hint.NUMANodeAffinity == nil {
			numaAffinities = append(numaAffinities, defaultAffinity)
		} else {
			// 否则亲和这个NUMANodeAffinity
			numaAffinities = append(numaAffinities, hint.NUMANodeAffinity)
		}

		if !hint.Preferred {
			preferred = false
		}
	}

	// Merge the affinities using a bitwise-and operation.
	// 按位逐个按与进行计算
	mergedAffinity := bitmask.And(defaultAffinity, numaAffinities...)
	// Build a mergedHint from the merged affinity mask, indicating if an
	// preferred allocation was used to generate the affinity mask or not.
	return TopologyHint{mergedAffinity, preferred}
}

// providersHints []map[string][]TopologyHint，第一层是provider，一个provider有多个resource（map的key为resource名字），一个resource有多个TopologyHint。TopologyHint为cpu的mask和是否prefer。
// 遍历providersHints（所有provider的TopologyHint），输出[][]TopologyHint
// 如果provider没有TopologyHint，则默认为[]TopologyHint{{nil, true}}
// 如果资源没有TopologyHint，则这个资源的TopologyHint，默认为[]TopologyHint{{nil, true}}
// 如果资源有TopologyHint且为空（不能跟NUMA affinities），则为[]TopologyHint{{nil, false}}
func filterProvidersHints(providersHints []map[string][]TopologyHint) [][]TopologyHint {
	// Loop through all hint providers and save an accumulated list of the
	// hints returned by each hint provider. If no hints are provided, assume
	// that provider has no preference for topology-aware allocation.
	var allProviderHints [][]TopologyHint
	for _, hints := range providersHints {
		// If hints is nil, insert a single, preferred any-numa hint into allProviderHints.
		// 如果provider没有提供TopologyHint，则默认为[]TopologyHint{{nil, true}}
		if len(hints) == 0 {
			klog.Infof("[topologymanager] Hint Provider has no preference for NUMA affinity with any resource")
			allProviderHints = append(allProviderHints, []TopologyHint{{nil, true}})
			continue
		}

		// Otherwise, accumulate the hints for each resource type into allProviderHints.
		for resource := range hints {
			// cpu资源，对应的是container没有设置request，或不是Guaranteed的pod，或是Guaranteed的pod的container的request的cpu为非整数个cpu，或Guaranteed的pod的init container的request为0，或numnode里可用的逻辑cpu数量小于需要的数量
			// device plugin资源，对应的是container没有设置limit，或container请求的资源不支持Topology
			if hints[resource] == nil {
				klog.Infof("[topologymanager] Hint Provider has no preference for NUMA affinity with resource '%s'", resource)
				allProviderHints = append(allProviderHints, []TopologyHint{{nil, true}})
				continue
			}

			// cpu资源，已经分配的cpu不等于request需要的cpu数量
			// device plugin资源，可用的device数量小于需要的数量，或已经分配的resource的device id跟container.Resources.Limit需要的不一致
			if len(hints[resource]) == 0 {
				klog.Infof("[topologymanager] Hint Provider has no possible NUMA affinities for resource '%s'", resource)
				allProviderHints = append(allProviderHints, []TopologyHint{{nil, false}})
				continue
			}

			allProviderHints = append(allProviderHints, hints[resource])
		}
	}
	return allProviderHints
}

// 从filteredHints二维[]TopologyHint中，每个[]TopologyHint取一个TopologyHint，进行组合，生成新的[]TopologyHint，然后调用callback
// callback是，每一组[]TopologyHint，进行（跟defaultAffinity（亲和所有numaNode）跟所有的TopologyHint进行与计算）聚合，在所有聚合的结果，如果TopologyHint的位的值为1的数量相等，则取TopologyHint的NUMANodeAffinity值较小的TopologyHint。如果TopologyHint的NUMANodeAffinity位的值为1的数量不相等，则TopologyHint的NUMANodeAffinity位的值为1的数量最小的TopologyHint
func mergeFilteredHints(numaNodes []int, filteredHints [][]TopologyHint) TopologyHint {
	// Set the default affinity as an any-numa affinity containing the list
	// of NUMA Nodes available on this machine.
	// 默认为亲和所有numaNode
	defaultAffinity, _ := bitmask.NewBitMask(numaNodes...)

	// Set the bestHint to return from this function as {nil false}.
	// This will only be returned if no better hint can be found when
	// merging hints from each hint provider.
	bestHint := TopologyHint{defaultAffinity, false}
	// 从filteredHints二维[]TopologyHint中，每个[]TopologyHint取一个TopologyHint，进行组合，生成新的[]TopologyHint，然后调用callback
	// callback是，每一组[]TopologyHint，进行（跟defaultAffinity跟所有的TopologyHint进行与计算）聚合，在所有聚合的结果，选出最合适的TopologyHint。
	// 筛选算法：
	// 如果TopologyHint的位的值为1的数量相等，则取TopologyHint的NUMANodeAffinity值较小的TopologyHint。
	// 如果TopologyHint的NUMANodeAffinity位的值为1的数量不相等，则TopologyHint的NUMANodeAffinity位的值为1的数量最小的TopologyHint。
	// 优先选择聚合的结果TopologyHint的Preferred是true
	iterateAllProviderTopologyHints(filteredHints, func(permutation []TopologyHint) {
		// Get the NUMANodeAffinity from each hint in the permutation and see if any
		// of them encode unpreferred allocations.
		// 遍历permutation将每个TopologyHint跟numaNodes进行（与计算）聚合
		// TopologyHint的NUMANodeAffinity为nil，那么就是亲和所有numaNode
		mergedHint := mergePermutation(numaNodes, permutation)
		// Only consider mergedHints that result in a NUMANodeAffinity > 0 to
		// replace the current bestHint.
		// 没有任何亲和的位（mask里没有为1的位），直接返回
		if mergedHint.NUMANodeAffinity.Count() == 0 {
			return
		}

		// If the current bestHint is non-preferred and the new mergedHint is
		// preferred, always choose the preferred hint over the non-preferred one.
		// 如果目前的bestHint的Preferred为false（第一次循环），且聚合的结果TopologyHint的Preferred是true，则设置bestHint为这个聚合的结果TopologyHint
		if mergedHint.Preferred && !bestHint.Preferred {
			bestHint = mergedHint
			return
		}

		// If the current bestHint is preferred and the new mergedHint is
		// non-preferred, never update bestHint, regardless of mergedHint's
		// narowness.
		// 如果聚合的结果TopologyHint的Preferred是false，且目前的bestHint的Preferred为false
		if !mergedHint.Preferred && bestHint.Preferred {
			return
		}

		// If mergedHint and bestHint has the same preference, only consider
		// mergedHints that have a narrower NUMANodeAffinity than the
		// NUMANodeAffinity in the current bestHint.
		// 如果聚合的结果TopologyHint的Preferred和目前的bestHint的Preferred一样（都为true或false）	
		// 1. 聚合的结果TopologyHint的位的值为1的数量与bestHint相等，且聚合的结果TopologyHint的NUMANodeAffinity值大于等于bestHint的NUMANodeAffinity值，直接返回（不设置为bestHint）
		// 2. 聚合的结果TopologyHint的位的值为1的数量，大于等于bestHint里位的值为1的数量，直接返回（不设置为bestHint）
		if !mergedHint.NUMANodeAffinity.IsNarrowerThan(bestHint.NUMANodeAffinity) {
			return
		}

		// In all other cases, update bestHint to the current mergedHint
		// 聚合的结果TopologyHint的位的值为1的数量与bestHint相等，且聚合的结果TopologyHint的NUMANodeAffinity值小于bestHint，则设置为bestHint
		// 聚合的结果TopologyHint的位的值为1的数量，小于bestHint里位的值为1的数量，则设置为bestHint
		bestHint = mergedHint
	})

	return bestHint
}

// Iterate over all permutations of hints in 'allProviderHints [][]TopologyHint'.
//
// This procedure is implemented as a recursive function over the set of hints
// in 'allproviderHints[i]'. It applies the function 'callback' to each
// permutation as it is found. It is the equivalent of:
//
// for i := 0; i < len(providerHints[0]); i++
//     for j := 0; j < len(providerHints[1]); j++
//         for k := 0; k < len(providerHints[2]); k++
//             ...
//             for z := 0; z < len(providerHints[-1]); z++
//                 permutation := []TopologyHint{
//                     providerHints[0][i],
//                     providerHints[1][j],
//                     providerHints[2][k],
//                     ...
//                     providerHints[-1][z]
//                 }
//                 callback(permutation)
// 二维数组中，每个数组取一个，进行组合，生成新得数组，然后调用callback
func iterateAllProviderTopologyHints(allProviderHints [][]TopologyHint, callback func([]TopologyHint)) {
	// Internal helper function to accumulate the permutation before calling the callback.
	var iterate func(i int, accum []TopologyHint)
	iterate = func(i int, accum []TopologyHint) {
		// Base case: we have looped through all providers and have a full permutation.
		if i == len(allProviderHints) {
			callback(accum)
			return
		}

		// Loop through all hints for provider 'i', and recurse to build the
		// the permutation of this hint with all hints from providers 'i++'.
		for j := range allProviderHints[i] {
			// 本地迭代，将上次的迭代的[]TopologyHint里的TopologyHint，append到accum
			iterate(i+1, append(accum, allProviderHints[i][j]))
		}
	}
	iterate(0, []TopologyHint{})
}
