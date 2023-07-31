/*
Copyright 2016 The Kubernetes Authors.

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

package podautoscaler

import (
	"context"
	"fmt"
	"math"
	"time"

	autoscaling "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	corelisters "k8s.io/client-go/listers/core/v1"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	metricsclient "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
)

const (
	// defaultTestingTolerance is default value for calculating when to
	// scale up/scale down.
	defaultTestingTolerance                     = 0.1
	defaultTestingCPUInitializationPeriod       = 2 * time.Minute
	defaultTestingDelayOfInitialReadinessStatus = 10 * time.Second
)

// ReplicaCalculator bundles all needed information to calculate the target amount of replicas
type ReplicaCalculator struct {
	metricsClient                 metricsclient.MetricsClient
	podLister                     corelisters.PodLister
	tolerance                     float64
	cpuInitializationPeriod       time.Duration
	delayOfInitialReadinessStatus time.Duration
}

// NewReplicaCalculator creates a new ReplicaCalculator and passes all necessary information to the new instance
func NewReplicaCalculator(metricsClient metricsclient.MetricsClient, podLister corelisters.PodLister, tolerance float64, cpuInitializationPeriod, delayOfInitialReadinessStatus time.Duration) *ReplicaCalculator {
	return &ReplicaCalculator{
		metricsClient:                 metricsClient,
		podLister:                     podLister,
		tolerance:                     tolerance,
		cpuInitializationPeriod:       cpuInitializationPeriod,
		delayOfInitialReadinessStatus: delayOfInitialReadinessStatus,
	}
}

// GetResourceReplicas calculates the desired replica count based on a target resource utilization percentage
// of the given resource for pods matching the given selector in the given namespace, and the current replica count
// 进行一系列复杂的数据计算和metric数据修复逻辑
// 当前平均利用率为总的metrics * 100 / 总的request
// 返回当前平均利用率/目标平均利用率的比值，当前平均利用率（相对总request），总的metric/总的pod数量（原始值），第一个metric的Timestamp
func (c *ReplicaCalculator) GetResourceReplicas(ctx context.Context, currentReplicas int32, targetUtilization int32, resource v1.ResourceName, namespace string, selector labels.Selector, container string) (replicaCount int32, utilization int32, rawUtilization int64, timestamp time.Time, err error) {
	// 根据namespace和selector list所有podMetrics资源
	// 如果container不为空，则从所有metrics.Items里（metricsapi.podMetrics的Containers里）找到resource类型的metric，保存到PodMetricsInfo
	// 如果container为空，则遍历rawMetrics（[]metricsapi.podMetrics）里的所有Containers的resource之和，保存在PodMetricsInfo（pod名称对应的PodMetric）
	// 返回PodMetricsInfo和第一个metric的Timestamp
	metrics, timestamp, err := c.metricsClient.GetResourceMetric(ctx, resource, namespace, selector, container)
	if err != nil {
		return 0, 0, 0, time.Time{}, fmt.Errorf("unable to get metrics for resource %s: %v", resource, err)
	}
	podList, err := c.podLister.Pods(namespace).List(selector)
	if err != nil {
		return 0, 0, 0, time.Time{}, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	itemsLen := len(podList)
	if itemsLen == 0 {
		return 0, 0, 0, time.Time{}, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	// c.cpuInitializationPeriod默认为5分钟，c.delayOfInitialReadinessStatus默认为30秒
	// missingPods是没有监控数据的pod，ignorePods是被删除或phase为Failed的pod。
	// pod为unready：
	// 当resource为cpu时候，没有readyCondition或pod.Status.StartTime为nil
	// 或有readyCondition或pod.Status.StartTime为nil 
	//   pod启动时间在c.cpuInitializationPeriod内，readyCondition为False或metric是在readyCondition.LastTransitionTime加metric.Window之前（ready状态且metric计算周期在condition.LastTransitionTime之前）
	//   或readyCondition为False且pod.Status.StartTime加c.delayOfInitialReadinessStatus在condition.LastTransitionTime之后(不在pod.Status.StartTime到pod.Status.StartTime+c.delayOfInitialReadinessStatus内），则认为pod为unready
	readyPodCount, unreadyPods, missingPods, ignoredPods := groupPods(podList, metrics, resource, c.cpuInitializationPeriod, c.delayOfInitialReadinessStatus)
	// metrics移除ignoredPods和unreadyPods
	removeMetricsForPods(metrics, ignoredPods)
	removeMetricsForPods(metrics, unreadyPods)
	// 如果container == ""，统计所有pod里所有container的resource的request
	// 如果container不为""，统计所有pod里container的resource的request
	requests, err := calculatePodRequests(podList, container, resource)
	if err != nil {
		return 0, 0, 0, time.Time{}, err
	}

	if len(metrics) == 0 {
		return 0, 0, 0, time.Time{}, fmt.Errorf("did not receive metrics for any ready pods")
	}

	// 统计所有pod在request和metric交集（有request和metrics），总metric和总request和总的pod数量
	// 当前平均利用率为总的metrics * 100 / 总的request
	// 返回当前平均利用率/目标平均利用率的比值，当前平均利用率（相对总request），总的metric/总的pod数量（原始值）
	usageRatio, utilization, rawUtilization, err := metricsclient.GetResourceUtilizationRatio(metrics, requests, targetUtilization)
	if err != nil {
		return 0, 0, 0, time.Time{}, err
	}

	// 是否（需要扩容且存在unreadyPods）
	rebalanceIgnored := len(unreadyPods) > 0 && usageRatio > 1.0
	// 存在没有metric监控数据的pod，都要进行数据修复。有unreadyPods，只在需要扩容时候需要数据修复

	// 不存在没有metric监控数据的pod，除了（需要扩容且存在unreadyPods），都执行下面算法进行返回。
	// 不是（扩容且存在unreadyPods）（比如不存在unready or missing pods，或缩容或不扩缩容，扩容且不存在unready or missing pods），且存在没有metric监控数据的pod
	// 比如需要缩容且有unreadyPods，且不存在无监控数据pod
	if !rebalanceIgnored && len(missingPods) == 0 {
		// usageRatio在[1-c.tolerance, 1+c.tolerance]之间，则不进行扩缩容
		if math.Abs(1.0-usageRatio) <= c.tolerance {
			// return the current replicas if the change would be too small
			return currentReplicas, utilization, rawUtilization, timestamp, nil
		}

		// if we don't have any unready or missing pods, we can calculate the new replica count now
		// 最终副本数为usageRatio * readyPodCount，然后向上取整。平均使用值（相对总request），总的平均使用值（原始值），和第一个metric的Timestamp
		return int32(math.Ceil(usageRatio * float64(readyPodCount))), utilization, rawUtilization, timestamp, nil
	}

	// 存在没有metric监控数据的pod
	// 根据已有监控的pod，计算结果需要缩容时候，没有监控数据认为使用100%，少缩容。
	// 计算结果需要扩容时候，没有监控数据认为使用0，少扩容。
	if len(missingPods) > 0 {
		// 需要缩容
		if usageRatio < 1.0 {
			// on a scale-down, treat missing pods as using 100% of the resource request
			// pod没有metric监控数据，设置pod的metric值为targetUtilization（即使用100%）
			for podName := range missingPods {
				metrics[podName] = metricsclient.PodMetric{Value: requests[podName]}
			}
		} else if usageRatio > 1.0 {
			// on a scale-up, treat missing pods as using 0% of the resource request
			// 需要扩容，pod没有metric监控数据，设置pod的metric值为0
			for podName := range missingPods {
				metrics[podName] = metricsclient.PodMetric{Value: 0}
			}
		}
	}

	// 需要扩容且存在unreadyPods，则认为unreadyPods的metric使用值为0，少扩容
	if rebalanceIgnored {
		// on a scale-up, treat unready pods as using 0% of the resource request
		for podName := range unreadyPods {
			metrics[podName] = metricsclient.PodMetric{Value: 0}
		}
	}

	// re-run the utilization calculation with our new numbers
	// 进行metric数据修复后，重新计算使用比率
	// 统计所有pod在request和metric交集（有request和metrics），总metric和总request和总的pod数量
	// 当前平均利用率为总的metrics * 100 / 总的request
	// 返回当前平均利用率/目标平均利用率的比值，当前平均利用率（相对总request），总的metric/总的pod数量（原始值）
	newUsageRatio, _, _, err := metricsclient.GetResourceUtilizationRatio(metrics, requests, targetUtilization)
	if err != nil {
		return 0, utilization, rawUtilization, time.Time{}, err
	}

	// 以下三种情况，不进行扩缩容
	// newUsageRatio在[1-c.tolerance, 1+c.tolerance]之间
	// 没有修复metrics数据之前（只计算有监控数据且移除ignorePods（被删除或phase为Failed的pod）和unreadyPods）状态，是需要缩容。修复metrics数据之后，需要扩容
	// 没有修复metrics数据之前状态，是需要扩容。修复metrics数据之后，需要缩容
	// 返回当前副本数，修复metrics数据之前（只计算有监控数据且移除ignorePods（被删除或phase为Failed的pod）和unreadyPods）平均利用率（相对总request），总的平均使用值（原始值），和第一个metric的Timestamp
	if math.Abs(1.0-newUsageRatio) <= c.tolerance || (usageRatio < 1.0 && newUsageRatio > 1.0) || (usageRatio > 1.0 && newUsageRatio < 1.0) {
		// return the current replicas if the change would be too small,
		// or if the new usage ratio would cause a change in scale direction
		return currentReplicas, utilization, rawUtilization, timestamp, nil
	}

	// 最终的副本数为newUsageRatio * len(metrics)（包含ready和apiserver中没有metric，原来需要扩容时候，包含unready pod），然后向上取整
	newReplicas := int32(math.Ceil(newUsageRatio * float64(len(metrics))))
	// 以下二种情况，不进行扩缩容 https://github.com/kubernetes/kubernetes/pull/89465 https://github.com/kubernetes/kubernetes/pull/85027
	// 修复metrics数据之后，仍然需要缩容。且最终的副本数大于当前副本数（滚动更新时候，（进行list）当前的副本数为workload.replicas，等获取metric时候pod数量变多（多生出的pod））
	// 修复metrics数据之后，仍然需要扩容。且最终的副本数小于当前副本数。（滚动更新时候，（进行list）当前的副本数，大于workload.replicas，等获取metric时候pod数量变为workload.replicas（变少））（scale.status.replicas跟workload.replicas什么关系？如果workload是deployment，则为deployment.Status.Replicas，在pkg\registry\apps\deployment\storage\storage.go scaleFromDeployment）
	// 返回当前副本数，修复metrics数据之前（只计算有监控数据且移除ignorePods（被删除或phase为Failed的pod）和unreadyPods）平均使用值
	if (newUsageRatio < 1.0 && newReplicas > currentReplicas) || (newUsageRatio > 1.0 && newReplicas < currentReplicas) {
		// return the current replicas if the change of metrics length would cause a change in scale direction
		return currentReplicas, utilization, rawUtilization, timestamp, nil
	}

	// return the result, where the number of replicas considered is
	// however many replicas factored into our calculation
	return newReplicas, utilization, rawUtilization, timestamp, nil
}

// GetRawResourceReplicas calculates the desired replica count based on a target resource utilization (as a raw milli-value)
// for pods matching the given selector in the given namespace, and the current replica count
// 根据namespace和selector list所有podMetrics资源
// 如果container不为空，则从所有metrics.Items里（metricsapi.podMetrics的Containers里）找到resource类型的metric，保存到PodMetricsInfo
// 如果container为空，则遍历rawMetrics（[]metricsapi.podMetrics）里的所有Containers的resource之和，保存在PodMetricsInfo（pod名称对应的PodMetric）
// 获得PodMetricsInfo（所有pod metric）和第一个pod metric的Timestamp
// 进行一系列复杂的数据计算和metric数据修复逻辑，获得最终副本数和平均使用值
// 返回最终副本数，平均使用值，第一个pod metric的Timestamp
func (c *ReplicaCalculator) GetRawResourceReplicas(ctx context.Context, currentReplicas int32, targetUtilization int64, resource v1.ResourceName, namespace string, selector labels.Selector, container string) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	// 根据namespace和selector list所有podMetrics资源
	// 如果container不为空，则从所有metrics.Items里（metricsapi.podMetrics的Containers里）找到resource类型的metric，保存到PodMetricsInfo
	// 如果container为空，则遍历rawMetrics（[]metricsapi.podMetrics）里的所有Containers的resource之和，保存在PodMetricsInfo（pod名称对应的PodMetric）
	// 返回PodMetricsInfo和第一个metric的Timestamp
	metrics, timestamp, err := c.metricsClient.GetResourceMetric(ctx, resource, namespace, selector, container)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metrics for resource %s: %v", resource, err)
	}

	// 进行一系列复杂的数据计算和metric数据修复逻辑，返回最终副本数和平均使用值
	replicaCount, utilization, err = c.calcPlainMetricReplicas(metrics, currentReplicas, targetUtilization, namespace, selector, resource)
	return replicaCount, utilization, timestamp, err
}

// GetMetricReplicas calculates the desired replica count based on a target metric utilization
// (as a milli-value) for pods matching the given selector in the given namespace, and the
// current replica count
func (c *ReplicaCalculator) GetMetricReplicas(currentReplicas int32, targetUtilization int64, metricName string, namespace string, selector labels.Selector, metricSelector labels.Selector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/pod/{name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]，第一个metric的timestamp
	metrics, timestamp, err := c.metricsClient.GetRawMetric(metricName, namespace, selector, metricSelector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metric %s: %v", metricName, err)
	}

	// 进行一系列复杂的数据计算和metric数据修复逻辑，返回最终副本数和平均使用值
	replicaCount, utilization, err = c.calcPlainMetricReplicas(metrics, currentReplicas, targetUtilization, namespace, selector, v1.ResourceName(""))
	return replicaCount, utilization, timestamp, err
}

// calcPlainMetricReplicas calculates the desired replicas for plain (i.e. non-utilization percentage) metrics.
func (c *ReplicaCalculator) calcPlainMetricReplicas(metrics metricsclient.PodMetricsInfo, currentReplicas int32, targetUtilization int64, namespace string, selector labels.Selector, resource v1.ResourceName) (replicaCount int32, utilization int64, err error) {

	podList, err := c.podLister.Pods(namespace).List(selector)
	if err != nil {
		return 0, 0, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	if len(podList) == 0 {
		return 0, 0, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	// c.cpuInitializationPeriod默认为5分钟，c.delayOfInitialReadinessStatus默认为30秒
	// missingPods是没有监控数据的pod，ignorePods是被删除或phase为Failed的pod。
	// pod为unready：
	// 当resource为cpu时候，没有readyCondition或pod.Status.StartTime为nil
	// 或有readyCondition或pod.Status.StartTime为nil 
	//   pod启动时间在c.cpuInitializationPeriod内，readyCondition为False或metric是在readyCondition.LastTransitionTime加metric.Window之前（ready状态且metric计算周期在condition.LastTransitionTime之前）
	//   或readyCondition为False且pod.Status.StartTime加c.delayOfInitialReadinessStatus在condition.LastTransitionTime之后(不在pod.Status.StartTime到pod.Status.StartTime+c.delayOfInitialReadinessStatus内），则认为pod为unready
	readyPodCount, unreadyPods, missingPods, ignoredPods := groupPods(podList, metrics, resource, c.cpuInitializationPeriod, c.delayOfInitialReadinessStatus)
	// metrics移除ignoredPods和unreadyPods
	removeMetricsForPods(metrics, ignoredPods)
	removeMetricsForPods(metrics, unreadyPods)

	if len(metrics) == 0 {
		return 0, 0, fmt.Errorf("did not receive metrics for any ready pods")
	}

	// 计算所有metrics里总的value
	// 平均使用值 = 所有metrics里总的value / metrics数量
	// 返回平均使用值 / 目标利用值，平均使用值
	usageRatio, utilization := metricsclient.GetMetricUtilizationRatio(metrics, targetUtilization)

	// 是否（需要扩容且存在unreadyPods）
	rebalanceIgnored := len(unreadyPods) > 0 && usageRatio > 1.0

	// 存在没有metric监控数据的pod，都要进行数据修复。有unreadyPods，只在需要扩容时候需要数据修复

	// 不存在没有metric监控数据的pod，除了（需要扩容且存在unreadyPods），都执行下面算法进行返回。
	// 不是（扩容且存在unreadyPods）（比如不存在unready or missing pods，或缩容或不扩缩容，扩容且不存在unready or missing pods），且存在没有metric监控数据的pod
	// 比如需要缩容且有unreadyPods，且不存在无监控数据pod
	if !rebalanceIgnored && len(missingPods) == 0 {
		// usageRatio在[1-c.tolerance, 1+c.tolerance]之间，则不进行扩缩容
		if math.Abs(1.0-usageRatio) <= c.tolerance {
			// return the current replicas if the change would be too small
			return currentReplicas, utilization, nil
		}

		// if we don't have any unready or missing pods, we can calculate the new replica count now
		// 最终副本数为usageRatio * readyPodCount，然后向上取整。平均使用值
		return int32(math.Ceil(usageRatio * float64(readyPodCount))), utilization, nil
	}

	// 存在没有metric监控数据的pod
	// 根据已有监控的pod，计算结果需要缩容时候，没有监控数据认为使用100%，少缩容。
	// 计算结果需要扩容时候，没有监控数据认为使用0，少扩容。
	if len(missingPods) > 0 {
		// 需要缩容
		if usageRatio < 1.0 {
			// on a scale-down, treat missing pods as using 100% of the resource request
			// pod没有metric监控数据，设置pod的metric值为targetUtilization（即使用100%）
			for podName := range missingPods {
				metrics[podName] = metricsclient.PodMetric{Value: targetUtilization}
			}
		} else {
			// on a scale-up, treat missing pods as using 0% of the resource request
			// 需要扩容，pod没有metric监控数据，设置pod的metric值为0
			for podName := range missingPods {
				metrics[podName] = metricsclient.PodMetric{Value: 0}
			}
		}
	}

	// 需要扩容且存在unreadyPods，则认为unreadyPods的metric使用值为0，少扩容
	if rebalanceIgnored {
		// on a scale-up, treat unready pods as using 0% of the resource request
		for podName := range unreadyPods {
			metrics[podName] = metricsclient.PodMetric{Value: 0}
		}
	}

	// re-run the utilization calculation with our new numbers
	// 进行metric数据修复后，重新计算使用比率（平均使用值 / 目标利用值）
	// 计算所有metrics里总的value
	// 平均使用值 = 所有metrics里总的value / metrics数量
	// 返回平均使用值 / 目标利用值，平均使用值
	newUsageRatio, _ := metricsclient.GetMetricUtilizationRatio(metrics, targetUtilization)

	// 以下三种情况，不进行扩缩容
	// newUsageRatio在[1-c.tolerance, 1+c.tolerance]之间
	// 没有修复metrics数据之前（只计算有监控数据且移除ignorePods（被删除或phase为Failed的pod）和unreadyPods）状态，是需要缩容。修复metrics数据之后，需要扩容
	// 没有修复metrics数据之前状态，是需要扩容。修复metrics数据之后，需要缩容
	// 返回当前副本数，修复metrics数据之前（只计算有监控数据且移除ignorePods（被删除或phase为Failed的pod）和unreadyPods）平均使用值
	if math.Abs(1.0-newUsageRatio) <= c.tolerance || (usageRatio < 1.0 && newUsageRatio > 1.0) || (usageRatio > 1.0 && newUsageRatio < 1.0) {
		// return the current replicas if the change would be too small,
		// or if the new usage ratio would cause a change in scale direction
		return currentReplicas, utilization, nil
	}

	// 最终的副本数为newUsageRatio * len(metrics)（包含ready和apiserver中没有metric，原来需要扩容时候，包含unready pod），然后向上取整
	newReplicas := int32(math.Ceil(newUsageRatio * float64(len(metrics))))
	// 以下二种情况，不进行扩缩容 https://github.com/kubernetes/kubernetes/pull/89465 https://github.com/kubernetes/kubernetes/pull/85027
	// 修复metrics数据之后，仍然需要缩容。且最终的副本数大于当前副本数（滚动更新时候，（进行list）当前的副本数为workload.replicas，等获取metric时候pod数量变多（多生出的pod））
	// 修复metrics数据之后，仍然需要扩容。且最终的副本数小于当前副本数。（滚动更新时候，（进行list）当前的副本数，大于workload.replicas，等获取metric时候pod数量变为workload.replicas（变少））（scale.status.replicas跟workload.replicas什么关系？如果workload是deployment，则为deployment.Status.Replicas，在pkg\registry\apps\deployment\storage\storage.go scaleFromDeployment）
	// 返回当前副本数，修复metrics数据之前（只计算有监控数据且移除ignorePods（被删除或phase为Failed的pod）和unreadyPods）平均使用值
	if (newUsageRatio < 1.0 && newReplicas > currentReplicas) || (newUsageRatio > 1.0 && newReplicas < currentReplicas) {
		// return the current replicas if the change of metrics length would cause a change in scale direction
		return currentReplicas, utilization, nil
	}

	// return the result, where the number of replicas considered is
	// however many replicas factored into our calculation
	return newReplicas, utilization, nil
}

// GetObjectMetricReplicas calculates the desired replica count based on a target metric utilization (as a milli-value)
// for the given object in the given namespace, and the current replica count.
// 这里的objectRef为metricSpec.Object.DescribedObject
// 1.
// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
// 2.
// usageRatio为当前值/目标值
// usageRatio在[1-c.tolerance, 1+c.tolerance]范围内，就不进行扩缩容，返回现在的副本数
// 否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
// 当前的副本数为0，则最终副本数为usageRatio向上取整
// 返回最终的副本数，当前metrics值，空的timestamp，错误
func (c *ReplicaCalculator) GetObjectMetricReplicas(currentReplicas int32, targetUtilization int64, metricName string, namespace string, objectRef *autoscaling.CrossVersionObjectReference, selector labels.Selector, metricSelector labels.Selector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
	// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
	// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
	utilization, _, err = c.metricsClient.GetObjectMetric(metricName, namespace, objectRef, metricSelector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metric %s: %v on %s %s/%s", metricName, objectRef.Kind, namespace, objectRef.Name, err)
	}

	// usageRatio为当前值/目标值
	usageRatio := float64(utilization) / float64(targetUtilization)
	// usageRatio在[1-c.tolerance, 1+c.tolerance]范围内，就不进行扩缩容，返回现在的副本数
	// 否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
	// 当前的副本数为0，则为usageRatio向上取整
	replicaCount, timestamp, err = c.getUsageRatioReplicaCount(currentReplicas, usageRatio, namespace, selector)
	return replicaCount, utilization, timestamp, err
}

// getUsageRatioReplicaCount calculates the desired replica count based on usageRatio and ready pods count.
// For currentReplicas=0 doesn't take into account ready pods count and tolerance to support scaling to zero pods.
// usageRatio在[1-c.tolerance, 1+c.tolerance]范围内，就不进行扩缩容，返回现在的副本数，空时间
// 否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
// 当前的副本数为0，则为usageRatio向上取整
// 返回最终副本数和空时间
func (c *ReplicaCalculator) getUsageRatioReplicaCount(currentReplicas int32, usageRatio float64, namespace string, selector labels.Selector) (replicaCount int32, timestamp time.Time, err error) {
	if currentReplicas != 0 {
		// 默认c.tolerance为0.1，则usageRatio在0.9到1.1之间，就不进行扩缩容
		if math.Abs(1.0-usageRatio) <= c.tolerance {
			// return the current replicas if the change would be too small
			return currentReplicas, timestamp, nil
		}
		readyPodCount := int64(0)
		// 根据namespace和selector，过滤出所有ready的pod
		readyPodCount, err = c.getReadyPodsCount(namespace, selector)
		if err != nil {
			return 0, time.Time{}, fmt.Errorf("unable to calculate ready pods: %s", err)
		}
		// 最终副本数为usageRatio*readyPodCount，然后向上取整
		replicaCount = int32(math.Ceil(usageRatio * float64(readyPodCount)))
	} else {
		// Scale to zero or n pods depending on usageRatio
		// 当前的副本数为0，则为usageRatio向上取整
		replicaCount = int32(math.Ceil(usageRatio))
	}

	return replicaCount, timestamp, err
}

// GetObjectPerPodMetricReplicas calculates the desired replica count based on a target metric utilization (as a milli-value)
// for the given object in the given namespace, and the current replica count.
// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
// usageRatio是聚合值（所有pod的总值）/(目标平均利用率*副本数)
// 如果usageRatio不在[1-c.tolerance, 1+c.tolerance]范围内，则最终副本数为副本数为总使用的值/目标平均利用率，然后向上取整。否则，为现在副本数（不进行扩缩容）
// 现在的平均利用率是总使用的值/当前副本数
// 返回最终副本数、现在的平均利用率、metric的时间戳
func (c *ReplicaCalculator) GetObjectPerPodMetricReplicas(statusReplicas int32, targetAverageUtilization int64, metricName string, namespace string, objectRef *autoscaling.CrossVersionObjectReference, metricSelector labels.Selector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
	// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
	// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
	utilization, timestamp, err = c.metricsClient.GetObjectMetric(metricName, namespace, objectRef, metricSelector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get metric %s: %v on %s %s/%s", metricName, objectRef.Kind, namespace, objectRef.Name, err)
	}

	replicaCount = statusReplicas
	// utilization是聚合值（所有pod的总值）
	// usageRatio是聚合值/(目标平均利用率*副本数)
	usageRatio := float64(utilization) / (float64(targetAverageUtilization) * float64(replicaCount))
	// 不在[1-c.tolerance, 1+c.tolerance]范围内
	if math.Abs(1.0-usageRatio) > c.tolerance {
		// update number of replicas if change is large enough
		// 副本数为总使用的值/目标平均利用率，然后向上取整
		replicaCount = int32(math.Ceil(float64(utilization) / float64(targetAverageUtilization)))
	}
	// 现在的平均利用率是总使用的值/当前副本数
	utilization = int64(math.Ceil(float64(utilization) / float64(statusReplicas)))
	return replicaCount, utilization, timestamp, nil
}

// @TODO(mattjmcnaughton) Many different functions in this module use variations
// of this function. Make this function generic, so we don't repeat the same
// logic in multiple places.
// 根据namespace和selector，过滤出所有ready的pod
func (c *ReplicaCalculator) getReadyPodsCount(namespace string, selector labels.Selector) (int64, error) {
	podList, err := c.podLister.Pods(namespace).List(selector)
	if err != nil {
		return 0, fmt.Errorf("unable to get pods while calculating replica count: %v", err)
	}

	if len(podList) == 0 {
		return 0, fmt.Errorf("no pods returned by selector while calculating replica count")
	}

	readyPodCount := 0

	for _, pod := range podList {
		if pod.Status.Phase == v1.PodRunning && podutil.IsPodReady(pod) {
			readyPodCount++
		}
	}

	return int64(readyPodCount), nil
}

// GetExternalMetricReplicas calculates the desired replica count based on a
// target metric value (as a milli-value) for the external metric in the given
// namespace, and the current replica count.
// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{namespace}/{metricName}?labelSelector={selector}"
// 获得所有metrics值列表
// 对所有metrics值列表相加，计算总使用值
// usageRatio为总使用值/目标使用值
// usageRatio在[1-c.tolerance, 1+c.tolerance]范围内，就不进行扩缩容，最终副本数为现在的副本数
// 否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
// 当前的副本数为0，则为usageRatio向上取整
// 返回最终副本数，现在的总使用值，空时间
func (c *ReplicaCalculator) GetExternalMetricReplicas(currentReplicas int32, targetUtilization int64, metricName, namespace string, metricSelector *metav1.LabelSelector, podSelector labels.Selector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	metricLabelSelector, err := metav1.LabelSelectorAsSelector(metricSelector)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{namespace}/{metricName}?labelSelector={selector}"
	// 返回所有metrics值列表和第一个metric的timestamp
	metrics, _, err := c.metricsClient.GetExternalMetric(metricName, namespace, metricLabelSelector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get external metric %s/%s/%+v: %s", namespace, metricName, metricSelector, err)
	}
	utilization = 0
	// 计算总使用值
	for _, val := range metrics {
		utilization = utilization + val
	}

	// usageRatio为总使用值/目标使用值
	usageRatio := float64(utilization) / float64(targetUtilization)
	// usageRatio在[1-c.tolerance, 1+c.tolerance]范围内，就不进行扩缩容，返回现在的副本数，空时间
	// 否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
	// 当前的副本数为0，则为usageRatio向上取整
	// 返回最终副本数和空时间
	replicaCount, timestamp, err = c.getUsageRatioReplicaCount(currentReplicas, usageRatio, namespace, podSelector)
	return replicaCount, utilization, timestamp, err
}

// GetExternalPerPodMetricReplicas calculates the desired replica count based on a
// target metric value per pod (as a milli-value) for the external metric in the
// given namespace, and the current replica count.
// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{namespace}/{metricName}?labelSelector={selector}"
// 获得所有metrics值列表
// 对所有metrics值列表相加，计算总使用值
// usageRatio为总使用值/(目标平均使用值*当前pod数量)
// 当usageRatio不在[1-c.tolerance，1+c.tolerance]范围内，最终副本数为总使用值/目标平均使用值。否则最终副本数为当前副本数（不扩缩容）
// 平均使用值为总使用值/当前副本数
// 返回最终副本数，平均使用值，第一个metric的timestamp
func (c *ReplicaCalculator) GetExternalPerPodMetricReplicas(statusReplicas int32, targetUtilizationPerPod int64, metricName, namespace string, metricSelector *metav1.LabelSelector) (replicaCount int32, utilization int64, timestamp time.Time, err error) {
	metricLabelSelector, err := metav1.LabelSelectorAsSelector(metricSelector)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{namespace}/{metricName}?labelSelector={selector}"
	// 返回所有metrics值列表和第一个metric的timestamp
	metrics, timestamp, err := c.metricsClient.GetExternalMetric(metricName, namespace, metricLabelSelector)
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("unable to get external metric %s/%s/%+v: %s", namespace, metricName, metricSelector, err)
	}
	utilization = 0
	// 计算总使用值
	for _, val := range metrics {
		utilization = utilization + val
	}

	replicaCount = statusReplicas
	// usageRatio为总使用值/(目标平均使用值*当前pod数量)
	usageRatio := float64(utilization) / (float64(targetUtilizationPerPod) * float64(replicaCount))
	// usageRatio不在[1-c.tolerance，1+c.tolerance]范围内
	if math.Abs(1.0-usageRatio) > c.tolerance {
		// update number of replicas if the change is large enough
		// 最终副本数为 总使用值/目标平均使用值
		replicaCount = int32(math.Ceil(float64(utilization) / float64(targetUtilizationPerPod)))
	}
	// 平均使用值为总使用值/当前副本数
	utilization = int64(math.Ceil(float64(utilization) / float64(statusReplicas)))
	return replicaCount, utilization, timestamp, nil
}

func groupPods(pods []*v1.Pod, metrics metricsclient.PodMetricsInfo, resource v1.ResourceName, cpuInitializationPeriod, delayOfInitialReadinessStatus time.Duration) (readyPodCount int, unreadyPods, missingPods, ignoredPods sets.String) {
	missingPods = sets.NewString()
	unreadyPods = sets.NewString()
	ignoredPods = sets.NewString()
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil || pod.Status.Phase == v1.PodFailed {
			ignoredPods.Insert(pod.Name)
			continue
		}
		// Pending pods are ignored.
		if pod.Status.Phase == v1.PodPending {
			unreadyPods.Insert(pod.Name)
			continue
		}
		// Pods missing metrics.
		metric, found := metrics[pod.Name]
		if !found {
			missingPods.Insert(pod.Name)
			continue
		}
		// Unready pods are ignored.
		if resource == v1.ResourceCPU {
			var unready bool
			_, condition := podutil.GetPodCondition(&pod.Status, v1.PodReady)
			if condition == nil || pod.Status.StartTime == nil {
				unready = true
			} else {
				// Pod still within possible initialisation period.
				// cpuInitializationPeriod默认为5分钟
				// pod启动时间在cpuInitializationPeriod内
				if pod.Status.StartTime.Add(cpuInitializationPeriod).After(time.Now()) {
					// Ignore sample if pod is unready or one window of metric wasn't collected since last state transition.
					// ConditionFalse或metric是在readyCondition.LastTransitionTime加metric.Window之前（ready状态且metric计算周期在condition.LastTransitionTime之前）
					unready = condition.Status == v1.ConditionFalse || metric.Timestamp.Before(condition.LastTransitionTime.Time.Add(metric.Window))
				} else {
					// Ignore metric if pod is unready and it has never been ready.
					// delayOfInitialReadinessStatus默认为30秒
					// ConditionFalse且Status.StartTime加delayOfInitialReadinessStatus在condition.LastTransitionTime之后
					// 在delayOfInitialReadinessStatus内（相对Status.StartTime）ConditionFalse，则认为pod为unready
					unready = condition.Status == v1.ConditionFalse && pod.Status.StartTime.Add(delayOfInitialReadinessStatus).After(condition.LastTransitionTime.Time)
				}
			}
			if unready {
				unreadyPods.Insert(pod.Name)
				continue
			}
		}
		readyPodCount++
	}
	return
}

// 如果container == ""，统计所有pod里所有container的resource的request
// 如果container不为""，统计所有pod里container的resource的request
func calculatePodRequests(pods []*v1.Pod, container string, resource v1.ResourceName) (map[string]int64, error) {
	requests := make(map[string]int64, len(pods))
	for _, pod := range pods {
		podSum := int64(0)
		for _, c := range pod.Spec.Containers {
			if container == "" || container == c.Name {
				if containerRequest, ok := c.Resources.Requests[resource]; ok {
					podSum += containerRequest.MilliValue()
				} else {
					return nil, fmt.Errorf("missing request for %s", resource)
				}
			}
		}
		requests[pod.Name] = podSum
	}
	return requests, nil
}

func removeMetricsForPods(metrics metricsclient.PodMetricsInfo, pods sets.String) {
	for _, pod := range pods.UnsortedList() {
		delete(metrics, pod)
	}
}
