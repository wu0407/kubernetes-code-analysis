// Copyright 2015 Google Inc. All Rights Reserved.
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

// The machine package contains functions that extract machine-level specs.
package machine

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	// s390/s390x changes
	"runtime"

	info "github.com/google/cadvisor/info/v1"
	"github.com/google/cadvisor/utils"
	"github.com/google/cadvisor/utils/sysfs"
	"github.com/google/cadvisor/utils/sysinfo"

	"k8s.io/klog"

	"golang.org/x/sys/unix"
)

var (
	cpuRegExp     = regexp.MustCompile(`^processor\s*:\s*([0-9]+)$`)
	coreRegExp    = regexp.MustCompile(`^core id\s*:\s*([0-9]+)$`)
	nodeRegExp    = regexp.MustCompile(`^physical id\s*:\s*([0-9]+)$`)
	nodeBusRegExp = regexp.MustCompile(`^node([0-9]+)$`)
	// Power systems have a different format so cater for both
	cpuClockSpeedMHz     = regexp.MustCompile(`(?:cpu MHz|clock)\s*:\s*([0-9]+\.[0-9]+)(?:MHz)?`)
	memoryCapacityRegexp = regexp.MustCompile(`MemTotal:\s*([0-9]+) kB`)
	swapCapacityRegexp   = regexp.MustCompile(`SwapTotal:\s*([0-9]+) kB`)
)

const maxFreqFile = "/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq"
const cpuBusPath = "/sys/bus/cpu/devices/"
const nodePath = "/sys/devices/system/node"

// GetClockSpeed returns the CPU clock speed, given a []byte formatted as the /proc/cpuinfo file.
func GetClockSpeed(procInfo []byte) (uint64, error) {
	// s390/s390x, aarch64 and arm32 changes
	if isSystemZ() || isAArch64() || isArm32() {
		return 0, nil
	}

	// First look through sys to find a max supported cpu frequency.
	if utils.FileExists(maxFreqFile) {
		val, err := ioutil.ReadFile(maxFreqFile)
		if err != nil {
			return 0, err
		}
		var maxFreq uint64
		n, err := fmt.Sscanf(string(val), "%d", &maxFreq)
		if err != nil || n != 1 {
			return 0, fmt.Errorf("could not parse frequency %q", val)
		}
		return maxFreq, nil
	}
	// Fall back to /proc/cpuinfo
	matches := cpuClockSpeedMHz.FindSubmatch(procInfo)
	if len(matches) != 2 {
		return 0, fmt.Errorf("could not detect clock speed from output: %q", string(procInfo))
	}

	speed, err := strconv.ParseFloat(string(matches[1]), 64)
	if err != nil {
		return 0, err
	}
	// Convert to kHz
	return uint64(speed * 1000), nil
}

// GetMachineMemoryCapacity returns the machine's total memory from /proc/meminfo.
// Returns the total memory capacity as an uint64 (number of bytes).
func GetMachineMemoryCapacity() (uint64, error) {
	out, err := ioutil.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}

	memoryCapacity, err := parseCapacity(out, memoryCapacityRegexp)
	if err != nil {
		return 0, err
	}
	return memoryCapacity, err
}

// GetMachineSwapCapacity returns the machine's total swap from /proc/meminfo.
// Returns the total swap capacity as an uint64 (number of bytes).
func GetMachineSwapCapacity() (uint64, error) {
	out, err := ioutil.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}

	swapCapacity, err := parseCapacity(out, swapCapacityRegexp)
	if err != nil {
		return 0, err
	}
	return swapCapacity, err
}

// parseCapacity matches a Regexp in a []byte, returning the resulting value in bytes.
// Assumes that the value matched by the Regexp is in KB.
func parseCapacity(b []byte, r *regexp.Regexp) (uint64, error) {
	matches := r.FindSubmatch(b)
	if len(matches) != 2 {
		return 0, fmt.Errorf("failed to match regexp in output: %q", string(b))
	}
	m, err := strconv.ParseUint(string(matches[1]), 10, 64)
	if err != nil {
		return 0, err
	}

	// Convert to bytes.
	return m * 1024, err
}

/* Look for sysfs cpu path containing core_id */
/* Such as: sys/bus/cpu/devices/cpu0/topology/core_id */
func getCoreIdFromCpuBus(cpuBusPath string, threadId int) (int, error) {
	path := filepath.Join(cpuBusPath, fmt.Sprintf("cpu%d/topology", threadId))
	file := filepath.Join(path, "core_id")

	num, err := ioutil.ReadFile(file)
	if err != nil {
		return threadId, err
	}

	coreId, err := strconv.ParseInt(string(bytes.TrimSpace(num)), 10, 32)
	if err != nil {
		return threadId, err
	}

	if coreId < 0 {
		// report threadId if found coreId < 0
		coreId = int64(threadId)
	}

	return int(coreId), nil
}

/* Look for sysfs cpu path containing node id */
/* Such as: /sys/bus/cpu/devices/cpu0/node%d */
func getNodeIdFromCpuBus(cpuBusPath string, threadId int) (int, error) {
	path := filepath.Join(cpuBusPath, fmt.Sprintf("cpu%d", threadId))

	files, err := ioutil.ReadDir(path)
	if err != nil {
		return 0, err
	}

	nodeId := 0
	for _, file := range files {
		filename := file.Name()

		isNode, error := regexp.MatchString("^node([0-9]+)$", filename)
		if error != nil {
			continue
		}
		if !isNode {
			continue
		}

		ok, val, _ := extractValue(filename, nodeBusRegExp)
		if err != nil {
			continue
		}
		if ok {
			if val < 0 {
				continue
			}
			nodeId = val
		}
	}

	return nodeId, nil
}

// GetHugePagesInfo returns information about pre-allocated huge pages
// hugepagesDirectory should be top directory of hugepages
// Such as: /sys/kernel/mm/hugepages/
func GetHugePagesInfo(hugepagesDirectory string) ([]info.HugePagesInfo, error) {
	var hugePagesInfo []info.HugePagesInfo
	files, err := ioutil.ReadDir(hugepagesDirectory)
	if err != nil {
		// treat as non-fatal since kernels and machine can be
		// configured to disable hugepage support
		return hugePagesInfo, nil
	}
	for _, st := range files {
		nameArray := strings.Split(st.Name(), "-")
		pageSizeArray := strings.Split(nameArray[1], "kB")
		pageSize, err := strconv.ParseUint(string(pageSizeArray[0]), 10, 64)
		if err != nil {
			return hugePagesInfo, err
		}

		numFile := hugepagesDirectory + st.Name() + "/nr_hugepages"
		val, err := ioutil.ReadFile(numFile)
		if err != nil {
			return hugePagesInfo, err
		}
		var numPages uint64
		// we use sscanf as the file as a new-line that trips up ParseUint
		// it returns the number of tokens successfully parsed, so if
		// n != 1, it means we were unable to parse a number from the file
		n, err := fmt.Sscanf(string(val), "%d", &numPages)
		if err != nil || n != 1 {
			return hugePagesInfo, fmt.Errorf("could not parse file %v contents %q", numFile, string(val))
		}

		hugePagesInfo = append(hugePagesInfo, info.HugePagesInfo{
			NumPages: numPages,
			PageSize: pageSize,
		})
	}
	return hugePagesInfo, nil
}

// 获得cpu的拓扑，物理cpu下包含多个core和每个core的缓存信息，core下面包含多个线程
func GetTopology(sysFs sysfs.SysFs, cpuinfo string) ([]info.Node, int, error) {
	nodes := []info.Node{}

	// s390/s390x changes
	if true == isSystemZ() {
		return nodes, getNumCores(), nil
	}

	numCores := 0
	lastThread := -1
	lastCore := -1
	lastNode := -1
	for _, line := range strings.Split(cpuinfo, "\n") {
		if line == "" {
			continue
		}
		// 当前行否包含的cpu的线程id--匹配`^processor\s*:\s*([0-9]+)$`
		ok, val, err := extractValue(line, cpuRegExp)
		if err != nil {
			return nil, -1, fmt.Errorf("could not parse cpu info from %q: %v", line, err)
		}

		// 包含cpu的线程id，则进行添加之前发现的thread id到core中
		if ok {
			thread := val
			numCores++
			// 添加上次发现的线程id（lastThread）
			if lastThread != -1 {
				// New cpu section. Save last one.
				nodeIdx, err := addNode(&nodes, lastNode)
				if err != nil {
					return nil, -1, fmt.Errorf("failed to add node %d: %v", lastNode, err)
				}
				// 添加thread到这个core中，如果core没有发现，则添加新的core到nodes
				nodes[nodeIdx].AddThread(lastThread, lastCore)
				// 后续循环里会找到核心id和node id，设置lastCore和lastNode
				// 这里设置为-1，是为arm cpu，没有'core id' and 'physical id'
				lastCore = -1
				lastNode = -1
			}
			// 设置lastThread用于后续循环时候（上面的lastThread != -1为true），添加该thread id到core中
			lastThread = thread

			/* On Arm platform, no 'core id' and 'physical id' in '/proc/cpuinfo'. */
			/* So we search sysfs cpu path directly. */
			/* This method can also be used on other platforms, such as x86, ppc64le... */
			/* /sys/bus/cpu/devices/cpu%d contains the information of 'core_id' & 'node_id'. */
			/* Such as: /sys/bus/cpu/devices/cpu0/topology/core_id */
			/* Such as:  /sys/bus/cpu/devices/cpu0/node0 */
			if isAArch64() {
				val, err = getCoreIdFromCpuBus(cpuBusPath, lastThread)
				if err != nil {
					// Report thread id if no NUMA
					val = lastThread
				}
				lastCore = val

				val, err = getNodeIdFromCpuBus(cpuBusPath, lastThread)
				if err != nil {
					// Report node 0 if no NUMA
					val = 0
				}
				lastNode = val
			}
			continue
		}

		if isAArch64() {
			/* On Arm platform, no 'core id' and 'physical id' in '/proc/cpuinfo'. */
			continue
		}

		// 当前行是否包含核心id--匹配`^core id\s*:\s*([0-9]+)$`
		ok, val, err = extractValue(line, coreRegExp)
		if err != nil {
			return nil, -1, fmt.Errorf("could not parse core info from %q: %v", line, err)
		}
		// 包含含核心id
		if ok {
			lastCore = val
			continue
		}

		// 当前行是否包含物理cpu id--匹配`^physical id\s*:\s*([0-9]+)$`
		ok, val, err = extractValue(line, nodeRegExp)
		if err != nil {
			return nil, -1, fmt.Errorf("could not parse node info from %q: %v", line, err)
		}
		if ok {
			lastNode = val
			continue
		}
	}

	// 确保最后一次发现lastNode，添加到nodes
	nodeIdx, err := addNode(&nodes, lastNode)
	if err != nil {
		return nil, -1, fmt.Errorf("failed to add node %d: %v", lastNode, err)
	}
	// 添加最后的一次发现的lastCore、lastThread
	nodes[nodeIdx].AddThread(lastThread, lastCore)
	if numCores < 1 {
		return nil, numCores, fmt.Errorf("could not detect any cores")
	}
	// 填充node的cache
	for idx, node := range nodes {
		// 获得每个node（物理cpu）上的第一个core的第一个线程cpu的各级缓存的信息--缓存大小、级别、类型、共享的线程个数
		caches, err := sysinfo.GetCacheInfo(sysFs, node.Cores[0].Threads[0])
		if err != nil {
			klog.Errorf("failed to get cache information for node %d: %v", node.Id, err)
			continue
		}
		numThreadsPerCore := len(node.Cores[0].Threads)
		numThreadsPerNode := len(node.Cores) * numThreadsPerCore
		for _, cache := range caches {
			c := info.Cache{
				Size:  cache.Size,
				Level: cache.Level,
				Type:  cache.Type,
			}
			// 共享该级缓存的线程cpu数等于这个node（物理cpu）的拥有的线程数，且当前cache.Level大于2（缓存级别为3级缓存）
			if cache.Cpus == numThreadsPerNode && cache.Level > 2 {
				// Add a node-level cache.
				// 有3级缓存，则添加cache到node层（3级缓存整个物理cpu的线程共享）
				nodes[idx].AddNodeCache(c)
			} else if cache.Cpus == numThreadsPerCore {
				// Add to each core.
				// 一级和二级缓存，添加到core中
				nodes[idx].AddPerCoreCache(c)
			}
			// Ignore unknown caches.
		}
	}
	return nodes, numCores, nil
}

func extractValue(s string, r *regexp.Regexp) (bool, int, error) {
	matches := r.FindSubmatch([]byte(s))
	if len(matches) == 2 {
		val, err := strconv.ParseInt(string(matches[1]), 10, 32)
		if err != nil {
			return false, -1, err
		}
		return true, int(val), nil
	}
	return false, -1, nil
}

func findNode(nodes []info.Node, id int) (bool, int) {
	for i, n := range nodes {
		if n.Id == id {
			return true, i
		}
	}
	return false, -1
}

// 物理cpu节点id不在nodes里面，则增加新的物理cpu节点到nodes，包括id，Memory，HugePages
func addNode(nodes *[]info.Node, id int) (int, error) {
	var idx int
	if id == -1 {
		// Some VMs don't fill topology data. Export single package.
		id = 0
	}

	// id是否在已经在nodes中
	ok, idx := findNode(*nodes, id)
	if !ok {
		// New node
		node := info.Node{Id: id}
		// Add per-node memory information.
		meminfo := fmt.Sprintf("/sys/devices/system/node/node%d/meminfo", id)
		out, err := ioutil.ReadFile(meminfo)
		// Ignore if per-node info is not available.
		if err == nil {
			// 解析node上总的memory，单位是kb
			m, err := parseCapacity(out, memoryCapacityRegexp)
			if err != nil {
				return -1, err
			}
			node.Memory = uint64(m)
		}
		// Look for per-node hugepages info using node id
		// Such as: /sys/devices/system/node/node%d/hugepages
		hugepagesDirectory := fmt.Sprintf("%s/node%d/hugepages/", nodePath, id)
		// 解析出所有hugepage的PageSize和这个PageSize的numPages
		hugePagesInfo, err := GetHugePagesInfo(hugepagesDirectory)
		if err != nil {
			return -1, err
		}
		node.HugePages = hugePagesInfo

		*nodes = append(*nodes, node)
		idx = len(*nodes) - 1
	}
	return idx, nil
}

// s390/s390x changes
func getMachineArch() (string, error) {
	uname := unix.Utsname{}
	err := unix.Uname(&uname)
	if err != nil {
		return "", err
	}

	return string(uname.Machine[:]), nil
}

// arm32 chanes
func isArm32() bool {
	arch, err := getMachineArch()
	if err == nil {
		return strings.Contains(arch, "arm")
	}
	return false
}

// aarch64 changes
func isAArch64() bool {
	arch, err := getMachineArch()
	if err == nil {
		return strings.Contains(arch, "aarch64")
	}
	return false
}

// s390/s390x changes
func isSystemZ() bool {
	arch, err := getMachineArch()
	if err == nil {
		return strings.Contains(arch, "390")
	}
	return false
}

// s390/s390x changes
func getNumCores() int {
	maxProcs := runtime.GOMAXPROCS(0)
	numCPU := runtime.NumCPU()

	if maxProcs < numCPU {
		return maxProcs
	}

	return numCPU
}
