// Copyright 2015 Google Inc. All Rights Reserved.
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

// Utility methods to calculate percentiles.

package summary

import (
	"fmt"
	"math"
	"sort"

	info "github.com/google/cadvisor/info/v2"
)

const secondsToMilliSeconds = 1000
const milliSecondsToNanoSeconds = 1000000
const secondsToNanoSeconds = secondsToMilliSeconds * milliSecondsToNanoSeconds

type Uint64Slice []uint64

func (a Uint64Slice) Len() int           { return len(a) }
func (a Uint64Slice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a Uint64Slice) Less(i, j int) bool { return a[i] < a[j] }

// Get percentile of the provided samples. Round to integer.
func (self Uint64Slice) GetPercentile(d float64) uint64 {
	if d < 0.0 || d > 1.0 {
		return 0
	}
	count := self.Len()
	if count == 0 {
		return 0
	}
	sort.Sort(self)
	// 算出需要的是第几个的值，向上取整，比如0.9 * 50 +1= 46.0
	n := float64(d * (float64(count) + 1))
	idx, frac := math.Modf(n)
	index := int(idx)
	percentile := float64(self[index-1])
	// 整数部分不是第一个或最后一个
	if index > 1 && index < count {
		// 要加上小数位的值（小数位乘以后一个与这一个之间的差值）
		percentile += frac * float64(self[index]-self[index-1])
	}
	return uint64(percentile)
}

type mean struct {
	// current count.
	count uint64
	// current mean.
	Mean float64
}

// self.count加1，计算新的平均值self.Mean
func (self *mean) Add(value uint64) {
	self.count++
	if self.count == 1 {
		self.Mean = float64(value)
		return
	}
	c := float64(self.count)
	v := float64(value)
	// 计算新的平均值，等于原来的count乘以self.Mean加上新加的value，除以现在的count
	self.Mean = (self.Mean*(c-1) + v) / c
}

type resource struct {
	// list of samples being tracked.
	samples Uint64Slice
	// average from existing samples.
	mean mean
	// maximum value seen so far in the added samples.
	max uint64
}

// Adds a new percentile sample.
func (self *resource) Add(p info.Percentiles) {
	if !p.Present {
		return
	}
	// 新的最大值记录，记录到self.max
	if p.Max > self.max {
		self.max = p.Max
	}
	// self.count加1，计算新的平均值self.Mean
	self.mean.Add(p.Mean)
	// Selecting 90p of 90p :(
	// 90分位值添加到样本列表中
	self.samples = append(self.samples, p.Ninety)
}

// Add a single sample. Internally, we convert it to a fake percentile sample.
// 添加新的sample到self.sample，有新的最大值记录则更新self.max和计算最新的self.Mean
func (self *resource) AddSample(val uint64) {
	sample := info.Percentiles{
		Present:    true,
		Mean:       val,
		Max:        val,
		Fifty:      val,
		Ninety:     val,
		NinetyFive: val,
	}
	// 如果找到新的最大值，则更新self.max
	// self.count加1，计算新的平均值self.Mean
	// 添加90分位值到self.samples样本列表中
	self.Add(sample)
}

// Get max, average, and 90p from existing samples.
// 获得所有self.samples的平均值、最大值、0.5分位值、0.9分位值、0.95分位值
func (self *resource) GetAllPercentiles() info.Percentiles {
	p := info.Percentiles{}
	p.Mean = uint64(self.mean.Mean)
	p.Max = self.max
	// 获得self.samples列表中0.5分位的值
	p.Fifty = self.samples.GetPercentile(0.5)
	// 获得self.samples列表中0.9分位的值
	p.Ninety = self.samples.GetPercentile(0.9)
	// 获得self.samples列表中0.95分位的值
	p.NinetyFive = self.samples.GetPercentile(0.95)
	p.Present = true
	return p
}

func NewResource(size int) *resource {
	return &resource{
		samples: make(Uint64Slice, 0, size),
		mean:    mean{count: 0, Mean: 0},
	}
}

// Return aggregated percentiles from the provided percentile samples.
// 返回所有stats数据中的cpu和memory的聚合值（平均值、最大值、0.5分位值、0.9分位值、0.95分位值）
func GetDerivedPercentiles(stats []*info.Usage) info.Usage {
	cpu := NewResource(len(stats))
	memory := NewResource(len(stats))
	for _, stat := range stats {
		cpu.Add(stat.Cpu)
		memory.Add(stat.Memory)
	}
	usage := info.Usage{}
	usage.Cpu = cpu.GetAllPercentiles()
	usage.Memory = memory.GetAllPercentiles()
	return usage
}

// Calculate part of a minute this sample set represent.
// 计算stats里的样本的时间跨度占一分钟的百分比--样本的饱和度（是不是头尾间隔60s）
func getPercentComplete(stats []*secondSample) (percent int32) {
	numSamples := len(stats)
	if numSamples > 1 {
		percent = 100
		timeRange := stats[numSamples-1].Timestamp.Sub(stats[0].Timestamp).Nanoseconds()
		// allow some slack
		if timeRange < 58*secondsToNanoSeconds {
			percent = int32((timeRange * 100) / 60 * secondsToNanoSeconds)
		}
	}
	return
}

// Calculate cpurate from two consecutive total cpu usage samples.
func getCpuRate(latest, previous secondSample) (uint64, error) {
	var elapsed int64
	elapsed = latest.Timestamp.Sub(previous.Timestamp).Nanoseconds()
	// 间隔小于10ms，直接返回0
	if elapsed < 10*milliSecondsToNanoSeconds {
		return 0, fmt.Errorf("elapsed time too small: %d ns: time now %s last %s", elapsed, latest.Timestamp.String(), previous.Timestamp.String())
	}
	if latest.Cpu < previous.Cpu {
		return 0, fmt.Errorf("bad sample: cumulative cpu usage dropped from %d to %d", latest.Cpu, previous.Cpu)
	}
	// Cpurate is calculated in cpu-milliseconds per second.
	cpuRate := (latest.Cpu - previous.Cpu) * secondsToMilliSeconds / uint64(elapsed)
	return cpuRate, nil
}

// Returns a percentile sample for a minute by aggregating seconds samples.
// 获得一分钟的聚合值，cpu和memory（平均值、最大值、0.5分位值、0.9分位值、0.95分位值）和样本时间覆盖率
func GetMinutePercentiles(stats []*secondSample) info.Usage {
	// 先前sample
	lastSample := secondSample{}
	// 包含sample列表和平均值
	cpu := NewResource(len(stats))
	memory := NewResource(len(stats))
	for _, stat := range stats {
		// 先前的sample不为空，则多添加cpu使用情况
		if !lastSample.Timestamp.IsZero() {
			// 计算cpu使用率，通过现在sample与先前的sample
			cpuRate, err := getCpuRate(*stat, lastSample)
			if err != nil {
				continue
			}
			// 添加新的sample样本（90分位值）到self.sample，有新的最大值记录则更新self.max和计算最新的self.Mean
			cpu.AddSample(cpuRate)
			memory.AddSample(stat.Memory)
		} else {
			memory.AddSample(stat.Memory)
		}
		// 设置现在的sample为下一个循环的先前的sample
		lastSample = *stat
	}
	// 计算stats里的样本的时间跨度占一分钟的百分比--样本的饱和度（时间覆盖率，是不是头尾间隔60s）
	percent := getPercentComplete(stats)
	return info.Usage{
		PercentComplete: percent,
		// 获得cpu的平均值、最大值、0.5分位值、0.9分位值、0.95分位值
		Cpu:             cpu.GetAllPercentiles(),
		// 获得memory的平均值、最大值、0.5分位值、0.9分位值、0.95分位值
		Memory:          memory.GetAllPercentiles(),
	}
}
