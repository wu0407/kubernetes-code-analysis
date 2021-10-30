// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"sort"
	"time"
)

type timedStoreDataSlice []timedStoreData

func (t timedStoreDataSlice) Less(i, j int) bool {
	return t[i].timestamp.Before(t[j].timestamp)
}

func (t timedStoreDataSlice) Len() int {
	return len(t)
}

func (t timedStoreDataSlice) Swap(i, j int) {
	t[i], t[j] = t[j], t[i]
}

// A time-based buffer for ContainerStats.
// Holds information for a specific time period and/or a max number of items.
type TimedStore struct {
	buffer   timedStoreDataSlice
	age      time.Duration
	maxItems int
}

type timedStoreData struct {
	timestamp time.Time
	data      interface{}
}

// Returns a new thread-compatible TimedStore.
// A maxItems value of -1 means no limit.
func NewTimedStore(age time.Duration, maxItems int) *TimedStore {
	return &TimedStore{
		buffer:   make(timedStoreDataSlice, 0),
		age:      age,
		maxItems: maxItems,
	}
}

// Adds an element to the start of the buffer (removing one from the end if necessary).
func (self *TimedStore) Add(timestamp time.Time, item interface{}) {
	data := timedStoreData{
		timestamp: timestamp,
		data:      item,
	}
	// Common case: data is added in order.
	// 队列为空或输入的timestamp不在最后一个元素的时间之前，则直接append到self.buffer
	if len(self.buffer) == 0 || !timestamp.Before(self.buffer[len(self.buffer)-1].timestamp) {
		self.buffer = append(self.buffer, data)
	} else {
		// Data is out of order; insert it in the correct position.
		// 数据插入到正确的位置

		// 在self.buffer中找到在输入timestamp之后的第一个元素的index
		index := sort.Search(len(self.buffer), func(index int) bool {
			return self.buffer[index].timestamp.After(timestamp)
		})
		// 尾部填充一个空的元素
		self.buffer = append(self.buffer, timedStoreData{}) // Make room to shift the elements
		// 从index元素到倒数第二个元素（倒数第一个是空元素），往后移一位（index到了index+1），这时self.buffer里self.buffer[index]与self.buffer[index+1]相等
		copy(self.buffer[index+1:], self.buffer[index:])    // Shift the elements over
		// index元素被覆盖为data
		self.buffer[index] = data
	}

	// Remove any elements before eviction time.
	// TODO(rjnagal): This is assuming that the added entry has timestamp close to now.
	// 移除过期的元素
	// 计算过期截止时间
	evictTime := timestamp.Add(-self.age)
	// 找到第一个未过期元素的编号
	index := sort.Search(len(self.buffer), func(index int) bool {
		return self.buffer[index].timestamp.After(evictTime)
	})
	// 如果找到未过期元素（未找到过期元素 index等于len(self.buffer)），则设置self.buffer为未过期元素集合
	if index < len(self.buffer) {
		self.buffer = self.buffer[index:]
	}

	// Remove any elements if over our max size.
	// 定义了maxItems（不小于0）且当前元素个数大于maxItems，则进行从头部开始移除元素。self.maxItems小于0，代表无限制
	if self.maxItems >= 0 && len(self.buffer) > self.maxItems {
		// 计算第一个不被移除的编号
		startIndex := len(self.buffer) - self.maxItems
		// 重新设置新的self.buffer（所有没有被移除元素）
		self.buffer = self.buffer[startIndex:]
	}
}

// Returns up to maxResult elements in the specified time period (inclusive).
// Results are from first to last. maxResults of -1 means no limit.
func (self *TimedStore) InTimeRange(start, end time.Time, maxResults int) []interface{} {
	// No stats, return empty.
	if len(self.buffer) == 0 {
		return []interface{}{}
	}

	// 倒序编号
	var startIndex int
	if start.IsZero() {
		// None specified, start at the beginning.
		// 由于是倒序查找，这里代表第一个元素
		startIndex = len(self.buffer) - 1
	} else {
		// Start is the index before the elements smaller than it. We do this by
		// finding the first element smaller than start and taking the index
		// before that element
		// 从尾部开始查找第一个在start之前的元素，这里会再减一，因为要取这个找到元素（第一个在start之前的元素）的之前一个元素（倒序查找的之前，等同于正序的之后，这个元素一定大于等于start）
		startIndex = sort.Search(len(self.buffer), func(index int) bool {
			// buffer[index] < start
			return self.getData(index).timestamp.Before(start)
		}) - 1
		// Check if start is after all the data we have.
		// 当startIndex为0的时候（代表最后一个元素）找到第一个在start之前的元素，即start在所有元素之后，返回空
		if startIndex < 0 {
			return []interface{}{}
		}
	}

	// 倒序编号
	var endIndex int
	if end.IsZero() {
		// None specified, end with the latest stats.
		// 由于是倒序查找，这里代表最后一个元素
		endIndex = 0
	} else {
		// End is the first index smaller than or equal to it (so, not larger).
		// 从尾部开始查找，找到第一个不在end之后的第一个元素（代表小于或等于end）
		endIndex = sort.Search(len(self.buffer), func(index int) bool {
			// buffer[index] <= t -> !(buffer[index] > t)
			return !self.getData(index).timestamp.After(end)
		})
		// Check if end is before all the data we have.
		// 当查找不到第一个不在end之后的第一个元素的时候（endIndex等于len(self.buffer)），即end在所有元素之前，返回空
		if endIndex == len(self.buffer) {
			return []interface{}{}
		}
	}

	// Trim to maxResults size.
	// startIndex一定大于等于endIndex（传入参数要保证start <= end，且元素是有序）
	numResults := startIndex - endIndex + 1
	// 限制了结果数量（maxResults不等于-1）且得到结果元素的数量大于限制
	if maxResults != -1 && numResults > maxResults {
		// startIndex等于startIndex减去多出来的数量（倒序查找，向后面移动多出来的数量，startIndex变小）
		startIndex -= numResults - maxResults
		numResults = maxResults
	}

	// Return in sorted timestamp order so from the "back" to "front".
	// 由于倒序查找，取startIndex 到 startIndex-(numResults-1)编号（倒序的编号）的元素
	result := make([]interface{}, numResults)
	for i := 0; i < numResults; i++ {
		result[i] = self.Get(startIndex - i)
	}
	return result
}

// Gets the element at the specified index. Note that elements are output in LIFO order.
// 获得倒序index编号的元素
func (self *TimedStore) Get(index int) interface{} {
	return self.getData(index).data
}

// Gets the data at the specified index. Note that elements are output in LIFO order.
// 0编号代表尾部第一个元素，倒序查找
func (self *TimedStore) getData(index int) timedStoreData {
	// 倒序编号转成正序编号查找
	return self.buffer[len(self.buffer)-index-1]
}

func (self *TimedStore) Size() int {
	return len(self.buffer)
}
