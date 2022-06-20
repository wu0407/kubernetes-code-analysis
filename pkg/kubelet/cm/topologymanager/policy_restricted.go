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

type restrictedPolicy struct {
	bestEffortPolicy
}

var _ Policy = &restrictedPolicy{}

// PolicyRestricted policy name.
const PolicyRestricted string = "restricted"

// NewRestrictedPolicy returns restricted policy.
func NewRestrictedPolicy(numaNodes []int) Policy {
	return &restrictedPolicy{bestEffortPolicy{numaNodes: numaNodes}}
}

func (p *restrictedPolicy) Name() string {
	return PolicyRestricted
}

func (p *restrictedPolicy) canAdmitPodResult(hint *TopologyHint) bool {
	if !hint.Preferred {
		return false
	}
	return true
}

func (p *restrictedPolicy) Merge(providersHints []map[string][]TopologyHint) (TopologyHint, bool) {
	// 遍历providersHints（所有provider的TopologyHint），输出[][]TopologyHint
	// 如果provider没有TopologyHint，则默认为[]TopologyHint{{nil, true}}
	// 如果资源没有TopologyHint，则这个资源的TopologyHint，默认为[]TopologyHint{{nil, true}}
	// 如果资源有TopologyHint且为空（不能跟NUMA affinities），则为[]TopologyHint{{nil, false}}
	filteredHints := filterProvidersHints(providersHints)
	// 从filteredProvidersHints二维[]TopologyHint中，每个[]TopologyHint取一个TopologyHint，进行组合，生成新的[]TopologyHint，然后调用callback
	// callback是，每一组[]TopologyHint，进行（跟defaultAffinity（亲和所有numaNode）跟所有的TopologyHint进行与计算）聚合，在所有聚合的结果，选出最适合的TopologyHint。
	// 筛选规则：
	// 如果TopologyHint的位的值为1的数量相等，则取TopologyHint的NUMANodeAffinity值较小的TopologyHint。
	// 如果TopologyHint的NUMANodeAffinity位的值为1的数量不相等，则TopologyHint的NUMANodeAffinity位的值为1的数量最小的TopologyHint
	// 优先选择，聚合的结果TopologyHint的Preferred是true
	hint := mergeFilteredHints(p.numaNodes, filteredHints)
	// admit为TopologyHint的Preferred的值
	admit := p.canAdmitPodResult(&hint)
	return hint, admit
}
