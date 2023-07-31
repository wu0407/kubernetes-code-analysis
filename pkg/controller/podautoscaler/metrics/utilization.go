/*
Copyright 2015 The Kubernetes Authors.

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

package metrics

import (
	"fmt"
)

// GetResourceUtilizationRatio takes in a set of metrics, a set of matching requests,
// and a target utilization percentage, and calculates the ratio of
// desired to actual utilization (returning that, the actual utilization, and the raw average value)
// 统计所有pod在request和metric交集（有request和metrics），总metric和总request和总的pod数量
// 当前平均利用率为总的metrics * 100 / 总的request
// 返回当前平均利用率/目标平均利用率的比值，当前平均利用率（相对总request），总的metric/总的pod数量（原始值）
func GetResourceUtilizationRatio(metrics PodMetricsInfo, requests map[string]int64, targetUtilization int32) (utilizationRatio float64, currentUtilization int32, rawAverageValue int64, err error) {
	metricsTotal := int64(0)
	requestsTotal := int64(0)
	numEntries := 0

	// 统计所有pod在request和metric交集（有request和metrics），总metric和总request和总的pod数量
	for podName, metric := range metrics {
		request, hasRequest := requests[podName]
		// 排除没有在request列表的metric
		if !hasRequest {
			// we check for missing requests elsewhere, so assuming missing requests == extraneous metrics
			continue
		}

		metricsTotal += metric.Value
		requestsTotal += request
		numEntries++
	}

	// if the set of requests is completely disjoint from the set of metrics,
	// then we could have an issue where the requests total is zero
	if requestsTotal == 0 {
		return 0, 0, 0, fmt.Errorf("no metrics returned matched known pods")
	}

	// 当前平均利用率为总的metrics * 100 / 总的request
	currentUtilization = int32((metricsTotal * 100) / requestsTotal)

	// 当前平均利用率/目标平均利用率的比值，当前平均利用率，总的metric/总的pod数量
	return float64(currentUtilization) / float64(targetUtilization), currentUtilization, metricsTotal / int64(numEntries), nil
}

// GetMetricUtilizationRatio takes in a set of metrics and a target utilization value,
// and calculates the ratio of desired to actual utilization
// (returning that and the actual utilization)
// 计算所有metrics里总的value
// 平均使用值 = 所有metrics里总的value / metrics数量
// 返回平均使用值 / 目标利用值，平均使用值
func GetMetricUtilizationRatio(metrics PodMetricsInfo, targetUtilization int64) (utilizationRatio float64, currentUtilization int64) {
	metricsTotal := int64(0)
	// 计算所有metrics里总的value
	for _, metric := range metrics {
		metricsTotal += metric.Value
	}

	// 平均使用值
	currentUtilization = metricsTotal / int64(len(metrics))

	// 平均使用值 / 目标利用值，平均使用值
	return float64(currentUtilization) / float64(targetUtilization), currentUtilization
}
