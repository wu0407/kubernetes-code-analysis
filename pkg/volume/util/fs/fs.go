// +build linux darwin

/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fs

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/kubernetes/pkg/volume/util/fsquota"
)

// FSInfo linux returns (available bytes, byte capacity, byte usage, total inodes, inodes free, inode usage, error)
// for the filesystem that path resides upon.
// 类似执行df命令，返回(available bytes, byte capacity, byte usage, total inodes, inodes free, inode usage, error)
func FsInfo(path string) (int64, int64, int64, int64, int64, int64, error) {
	statfs := &unix.Statfs_t{}
	err := unix.Statfs(path, statfs)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}

	// Available is blocks available * fragment size
	available := int64(statfs.Bavail) * int64(statfs.Bsize)

	// Capacity is total block count * fragment size
	capacity := int64(statfs.Blocks) * int64(statfs.Bsize)

	// Usage is block being used * fragment size (aka block size).
	usage := (int64(statfs.Blocks) - int64(statfs.Bfree)) * int64(statfs.Bsize)

	inodes := int64(statfs.Files)
	inodesFree := int64(statfs.Ffree)
	inodesUsed := inodes - inodesFree

	return available, capacity, usage, inodes, inodesFree, inodesUsed, nil
}

// DiskUsage gets disk usage of specified path.
// 执行xfs_quota或du命令获取路径磁盘使用量
func DiskUsage(path string) (*resource.Quantity, error) {
	// First check whether the quota system knows about this directory
	// A nil quantity with no error means that the path does not support quotas
	// and we should use other mechanisms.
	// 执行"/usr/sbin/xfs_quota -t /tmp/mounts{xxx} -P/dev/null -D/dev/null -x -f {mountpoint} quota -p -N -n -v -b {id}"
	data, err := fsquota.GetConsumption(path)
	// 如果输出不为空，说明是xfs系统
	if data != nil {
		return data, nil
	} else if err != nil {
		return nil, fmt.Errorf("unable to retrieve disk consumption via quota for %s: %v", path, err)
	}
	// 输出为空，代表不是xfs系统，则执行du命令来获取磁盘使用大小
	// Uses the same niceness level as cadvisor.fs does when running du
	// Uses -B 1 to always scale to a blocksize of 1 byte
	out, err := exec.Command("nice", "-n", "19", "du", "-x", "-s", "-B", "1", path).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed command 'du' ($ nice -n 19 du -x -s -B 1) on path %s with error %v", path, err)
	}
	used, err := resource.ParseQuantity(strings.Fields(string(out))[0])
	if err != nil {
		return nil, fmt.Errorf("failed to parse 'du' output %s due to error %v", out, err)
	}
	used.Format = resource.BinarySI
	return &used, nil
}

// Find uses the equivalent of the command `find <path> -dev -printf '.' | wc -c` to count files and directories.
// While this is not an exact measure of inodes used, it is a very good approximation.
// 执行xfs_quota或find，获得目录的inode数量
func Find(path string) (int64, error) {
	if path == "" {
		return 0, fmt.Errorf("invalid directory")
	}
	// First check whether the quota system knows about this directory
	// A nil quantity with no error means that the path does not support quotas
	// and we should use other mechanisms.
	// 执行"/usr/sbin/xfs_quota -t /tmp/mounts{xxx} -P/dev/null -D/dev/null -x -f {mountpoint} quota -p -N -n -v -i {id}"
	// 返回命令输出的数字
	inodes, err := fsquota.GetInodes(path)
	// 返回不为nil，说明文件系统是xfs
	if inodes != nil {
		return inodes.Value(), nil
	} else if err != nil {
		return 0, fmt.Errorf("unable to retrieve inode consumption via quota for %s: %v", path, err)
	}
	var counter byteCounter
	var stderr bytes.Buffer
	// 文件系统不是xfs，则执行"find {path} -xdev -printf ."
	findCmd := exec.Command("find", path, "-xdev", "-printf", ".")
	findCmd.Stdout, findCmd.Stderr = &counter, &stderr
	if err := findCmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to exec cmd %v - %v; stderr: %v", findCmd.Args, err, stderr.String())
	}
	if err := findCmd.Wait(); err != nil {
		return 0, fmt.Errorf("cmd %v failed. stderr: %s; err: %v", findCmd.Args, stderr.String(), err)
	}
	return counter.bytesWritten, nil
}

// Simple io.Writer implementation that counts how many bytes were written.
type byteCounter struct{ bytesWritten int64 }

func (b *byteCounter) Write(p []byte) (int, error) {
	b.bytesWritten += int64(len(p))
	return len(p), nil
}
