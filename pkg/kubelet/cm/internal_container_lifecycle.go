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

package cm

import (
	"k8s.io/api/core/v1"

	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
)

type InternalContainerLifecycle interface {
	PreStartContainer(pod *v1.Pod, container *v1.Container, containerID string) error
	PreStopContainer(containerID string) error
	PostStopContainer(containerID string) error
}

// Implements InternalContainerLifecycle interface.
type internalContainerLifecycleImpl struct {
	cpuManager      cpumanager.Manager
	topologyManager topologymanager.Manager
}

// 调用cri更新容器的cpuset cgroup
// 添加container id到i.topologyManager.podMap
func (i *internalContainerLifecycleImpl) PreStartContainer(pod *v1.Pod, container *v1.Container, containerID string) error {
	if i.cpuManager != nil {
		// 调用cri更新容器的cpuset cgroup，如果发生错误则移除分配的container（从i.cpuManager.state.assignments移除container所占有的cpu，并在i.cpuManager.state（stateMemory）中defaultCPUSet添加这个cpu集合（共享的cpu集合），i.cpuManager.containerMap中移除这个container）
		// 之前在pod admit时候已经，分配了cpu，所以这里是只是更新容器的cpuset
		err := i.cpuManager.AddContainer(pod, container, containerID)
		if err != nil {
			return err
		}
	}
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.TopologyManager) {
		// 添加container id到i.topologyManager.podMap
		err := i.topologyManager.AddContainer(pod, containerID)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *internalContainerLifecycleImpl) PreStopContainer(containerID string) error {
	return nil
}

// 释放container ID在i.topologyManager.podTopologyHints分配资源
func (i *internalContainerLifecycleImpl) PostStopContainer(containerID string) error {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.TopologyManager) {
		// 清理container ID在i.topologyManager.podTopologyHints分配资源
		err := i.topologyManager.RemoveContainer(containerID)
		if err != nil {
			return err
		}
	}
	return nil
}
