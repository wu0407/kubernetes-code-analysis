// Copyright 2016 Google Inc. All Rights Reserved.
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

package common

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/cadvisor/container"
	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/utils"
	"github.com/karrick/godirwalk"
	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/pkg/errors"

	"k8s.io/klog"
)

func DebugInfo(watches map[string][]string) map[string][]string {
	out := make(map[string][]string)

	lines := make([]string, 0, len(watches))
	for containerName, cgroupWatches := range watches {
		lines = append(lines, fmt.Sprintf("%s:", containerName))
		for _, cg := range cgroupWatches {
			lines = append(lines, fmt.Sprintf("\t%s", cg))
		}
	}
	out["Inotify watches"] = lines

	return out
}

// findFileInAncestorDir returns the path to the parent directory that contains the specified file.
// "" is returned if the lookup reaches the limit.
func findFileInAncestorDir(current, file, limit string) (string, error) {
	for {
		fpath := path.Join(current, file)
		_, err := os.Stat(fpath)
		if err == nil {
			return current, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if current == limit {
			return "", nil
		}
		current = filepath.Dir(current)
	}
}

// 获得container的CreationTime、cpu cgroup的各种属性（cpu.shares，cpu.cfs_period_us，cpu.cfs_quota_us）、memory的（memory.limit_in_bytes、memory.memsw.limit_in_bytes、memory.soft_limit_in_bytes）、pid的pids.max、是否有文件系统、是否有网络、是否有blkio
func GetSpec(cgroupPaths map[string]string, machineInfoFactory info.MachineInfoFactory, hasNetwork, hasFilesystem bool) (info.ContainerSpec, error) {
	var spec info.ContainerSpec

	// Assume unified hierarchy containers.
	// Get the lowest creation time from all hierarchies as the container creation time.
	now := time.Now()
	lowestTime := now
	for _, cgroupPath := range cgroupPaths {
		// The modified time of the cgroup directory changes whenever a subcontainer is created.
		// eg. /docker will have creation time matching the creation of latest docker container.
		// Use clone_children as a workaround as it isn't usually modified. It is only likely changed
		// immediately after creating a container.
		cgroupPath = path.Join(cgroupPath, "cgroup.clone_children")
		fi, err := os.Stat(cgroupPath)
		if err == nil && fi.ModTime().Before(lowestTime) {
			lowestTime = fi.ModTime()
		}
	}
	if lowestTime != now {
		spec.CreationTime = lowestTime
	}

	// Get machine info.
	mi, err := machineInfoFactory.GetMachineInfo()
	if err != nil {
		return spec, err
	}

	// CPU.
	// 容器的cpu cgroup路径
	cpuRoot, ok := cgroupPaths["cpu"]
	// 获得cpu cgroup的各种属性（cpu.shares，cpu.cfs_period_us，cpu.cfs_quota_us）
	if ok {
		if utils.FileExists(cpuRoot) {
			spec.HasCpu = true
			spec.Cpu.Limit = readUInt64(cpuRoot, "cpu.shares")
			spec.Cpu.Period = readUInt64(cpuRoot, "cpu.cfs_period_us")
			quota := readString(cpuRoot, "cpu.cfs_quota_us")

			if quota != "" && quota != "-1" {
				val, err := strconv.ParseUint(quota, 10, 64)
				if err != nil {
					klog.Errorf("GetSpec: Failed to parse CPUQuota from %q: %s", path.Join(cpuRoot, "cpu.cfs_quota_us"), err)
				}
				spec.Cpu.Quota = val
			}
		}
	}

	// Cpu Mask.
	// This will fail for non-unified hierarchies. We'll return the whole machine mask in that case.
	cpusetRoot, ok := cgroupPaths["cpuset"]
	if ok {
		if utils.FileExists(cpusetRoot) {
			spec.HasCpu = true
			mask := ""
			if cgroups.IsCgroup2UnifiedMode() {
				mask = readString(cpusetRoot, "cpuset.cpus.effective")
			} else {
				mask = readString(cpusetRoot, "cpuset.cpus")
			}
			spec.Cpu.Mask = utils.FixCpuMask(mask, mi.NumCores)
		}
	}

	// Memory
	memoryRoot, ok := cgroupPaths["memory"]
	if ok {
		if !cgroups.IsCgroup2UnifiedMode() {
			if utils.FileExists(memoryRoot) {
				spec.HasMemory = true
				spec.Memory.Limit = readUInt64(memoryRoot, "memory.limit_in_bytes")
				spec.Memory.SwapLimit = readUInt64(memoryRoot, "memory.memsw.limit_in_bytes")
				spec.Memory.Reservation = readUInt64(memoryRoot, "memory.soft_limit_in_bytes")
			}
		} else {
			// 从{memoryRoot}到各级的上级目录中查找memory.max文件，如果没有查找则在/sys/fs/cgroup停止查找。返回找到的文件的目录
			memoryRoot, err := findFileInAncestorDir(memoryRoot, "memory.max", "/sys/fs/cgroup")
			if err != nil {
				return spec, err
			}
			if memoryRoot != "" {
				spec.HasMemory = true
				spec.Memory.Reservation = readUInt64(memoryRoot, "memory.high")
				spec.Memory.Limit = readUInt64(memoryRoot, "memory.max")
				spec.Memory.SwapLimit = readUInt64(memoryRoot, "memory.swap.max")
			}
		}
	}

	// Processes, read it's value from pids path directly
	pidsRoot, ok := cgroupPaths["pids"]
	if ok {
		if utils.FileExists(pidsRoot) {
			spec.HasProcesses = true
			spec.Processes.Limit = readUInt64(pidsRoot, "pids.max")
		}
	}

	spec.HasNetwork = hasNetwork
	spec.HasFilesystem = hasFilesystem

	ioControllerName := "blkio"
	if cgroups.IsCgroup2UnifiedMode() {
		ioControllerName = "io"
	}
	if blkioRoot, ok := cgroupPaths[ioControllerName]; ok && utils.FileExists(blkioRoot) {
		spec.HasDiskIo = true
	}

	return spec, nil
}

func readString(dirpath string, file string) string {
	cgroupFile := path.Join(dirpath, file)

	// Read
	out, err := ioutil.ReadFile(cgroupFile)
	if err != nil {
		// Ignore non-existent files
		if !os.IsNotExist(err) {
			klog.Warningf("readString: Failed to read %q: %s", cgroupFile, err)
		}
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readUInt64(dirpath string, file string) uint64 {
	out := readString(dirpath, file)
	if out == "" || out == "max" {
		return 0
	}

	val, err := strconv.ParseUint(out, 10, 64)
	if err != nil {
		klog.Errorf("readUInt64: Failed to parse int %q from file %q: %s", out, path.Join(dirpath, file), err)
		return 0
	}

	return val
}

// Lists all directories under "path" and outputs the results as children of "parent".
func ListDirectories(dirpath string, parent string, recursive bool, output map[string]struct{}) error {
	buf := make([]byte, godirwalk.DefaultScratchBufferSize)
	return listDirectories(dirpath, parent, recursive, output, buf)
}

func listDirectories(dirpath string, parent string, recursive bool, output map[string]struct{}, buf []byte) error {
	dirents, err := godirwalk.ReadDirents(dirpath, buf)
	if err != nil {
		// Ignore if this hierarchy does not exist.
		if os.IsNotExist(errors.Cause(err)) {
			err = nil
		}
		return err
	}
	for _, dirent := range dirents {
		// We only grab directories.
		if !dirent.IsDir() {
			continue
		}
		dirname := dirent.Name()

		// output返回为{parent}/{dirname}，去除了cgroup子系统的路径，比如dirpath为/sys/fs/cgroup/cpu， parent为/，/sys/fs/cgroup/cpu目录下面有system.service目录，则name为"/system.service"
		name := path.Join(parent, dirname)
		output[name] = struct{}{}

		// List subcontainers if asked to.
		if recursive {
			err := listDirectories(path.Join(dirpath, dirname), name, true, output, buf)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// 所有mountPoints的路径后面都添加name
func MakeCgroupPaths(mountPoints map[string]string, name string) map[string]string {
	cgroupPaths := make(map[string]string, len(mountPoints))
	for key, val := range mountPoints {
		cgroupPaths[key] = path.Join(val, name)
	}

	return cgroupPaths
}

func CgroupExists(cgroupPaths map[string]string) bool {
	// If any cgroup exists, the container is still alive.
	for _, cgroupPath := range cgroupPaths {
		if utils.FileExists(cgroupPath) {
			return true
		}
	}
	return false
}

// 获得所有cgroup子系统路径下的所有目录，当listType == container.ListRecursive，会递归获取所有子目录。
// 返回的目录为/{name}/{子目录}，/{name}/{子目录}/{子目录}
// 并封装成info.ContainerReference，返回[]info.ContainerReference
func ListContainers(name string, cgroupPaths map[string]string, listType container.ListType) ([]info.ContainerReference, error) {
	containers := make(map[string]struct{})
	// 获得所有cgroup子系统路径下的所有目录，当listType == container.ListRecursive，会递归获取所有子目录
	// 返回的目录为/{name}/{子目录}，/{name}/{子目录}/{子目录}
	// 比如/system.slice、/system.slice/kubelet.service，就是真实路径去除/sys/fs/cgroup
	for _, cgroupPath := range cgroupPaths {
		err := ListDirectories(cgroupPath, name, listType == container.ListRecursive, containers)
		if err != nil {
			return nil, err
		}
	}

	// Make into container references.
	ret := make([]info.ContainerReference, 0, len(containers))
	for cont := range containers {
		ret = append(ret, info.ContainerReference{
			Name: cont,
		})
	}

	return ret, nil
}

// AssignDeviceNamesToDiskStats assigns the Device field on the provided DiskIoStats by looking up
// the device major and minor identifiers in the provided device namer.
// 设置stats.IoMerged、stats.IoQueued、stats.IoServiceBytes、stats.IoServiceTime、stats.IoServiced、stats.IoTime、stats.IoWaitTimestats、stats.Sectors的Device字段（设备名称）
func AssignDeviceNamesToDiskStats(namer DeviceNamer, stats *info.DiskIoStats) {
	assignDeviceNamesToPerDiskStats(
		namer,
		stats.IoMerged,
		stats.IoQueued,
		stats.IoServiceBytes,
		stats.IoServiceTime,
		stats.IoServiced,
		stats.IoTime,
		stats.IoWaitTime,
		stats.Sectors,
	)
}

// assignDeviceNamesToPerDiskStats looks up device names for the provided stats, caching names
// if necessary.
// 从MachineInfo中查找设备的名称，并设置到diskStats[*].Device中
func assignDeviceNamesToPerDiskStats(namer DeviceNamer, diskStats ...[]info.PerDiskStats) {
	devices := make(deviceIdentifierMap)
	for _, stats := range diskStats {
		for i, stat := range stats {
			stats[i].Device = devices.Find(stat.Major, stat.Minor, namer)
		}
	}
}

// DeviceNamer returns string names for devices by their major and minor id.
type DeviceNamer interface {
	// DeviceName returns the name of the device by its major and minor ids, or false if no
	// such device is recognized.
	DeviceName(major, minor uint64) (string, bool)
}

type MachineInfoNamer info.MachineInfo

// 从machineinfo中DiskMap（块设备大小、设备号、io调度算法）查找major, minor对应的块设备名称，如果找到则返回"/dev/{设备名}"
// 上面没有找到，则从machineinfo中的Filesystems（所有挂载源设备的磁盘信息）查找major, minor对应的块设备名称
func (n *MachineInfoNamer) DeviceName(major, minor uint64) (string, bool) {
	for _, info := range n.DiskMap {
		if info.Major == major && info.Minor == minor {
			return "/dev/" + info.Name, true
		}
	}
	for _, info := range n.Filesystems {
		if info.DeviceMajor == major && info.DeviceMinor == minor {
			return info.Device, true
		}
	}
	return "", false
}

type deviceIdentifier struct {
	major uint64
	minor uint64
}

type deviceIdentifierMap map[deviceIdentifier]string

// Find locates the device name by device identifier out of from, caching the result as necessary.
// 先从自身（cache）查找，没有找到则从MachineInfo中查找，查到的结果缓存（保存）到自身
func (m deviceIdentifierMap) Find(major, minor uint64, namer DeviceNamer) string {
	d := deviceIdentifier{major, minor}
	// 从deviceIdentifierMap中查找
	if s, ok := m[d]; ok {
		return s
	}
	// 从MachineInfo中获取major, minor对应的块设备名称
	s, _ := namer.DeviceName(major, minor)
	// 查找到块设备名称，保存到deviceIdentifierMap，方便下次查找
	m[d] = s
	return s
}
