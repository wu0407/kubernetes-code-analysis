// +build linux,!nokmem

package fs

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"syscall" // for Errno type only

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"golang.org/x/sys/unix"
)

const cgroupKernelMemoryLimit = "memory.kmem.limit_in_bytes"

// 将"1"写入到path下的memory.kmem.limit_in_bytes文件
// 如果发生了"EBUSY"错误，说明cgroup路径下面还有子cgroup路径或cgroup.procs里已经有进程绑定了，直接返回
// 否则将"-1"写入到path下的memory.kmem.limit_in_bytes文件，设置为无限制
func EnableKernelMemoryAccounting(path string) error {
	// Ensure that kernel memory is available in this kernel build. If it
	// isn't, we just ignore it because EnableKernelMemoryAccounting is
	// automatically called for all memory limits.
	// 检查path路径下面的memory.kmem.limit_in_bytes文件是否存在
	// 不存在说明系统内核不支持，直接返回
	if !cgroups.PathExists(filepath.Join(path, cgroupKernelMemoryLimit)) {
		return nil
	}
	// We have to limit the kernel memory here as it won't be accounted at all
	// until a limit is set on the cgroup and limit cannot be set once the
	// cgroup has children, or if there are already tasks in the cgroup.
	for _, i := range []int64{1, -1} {
		// 将kernelMemoryLimit的值写入到path下的memory.kmem.limit_in_bytes文件
		// 如果发生了"EBUSY"错误，说明cgroup路径下面还有子cgroup路径或cgroup.procs里已经有进程绑定了
		if err := setKernelMemory(path, i); err != nil {
			return err
		}
	}
	return nil
}

// 将kernelMemoryLimit的值写入到path下的memory.kmem.limit_in_bytes文件
// 如果发生了"EBUSY"错误，说明cgroup路径下面还有子cgroup路径或cgroup.procs里已经有进程绑定了
func setKernelMemory(path string, kernelMemoryLimit int64) error {
	if path == "" {
		return fmt.Errorf("no such directory for %s", cgroupKernelMemoryLimit)
	}
	if !cgroups.PathExists(filepath.Join(path, cgroupKernelMemoryLimit)) {
		// We have specifically been asked to set a kmem limit. If the kernel
		// doesn't support it we *must* error out.
		return errors.New("kernel memory accounting not supported by this kernel")
	}
	if err := ioutil.WriteFile(filepath.Join(path, cgroupKernelMemoryLimit), []byte(strconv.FormatInt(kernelMemoryLimit, 10)), 0700); err != nil {
		// Check if the error number returned by the syscall is "EBUSY"
		// The EBUSY signal is returned on attempts to write to the
		// memory.kmem.limit_in_bytes file if the cgroup has children or
		// once tasks have been attached to the cgroup
		// 发生了"EBUSY"错误，说明cgroup路径下面还有子cgroup路径或cgroup.procs里已经有进程绑定了
		if pathErr, ok := err.(*os.PathError); ok {
			if errNo, ok := pathErr.Err.(syscall.Errno); ok {
				if errNo == unix.EBUSY {
					return fmt.Errorf("failed to set %s, because either tasks have already joined this cgroup or it has children", cgroupKernelMemoryLimit)
				}
			}
		}
		return fmt.Errorf("failed to write %v to %v: %v", kernelMemoryLimit, cgroupKernelMemoryLimit, err)
	}
	return nil
}
