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

	"k8s.io/apimachinery/pkg/types"
)

// Cache stores the PodStatus for the pods. It represents *all* the visible
// pods/containers in the container runtime. All cache entries are at least as
// new or newer than the global timestamp (set by UpdateTime()), while
// individual entries may be slightly newer than the global timestamp. If a pod
// has no states known by the runtime, Cache returns an empty PodStatus object
// with ID populated.
//
// Cache provides two methods to retrieve the PodStatus: the non-blocking Get()
// and the blocking GetNewerThan() method. The component responsible for
// populating the cache is expected to call Delete() to explicitly free the
// cache entries.
type Cache interface {
	Get(types.UID) (*PodStatus, error)
	Set(types.UID, *PodStatus, error, time.Time)
	// GetNewerThan is a blocking call that only returns the status
	// when it is newer than the given time.
	GetNewerThan(types.UID, time.Time) (*PodStatus, error)
	Delete(types.UID)
	UpdateTime(time.Time)
}

type data struct {
	// Status of the pod.
	status *PodStatus
	// Error got when trying to inspect the pod.
	err error
	// Time when the data was last modified.
	modified time.Time
}

type subRecord struct {
	time time.Time
	ch   chan *data
}

// cache implements Cache.
type cache struct {
	// Lock which guards all internal data structures.
	lock sync.RWMutex
	// Map that stores the pod statuses.
	pods map[types.UID]*data
	// A global timestamp represents how fresh the cached data is. All
	// cache content is at the least newer than this timestamp. Note that the
	// timestamp is nil after initialization, and will only become non-nil when
	// it is ready to serve the cached statuses.
	timestamp *time.Time
	// Map that stores the subscriber records.
	subscribers map[types.UID][]*subRecord
}

// NewCache creates a pod cache.
func NewCache() Cache {
	return &cache{pods: map[types.UID]*data{}, subscribers: map[types.UID][]*subRecord{}}
}

// Get returns the PodStatus for the pod; callers are expected not to
// modify the objects returned.
// 返回缓存（c.pods）中的id的data里status，如果不在缓存中，则返回空的数据（只有pod id）
func (c *cache) Get(id types.UID) (*PodStatus, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	d := c.get(id)
	return d.status, d.err
}

// 尝试从缓存中获取pod的runtime status缓存，成功则直接返回pod的runtime status和是否inspect pod发生错误
// 否则添加一个订阅记录到c.subscribers[id]，等待chan中有数据，然后返回pod的runtime status和是否inspect pod发生错误
func (c *cache) GetNewerThan(id types.UID, minTime time.Time) (*PodStatus, error) {
	// 先从缓存中获取新鲜数据，如果成功将数据放到data chan中，然后返回chan。
	// 否则c.subscribers[id]列表中增加一个订阅记录，返回chan
	ch := c.subscribe(id, minTime)
	d := <-ch
	return d.status, d.err
}

// Set sets the PodStatus for the pod.
// 由PLEG调用set，添加pod status缓存
// 添加id的data到添加到c.pods中，并通知关注这个id的订阅者，发送缓存（c.pods）中的id的data，到订阅者的chan中。
func (c *cache) Set(id types.UID, status *PodStatus, err error, timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	// 通知关注这个id的订阅者，如果timestamp在订阅者需要的时间之后（满足订阅者需要），则发送缓存（c.pods）中的id的data，到订阅者的chan中。并从c.subscribers[id]的订阅者列表中移除这个订阅者
	// 否则，不通知订阅者
	defer c.notify(id, timestamp)
	// 添加到c.pods
	c.pods[id] = &data{status: status, err: err, modified: timestamp}
}

// Delete removes the entry of the pod.
// 从c.pods删除这个id记录
func (c *cache) Delete(id types.UID) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.pods, id)
}

//  UpdateTime modifies the global timestamp of the cache and notify
//  subscribers if needed.
// 更新c.timestamp为timestamp，并发生data到timestamp满足订阅者需要（还未过期）的订阅者的chan中
func (c *cache) UpdateTime(timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.timestamp = &timestamp
	// Notify all the subscribers if the condition is met.
	for id := range c.subscribers {
		// 通知关注这个id的订阅者，如果timestamp在订阅者需要的时间之后（满足订阅者需要），则发送缓存（c.pods）中的id的data，到订阅者的chan中。并从c.subscribers[id]的订阅者列表中移除这个订阅者
		// 否则，不通知订阅者
		c.notify(id, *c.timestamp)
	}
}

func makeDefaultData(id types.UID) *data {
	return &data{status: &PodStatus{ID: id}, err: nil}
}

// 返回缓存（c.pods）中的id的data，如果不在缓存中，则返回空的数据
func (c *cache) get(id types.UID) *data {
	// id不在c.pods中，则返回空的数据
	d, ok := c.pods[id]
	if !ok {
		// Cache should store *all* pod/container information known by the
		// container runtime. A cache miss indicates that there are no states
		// regarding the pod last time we queried the container runtime.
		// What this *really* means is that there are no visible pod/containers
		// associated with this pod. Simply return an default (mostly empty)
		// PodStatus to reflect this.
		return makeDefaultData(id)
	}
	return d
}

// getIfNewerThan returns the data it is newer than the given time.
// Otherwise, it returns nil. The caller should acquire the lock.
// 从缓存中获取比minTime晚的pod的runtime status
// 1. 缓存中不存在或缓存中数据过期，且缓存是新鲜的，则返回默认空的数据
// 2. 缓存中存在且是新鲜的，则返回缓存数据
// 3. 其他情况，返回nil
func (c *cache) getIfNewerThan(id types.UID, minTime time.Time) *data {
	d, ok := c.pods[id]
	globalTimestampIsNewer := (c.timestamp != nil && c.timestamp.After(minTime))
	// 缓存中不存在pod的runtime status且缓存生产时间在minTime的之后，返回空数据
	if !ok && globalTimestampIsNewer {
		// Status is not cached, but the global timestamp is newer than
		// minTime, return the default status.
		return makeDefaultData(id)
	}
	// 缓存中存在pod runtime status，且pod runtime status生成时间在miniTime之后或缓存生产时间在minTime的之后
	if ok && (d.modified.After(minTime) || globalTimestampIsNewer) {
		// Status is cached, return status if either of the following is true.
		//   * status was modified after minTime
		//   * the global timestamp of the cache is newer than minTime.
		return d
	}
	// The pod status is not ready.
	// 其他情况，pod runtime status都是未知状态
	// 缓存中不存在pod的runtime status且缓存生产时间在minTime的之前，或缓存中存在pod runtime status且pod runtime status生成时间在miniTime之前且在缓存生产时间在minTime的之前
	return nil
}

// notify sends notifications for pod with the given id, if the requirements
// are met. Note that the caller should acquire the lock.
// 通知关注这个id的订阅者，如果timestamp在订阅者需要的时间之后（满足订阅者需要），则发送缓存（c.pods）中的id的data，到订阅者的chan中。并从c.subscribers[id]的订阅者列表中移除这个订阅者
// 否则，不通知订阅者
func (c *cache) notify(id types.UID, timestamp time.Time) {
	// 根据id查找订阅者
	list, ok := c.subscribers[id]
	// 如果没有订阅者，直接返回
	if !ok {
		// No one to notify.
		return
	}
	newList := []*subRecord{}
	for i, r := range list {
		// 提供的timestamp在订阅者需要的时间之前（不满足订阅者需要），则append到newList
		if timestamp.Before(r.time) {
			// Doesn't meet the time requirement; keep the record.
			newList = append(newList, list[i])
			continue
		}
		// 返回缓存（c.pods）中的id的data，如果不在缓存中，则返回空的数据
		// 发送到订阅者的chan中，这个订阅者是c.GetNewerThan方法中
		r.ch <- c.get(id)
		close(r.ch)
	}
	// 满足所有订阅者需要，则从c.subscribers移除这个id
	if len(newList) == 0 {
		delete(c.subscribers, id)
	} else {
		// 还有不满足的订阅者，更新不满足的订阅者到c.subscribers[id]
		c.subscribers[id] = newList
	}
}

// 先从缓存中获取新鲜数据，如果成功将数据放到data chan中，然后返回chan。
// 否则c.subscribers[id]列表中增加一个订阅记录
func (c *cache) subscribe(id types.UID, timestamp time.Time) chan *data {
	ch := make(chan *data, 1)
	c.lock.Lock()
	defer c.lock.Unlock()
	// 从缓存中获取比minTime晚的pod的runtime status
	d := c.getIfNewerThan(id, timestamp)
	// 找到新鲜的数据，或在缓存是新鲜的前提下，未找到数据或找到数据是过期的
	if d != nil {
		// If the cache entry is ready, send the data and return immediately.
		ch <- d
		return ch
	}
	// Add the subscription record.
	// 缓存中未找到且缓存中数据是过期的，或缓存中找到且是过期的
	c.subscribers[id] = append(c.subscribers[id], &subRecord{time: timestamp, ch: ch})
	return ch
}
