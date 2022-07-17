// +build linux

package systemd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	systemdDbus "github.com/coreos/go-systemd/dbus"
	"github.com/godbus/dbus"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fs"
	"github.com/opencontainers/runc/libcontainer/configs"
	"github.com/sirupsen/logrus"
)

type LegacyManager struct {
	mu      sync.Mutex
	Cgroups *configs.Cgroup
	Paths   map[string]string
}

type subsystem interface {
	// Name returns the name of the subsystem.
	Name() string
	// Returns the stats, as 'stats', corresponding to the cgroup under 'path'.
	GetStats(path string, stats *cgroups.Stats) error
	// Set the cgroup represented by cgroup.
	Set(path string, cgroup *configs.Cgroup) error
}

var errSubsystemDoesNotExist = errors.New("cgroup: subsystem does not exist")

type subsystemSet []subsystem

func (s subsystemSet) Get(name string) (subsystem, error) {
	for _, ss := range s {
		if ss.Name() == name {
			return ss, nil
		}
	}
	return nil, errSubsystemDoesNotExist
}

var legacySubsystems = subsystemSet{
	&fs.CpusetGroup{},
	&fs.DevicesGroup{},
	&fs.MemoryGroup{},
	&fs.CpuGroup{},
	&fs.CpuacctGroup{},
	&fs.PidsGroup{},
	&fs.BlkioGroup{},
	&fs.HugetlbGroup{},
	&fs.PerfEventGroup{},
	&fs.FreezerGroup{},
	&fs.NetPrioGroup{},
	&fs.NetClsGroup{},
	&fs.NameGroup{GroupName: "name=systemd"},
}

const (
	testScopeWait = 4
	testSliceWait = 4
)

var (
	connLock sync.Mutex
	theConn  *systemdDbus.Conn
)

func newProp(name string, units interface{}) systemdDbus.Property {
	return systemdDbus.Property{
		Name:  name,
		Value: dbus.MakeVariant(units),
	}
}

// NOTE: This function comes from package github.com/coreos/go-systemd/util
// It was borrowed here to avoid a dependency on cgo.
//
// IsRunningSystemd checks whether the host was booted with systemd as its init
// system. This functions similarly to systemd's `sd_booted(3)`: internally, it
// checks whether /run/systemd/system/ exists and is a directory.
// http://www.freedesktop.org/software/systemd/man/sd_booted.html
// 判断"/run/systemd/system"是否存在且是一个目录
func isRunningSystemd() bool {
	fi, err := os.Lstat("/run/systemd/system")
	if err != nil {
		return false
	}
	return fi.IsDir()
}

// 判断"/run/systemd/system"是否存在且是一个目录且创建一个systemdDbus成功
func UseSystemd() bool {
	// 判断"/run/systemd/system"是否存在且是一个目录
	if !isRunningSystemd() {
		return false
	}

	connLock.Lock()
	defer connLock.Unlock()

	if theConn == nil {
		var err error
		// 新建一个systemdbus连接
		theConn, err = systemdDbus.New()
		if err != nil {
			return false
		}
	}
	return true
}

func NewSystemdCgroupsManager() (func(config *configs.Cgroup, paths map[string]string) cgroups.Manager, error) {
	if !isRunningSystemd() {
		return nil, fmt.Errorf("systemd not running on this host, can't use systemd as a cgroups.Manager")
	}
	if cgroups.IsCgroup2UnifiedMode() {
		return func(config *configs.Cgroup, paths map[string]string) cgroups.Manager {
			return &UnifiedManager{
				Cgroups: config,
				Paths:   paths,
			}
		}, nil
	}
	return func(config *configs.Cgroup, paths map[string]string) cgroups.Manager {
		return &LegacyManager{
			Cgroups: config,
			Paths:   paths,
		}
	}, nil
}

// 执行systemd临时unit（类似执行systemd-run命令）来创建相应的cgroup目录，并设置相应的资源属性值（MemoryLimit、"CPUShares"、"CPUQuotaPerSecUSec"、"BlockIOWeight"、"TasksMax"）这里没有支持cpuset设置，这个需要systemd244版本
// 如果pid为-1，则直接返回。否则将pid写入到cgroup子系统下的"cgroup.procs"文件
func (m *LegacyManager) Apply(pid int) error {
	var (
		c          = m.Cgroups
		// c.Name不为".slice"结尾，则返回{c.ScopePrefix}-{c.Name}+".scope"
		// c.Name为".slice"结尾，则返回c.Name
		unitName   = getUnitName(c)
		slice      = "system.slice"
		properties []systemdDbus.Property
	)

	// 已经知道了所有cgroup子系统的路径
	if c.Paths != nil {
		paths := make(map[string]string)
		for name, path := range c.Paths {
			// 获得subsystem cgroup的路径，比如subsystem为"cpu", 返回"/sys/fs/cgroup/cpu,cpuacct/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"
			// 必须m.Cgroups.Name不为空
			_, err := getSubsystemPath(m.Cgroups, name)
			if err != nil {
				// Don't fail if a cgroup hierarchy was not found, just skip this subsystem
				if cgroups.IsNotFound(err) {
					continue
				}
				return err
			}
			paths[name] = path
		}
		m.Paths = paths
		// 遍历m.Paths将pid写入path下的"cgroup.procs"文件
		return cgroups.EnterPid(m.Paths, pid)
	}

	if c.Parent != "" {
		slice = c.Parent
	}

	properties = append(properties, systemdDbus.PropDescription("libcontainer container "+c.Name))

	// if we create a slice, the parent is defined via a Wants=
	if strings.HasSuffix(unitName, ".slice") {
		properties = append(properties, systemdDbus.PropWants(slice))
	} else {
		// otherwise, we use Slice=
		properties = append(properties, systemdDbus.PropSlice(slice))
	}

	// only add pid if its valid, -1 is used w/ general slice creation.
	if pid != -1 {
		properties = append(properties, newProp("PIDs", []uint32{uint32(pid)}))
	}

	// Check if we can delegate. This is only supported on systemd versions 218 and above.
	if !strings.HasSuffix(unitName, ".slice") {
		// Assume scopes always support delegation.
		// https://systemd.io/CGROUP_DELEGATION/#delegation
		// https://www.freedesktop.org/software/systemd/man/systemd.resource-control.html#Delegate=
		// 开启后将更多的资源控制交给进程自己管理。开启后unit可以在单其cgroup下创建和管理其自己的cgroup的私人子层级，systemd将不在维护其cgroup以及将其进程从unit的cgroup里移走
		properties = append(properties, newProp("Delegate", true))
	}

	// Always enable accounting, this gets us the same behaviour as the fs implementation,
	// plus the kernel has some problems with joining the memory cgroup at a later time.
	properties = append(properties,
		newProp("MemoryAccounting", true),
		newProp("CPUAccounting", true),
		newProp("BlockIOAccounting", true))

	// Assume DefaultDependencies= will always work (the check for it was previously broken.)
	// https://www.freedesktop.org/software/systemd/man/systemd.unit.html#DefaultDependencies=
	// If set to no, this option does not disable all implicit dependencies, just non-essential ones.
	properties = append(properties,
		newProp("DefaultDependencies", false))

	if c.Resources.Memory != 0 {
		properties = append(properties,
			newProp("MemoryLimit", uint64(c.Resources.Memory)))
	}

	if c.Resources.CpuShares != 0 {
		properties = append(properties,
			newProp("CPUShares", c.Resources.CpuShares))
	}

	// cpu.cfs_quota_us and cpu.cfs_period_us are controlled by systemd.
	if c.Resources.CpuQuota != 0 && c.Resources.CpuPeriod != 0 {
		// corresponds to USEC_INFINITY in systemd
		// if USEC_INFINITY is provided, CPUQuota is left unbound by systemd
		// always setting a property value ensures we can apply a quota and remove it later
		cpuQuotaPerSecUSec := uint64(math.MaxUint64)
		if c.Resources.CpuQuota > 0 {
			// systemd converts CPUQuotaPerSecUSec (microseconds per CPU second) to CPUQuota
			// (integer percentage of CPU) internally.  This means that if a fractional percent of
			// CPU is indicated by Resources.CpuQuota, we need to round up to the nearest
			// 10ms (1% of a second) such that child cgroups can set the cpu.cfs_quota_us they expect.
			// Resources.CpuQuota是多少个Resources.CpuPeriod
			// 首先将c.Resources.CpuQuota转成一个cpuPeriod为1000000 microseconds（即一个cpuPeriod为1s），c.Resources.CpuQuota*1000000 / c.Resources.CpuPeriod = cpuQuotaPerSecUSe
			// 在一个cpucpuPeriod为1秒microseconds的值，不能整除10000（10ms），则将cpuQuotaPerSecUSe向上取整，cpuQuotaPerSecUSec = ((cpuQuotaPerSecUSec / 10000) + 1) * 10000
			cpuQuotaPerSecUSec = uint64(c.Resources.CpuQuota*1000000) / c.Resources.CpuPeriod
			if cpuQuotaPerSecUSec%10000 != 0 {
				cpuQuotaPerSecUSec = ((cpuQuotaPerSecUSec / 10000) + 1) * 10000
			}
		}
		properties = append(properties,
			newProp("CPUQuotaPerSecUSec", cpuQuotaPerSecUSec))
	}

	if c.Resources.BlkioWeight != 0 {
		properties = append(properties,
			newProp("BlockIOWeight", uint64(c.Resources.BlkioWeight)))
	}

	if c.Resources.PidsLimit > 0 {
		properties = append(properties,
			newProp("TasksAccounting", true),
			newProp("TasksMax", uint64(c.Resources.PidsLimit)))
	}

	// We have to set kernel memory here, as we can't change it once
	// processes have been attached to the cgroup.
	if c.Resources.KernelMemory != 0 {
		// 创建memory的cgroup路径
		// 当memory cgroup的目录下没有绑定pid文件，设置memory的cgroup路径下的memory.kmem.limit_in_bytes为-1（无限制）
		if err := setKernelMemory(c); err != nil {
			return err
		}
	}

	statusChan := make(chan string, 1)
	// https://www.freedesktop.org/software/systemd/man/systemd-run.html
	// 类似执行systemd-run命令，运行临时的systemd unit
	if _, err := theConn.StartTransientUnit(unitName, "replace", properties, statusChan); err == nil {
		select {
		case <-statusChan:
		case <-time.After(time.Second):
			logrus.Warnf("Timed out while waiting for StartTransientUnit(%s) completion signal from dbus. Continuing...", unitName)
		}
	} else if !isUnitExists(err) {
		return err
	}

	// 将pid写入到cgroup子系统下的"cgroup.procs"文件
	// "name=systemd"不处理
	// "cpuset"
	//    1. 递归的创建cpuset的cgroup父目录
	//    2. 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
	//    3. 创建cpuset的cgroup目录
	//    4. cgroup里设置了CpusetCpus和CpusetMems值，则写入进程cpuset的cgroup目录下"cpuset.cpus"和"cpuset.mems"文件
	//    5. 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
	//    6. 如果pid为-1，则直接返回。否则将pid写入（覆盖）到进程cpuset的cgroup下的"cgroup.procs"文件，最多重试5次
	// "devices"、"memory"、"cpu"、"cpuacct"、"pids"、"hugetlb"、"blkio"、"perf_event"、"freezer"、"net_prio"、"net_cls"
	//   1. 创建subsystem cgroup子系统路径并将pid写入到subsystem cgroup子系统路径下的"cgroup.procs"文件，如果pid为-1，则直接返回。
	//   其中"devices"发生错误直接返回，其他子系统发生cgroup子系统NotFoundError不存错误可以忽略，其他错误直接返回
	if err := joinCgroups(c, pid); err != nil {
		return err
	}

	// 将所有存在的cgroup子系统路径写入到m.Paths
	paths := make(map[string]string)
	for _, s := range legacySubsystems {
		// 获得subsystem cgroup的路径，比如subsystem为"cpu", 返回"/sys/fs/cgroup/cpu,cpuacct/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"
		subsystemPath, err := getSubsystemPath(m.Cgroups, s.Name())
		if err != nil {
			// Don't fail if a cgroup hierarchy was not found, just skip this subsystem
			if cgroups.IsNotFound(err) {
				continue
			}
			return err
		}
		paths[s.Name()] = subsystemPath
	}
	m.Paths = paths
	return nil
}

// 调用systemd dbus停止unit
// 删除m.paths里的所有路径（包括路径下所有文件和文件夹）
// 重置m.Paths为空map[string]string
func (m *LegacyManager) Destroy() error {
	// 没有定义Paths，直接返回nil
	if m.Cgroups.Paths != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// getUnitName(m.Cgroups)
	// m.Cgroups.Name不为".slice"结尾，则返回{m.Cgroups.ScopePrefix}-{m.Cgroups.Name}+".scope"
	// m.Cgroups.Name为".slice"结尾，则返回m.Cgroups.Name
	// 使用systemd dbus停止unit
	theConn.StopUnit(getUnitName(m.Cgroups), "replace", nil)
	// 删除m.paths里的所有路径（包括路径下所有文件和文件夹）
	if err := cgroups.RemovePaths(m.Paths); err != nil {
		return err
	}
	// 重置m.Paths为空map[string]string
	m.Paths = make(map[string]string)
	return nil
}

func (m *LegacyManager) GetPaths() map[string]string {
	m.mu.Lock()
	paths := m.Paths
	m.mu.Unlock()
	return paths
}

func (m *LegacyManager) GetUnifiedPath() (string, error) {
	return "", errors.New("unified path is only supported when running in unified mode")
}

// 创建subsystem cgroup子系统路径并将pid写入到subsystem cgroup子系统路径下的"cgroup.procs"文件
func join(c *configs.Cgroup, subsystem string, pid int) (string, error) {
	// 获得subsystem cgroup的路径，比如subsystem为"cpu", 返回"/sys/fs/cgroup/cpu,cpuacct/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"
	path, err := getSubsystemPath(c, subsystem)
	if err != nil {
		return "", err
	}

	// 创建subsystem cgroup子系统路径
	if err := os.MkdirAll(path, 0755); err != nil {
		return "", err
	}
	// 将pid写入到path下的"cgroup.procs"文件，最多重试5次
	if err := cgroups.WriteCgroupProc(path, pid); err != nil {
		return "", err
	}
	return path, nil
}

// 将pid写入到cgroup子系统下的"cgroup.procs"文件
// "name=systemd"不处理
// "cpuset"
//    1. 递归的创建cpuset的cgroup父目录
//    2. 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
//    3. 创建cpuset的cgroup目录
//    4. cgroup里设置了CpusetCpus和CpusetMems值，则写入进程cpuset的cgroup目录下"cpuset.cpus"和"cpuset.mems"文件
//    5. 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
//    6. 如果pid为-1，则直接返回。否则将pid写入（覆盖）到进程cpuset的cgroup下的"cgroup.procs"文件，最多重试5次
// "devices"、"memory"、"cpu"、"cpuacct"、"pids"、"hugetlb"、"blkio"、"perf_event"、"freezer"、"net_prio"、"net_cls"
//   1. 创建subsystem cgroup子系统路径并将pid写入到subsystem cgroup子系统路径下的"cgroup.procs"文件，如果pid为-1，则直接返回。
//   其中"devices"发生错误直接返回，其他子系统发生cgroup子系统NotFoundError不存错误可以忽略，其他错误直接返回
func joinCgroups(c *configs.Cgroup, pid int) error {
	for _, sys := range legacySubsystems {
		name := sys.Name()
		switch name {
		case "name=systemd":
			// let systemd handle this
		case "cpuset":
			// 如果dir目录下"cpuset.cpus"和"cpuset.mems"文件为空，则写入dir下的"cgroup.procs"文件会报错"write error: No space left on device"，所以这里会特殊处理

			// 获得subsystem cgroup的路径，比如subsystem为"cpuset", 返回"/sys/fs/cgroup/cpuset/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"
			path, err := getSubsystemPath(c, name)
			// 忽略获取cpuset cgroup路径时候发生的NotFoundError错误
			if err != nil && !cgroups.IsNotFound(err) {
				return err
			}
			s := &fs.CpusetGroup{}
			// 递归的创建cpuset的cgroup父目录
			// 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
			// 创建cpuset的cgroup目录
			// cgroup里设置了CpusetCpus和CpusetMems值，则写入进程cpuset的cgroup目录下"cpuset.cpus"和"cpuset.mems"文件
			// 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
			// 将pid写入（覆盖）到进程cpuset的cgroup下的"cgroup.procs"文件，最多重试5次
			if err := s.ApplyDir(path, c, pid); err != nil {
				return err
			}
		default:
			// 创建subsystem cgroup子系统路径并将pid写入到subsystem cgroup子系统路径下的"cgroup.procs"文件
			_, err := join(c, name, pid)
			if err != nil {
				// Even if it's `not found` error, we'll return err
				// because devices cgroup is hard requirement for
				// container security.
				// 为devices cgroup，则返回任意错误
				if name == "devices" {
					return err
				}
				// For other subsystems, omit the `not found` error
				// because they are optional.
				// 其他cgroup子系统可以忽略不存在
				if !cgroups.IsNotFound(err) {
					return err
				}
			}
		}
	}

	return nil
}

// systemd represents slice hierarchy using `-`, so we need to follow suit when
// generating the path of slice. Essentially, test-a-b.slice becomes
// /test.slice/test-a.slice/test-a-b.slice.
func ExpandSlice(slice string) (string, error) {
	suffix := ".slice"
	// Name has to end with ".slice", but can't be just ".slice".
	if len(slice) < len(suffix) || !strings.HasSuffix(slice, suffix) {
		return "", fmt.Errorf("invalid slice name: %s", slice)
	}

	// Path-separators are not allowed.
	if strings.Contains(slice, "/") {
		return "", fmt.Errorf("invalid slice name: %s", slice)
	}

	var path, prefix string
	sliceName := strings.TrimSuffix(slice, suffix)
	// if input was -.slice, we should just return root now
	if sliceName == "-" {
		return "/", nil
	}
	for _, component := range strings.Split(sliceName, "-") {
		// test--a.slice isn't permitted, nor is -test.slice.
		if component == "" {
			return "", fmt.Errorf("invalid slice name: %s", slice)
		}

		// Append the component to the path and to the prefix.
		path += "/" + prefix + component + suffix
		prefix += component + "-"
	}
	return path, nil
}

// 获得subsystem cgroup的路径，比如subsystem为"cpu", 返回"/sys/fs/cgroup/cpu,cpuacct/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"
func getSubsystemPath(c *configs.Cgroup, subsystem string) (string, error) {
	// 如果是cgroup2子系统，返回"/sys/fs/cgroup"
	// 否则cgroup1子系统，找到subsystem子系统在/proc/self/mountinfo信息里挂载目录（匹配cgroupPath）和root目录
	// 比如cgroupPath为"/sys/fs/cgroup" subsystem为"cpu", 返回"/sys/fs/cgroup/cpu,cpuacct"
	mountpoint, err := cgroups.FindCgroupMountpoint(c.Path, subsystem)
	if err != nil {
		return "", err
	}

	// 从/proc/1/cgroup获得subsystem对应的相对subsystem cgroup根路径的路径，比如"cpu"对应"/"
	initPath, err := cgroups.GetInitCgroup(subsystem)
	if err != nil {
		return "", err
	}
	// if pid 1 is systemd 226 or later, it will be in init.scope, not the root
	initPath = strings.TrimSuffix(filepath.Clean(initPath), "init.scope")

	slice := "system.slice"
	if c.Parent != "" {
		slice = c.Parent
	}

	// 对"-"进行转换，test-a-b.slice becomes /test.slice/test-a.slice/test-a-b.slice.
	slice, err = ExpandSlice(slice)
	if err != nil {
		return "", err
	}

	return filepath.Join(mountpoint, initPath, slice, getUnitName(c)), nil
}

func (m *LegacyManager) Freeze(state configs.FreezerState) error {
	path, err := getSubsystemPath(m.Cgroups, "freezer")
	if err != nil {
		return err
	}
	prevState := m.Cgroups.Resources.Freezer
	m.Cgroups.Resources.Freezer = state
	freezer, err := legacySubsystems.Get("freezer")
	if err != nil {
		return err
	}
	err = freezer.Set(path, m.Cgroups)
	if err != nil {
		m.Cgroups.Resources.Freezer = prevState
		return err
	}
	return nil
}

func (m *LegacyManager) GetPids() ([]int, error) {
	path, err := getSubsystemPath(m.Cgroups, "devices")
	if err != nil {
		return nil, err
	}
	return cgroups.GetPids(path)
}

func (m *LegacyManager) GetAllPids() ([]int, error) {
	path, err := getSubsystemPath(m.Cgroups, "devices")
	if err != nil {
		return nil, err
	}
	return cgroups.GetAllPids(path)
}

func (m *LegacyManager) GetStats() (*cgroups.Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := cgroups.NewStats()
	for name, path := range m.Paths {
		sys, err := legacySubsystems.Get(name)
		if err == errSubsystemDoesNotExist || !cgroups.PathExists(path) {
			continue
		}
		if err := sys.GetStats(path, stats); err != nil {
			return nil, err
		}
	}

	return stats, nil
}

func (m *LegacyManager) Set(container *configs.Config) error {
	// If Paths are set, then we are just joining cgroups paths
	// and there is no need to set any values.
	if m.Cgroups.Paths != nil {
		return nil
	}
	for _, sys := range legacySubsystems {
		// Get the subsystem path, but don't error out for not found cgroups.
		path, err := getSubsystemPath(container.Cgroups, sys.Name())
		if err != nil && !cgroups.IsNotFound(err) {
			return err
		}

		if err := sys.Set(path, container.Cgroups); err != nil {
			return err
		}
	}

	if m.Paths["cpu"] != "" {
		if err := fs.CheckCpushares(m.Paths["cpu"], container.Cgroups.Resources.CpuShares); err != nil {
			return err
		}
	}
	return nil
}

// c.Name不为".slice"结尾，则返回{c.ScopePrefix}-{c.Name}+".scope"
// c.Name为".slice"结尾，则返回c.Name
func getUnitName(c *configs.Cgroup) string {
	// by default, we create a scope unless the user explicitly asks for a slice.
	if !strings.HasSuffix(c.Name, ".slice") {
		return fmt.Sprintf("%s-%s.scope", c.ScopePrefix, c.Name)
	}
	return c.Name
}

// 创建memory的cgroup路径
// 当memory cgroup的目录下没有绑定pid，设置memory的cgroup路径下的memory.kmem.limit_in_bytes为-1（无限制）
func setKernelMemory(c *configs.Cgroup) error {
	// 获得subsystem cgroup的路径，比如subsystem为"memory", 返回"/sys/fs/cgroup/memory/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod33affd6b_2117_4b1a_9b47_4d33869b6ea1.slice"
	path, err := getSubsystemPath(c, "memory")
	if err != nil && !cgroups.IsNotFound(err) {
		return err
	}

	// 创建memory cgroup的路径
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	// do not try to enable the kernel memory if we already have
	// tasks in the cgroup.
	// 读取memory cgroup的目录下"tasks"文件
	content, err := ioutil.ReadFile(filepath.Join(path, "tasks"))
	if err != nil {
		return err
	}
	if len(content) > 0 {
		return nil
	}
	// memory cgroup的路径里没有pid，则设置
	// 将"1"写入到path下的memory.kmem.limit_in_bytes文件
	// 如果发生了"EBUSY"错误，说明cgroup路径下面还有子cgroup路径或cgroup.procs里已经有进程绑定了，直接返回
	// 否则将"-1"写入到path下的memory.kmem.limit_in_bytes文件，设置为无限制
	return fs.EnableKernelMemoryAccounting(path)
}

// isUnitExists returns true if the error is that a systemd unit already exists.
func isUnitExists(err error) bool {
	if err != nil {
		if dbusError, ok := err.(dbus.Error); ok {
			return strings.Contains(dbusError.Name, "org.freedesktop.systemd1.UnitExists")
		}
	}
	return false
}

func (m *LegacyManager) GetCgroups() (*configs.Cgroup, error) {
	return m.Cgroups, nil
}
