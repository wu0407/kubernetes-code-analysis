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
	"sync"

	"github.com/blang/semver"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"k8s.io/klog"
)

/*
kubeCollector extends the prometheus.Collector interface to allow customization of the metric
registration process. Defer metric initialization until Create() is called, which then
delegates to the underlying metric's initializeMetric or initializeDeprecatedMetric
method call depending on whether the metric is deprecated or not.
*/
type kubeCollector interface {
	Collector
	lazyKubeMetric
	DeprecatedVersion() *semver.Version
	// Each collector metric should provide an initialization function
	// for both deprecated and non-deprecated variants of a metric. This
	// is necessary since metric instantiation will be deferred
	// until the metric is actually registered somewhere.
	initializeMetric()
	initializeDeprecatedMetric()
}

/*
lazyKubeMetric defines our metric registration interface. lazyKubeMetric objects are expected
to lazily instantiate metrics (i.e defer metric instantiation until when
the Create() function is explicitly called).
*/
type lazyKubeMetric interface {
	Create(*semver.Version) bool
	IsCreated() bool
	IsHidden() bool
	IsDeprecated() bool
}

/*
lazyMetric implements lazyKubeMetric. A lazy metric is lazy because it waits until metric
registration time before instantiation. Add it as an anonymous field to a struct that
implements kubeCollector to get deferred registration behavior. You must call lazyInit
with the kubeCollector itself as an argument.
*/
type lazyMetric struct {
	fqName              string
	isDeprecated        bool
	isHidden            bool
	isCreated           bool
	createLock          sync.RWMutex
	markDeprecationOnce sync.Once
	createOnce          sync.Once
	self                kubeCollector
}

func (r *lazyMetric) IsCreated() bool {
	r.createLock.RLock()
	defer r.createLock.RUnlock()
	return r.isCreated
}

// lazyInit provides the lazyMetric with a reference to the kubeCollector it is supposed
// to allow lazy initialization for. It should be invoked in the factory function which creates new
// kubeCollector type objects.
func (r *lazyMetric) lazyInit(self kubeCollector, fqName string) {
	r.fqName = fqName
	r.self = self
}

// determineDeprecationStatus figures out whether the lazy metric should be deprecated or not.
// This method takes a Version argument which should be the version of the binary in which
// this code is currently being executed.
// r.self里的配置的DeprecatedVersion为nil，则不做任何操作
// r.self里的配置的DeprecatedVersion小于等于version，则设置r.isDeprecated为true
// 如果kubernetes某个组件没有设置showHidden，且r.self里的配置的DeprecatedVersion小于version，则r.isHidden为true
func (r *lazyMetric) determineDeprecationStatus(version semver.Version) {
	// 解析r.self里的配置的DeprecatedVersion，比如Histogram为h.HistogramOpts.DeprecatedVersion
	selfVersion := r.self.DeprecatedVersion()
	if selfVersion == nil {
		return
	}
	r.markDeprecationOnce.Do(func() {
		// selfVersion（metrics里的DeprecatedVersion）小于等于version，则设置r.isDeprecated为true
		if selfVersion.LTE(version) {
			r.isDeprecated = true
		}
		// kubelet里配置了showHiddenMetricsForVersion或--show-hidden-metrics-for-version命令行
		// 会设置staging\src\k8s.io\component-base\metrics\registry.go里的showHidden值为true
		// 读取staging\src\k8s.io\component-base\metrics\registry.go里的showHidden值
		if ShouldShowHidden() {
			klog.Warningf("Hidden metrics (%s) have been manually overridden, showing this very deprecated metric.", r.fqName)
			return
		}
		// version（小版本版本号第三位置0）大于selfVersion，则返回true
		if shouldHide(&version, selfVersion) {
			// TODO(RainbowMango): Remove this log temporarily. https://github.com/kubernetes/kubernetes/issues/85369
			// klog.Warningf("This metric has been deprecated for more than one release, hiding.")
			r.isHidden = true
		}
	})
}

func (r *lazyMetric) IsHidden() bool {
	return r.isHidden
}

func (r *lazyMetric) IsDeprecated() bool {
	return r.isDeprecated
}

// Create forces the initialization of metric which has been deferred until
// the point at which this method is invoked. This method will determine whether
// the metric is deprecated or hidden, no-opting if the metric should be considered
// hidden. Furthermore, this function no-opts and returns true if metric is already
// created.
// 根据metric的deprecate状态，判断是否要进行初始化，返回是否进行了初始化
// 比如r.self是Histogram
// r.self里的配置的DeprecatedVersion小于等于version，则设置r.isDeprecated为true
// 如果kubernetes某个组件没有设置showHidden，且r.self里的配置的DeprecatedVersion小于version，则r.isHidden为true，然后直接返回
// 如果r.isDeprecated为true，则进行deprecated metric初始化
//     修改r.self.HistogramOpts.Help为"[{r.self.HistogramOpts.StabilityLevel}] (Deprecated since {r.self.HistogramOpts.DeprecatedVersion}) {r.self.HistogramOpts.Help}"
//     设置r.self.ObserverMetric为生成的prometheus histogram
//     设置r.self.selfCollector.metric为生成的prometheus histogram
// 如果r.isDeprecated为false
//     修改r.self.HistogramOpts.Help为"[{r.self.StabilityLevel}] {r.self.HistogramOpts.Help}"
//     设置r.self.ObserverMetric为生成prometheus histogram
//     设置r.self.selfCollector.metric为生成的prometheus histogram
func (r *lazyMetric) Create(version *semver.Version) bool {
	if version != nil {
		// r.self里的配置的DeprecatedVersion为nil，则不做任何操作
		// r.self里的配置的DeprecatedVersion小于等于version，则设置r.isDeprecated为true
		// 如果kubernetes某个组件没有设置showHidden，且r.self里的配置的DeprecatedVersion小于version，则r.isHidden为true
		r.determineDeprecationStatus(*version)
	}
	// let's not create if this metric is slated to be hidden
	// 如果r.isHidden为true，则直接返回
	if r.IsHidden() {
		return false
	}
	r.createOnce.Do(func() {
		r.createLock.Lock()
		defer r.createLock.Unlock()
		r.isCreated = true
		// 如果r.isDeprecated为true，则进行deprecated metric初始化
		if r.IsDeprecated() {
			// 比如r.self是Histogram
			// 修改r.self.HistogramOpts.Help为"[{r.self.HistogramOpts.StabilityLevel}] (Deprecated since {r.self.HistogramOpts.DeprecatedVersion}) {r.self.HistogramOpts.Help}"
			// 设置r.self.ObserverMetric为生成的prometheus histogram
			// 设置r.self.selfCollector.metric为生成的prometheus histogram
			r.self.initializeDeprecatedMetric()
		} else {
			// 比如r.self是Histogram
			// 修改r.self.HistogramOpts.Help为"[{r.self.StabilityLevel}] {r.self.HistogramOpts.Help}"
			// 设置r.self.ObserverMetric为生成prometheus histogram
			// 设置r.self.selfCollector.metric为生成的prometheus histogram
			r.self.initializeMetric()
		}
	})
	// 返回r.isCreated的值
	return r.IsCreated()
}

// ClearState will clear all the states marked by Create.
// It intends to be used for re-register a hidden metric.
// 重置所有的状态标记
// r.isDeprecated、r.isHidden、r.isCreated为false
// r.markDeprecationOnce、r.createOnce重置成零值
func (r *lazyMetric) ClearState() {
	r.createLock.Lock()
	defer r.createLock.Unlock()

	r.isDeprecated = false
	r.isHidden = false
	r.isCreated = false
	r.markDeprecationOnce = *(new(sync.Once))
	r.createOnce = *(new(sync.Once))
}

// FQName returns the fully-qualified metric name of the collector.
func (r *lazyMetric) FQName() string {
	return r.fqName
}

/*
This code is directly lifted from the prometheus codebase. It's a convenience struct which
allows you satisfy the Collector interface automatically if you already satisfy the Metric interface.

For reference: https://github.com/prometheus/client_golang/blob/v0.9.2/prometheus/collector.go#L98-L120
*/
type selfCollector struct {
	metric prometheus.Metric
}

// staging\src\k8s.io\component-base\metrics\histogram.go里的Histogram.initializeMetric()
// 调用initSelfCollection，设置Histogram.selfCollector.metric为生成的prometheus histogram（实现prometheus.Metric）
// 设置c.metric为m
func (c *selfCollector) initSelfCollection(m prometheus.Metric) {
	c.metric = m
}

func (c *selfCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.metric.Desc()
}

func (c *selfCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- c.metric
}

// no-op vecs for convenience
var noopCounterVec = &prometheus.CounterVec{}
var noopHistogramVec = &prometheus.HistogramVec{}

// lint:ignore U1000 Keep it for future use
var noopSummaryVec = &prometheus.SummaryVec{}
var noopGaugeVec = &prometheus.GaugeVec{}
var noopObserverVec = &noopObserverVector{}

// just use a convenience struct for all the no-ops
var noop = &noopMetric{}

type noopMetric struct{}

func (noopMetric) Inc()                             {}
func (noopMetric) Add(float64)                      {}
func (noopMetric) Dec()                             {}
func (noopMetric) Set(float64)                      {}
func (noopMetric) Sub(float64)                      {}
func (noopMetric) Observe(float64)                  {}
func (noopMetric) SetToCurrentTime()                {}
func (noopMetric) Desc() *prometheus.Desc           { return nil }
func (noopMetric) Write(*dto.Metric) error          { return nil }
func (noopMetric) Describe(chan<- *prometheus.Desc) {}
func (noopMetric) Collect(chan<- prometheus.Metric) {}

type noopObserverVector struct{}

func (noopObserverVector) GetMetricWith(prometheus.Labels) (prometheus.Observer, error) {
	return noop, nil
}
func (noopObserverVector) GetMetricWithLabelValues(...string) (prometheus.Observer, error) {
	return noop, nil
}
func (noopObserverVector) With(prometheus.Labels) prometheus.Observer    { return noop }
func (noopObserverVector) WithLabelValues(...string) prometheus.Observer { return noop }
func (noopObserverVector) CurryWith(prometheus.Labels) (prometheus.ObserverVec, error) {
	return noopObserverVec, nil
}
func (noopObserverVector) MustCurryWith(prometheus.Labels) prometheus.ObserverVec {
	return noopObserverVec
}
func (noopObserverVector) Describe(chan<- *prometheus.Desc) {}
func (noopObserverVector) Collect(chan<- prometheus.Metric) {}
