/*
Copyright 2015 The Kubernetes Authors.

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
	"sync"
	"time"
)

var (
	// TODO(yifan): Maybe set the them as parameters for NewCache().
	defaultCachePeriod = time.Second * 2
)

type RuntimeCache interface {
	GetPods() ([]*Pod, error)
	ForceUpdateIfOlder(time.Time) error
}

type podsGetter interface {
	GetPods(bool) ([]*Pod, error)
}

// NewRuntimeCache creates a container runtime cache.
func NewRuntimeCache(getter podsGetter) (RuntimeCache, error) {
	return &runtimeCache{
		getter: getter,
	}, nil
}

// runtimeCache caches a list of pods. It records a timestamp (cacheTime) right
// before updating the pods, so the timestamp is at most as new as the pods
// (and can be slightly older). The timestamp always moves forward. Callers are
// expected not to modify the pods returned from GetPods.
type runtimeCache struct {
	sync.Mutex
	// The underlying container runtime used to update the cache.
	getter podsGetter
	// Last time when cache was updated.
	cacheTime time.Time
	// The content of the cache.
	pods []*Pod
}

// GetPods returns the cached pods if they are not outdated; otherwise, it
// retrieves the latest pods and return them.
// 缓存未过期，则返回缓存中（r.pods）的pod
// 缓存过期了，则从runtime中获得所有的running pod，并更新r.pods和r.cacheTime
func (r *runtimeCache) GetPods() ([]*Pod, error) {
	r.Lock()
	defer r.Unlock()
	// 如果之前从runtime获取的所有pods的时间，已经过去2秒
	// 从runtime中获得所有的running pod
	// 更新r.pods和r.cacheTime
	if time.Since(r.cacheTime) > defaultCachePeriod {
		if err := r.updateCache(); err != nil {
			return nil, err
		}
	}
	return r.pods, nil
}

func (r *runtimeCache) ForceUpdateIfOlder(minExpectedCacheTime time.Time) error {
	r.Lock()
	defer r.Unlock()
	if r.cacheTime.Before(minExpectedCacheTime) {
		return r.updateCache()
	}
	return nil
}

// 从runtime中获得所有的running pod，返回所有pods、当前时间、错误
// 更新r.pods和r.cacheTime
func (r *runtimeCache) updateCache() error {
	// 从runtime中获得所有的running pod，返回所有pods、当前时间、错误
	pods, timestamp, err := r.getPodsWithTimestamp()
	if err != nil {
		return err
	}
	// 更新r.pods和r.cacheTime
	r.pods, r.cacheTime = pods, timestamp
	return nil
}

// getPodsWithTimestamp records a timestamp and retrieves pods from the getter.
// 从runtime中获得所有的running pod，返回所有pods、当前时间、错误
func (r *runtimeCache) getPodsWithTimestamp() ([]*Pod, time.Time, error) {
	// Always record the timestamp before getting the pods to avoid stale pods.
	timestamp := time.Now()
	// r.getter在pkg\kubelet\kuberuntime\kuberuntime_manager.go里的kubeGenericRuntimeManager
	// 从runtime中获得所有的running pod
	pods, err := r.getter.GetPods(false)
	return pods, timestamp, err
}
