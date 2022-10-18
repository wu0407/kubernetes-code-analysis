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
	"github.com/blang/semver"
	"github.com/prometheus/client_golang/prometheus"
)

// DefBuckets is a wrapper for prometheus.DefBuckets
var DefBuckets = prometheus.DefBuckets

// LinearBuckets is a wrapper for prometheus.LinearBuckets.
func LinearBuckets(start, width float64, count int) []float64 {
	return prometheus.LinearBuckets(start, width, count)
}

// ExponentialBuckets is a wrapper for prometheus.ExponentialBuckets.
func ExponentialBuckets(start, factor float64, count int) []float64 {
	return prometheus.ExponentialBuckets(start, factor, count)
}

// Histogram is our internal representation for our wrapping struct around prometheus
// histograms. Summary implements both kubeCollector and ObserverMetric
type Histogram struct {
	ObserverMetric
	*HistogramOpts
	lazyMetric
	// 这个实现了prometheus.Collector)
	selfCollector
}

// NewHistogram returns an object which is Histogram-like. However, nothing
// will be measured until the histogram is registered somewhere.
func NewHistogram(opts *HistogramOpts) *Histogram {
	opts.StabilityLevel.setDefaults()

	h := &Histogram{
		HistogramOpts: opts,
		lazyMetric:    lazyMetric{},
	}
	// 设置h.ObserverMetric为noopMetric{}
	// 设置h.selfCollector.metric为noopMetric{}
	h.setPrometheusHistogram(noopMetric{})
	// BuildFQName
	// 返回如果namespace, subsystem, name都不为空，则返回"{namespace}_{subsystem}_{name}"
	// 如果namespace, subsystem, name有一个为空，则空的省略
	//
	// 设置h.lazyMetric.fqName为BuildFQName执行结果
	// 设置h.lazyMetric.self为h
	h.lazyInit(h, BuildFQName(opts.Namespace, opts.Subsystem, opts.Name))
	return h
}

// setPrometheusHistogram sets the underlying KubeGauge object, i.e. the thing that does the measurement.
// 设置h.ObserverMetric为histogram
// 设置h.selfCollector.metric为histogram
func (h *Histogram) setPrometheusHistogram(histogram prometheus.Histogram) {
	h.ObserverMetric = histogram
	// 设置h.selfCollector.metric为histogram
	h.initSelfCollection(histogram)
}

// DeprecatedVersion returns a pointer to the Version or nil
// h.HistogramOpts.DeprecatedVersion解析成semver.Version
func (h *Histogram) DeprecatedVersion() *semver.Version {
	return parseSemver(h.HistogramOpts.DeprecatedVersion)
}

// initializeMetric invokes the actual prometheus.Histogram object instantiation
// and stores a reference to it
// 修改h.HistogramOpts.Help为"[{h.HistogramOpts.StabilityLevel}] {h.HistogramOpts.Help}"
// 设置h.ObserverMetric为生成prometheus histogram
// 设置h.selfCollector.metric为生成的prometheus histogram
func (h *Histogram) initializeMetric() {
	// 修改h.HistogramOpts.Help为"[{h.HistogramOpts.StabilityLevel}] {h.HistogramOpts.Help}"
	h.HistogramOpts.annotateStabilityLevel()
	// this actually creates the underlying prometheus gauge.
	// h.HistogramOpts.toPromHistogramOpts()为将HistogramOpts转成prometheus.HistogramOpts
	// 设置h.ObserverMetric为生成的prometheus.histogram（实现prometheus.Metric）
	// 设置h.selfCollector.metric为为生成的prometheus.histogram（实现prometheus.Metric）
	h.setPrometheusHistogram(prometheus.NewHistogram(h.HistogramOpts.toPromHistogramOpts()))
}

// initializeDeprecatedMetric invokes the actual prometheus.Histogram object instantiation
// but modifies the Help description prior to object instantiation.
// 修改h.HistogramOpts.Help为"[{o.StabilityLevel}] (Deprecated since {h.HistogramOpts.DeprecatedVersion}) {h.HistogramOpts.Help}"
// 设置h.ObserverMetric为生成的prometheus histogram
// 设置h.selfCollector.metric为生成的prometheus histogram
func (h *Histogram) initializeDeprecatedMetric() {
	// 修改h.HistogramOpts.Help为"(Deprecated since {h.HistogramOpts.DeprecatedVersion}) {h.HistogramOpts.Help}"
	h.HistogramOpts.markDeprecated()
	// 修改h.HistogramOpts.Help为"[{o.StabilityLevel}] {h.HistogramOpts.Help}"
	// 设置h.ObserverMetric为histogram
	// 设置h.selfCollector.metric为histogram
	h.initializeMetric()
}

// HistogramVec is the internal representation of our wrapping struct around prometheus
// histogramVecs.
type HistogramVec struct {
	*prometheus.HistogramVec
	*HistogramOpts
	lazyMetric
	originalLabels []string
}

// NewHistogramVec returns an object which satisfies kubeCollector and wraps the
// prometheus.HistogramVec object. However, the object returned will not measure
// anything unless the collector is first registered, since the metric is lazily instantiated.
func NewHistogramVec(opts *HistogramOpts, labels []string) *HistogramVec {
	opts.StabilityLevel.setDefaults()

	v := &HistogramVec{
		HistogramVec:   noopHistogramVec,
		HistogramOpts:  opts,
		originalLabels: labels,
		lazyMetric:     lazyMetric{},
	}
	v.lazyInit(v, BuildFQName(opts.Namespace, opts.Subsystem, opts.Name))
	return v
}

// DeprecatedVersion returns a pointer to the Version or nil
func (v *HistogramVec) DeprecatedVersion() *semver.Version {
	// string类型版本解析成semver.Version
	return parseSemver(v.HistogramOpts.DeprecatedVersion)
}

func (v *HistogramVec) initializeMetric() {
	v.HistogramOpts.annotateStabilityLevel()
	v.HistogramVec = prometheus.NewHistogramVec(v.HistogramOpts.toPromHistogramOpts(), v.originalLabels)
}

func (v *HistogramVec) initializeDeprecatedMetric() {
	v.HistogramOpts.markDeprecated()
	v.initializeMetric()
}

// Default Prometheus behavior actually results in the creation of a new metric
// if a metric with the unique label values is not found in the underlying stored metricMap.
// This means  that if this function is called but the underlying metric is not registered
// (which means it will never be exposed externally nor consumed), the metric will exist in memory
// for perpetuity (i.e. throughout application lifecycle).
//
// For reference: https://github.com/prometheus/client_golang/blob/v0.9.2/prometheus/histogram.go#L460-L470

// WithLabelValues returns the ObserverMetric for the given slice of label
// values (same order as the VariableLabels in Desc). If that combination of
// label values is accessed for the first time, a new ObserverMetric is created IFF the HistogramVec
// has been registered to a metrics registry.
func (v *HistogramVec) WithLabelValues(lvs ...string) ObserverMetric {
	if !v.IsCreated() {
		return noop
	}
	return v.HistogramVec.WithLabelValues(lvs...)
}

// With returns the ObserverMetric for the given Labels map (the label names
// must match those of the VariableLabels in Desc). If that label map is
// accessed for the first time, a new ObserverMetric is created IFF the HistogramVec has
// been registered to a metrics registry.
func (v *HistogramVec) With(labels map[string]string) ObserverMetric {
	if !v.IsCreated() {
		return noop
	}
	return v.HistogramVec.With(labels)
}

// Delete deletes the metric where the variable labels are the same as those
// passed in as labels. It returns true if a metric was deleted.
//
// It is not an error if the number and names of the Labels are inconsistent
// with those of the VariableLabels in Desc. However, such inconsistent Labels
// can never match an actual metric, so the method will always return false in
// that case.
func (v *HistogramVec) Delete(labels map[string]string) bool {
	if !v.IsCreated() {
		return false // since we haven't created the metric, we haven't deleted a metric with the passed in values
	}
	return v.HistogramVec.Delete(labels)
}

// Reset deletes all metrics in this vector.
func (v *HistogramVec) Reset() {
	if !v.IsCreated() {
		return
	}

	v.HistogramVec.Reset()
}
