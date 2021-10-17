// Copyright 2014 Google Inc. All Rights Reserved.
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

package machine

import (
	"bytes"
	"flag"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/docker/docker/pkg/parsers/operatingsystem"
	"github.com/google/cadvisor/fs"
	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/utils/cloudinfo"
	"github.com/google/cadvisor/utils/sysfs"
	"github.com/google/cadvisor/utils/sysinfo"

	"k8s.io/klog"

	"golang.org/x/sys/unix"
)

const hugepagesDirectory = "/sys/kernel/mm/hugepages/"

var machineIdFilePath = flag.String("machine_id_file", "/etc/machine-id,/var/lib/dbus/machine-id", "Comma-separated list of files to check for machine-id. Use the first one that exists.")
var bootIdFilePath = flag.String("boot_id_file", "/proc/sys/kernel/random/boot_id", "Comma-separated list of files to check for boot-id. Use the first one that exists.")

func getInfoFromFiles(filePaths string) string {
	if len(filePaths) == 0 {
		return ""
	}
	for _, file := range strings.Split(filePaths, ",") {
		id, err := ioutil.ReadFile(file)
		if err == nil {
			return strings.TrimSpace(string(id))
		}
	}
	klog.Warningf("Couldn't collect info from any of the files in %q", filePaths)
	return ""
}

func Info(sysFs sysfs.SysFs, fsInfo fs.FsInfo, inHostNamespace bool) (*info.MachineInfo, error) {
	rootFs := "/"
	if !inHostNamespace {
		rootFs = "/rootfs"
	}

	cpuinfo, err := ioutil.ReadFile(filepath.Join(rootFs, "/proc/cpuinfo"))
	if err != nil {
		return nil, err
	}
	// cpu最高主频
	clockSpeed, err := GetClockSpeed(cpuinfo)
	if err != nil {
		return nil, err
	}

	// 最大内存，单位字节
	memoryCapacity, err := GetMachineMemoryCapacity()
	if err != nil {
		return nil, err
	}

	// hugepage的各个pagesize和这个pagesize的numPages
	hugePagesInfo, err := GetHugePagesInfo(hugepagesDirectory)
	if err != nil {
		return nil, err
	}

	// 获取各个挂载源（设备）的总的大小、free大小、非特权用户avail可用大小、inodes容量、free的inode数量、文件系统类型为vfs，设备信息（设备名、主次设备号）、磁盘的读写数据量和耗时统计
	filesystems, err := fsInfo.GetGlobalFsInfo()
	if err != nil {
		klog.Errorf("Failed to get global filesystem information: %v", err)
	}

	// 获得所有块设备大小、设备号、io调度算法
	diskMap, err := sysinfo.GetBlockDeviceInfo(sysFs)
	if err != nil {
		klog.Errorf("Failed to get disk map: %v", err)
	}

	// 获得所有网卡的名字、mac、mtu、speed
	netDevices, err := sysinfo.GetNetworkDevices(sysFs)
	if err != nil {
		klog.Errorf("Failed to get network devices: %v", err)
	}

	// 获得cpu的拓扑，物理cpu下包含多个core和每个core的缓存信息，core下面包含多个线程
	topology, numCores, err := GetTopology(sysFs, string(cpuinfo))
	if err != nil {
		klog.Errorf("Failed to get topology information: %v", err)
	}

	// 系统的uuid
	systemUUID, err := sysinfo.GetSystemUUID(sysFs)
	if err != nil {
		klog.Errorf("Failed to get system UUID: %v", err)
	}

	// 获得node节点在云厂商里的instanceType和instanceID
	// 目前支持aws、azure、gce，其他不支持的cloudProvider为Unknown，instanceType为Unknown、instanceID为None
	realCloudInfo := cloudinfo.NewRealCloudInfo()
	cloudProvider := realCloudInfo.GetCloudProvider()
	instanceType := realCloudInfo.GetInstanceType()
	instanceID := realCloudInfo.GetInstanceID()

	machineInfo := &info.MachineInfo{
		NumCores:       numCores,
		CpuFrequency:   clockSpeed,
		MemoryCapacity: memoryCapacity,
		HugePages:      hugePagesInfo,
		DiskMap:        diskMap,
		NetworkDevices: netDevices,
		Topology:       topology,
		// 读取/etc/machine-id或/var/lib/dbus/machine-id
		MachineID:      getInfoFromFiles(filepath.Join(rootFs, *machineIdFilePath)),
		SystemUUID:     systemUUID,
		// 读取/proc/sys/kernel/random/boot_id
		BootID:         getInfoFromFiles(filepath.Join(rootFs, *bootIdFilePath)),
		CloudProvider:  cloudProvider,
		InstanceType:   instanceType,
		InstanceID:     instanceID,
	}

	// 将[]fs.Fs转成[]info.FsInfo
	// 只需要fs.Device、fs.Major、fs.Minor、fs.Type（一般都为"vfs"）,fs.Capacity（总大小），inode
	for i := range filesystems {
		fs := filesystems[i]
		inodes := uint64(0)
		if fs.Inodes != nil {
			inodes = *fs.Inodes
		}
		machineInfo.Filesystems = append(machineInfo.Filesystems, info.FsInfo{Device: fs.Device, DeviceMajor: uint64(fs.Major), DeviceMinor: uint64(fs.Minor), Type: fs.Type.String(), Capacity: fs.Capacity, Inodes: inodes, HasInodes: fs.Inodes != nil})
	}

	return machineInfo, nil
}

func ContainerOsVersion() string {
	os, err := operatingsystem.GetOperatingSystem()
	if err != nil {
		os = "Unknown"
	}
	return os
}

func KernelVersion() string {
	uname := &unix.Utsname{}

	if err := unix.Uname(uname); err != nil {
		return "Unknown"
	}

	return string(uname.Release[:bytes.IndexByte(uname.Release[:], 0)])
}
