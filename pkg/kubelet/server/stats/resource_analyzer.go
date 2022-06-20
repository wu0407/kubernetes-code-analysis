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

package stats

import (
	"time"
)

// ResourceAnalyzer provides statistics on node resource consumption
type ResourceAnalyzer interface {
	Start()

	fsResourceAnalyzerInterface
	SummaryProvider
}

// resourceAnalyzer implements ResourceAnalyzer
type resourceAnalyzer struct {
	*fsResourceAnalyzer
	SummaryProvider
}

var _ ResourceAnalyzer = &resourceAnalyzer{}

// NewResourceAnalyzer returns a new ResourceAnalyzer
func NewResourceAnalyzer(statsProvider Provider, calVolumeFrequency time.Duration) ResourceAnalyzer {
	fsAnalyzer := newFsResourceAnalyzer(statsProvider, calVolumeFrequency)
	summaryProvider := NewSummaryProvider(statsProvider)
	return &resourceAnalyzer{fsAnalyzer, summaryProvider}
}

// Start starts background functions necessary for the ResourceAnalyzer to function
// 启动一个goroutine，周期性更新ra.fsResourceAnalyzer.cachedVolumeStats
// ra.fsResourceAnalyzer.cachedVolumeStats保存了volumeStatCalculator集合，用于计算kubelet上所有pod的所有volume状态，volumeStatCalculator用于生成并保存pod的volume状态。
// 每个pod会启动一个goroutine，周期性获得pod各个volume的目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free，并将状态保存到volumeStatCalculator.latest
func (ra *resourceAnalyzer) Start() {
	ra.fsResourceAnalyzer.Start()
}
