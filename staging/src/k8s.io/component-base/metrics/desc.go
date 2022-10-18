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

	"github.com/blang/semver"
	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/klog"
)

// Desc is a prometheus.Desc extension.
//
// Use NewDesc to create new Desc instances.
type Desc struct {
	// fqName has been built from Namespace, Subsystem, and Name.
	fqName string
	// help provides some helpful information about this metric.
	help string
	// constLabels is the label names. Their label values are variable.
	constLabels Labels
	// variableLabels contains names of labels for which the metric
	// maintains variable values.
	variableLabels []string

	// promDesc is the descriptor used by every Prometheus Metric.
	promDesc      *prometheus.Desc
	annotatedHelp string

	// stabilityLevel represents the API guarantees for a given defined metric.
	stabilityLevel StabilityLevel
	// deprecatedVersion represents in which version this metric be deprecated.
	deprecatedVersion string

	isDeprecated        bool
	isHidden            bool
	isCreated           bool
	createLock          sync.RWMutex
	markDeprecationOnce sync.Once
	createOnce          sync.Once
	deprecateOnce       sync.Once
	hideOnce            sync.Once
	annotateOnce        sync.Once
}

// NewDesc extends prometheus.NewDesc with stability support.
//
// The stabilityLevel should be valid stability label, such as "metrics.ALPHA"
// and "metrics.STABLE"(Maybe "metrics.BETA" in future). Default value "metrics.ALPHA"
// will be used in case of empty or invalid stability label.
//
// The deprecatedVersion represents in which version this Metric be deprecated.
// The deprecation policy outlined by the control plane metrics stability KEP.
func NewDesc(fqName string, help string, variableLabels []string, constLabels Labels,
	stabilityLevel StabilityLevel, deprecatedVersion string) *Desc {
	d := &Desc{
		fqName:            fqName,
		help:              help,
		annotatedHelp:     help,
		variableLabels:    variableLabels,
		constLabels:       constLabels,
		stabilityLevel:    stabilityLevel,
		deprecatedVersion: deprecatedVersion,
	}
	// 如果d.stabilityLevel为空，则设置d.stabilityLevel为"ALPHA"
	d.stabilityLevel.setDefaults()

	return d
}

// String formats the Desc as a string.
// The stability metadata maybe annotated in 'HELP' section if called after registry,
// otherwise not.
// e.g. "Desc{fqName: "normal_stable_descriptor", help: "[STABLE] this is a stable descriptor", constLabels: {}, variableLabels: []}"
func (d *Desc) String() string {
	if d.isCreated {
		return d.promDesc.String()
	}

	return prometheus.NewDesc(d.fqName, d.help, d.variableLabels, prometheus.Labels(d.constLabels)).String()
}

// toPrometheusDesc transform self to prometheus.Desc
func (d *Desc) toPrometheusDesc() *prometheus.Desc {
	return d.promDesc
}

// DeprecatedVersion returns a pointer to the Version or nil
func (d *Desc) DeprecatedVersion() *semver.Version {
	// string类型的版本解析成semver.Version
	return parseSemver(d.deprecatedVersion)

}

// 根据version跟d.deprecatedVersion比较，设置d.isDeprecated
// 检测是否showHidden是否true，如果为true直接返回。
// 如果为false，根据version跟d.deprecatedVersion比较，设置d.isHidden
func (d *Desc) determineDeprecationStatus(version semver.Version) {
	selfVersion := d.DeprecatedVersion()
	if selfVersion == nil {
		return
	}
	d.markDeprecationOnce.Do(func() {
		// deprecateVersion小于等于version
		if selfVersion.LTE(version) {
			d.isDeprecated = true
		}
		// 检测showHidden是否true
		if ShouldShowHidden() {
			klog.Warningf("Hidden metrics(%s) have been manually overridden, showing this very deprecated metric.", d.fqName)
			return
		}
		// version（将小版本版本号第三位置0进行比较）大于selfVersion，则返回true
		if shouldHide(&version, selfVersion) {
			// TODO(RainbowMango): Remove this log temporarily. https://github.com/kubernetes/kubernetes/issues/85369
			// klog.Warningf("This metric(%s) has been deprecated for more than one release, hiding.", d.fqName)
			d.isHidden = true
		}
	})
}

// IsHidden returns if metric will be hidden
func (d *Desc) IsHidden() bool {
	return d.isHidden
}

// IsDeprecated returns if metric has been deprecated
func (d *Desc) IsDeprecated() bool {
	return d.isDeprecated
}

// IsCreated returns if metric has been created.
func (d *Desc) IsCreated() bool {
	d.createLock.RLock()
	defer d.createLock.RUnlock()

	return d.isCreated
}

// create forces the initialization of Desc which has been deferred until
// the point at which this method is invoked. This method will determine whether
// the Desc is deprecated or hidden, no-opting if the Desc should be considered
// hidden. Furthermore, this function no-opts and returns true if Desc is already
// created.
// 根据version和showHidden，判断是否为deprecated和是否应该进行隐藏
// 如果metrics为隐藏的，直接返回false
// 如果metrics不为隐藏，且为deprecated
//   设置d.annotatedHelp为"[{d.stabilityLevel}] (Deprecated since {d.deprecatedVersion}) {d.annotatedHelp}"
//   创建d.promDesc为prometheus desc
// 如果metrics不为隐藏，且不为deprecated
//   设置d.annotatedHelp为"[{d.stabilityLevel}] {d.annotatedHelp}"
//   创建d.promDesc为prometheus desc
// 设置d.isCreated为true，返回true
func (d *Desc) create(version *semver.Version) bool {
	if version != nil {
		// 根据version跟d.deprecatedVersion比较，设置d.isDeprecated
		// 检测是否showHidden是否true，如果为true直接返回。
		// 如果为false，根据version跟d.deprecatedVersion比较，设置d.isHidden
		d.determineDeprecationStatus(*version)
	}

	// let's not create if this metric is slated to be hidden
	// 如果metrics为隐藏的，直接返回
	if d.IsHidden() {
		return false
	}
	d.createOnce.Do(func() {
		d.createLock.Lock()
		defer d.createLock.Unlock()

		d.isCreated = true
		// 如果是deprecated
		if d.IsDeprecated() {
			// 设置d.annotatedHelp为"[{d.stabilityLevel}] (Deprecated since {d.deprecatedVersion}) {d.annotatedHelp}"
			// 创建d.promDesc为prometheus desc
			d.initializeDeprecatedDesc()
		} else {
			// 设置d.annotatedHelp为"[{d.stabilityLevel}] {d.annotatedHelp}"
			// 创建d.promDesc为prometheus desc
			d.initialize()
		}
	})
	return d.IsCreated()
}

// ClearState will clear all the states marked by Create.
// It intends to be used for re-register a hidden metric.
func (d *Desc) ClearState() {
	d.isDeprecated = false
	d.isHidden = false
	d.isCreated = false

	d.markDeprecationOnce = *new(sync.Once)
	d.createOnce = *new(sync.Once)
	d.deprecateOnce = *new(sync.Once)
	d.hideOnce = *new(sync.Once)
	d.annotateOnce = *new(sync.Once)

	d.annotatedHelp = d.help
	d.promDesc = nil
}

// 设置d.annotatedHelp为"(Deprecated since {d.deprecatedVersion}) {d.annotatedHelp}"
func (d *Desc) markDeprecated() {
	d.deprecateOnce.Do(func() {
		d.annotatedHelp = fmt.Sprintf("(Deprecated since %s) %s", d.deprecatedVersion, d.annotatedHelp)
	})
}

// 设置d.annotatedHelp为"[{d.stabilityLevel}] {d.annotatedHelp}"
func (d *Desc) annotateStabilityLevel() {
	d.annotateOnce.Do(func() {
		d.annotatedHelp = fmt.Sprintf("[%v] %v", d.stabilityLevel, d.annotatedHelp)
	})
}

// 设置d.annotatedHelp为"[{d.stabilityLevel}] {d.annotatedHelp}"
// 创建d.promDesc为prometheus desc
func (d *Desc) initialize() {
	// 设置d.annotatedHelp为"[{d.stabilityLevel}] {d.annotatedHelp}"
	d.annotateStabilityLevel()

	// this actually creates the underlying prometheus desc.
	d.promDesc = prometheus.NewDesc(d.fqName, d.annotatedHelp, d.variableLabels, prometheus.Labels(d.constLabels))
}

// 设置d.annotatedHelp为"[{d.stabilityLevel}] (Deprecated since {d.deprecatedVersion}) {d.annotatedHelp}"
// 创建d.promDesc为prometheus desc
func (d *Desc) initializeDeprecatedDesc() {
	// 设置d.annotatedHelp为"(Deprecated since {d.deprecatedVersion}) {d.annotatedHelp}"
	d.markDeprecated()
	// 设置d.annotatedHelp为"[{d.stabilityLevel}] {d.annotatedHelp}"
	// 创建d.promDesc为prometheus desc
	d.initialize()
}

// GetRawDesc will returns a new *Desc with original parameters provided to NewDesc().
//
// It will be useful in testing scenario that the same Desc be registered to different registry.
//   1. Desc `D` is registered to registry 'A' in TestA (Note: `D` maybe created)
//   2. Desc `D` is registered to registry 'B' in TestB (Note: since 'D' has been created once, thus will be ignored by registry 'B')
func (d *Desc) GetRawDesc() *Desc {
	return NewDesc(d.fqName, d.help, d.variableLabels, d.constLabels, d.stabilityLevel, d.deprecatedVersion)
}
