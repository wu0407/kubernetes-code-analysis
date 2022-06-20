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
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
)

type singleNumaNodePolicy struct {
	//List of NUMA Nodes available on the underlying machine
	numaNodes []int
}

var _ Policy = &singleNumaNodePolicy{}

// PolicySingleNumaNode policy name.
const PolicySingleNumaNode string = "single-numa-node"

// NewSingleNumaNodePolicy returns single-numa-node policy.
func NewSingleNumaNodePolicy(numaNodes []int) Policy {
	return &singleNumaNodePolicy{numaNodes: numaNodes}
}

func (p *singleNumaNodePolicy) Name() string {
	return PolicySingleNumaNode
}

func (p *singleNumaNodePolicy) canAdmitPodResult(hint *TopologyHint) bool {
	if !hint.Preferred {
		return false
	}
	return true
}

// Return hints that have valid bitmasks with exactly one bit set.
func filterSingleNumaHints(allResourcesHints [][]TopologyHint) [][]TopologyHint {
	var filteredResourcesHints [][]TopologyHint
	for _, oneResourceHints := range allResourcesHints {
		var filtered []TopologyHint
		for _, hint := range oneResourceHints {
			// NUMANodeAffinity为nil，且Preferred为true。即资源没有TopologyHint
			if hint.NUMANodeAffinity == nil && hint.Preferred == true {
				filtered = append(filtered, hint)
			}
			// NUMANodeAffinity不为nil，且NUMANodeAffinity位的值为1的数量为1（只亲和一个numa），且Preferred为true
			if hint.NUMANodeAffinity != nil && hint.NUMANodeAffinity.Count() == 1 && hint.Preferred == true {
				filtered = append(filtered, hint)
			}
		}
		filteredResourcesHints = append(filteredResourcesHints, filtered)
	}
	return filteredResourcesHints
}

func (p *singleNumaNodePolicy) Merge(providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
	// 遍历providersHints（所有provider的TopologyHint），输出[][]TopologyHint
	// 如果provider没有TopologyHint，则默认为[]TopologyHint{{nil, true}}
	// 如果资源没有TopologyHint，则这个资源的TopologyHint，默认为[]TopologyHint{{nil, true}}
	// 如果资源有TopologyHint且为空（不能跟NUMA affinities），则为[]TopologyHint{{nil, false}}
	filteredHints := filterProvidersHints(providersHints)
	// Filter to only include don't cares and hints with a single NUMA node.
	// filteredHints里每个[]TopologyHint里的TopologyHint进行过滤，输出过滤后的[][]TopologyHint。
	// 过滤规则：
	//   保留TopologyHint，NUMANodeAffinity为nil，且Preferred为true。即资源没有TopologyHint
	//   保留TopologyHint，NUMANodeAffinity不为nil，且NUMANodeAffinity位的值为1的数量为1（只亲和一个numa），且Preferred为true
	//   不保留其他TopologyHint
	singleNumaHints := filterSingleNumaHints(filteredHints)
	// 从singleNumaHints二维[]TopologyHint中，每个[]TopologyHint取一个TopologyHint，进行组合，生成新的[]TopologyHint，然后调用callback
	// callback是，每一组[]TopologyHint，进行（跟defaultAffinity（亲和所有numaNode）跟所有的TopologyHint进行与计算）聚合，在所有聚合的结果，选出最适合的TopologyHint。
	// 筛选规则：
	//   如果TopologyHint的位的值为1的数量相等，则取TopologyHint的NUMANodeAffinity值较小的TopologyHint。
	//   如果TopologyHint的NUMANodeAffinity位的值为1的数量不相等，则TopologyHint的NUMANodeAffinity位的值为1的数量最小的TopologyHint
	//   优先选择，聚合的结果TopologyHint的Preferred是true
	bestHint := mergeFilteredHints(p.numaNodes, singleNumaHints)

	defaultAffinity, _ := bitmask.NewBitMask(p.numaNodes...)
	// 如果bestHint的NUMANodeAffinity（最佳的NUMANodeAffinity）为默认的defaultAffinity（亲和所有numaNode），则bestHint的NUMANodeAffinity为nil
	if bestHint.NUMANodeAffinity.IsEqual(defaultAffinity) {
		bestHint = TopologyHint{nil, bestHint.Preferred}
	}

	// admit为TopologyHint的Preferred的值
	admit := p.canAdmitPodResult(&bestHint)
	return bestHint, admit
}
