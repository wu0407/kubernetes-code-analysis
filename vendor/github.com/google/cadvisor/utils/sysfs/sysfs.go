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

package sysfs

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
)

const (
	blockDir     = "/sys/block"
	cacheDir     = "/sys/devices/system/cpu/cpu"
	netDir       = "/sys/class/net"
	dmiDir       = "/sys/class/dmi"
	ppcDevTree   = "/proc/device-tree"
	s390xDevTree = "/etc" // s390/s390x changes
)

type CacheInfo struct {
	// size in bytes
	Size uint64
	// cache type - instruction, data, unified
	Type string
	// distance from cpus in a multi-level hierarchy
	Level int
	// number of cpus that can access this cache.
	Cpus int
}

// Abstracts the lowest level calls to sysfs.
type SysFs interface {
	// Get directory information for available block devices.
	GetBlockDevices() ([]os.FileInfo, error)
	// Get Size of a given block device.
	GetBlockDeviceSize(string) (string, error)
	// Get scheduler type for the block device.
	GetBlockDeviceScheduler(string) (string, error)
	// Get device major:minor number string.
	GetBlockDeviceNumbers(string) (string, error)

	GetNetworkDevices() ([]os.FileInfo, error)
	GetNetworkAddress(string) (string, error)
	GetNetworkMtu(string) (string, error)
	GetNetworkSpeed(string) (string, error)
	GetNetworkStatValue(dev string, stat string) (uint64, error)

	// Get directory information for available caches accessible to given cpu.
	GetCaches(id int) ([]os.FileInfo, error)
	// Get information for a cache accessible from the given cpu.
	GetCacheInfo(cpu int, cache string) (CacheInfo, error)

	GetSystemUUID() (string, error)
}

type realSysFs struct{}

func NewRealSysFs() SysFs {
	return &realSysFs{}
}

func (self *realSysFs) GetBlockDevices() ([]os.FileInfo, error) {
	return ioutil.ReadDir(blockDir)
}

// 读取/sys/block/{name}/dev，返回 主设备号:次设备号
func (self *realSysFs) GetBlockDeviceNumbers(name string) (string, error) {
	dev, err := ioutil.ReadFile(path.Join(blockDir, name, "/dev"))
	if err != nil {
		return "", err
	}
	return string(dev), nil
}

// 读取/sys/block/{name}/queue/scheduler， 返回io调度算法
func (self *realSysFs) GetBlockDeviceScheduler(name string) (string, error) {
	sched, err := ioutil.ReadFile(path.Join(blockDir, name, "/queue/scheduler"))
	if err != nil {
		return "", err
	}
	return string(sched), nil
}

// 读取/sys/block/{name}/size，返回块设备大小，单位字节
func (self *realSysFs) GetBlockDeviceSize(name string) (string, error) {
	size, err := ioutil.ReadFile(path.Join(blockDir, name, "/size"))
	if err != nil {
		return "", err
	}
	return string(size), nil
}

// 读取/sys/class/net目录，返回软链目标为目录的文件列表
func (self *realSysFs) GetNetworkDevices() ([]os.FileInfo, error) {
	files, err := ioutil.ReadDir(netDir)
	if err != nil {
		return nil, err
	}

	// Filter out non-directory & non-symlink files
	var dirs []os.FileInfo
	// 文件列表 比如cbr0  dummy0  eth0  kube-ipvs0  lo  veth6e857310  veth7f2330c4  vethcf0898a5
	for _, f := range files {
		// 文件是软链
		if f.Mode()|os.ModeSymlink != 0 {
			f, err = os.Stat(path.Join(netDir, f.Name()))
			if err != nil {
				continue
			}
		}
		// 真实文件是个目录
		if f.IsDir() {
			dirs = append(dirs, f)
		}
	}
	return dirs, nil
}

// 读取/sys/class/net/{name}/address，返回mac地址
func (self *realSysFs) GetNetworkAddress(name string) (string, error) {
	address, err := ioutil.ReadFile(path.Join(netDir, name, "/address"))
	if err != nil {
		return "", err
	}
	return string(address), nil
}

// 读取/sys/class/net/{name}/mtu，返回网卡的mtu大小
func (self *realSysFs) GetNetworkMtu(name string) (string, error) {
	mtu, err := ioutil.ReadFile(path.Join(netDir, name, "/mtu"))
	if err != nil {
		return "", err
	}
	return string(mtu), nil
}

// 读取/sys/class/net/{name}/speed，返回网卡的速度
func (self *realSysFs) GetNetworkSpeed(name string) (string, error) {
	speed, err := ioutil.ReadFile(path.Join(netDir, name, "/speed"))
	if err != nil {
		return "", err
	}
	return string(speed), nil
}

func (self *realSysFs) GetNetworkStatValue(dev string, stat string) (uint64, error) {
	statPath := path.Join(netDir, dev, "/statistics", stat)
	out, err := ioutil.ReadFile(statPath)
	if err != nil {
		return 0, fmt.Errorf("failed to read stat from %q for device %q", statPath, dev)
	}
	var s uint64
	n, err := fmt.Sscanf(string(out), "%d", &s)
	if err != nil || n != 1 {
		return 0, fmt.Errorf("could not parse value from %q for file %s", string(out), statPath)
	}
	return s, nil
}

// 读取/sys/devices/system/cpu/cpu{id}/cache目录
func (self *realSysFs) GetCaches(id int) ([]os.FileInfo, error) {
	cpuPath := fmt.Sprintf("%s%d/cache", cacheDir, id)
	return ioutil.ReadDir(cpuPath)
}

func bitCount(i uint64) (count int) {
	for i != 0 {
		if i&1 == 1 {
			count++
		}
		i >>= 1
	}
	return
}

func getCpuCount(cache string) (count int, err error) {
	out, err := ioutil.ReadFile(path.Join(cache, "/shared_cpu_map"))
	if err != nil {
		return 0, err
	}
	masks := strings.Split(string(out), ",")
	for _, mask := range masks {
		// convert hex string to uint64
		m, err := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse cpu map %q: %v", string(out), err)
		}
		count += bitCount(m)
	}
	return
}

// 读取/sys/devices/system/cpu/cpu{id}/cache/{name}/size 获得cpu缓存大小，比如 /sys/devices/system/cpu/cpu0/cache/index1/size
// 读取/sys/devices/system/cpu/cpu{id}/cache/{name}/level 获得cpu缓存级别
// 读取/sys/devices/system/cpu/cpu{id}/cache/{name}/type 获得cpu缓存类型
// 读取/sys/devices/system/cpu/cpu{id}/cache/{name}/shared_cpu_map 统计共享缓存的cpu数量 
func (self *realSysFs) GetCacheInfo(id int, name string) (CacheInfo, error) {
	cachePath := fmt.Sprintf("%s%d/cache/%s", cacheDir, id, name)
	out, err := ioutil.ReadFile(path.Join(cachePath, "/size"))
	if err != nil {
		return CacheInfo{}, err
	}
	var size uint64
	n, err := fmt.Sscanf(string(out), "%dK", &size)
	if err != nil || n != 1 {
		return CacheInfo{}, err
	}
	// convert to bytes
	size = size * 1024
	out, err = ioutil.ReadFile(path.Join(cachePath, "/level"))
	if err != nil {
		return CacheInfo{}, err
	}
	var level int
	n, err = fmt.Sscanf(string(out), "%d", &level)
	if err != nil || n != 1 {
		return CacheInfo{}, err
	}

	out, err = ioutil.ReadFile(path.Join(cachePath, "/type"))
	if err != nil {
		return CacheInfo{}, err
	}
	cacheType := strings.TrimSpace(string(out))
	// 读取/sys/devices/system/cpu/cpu{id}/cache/{name}/shared_cpu_map，获取共享缓存的线程cpu数量
	cpuCount, err := getCpuCount(cachePath)
	if err != nil {
		return CacheInfo{}, err
	}
	return CacheInfo{
		Size:  size,
		Level: level,
		Type:  cacheType,
		Cpus:  cpuCount,
	}, nil
}

func (self *realSysFs) GetSystemUUID() (string, error) {
	// 读取文件/sys/class/dmi/id/product_uuid
	if id, err := ioutil.ReadFile(path.Join(dmiDir, "id", "product_uuid")); err == nil {
		return strings.TrimSpace(string(id)), nil
	// 或读取/proc/device-tree/system-id
	} else if id, err = ioutil.ReadFile(path.Join(ppcDevTree, "system-id")); err == nil {
		return strings.TrimSpace(string(id)), nil
	// 或读取/proc/device-tree/vm,uuid
	} else if id, err = ioutil.ReadFile(path.Join(ppcDevTree, "vm,uuid")); err == nil {
		return strings.TrimSpace(string(id)), nil
	// 或读取/etc/machine-id
	} else if id, err = ioutil.ReadFile(path.Join(s390xDevTree, "machine-id")); err == nil {
		return strings.TrimSpace(string(id)), nil
	} else {
		return "", err
	}
}
