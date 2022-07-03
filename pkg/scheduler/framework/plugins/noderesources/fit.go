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

package noderesources

import (
	"context"
	"fmt"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/features"
	framework "k8s.io/kubernetes/pkg/scheduler/framework/v1alpha1"
	schedulernodeinfo "k8s.io/kubernetes/pkg/scheduler/nodeinfo"
)

var _ framework.PreFilterPlugin = &Fit{}
var _ framework.FilterPlugin = &Fit{}

const (
	// FitName is the name of the plugin used in the plugin registry and configurations.
	FitName = "NodeResourcesFit"

	// preFilterStateKey is the key in CycleState to NodeResourcesFit pre-computed data.
	// Using the name of the plugin will likely help us avoid collisions with other plugins.
	preFilterStateKey = "PreFilter" + FitName
)

// Fit is a plugin that checks if a node has sufficient resources.
type Fit struct {
	ignoredResources sets.String
}

// FitArgs holds the args that are used to configure the plugin.
type FitArgs struct {
	// IgnoredResources is the list of resources that NodeResources fit filter
	// should ignore.
	IgnoredResources []string `json:"ignoredResources,omitempty"`
}

// preFilterState computed at PreFilter and used at Filter.
type preFilterState struct {
	schedulernodeinfo.Resource
}

// Clone the prefilter state.
func (s *preFilterState) Clone() framework.StateData {
	return s
}

// Name returns name of the plugin. It is used in logs, etc.
func (f *Fit) Name() string {
	return FitName
}

// computePodResourceRequest returns a schedulernodeinfo.Resource that covers the largest
// width in each resource dimension. Because init-containers run sequentially, we collect
// the max in each dimension iteratively. In contrast, we sum the resource vectors for
// regular containers since they run simultaneously.
//
// If Pod Overhead is specified and the feature gate is set, the resources defined for Overhead
// are added to the calculated Resource request sum
//
// Example:
//
// Pod:
//   InitContainers
//     IC1:
//       CPU: 2
//       Memory: 1G
//     IC2:
//       CPU: 2
//       Memory: 3G
//   Containers
//     C1:
//       CPU: 2
//       Memory: 1G
//     C2:
//       CPU: 1
//       Memory: 1G
//
// Result: CPU: 3, Memory: 3G
// 返回的结果为
// 首先，pod中普通container的request资源进行累加
// 然后，普通container总的request资源与所有initContainer中最大的request资源
// 最后，如果启用"PodOverhead"功能，overhead资源进行累加
func computePodResourceRequest(pod *v1.Pod) *preFilterState {
	result := &preFilterState{}
	// pod中普通container的request资源进行累加
	for _, container := range pod.Spec.Containers {
		result.Add(container.Resources.Requests)
	}

	// take max_resource(sum_pod, any_init_container)
	// 普通container总的request资源与所有initContainer中最大的request资源
	for _, container := range pod.Spec.InitContainers {
		result.SetMaxResource(container.Resources.Requests)
	}

	// If Overhead is being utilized, add to the total requests for the pod
	// 如果启用"PodOverhead"功能，overhead资源进行累加
	if pod.Spec.Overhead != nil && utilfeature.DefaultFeatureGate.Enabled(features.PodOverhead) {
		result.Add(pod.Spec.Overhead)
	}

	return result
}

// PreFilter invoked at the prefilter extension point.
func (f *Fit) PreFilter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod) *framework.Status {
	cycleState.Write(preFilterStateKey, computePodResourceRequest(pod))
	return nil
}

// PreFilterExtensions returns prefilter extensions, pod add and remove.
func (f *Fit) PreFilterExtensions() framework.PreFilterExtensions {
	return nil
}

func getPreFilterState(cycleState *framework.CycleState) (*preFilterState, error) {
	c, err := cycleState.Read(preFilterStateKey)
	if err != nil {
		// preFilterState doesn't exist, likely PreFilter wasn't invoked.
		return nil, fmt.Errorf("error reading %q from cycleState: %v", preFilterStateKey, err)
	}

	s, ok := c.(*preFilterState)
	if !ok {
		return nil, fmt.Errorf("%+v  convert to NodeResourcesFit.preFilterState error", c)
	}
	return s, nil
}

// Filter invoked at the filter extension point.
// Checks if a node has sufficient resources, such as cpu, memory, gpu, opaque int resources etc to run a pod.
// It returns a list of insufficient resources, if empty, then the node has all the resources requested by the pod.
func (f *Fit) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *schedulernodeinfo.NodeInfo) *framework.Status {
	s, err := getPreFilterState(cycleState)
	if err != nil {
		return framework.NewStatus(framework.Error, err.Error())
	}

	insufficientResources := fitsRequest(s, nodeInfo, f.ignoredResources)

	if len(insufficientResources) != 0 {
		// We will keep all failure reasons.
		failureReasons := make([]string, 0, len(insufficientResources))
		for _, r := range insufficientResources {
			failureReasons = append(failureReasons, r.Reason)
		}
		return framework.NewStatus(framework.Unschedulable, failureReasons...)
	}
	return nil
}

// InsufficientResource describes what kind of resource limit is hit and caused the pod to not fit the node.
type InsufficientResource struct {
	ResourceName v1.ResourceName
	// We explicitly have a parameter for reason to avoid formatting a message on the fly
	// for common resources, which is expensive for cluster autoscaler simulations.
	Reason    string
	Requested int64
	Used      int64
	Capacity  int64
}

// Fits checks if node have enough resources to host the pod.
// 检测node可以分配的资源是否可以满足，pod的request资源里的cpu、memory、EphemeralStorage、ScalarResources
func Fits(pod *v1.Pod, nodeInfo *schedulernodeinfo.NodeInfo, ignoredExtendedResources sets.String) []InsufficientResource {
	// computePodResourceRequest(pod)返回的结果为
	// 首先，pod中普通container的request资源进行累加
	// 然后，普通container总的request资源与所有initContainer中最大的request资源
	// 最后，如果启用"PodOverhead"功能，overhead资源进行累加
	//
	// 检测node可以分配的资源是否可以满足，pod的request资源里的cpu、memory、EphemeralStorage、ScalarResources
	return fitsRequest(computePodResourceRequest(pod), nodeInfo, ignoredExtendedResources)
}

// 检测node可以分配的资源是否可以满足，pod的request资源里的cpu、memory、EphemeralStorage、ScalarResources
func fitsRequest(podRequest *preFilterState, nodeInfo *schedulernodeinfo.NodeInfo, ignoredExtendedResources sets.String) []InsufficientResource {
	insufficientResources := make([]InsufficientResource, 0, 4)

	allowedPodNumber := nodeInfo.AllowedPodNumber()
	// node上已经有的pod数量加上1（待分配的pod）大于node最大允许的pod数量，则不满足原因是pod数量
	if len(nodeInfo.Pods())+1 > allowedPodNumber {
		insufficientResources = append(insufficientResources, InsufficientResource{
			v1.ResourcePods,
			"Too many pods",
			1,
			int64(len(nodeInfo.Pods())),
			int64(allowedPodNumber),
		})
	}

	if ignoredExtendedResources == nil {
		ignoredExtendedResources = sets.NewString()
	}

	// 如果pod总的request资源里的cpu、memory、EphemeralStorage、ScalarResources（扩展资源和非扩展资源）都为0，则直接返回
	if podRequest.MilliCPU == 0 &&
		podRequest.Memory == 0 &&
		podRequest.EphemeralStorage == 0 &&
		len(podRequest.ScalarResources) == 0 {
		return insufficientResources
	}

	allocatable := nodeInfo.AllocatableResource()
	// node里的allocatable（可以分配）的cpu，小于pod总的request cpu加上已经分配的cpu数量，说明cpu资源不满足
	if allocatable.MilliCPU < podRequest.MilliCPU+nodeInfo.RequestedResource().MilliCPU {
		insufficientResources = append(insufficientResources, InsufficientResource{
			v1.ResourceCPU,
			"Insufficient cpu",
			podRequest.MilliCPU,
			nodeInfo.RequestedResource().MilliCPU,
			allocatable.MilliCPU,
		})
	}
	// node里的allocatable（可以分配）的memory，小于pod总的request memory加上已经分配的memory数量，说明memory资源不满足
	if allocatable.Memory < podRequest.Memory+nodeInfo.RequestedResource().Memory {
		insufficientResources = append(insufficientResources, InsufficientResource{
			v1.ResourceMemory,
			"Insufficient memory",
			podRequest.Memory,
			nodeInfo.RequestedResource().Memory,
			allocatable.Memory,
		})
	}
	// node里的allocatable（可以分配）的ephemeral-storage，小于pod总的request ephemeral-storage加上已经分配的ephemeral-storage数量，说明ephemeral-storage资源不满足
	if allocatable.EphemeralStorage < podRequest.EphemeralStorage+nodeInfo.RequestedResource().EphemeralStorage {
		insufficientResources = append(insufficientResources, InsufficientResource{
			v1.ResourceEphemeralStorage,
			"Insufficient ephemeral-storage",
			podRequest.EphemeralStorage,
			nodeInfo.RequestedResource().EphemeralStorage,
			allocatable.EphemeralStorage,
		})
	}

	for rName, rQuant := range podRequest.ScalarResources {
		if v1helper.IsExtendedResourceName(rName) {
			// If this resource is one of the extended resources that should be
			// ignored, we will skip checking it.
			// 如果request资源是扩展资源，且在ignoredExtendedResources里，则跳过
			if ignoredExtendedResources.Has(string(rName)) {
				continue
			}
		}
		// node里的allocatable（可以分配）的ScalarResources里的资源类型数量，小于pod总的request ScalarResources里的资源类型数量，加上已经分配的ScalarResources里的资源类型数量，说明这个ScalarResources里的资源不满足
		if allocatable.ScalarResources[rName] < rQuant+nodeInfo.RequestedResource().ScalarResources[rName] {
			insufficientResources = append(insufficientResources, InsufficientResource{
				rName,
				fmt.Sprintf("Insufficient %v", rName),
				podRequest.ScalarResources[rName],
				nodeInfo.RequestedResource().ScalarResources[rName],
				allocatable.ScalarResources[rName],
			})
		}
	}

	return insufficientResources
}

// NewFit initializes a new plugin and returns it.
func NewFit(plArgs *runtime.Unknown, _ framework.FrameworkHandle) (framework.Plugin, error) {
	args := &FitArgs{}
	if err := framework.DecodeInto(plArgs, args); err != nil {
		return nil, err
	}

	fit := &Fit{}
	fit.ignoredResources = sets.NewString(args.IgnoredResources...)
	return fit, nil
}
