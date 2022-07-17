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

// Package config implements the pod configuration readers.
package config

import (
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
)

// SourcesReadyFn is function that returns true if the specified sources have been seen.
type SourcesReadyFn func(sourcesSeen sets.String) bool

// SourcesReady tracks the set of configured sources seen by the kubelet.
type SourcesReady interface {
	// AddSource adds the specified source to the set of sources managed.
	AddSource(source string)
	// AllReady returns true if the currently configured sources have all been seen.
	AllReady() bool
}

// NewSourcesReady returns a SourcesReady with the specified function.
func NewSourcesReady(sourcesReadyFn SourcesReadyFn) SourcesReady {
	return &sourcesImpl{
		sourcesSeen:    sets.NewString(),
		sourcesReadyFn: sourcesReadyFn,
	}
}

// sourcesImpl implements SourcesReady.  It is thread-safe.
type sourcesImpl struct {
	// lock protects access to sources seen.
	lock sync.RWMutex
	// set of sources seen.
	sourcesSeen sets.String
	// sourcesReady is a function that evaluates if the sources are ready.
	sourcesReadyFn SourcesReadyFn
}

// Add adds the specified source to the set of sources managed.
// 这个在kl.syncLoopIteration里会添加（只要kl.run(updates)里update通道收到PodUpdate消息（PodUpdate的op类型不是kubetypes.RESTORE））
func (s *sourcesImpl) AddSource(source string) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.sourcesSeen.Insert(source)
}

// AllReady returns true if each configured source is ready.
// s.seenSources里的所有源已经注册且都提供了至少一个pod
// s.seenSources里包含了kubeDeps.PodConfig.sources里的所有source（调用kubeDeps.PodConfig.Channel方法会添加source）
// 且kubeDeps.PodConfig.pods（保存各个源的pod集合）（每个源都发送至少一个SET消息）包含了kubeDeps.PodConfig.sources里的所有source
func (s *sourcesImpl) AllReady() bool {
	s.lock.RLock()
	defer s.lock.RUnlock()
	return s.sourcesReadyFn(s.sourcesSeen)
}
