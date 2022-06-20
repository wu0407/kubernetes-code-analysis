/*
Copyright 2014 The Kubernetes Authors.

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

package container

import (
	"fmt"
	"time"

	"k8s.io/klog"
)

// Specified a policy for garbage collecting containers.
type ContainerGCPolicy struct {
	// Minimum age at which a container can be garbage collected, zero for no limit.
	MinAge time.Duration

	// Max number of dead containers any single pod (UID, container name) pair is
	// allowed to have, less than zero for no limit.
	MaxPerPodContainer int

	// Max number of total dead containers, less than zero for no limit.
	MaxContainers int
}

// Manages garbage collection of dead containers.
//
// Implementation is thread-compatible.
type ContainerGC interface {
	// Garbage collect containers.
	GarbageCollect() error
	// Deletes all unused containers, including containers belonging to pods that are terminated but not deleted
	DeleteAllUnusedContainers() error
}

// SourcesReadyProvider knows how to determine if configuration sources are ready
type SourcesReadyProvider interface {
	// AllReady returns true if the currently configured sources have all been seen.
	AllReady() bool
}

// TODO(vmarmol): Preferentially remove pod infra containers.
type realContainerGC struct {
	// Container runtime
	runtime Runtime

	// Policy for garbage collection.
	policy ContainerGCPolicy

	// sourcesReadyProvider provides the readiness of kubelet configuration sources.
	sourcesReadyProvider SourcesReadyProvider
}

// New ContainerGC instance with the specified policy.
func NewContainerGC(runtime Runtime, policy ContainerGCPolicy, sourcesReadyProvider SourcesReadyProvider) (ContainerGC, error) {
	if policy.MinAge < 0 {
		return nil, fmt.Errorf("invalid minimum garbage collection age: %v", policy.MinAge)
	}

	return &realContainerGC{
		runtime:              runtime,
		policy:               policy,
		sourcesReadyProvider: sourcesReadyProvider,
	}, nil
}

func (cgc *realContainerGC) GarbageCollect() error {
	return cgc.runtime.GarbageCollect(cgc.policy, cgc.sourcesReadyProvider.AllReady(), false)
}

// 移除可以移除的container
// 获取非running container且container createdAt时间距离现在已经经过minAge的container
// allSourcesReady为true，则移除所有可以驱逐的container
// 根据gcPolicy.MaxPerPodContainer，移除每个pod里老的可以移除的container
// 根据gcPolicy.MaxContainers，移除pod里或所有container里老的可以移除的container
//
// 移除可以移除的sandbox
// 如果pod删除了或pod处于Terminated状态，则移除所有不为running且没有container属于它的sandbox容器
// 如果pod没有被删除了，且pod不处于Terminated状态，则保留pod的最近一个sanbox容器（移除其他老的且不为running且没有container属于它的pod）
//
// 移除pod日志目录
// allSourcesReady为true，且pod（pod uid从目录名中提取）不为删除状态，则递归移除目录"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"
// "/var/log/containers/*.log"文件，如果链接的目标文件不存在，则移除这个链接
func (cgc *realContainerGC) DeleteAllUnusedContainers() error {
	klog.Infof("attempting to delete unused containers")
	return cgc.runtime.GarbageCollect(cgc.policy, cgc.sourcesReadyProvider.AllReady(), true)
}
