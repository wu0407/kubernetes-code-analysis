/*
Copyright 2019 The Kubernetes Authors.

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

package metrics

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/blang/semver"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	apimachineryversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/component-base/version"
)

var (
	showHiddenOnce sync.Once
	showHidden     atomic.Value
	registries     []*kubeRegistry // stores all registries created by NewKubeRegistry()
	registriesLock sync.RWMutex
)

// shouldHide be used to check if a specific metric with deprecated version should be hidden
// according to metrics deprecation lifecycle.
// currentVersion（将小版本版本号第三位置0进行比较）大于deprecatedVersion，则返回true
func shouldHide(currentVersion *semver.Version, deprecatedVersion *semver.Version) bool {
	guardVersion, err := semver.Make(fmt.Sprintf("%d.%d.0", currentVersion.Major, currentVersion.Minor))
	if err != nil {
		panic("failed to make version from current version")
	}

	if deprecatedVersion.LT(guardVersion) {
		return true
	}

	return false
}

// 验证targetVersionStr是否是currentVersion（一般当前kubernetes的上一个版本），是的话返回nil，不是返回错误
func validateShowHiddenMetricsVersion(currentVersion semver.Version, targetVersionStr string) error {
	if targetVersionStr == "" {
		return nil
	}

	// currentVersion（一般是当前kubernetes版本）的上一个版本
	validVersionStr := fmt.Sprintf("%d.%d", currentVersion.Major, currentVersion.Minor-1)
	if targetVersionStr != validVersionStr {
		return fmt.Errorf("--show-hidden-metrics-for-version must be omitted or have the value '%v'. Only the previous minor version is allowed", validVersionStr)
	}

	return nil
}

// ValidateShowHiddenMetricsVersion checks invalid version for which show hidden metrics.
// 在kubelet里pkg\kubelet\apis\config\validation\validation.go里会调用
// 验证v是否是当前kubernetes的上一个版本，是的话返回nil，不是返回错误
// v为空，则返回nil
func ValidateShowHiddenMetricsVersion(v string) []error {
	// kubernetes版本
	// v为空，则返回nil
	// 验证v是否是当前kubernetes的上一个版本，是的话返回nil，不是返回错误
	err := validateShowHiddenMetricsVersion(parseVersion(version.Get()), v)
	if err != nil {
		return []error{err}
	}

	return nil
}

// SetShowHidden will enable showing hidden metrics. This will no-opt
// after the initial call
// 这个在cmd\kubelet\app\server.go里的run里会调用
// kubelet里配置了showHiddenMetricsForVersion或--show-hidden-metrics-for-version命令行会调用这个
func SetShowHidden() {
	showHiddenOnce.Do(func() {
		// 设置showHidden为true
		showHidden.Store(true)

		// re-register collectors that has been hidden in phase of last registry.
		for _, r := range registries {
			// 启用这个registry里所有隐藏的Collector（metrics）
			r.enableHiddenCollectors()
			// StableCollector里包含了HiddenMetrics，则先进行Unregister，然后再执行ClearState()
			// 启用这个registry里所有隐藏的StableCollectors（metrics）
			r.enableHiddenStableCollectors()
		}
	})
}

// ShouldShowHidden returns whether showing hidden deprecated metrics
// is enabled. While the primary usecase for this is internal (to determine
// registration behavior) this can also be used to introspect
// 检测showHidden是否true
func ShouldShowHidden() bool {
	return showHidden.Load() != nil && showHidden.Load().(bool)
}

// Registerable is an interface for a collector metric which we
// will register with KubeRegistry.
type Registerable interface {
	prometheus.Collector

	// Create will mark deprecated state for the collector
	Create(version *semver.Version) bool

	// ClearState will clear all the states marked by Create.
	ClearState()

	// FQName returns the fully-qualified metric name of the collector.
	FQName() string
}

// KubeRegistry is an interface which implements a subset of prometheus.Registerer and
// prometheus.Gatherer interfaces
type KubeRegistry interface {
	// Deprecated
	RawMustRegister(...prometheus.Collector)
	CustomRegister(c StableCollector) error
	CustomMustRegister(cs ...StableCollector)
	Register(Registerable) error
	MustRegister(...Registerable)
	Unregister(collector Collector) bool
	Gather() ([]*dto.MetricFamily, error)
}

// kubeRegistry is a wrapper around a prometheus registry-type object. Upon initialization
// the kubernetes binary version information is loaded into the registry object, so that
// automatic behavior can be configured for metric versioning.
type kubeRegistry struct {
	PromRegistry
	version              semver.Version
	hiddenCollectors     map[string]Registerable // stores all collectors that has been hidden
	stableCollectors     []StableCollector       // stores all stable collector
	hiddenCollectorsLock sync.RWMutex
	stableCollectorsLock sync.RWMutex
}

// Register registers a new Collector to be included in metrics
// collection. It returns an error if the descriptors provided by the
// Collector are invalid or if they — in combination with descriptors of
// already registered Collectors — do not fulfill the consistency and
// uniqueness criteria described in the documentation of metric.Desc.
// 进行封装的metrics的初始化和prometheus的registry注册非隐藏的Collector（隐藏的Collector不注册）
func (kr *kubeRegistry) Register(c Registerable) error {
	if c.Create(&kr.version) {
		return kr.PromRegistry.Register(c)
	}

	kr.trackHiddenCollector(c)

	return nil
}

// MustRegister works like Register but registers any number of
// Collectors and panics upon the first registration that causes an
// error.
// 进行封装的metrics的初始化和prometheus的registry的注册（所有Collector都必须注册，无论是否是隐藏的）
func (kr *kubeRegistry) MustRegister(cs ...Registerable) {
	metrics := make([]prometheus.Collector, 0, len(cs))
	for _, c := range cs {
		// 比如c是Histogram
		// 在staging\src\k8s.io\component-base\metrics\metric.go里的lazyMetric
		// 根据metric的deprecate状态，判断是否要进行初始化，返回是否进行了初始化
		// c.lazyMetric.self是Histogram
		// c.lazyMetric.self里的配置的DeprecatedVersion小于等于version，则设置c.lazyMetric.isDeprecated为true
		// 如果kubernetes某个组件没有设置showHidden，且c.lazyMetric.self里的配置的DeprecatedVersion小于version，则c.lazyMetric.isHidden为true，然后直接返回
		// 如果c.lazyMetric.isDeprecated为true，则进行deprecated metric初始化
		//     修改c.lazyMetric.self.HistogramOpts.Help为"[{c.lazyMetric.self.HistogramOpts.StabilityLevel}] (Deprecated since {c.lazyMetric.self.HistogramOpts.DeprecatedVersion}) {c.lazyMetric.self.HistogramOpts.Help}"
		//     设置c.lazyMetric.self.ObserverMetric为生成的prometheus histogram
		//     设置c.lazyMetric.self.selfCollector.metric为生成的prometheus histogram
		// 如果c.lazyMetric.isDeprecated为false
		//     修改c.lazyMetric.self.HistogramOpts.Help为"[{c.lazyMetric.self.StabilityLevel}] {c.lazyMetric.self.HistogramOpts.Help}"
		//     设置c.lazyMetric.self.ObserverMetric为生成prometheus histogram
		//     设置c.lazyMetric.self.selfCollector.metric为生成的prometheus histogram
		if c.Create(&kr.version) {
			metrics = append(metrics, c)
		} else {
			// 没有进行创建（初始化）（被隐藏的metrics）
			// 将c以key为c.FQName()放入kr.hiddenCollectors（map里）
			kr.trackHiddenCollector(c)
		}
	}
	kr.PromRegistry.MustRegister(metrics...)
}

// CustomRegister registers a new custom collector.
func (kr *kubeRegistry) CustomRegister(c StableCollector) error {
	kr.trackStableCollectors(c)

	if c.Create(&kr.version, c) {
		return kr.PromRegistry.Register(c)
	}
	return nil
}

// CustomMustRegister works like CustomRegister but registers any number of
// StableCollectors and panics upon the first registration that causes an
// error.
// 进行封装的metrics的初始化和prometheus的registry的注册（所有StableCollector都必须注册，无论是否是隐藏的）
// 所有的StableCollector添加到kr.stableCollectors列表中
func (kr *kubeRegistry) CustomMustRegister(cs ...StableCollector) {
	// 添加cs到kr.stableCollectors列表中
	kr.trackStableCollectors(cs...)

	collectors := make([]prometheus.Collector, 0, len(cs))
	for _, c := range cs {
		if c.Create(&kr.version, c) {
			collectors = append(collectors, c)
		}
	}

	kr.PromRegistry.MustRegister(collectors...)
}

// RawMustRegister takes a native prometheus.Collector and registers the collector
// to the registry. This bypasses metrics safety checks, so should only be used
// to register custom prometheus collectors.
//
// Deprecated
func (kr *kubeRegistry) RawMustRegister(cs ...prometheus.Collector) {
	kr.PromRegistry.MustRegister(cs...)
}

// Unregister unregisters the Collector that equals the Collector passed
// in as an argument.  (Two Collectors are considered equal if their
// Describe method yields the same set of descriptors.) The function
// returns whether a Collector was unregistered. Note that an unchecked
// Collector cannot be unregistered (as its Describe method does not
// yield any descriptor).
// 在prometheus register里反注册collector
func (kr *kubeRegistry) Unregister(collector Collector) bool {
	return kr.PromRegistry.Unregister(collector)
}

// Gather calls the Collect method of the registered Collectors and then
// gathers the collected metrics into a lexicographically sorted slice
// of uniquely named MetricFamily protobufs. Gather ensures that the
// returned slice is valid and self-consistent so that it can be used
// for valid exposition. As an exception to the strict consistency
// requirements described for metric.Desc, Gather will tolerate
// different sets of label names for metrics of the same metric family.
func (kr *kubeRegistry) Gather() ([]*dto.MetricFamily, error) {
	return kr.PromRegistry.Gather()
}

// trackHiddenCollector stores all hidden collectors.
// 将c以key为c.FQName()放入kr.hiddenCollectors（map里）
func (kr *kubeRegistry) trackHiddenCollector(c Registerable) {
	kr.hiddenCollectorsLock.Lock()
	defer kr.hiddenCollectorsLock.Unlock()

	kr.hiddenCollectors[c.FQName()] = c
}

// trackStableCollectors stores all custom collectors.
// 添加cs到kr.stableCollectors列表中
func (kr *kubeRegistry) trackStableCollectors(cs ...StableCollector) {
	kr.stableCollectorsLock.Lock()
	defer kr.stableCollectorsLock.Unlock()

	kr.stableCollectors = append(kr.stableCollectors, cs...)
}

// enableHiddenCollectors will re-register all of the hidden collectors.
// 启用这个registry里所有隐藏的Collector（metrics）
func (kr *kubeRegistry) enableHiddenCollectors() {
	// 没有隐藏的Collectors（metrics）直接返回
	if len(kr.hiddenCollectors) == 0 {
		return
	}

	kr.hiddenCollectorsLock.Lock()
	cs := make([]Registerable, 0, len(kr.hiddenCollectors))

	// 遍历所有隐藏的Collectors（metrics）
	for _, c := range kr.hiddenCollectors {
		// 比如Histogram
		// staging\src\k8s.io\component-base\metrics\metric.go里的lazyMetric
		// 重置所有的状态标记
		// r.isDeprecated、r.isHidden、r.isCreated为false
		// r.markDeprecationOnce、r.createOnce重置成零值
		c.ClearState()
		cs = append(cs, c)
	}

	// 重置kr.hiddenCollectors为nil
	kr.hiddenCollectors = nil
	kr.hiddenCollectorsLock.Unlock()
	// 进行封装的metrics的初始化和prometheus的registry的注册
	// 重新执行Create（这次由于设置了全局变量showHidden为true，所以不会被隐藏）
	kr.MustRegister(cs...)
}

// enableHiddenStableCollectors will re-register the stable collectors if there is one or more hidden metrics in it.
// Since we can not register a metrics twice, so we have to unregister first then register again.
// StableCollector里包含了HiddenMetrics，则先进行Unregister，然后再执行ClearState()
// 启用这个registry里所有隐藏的StableCollectors（metrics）
func (kr *kubeRegistry) enableHiddenStableCollectors() {
	// 如果stableCollectors为空，则直接返回
	if len(kr.stableCollectors) == 0 {
		return
	}

	kr.stableCollectorsLock.Lock()

	cs := make([]StableCollector, 0, len(kr.stableCollectors))
	for _, c := range kr.stableCollectors {
		// 如果StableCollector里包含了HiddenMetrics，则先进行Unregister，然后再执行ClearState()
		if len(c.HiddenMetrics()) > 0 {
			// 在prometheus register里反注册collector
			kr.Unregister(c) // unregister must happens before clear state, otherwise no metrics would be unregister
			c.ClearState()
			cs = append(cs, c)
		}
	}

	kr.stableCollectors = nil
	kr.stableCollectorsLock.Unlock()
	// 进行封装的metrics的初始化和prometheus的registry的注册（所有StableCollector都必须注册，无论是否是隐藏的）
	// 所有的StableCollector添加到kr.stableCollectors列表中
	kr.CustomMustRegister(cs...)
}

// BuildVersion is a helper function that can be easily mocked.
var BuildVersion = version.Get

func newKubeRegistry(v apimachineryversion.Info) *kubeRegistry {
	r := &kubeRegistry{
		PromRegistry:     prometheus.NewRegistry(),
		version:          parseVersion(v),
		hiddenCollectors: make(map[string]Registerable),
	}

	registriesLock.Lock()
	defer registriesLock.Unlock()
	registries = append(registries, r)

	return r
}

// NewKubeRegistry creates a new vanilla Registry without any Collectors
// pre-registered.
func NewKubeRegistry() KubeRegistry {
	r := newKubeRegistry(BuildVersion())

	return r
}
