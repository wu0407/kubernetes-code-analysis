// +build linux

package fs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/configs"
	libcontainerUtils "github.com/opencontainers/runc/libcontainer/utils"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

var (
	subsystemsLegacy = subsystemSet{
		&CpusetGroup{},
		&DevicesGroup{},
		&MemoryGroup{},
		&CpuGroup{},
		&CpuacctGroup{},
		&PidsGroup{},
		&BlkioGroup{},
		&HugetlbGroup{},
		&NetClsGroup{},
		&NetPrioGroup{},
		&PerfEventGroup{},
		&FreezerGroup{},
		&NameGroup{GroupName: "name=systemd", Join: true},
	}
	// 系统支持的hugepage类型列表
	HugePageSizes, _ = cgroups.GetHugePageSize()
)

var errSubsystemDoesNotExist = fmt.Errorf("cgroup: subsystem does not exist")

type subsystemSet []subsystem

// 返回相应name的cgroup子系统--实现subsystem的对应结构体
func (s subsystemSet) Get(name string) (subsystem, error) {
	for _, ss := range s {
		if ss.Name() == name {
			return ss, nil
		}
	}
	return nil, errSubsystemDoesNotExist
}

type subsystem interface {
	// Name returns the name of the subsystem.
	Name() string
	// Returns the stats, as 'stats', corresponding to the cgroup under 'path'.
	GetStats(path string, stats *cgroups.Stats) error
	// Removes the cgroup represented by 'cgroupData'.
	Remove(*cgroupData) error
	// Creates and joins the cgroup represented by 'cgroupData'.
	Apply(*cgroupData) error
	// Set the cgroup represented by cgroup.
	Set(path string, cgroup *configs.Cgroup) error
}

type Manager struct {
	mu       sync.Mutex
	Cgroups  *configs.Cgroup
	Rootless bool // ignore permission-related errors
	Paths    map[string]string
}

// The absolute path to the root of the cgroup hierarchies.
var cgroupRootLock sync.Mutex
var cgroupRoot string

// Gets the cgroupRoot.
// 返回cgroup子系统挂载父路径，比如返回"/sys/fs/cgroup"
func getCgroupRoot() (string, error) {
	cgroupRootLock.Lock()
	defer cgroupRootLock.Unlock()

	if cgroupRoot != "" {
		return cgroupRoot, nil
	}

	// 从"/proc/self/mountinfo"找到" 第一个cgroup子系统挂载路径，返回其父目录
	// 比如返回"/sys/fs/cgroup"
	root, err := cgroups.FindCgroupMountpointDir()
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(root); err != nil {
		return "", err
	}

	cgroupRoot = root
	return cgroupRoot, nil
}

type cgroupData struct {
	root      string
	innerPath string
	config    *configs.Cgroup
	pid       int
}

// isIgnorableError returns whether err is a permission error (in the loose
// sense of the word). This includes EROFS (which for an unprivileged user is
// basically a permission error) and EACCES (for similar reasons) as well as
// the normal EPERM.
func isIgnorableError(rootless bool, err error) bool {
	// We do not ignore errors if we are root.
	if !rootless {
		return false
	}
	// Is it an ordinary EPERM?
	if os.IsPermission(errors.Cause(err)) {
		return true
	}

	// Try to handle other errnos.
	var errno error
	switch err := errors.Cause(err).(type) {
	case *os.PathError:
		errno = err.Err
	case *os.LinkError:
		errno = err.Err
	case *os.SyscallError:
		errno = err.Err
	}
	return errno == unix.EROFS || errno == unix.EPERM || errno == unix.EACCES
}

// 返回所有cgroup子系统集合--各个子系统结构体（实现subsystem接口）集合
func (m *Manager) getSubsystems() subsystemSet {
	return subsystemsLegacy
}

// 创建各个cgroup系统目录，并将pid（不为-1时）加入（覆盖）到这个cgroup中"cgroup.procs"文件
func (m *Manager) Apply(pid int) (err error) {
	if m.Cgroups == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var c = m.Cgroups

	// 返回cgroupData
	// return &cgroupData{
	// 	root:    cgroup挂载的父路径，比如" /sys/fs/cgroup"
	// 	innerPath: 这个实例的cgroup路径, 即m.Cgroups.Path路径或m.Cgroups.Parent和m.Cgroups.Name拼起来的路径
	// 	config:    c,
	// 	pid:       pid,
	// }
	d, err := getCgroupData(m.Cgroups, pid)
	if err != nil {
		return err
	}

	m.Paths = make(map[string]string)
	// 已经知道cgroup子系统和对应挂载路径，这个在重复执行的时候会使用到
	if c.Paths != nil {
		for name, path := range c.Paths {
			// 返回cgroup subsystem子系统的绝对路径
			_, err := d.path(name)
			if err != nil {
				if cgroups.IsNotFound(err) {
					continue
				}
				return err
			}
			m.Paths[name] = path
		}
		return cgroups.EnterPid(m.Paths, pid)
	}

	// c.Paths为空，则遍历执行所有cgroup子系统来设置c.Paths，一般这个在第一次执行的时候
	for _, sys := range m.getSubsystems() {
		// TODO: Apply should, ideally, be reentrant or be broken up into a separate
		// create and join phase so that the cgroup hierarchy for a container can be
		// created then join consists of writing the process pids to cgroup.procs
		// 返回cgroup subsystem子系统的绝对路径
		p, err := d.path(sys.Name())
		if err != nil {
			// The non-presence of the devices subsystem is
			// considered fatal for security reasons.
			if cgroups.IsNotFound(err) && sys.Name() != "devices" {
				continue
			}
			return err
		}
		m.Paths[sys.Name()] = p

		// cpuset:
		//   1. 递归的创建cpuset的cgroup父目录
		//   2. 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
		//   3. 创建cpuset的cgroup目录
		//   4. cgroup里设置了CpusetCpus和CpusetMems值，则写入进程cpuset的cgroup目录下"cpuset.cpus"和"cpuset.mems"文件
		//   5. 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
		//   6. 将pid写入（覆盖）到cpuset的cgroup下的"cgroup.procs"文件，最多重试5次
		// memory:
		//   1. 根据cgroup.Resources判断是否要设置memory cgroup
		//   2. 如果cgroup路径下面没有子cgroup路径且cgroup.procs里没有进程绑定了，则将"-1"写入到path下的memory.kmem.limit_in_bytes文件，设置为无限制
		//   3. 将pid加入到subsystem cgroup下的"cgroup.procs"文件
		//   4. 发生任何错误移除memory subsystem子系统的绝对路径
		// cpu:
		//   1. 创建path cgroup路径
		//   2. 如果配置里CpuRtPeriod不为0，则将值写入到path下的"cpu.rt_period_us"文件
		//   3. 如果配置里CpuRtRuntime不为0，则将值写入到path下的"cpu.rt_runtime_us"文件
		//   4. 将d.pid写入（覆盖）到path下的"cgroup.procs"文件，最多重试5次
		// devices、cpuacct、pid、blkio、hugetlb、net_cls、net_prio、pref_event、freezer、"name=systemd":
		//   1. 创建 subsystem cgroup子系统路径
		//   2. 将pid加入（覆盖）到 subsystem cgroup下的"cgroup.procs"文件
		if err := sys.Apply(d); err != nil {
			// In the case of rootless (including euid=0 in userns), where an explicit cgroup path hasn't
			// been set, we don't bail on error in case of permission problems.
			// Cases where limits have been set (and we couldn't create our own
			// cgroup) are handled by Set.
			// rootless（m.Rootless为true，非root模式）且错误是权限错误且m.Cgroups.Path为空，则忽略这个错误并将这个cgroup子系统从m.Paths移除，下次执行的时候不再执行这个cgroup子系统
			if isIgnorableError(m.Rootless, err) && m.Cgroups.Path == "" {
				delete(m.Paths, sys.Name())
				continue
			}
			return err
		}

	}
	return nil
}

// 删除m.paths里的所有路径（包括路径下的所有文件和文件夹）
// 重置m.Paths为空map[string]string
func (m *Manager) Destroy() error {
	if m.Cgroups == nil || m.Cgroups.Paths != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// 删除m.paths里的所有路径（包括路径下的所有文件和文件夹）
	if err := cgroups.RemovePaths(m.Paths); err != nil {
		return err
	}
	m.Paths = make(map[string]string)
	return nil
}

func (m *Manager) GetPaths() map[string]string {
	m.mu.Lock()
	paths := m.Paths
	m.mu.Unlock()
	return paths
}

func (m *Manager) GetUnifiedPath() (string, error) {
	return "", errors.New("unified path is only supported when running in unified mode")
}

// 获取cgroup里的cpu、memory、hugetlb、pid、blkio状态
func (m *Manager) GetStats() (*cgroups.Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := cgroups.NewStats()
	// m.Paths为cgroup子系统和对应的cgroup挂载路径
	for name, path := range m.Paths {
		// 返回相应为name的cgroup子系统结构体--实现subsystem结构体
		sys, err := m.getSubsystems().Get(name)
		// 不支持cgroup子系统或相应的cgroup路径不存在，则跳过
		if err == errSubsystemDoesNotExist || !cgroups.PathExists(path) {
			continue
		}
		// cpu读取cpu.stat文件，设置stats.CpuStats.ThrottlingData.Periods、stats.CpuStats.ThrottlingData.ThrottledPeriods、stats.CpuStats.ThrottlingData.ThrottledTime
		// cpuacct读取cpuacct.stat文件，设置stats.CpuStats.CpuUsage.UsageInUsermode、stats.CpuStats.CpuUsage.UsageInKernelmode。读取cpuacct.usage文件，设置stats.CpuStats.CpuUsage.TotalUsage。读取cpuacct.usage_percpu，设置stats.CpuStats.CpuUsage.PercpuUsage
		// cpuset不做任何事
		// devices不做任何事
		// freezer不做任何事
		// hugetlb读取每种pagesize的hugetlb.{pageSize}.usage_in_bytes，设置stats.HugetlbStats[pageSize].Usage。读取hugetlb.{pageSize}.max_usage_in_bytes，设置stats.HugetlbStats[pageSize].MaxUsage。读取hugetlb.{pageSize}.usage_in_bytes.failcnt，设置stats.HugetlbStats[pageSize].Failcnt
		// memory读取memory.stat，设置stats.MemoryStats.Stats[{各项内存项}]、stats.MemoryStats.Cache。读取memory.usage_in_bytes，设置stats.MemoryStats.Usage.Usage。读取memory.max_usage_in_bytes，设置stats.MemoryStats.Usage.MaxUsage。读取memory.failcnt，设置stats.MemoryStats.Usage.Failcnt。读取memory.limit_in_bytes，设置stats.MemoryStats.Usage.Limit。
		// 读取memory.memsw.usage_in_bytes，设置stats.MemoryStats.SwapUsage.Usage。读取memory.memsw.max_usage_in_bytes，设置stats.MemoryStats.SwapUsage.MaxUsage。读取memory.memsw.failcnt，设置stats.memsw.MemoryStats.SwapUsage.Failcnt。读取memory.memsw.limit_in_bytes，设置stats.MemoryStats.SwapUsage.Limit。
		// 读取memory.kmem.usage_in_bytes，设置stats.MemoryStats.KernelUsage.Usage。读取memory.kmem.max_usage_in_bytes，设置stats.MemoryStats.KernelUsage.MaxUsage。读取memory.kmem.failcnt，设置stats.kmem.MemoryStats.KernelUsage.Failcnt。读取memory.kmem.limit_in_bytes，设置stats.MemoryStats.KernelUsage.Limit。
		// 读取memory.kmem.tcp.usage_in_bytes，设置stats.MemoryStats.KernelTCPUsage.Usage。读取memory.kmem.tcp.max_usage_in_bytes，设置stats.MemoryStats.KernelTCPUsage.MaxUsage。读取memory.kmem.tcp.failcnt，设置stats.kmem.MemoryStats.KernelTCPUsage.Failcnt。读取memory.kmem.tcp.limit_in_bytes，设置stats.MemoryStats.KernelTCPUsage.Limit。
		// 读取memory.use_hierarchy，设置stats.MemoryStats.UseHierarchy
		// name=systemd不做任何事
		// net_cls不做任何事
		// net_prio不做任何事
		// perf_event不做任何事
		// pids读取pids.current，设置stats.PidsStats.Current。读取pids.max，设置stats.PidsStats.Limit。
		// blkio 读取blkio.io_serviced_recursive，如果有数据，则读取blkio.sectors_recursive，设置stats.BlkioStats.SectorsRecursive。读取blkio.io_service_bytes_recursive，设置stats.BlkioStats.IoServiceBytesRecursive。读取blkio.io_serviced_recursive，设置stats.BlkioStats.IoServicedRecursive。读取blkio.io_queued_recursive，设置stats.BlkioStats.IoQueuedRecursive。读取blkio.io_service_time_recursive，设置stats.BlkioStats.IoServiceTimeRecursive。读取blkio.io_wait_time_recursive，设置stats.BlkioStats.IoWaitTimeRecursive。读取blkio.io_merged_recursive，设置stats.BlkioStats.IoMergedRecursive。读取blkio.time_recursive，设置stats.BlkioStats.IoTimeRecursive
		// 否则读取blkio.throttle.io_service_bytes，设置stats.BlkioStats.IoServiceBytesRecursive。读取blkio.throttle.io_serviced，设置stats.BlkioStats.IoServicedRecursive
		if err := sys.GetStats(path, stats); err != nil {
			return nil, err
		}
	}
	return stats, nil
}

func (m *Manager) Set(container *configs.Config) error {
	if container.Cgroups == nil {
		return nil
	}

	// If Paths are set, then we are just joining cgroups paths
	// and there is no need to set any values.
	if m.Cgroups != nil && m.Cgroups.Paths != nil {
		return nil
	}

	paths := m.GetPaths()
	for _, sys := range m.getSubsystems() {
		path := paths[sys.Name()]
		if err := sys.Set(path, container.Cgroups); err != nil {
			if m.Rootless && sys.Name() == "devices" {
				continue
			}
			// When m.Rootless is true, errors from the device subsystem are ignored because it is really not expected to work.
			// However, errors from other subsystems are not ignored.
			// see @test "runc create (rootless + limits + no cgrouppath + no permission) fails with informative error"
			if path == "" {
				// We never created a path for this cgroup, so we cannot set
				// limits for it (though we have already tried at this point).
				return fmt.Errorf("cannot set %s limit: container could not join or create cgroup", sys.Name())
			}
			return err
		}
	}

	if m.Paths["cpu"] != "" {
		if err := CheckCpushares(m.Paths["cpu"], container.Cgroups.Resources.CpuShares); err != nil {
			return err
		}
	}
	return nil
}

// Freeze toggles the container's freezer cgroup depending on the state
// provided
func (m *Manager) Freeze(state configs.FreezerState) error {
	if m.Cgroups == nil {
		return errors.New("cannot toggle freezer: cgroups not configured for container")
	}

	paths := m.GetPaths()
	dir := paths["freezer"]
	prevState := m.Cgroups.Resources.Freezer
	m.Cgroups.Resources.Freezer = state
	freezer, err := m.getSubsystems().Get("freezer")
	if err != nil {
		return err
	}
	err = freezer.Set(dir, m.Cgroups)
	if err != nil {
		m.Cgroups.Resources.Freezer = prevState
		return err
	}
	return nil
}

func (m *Manager) GetPids() ([]int, error) {
	paths := m.GetPaths()
	return cgroups.GetPids(paths["devices"])
}

// 读取devices子系统的cgroup目录和子目录下的cgroup.procs，返回所有pid
func (m *Manager) GetAllPids() ([]int, error) {
	// 获得所有cgroup子系统与相应的目录
	paths := m.GetPaths()
	// 读取devices子系统的cgroup目录和子目录下的cgroup.procs，返回所有pid
	return cgroups.GetAllPids(paths["devices"])
}

func getCgroupData(c *configs.Cgroup, pid int) (*cgroupData, error) {
	// 返回cgroup子系统挂载父路径，比如返回"/sys/fs/cgroup"
	root, err := getCgroupRoot()
	if err != nil {
		return nil, err
	}

	// c.Name、c.Parent不能和c.Path一起存在
	if (c.Name != "" || c.Parent != "") && c.Path != "" {
		return nil, fmt.Errorf("cgroup: either Path or Name and Parent should be used")
	}

	// XXX: Do not remove this code. Path safety is important! -- cyphar
	cgPath := libcontainerUtils.CleanPath(c.Path)
	cgParent := libcontainerUtils.CleanPath(c.Parent)
	cgName := libcontainerUtils.CleanPath(c.Name)

	innerPath := cgPath
	if innerPath == "" {
		innerPath = filepath.Join(cgParent, cgName)
	}

	return &cgroupData{
		root:      root,
		innerPath: innerPath,
		config:    c,
		pid:       pid,
	}, nil
}

// 返回cgroup subsystem子系统的绝对路径，比如"/sys/fs/cgroup/cpu,cpuacct/system.slice/kubelet.service"
func (raw *cgroupData) path(subsystem string) (string, error) {
	// 如果是cgroup2子系统，返回"/sys/fs/cgroup"
	// 否则cgroup1子系统，找到subsystem子系统在/proc/self/mountinfo信息里挂载目录和root目录
	// 比如raw.root为"/sys/fs/cgroup" subsystem为"cpu", 返回"/sys/fs/cgroup/cpu,cpuacct"
	mnt, err := cgroups.FindCgroupMountpoint(raw.root, subsystem)
	// If we didn't mount the subsystem, there is no point we make the path.
	if err != nil {
		return "", err
	}

	// If the cgroup name/path is absolute do not look relative to the cgroup of the init process.
	if filepath.IsAbs(raw.innerPath) {
		// Sometimes subsystems can be mounted together as 'cpu,cpuacct'.
		// raw.innerPath是绝对路径，则返回拼接路径raw.root, filepath.Base(mnt), raw.innerPath
		// 比如raw.root为"/sys/fs/cgroup"，mnt为"/sys/fs/cgroup/cpu,cpuacct"， raw.innerPath为"/system.slice/kubelet.service"
		// 返回"/sys/fs/cgroup/cpu,cpuacct/system.slice/kubelet.service"
		return filepath.Join(raw.root, filepath.Base(mnt), raw.innerPath), nil
	}

	// Use GetOwnCgroupPath instead of GetInitCgroupPath, because the creating
	// process could in container and shared pid namespace with host, and
	// /proc/1/cgroup could point to whole other world of cgroups.
	// 进程的cgroup subsystem的绝对路径
	parentPath, err := cgroups.GetOwnCgroupPath(subsystem)
	if err != nil {
		return "", err
	}

	// raw.innerPath是相对路径，则返回拼合parentPath和raw.innerPath
	return filepath.Join(parentPath, raw.innerPath), nil
}

// 创建subsystem cgroup子系统路径
// 将raw.pid加入（覆盖）到subsystem cgroup下的"cgroup.procs"文件
func (raw *cgroupData) join(subsystem string) (string, error) {
	// 返回cgroup subsystem子系统的绝对路径，比如"/sys/fs/cgroup/cpu,cpuacct/system.slice/kubelet.service"
	path, err := raw.path(subsystem)
	if err != nil {
		return "", err
	}
	// 创建这个cgroup子系统路径
	if err := os.MkdirAll(path, 0755); err != nil {
		return "", err
	}
	// 将pid写入（覆盖）到的subsystem cgroup下的"cgroup.procs"文件，最多重试5次
	if err := cgroups.WriteCgroupProc(path, raw.pid); err != nil {
		return "", err
	}
	return path, nil
}

func removePath(p string, err error) error {
	if err != nil {
		return err
	}
	if p != "" {
		return os.RemoveAll(p)
	}
	return nil
}

func CheckCpushares(path string, c uint64) error {
	var cpuShares uint64

	if c == 0 {
		return nil
	}

	fd, err := os.Open(filepath.Join(path, "cpu.shares"))
	if err != nil {
		return err
	}
	defer fd.Close()

	_, err = fmt.Fscanf(fd, "%d", &cpuShares)
	if err != nil && err != io.EOF {
		return err
	}

	if c > cpuShares {
		return fmt.Errorf("The maximum allowed cpu-shares is %d", cpuShares)
	} else if c < cpuShares {
		return fmt.Errorf("The minimum allowed cpu-shares is %d", cpuShares)
	}

	return nil
}

func (m *Manager) GetCgroups() (*configs.Cgroup, error) {
	return m.Cgroups, nil
}
