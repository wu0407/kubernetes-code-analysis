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

type bestEffortPolicy struct {
	//List of NUMA Nodes available on the underlying machine
	numaNodes []int
}

var _ Policy = &bestEffortPolicy{}

// PolicyBestEffort policy name.
const PolicyBestEffort string = "best-effort"

// NewBestEffortPolicy returns best-effort policy.
func NewBestEffortPolicy(numaNodes []int) Policy {
	return &bestEffortPolicy{numaNodes: numaNodes}
}

func (p *bestEffortPolicy) Name() string {
	return PolicyBestEffort
}

func (p *bestEffortPolicy) canAdmitPodResult(hint *TopologyHint) bool {
	return true
}

func (p *bestEffortPolicy) Merge(providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
	// 遍历providersHints（所有provider的TopologyHint），输出[][]TopologyHint
	// 如果provider没有TopologyHint，则默认为[]TopologyHint{{nil, true}}
	// 如果资源没有TopologyHint，则这个资源的TopologyHint，默认为[]TopologyHint{{nil, true}}
	// 如果资源有TopologyHint且为空（不能跟NUMA affinities），则为[]TopologyHint{{nil, false}}
	filteredProvidersHints := filterProvidersHints(providersHints)
	// 从filteredProvidersHints二维[]TopologyHint中，每个[]TopologyHint取一个TopologyHint，进行组合，生成新的[]TopologyHint，然后调用callback
	// callback是，每一组[]TopologyHint，进行（跟defaultAffinity（亲和所有numaNode）跟所有的TopologyHint进行与计算）聚合，在所有聚合的结果，选出最适合的TopologyHint。
	// 筛选规则：
	// 如果TopologyHint的位的值为1的数量相等，则取TopologyHint的NUMANodeAffinity值较小的TopologyHint。
	// 如果TopologyHint的NUMANodeAffinity位的值为1的数量不相等，则TopologyHint的NUMANodeAffinity位的值为1的数量最小的TopologyHint
	// 优先选择，聚合的结果TopologyHint的Preferred是true
	bestHint := mergeFilteredHints(p.numaNodes, filteredProvidersHints)
	// 一直为true
	admit := p.canAdmitPodResult(&bestHint)
	return bestHint, admit
}
