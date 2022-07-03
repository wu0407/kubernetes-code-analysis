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

package preemption

import (
	"fmt"
	"math"

	"k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

const message = "Preempted in order to admit critical pod"

// CriticalPodAdmissionHandler is an AdmissionFailureHandler that handles admission failure for Critical Pods.
// If the ONLY admission failures are due to insufficient resources, then CriticalPodAdmissionHandler evicts pods
// so that the critical pod can be admitted.  For evictions, the CriticalPodAdmissionHandler evicts a set of pods that
// frees up the required resource requests.  The set of pods is designed to minimize impact, and is prioritized according to the ordering:
// minimal impact for guaranteed pods > minimal impact for burstable pods > minimal impact for besteffort pods.
// minimal impact is defined as follows: fewest pods evicted > fewest total requests of pods.
// finding the fewest total requests of pods is considered besteffort.
type CriticalPodAdmissionHandler struct {
	getPodsFunc eviction.ActivePodsFunc
	killPodFunc eviction.KillPodFunc
	recorder    record.EventRecorder
}

var _ lifecycle.AdmissionFailureHandler = &CriticalPodAdmissionHandler{}

func NewCriticalPodAdmissionHandler(getPodsFunc eviction.ActivePodsFunc, killPodFunc eviction.KillPodFunc, recorder record.EventRecorder) *CriticalPodAdmissionHandler {
	return &CriticalPodAdmissionHandler{
		getPodsFunc: getPodsFunc,
		killPodFunc: killPodFunc,
		recorder:    recorder,
	}
}

// HandleAdmissionFailure gracefully handles admission rejection, and, in some cases,
// to allow admission of the pod despite its previous failure.
// pod不是static pod、不是mirror pod，且pod优先级小于2000000000，则直接返回失败原因
// 如果存在非资源不满足原因导致的Predicate失败，直接返回非资源不满足原因
// 只有资源不满足原因导致的Predicate失败，根据最优抢占算法和pod qos筛选出被抢占的pod，然后删除这些pod
func (c *CriticalPodAdmissionHandler) HandleAdmissionFailure(admitPod *v1.Pod, failureReasons []lifecycle.PredicateFailureReason) ([]lifecycle.PredicateFailureReason, error) {
	// pod不是static pod、不是mirror pod，且pod优先级小于2000000000，则直接返回
	if !kubetypes.IsCriticalPod(admitPod) {
		return failureReasons, nil
	}
	// InsufficientResourceError is not a reason to reject a critical pod.
	// Instead of rejecting, we free up resources to admit it, if no other reasons for rejection exist.
	nonResourceReasons := []lifecycle.PredicateFailureReason{}
	resourceReasons := []*admissionRequirement{}
	for _, reason := range failureReasons {
		// 如果是资源不满足原因导致的Predicate失败，则加入到resourceReasons
		if r, ok := reason.(*lifecycle.InsufficientResourceError); ok {
			resourceReasons = append(resourceReasons, &admissionRequirement{
				resourceName: r.ResourceName,
				// 还需要多少个资源，才能满足
				quantity:     r.GetInsufficientAmount(),
			})
		} else {
			nonResourceReasons = append(nonResourceReasons, reason)
		}
	}
	// 如果存在非资源不满足原因导致的Predicate失败，直接返回非资源不满足原因
	if len(nonResourceReasons) > 0 {
		// Return only reasons that are not resource related, since critical pods cannot fail admission for resource reasons.
		return nonResourceReasons, nil
	}
	// 只有资源不满足原因导致的Predicate失败
	// 根据最优抢占算法和pod qos筛选出被抢占的pod，然后删除这些pod
	err := c.evictPodsToFreeRequests(admitPod, admissionRequirementList(resourceReasons))
	// if no error is returned, preemption succeeded and the pod is safe to admit.
	return nil, err
}

// evictPodsToFreeRequests takes a list of insufficient resources, and attempts to free them by evicting pods
// based on requests.  For example, if the only insufficient resource is 200Mb of memory, this function could
// evict a pod with request=250Mb.
// 根据最优抢占算法和pod qos筛选出被抢占的pod，然后删除这些pod
func (c *CriticalPodAdmissionHandler) evictPodsToFreeRequests(admitPod *v1.Pod, insufficientResources admissionRequirementList) error {
	// 最优抢占算法，比如pod request分配为1，2，3，4，5，6，这时候需要6，则只选择为6的pod（不会选择1，2，3或2，4），最小影响原则
	// 根据pod qos进行抢占，最先抢占besteffort，然后burstable, 最后guaranteed
	// 1. 先对所有pod进行模拟执行抢占，检查所有pod抢占后的资源能否满足需要
	// 2. 进行besteffort、burstable pod抢占后的，对还不满足的资源admissionRequirement列表。对guaranteed pod选择最优的（总距离最短，最小满足）一些pod进行抢占
	// 3. 进行besteffort、guaranteedToEvict（上面筛选后） pod抢占后，对还不满足的资源admissionRequirement列表，burstable pod选择最优的（总距离最短，最小满足）一些pod进行抢占
	// 4. 进行burstableToEvict、guaranteedToEvict pod抢占后，对还不满足的资源admissionRequirement列表，besteffort pod选择最优的（总距离最短，最小满足）一些pod进行抢占
	podsToPreempt, err := getPodsToPreempt(admitPod, c.getPodsFunc(), insufficientResources)
	if err != nil {
		return fmt.Errorf("preemption: error finding a set of pods to preempt: %v", err)
	}
	klog.Infof("preemption: attempting to evict pods %v, in order to free up resources: %s", podsToPreempt, insufficientResources.toString())
	for _, pod := range podsToPreempt {
		status := v1.PodStatus{
			Phase:   v1.PodFailed,
			Message: message,
			Reason:  events.PreemptContainer,
		}
		// record that we are evicting the pod
		c.recorder.Eventf(pod, v1.EventTypeWarning, events.PreemptContainer, message)
		// this is a blocking call and should only return when the pod and its containers are killed.
		// 进行pod删除
		err := c.killPodFunc(pod, status, nil)
		if err != nil {
			klog.Warningf("preemption: pod %s failed to evict %v", format.Pod(pod), err)
			// In future syncPod loops, the kubelet will retry the pod deletion steps that it was stuck on.
			continue
		}
		if len(insufficientResources) > 0 {
			metrics.Preemptions.WithLabelValues(insufficientResources[0].resourceName.String()).Inc()
		} else {
			metrics.Preemptions.WithLabelValues("").Inc()
		}
		klog.Infof("preemption: pod %s evicted successfully", format.Pod(pod))
	}
	return nil
}

// getPodsToPreempt returns a list of pods that could be preempted to free requests >= requirements
// 最优抢占算法，比如pod request分配为1，2，3，4，5，6，这时候需要6，则只选择为6的pod（不会选择1，2，3或2，4），最小影响原则
// 根据pod qos进行抢占，最先抢占besteffort，然后burstable, 最后guaranteed
// 1. 先对所有pod进行模拟执行抢占，检查所有pod抢占后的资源能否满足需要
// 2. 进行besteffort、burstable pod抢占后的，对还不满足的资源admissionRequirement列表。对guaranteed pod选择最优的（总距离最短，最小满足）一些pod进行抢占
// 3. 进行besteffort、guaranteedToEvict（上面筛选后） pod抢占后，对还不满足的资源admissionRequirement列表，burstable pod选择最优的（总距离最短，最小满足）一些pod进行抢占
// 4. 进行burstableToEvict、guaranteedToEvict pod抢占后，对还不满足的资源admissionRequirement列表，besteffort pod选择最优的（总距离最短，最小满足）一些pod进行抢占
func getPodsToPreempt(pod *v1.Pod, pods []*v1.Pod, requirements admissionRequirementList) ([]*v1.Pod, error) {
	// 返回所有能够被抢占的所有pod，按照qos进行分类后的besteffort, burstable, and guaranteed的pod
	bestEffortPods, burstablePods, guaranteedPods := sortPodsByQOS(pod, pods)

	// make sure that pods exist to reclaim the requirements
	// 最先抢占besteffort，然后burstable, 最后guaranteed
	// 遍历admissionRequirementList，对admissionRequirement进行遍历所有pod，模拟执行抢占，返回抢占后，还不满足的资源admissionRequirement列表
	unableToMeetRequirements := requirements.subtract(append(append(bestEffortPods, burstablePods...), guaranteedPods...)...)
	// 还有不满足的资源admissionRequirement，则直接返回错误
	if len(unableToMeetRequirements) > 0 {
		return nil, fmt.Errorf("no set of running pods found to reclaim resources: %v", unableToMeetRequirements.toString())
	}
	// find the guaranteed pods we would need to evict if we already evicted ALL burstable and besteffort pods.
	// 进行besteffort、burstable pod抢占后的，对还不满足的资源admissionRequirement列表。对guaranteed pod选择最优的（总距离最短，最小满足）一些pod进行抢占
	guaranteedToEvict, err := getPodsToPreemptByDistance(guaranteedPods, requirements.subtract(append(bestEffortPods, burstablePods...)...))
	if err != nil {
		return nil, err
	}
	// Find the burstable pods we would need to evict if we already evicted ALL besteffort pods, and the required guaranteed pods.
	// 进行besteffort、guaranteedToEvict（上面筛选后） pod抢占后，对还不满足的资源admissionRequirement列表，burstable pod选择最优的（总距离最短，最小满足）一些pod进行抢占
	burstableToEvict, err := getPodsToPreemptByDistance(burstablePods, requirements.subtract(append(bestEffortPods, guaranteedToEvict...)...))
	if err != nil {
		return nil, err
	}
	// Find the besteffort pods we would need to evict if we already evicted the required guaranteed and burstable pods.
	// 进行burstableToEvict、guaranteedToEvict pod抢占后，对还不满足的资源admissionRequirement列表，besteffort pod选择最优的（总距离最短，最小满足）一些pod进行抢占
	bestEffortToEvict, err := getPodsToPreemptByDistance(bestEffortPods, requirements.subtract(append(burstableToEvict, guaranteedToEvict...)...))
	if err != nil {
		return nil, err
	}
	return append(append(bestEffortToEvict, burstableToEvict...), guaranteedToEvict...), nil
}

// getPodsToPreemptByDistance finds the pods that have pod requests >= admission requirements.
// Chooses pods that minimize "distance" to the requirements.
// If more than one pod exists that fulfills the remaining requirements,
// it chooses the pod that has the "smaller resource request"
// This method, by repeatedly choosing the pod that fulfills as much of the requirements as possible,
// attempts to minimize the number of pods returned.
// 选择总距离最小pods（最小尽可能满足所有admissionRequirement）
func getPodsToPreemptByDistance(pods []*v1.Pod, requirements admissionRequirementList) ([]*v1.Pod, error) {
	podsToEvict := []*v1.Pod{}
	// evict pods by shortest distance from remaining requirements, updating requirements every round.
	// 这里requirements最后一定小于等于0，这个由调度器决定的，超出node allocatable pod不会调度到这个节点。requirements最大的情况是node allocatable（所有pod占了node allocatable资源）
	for len(requirements) > 0 {
		if len(pods) == 0 {
			return nil, fmt.Errorf("no set of running pods found to reclaim resources: %v", requirements.toString())
		}
		// all distances must be less than len(requirements), because the max distance for a single requirement is 1
		// 最差的distance为len(requirements)，为了所有pod的distance都小于这个值（第一次循环中选择第一个pod），所以是最差的distance加1
		bestDistance := float64(len(requirements) + 1)
		bestPodIndex := 0
		// Find the pod with the smallest distance from requirements
		// Or, in the case of two equidistant pods, find the pod with "smaller" resource requests.
		for i, pod := range pods {
			// 遍历requirements里的每个admissionRequirement，计算dist
			// 如果pod的资源request不能满足admissionRequirement需要的资源，则dist距离= dist距离+（还需要的资源除以admissionRequirement需要的资源）^2
			dist := requirements.distance(pod)
			// smallerResourceRequest
			// 根据"memory"、"cpu"资源，如果pod1的request小于pod2的request小，返回true。
			// 如果"memory"、"cpu"资源的request都相等返回true
			//
			// 距离小于bestDistance（发现更小request的pod）或bestDistance一样且request的资源更小，则更新bestDistance为这个pod的dist，bestPodIndex为pod的index
			if dist < bestDistance || (bestDistance == dist && smallerResourceRequest(pod, pods[bestPodIndex])) {
				bestDistance = dist
				bestPodIndex = i
			}
		}
		// subtract the pod from requirements, and transfer the pod from input-pods to pods-to-evicted
		// 遍历requirements，对admissionRequirement进行遍历所有pod，模拟执行抢占，返回抢占后，还不满足的资源admissionRequirement列表
		requirements = requirements.subtract(pods[bestPodIndex])
		podsToEvict = append(podsToEvict, pods[bestPodIndex])
		// 最优的pod替换成最后一个pod
		pods[bestPodIndex] = pods[len(pods)-1]
		// pods列表里去除最后一个pod
		pods = pods[:len(pods)-1]
	}
	return podsToEvict, nil
}

type admissionRequirement struct {
	resourceName v1.ResourceName
	quantity     int64
}

type admissionRequirementList []*admissionRequirement

// distance returns distance of the pods requests from the admissionRequirements.
// The distance is measured by the fraction of the requirement satisfied by the pod,
// so that each requirement is weighted equally, regardless of absolute magnitude.
// 每个admissionRequirement的最大的距离为1
// 遍历每个admissionRequirement
// 如果pod的资源request不能满足admissionRequirement需要的资源，则dist距离= dist距离+（还需要的资源除以admissionRequirement需要的资源）^2
func (a admissionRequirementList) distance(pod *v1.Pod) float64 {
	dist := float64(0)
	// 每个admissionRequirement需要的资源，减去pod的里资源的request，如果还大于0，则计算距离
	for _, req := range a {
		// 还需要的资源等于admissionRequirement需要的资源，减去pod的里资源的request
		remainingRequest := float64(req.quantity - resource.GetResourceRequest(pod, req.resourceName))
		// 还需要的资源大于0，dist距离= dist距离+（还需要的资源除以admissionRequirement需要的资源）^2
		if remainingRequest > 0 {
			dist += math.Pow(remainingRequest/float64(req.quantity), 2)
		}
	}
	return dist
}

// subtract returns a new admissionRequirementList containing remaining requirements if the provided pod
// were to be preempted
// 遍历admissionRequirementList，对admissionRequirement进行遍历所有pod，模拟执行抢占，返回抢占后，还不满足的资源admissionRequirement列表
func (a admissionRequirementList) subtract(pods ...*v1.Pod) admissionRequirementList {
	newList := []*admissionRequirement{}
	for _, req := range a {
		newQuantity := req.quantity
		for _, pod := range pods {
			// newQuantity（还需要的资源数量）减去，pod里的resource资源的request值
			newQuantity -= resource.GetResourceRequest(pod, req.resourceName)
			// 减完之后需要资源小于0，说明已经找到足够的pod，能够腾出资源，满足需要的资源
			if newQuantity <= 0 {
				break
			}
		}
		// 遍历所有pod之后需要资源还是大于0，则说明没有足够的pod，能够腾出资源，满足需要的资源
		// 则append还不满足的资源的admissionRequirement
		if newQuantity > 0 {
			newList = append(newList, &admissionRequirement{
				resourceName: req.resourceName,
				// 最后还需要的资源数量
				quantity:     newQuantity,
			})
		}
	}
	return newList
}

func (a admissionRequirementList) toString() string {
	s := "["
	for _, req := range a {
		s += fmt.Sprintf("(res: %v, q: %d), ", req.resourceName, req.quantity)
	}
	return s + "]"
}

// sortPodsByQOS returns lists containing besteffort, burstable, and guaranteed pods that
// can be preempted by preemptor pod.
// 返回所有能够被抢占的所有pod，按照qos进行分类后的besteffort, burstable, and guaranteed的pod
func sortPodsByQOS(preemptor *v1.Pod, pods []*v1.Pod) (bestEffort, burstable, guaranteed []*v1.Pod) {
	for _, pod := range pods {
		// preemptor是CriticalPod，且pod不是IsCriticalPod，直接返回true
		// preemptor的优先级大于pod的优先级，返回true
		if kubetypes.Preemptable(preemptor, pod) {
			switch v1qos.GetPodQOS(pod) {
			case v1.PodQOSBestEffort:
				bestEffort = append(bestEffort, pod)
			case v1.PodQOSBurstable:
				burstable = append(burstable, pod)
			case v1.PodQOSGuaranteed:
				guaranteed = append(guaranteed, pod)
			default:
			}
		}
	}

	return
}

// smallerResourceRequest returns true if pod1 has a smaller request than pod2
// 根据"memory"、"cpu"资源，如果pod1的request小于pod2的request小，返回true。
// 如果"memory"、"cpu"资源的request都相等返回true
func smallerResourceRequest(pod1 *v1.Pod, pod2 *v1.Pod) bool {
	priorityList := []v1.ResourceName{
		v1.ResourceMemory,
		v1.ResourceCPU,
	}
	for _, res := range priorityList {
		req1 := resource.GetResourceRequest(pod1, res)
		req2 := resource.GetResourceRequest(pod2, res)
		if req1 < req2 {
			return true
		} else if req1 > req2 {
			return false
		}
	}
	return true
}
