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

// Maintains the summary of aggregated minute, hour, and day stats.
// For a container running for more than a day, amount of tracked data can go up to
// 40 KB when cpu and memory are tracked. We'll start by enabling collection for the
// node, followed by docker, and then all containers as we understand the usage pattern
// better
// TODO(rjnagal): Optimize the size if we start running it for every container.
package summary

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/cadvisor/info/v1"
	info "github.com/google/cadvisor/info/v2"
)

// Usage fields we track for generating percentiles.
type secondSample struct {
	Timestamp time.Time // time when the sample was recorded.
	Cpu       uint64    // cpu usage
	Memory    uint64    // memory usage
}

type availableResources struct {
	Cpu    bool
	Memory bool
}

type StatsSummary struct {
	// Resources being tracked for this container.
	available availableResources
	// list of second samples. The list is cleared when a new minute samples is generated.
	secondSamples []*secondSample
	// minute percentiles. We track 24 * 60 maximum samples.
	minuteSamples *SamplesBuffer
	// latest derived instant, minute, hour, and day stats. Instant sample updated every second.
	// Others updated every minute.
	derivedStats info.DerivedStats // Guarded by dataLock.
	dataLock     sync.RWMutex
}

// Adds a new seconds sample.
// If enough seconds samples are collected, a minute sample is generated and derived
// stats are updated.
// 从stat中获取cpu的Total值和memory的WorkingSet值，添加到s.secondSamples列表中，并维护s.secondSamples列表，保证头尾的样本点的时间间隔不大于60s（大于60s，清理secondSamples列表，只保留最后一个secondSample）
// 更新最近的cpu（使用率）和memory使用情况，设置s.derivedStats.LatestUsage和s.derivedStats.Timestamp
// 头尾的样本点的时间间隔大于60s，则从s.secondSamples中聚合出一个minuteSample，将这个minuteSample添加到s.minuteSamples中。更新s.derivedStats中HourUsage、DayUsage，cpu（利用率）和memory（当前值）小时聚合值和天的聚合值
func (s *StatsSummary) AddSample(stat v1.ContainerStats) error {
	sample := secondSample{}
	sample.Timestamp = stat.Timestamp
	if s.available.Cpu {
		// stat.Cpu.Usage.Total累加值，在cpuacct cgroup里的cpuacct.usage文件的值
		sample.Cpu = stat.Cpu.Usage.Total
	}
	if s.available.Memory {
		// 这个是实时值，在memory cgroup里的 memory.usage_in_bytes 减去 memory.stat里total_inactive_file
		sample.Memory = stat.Memory.WorkingSet
	}
	// 添加到secondSamples列表中
	s.secondSamples = append(s.secondSamples, &sample)
	// 设置最近的cpu（使用率）和memory使用情况和最近的时间点
	// 设置s.derivedStats.LatestUsage和s.derivedStats.Timestamp
	s.updateLatestUsage()
	// TODO(jnagal): Use 'available' to avoid unnecessary computation.
	numSamples := len(s.secondSamples)
	// 计算头尾的样本点的时间间隔
	elapsed := time.Nanosecond
	if numSamples > 1 {
		start := s.secondSamples[0].Timestamp
		end := s.secondSamples[numSamples-1].Timestamp
		elapsed = end.Sub(start)
	}
	// secondSamples头尾的样本点的时间间隔大于60s，清理secondSamples列表，只保留最后一个secondSample
	if elapsed > 60*time.Second {
		// Make a minute sample. This works with dynamic housekeeping as long
		// as we keep max dynamic houskeeping period close to a minute.
		// 获得一分钟的聚合值，cpu和memory（平均值、最大值、0.5分位值、0.9分位值、0.95分位值）和样本时间覆盖率
		minuteSample := GetMinutePercentiles(s.secondSamples)
		// Clear seconds samples. Keep the latest sample for continuity.
		// Copying and resizing helps avoid slice re-allocation.
		// s.secondSamples里只保留最后一个secondSample
		s.secondSamples[0] = s.secondSamples[numSamples-1]
		s.secondSamples = s.secondSamples[:1]
		// minuteSamples为环形buffer，添加一个sample到s.minuteSamples.samples（[]info.Usage切片），可能会覆盖老的sample
		s.minuteSamples.Add(minuteSample)
		// 更新s.derivedStats，cpu（利用率）和memory（当前值）小时聚合值和天的聚合值
		err := s.updateDerivedStats()
		if err != nil {
			return err
		}
	}
	return nil
}

// 设置最近的cpu（使用率）和memory使用情况和最近的时间点
// 设置s.derivedStats.LatestUsage和s.derivedStats.Timestamp
func (s *StatsSummary) updateLatestUsage() {
	// cpu是累加值，memory是实时值
	usage := info.InstantUsage{}
	numStats := len(s.secondSamples)
	if numStats < 1 {
		return
	}
	latest := s.secondSamples[numStats-1]
	usage.Memory = latest.Memory
	if numStats > 1 {
		previous := s.secondSamples[numStats-2]
		// 计算cpu使用率 = (latest.Cpu - previous.Cpu) * secondsToMilliSeconds / uint64(elapsed)
		cpu, err := getCpuRate(*latest, *previous)
		if err == nil {
			usage.Cpu = cpu
		}
	}
	// 如果numStats=1，则cpu为0

	s.dataLock.Lock()
	defer s.dataLock.Unlock()
	s.derivedStats.LatestUsage = usage
	s.derivedStats.Timestamp = latest.Timestamp
	return
}

// Generate new derived stats based on current minute stats samples.
// 更新s.derivedStats，cpu（利用率）和memory（当前值）的最近的使用状态LatestUsage和小时聚合值和天的聚合值
func (s *StatsSummary) updateDerivedStats() error {
	derived := info.DerivedStats{}
	derived.Timestamp = time.Now()
	// 返回最近一个sample
	minuteSamples := s.minuteSamples.RecentStats(1)
	if len(minuteSamples) != 1 {
		return fmt.Errorf("failed to retrieve minute stats")
	}
	// 最近一分钟的sample数据
	derived.MinuteUsage = *minuteSamples[0]
	// 获取最近60个分钟级别（1小时）的sample进行聚合，返回cpu（利用率）和memory（当前值）的聚合值（平均值、最大值、0.5分位值、0.9分位值、0.95分位值）和数据样本时间覆盖率
	hourUsage, err := s.getDerivedUsage(60)
	if err != nil {
		return fmt.Errorf("failed to compute hour stats: %v", err)
	}
	// 获取最近60*24个分钟级别（1天）的sample进行聚合，返回cpu（利用率）和memory（当前值）的聚合值（平均值、最大值、0.5分位值、0.9分位值、0.95分位值）和数据样本时间覆盖率
	dayUsage, err := s.getDerivedUsage(60 * 24)
	if err != nil {
		return fmt.Errorf("failed to compute day usage: %v", err)
	}
	derived.HourUsage = hourUsage
	derived.DayUsage = dayUsage

	s.dataLock.Lock()
	defer s.dataLock.Unlock()
	// 保留原有LatestUsage，意味者只更新s.derivedStats.HourUsage、s.derivedStats.DayUsage
	derived.LatestUsage = s.derivedStats.LatestUsage
	s.derivedStats = derived

	return nil
}

// helper method to get hour and daily derived stats
// 获取最近n个sample进行聚合，返回cpu（利用率）和memory（当前值）的聚合值（平均值、最大值、0.5分位值、0.9分位值、0.95分位值）和数据样本时间覆盖率
func (s *StatsSummary) getDerivedUsage(n int) (info.Usage, error) {
	if n < 1 {
		return info.Usage{}, fmt.Errorf("invalid number of samples requested: %d", n)
	}
	samples := s.minuteSamples.RecentStats(n)
	numSamples := len(samples)
	if numSamples < 1 {
		return info.Usage{}, fmt.Errorf("failed to retrieve any minute stats.")
	}
	// We generate derived stats even with partial data.
	// 返回所有samples数据中的cpu（利用率）和memory（当前值）的聚合值（平均值、最大值、0.5分位值、0.9分位值、0.95分位值）
	usage := GetDerivedPercentiles(samples)
	// Assumes we have equally placed minute samples.
	usage.PercentComplete = int32(numSamples * 100 / n)
	return usage, nil
}

// Return the latest calculated derived stats.
func (s *StatsSummary) DerivedStats() (info.DerivedStats, error) {
	s.dataLock.RLock()
	defer s.dataLock.RUnlock()

	return s.derivedStats, nil
}

func New(spec v1.ContainerSpec) (*StatsSummary, error) {
	summary := StatsSummary{}
	if spec.HasCpu {
		summary.available.Cpu = true
	}
	if spec.HasMemory {
		summary.available.Memory = true
	}
	if !summary.available.Cpu && !summary.available.Memory {
		return nil, fmt.Errorf("none of the resources are being tracked.")
	}
	summary.minuteSamples = NewSamplesBuffer(60 /* one hour */)
	return &summary, nil
}
