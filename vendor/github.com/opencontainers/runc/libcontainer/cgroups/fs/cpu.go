// +build linux

package fs

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fscommon"
	"github.com/opencontainers/runc/libcontainer/configs"
)

type CpuGroup struct {
}

func (s *CpuGroup) Name() string {
	return "cpu"
}

// 创建path cgroup路径
// 如果配置里CpuRtPeriod不为0，则将值写入到path下的"cpu.rt_period_us"文件
// 如果配置里CpuRtRuntime不为0，则将值写入到path下的"cpu.rt_runtime_us"文件
// 将d.pid（不为-1时）写入（覆盖）到path下的"cgroup.procs"文件，最多重试5次 
func (s *CpuGroup) Apply(d *cgroupData) error {
	// We always want to join the cpu group, to allow fair cpu scheduling
	// on a container basis
	// 返回cpu subsystem子系统的绝对路径，比如"/sys/fs/cgroup/cpu/system.slice/kubelet.service"
	path, err := d.path("cpu")
	if err != nil && !cgroups.IsNotFound(err) {
		return err
	}
	// 创建path cgroup路径
	// 如果配置里CpuRtPeriod不为0，则将值写入到path下的"cpu.rt_period_us"文件
	// 如果配置里CpuRtRuntime不为0，则将值写入到path下的"cpu.rt_runtime_us"文件
	// 将pid（不为-1时）写入（覆盖）到path下的"cgroup.procs"文件，最多重试5次
	return s.ApplyDir(path, d.config, d.pid)
}

// 创建path cgroup路径
// 如果配置里CpuRtPeriod不为0，则将值写入到path下的"cpu.rt_period_us"文件
// 如果配置里CpuRtRuntime不为0，则将值写入到path下的"cpu.rt_runtime_us"文件
// 将pid（不为-1时）写入（覆盖）到path下的"cgroup.procs"文件，最多重试5次
func (s *CpuGroup) ApplyDir(path string, cgroup *configs.Cgroup, pid int) error {
	// This might happen if we have no cpu cgroup mounted.
	// Just do nothing and don't fail.
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	// We should set the real-Time group scheduling settings before moving
	// in the process because if the process is already in SCHED_RR mode
	// and no RT bandwidth is set, adding it will fail.
	// 如果CpuRtPeriod不为0，则将值写入到path下的"cpu.rt_period_us"文件
	// 如果CpuRtRuntime不为0，则将值写入到path下的"cpu.rt_runtime_us"文件
	if err := s.SetRtSched(path, cgroup); err != nil {
		return err
	}
	// because we are not using d.join we need to place the pid into the procs file
	// unlike the other subsystems
	// 将pid（不为-1）写入（覆盖）到path下的"cgroup.procs"文件，最多重试5次
	return cgroups.WriteCgroupProc(path, pid)
}

// 如果CpuRtPeriod不为0，则将值写入到path下的"cpu.rt_period_us"文件
// 如果CpuRtRuntime不为0，则将值写入到path下的"cpu.rt_runtime_us"文件
func (s *CpuGroup) SetRtSched(path string, cgroup *configs.Cgroup) error {
	if cgroup.Resources.CpuRtPeriod != 0 {
		if err := fscommon.WriteFile(path, "cpu.rt_period_us", strconv.FormatUint(cgroup.Resources.CpuRtPeriod, 10)); err != nil {
			return err
		}
	}
	if cgroup.Resources.CpuRtRuntime != 0 {
		if err := fscommon.WriteFile(path, "cpu.rt_runtime_us", strconv.FormatInt(cgroup.Resources.CpuRtRuntime, 10)); err != nil {
			return err
		}
	}
	return nil
}

func (s *CpuGroup) Set(path string, cgroup *configs.Cgroup) error {
	if cgroup.Resources.CpuShares != 0 {
		if err := fscommon.WriteFile(path, "cpu.shares", strconv.FormatUint(cgroup.Resources.CpuShares, 10)); err != nil {
			return err
		}
	}
	if cgroup.Resources.CpuPeriod != 0 {
		if err := fscommon.WriteFile(path, "cpu.cfs_period_us", strconv.FormatUint(cgroup.Resources.CpuPeriod, 10)); err != nil {
			return err
		}
	}
	if cgroup.Resources.CpuQuota != 0 {
		if err := fscommon.WriteFile(path, "cpu.cfs_quota_us", strconv.FormatInt(cgroup.Resources.CpuQuota, 10)); err != nil {
			return err
		}
	}
	return s.SetRtSched(path, cgroup)
}

func (s *CpuGroup) Remove(d *cgroupData) error {
	return removePath(d.path("cpu"))
}

func (s *CpuGroup) GetStats(path string, stats *cgroups.Stats) error {
	f, err := os.Open(filepath.Join(path, "cpu.stat"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		t, v, err := fscommon.GetCgroupParamKeyValue(sc.Text())
		if err != nil {
			return err
		}
		switch t {
		case "nr_periods":
			stats.CpuStats.ThrottlingData.Periods = v

		case "nr_throttled":
			stats.CpuStats.ThrottlingData.ThrottledPeriods = v

		case "throttled_time":
			stats.CpuStats.ThrottlingData.ThrottledTime = v
		}
	}
	return nil
}
