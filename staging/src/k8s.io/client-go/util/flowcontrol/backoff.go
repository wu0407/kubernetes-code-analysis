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

package flowcontrol

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/utils/integer"
)

type backoffEntry struct {
	// 回退周期
	backoff    time.Duration
	// 回退周期开始时间点
	lastUpdate time.Time
}

// Backoff进行指数回退，在2倍最大回退周期之后重置回退周期为初始回退周期
type Backoff struct {
	sync.RWMutex
	Clock           clock.Clock
	// 初始回退周期
	defaultDuration time.Duration
	// 最大回退周期
	maxDuration     time.Duration
	perItemBackoff  map[string]*backoffEntry
}

func NewFakeBackOff(initial, max time.Duration, tc *clock.FakeClock) *Backoff {
	return &Backoff{
		perItemBackoff:  map[string]*backoffEntry{},
		Clock:           tc,
		defaultDuration: initial,
		maxDuration:     max,
	}
}

func NewBackOff(initial, max time.Duration) *Backoff {
	return &Backoff{
		perItemBackoff:  map[string]*backoffEntry{},
		Clock:           clock.RealClock{},
		defaultDuration: initial,
		maxDuration:     max,
	}
}

// Get the current backoff Duration
func (p *Backoff) Get(id string) time.Duration {
	p.RLock()
	defer p.RUnlock()
	var delay time.Duration
	entry, ok := p.perItemBackoff[id]
	if ok {
		delay = entry.backoff
	}
	return delay
}

// move backoff to the next mark, capping at maxDuration
//
// 根据eventTime，进行回退
// 未设置过回退或不在回退周期内且离开始回退时间已经过了2个最大的回退周期maxDuration，则新建一个回退backoffEntry
// 已在回退周期内则进行指数回退--但是不会超过最大回退周期maxDuration
func (p *Backoff) Next(id string, eventTime time.Time) {
	p.Lock()
	defer p.Unlock()
	entry, ok := p.perItemBackoff[id]
	// 不存在或不在回退周期内且离开始回退时间已经过了2个最大的回退周期maxDuration
	if !ok || hasExpired(eventTime, entry.lastUpdate, p.maxDuration) {
		entry = p.initEntryUnsafe(id)
	} else {
		// 在回退周期内，更新回退周期为原来的两倍
		delay := entry.backoff * 2 // exponential
		entry.backoff = time.Duration(integer.Int64Min(int64(delay), int64(p.maxDuration)))
	}
	// 从现在开始计算回退周期
	entry.lastUpdate = p.Clock.Now()
}

// Reset forces clearing of all backoff data for a given key.
func (p *Backoff) Reset(id string) {
	p.Lock()
	defer p.Unlock()
	delete(p.perItemBackoff, id)
}

// Returns True if the elapsed time since eventTime is smaller than the current backoff window
// 
// 以eventTime为起始点，现在是否在回退周期内
func (p *Backoff) IsInBackOffSince(id string, eventTime time.Time) bool {
	p.RLock()
	defer p.RUnlock()
	entry, ok := p.perItemBackoff[id]
	if !ok {
		return false
	}
	// 重置了回退周期--离开始回退时间已经过了2个最大的回退周期maxDuration
	if hasExpired(eventTime, entry.lastUpdate, p.maxDuration) {
		return false
	}
	// eventTime距现在时间是否小于回退周期
	// 以eventTime为起始点，现在是否在回退周期内
	return p.Clock.Since(eventTime) < entry.backoff
}

// Returns True if time since lastupdate is less than the current backoff window.
//
// 是否在回退周期内
func (p *Backoff) IsInBackOffSinceUpdate(id string, eventTime time.Time) bool {
	p.RLock()
	defer p.RUnlock()
	entry, ok := p.perItemBackoff[id]
	// 不存在--未设置过backoffEntry
	if !ok {
		return false
	}
	// 重置了回退周期--离开始回退时间已经过了2个最大的回退周期maxDuration
	if hasExpired(eventTime, entry.lastUpdate, p.maxDuration) {
		return false
	}
	// eventTime是否在回退周期内
	// eventTime时间距lastUpdate时间是否小于回退周期
	return eventTime.Sub(entry.lastUpdate) < entry.backoff
}

// Garbage collect records that have aged past maxDuration. Backoff users are expected
// to invoke this periodically.
func (p *Backoff) GC() {
	p.Lock()
	defer p.Unlock()
	now := p.Clock.Now()
	for id, entry := range p.perItemBackoff {
		if now.Sub(entry.lastUpdate) > p.maxDuration*2 {
			// GC when entry has not been updated for 2*maxDuration
			delete(p.perItemBackoff, id)
		}
	}
}

func (p *Backoff) DeleteEntry(id string) {
	p.Lock()
	defer p.Unlock()
	delete(p.perItemBackoff, id)
}

// Take a lock on *Backoff, before calling initEntryUnsafe
//
// 新建一个id相关的backoffEntry
func (p *Backoff) initEntryUnsafe(id string) *backoffEntry {
	entry := &backoffEntry{backoff: p.defaultDuration}
	p.perItemBackoff[id] = entry
	return entry
}

// After 2*maxDuration we restart the backoff factor to the beginning
//
// 检查eventTime时否在2倍maxDuration回退周期内
// 不在这个周期内说明回退周期需要重置
func hasExpired(eventTime time.Time, lastUpdate time.Time, maxDuration time.Duration) bool {
	return eventTime.Sub(lastUpdate) > maxDuration*2 // consider stable if it's ok for twice the maxDuration
}
