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
	"sort"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

const (
	// The limit on the number of buffered container deletion requests
	// This number is a bit arbitrary and may be adjusted in the future.
	containerDeletorBufferLimit = 50
)

type containerStatusbyCreatedList []*kubecontainer.ContainerStatus

type podContainerDeletor struct {
	worker           chan<- kubecontainer.ContainerID
	containersToKeep int
}

func (a containerStatusbyCreatedList) Len() int      { return len(a) }
func (a containerStatusbyCreatedList) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a containerStatusbyCreatedList) Less(i, j int) bool {
	return a[i].CreatedAt.After(a[j].CreatedAt)
}

func newPodContainerDeletor(runtime kubecontainer.Runtime, containersToKeep int) *podContainerDeletor {
	buffer := make(chan kubecontainer.ContainerID, containerDeletorBufferLimit)
	go wait.Until(func() {
		for {
			id := <-buffer
			// 调用runtime manager，pkg\kubelet\kuberuntime\kuberuntime_container.go里的kubeGenericRuntimeManager，删除container
			runtime.DeleteContainer(id)
		}
	}, 0, wait.NeverStop)

	return &podContainerDeletor{
		worker:           buffer,
		containersToKeep: containersToKeep,
	}
}

// getContainersToDeleteInPod returns the exited containers in a pod whose name matches the name inferred from filterContainerId (if not empty), ordered by the creation time from the latest to the earliest.
// If filterContainerID is empty, all dead containers in the pod are returned.
// 找到podStatus里所有container status里的status为"exited"的container status
// 如果有filterContainerID过滤的container，则还要匹配过滤的container
// 返回保留最近containersToKeep个后剩余的container status（按照创建时间倒序排列）
func getContainersToDeleteInPod(filterContainerID string, podStatus *kubecontainer.PodStatus, containersToKeep int) containerStatusbyCreatedList {
	// 从podStatus.ContainerStatuses里找到filterContainerId的container status
	matchedContainer := func(filterContainerId string, podStatus *kubecontainer.PodStatus) *kubecontainer.ContainerStatus {
		if filterContainerId == "" {
			return nil
		}
		for _, containerStatus := range podStatus.ContainerStatuses {
			if containerStatus.ID.ID == filterContainerId {
				return containerStatus
			}
		}
		return nil
	}(filterContainerID, podStatus)

	// filterContainerID不为空，但是matchedContainer为nil，说明没有找到这个filterContainerID的container status，直接返回空的containerStatusbyCreatedList
	if filterContainerID != "" && matchedContainer == nil {
		klog.Warningf("Container %q not found in pod's containers", filterContainerID)
		return containerStatusbyCreatedList{}
	}

	// Find the exited containers whose name matches the name of the container with id being filterContainerId
	var candidates containerStatusbyCreatedList
	// 找到所有container status里的status为"exited"的container status
	// 如果有过滤的container，则还要匹配过滤的container
	for _, containerStatus := range podStatus.ContainerStatuses {
		if containerStatus.State != kubecontainer.ContainerStateExited {
			continue
		}
		if matchedContainer == nil || matchedContainer.Name == containerStatus.Name {
			candidates = append(candidates, containerStatus)
		}
	}

	// 筛选处理的container status的数量小于containersToKeep，则直接返回空的containerStatusbyCreatedList
	if len(candidates) <= containersToKeep {
		return containerStatusbyCreatedList{}
	}
	// 根据container的创建时间倒序排
	sort.Sort(candidates)
	// 返回保留最近containersToKeep个后剩余的container status
	return candidates[containersToKeep:]
}

// deleteContainersInPod issues container deletion requests for containers selected by getContainersToDeleteInPod.
// 找到podStatus里所有container status里的status为"exited"的container status
// 如果有filterContainerID过滤的container，则还要匹配过滤的container
// 返回保留最近containersToKeep个后剩余的container status（按照创建时间倒序排列）
// 发送所有container给p.worker通道，让goroutine消费这个通道进行移除这个container
func (p *podContainerDeletor) deleteContainersInPod(filterContainerID string, podStatus *kubecontainer.PodStatus, removeAll bool) {
	// 默认为1
	containersToKeep := p.containersToKeep
	// 如果移除所有，则保留container数量为0
	if removeAll {
		containersToKeep = 0
		filterContainerID = ""
	}

	// 找到podStatus里所有container status里的status为"exited"的container status
	// 如果有filterContainerID过滤的container，则还要匹配过滤的container
	// 返回保留最近containersToKeep个后剩余的container status（按照创建时间倒序排列）
	for _, candidate := range getContainersToDeleteInPod(filterContainerID, podStatus, containersToKeep) {
		select {
		case p.worker <- candidate.ID:
		default:
			klog.Warningf("Failed to issue the request to remove container %v", candidate.ID)
		}
	}
}
