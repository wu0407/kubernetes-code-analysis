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
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/klog"
)

type statCache map[types.UID]*volumeStatCalculator

// fsResourceAnalyzerInterface is for embedding fs functions into ResourceAnalyzer
type fsResourceAnalyzerInterface interface {
	GetPodVolumeStats(uid types.UID) (PodVolumeStats, bool)
}

// fsResourceAnalyzer provides stats about fs resource usage
type fsResourceAnalyzer struct {
	statsProvider     Provider
	calcPeriod        time.Duration
	cachedVolumeStats atomic.Value
	startOnce         sync.Once
}

var _ fsResourceAnalyzerInterface = &fsResourceAnalyzer{}

// newFsResourceAnalyzer returns a new fsResourceAnalyzer implementation
func newFsResourceAnalyzer(statsProvider Provider, calcVolumePeriod time.Duration) *fsResourceAnalyzer {
	r := &fsResourceAnalyzer{
		statsProvider: statsProvider,
		calcPeriod:    calcVolumePeriod,
	}
	r.cachedVolumeStats.Store(make(statCache))
	return r
}

// Start eager background caching of volume stats.
// 启动一个goroutine，周期性更新s.cachedVolumeStats
// s.cachedVolumeStats保存了volumeStatCalculator集合，用于计算kubelet上所有pod的所有volume状态，volumeStatCalculator用于生成并保存pod的volume状态。
// 每个pod会启动一个goroutine，周期性获得pod各个volume的目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free，并将状态保存到volumeStatCalculator.latest
func (s *fsResourceAnalyzer) Start() {
	s.startOnce.Do(func() {
		if s.calcPeriod <= 0 {
			klog.Info("Volume stats collection disabled.")
			return
		}
		klog.Info("Starting FS ResourceAnalyzer")
		// 更新s.cachedVolumeStats保存了volumeStatCalculator集合，用于计算kubelet上所有pod的所有volume状态，volumeStatCalculator用于生成并保存pod的volume状态。
		// 每个pod会启动一个goroutine，周期性获得pod各个volume的目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free，并将状态保存到volumeStatCalculator.latest
		go wait.Forever(func() { s.updateCachedPodVolumeStats() }, s.calcPeriod)
	})
}

// updateCachedPodVolumeStats calculates and caches the PodVolumeStats for every Pod known to the kubelet.
// 更新s.cachedVolumeStats，s.cachedVolumeStats保存了volumeStatCalculator集合。volumeStatCalculator用于计算kubelet上所有pod的所有volume状态，保存pod的volume状态的对象。
// 每个pod会启动一个goroutine，周期性获得pod各个volume的目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free，并将状态保存到volumeStatCalculator.latest
func (s *fsResourceAnalyzer) updateCachedPodVolumeStats() {
	oldCache := s.cachedVolumeStats.Load().(statCache)
	newCache := make(statCache)

	// Copy existing entries to new map, creating/starting new entries for pods missing from the cache
	// statsProvider一般是kubelet
	// 所有普通pod和static pod，如果static pod，则从status manager中获取最新状态赋值给pod的Status字段
	for _, pod := range s.statsProvider.GetPods() {
		// 没有在老的cache中，则添加到newCache中
		if value, found := oldCache[pod.GetUID()]; !found {
			// 启动一个goroutine，周期性获得pod各个volume的目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free，并将状态保存到volumeStatCalculator.latest，返回volumeStatCalculator
			newCache[pod.GetUID()] = newVolumeStatCalculator(s.statsProvider, s.calcPeriod, pod).StartOnce()
		} else {
			// 在老的cache中，则赋值给newCache
			newCache[pod.GetUID()] = value
		}
	}

	// Stop entries for pods that have been deleted
	// pod如果delete，则停止周期性获得pod各个volume的状态的goroutine
	for uid, entry := range oldCache {
		if _, found := newCache[uid]; !found {
			entry.StopOnce()
		}
	}

	// Update the cache reference
	// 将新的cache保存到s.cachedVolumeStats
	s.cachedVolumeStats.Store(newCache)
}

// GetPodVolumeStats returns the PodVolumeStats for a given pod.  Results are looked up from a cache that
// is eagerly populated in the background, and never calculated on the fly.
// 从s.cachedVolumeStats获得最新的statCache（所有pod与对应的保存pod的所有volume状态的volumeStatCalculator）
// 从statCache中获取保存pod的所有volume状态的volumeStatCalculator
// 从statCalc.latest里获取PodVolumeStats（pod各个volume的获取目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free）
func (s *fsResourceAnalyzer) GetPodVolumeStats(uid types.UID) (PodVolumeStats, bool) {
	// 从s.cachedVolumeStats获得最新的statCache（所有pod与对应的保存pod的所有volume状态的volumeStatCalculator）
	cache := s.cachedVolumeStats.Load().(statCache)
	// 从statCache中获取保存pod的所有volume状态的volumeStatCalculator
	statCalc, found := cache[uid]
	if !found {
		// TODO: Differentiate between stats being empty
		// See issue #20679
		return PodVolumeStats{}, false
	}
	// 从statCalc.latest里获取PodVolumeStats（pod各个volume的获取目录使用量，inode使用量，文件系统的available bytes, byte capacity,total inodes, inodes free）
	// 状态分为两类EphemeralVolumes（EmptyDir且EmptyDir底层存储不是内存、ConfigMap、GitRepo）和PersistentVolumes
	return statCalc.GetLatest()
}
