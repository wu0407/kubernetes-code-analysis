/*
Copyright 2016 The Kubernetes Authors.

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

package cm

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
	cgroupfs "github.com/opencontainers/runc/libcontainer/cgroups/fs"
	cgroupsystemd "github.com/opencontainers/runc/libcontainer/cgroups/systemd"
	libcontainerconfigs "github.com/opencontainers/runc/libcontainer/configs"
	"k8s.io/klog"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

// libcontainerCgroupManagerType defines how to interface with libcontainer
type libcontainerCgroupManagerType string

const (
	// libcontainerCgroupfs means use libcontainer with cgroupfs
	libcontainerCgroupfs libcontainerCgroupManagerType = "cgroupfs"
	// libcontainerSystemd means use libcontainer with systemd
	libcontainerSystemd libcontainerCgroupManagerType = "systemd"
	// systemdSuffix is the cgroup name suffix for systemd
	systemdSuffix string = ".slice"
)

var RootCgroupName = CgroupName([]string{})

// NewCgroupName composes a new cgroup name.
// Use RootCgroupName as base to start at the root.
// This function does some basic check for invalid characters at the name.
func NewCgroupName(base CgroupName, components ...string) CgroupName {
	for _, component := range components {
		// Forbit using "_" in internal names. When remapping internal
		// names to systemd cgroup driver, we want to remap "-" => "_",
		// so we forbid "_" so that we can always reverse the mapping.
		if strings.Contains(component, "/") || strings.Contains(component, "_") {
			panic(fmt.Errorf("invalid character in component [%q] of CgroupName", component))
		}
	}
	// copy data from the base cgroup to eliminate cases where CgroupNames share underlying slices.  See #68416
	baseCopy := make([]string, len(base))
	copy(baseCopy, base)
	return CgroupName(append(baseCopy, components...))
}

// 替换"-"成"_" 
func escapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "-", "_", -1)
}

// 替换"_"为"-"
func unescapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "_", "-", -1)
}

// cgroupName.ToSystemd converts the internal cgroup name to a systemd name.
// For example, the name {"kubepods", "burstable", "pod1234-abcd-5678-efgh"} becomes
// "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice"
// This function always expands the systemd name into the cgroupfs form. If only
// the last part is needed, use path.Base(...) on it to discard the rest.
func (cgroupName CgroupName) ToSystemd() string {
	if len(cgroupName) == 0 || (len(cgroupName) == 1 && cgroupName[0] == "") {
		return "/"
	}
	newparts := []string{}
	for _, part := range cgroupName {
		// 替换"-"成"_"
		part = escapeSystemdCgroupName(part)
		newparts = append(newparts, part)
	}

	result, err := cgroupsystemd.ExpandSlice(strings.Join(newparts, "-") + systemdSuffix)
	if err != nil {
		// Should never happen...
		panic(fmt.Errorf("error converting cgroup name [%v] to systemd format: %v", cgroupName, err))
	}
	return result
}

// 从name中解析出CgroupName
// name比如为"/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-podeb424a44_7004_429d_9925_fbfbc69a7749.slice"
// 返回["kubepods", "besteffort", "podeb424a44-7004-429d-9925-fbfbc69a7749"]
func ParseSystemdToCgroupName(name string) CgroupName {
	driverName := path.Base(name)
	// 移除".slice"后缀
	driverName = strings.TrimSuffix(driverName, systemdSuffix)
	parts := strings.Split(driverName, "-")
	result := []string{}
	for _, part := range parts {
		// 替换"_"为"-"
		result = append(result, unescapeSystemdCgroupName(part))
	}
	return CgroupName(result)
}

// 返回类似"/{cgroup name[0]}/{cgroup name[1]}..."
func (cgroupName CgroupName) ToCgroupfs() string {
	return "/" + path.Join(cgroupName...)
}

func ParseCgroupfsToCgroupName(name string) CgroupName {
	components := strings.Split(strings.TrimPrefix(name, "/"), "/")
	if len(components) == 1 && components[0] == "" {
		components = []string{}
	}
	return CgroupName(components)
}

// name是否有".slice"后缀
func IsSystemdStyleName(name string) bool {
	return strings.HasSuffix(name, systemdSuffix)
}

// libcontainerAdapter provides a simplified interface to libcontainer based on libcontainer type.
type libcontainerAdapter struct {
	// cgroupManagerType defines how to interface with libcontainer
	cgroupManagerType libcontainerCgroupManagerType
}

// newLibcontainerAdapter returns a configured libcontainerAdapter for specified manager.
// it does any initialization required by that manager to function.
func newLibcontainerAdapter(cgroupManagerType libcontainerCgroupManagerType) *libcontainerAdapter {
	return &libcontainerAdapter{cgroupManagerType: cgroupManagerType}
}

// newManager returns an implementation of cgroups.Manager
// cgroup的driver为"cgroupfs"，则使用“runc/libcontainer/cgroups/fs”里的manager
// cgroup的driver为"systemd", 则使用“runc/libcontainer/cgroups/systemd”里的manager
func (l *libcontainerAdapter) newManager(cgroups *libcontainerconfigs.Cgroup, paths map[string]string) (libcontainercgroups.Manager, error) {
	switch l.cgroupManagerType {
	case libcontainerCgroupfs:
		return &cgroupfs.Manager{
			Cgroups: cgroups,
			Paths:   paths,
		}, nil
	case libcontainerSystemd:
		// this means you asked systemd to manage cgroups, but systemd was not on the host, so all you can do is panic...
		// 判断"/run/systemd/system"是否存在且是一个目录且创建一个systemdDbus成功
		if !cgroupsystemd.UseSystemd() {
			panic("systemd cgroup manager not available")
		}
		return &cgroupsystemd.LegacyManager{
			Cgroups: cgroups,
			Paths:   paths,
		}, nil
	}
	return nil, fmt.Errorf("invalid cgroup manager configuration")
}

// CgroupSubsystems holds information about the mounted cgroup subsystems
type CgroupSubsystems struct {
	// Cgroup subsystem mounts.
	// e.g.: "/sys/fs/cgroup/cpu" -> ["cpu", "cpuacct"]
	Mounts []libcontainercgroups.Mount

	// Cgroup subsystem to their mount location.
	// e.g.: "cpu" -> "/sys/fs/cgroup/cpu"
	MountPoints map[string]string
}

// cgroupManagerImpl implements the CgroupManager interface.
// Its a stateless object which can be used to
// update,create or delete any number of cgroups
// It uses the Libcontainer raw fs cgroup manager for cgroup management.
type cgroupManagerImpl struct {
	// subsystems holds information about all the
	// mounted cgroup subsystems on the node
	subsystems *CgroupSubsystems
	// simplifies interaction with libcontainer and its cgroup managers
	adapter *libcontainerAdapter
}

// Make sure that cgroupManagerImpl implements the CgroupManager interface
var _ CgroupManager = &cgroupManagerImpl{}

// NewCgroupManager is a factory method that returns a CgroupManager
func NewCgroupManager(cs *CgroupSubsystems, cgroupDriver string) CgroupManager {
	managerType := libcontainerCgroupfs
	if cgroupDriver == string(libcontainerSystemd) {
		managerType = libcontainerSystemd
	}
	return &cgroupManagerImpl{
		subsystems: cs,
		adapter:    newLibcontainerAdapter(managerType),
	}
}

// Name converts the cgroup to the driver specific value in cgroupfs form.
// This always returns a valid cgroupfs path even when systemd driver is in use!
// cgroup driver为systemd，比如{"kubepods", "burstable", "pod1234-abcd-5678-efgh"} becomes "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice"
// cgroup driver为cgroupfs ，则"/kubepods/burstable/pod1234_abcd_5678_efgh"
func (m *cgroupManagerImpl) Name(name CgroupName) string {
	if m.adapter.cgroupManagerType == libcontainerSystemd {
		return name.ToSystemd()
	}
	return name.ToCgroupfs()
}

// CgroupName converts the literal cgroupfs name on the host to an internal identifier.
func (m *cgroupManagerImpl) CgroupName(name string) CgroupName {
	if m.adapter.cgroupManagerType == libcontainerSystemd {
		return ParseSystemdToCgroupName(name)
	}
	return ParseCgroupfsToCgroupName(name)
}

// buildCgroupPaths builds a path to each cgroup subsystem for the specified name.
// 在m.subsystems.MountPoints里每个cgroup子系统的路径都添加上转成cgroup driver的路径
func (m *cgroupManagerImpl) buildCgroupPaths(name CgroupName) map[string]string {
	// 转成cgroup driver的路径名
	cgroupFsAdaptedName := m.Name(name)
	cgroupPaths := make(map[string]string, len(m.subsystems.MountPoints))
	for key, val := range m.subsystems.MountPoints {
		cgroupPaths[key] = path.Join(val, cgroupFsAdaptedName)
	}
	return cgroupPaths
}

// TODO(filbranden): This logic belongs in libcontainer/cgroup/systemd instead.
// It should take a libcontainerconfigs.Cgroup.Path field (rather than Name and Parent)
// and split it appropriately, using essentially the logic below.
// This was done for cgroupfs in opencontainers/runc#497 but a counterpart
// for systemd was never introduced.
// 
// 设置cgroupConfig的Name（转成systemd路径的目录名）和Parent（转成systemd路径的父目录）
// 比如  cgroupConfig.Name为[]string["kubepods", "besteffort", "pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"]
// libcontainerCgroupConfig.Name为"kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"，libcontainerCgroupConfig.Parent为"/kubepods-besteffort.slice"
func updateSystemdCgroupInfo(cgroupConfig *libcontainerconfigs.Cgroup, cgroupName CgroupName) {
	// cgroupName转成systemd cgroup路径，比如["kubepods"]转成"/kubepods.slice"
	dir, base := path.Split(cgroupName.ToSystemd())
	if dir == "/" {
		dir = "-.slice"
	} else {
		dir = path.Base(dir)
	}
	cgroupConfig.Parent = dir
	cgroupConfig.Name = base
}

// Exists checks if all subsystem cgroups already exist
func (m *cgroupManagerImpl) Exists(name CgroupName) bool {
	// Get map of all cgroup paths on the system for the particular cgroup
	// 如果的name为空，在cgroup driver为systemd情况下，则cgroupPaths为各个子系统在系统中的挂载，比如cpu-->/sys/fs/cgroup/cpu,cpuacct
	// 如果是name是["system"]，在cgroup driver为systemd情况下，则cgroupPaths里 cpu挂载-->/sys/fs/cgroup/cpu,cpuacct/system.slice
	// 如果是cgroup driver为cgroupfs，name是["system"]，cgroupPaths里 cpu挂载-->/sys/fs/cgroup/cpu,cpuacct/system
	cgroupPaths := m.buildCgroupPaths(name)

	// the presence of alternative control groups not known to runc confuses
	// the kubelet existence checks.
	// ideally, we would have a mechanism in runc to support Exists() logic
	// scoped to the set control groups it understands.  this is being discussed
	// in https://github.com/opencontainers/runc/issues/1440
	// once resolved, we can remove this code.
	whitelistControllers := sets.NewString("cpu", "cpuacct", "cpuset", "memory", "systemd")
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportPodPidsLimit) || utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportNodePidsLimit) {
		whitelistControllers.Insert("pids")
	}
	if _, ok := m.subsystems.MountPoints["hugetlb"]; ok {
		whitelistControllers.Insert("hugetlb")
	}
	var missingPaths []string
	// If even one cgroup path doesn't exist, then the cgroup doesn't exist.
	for controller, path := range cgroupPaths {
		// ignore mounts we don't care about
		if !whitelistControllers.Has(controller) {
			continue
		}
		if !libcontainercgroups.PathExists(path) {
			missingPaths = append(missingPaths, path)
		}
	}

	if len(missingPaths) > 0 {
		klog.V(4).Infof("The Cgroup %v has some missing paths: %v", name, missingPaths)
		return false
	}

	return true
}

// Destroy destroys the specified cgroup
// 移除所有cgroup子系统里的cgroup路径
func (m *cgroupManagerImpl) Destroy(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("destroy").Observe(metrics.SinceInSeconds(start))
	}()

	// 在m.subsystems.MountPoints里每个cgroup子系统的路径都添加上转成cgroup driver的路径
	cgroupPaths := m.buildCgroupPaths(cgroupConfig.Name)

	libcontainerCgroupConfig := &libcontainerconfigs.Cgroup{}
	// libcontainer consumes a different field and expects a different syntax
	// depending on the cgroup driver in use, so we need this conditional here.
	if m.adapter.cgroupManagerType == libcontainerSystemd {
		// 设置cgroupConfig的Name（转成systemd路径的目录名）和Parent（转成systemd路径的父目录）
		// 比如  cgroupConfig.Name为[]string["kubepods", "besteffort", "pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"]
		// libcontainerCgroupConfig.Name为"kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"，libcontainerCgroupConfig.Parent为"/kubepods-besteffort.slice"
		updateSystemdCgroupInfo(libcontainerCgroupConfig, cgroupConfig.Name)
	} else {
		// 返回类似"/{cgroup name[0]}/{cgroup name[1]}..."
		// 比如 cgroupConfig.Name为[]string["kubepods", "besteffort", "pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"]
		// libcontainerCgroupConfig.Path为"/kubepods/besteffort/pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"
		libcontainerCgroupConfig.Path = cgroupConfig.Name.ToCgroupfs()
	}

	// 根据不同的cgroup driver类型，创建runc里的cgroup manager
	manager, err := m.adapter.newManager(libcontainerCgroupConfig, cgroupPaths)
	if err != nil {
		return err
	}

	// Delete cgroups using libcontainers Managers Destroy() method
	// systemd为driver
	// 调用systemd dbus停止unit(比如"kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice")
	// 删除manager.paths里的所有路径（包括路径下所有文件和文件夹）
	// 重置manager.Paths为空map[string]string
	// 
	// cgroupfs为driver
	// 删除manager.paths里的所有路径（包括路径下的所有文件和文件夹）
	// 重置manager.Paths为空map[string]string
	if err = manager.Destroy(); err != nil {
		return fmt.Errorf("unable to destroy cgroup paths for cgroup %v : %v", cgroupConfig.Name, err)
	}

	return nil
}

type subsystem interface {
	// Name returns the name of the subsystem.
	Name() string
	// Set the cgroup represented by cgroup.
	Set(path string, cgroup *libcontainerconfigs.Cgroup) error
	// GetStats returns the statistics associated with the cgroup
	GetStats(path string, stats *libcontainercgroups.Stats) error
}

// getSupportedSubsystems returns a map of subsystem and if it must be mounted for the kubelet to function.
// cpu为true、memory为true、pid为true（开启了SupportPodPidsLimit或SupportNodePidsLimit）、hugetlb为false
func getSupportedSubsystems() map[subsystem]bool {
	supportedSubsystems := map[subsystem]bool{
		&cgroupfs.MemoryGroup{}: true,
		&cgroupfs.CpuGroup{}:    true,
		&cgroupfs.PidsGroup{}:   false,
	}
	// not all hosts support hugetlb cgroup, and in the absent of hugetlb, we will fail silently by reporting no capacity.
	supportedSubsystems[&cgroupfs.HugetlbGroup{}] = false
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportPodPidsLimit) || utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportNodePidsLimit) {
		supportedSubsystems[&cgroupfs.PidsGroup{}] = true
	}
	return supportedSubsystems
}

// setSupportedSubsystems sets cgroup resource limits only on the supported
// subsystems. ie. cpu and memory. We don't use libcontainer's cgroup/fs/Set()
// method as it doesn't allow us to skip updates on the devices cgroup
// Allowing or denying all devices by writing 'a' to devices.allow or devices.deny is
// not possible once the device cgroups has children. Once the pod level cgroup are
// created under the QOS level cgroup we cannot update the QOS level device cgroup.
// We would like to skip setting any values on the device cgroup in this case
// but this is not possible with libcontainers Set() method
// See https://github.com/opencontainers/runc/issues/932
func setSupportedSubsystems(cgroupConfig *libcontainerconfigs.Cgroup) error {
	// cpu、memory、pid（开启了SupportPodPidsLimit或SupportNodePidsLimit）为必须的，hugepage非必须
	for sys, required := range getSupportedSubsystems() {
		if _, ok := cgroupConfig.Paths[sys.Name()]; !ok {
			if required {
				return fmt.Errorf("failed to find subsystem mount for required subsystem: %v", sys.Name())
			}
			// the cgroup is not mounted, but its not required so continue...
			klog.V(6).Infof("Unable to find subsystem mount for optional subsystem: %v", sys.Name())
			continue
		}
		// 设置各个cgroup系统（cpu、memory、pid、hugepage（在系统中开启的hugepage类型））的属性
		if err := sys.Set(cgroupConfig.Paths[sys.Name()], cgroupConfig); err != nil {
			return fmt.Errorf("failed to set config for supported subsystems : %v", err)
		}
	}
	return nil
}

// ResourceConfig转换成runc的libcontainer的Resources
func (m *cgroupManagerImpl) toResources(resourceConfig *ResourceConfig) *libcontainerconfigs.Resources {
	resources := &libcontainerconfigs.Resources{}
	if resourceConfig == nil {
		return resources
	}
	if resourceConfig.Memory != nil {
		resources.Memory = *resourceConfig.Memory
	}
	if resourceConfig.CpuShares != nil {
		resources.CpuShares = *resourceConfig.CpuShares
	}
	if resourceConfig.CpuQuota != nil {
		resources.CpuQuota = *resourceConfig.CpuQuota
	}
	if resourceConfig.CpuPeriod != nil {
		resources.CpuPeriod = *resourceConfig.CpuPeriod
	}
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportPodPidsLimit) || utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportNodePidsLimit) {
		if resourceConfig.PidsLimit != nil {
			resources.PidsLimit = *resourceConfig.PidsLimit
		}
	}
	// if huge pages are enabled, we set them in libcontainer
	// for each page size enumerated, set that value
	pageSizes := sets.NewString()
	for pageSize, limit := range resourceConfig.HugePageLimit {
		sizeString, err := v1helper.HugePageUnitSizeFromByteSize(pageSize)
		if err != nil {
			klog.Warningf("pageSize is invalid: %v", err)
			continue
		}
		resources.HugetlbLimit = append(resources.HugetlbLimit, &libcontainerconfigs.HugepageLimit{
			Pagesize: sizeString,
			Limit:    uint64(limit),
		})
		pageSizes.Insert(sizeString)
	}
	// for each page size omitted, limit to 0
	// resourceConfig.HugePageLimit里没有设置的其他pagesize，设置limit为0
	for _, pageSize := range cgroupfs.HugePageSizes {
		if pageSizes.Has(pageSize) {
			continue
		}
		resources.HugetlbLimit = append(resources.HugetlbLimit, &libcontainerconfigs.HugepageLimit{
			Pagesize: pageSize,
			Limit:    uint64(0),
		})
	}
	return resources
}

// Update updates the cgroup with the specified Cgroup Configuration
// 更新各个（cpu、memory、pid、hugepage）cgroup属性值
func (m *cgroupManagerImpl) Update(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("update").Observe(metrics.SinceInSeconds(start))
	}()

	// Extract the cgroup resource parameters
	resourceConfig := cgroupConfig.ResourceParameters
	// ResourceConfig转换成runc的libcontainer的Resources
	resources := m.toResources(resourceConfig)

	//比如cgroupConfig.Name为["kubepods"]，则cgroupPaths为/sys/fs/cgroup/{cgroup subsystem}/kubepods.slice
	cgroupPaths := m.buildCgroupPaths(cgroupConfig.Name)

	libcontainerCgroupConfig := &libcontainerconfigs.Cgroup{
		Resources: resources,
		Paths:     cgroupPaths,
	}
	// libcontainer consumes a different field and expects a different syntax
	// depending on the cgroup driver in use, so we need this conditional here.
	if m.adapter.cgroupManagerType == libcontainerSystemd {
		// 设置libcontainerCgroupConfig的Name（cgroupConfig.Name转成systemd路径的目录名）和Parent（cgroupConfig.Name转成systemd路径的父目录）
		// 比如  cgroupConfig.Name为[]string["kubepods", "besteffort", "pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"]
		// libcontainerCgroupConfig.Name为"kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"，libcontainerCgroupConfig.Parent为"/kubepods-besteffort.slice"
		updateSystemdCgroupInfo(libcontainerCgroupConfig, cgroupConfig.Name)
	} else {
		// 比如上面例子，则libcontainerCgroupConfig.Path为"/kubepods/besteffort/pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"
		libcontainerCgroupConfig.Path = cgroupConfig.Name.ToCgroupfs()
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportPodPidsLimit) && cgroupConfig.ResourceParameters != nil && cgroupConfig.ResourceParameters.PidsLimit != nil {
		libcontainerCgroupConfig.PidsLimit = *cgroupConfig.ResourceParameters.PidsLimit
	}

	// 设置各个cgroup系统（cpu、memory、pid、hugepage在系统中开启的）的属性
	if err := setSupportedSubsystems(libcontainerCgroupConfig); err != nil {
		return fmt.Errorf("failed to set supported cgroup subsystems for cgroup %v: %v", cgroupConfig.Name, err)
	}
	return nil
}

// Create creates the specified cgroup
// cgroup driver为systemd
//   1. 调用systemdDbus执行systemd临时unit（类似执行systemd-run命令）来创建相应的cgroup目录，并设置相应的资源属性值（MemoryLimit、"CPUShares"、"CPUQuotaPerSecUSec"、"BlockIOWeight"、"TasksMax"）这里没有支持cpuset设置，这个需要systemd244版本
//   2. 设置各个cgroup系统（cpu、memory、pid、hugepage在系统中开启的）的属性
// cgroup driver为cgroupfs
//   1. 创建各个cgroup系统目录，设置一些基本的cgroup子系统属性
//   2. 设置各个cgroup系统（cpu、memory、pid、hugepage在系统中开启的）的属性
func (m *cgroupManagerImpl) Create(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("create").Observe(metrics.SinceInSeconds(start))
	}()

	// ResourceConfig转换成runc的libcontainer的Resources
	resources := m.toResources(cgroupConfig.ResourceParameters)

	libcontainerCgroupConfig := &libcontainerconfigs.Cgroup{
		Resources: resources,
	}
	// libcontainer consumes a different field and expects a different syntax
	// depending on the cgroup driver in use, so we need this conditional here.
	if m.adapter.cgroupManagerType == libcontainerSystemd {
		// 设置libcontainerCgroupConfig的Name（转成systemd路径的目录名）和Parent（转成systemd路径的父目录）
		// 比如  cgroupConfig.Name为[]string["kubepods", "besteffort", "pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"]
		// libcontainerCgroupConfig.Name为"kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"，libcontainerCgroupConfig.Parent为"/kubepods-besteffort.slice"
		updateSystemdCgroupInfo(libcontainerCgroupConfig, cgroupConfig.Name)
	} else {
		// 拼成路径
		// 比如上面例子，则libcontainerCgroupConfig.Path为"/kubepods/besteffort/pod33affd6b_2117_4b1a_9b47_4d33869b6ea1"
		libcontainerCgroupConfig.Path = cgroupConfig.Name.ToCgroupfs()
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportPodPidsLimit) && cgroupConfig.ResourceParameters != nil && cgroupConfig.ResourceParameters.PidsLimit != nil {
		libcontainerCgroupConfig.PidsLimit = *cgroupConfig.ResourceParameters.PidsLimit
	}

	// get the manager with the specified cgroup configuration
	manager, err := m.adapter.newManager(libcontainerCgroupConfig, nil)
	if err != nil {
		return err
	}

	// Apply(-1) is a hack to create the cgroup directories for each resource
	// subsystem. The function [cgroups.Manager.apply()] applies cgroup
	// configuration to the process with the specified pid.
	// It creates cgroup files for each subsystems and writes the pid
	// in the tasks file. We use the function to create all the required
	// cgroup files but not attach any "real" pid to the cgroup.
	// cgroup driver为systemd，比如cgroupConfig.Name为["kubepods"]，创建/sys/fs/cgroup/{cgroup system}/kubepods.slice目录和设置相应的cgroup属性
	//   1. 调用systemdDbus执行systemd临时unit（类似执行systemd-run命令）来创建相应的cgroup目录，并设置相应的资源属性值（MemoryLimit、"CPUShares"、"CPUQuotaPerSecUSec"、"BlockIOWeight"、"TasksMax"）这里没有支持cpuset设置，这个需要systemd244版本
	// 
	// cgroup driver为cgroupfs，比如cgroupConfig.Name为["kubepods"]，创建/sys/fs/cgroup/{cgroup system}/kubepods目录和设置相应的cgroup属性
	//  1. 创建各个cgroup系统目录，cpuset和memory和cpu设置一些基本的属性
	if err := manager.Apply(-1); err != nil {
		return err
	}

	// it may confuse why we call set after we do apply, but the issue is that runc
	// follows a similar pattern.  it's needed to ensure cpu quota is set properly.
	// 这里因为runc库里的apply不会设置所有子系统的所有属性，需要手动调用必须的cgroup子系统的Set()方法，设置这些需要的cgroup子系统的属性（比如cgroup driver为cgroupfs的memory子系统的"memory.limit_in_bytes"，所有cgroup driver设置"cpu.cfs_period_us"）
	// 设置各个cgroup系统（cpu、memory、pid、hugepage在系统中开启的）的属性
	if err := m.Update(cgroupConfig); err != nil {
		utilruntime.HandleError(fmt.Errorf("cgroup update failed %v", err))
	}

	return nil
}

// Scans through all subsystems to find pids associated with specified cgroup.
// 获得所有cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
func (m *cgroupManagerImpl) Pids(name CgroupName) []int {
	// we need the driver specific name
	// cgroup driver为systemd，比如{"kubepods", "burstable", "pod1234-abcd-5678-efgh"} becomes "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice"
	// cgroup driver为cgroupfs ，则"/kubepods/burstable/pod1234_abcd_5678_efgh"
	cgroupFsName := m.Name(name)

	// Get a list of processes that we need to kill
	pidsToKill := sets.NewInt()
	var pids []int
	for _, val := range m.subsystems.MountPoints {
		dir := path.Join(val, cgroupFsName)
		_, err := os.Stat(dir)
		if os.IsNotExist(err) {
			// The subsystem pod cgroup is already deleted
			// do nothing, continue
			continue
		}
		// Get a list of pids that are still charged to the pod's cgroup
		// 获取"{dir}/cgroup.procs"里的文件内容里的所有pid
		pids, err = getCgroupProcs(dir)
		if err != nil {
			continue
		}
		pidsToKill.Insert(pids...)

		// WalkFunc which is called for each file and directory in the pod cgroup dir
		visitor := func(path string, info os.FileInfo, err error) error {
			if err != nil {
				klog.V(4).Infof("cgroup manager encountered error scanning cgroup path %q: %v", path, err)
				return filepath.SkipDir
			}
			if !info.IsDir() {
				return nil
			}
			// 获取"{path}/cgroup.procs"里的文件内容里的所有pid
			pids, err = getCgroupProcs(path)
			if err != nil {
				klog.V(4).Infof("cgroup manager encountered error getting procs for cgroup path %q: %v", path, err)
				return filepath.SkipDir
			}
			pidsToKill.Insert(pids...)
			return nil
		}
		// Walk through the pod cgroup directory to check if
		// container cgroups haven't been GCed yet. Get attached processes to
		// all such unwanted containers under the pod cgroup
		// 遍历dir目录下所有的子cgroup，获得所有attached pid
		if err = filepath.Walk(dir, visitor); err != nil {
			klog.V(4).Infof("cgroup manager encountered error scanning pids for directory: %q: %v", dir, err)
		}
	}
	return pidsToKill.List()
}

// ReduceCPULimits reduces the cgroup's cpu shares to the lowest possible value
// 设置cgroup name的cpu share的值为2
func (m *cgroupManagerImpl) ReduceCPULimits(cgroupName CgroupName) error {
	// Set lowest possible CpuShares value for the cgroup
	minimumCPUShares := uint64(MinShares)
	resources := &ResourceConfig{
		CpuShares: &minimumCPUShares,
	}
	containerConfig := &CgroupConfig{
		Name:               cgroupName,
		ResourceParameters: resources,
	}
	// 更新各个（cpu、memory、pid、hugepage）cgroup属性值
	return m.Update(containerConfig)
}

func getStatsSupportedSubsystems(cgroupPaths map[string]string) (*libcontainercgroups.Stats, error) {
	stats := libcontainercgroups.NewStats()
	// cpu为true、memory为true、pid为true（开启了SupportPodPidsLimit或SupportNodePidsLimit）、hugetlb为false
	for sys, required := range getSupportedSubsystems() {
		if _, ok := cgroupPaths[sys.Name()]; !ok {
			if required {
				return nil, fmt.Errorf("failed to find subsystem mount for required subsystem: %v", sys.Name())
			}
			// the cgroup is not mounted, but its not required so continue...
			klog.V(6).Infof("Unable to find subsystem mount for optional subsystem: %v", sys.Name())
			continue
		}
		if err := sys.GetStats(cgroupPaths[sys.Name()], stats); err != nil {
			return nil, fmt.Errorf("failed to get stats for supported subsystems : %v", err)
		}
	}
	return stats, nil
}

func toResourceStats(stats *libcontainercgroups.Stats) *ResourceStats {
	return &ResourceStats{
		MemoryStats: &MemoryStats{
			Usage: int64(stats.MemoryStats.Usage.Usage),
		},
	}
}

// Get sets the ResourceParameters of the specified cgroup as read from the cgroup fs
// 只返回cgroup里memory的memory.usage_in_bytes 
func (m *cgroupManagerImpl) GetResourceStats(name CgroupName) (*ResourceStats, error) {
	cgroupPaths := m.buildCgroupPaths(name)
	stats, err := getStatsSupportedSubsystems(cgroupPaths)
	if err != nil {
		return nil, fmt.Errorf("failed to get stats supported cgroup subsystems for cgroup %v: %v", name, err)
	}
	return toResourceStats(stats), nil
}
