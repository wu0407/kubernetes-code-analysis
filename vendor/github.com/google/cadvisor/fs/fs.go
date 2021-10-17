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

// +build linux

// Provides Filesystem Stats
package fs

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/docker/docker/pkg/mount"
	"github.com/google/cadvisor/devicemapper"
	"github.com/google/cadvisor/utils"
	dockerutil "github.com/google/cadvisor/utils/docker"
	zfs "github.com/mistifyio/go-zfs"
	"k8s.io/klog"
)

const (
	LabelSystemRoot   = "root"
	LabelDockerImages = "docker-images"
	LabelCrioImages   = "crio-images"
)

const (
	// The block size in bytes.
	statBlockSize uint64 = 512
	// The maximum number of `disk usage` tasks that can be running at once.
	maxConcurrentOps = 20
)

// A pool for restricting the number of consecutive `du` and `find` tasks running.
var pool = make(chan struct{}, maxConcurrentOps)

func init() {
	for i := 0; i < maxConcurrentOps; i++ {
		releaseToken()
	}
}

func claimToken() {
	<-pool
}

func releaseToken() {
	pool <- struct{}{}
}

type partition struct {
	mountpoint string
	major      uint
	minor      uint
	fsType     string
	blockSize  uint
}

type RealFsInfo struct {
	// Map from block device path to partition information.
	partitions map[string]partition
	// Map from label to block device path.
	// Labels are intent-specific tags that are auto-detected.
	labels map[string]string
	// Map from mountpoint to mount information.
	mounts map[string]*mount.Info
	// devicemapper client
	dmsetup devicemapper.DmsetupClient
	// fsUUIDToDeviceName is a map from the filesystem UUID to its device name.
	fsUUIDToDeviceName map[string]string
}

func NewFsInfo(context Context) (FsInfo, error) {
	// 解析/proc/self/mountinfo信息
	mounts, err := mount.GetMounts(nil)
	if err != nil {
		return nil, err
	}

	// 返回map[uuid]"磁盘设备"
	fsUUIDToDeviceName, err := getFsUUIDToDeviceNameMap()
	if err != nil {
		// UUID is not always available across different OS distributions.
		// Do not fail if there is an error.
		klog.Warningf("Failed to get disk UUID mapping, getting disk info by uuid will not work: %v", err)
	}

	// Avoid devicemapper container mounts - these are tracked by the ThinPoolWatcher
	excluded := []string{fmt.Sprintf("%s/devicemapper/mnt", context.Docker.Root)}
	fsInfo := &RealFsInfo{
		// 从mount信息中过滤掉不支持的文件系统，排除非tmpfs的bind mounts
		// 如果是tmpfs类型，返回的key值为mount.Mountpoint
		// 如果是overlay类型，返回key值为fmt.Sprintf("%s_%d-%d", mount.Source, mount.Major, mount.Minor)
		partitions:         processMounts(mounts, excluded),
		labels:             make(map[string]string, 0),
		// 挂载点和对应的挂载信息
		mounts:             make(map[string]*mount.Info, 0),
		dmsetup:            devicemapper.NewDmsetupClient(),
		fsUUIDToDeviceName: fsUUIDToDeviceName,
	}

	for _, mount := range mounts {
		// 挂载点和对应的挂载信息
		fsInfo.mounts[mount.Mountpoint] = mount
	}

	// need to call this before the log line below printing out the partitions, as this function may
	// add a "partition" for devicemapper to fsInfo.partitions
	// 添加labels["docker-images"]={镜像保存路径的挂载源（设备）}和partitions里添加挂载源信息（一般不会新增，mountinfo里都已经有了这个挂载源）
	fsInfo.addDockerImagesLabel(context, mounts)
	// 添加labels["crio-images"]={镜像保存路径的挂载源（设备）}和partitions里添加挂载源信息（一般不会新增，mountinfo里都已经有了这个挂载源）
	fsInfo.addCrioImagesLabel(context, mounts)

	klog.V(1).Infof("Filesystem UUIDs: %+v", fsInfo.fsUUIDToDeviceName)
	klog.V(1).Infof("Filesystem partitions: %+v", fsInfo.partitions)
	// 添加labels["root"]={根路径的挂载源（设备）}和partitions里添加挂载源信息（一般不会新增，mountinfo里都已经有了这个挂载源）
	fsInfo.addSystemRootLabel(mounts)
	return fsInfo, nil
}

// getFsUUIDToDeviceNameMap creates the filesystem uuid to device name map
// using the information in /dev/disk/by-uuid. If the directory does not exist,
// this function will return an empty map.
func getFsUUIDToDeviceNameMap() (map[string]string, error) {
	const dir = "/dev/disk/by-uuid"

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return make(map[string]string), nil
	}

	// 文件列表类似 "2020-03-03-17-30-39-00"  "21dbe030-aa71-4b3a-8610-3b942dd447fa"  "227c075a-362a-40db-861d-263399d6e130"
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	fsUUIDToDeviceName := make(map[string]string)
	for _, file := range files {
		path := filepath.Join(dir, file.Name())
		// 返回类似 "../../vda1"
		target, err := os.Readlink(path)
		if err != nil {
			klog.Warningf("Failed to resolve symlink for %q", path)
			continue
		}
		// 返回/dev/vda1
		device, err := filepath.Abs(filepath.Join(dir, target))
		if err != nil {
			return nil, fmt.Errorf("failed to resolve the absolute path of %q", filepath.Join(dir, target))
		}
		fsUUIDToDeviceName[file.Name()] = device
	}
	return fsUUIDToDeviceName, nil
}

// 从mount信息中过滤掉不支持的文件系统，排除非tmpfs的bind mounts
// 如果是tmpfs类型，返回的key值为mount.Mountpoint
// 如果是overlay类型，返回key值为fmt.Sprintf("%s_%d-%d", mount.Source, mount.Major, mount.Minor)
func processMounts(mounts []*mount.Info, excludedMountpointPrefixes []string) map[string]partition {
	partitions := make(map[string]partition, 0)

	supportedFsType := map[string]bool{
		// all ext systems are checked through prefix.
		"btrfs":   true,
		"overlay": true,
		"tmpfs":   true,
		"xfs":     true,
		"zfs":     true,
	}

	for _, mount := range mounts {
		if !strings.HasPrefix(mount.Fstype, "ext") && !supportedFsType[mount.Fstype] {
			continue
		}
		// Avoid bind mounts, exclude tmpfs.
		if _, ok := partitions[mount.Source]; ok {
			if mount.Fstype != "tmpfs" {
				continue
			}
		}

		hasPrefix := false
		for _, prefix := range excludedMountpointPrefixes {
			if strings.HasPrefix(mount.Mountpoint, prefix) {
				hasPrefix = true
				break
			}
		}
		if hasPrefix {
			continue
		}

		// using mountpoint to replace device once fstype it tmpfs
		if mount.Fstype == "tmpfs" {
			mount.Source = mount.Mountpoint
		}
		// btrfs fix: following workaround fixes wrong btrfs Major and Minor Ids reported in /proc/self/mountinfo.
		// instead of using values from /proc/self/mountinfo we use stat to get Ids from btrfs mount point
		if mount.Fstype == "btrfs" && mount.Major == 0 && strings.HasPrefix(mount.Source, "/dev/") {
			major, minor, err := getBtrfsMajorMinorIds(mount)
			if err != nil {
				klog.Warningf("%s", err)
			} else {
				mount.Major = major
				mount.Minor = minor
			}
		}

		// overlay fix: Making mount source unique for all overlay mounts, using the mount's major and minor ids.
		if mount.Fstype == "overlay" {
			mount.Source = fmt.Sprintf("%s_%d-%d", mount.Source, mount.Major, mount.Minor)
		}

		partitions[mount.Source] = partition{
			fsType:     mount.Fstype,
			mountpoint: mount.Mountpoint,
			major:      uint(mount.Major),
			minor:      uint(mount.Minor),
		}
	}

	return partitions
}

// getDockerDeviceMapperInfo returns information about the devicemapper device and "partition" if
// docker is using devicemapper for its storage driver. If a loopback device is being used, don't
// return any information or error, as we want to report based on the actual partition where the
// loopback file resides, inside of the loopback file itself.
func (self *RealFsInfo) getDockerDeviceMapperInfo(context DockerContext) (string, *partition, error) {
	if context.Driver != DeviceMapper.String() {
		return "", nil, nil
	}

	dataLoopFile := context.DriverStatus[dockerutil.DriverStatusDataLoopFile]
	if len(dataLoopFile) > 0 {
		return "", nil, nil
	}

	dev, major, minor, blockSize, err := dockerDMDevice(context.DriverStatus, self.dmsetup)
	if err != nil {
		return "", nil, err
	}

	return dev, &partition{
		fsType:    DeviceMapper.String(),
		major:     major,
		minor:     minor,
		blockSize: blockSize,
	}, nil
}

// addSystemRootLabel attempts to determine which device contains the mount for /.
func (self *RealFsInfo) addSystemRootLabel(mounts []*mount.Info) {
	for _, m := range mounts {
		if m.Mountpoint == "/" {
			self.partitions[m.Source] = partition{
				fsType:     m.Fstype,
				mountpoint: m.Mountpoint,
				major:      uint(m.Major),
				minor:      uint(m.Minor),
			}
			self.labels[LabelSystemRoot] = m.Source
			return
		}
	}
}

// addDockerImagesLabel attempts to determine which device contains the mount for docker images.
func (self *RealFsInfo) addDockerImagesLabel(context Context, mounts []*mount.Info) {
	dockerDev, dockerPartition, err := self.getDockerDeviceMapperInfo(context.Docker)
	if err != nil {
		klog.Warningf("Could not get Docker devicemapper device: %v", err)
	}
	if len(dockerDev) > 0 && dockerPartition != nil {
		self.partitions[dockerDev] = *dockerPartition
		self.labels[LabelDockerImages] = dockerDev
	} else {
		// 非devicemapper的docker strorage driver，找到docker image存储路径的挂载点
		// 添加labels["docker-images"]={镜像保存路径的挂载源（设备）}和partitions里添加挂载源信息
		self.updateContainerImagesPath(LabelDockerImages, mounts, getDockerImagePaths(context))
	}
}

func (self *RealFsInfo) addCrioImagesLabel(context Context, mounts []*mount.Info) {
	if context.Crio.Root != "" {
		crioPath := context.Crio.Root
		crioImagePaths := map[string]struct{}{
			"/": {},
		}
		for _, dir := range []string{"overlay", "overlay2"} {
			crioImagePaths[path.Join(crioPath, dir+"-images")] = struct{}{}
		}
		for crioPath != "/" && crioPath != "." {
			crioImagePaths[crioPath] = struct{}{}
			crioPath = filepath.Dir(crioPath)
		}
		self.updateContainerImagesPath(LabelCrioImages, mounts, crioImagePaths)
	}
}

// Generate a list of possible mount points for docker image management from the docker root directory.
// Right now, we look for each type of supported graph driver directories, but we can do better by parsing
// some of the context from `docker info`.
// 返回所有可能的docker image存储路径
// 路径列表：
// "/"
// {dockerRoot}/devicemapper
// {dockerRoot}/btrfs
// {dockerRoot}/aufs
// {dockerRoot}/overlay
// {dockerRoot}/overlay2
// {dockerRoot}/zfs
// {dockerRoot}的各级目录
func getDockerImagePaths(context Context) map[string]struct{} {
	dockerImagePaths := map[string]struct{}{
		"/": {},
	}

	// TODO(rjnagal): Detect docker root and graphdriver directories from docker info.
	dockerRoot := context.Docker.Root
	for _, dir := range []string{"devicemapper", "btrfs", "aufs", "overlay", "overlay2", "zfs"} {
		dockerImagePaths[path.Join(dockerRoot, dir)] = struct{}{}
	}
	// 依次遍历dockerRoot的各级目录，比如/a/b/c,/a/b,/a
	for dockerRoot != "/" && dockerRoot != "." {
		dockerImagePaths[dockerRoot] = struct{}{}
		dockerRoot = filepath.Dir(dockerRoot)
	}
	return dockerImagePaths
}

// This method compares the mountpoints with possible container image mount points. If a match is found,
// the label is added to the partition.
// containerImagePaths与mounts挂载信息进行比对，找出容器镜像保存目录对应的挂载点，添加挂载源到labels和partitions里添加挂载源信息
func (self *RealFsInfo) updateContainerImagesPath(label string, mounts []*mount.Info, containerImagePaths map[string]struct{}) {
	var useMount *mount.Info
	for _, m := range mounts {
		if _, ok := containerImagePaths[m.Mountpoint]; ok {
			// 如果以前没有找到，现在找到。或现在找到的挂载路径比之前找到的挂载路径长度更长（比如/a是已经找到挂载点，又找到/a/b也是挂载点）
			// 则现在的挂载点为使用的挂载点
			if useMount == nil || (len(useMount.Mountpoint) < len(m.Mountpoint)) {
				useMount = m
			}
		}
	}
	// 已经找到挂载点
	if useMount != nil {
		// 添加partitions
		self.partitions[useMount.Source] = partition{
			fsType:     useMount.Fstype,
			mountpoint: useMount.Mountpoint,
			major:      uint(useMount.Major),
			minor:      uint(useMount.Minor),
		}
		// 添加labels
		self.labels[label] = useMount.Source
	}
}

func (self *RealFsInfo) GetDeviceForLabel(label string) (string, error) {
	dev, ok := self.labels[label]
	if !ok {
		return "", fmt.Errorf("non-existent label %q", label)
	}
	return dev, nil
}

func (self *RealFsInfo) GetLabelsForDevice(device string) ([]string, error) {
	labels := []string{}
	for label, dev := range self.labels {
		if dev == device {
			labels = append(labels, label)
		}
	}
	return labels, nil
}

func (self *RealFsInfo) GetMountpointForDevice(dev string) (string, error) {
	p, ok := self.partitions[dev]
	if !ok {
		return "", fmt.Errorf("no partition info for device %q", dev)
	}
	return p.mountpoint, nil
}

// 获取各个挂载源（设备）的总的大小、free大小、非特权用户avail可用大小、inodes容量、free的inode数量、文件系统类型为vfs，设备信息（设备名、主次设备号）、磁盘的读写数据量和耗时统计
func (self *RealFsInfo) GetFsInfoForPath(mountSet map[string]struct{}) ([]Fs, error) {
	filesystems := make([]Fs, 0)
	deviceSet := make(map[string]struct{})
	// 各个磁盘的读写数据量和耗时统计
	diskStatsMap, err := getDiskStatsMap("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	for device, partition := range self.partitions {
		_, hasMount := mountSet[partition.mountpoint]
		_, hasDevice := deviceSet[device]
		if mountSet == nil || (hasMount && !hasDevice) {
			var (
				err error
				fs  Fs
			)
			switch partition.fsType {
			case DeviceMapper.String():
				fs.Capacity, fs.Free, fs.Available, err = getDMStats(device, partition.blockSize)
				klog.V(5).Infof("got devicemapper fs capacity stats: capacity: %v free: %v available: %v:", fs.Capacity, fs.Free, fs.Available)
				fs.Type = DeviceMapper
			case ZFS.String():
				if _, devzfs := os.Stat("/dev/zfs"); os.IsExist(devzfs) {
					fs.Capacity, fs.Free, fs.Available, err = getZfstats(device)
					fs.Type = ZFS
					break
				}
				// if /dev/zfs is not present default to VFS
				fallthrough
			default:
				var inodes, inodesFree uint64
				if utils.FileExists(partition.mountpoint) {
					// 总的大小、free大小、非特权用户avail可用大小、inodes容量、free的inode数量
					fs.Capacity, fs.Free, fs.Available, inodes, inodesFree, err = getVfsStats(partition.mountpoint)
					fs.Inodes = &inodes
					fs.InodesFree = &inodesFree
					fs.Type = VFS
				} else {
					klog.V(4).Infof("unable to determine file system type, partition mountpoint does not exist: %v", partition.mountpoint)
				}
			}
			if err != nil {
				klog.V(4).Infof("Stat fs failed. Error: %v", err)
			} else {
				deviceSet[device] = struct{}{}
				fs.DeviceInfo = DeviceInfo{
					Device: device,
					Major:  uint(partition.major),
					Minor:  uint(partition.minor),
				}
				fs.DiskStats = diskStatsMap[device]
				filesystems = append(filesystems, fs)
			}
		}
	}
	return filesystems, nil
}

var partitionRegex = regexp.MustCompile(`^(?:(?:s|v|xv)d[a-z]+\d*|dm-\d+)$`)

func getDiskStatsMap(diskStatsFile string) (map[string]DiskStats, error) {
	diskStatsMap := make(map[string]DiskStats)
	file, err := os.Open(diskStatsFile)
	if err != nil {
		if os.IsNotExist(err) {
			klog.Warningf("Not collecting filesystem statistics because file %q was not found", diskStatsFile)
			return diskStatsMap, nil
		}
		return nil, err
	}

	defer file.Close()
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		words := strings.Fields(line)
		if !partitionRegex.MatchString(words[2]) {
			continue
		}
		// 8      50 sdd2 40 0 280 223 7 0 22 108 0 330 330
		deviceName := path.Join("/dev", words[2])
		wordLength := len(words)
		offset := 3
		var stats = make([]uint64, wordLength-offset)
		if len(stats) < 11 {
			return nil, fmt.Errorf("could not parse all 11 columns of /proc/diskstats")
		}
		var error error
		for i := offset; i < wordLength; i++ {
			stats[i-offset], error = strconv.ParseUint(words[i], 10, 64)
			if error != nil {
				return nil, error
			}
		}
		diskStats := DiskStats{
			ReadsCompleted:  stats[0],
			ReadsMerged:     stats[1],
			SectorsRead:     stats[2],
			ReadTime:        stats[3],
			WritesCompleted: stats[4],
			WritesMerged:    stats[5],
			SectorsWritten:  stats[6],
			WriteTime:       stats[7],
			IoInProgress:    stats[8],
			IoTime:          stats[9],
			WeightedIoTime:  stats[10],
		}
		diskStatsMap[deviceName] = diskStats
	}
	return diskStatsMap, nil
}

// 获取各个挂载源（设备）的总的大小、free大小、非特权用户avail可用大小、inodes容量、free的inode数量、文件系统类型为vfs，设备信息（设备名、主次设备号）、磁盘的读写数据量和耗时统计
func (self *RealFsInfo) GetGlobalFsInfo() ([]Fs, error) {
	return self.GetFsInfoForPath(nil)
}

func major(devNumber uint64) uint {
	return uint((devNumber >> 8) & 0xfff)
}

func minor(devNumber uint64) uint {
	return uint((devNumber & 0xff) | ((devNumber >> 12) & 0xfff00))
}

func (self *RealFsInfo) GetDeviceInfoByFsUUID(uuid string) (*DeviceInfo, error) {
	deviceName, found := self.fsUUIDToDeviceName[uuid]
	if !found {
		return nil, ErrNoSuchDevice
	}
	p, found := self.partitions[deviceName]
	if !found {
		return nil, fmt.Errorf("cannot find device %q in partitions", deviceName)
	}
	return &DeviceInfo{deviceName, p.major, p.minor}, nil
}

func (self *RealFsInfo) GetDirFsDevice(dir string) (*DeviceInfo, error) {
	buf := new(syscall.Stat_t)
	err := syscall.Stat(dir, buf)
	if err != nil {
		return nil, fmt.Errorf("stat failed on %s with error: %s", dir, err)
	}

	major := major(buf.Dev)
	minor := minor(buf.Dev)
	for device, partition := range self.partitions {
		if partition.major == major && partition.minor == minor {
			return &DeviceInfo{device, major, minor}, nil
		}
	}

	mount, found := self.mounts[dir]
	// try the parent dir if not found until we reach the root dir
	// this is an issue on btrfs systems where the directory is not
	// the subvolume
	for !found {
		pathdir, _ := filepath.Split(dir)
		// break when we reach root
		if pathdir == "/" {
			break
		}
		// trim "/" from the new parent path otherwise the next possible
		// filepath.Split in the loop will not split the string any further
		dir = strings.TrimSuffix(pathdir, "/")
		mount, found = self.mounts[dir]
	}

	if found && mount.Fstype == "btrfs" && mount.Major == 0 && strings.HasPrefix(mount.Source, "/dev/") {
		major, minor, err := getBtrfsMajorMinorIds(mount)
		if err != nil {
			klog.Warningf("%s", err)
		} else {
			return &DeviceInfo{mount.Source, uint(major), uint(minor)}, nil
		}
	}
	return nil, fmt.Errorf("could not find device with major: %d, minor: %d in cached partitions map", major, minor)
}

func GetDirUsage(dir string) (UsageInfo, error) {
	var usage UsageInfo

	if dir == "" {
		return usage, fmt.Errorf("invalid directory")
	}

	rootInfo, err := os.Stat(dir)
	if err != nil {
		return usage, fmt.Errorf("could not stat %q to get inode usage: %v", dir, err)
	}

	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return usage, fmt.Errorf("unsuported fileinfo for getting inode usage of %q", dir)
	}

	rootDevId := rootStat.Dev

	// dedupedInode stores inodes that could be duplicates (nlink > 1)
	dedupedInodes := make(map[uint64]struct{})

	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			// expected if files appear/vanish
			return nil
		}
		if err != nil {
			return fmt.Errorf("unable to count inodes for part of dir %s: %s", dir, err)
		}

		// according to the docs, Sys can be nil
		if info.Sys() == nil {
			return fmt.Errorf("fileinfo Sys is nil")
		}

		s, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("unsupported fileinfo; could not convert to stat_t")
		}

		if s.Dev != rootDevId {
			// don't descend into directories on other devices
			return filepath.SkipDir
		}
		if s.Nlink > 1 {
			if _, ok := dedupedInodes[s.Ino]; !ok {
				// Dedupe things that could be hardlinks
				dedupedInodes[s.Ino] = struct{}{}

				usage.Bytes += uint64(s.Blocks) * statBlockSize
				usage.Inodes++
			}
		} else {
			usage.Bytes += uint64(s.Blocks) * statBlockSize
			usage.Inodes++
		}
		return nil
	})

	return usage, nil
}

func (self *RealFsInfo) GetDirUsage(dir string) (UsageInfo, error) {
	claimToken()
	defer releaseToken()
	return GetDirUsage(dir)
}

// 获得path里的磁盘设备的总的大小、free大小、非特权用户avail可用大小、inodes容量、free的inode数量
func getVfsStats(path string) (total uint64, free uint64, avail uint64, inodes uint64, inodesFree uint64, err error) {
	var s syscall.Statfs_t
	if err = syscall.Statfs(path, &s); err != nil {
		return 0, 0, 0, 0, 0, err
	}
	total = uint64(s.Frsize) * s.Blocks
	free = uint64(s.Frsize) * s.Bfree
	avail = uint64(s.Frsize) * s.Bavail
	inodes = uint64(s.Files)
	inodesFree = uint64(s.Ffree)
	return total, free, avail, inodes, inodesFree, nil
}

// Devicemapper thin provisioning is detailed at
// https://www.kernel.org/doc/Documentation/device-mapper/thin-provisioning.txt
func dockerDMDevice(driverStatus map[string]string, dmsetup devicemapper.DmsetupClient) (string, uint, uint, uint, error) {
	poolName, ok := driverStatus[dockerutil.DriverStatusPoolName]
	if !ok || len(poolName) == 0 {
		return "", 0, 0, 0, fmt.Errorf("Could not get dm pool name")
	}

	out, err := dmsetup.Table(poolName)
	if err != nil {
		return "", 0, 0, 0, err
	}

	major, minor, dataBlkSize, err := parseDMTable(string(out))
	if err != nil {
		return "", 0, 0, 0, err
	}

	return poolName, major, minor, dataBlkSize, nil
}

// parseDMTable parses a single line of `dmsetup table` output and returns the
// major device, minor device, block size, and an error.
func parseDMTable(dmTable string) (uint, uint, uint, error) {
	dmTable = strings.Replace(dmTable, ":", " ", -1)
	dmFields := strings.Fields(dmTable)

	if len(dmFields) < 8 {
		return 0, 0, 0, fmt.Errorf("Invalid dmsetup status output: %s", dmTable)
	}

	major, err := strconv.ParseUint(dmFields[5], 10, 32)
	if err != nil {
		return 0, 0, 0, err
	}
	minor, err := strconv.ParseUint(dmFields[6], 10, 32)
	if err != nil {
		return 0, 0, 0, err
	}
	dataBlkSize, err := strconv.ParseUint(dmFields[7], 10, 32)
	if err != nil {
		return 0, 0, 0, err
	}

	return uint(major), uint(minor), uint(dataBlkSize), nil
}

func getDMStats(poolName string, dataBlkSize uint) (uint64, uint64, uint64, error) {
	out, err := exec.Command("dmsetup", "status", poolName).Output()
	if err != nil {
		return 0, 0, 0, err
	}

	used, total, err := parseDMStatus(string(out))
	if err != nil {
		return 0, 0, 0, err
	}

	used *= 512 * uint64(dataBlkSize)
	total *= 512 * uint64(dataBlkSize)
	free := total - used

	return total, free, free, nil
}

func parseDMStatus(dmStatus string) (uint64, uint64, error) {
	dmStatus = strings.Replace(dmStatus, "/", " ", -1)
	dmFields := strings.Fields(dmStatus)

	if len(dmFields) < 8 {
		return 0, 0, fmt.Errorf("Invalid dmsetup status output: %s", dmStatus)
	}

	used, err := strconv.ParseUint(dmFields[6], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	total, err := strconv.ParseUint(dmFields[7], 10, 64)
	if err != nil {
		return 0, 0, err
	}

	return used, total, nil
}

// getZfstats returns ZFS mount stats using zfsutils
func getZfstats(poolName string) (uint64, uint64, uint64, error) {
	dataset, err := zfs.GetDataset(poolName)
	if err != nil {
		return 0, 0, 0, err
	}

	total := dataset.Used + dataset.Avail + dataset.Usedbydataset

	return total, dataset.Avail, dataset.Avail, nil
}

// Simple io.Writer implementation that counts how many bytes were written.
type byteCounter struct{ bytesWritten uint64 }

func (b *byteCounter) Write(p []byte) (int, error) {
	b.bytesWritten += uint64(len(p))
	return len(p), nil
}

// Get major and minor Ids for a mount point using btrfs as filesystem.
func getBtrfsMajorMinorIds(mount *mount.Info) (int, int, error) {
	// btrfs fix: following workaround fixes wrong btrfs Major and Minor Ids reported in /proc/self/mountinfo.
	// instead of using values from /proc/self/mountinfo we use stat to get Ids from btrfs mount point

	buf := new(syscall.Stat_t)
	err := syscall.Stat(mount.Source, buf)
	if err != nil {
		err = fmt.Errorf("stat failed on %s with error: %s", mount.Source, err)
		return 0, 0, err
	}

	klog.V(4).Infof("btrfs mount %#v", mount)
	if buf.Mode&syscall.S_IFMT == syscall.S_IFBLK {
		err := syscall.Stat(mount.Mountpoint, buf)
		if err != nil {
			err = fmt.Errorf("stat failed on %s with error: %s", mount.Mountpoint, err)
			return 0, 0, err
		}

		klog.V(4).Infof("btrfs dev major:minor %d:%d\n", int(major(buf.Dev)), int(minor(buf.Dev)))
		klog.V(4).Infof("btrfs rdev major:minor %d:%d\n", int(major(buf.Rdev)), int(minor(buf.Rdev)))

		return int(major(buf.Dev)), int(minor(buf.Dev)), nil
	} else {
		return 0, 0, fmt.Errorf("%s is not a block device", mount.Source)
	}
}
