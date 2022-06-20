// +build linux

package fs

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fscommon"
	"github.com/opencontainers/runc/libcontainer/configs"
	libcontainerUtils "github.com/opencontainers/runc/libcontainer/utils"
)

type CpusetGroup struct {
}

func (s *CpusetGroup) Name() string {
	return "cpuset"
}

// 递归的创建cpuset的cgroup父目录
// 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
// 创建cpuset的cgroup目录
// cgroup里设置了CpusetCpus和CpusetMems值，则写入进程cpuset的cgroup目录下"cpuset.cpus"和"cpuset.mems"文件
// 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
// 将pid（不为-1时）写入（覆盖）到进程cpuset的cgroup下的"cgroup.procs"文件，最多重试5次
func (s *CpusetGroup) Apply(d *cgroupData) error {
	// 返回cpuset的cgroup绝对路径，比如"/sys/fs/cgroup/cpuset/system.slice/kubelet.service"
	dir, err := d.path("cpuset")
	if err != nil && !cgroups.IsNotFound(err) {
		return err
	}
	// 递归的创建进程cpuset的cgroup父目录
	// 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
	// 创建cpuset的cgroup目录
	// cgroup里设置了CpusetCpus和CpusetMems值，则写入进程cpuset的cgroup目录下"cpuset.cpus"和"cpuset.mems"文件
	// 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
	// 将pid（不为-1时）写入（覆盖）到cpuset的cgroup下的"cgroup.procs"文件，最多重试5次
	return s.ApplyDir(dir, d.config, d.pid)
}

// 设置了CpusetCpus和CpusetMems值，则写入path目录下"cpuset.cpus"和"cpuset.mems"文件
func (s *CpusetGroup) Set(path string, cgroup *configs.Cgroup) error {
	if cgroup.Resources.CpusetCpus != "" {
		if err := fscommon.WriteFile(path, "cpuset.cpus", cgroup.Resources.CpusetCpus); err != nil {
			return err
		}
	}
	if cgroup.Resources.CpusetMems != "" {
		if err := fscommon.WriteFile(path, "cpuset.mems", cgroup.Resources.CpusetMems); err != nil {
			return err
		}
	}
	return nil
}

func (s *CpusetGroup) Remove(d *cgroupData) error {
	return removePath(d.path("cpuset"))
}

func (s *CpusetGroup) GetStats(path string, stats *cgroups.Stats) error {
	return nil
}

// 递归的创建dir父目录
// 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
// 创建dir目录
// cgroup里设置了CpusetCpus和CpusetMems值，则写入dir目录下"cpuset.cpus"和"cpuset.mems"文件
// 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
// 将pid写入（覆盖）到dir下的"cgroup.procs"文件，最多重试5次
func (s *CpusetGroup) ApplyDir(dir string, cgroup *configs.Cgroup, pid int) error {
	// This might happen if we have no cpuset cgroup mounted.
	// Just do nothing and don't fail.
	if dir == "" {
		return nil
	}
	mountInfo, err := ioutil.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	// 从mountinfo中获取最长的匹配dir的挂载路径的父目录
	root := filepath.Dir(cgroups.GetClosestMountpointAncestor(dir, string(mountInfo)))
	// 'ensureParent' start with parent because we don't want to
	// explicitly inherit from parent, it could conflict with
	// 'cpuset.cpu_exclusive'.
	// 递归的创建dir父目录
	// 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为其父目录里对应文件的值
	if err := s.ensureParent(filepath.Dir(dir), root); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// We didn't inherit cpuset configs from parent, but we have
	// to ensure cpuset configs are set before moving task into the
	// cgroup.
	// The logic is, if user specified cpuset configs, use these
	// specified configs, otherwise, inherit from parent. This makes
	// cpuset configs work correctly with 'cpuset.cpu_exclusive', and
	// keep backward compatibility.
	// cgroup里设置了CpusetCpus和CpusetMems值，则写入dir目录下"cpuset.cpus"和"cpuset.mems"文件
	// 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
	if err := s.ensureCpusAndMems(dir, cgroup); err != nil {
		return err
	}

	// because we are not using d.join we need to place the pid into the procs file
	// unlike the other subsystems
	// 将pid写入（覆盖）到dir下的"cgroup.procs"文件，最多重试5次
	// 如果dir目录下"cpuset.cpus"和"cpuset.mems"文件为空，则写入dir下的"cgroup.procs"文件会报错"write error: No space left on device"
	return cgroups.WriteCgroupProc(dir, pid)
}

// 读取parent路径下面的"cpuset.cpus"和"cpuset.mems"文件，返回文件的内容
func (s *CpusetGroup) getSubsystemSettings(parent string) (cpus []byte, mems []byte, err error) {
	if cpus, err = ioutil.ReadFile(filepath.Join(parent, "cpuset.cpus")); err != nil {
		return
	}
	if mems, err = ioutil.ReadFile(filepath.Join(parent, "cpuset.mems")); err != nil {
		return
	}
	return cpus, mems, nil
}

// ensureParent makes sure that the parent directory of current is created
// and populated with the proper cpus and mems files copied from
// it's parent.
// 递归的创建current目录和父目录
// 如果递归路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
func (s *CpusetGroup) ensureParent(current, root string) error {
	parent := filepath.Dir(current)
	// root是current的父目录，直接返回
	// 这个是递归的退出条件，即目录递归到root
	if libcontainerUtils.CleanPath(parent) == root {
		return nil
	}
	// Avoid infinite recursion.
	// 如果current父目录等于current，说明是根路径，就直接报错
	if parent == current {
		return fmt.Errorf("cpuset: cgroup parent path outside cgroup root")
	}
	// 对父目录进行相同的操作
	if err := s.ensureParent(parent, root); err != nil {
		return err
	}
	// 创建相应的current目录
	if err := os.MkdirAll(current, 0755); err != nil {
		return err
	}
	// 如果current路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
	return s.copyIfNeeded(current, parent)
}

// copyIfNeeded copies the cpuset.cpus and cpuset.mems from the parent
// directory to the current directory if the file's contents are 0
// 如果current路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
func (s *CpusetGroup) copyIfNeeded(current, parent string) error {
	var (
		err                      error
		currentCpus, currentMems []byte
		parentCpus, parentMems   []byte
	)

	// 读取current路径下面的"cpuset.cpus"和"cpuset.mems"文件，返回文件的内容
	if currentCpus, currentMems, err = s.getSubsystemSettings(current); err != nil {
		return err
	}
	// 读取parent路径下面的"cpuset.cpus"和"cpuset.mems"文件，返回文件的内容
	if parentCpus, parentMems, err = s.getSubsystemSettings(parent); err != nil {
		return err
	}

	if s.isEmpty(currentCpus) {
		// current目录下"cpuset.cpus"为空，则"cpuset.cpus"写入父目录的"cpuset.cpus"的值
		if err := fscommon.WriteFile(current, "cpuset.cpus", string(parentCpus)); err != nil {
			return err
		}
	}
	if s.isEmpty(currentMems) {
		// current目录下"cpuset.mems"为空，则"cpuset.mems"写入父目录的"cpuset.mems"的值
		if err := fscommon.WriteFile(current, "cpuset.mems", string(parentMems)); err != nil {
			return err
		}
	}
	return nil
}

func (s *CpusetGroup) isEmpty(b []byte) bool {
	return len(bytes.Trim(b, "\n")) == 0
}

// cgroup设置了CpusetCpus和CpusetMems值，则写入path目录下"cpuset.cpus"和"cpuset.mems"文件 
// 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
func (s *CpusetGroup) ensureCpusAndMems(path string, cgroup *configs.Cgroup) error {
	// 设置了CpusetCpus和CpusetMems值，则写入path目录下"cpuset.cpus"和"cpuset.mems"文件 
	if err := s.Set(path, cgroup); err != nil {
		return err
	}
	// 如果path路径下面的"cpuset.cpus"和"cpuset.mems"文件的内容为空，则设置为父目录里对应文件的值
	return s.copyIfNeeded(path, filepath.Dir(path))
}
