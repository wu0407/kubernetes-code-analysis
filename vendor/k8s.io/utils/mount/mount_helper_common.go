/*
Copyright 2018 The Kubernetes Authors.

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

package mount

import (
	"fmt"
	"os"

	"k8s.io/klog"
)

// CleanupMountPoint unmounts the given path and deletes the remaining directory
// if successful. If extensiveMountPointCheck is true IsNotMountPoint will be
// called instead of IsLikelyNotMountPoint. IsNotMountPoint is more expensive
// but properly handles bind mounts within the same fs.
// 执行umount {mountPath}，然后移除mountPath目录
func CleanupMountPoint(mountPath string, mounter Interface, extensiveMountPointCheck bool) error {
	pathExists, pathErr := PathExists(mountPath)
	// 挂载路径不存在，直接返回
	if !pathExists {
		klog.Warningf("Warning: Unmount skipped because path does not exist: %v", mountPath)
		return nil
	}
	// 是否是挂载点损坏错误
	corruptedMnt := IsCorruptedMnt(pathErr)
	// pathErr不为nil且不是挂载点损坏错误，返回错误
	if pathErr != nil && !corruptedMnt {
		return fmt.Errorf("Error checking path: %v", pathErr)
	}
	// 卸载mountPath挂载点，然后移除mountPath目录
	return doCleanupMountPoint(mountPath, mounter, extensiveMountPointCheck, corruptedMnt)
}

// doCleanupMountPoint unmounts the given path and
// deletes the remaining directory if successful.
// if extensiveMountPointCheck is true
// IsNotMountPoint will be called instead of IsLikelyNotMountPoint.
// IsNotMountPoint is more expensive but properly handles bind mounts within the same fs.
// if corruptedMnt is true, it means that the mountPath is a corrupted mountpoint, and the mount point check
// will be skipped
// 卸载mountPath挂载点，然后移除mountPath目录
func doCleanupMountPoint(mountPath string, mounter Interface, extensiveMountPointCheck bool, corruptedMnt bool) error {
	var notMnt bool
	var err error
	// 不是挂载点损坏
	if !corruptedMnt {
		// 需要全面的挂载点检查
		if extensiveMountPointCheck {
			// 如果mountPath的设备号与父目录的设备号不一样，说明mountPath是挂载点，直接返回false
			// 否则从"/proc/mounts"文件里查找，是否为挂载路径
			notMnt, err = IsNotMountPoint(mounter, mountPath)
		} else {
			// 不需要全面挂载点检查，则执行快速挂载点检查
			// 当mountPath的设备号与父目录的设备号不一样（但是不能判断同一目录下的bind挂载），则为mountPath为挂载点，返回false，否则返回true
			notMnt, err = mounter.IsLikelyNotMountPoint(mountPath)
		}

		if err != nil {
			return err
		}

		// mountPath不是挂载点，则直接删除mountPath目录
		if notMnt {
			klog.Warningf("Warning: %q is not a mountpoint, deleting", mountPath)
			return os.Remove(mountPath)
		}
	}

	// mountPath挂载点损坏

	// Unmount the mount path
	klog.V(4).Infof("%q is a mountpoint, unmounting", mountPath)
	// 执行umount {target}命令进行卸载
	if err := mounter.Unmount(mountPath); err != nil {
		return err
	}

	// 删除挂载点目录mountPath

	// 需要全面的挂载点检查
	if extensiveMountPointCheck {
		// 如果mountPath的设备号与父目录的设备号不一样，说明mountPath是挂载点，直接返回false
		// 否则从"/proc/mounts"文件里查找，是否为挂载路径
		notMnt, err = IsNotMountPoint(mounter, mountPath)
	} else {
		// 不需要全面挂载点检查，则执行快速挂载点检查
		// 当mountPath的设备号与父目录的设备号不一样（但是不能判断同一目录下的bind挂载），则为mountPath为挂载点，返回false，否则返回true
		notMnt, err = mounter.IsLikelyNotMountPoint(mountPath)
	}
	if err != nil {
		return err
	}
	// mountPath不是挂载点，则直接删除mountPath目录
	if notMnt {
		klog.V(4).Infof("%q is unmounted, deleting the directory", mountPath)
		return os.Remove(mountPath)
	}
	return fmt.Errorf("Failed to unmount path %v", mountPath)
}

// PathExists returns true if the specified path exists.
// TODO: clean this up to use pkg/util/file/FileExists
func PathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	} else if IsCorruptedMnt(err) {
		// 如果为挂载点损坏错误，也返回true
		return true, err
	}
	return false, err
}
