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

package helper

import (
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// DefaultNormalizeScore generates a Normalize Score function that can normalize the
// scores to [0, maxPriority]. If reverse is set to true, it reverses the scores by
// subtracting it from maxPriority.
// 所有节点最大的分数为0，说明所有节点的分数都为0
//   如果reverse为true，则所有节点分数设置为maxPriority
//   否则不变
// 最大的分数不为0
//   如果reverse为true，则节点分数为maxPriority - 节点的分数
//   否则，节点的分数为maxPriority * (节点的分数 / 所有节点的最大分数)
func DefaultNormalizeScore(maxPriority int64, reverse bool, scores framework.NodeScoreList) *framework.Status {
	var maxCount int64
	for i := range scores {
		// 找到最大的分数
		if scores[i].Score > maxCount {
			maxCount = scores[i].Score
		}
	}

	// 最大的分数为0，说明所有节点的分数都为0
	if maxCount == 0 {
		// 如果倒序，则所有节点分数设置为maxPriority
		if reverse {
			for i := range scores {
				scores[i].Score = maxPriority
			}
		}
		return nil
	}

	// 最大的分数不为0
	for i := range scores {
		score := scores[i].Score

		// maxPriority * (节点的分数 / 所有节点的最大分数)
		score = maxPriority * score / maxCount
		if reverse {
			score = maxPriority - score
		}

		scores[i].Score = score
	}
	return nil
}
