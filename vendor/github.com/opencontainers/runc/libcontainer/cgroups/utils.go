// +build linux

package cgroups

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	units "github.com/docker/go-units"
	"golang.org/x/sys/unix"
)

const (
	CgroupNamePrefix  = "name="
	CgroupProcesses   = "cgroup.procs"
	unifiedMountpoint = "/sys/fs/cgroup"
)

var (
	isUnifiedOnce sync.Once
	isUnified     bool
)

// HugePageSizeUnitList is a list of the units used by the linux kernel when
// naming the HugePage control files.
// https://www.kernel.org/doc/Documentation/cgroup-v1/hugetlb.txt
// TODO Since the kernel only use KB, MB and GB; TB and PB should be removed,
// depends on https://github.com/docker/go-units/commit/a09cd47f892041a4fac473133d181f5aea6fa393
var HugePageSizeUnitList = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// IsCgroup2UnifiedMode returns whether we are running in cgroup v2 unified mode.
func IsCgroup2UnifiedMode() bool {
	isUnifiedOnce.Do(func() {
		var st syscall.Statfs_t
		if err := syscall.Statfs(unifiedMountpoint, &st); err != nil {
			panic("cannot statfs cgroup root")
		}
		isUnified = st.Type == unix.CGROUP2_SUPER_MAGIC
	})
	return isUnified
}

// https://www.kernel.org/doc/Documentation/cgroup-v1/cgroups.txt
// 如果是cgroup2子系统，返回"/sys/fs/cgroup"
// 否则cgroup1子系统，找到subsystem子系统在/proc/self/mountinfo信息里挂载目录（匹配cgroupPath）和root目录
// 比如cgroupPath为"/sys/fs/cgroup" subsystem为"cpu", 返回"/sys/fs/cgroup/cpu,cpuacct"
func FindCgroupMountpoint(cgroupPath, subsystem string) (string, error) {
	if IsCgroup2UnifiedMode() {
		return unifiedMountpoint, nil
	}
	mnt, _, err := FindCgroupMountpointAndRoot(cgroupPath, subsystem)
	return mnt, err
}

// 找到subsystem子系统在/proc/self/mountinfo信息里挂载目录和root目录--从/proc/self/mountinfo里每一行的最后一个字段，按照逗号进行分割，得到的切片中是否包含subsystem，如果包含，则返回第5个字段（挂载目录）和第四个字段（root目录--当前目录）
func FindCgroupMountpointAndRoot(cgroupPath, subsystem string) (string, string, error) {
	// We are not using mount.GetMounts() because it's super-inefficient,
	// parsing it directly sped up x10 times because of not using Sscanf.
	// It was one of two major performance drawbacks in container start.
	// 在"/proc/self/cgroup"里是否有subsystem
	// 如果cgroup2，判断subsystem是否在"/sys/fs/cgroup/cgroup.controllers"
	if !isSubsystemAvailable(subsystem) {
		return "", "", NewNotFoundError(subsystem)
	}

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	if IsCgroup2UnifiedMode() {
		subsystem = ""
	}

	// 这里的cgroupPath为"",意味着在findCgroupMountpointAndRootFromReader里strings.HasPrefix(fields[4], cgroupPath)都为true
	// 即从/proc/self/mountinfo里每一行的最后一个字段，按照逗号进行分割，判断得到的切片中是否包含subsystem，如果包含subsystem，返回挂载点和根路径
	return findCgroupMountpointAndRootFromReader(f, cgroupPath, subsystem)
}

// 从/proc/self/mountinfo里找到挂载路径前缀匹配cgroupPath的行，并对这一行的最后一个字段，按照逗号进行分割，判断得到的切片中是否包含subsystem，返回挂载点和根路径
func findCgroupMountpointAndRootFromReader(reader io.Reader, cgroupPath, subsystem string) (string, string, error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		txt := scanner.Text()
		fields := strings.Fields(txt)
		if len(fields) < 9 {
			continue
		}
		if strings.HasPrefix(fields[4], cgroupPath) {
			// 最后一个字段，以逗号进行分隔，遍历匹配subsystem，返回挂载点和根路径
			for _, opt := range strings.Split(fields[len(fields)-1], ",") {
				if (subsystem == "" && fields[9] == "cgroup2") || opt == subsystem {
					return fields[4], fields[3], nil
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}

	return "", "", NewNotFoundError(subsystem)
}

// 判断内核是否支持subsystem cgroup子系统
// cgroup2 从"/sys/fs/cgroup/cgroup.controllers"，获取支持的cgroup子系统列表
// 从/proc/self/cgroup获得各种cgroup系统的路径，解析出map[{cgroup subsystem}]{cgroup-path}
func isSubsystemAvailable(subsystem string) bool {
	if IsCgroup2UnifiedMode() {
		// cgroup2 从"/sys/fs/cgroup/cgroup.controllers"，获取支持的cgroup子系统列表
		controllers, err := GetAllSubsystems()
		if err != nil {
			return false
		}
		for _, c := range controllers {
			if c == subsystem {
				return true
			}
		}
		return false
	}

	// 从/proc/self/cgroup获得各种cgroup系统的路径，解析出map[{cgroup subsystem}]{cgroup-path}
	cgroups, err := ParseCgroupFile("/proc/self/cgroup")
	if err != nil {
		return false
	}
	_, avail := cgroups[subsystem]
	return avail
}

// 从mountinfo中获取最长的匹配dir的挂载路径
func GetClosestMountpointAncestor(dir, mountinfo string) string {
	deepestMountPoint := ""
	for _, mountInfoEntry := range strings.Split(mountinfo, "\n") {
		mountInfoParts := strings.Fields(mountInfoEntry)
		if len(mountInfoParts) < 5 {
			continue
		}
		mountPoint := mountInfoParts[4]
		if strings.HasPrefix(mountPoint, deepestMountPoint) && strings.HasPrefix(dir, mountPoint) {
			deepestMountPoint = mountPoint
		}
	}
	return deepestMountPoint
}

// 从"/proc/self/mountinfo"找到" 第一个cgroup子系统挂载路径
// 从"/proc/self/mountinfo"找到" - "后面部分（字段数量必须大于等于3）中，第一个字段为"cgroup"或"cgroup2"的行，返回这一行的第5个字段的父路径
// 比如 31 30 0:27 / /sys/fs/cgroup/systemd rw,nosuid,nodev,noexec,relatime shared:9 - cgroup cgroup rw,xattr,release_agent=/usr/lib/systemd/systemd-cgroups-agent,name=systemd
// 返回"/sys/fs/cgroup"
func FindCgroupMountpointDir() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		text := scanner.Text()
		fields := strings.Split(text, " ")
		// Safe as mountinfo encodes mountpoints with spaces as \040.
		index := strings.Index(text, " - ")
		// " - "后面部分
		postSeparatorFields := strings.Fields(text[index+3:])
		numPostFields := len(postSeparatorFields)

		// This is an error as we can't detect if the mount is for "cgroup"
		if numPostFields == 0 {
			return "", fmt.Errorf("Found no fields post '-' in %q", text)
		}

		if postSeparatorFields[0] == "cgroup" || postSeparatorFields[0] == "cgroup2" {
			// Check that the mount is properly formatted.
			if numPostFields < 3 {
				return "", fmt.Errorf("Error found less than 3 fields post '-' in %q", text)
			}

			return filepath.Dir(fields[4]), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", NewNotFoundError("cgroup")
}

type Mount struct {
	Mountpoint string
	Root       string
	Subsystems []string
}

func (m Mount) GetOwnCgroup(cgroups map[string]string) (string, error) {
	if len(m.Subsystems) == 0 {
		return "", fmt.Errorf("no subsystem for mount")
	}

	return getControllerPath(m.Subsystems[0], cgroups)
}

// 从mi中读取出cgroup系统挂载的信息，返回各个cgroup系统的Mountpoint、Root、对应的Subsystems列表
func getCgroupMountsHelper(ss map[string]bool, mi io.Reader, all bool) ([]Mount, error) {
	res := make([]Mount, 0, len(ss))
	scanner := bufio.NewScanner(mi)
	numFound := 0
	for scanner.Scan() && numFound < len(ss) {
		txt := scanner.Text()
		sepIdx := strings.Index(txt, " - ")
		if sepIdx == -1 {
			return nil, fmt.Errorf("invalid mountinfo format")
		}
		// 非cgroup文件系统挂载跳过
		if txt[sepIdx+3:sepIdx+10] == "cgroup2" || txt[sepIdx+3:sepIdx+9] != "cgroup" {
			continue
		}
		fields := strings.Split(txt, " ")
		m := Mount{
			Mountpoint: fields[4],
			Root:       fields[3],
		}
		for _, opt := range strings.Split(fields[len(fields)-1], ",") {
			seen, known := ss[opt]
			// 不是已知的cgroup子系统，或只获取部分cgroup子系统（all为false）且是已经出现过cgroup子系统，则跳过
			if !known || (!all && seen) {
				continue
			}
			ss[opt] = true
			// subsystem匹配前缀为"name="（比如name=systemd），则取后面部分string（一般是systemd）
			if strings.HasPrefix(opt, CgroupNamePrefix) {
				opt = opt[len(CgroupNamePrefix):]
			}
			m.Subsystems = append(m.Subsystems, opt)
			numFound++
		}
		if len(m.Subsystems) > 0 || all {
			res = append(res, m)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// GetCgroupMounts returns the mounts for the cgroup subsystems.
// all indicates whether to return just the first instance or all the mounts.
// 从/proc/self/mountinfo中获得所有cgroup子系统的Mountpoint、Root、对应的Subsystems列表
func GetCgroupMounts(all bool) ([]Mount, error) {
	if IsCgroup2UnifiedMode() {
		availableControllers, err := GetAllSubsystems()
		if err != nil {
			return nil, err
		}
		m := Mount{
			Mountpoint: unifiedMountpoint,
			Root:       unifiedMountpoint,
			Subsystems: availableControllers,
		}
		return []Mount{m}, nil
	}

	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 从/proc/self/cgroup获得各种cgroup系统的路径，解析出map[{cgroup subsystem}]{cgroup-path}
	allSubsystems, err := ParseCgroupFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}

	allMap := make(map[string]bool)
	for s := range allSubsystems {
		allMap[s] = false
	}
	// 获得各个cgroup系统的Mountpoint、Root、对应的Subsystems列表
	return getCgroupMountsHelper(allMap, f, all)
}

// GetAllSubsystems returns all the cgroup subsystems supported by the kernel
// cgroup2 从"/sys/fs/cgroup/cgroup.controllers"，获取支持的cgroup子系统列表
// cgroup1 从"/proc/cgroups"，获取支持的cgroup子系统列表
func GetAllSubsystems() ([]string, error) {
	// /proc/cgroups is meaningless for v2
	// https://github.com/torvalds/linux/blob/v5.3/Documentation/admin-guide/cgroup-v2.rst#deprecated-v1-core-features
	if IsCgroup2UnifiedMode() {
		// "pseudo" controllers do not appear in /sys/fs/cgroup/cgroup.controllers.
		// - devices: implemented in kernel 4.15
		// - freezer: implemented in kernel 5.2
		// We assume these are always available, as it is hard to detect availability.
		pseudo := []string{"devices", "freezer"}
		data, err := ioutil.ReadFile("/sys/fs/cgroup/cgroup.controllers")
		if err != nil {
			return nil, err
		}
		subsystems := append(pseudo, strings.Fields(string(data))...)
		return subsystems, nil
	}
	f, err := os.Open("/proc/cgroups")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	subsystems := []string{}

	s := bufio.NewScanner(f)
	for s.Scan() {
		text := s.Text()
		if text[0] != '#' {
			parts := strings.Fields(text)
			if len(parts) >= 4 && parts[3] != "0" {
				subsystems = append(subsystems, parts[0])
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return subsystems, nil
}

// GetOwnCgroup returns the relative path to the cgroup docker is running in.
// 从/proc/self/cgroup，获得subsystem的cgroup路径（相对cgroup根路径（比如/sys/fs/cgroup））
func GetOwnCgroup(subsystem string) (string, error) {
	// 从/proc/self/cgroup获得各种cgroup系统的路径，解析出map[{cgroup subsystem}]{cgroup-path}，这里的cgroup-path是相对cgroup根路径（比如/sys/fs/cgroup）
	cgroups, err := ParseCgroupFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}

	// 从提供cgroups中提取出subsystem对应的路径
	return getControllerPath(subsystem, cgroups)
}

// 进程的cgroup subsystem的绝对路径
func GetOwnCgroupPath(subsystem string) (string, error) {
	// 从/proc/self/cgroup获得subsystem的cgroup路径(相对于cgroup root目录)
	cgroup, err := GetOwnCgroup(subsystem)
	if err != nil {
		return "", err
	}

	// 返回cgroup subsystem子系统的绝对路径
	return getCgroupPathHelper(subsystem, cgroup)
}

// 从/proc/1/cgroup获得subsystem对应的相对cgroup根路径
func GetInitCgroup(subsystem string) (string, error) {
	// 从/proc/1/cgroup获得各种cgroup系统的路径，解析出map[{cgroup subsystem}]{cgroup-path}，这里的cgroup-path是相对cgroup根路径（比如/sys/fs/cgroup），比如"cpu"对应"/"
	cgroups, err := ParseCgroupFile("/proc/1/cgroup")
	if err != nil {
		return "", err
	}

	// 从提供cgroups中提取出subsystem对应的路径
	return getControllerPath(subsystem, cgroups)
}

func GetInitCgroupPath(subsystem string) (string, error) {
	cgroup, err := GetInitCgroup(subsystem)
	if err != nil {
		return "", err
	}

	return getCgroupPathHelper(subsystem, cgroup)
}

// 返回cgroup subsystem子系统的绝对路径（从/proc/self/mountinfo里获取subsystem的挂载路径和根路径，然后进行cgroup路径拼合）
func getCgroupPathHelper(subsystem, cgroup string) (string, error) {
	// 找到subsystem子系统在/proc/self/mountinfo信息里挂载目录和root目录
	mnt, root, err := FindCgroupMountpointAndRoot("", subsystem)
	if err != nil {
		return "", err
	}

	// This is needed for nested containers, because in /proc/self/cgroup we
	// see paths from host, which don't exist in container.
	relCgroup, err := filepath.Rel(root, cgroup)
	if err != nil {
		return "", err
	}

	return filepath.Join(mnt, relCgroup), nil
}

func readProcsFile(dir string) ([]int, error) {
	f, err := os.Open(filepath.Join(dir, CgroupProcesses))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var (
		s   = bufio.NewScanner(f)
		out = []int{}
	)

	for s.Scan() {
		if t := s.Text(); t != "" {
			pid, err := strconv.Atoi(t)
			if err != nil {
				return nil, err
			}
			out = append(out, pid)
		}
	}
	return out, nil
}

// ParseCgroupFile parses the given cgroup file, typically from
// /proc/<pid>/cgroup, into a map of subgroups to cgroup names.
// 从/proc/{pid}/cgroup中解析出map[{cgroup subsystem}]{cgroup-path}，这里的cgroup-path是相对cgroup根路径（比如/sys/fs/cgroup）
// 比如"cpu"对应"/system.slice/kubelet.service"
func ParseCgroupFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return parseCgroupFromReader(f)
}

// helper function for ParseCgroupFile to make testing easier
func parseCgroupFromReader(r io.Reader) (map[string]string, error) {
	s := bufio.NewScanner(r)
	cgroups := make(map[string]string)

	for s.Scan() {
		text := s.Text()
		// from cgroups(7):
		// /proc/[pid]/cgroup
		// ...
		// For each cgroup hierarchy ... there is one entry
		// containing three colon-separated fields of the form:
		//     hierarchy-ID:subsystem-list:cgroup-path
		parts := strings.SplitN(text, ":", 3)
		if len(parts) < 3 {
			return nil, fmt.Errorf("invalid cgroup entry: must contain at least two colons: %v", text)
		}

		for _, subs := range strings.Split(parts[1], ",") {
			cgroups[subs] = parts[2]
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}

	return cgroups, nil
}

// 从提供cgroups中提取出subsystem对应的路径
func getControllerPath(subsystem string, cgroups map[string]string) (string, error) {
	if IsCgroup2UnifiedMode() {
		return "/", nil
	}

	if p, ok := cgroups[subsystem]; ok {
		return p, nil
	}

	if p, ok := cgroups[CgroupNamePrefix+subsystem]; ok {
		return p, nil
	}

	return "", NewNotFoundError(subsystem)
}

func PathExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

// 遍历cgroupPaths将pid写入path下的"cgroup.procs"文件
func EnterPid(cgroupPaths map[string]string, pid int) error {
	for _, path := range cgroupPaths {
		if PathExists(path) {
			// 将pid写入到path下的"cgroup.procs"文件，最多重试5次
			// 在cpuset子系统中，如果dir目录下"cpuset.cpus"和"cpuset.mems"文件为空，则写入dir下的"cgroup.procs"文件会报错"write error: No space left on device"
			if err := WriteCgroupProc(path, pid); err != nil {
				return err
			}
		}
	}
	return nil
}

// RemovePaths iterates over the provided paths removing them.
// We trying to remove all paths five times with increasing delay between tries.
// If after all there are not removed cgroups - appropriate error will be
// returned.
func RemovePaths(paths map[string]string) (err error) {
	delay := 10 * time.Millisecond
	for i := 0; i < 5; i++ {
		if i != 0 {
			time.Sleep(delay)
			delay *= 2
		}
		for s, p := range paths {
			os.RemoveAll(p)
			// TODO: here probably should be logging
			_, err := os.Stat(p)
			// We need this strange way of checking cgroups existence because
			// RemoveAll almost always returns error, even on already removed
			// cgroups
			if os.IsNotExist(err) {
				delete(paths, s)
			}
		}
		if len(paths) == 0 {
			return nil
		}
	}
	return fmt.Errorf("Failed to remove paths: %v", paths)
}

func GetHugePageSize() ([]string, error) {
	files, err := ioutil.ReadDir("/sys/kernel/mm/hugepages")
	if err != nil {
		return []string{}, err
	}
	var fileNames []string
	for _, st := range files {
		fileNames = append(fileNames, st.Name())
	}
	return getHugePageSizeFromFilenames(fileNames)
}

func getHugePageSizeFromFilenames(fileNames []string) ([]string, error) {
	var pageSizes []string
	for _, fileName := range fileNames {
		nameArray := strings.Split(fileName, "-")
		pageSize, err := units.RAMInBytes(nameArray[1])
		if err != nil {
			return []string{}, err
		}
		sizeString := units.CustomSize("%g%s", float64(pageSize), 1024.0, HugePageSizeUnitList)
		pageSizes = append(pageSizes, sizeString)
	}

	return pageSizes, nil
}

// GetPids returns all pids, that were added to cgroup at path.
func GetPids(path string) ([]int, error) {
	return readProcsFile(path)
}

// GetAllPids returns all pids, that were added to cgroup at path and to all its
// subcgroups.
// 读取path目录和子目录下的cgroup.procs，返回所有pid
func GetAllPids(path string) ([]int, error) {
	var pids []int
	// collect pids from all sub-cgroups
	err := filepath.Walk(path, func(p string, info os.FileInfo, iErr error) error {
		dir, file := filepath.Split(p)
		if file != CgroupProcesses {
			return nil
		}
		if iErr != nil {
			return iErr
		}
		cPids, err := readProcsFile(dir)
		if err != nil {
			return err
		}
		pids = append(pids, cPids...)
		return nil
	})
	return pids, err
}

// WriteCgroupProc writes the specified pid into the cgroup's cgroup.procs file
// 如果pid为-1，直接返回
// 将pid写入到dir下的"cgroup.procs"文件，最多重试5次
// 在cpuset子系统中，如果dir目录下"cpuset.cpus"和"cpuset.mems"文件为空，则写入dir下的"cgroup.procs"文件会报错"write error: No space left on device"
func WriteCgroupProc(dir string, pid int) error {
	// Normally dir should not be empty, one case is that cgroup subsystem
	// is not mounted, we will get empty dir, and we want it fail here.
	if dir == "" {
		return fmt.Errorf("no such directory for %s", CgroupProcesses)
	}

	// Dont attach any pid to the cgroup if -1 is specified as a pid
	if pid == -1 {
		return nil
	}

	cgroupProcessesFile, err := os.OpenFile(filepath.Join(dir, CgroupProcesses), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0700)
	if err != nil {
		return fmt.Errorf("failed to write %v to %v: %v", pid, CgroupProcesses, err)
	}
	defer cgroupProcessesFile.Close()

	for i := 0; i < 5; i++ {
		_, err = cgroupProcessesFile.WriteString(strconv.Itoa(pid))
		if err == nil {
			return nil
		}

		// EINVAL might mean that the task being added to cgroup.procs is in state
		// TASK_NEW. We should attempt to do so again.
		if isEINVAL(err) {
			time.Sleep(30 * time.Millisecond)
			continue
		}

		return fmt.Errorf("failed to write %v to %v: %v", pid, CgroupProcesses, err)
	}
	return err
}

func isEINVAL(err error) bool {
	switch err := err.(type) {
	case *os.PathError:
		return err.Err == unix.EINVAL
	default:
		return false
	}
}
