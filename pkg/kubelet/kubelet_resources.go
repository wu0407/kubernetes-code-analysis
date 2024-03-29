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

package kubelet

import (
	"fmt"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/api/v1/resource"
)

// defaultPodLimitsForDownwardAPI copies the input pod, and optional container,
// and applies default resource limits. it returns a copy of the input pod,
// and a copy of the input container (if specified) with default limits
// applied. if a container has no limit specified, it will default the limit to
// the node allocatable.
// TODO: if/when we have pod level resources, we need to update this function
// to use those limits instead of node allocatable.
// 设置pod里所有普通container的resource limit和参数里的container的resource limit，如果container某种resource没有设置limit，则设置为node allocatable里对应resource的值
func (kl *Kubelet) defaultPodLimitsForDownwardAPI(pod *v1.Pod, container *v1.Container) (*v1.Pod, *v1.Container, error) {
	if pod == nil {
		return nil, nil, fmt.Errorf("invalid input, pod cannot be nil")
	}

	// 先从informer中获取本机的node对象，成功则返回 否则返回kl.initialNode手动生成的初始的node对象
	node, err := kl.getNodeAnyWay()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find node object, expected a node")
	}
	allocatable := node.Status.Allocatable
	klog.Infof("allocatable: %v", allocatable)
	outputPod := pod.DeepCopy()
	for idx := range outputPod.Spec.Containers {
		// 设置container的resource limit，如果container某种resource没有设置limit，则设置为node allocatable里对应resource的值
		resource.MergeContainerResourceLimits(&outputPod.Spec.Containers[idx], allocatable)
	}

	var outputContainer *v1.Container
	if container != nil {
		outputContainer = container.DeepCopy()
		resource.MergeContainerResourceLimits(outputContainer, allocatable)
	}
	return outputPod, outputContainer, nil
}
