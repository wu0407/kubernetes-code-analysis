/*
Copyright 2017 The Kubernetes Authors.

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

package removeall

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"k8s.io/utils/mount"
)

// RemoveAllOneFilesystem removes path and any children it contains.
// It removes everything it can but returns the first error
// it encounters. If the path does not exist, RemoveAll
// returns nil (no error).
// It makes sure it does not cross mount boundary, i.e. it does *not* remove
// files from another filesystems. Like 'rm -rf --one-file-system'.
// It is copied from RemoveAll() sources, with IsLikelyNotMountPoint
// 删除目录所有文件和文件夹，或直接删除文件，等同于执行rm -rf --one-file-system
func RemoveAllOneFilesystem(mounter mount.Interface, path string) error {
	// Simple case: if Remove works, we're done.
	err := os.Remove(path)
	// 直接移除成功或路径不存在，直接返回
	if err == nil || os.IsNotExist(err) {
		return nil
	}

	// Otherwise, is this a directory we need to recurse into?
	// 不能直接移除，说明路径不是空的文件夹
	dir, serr := os.Lstat(path)
	if serr != nil {
		// 如果是路径操作错误，且路径不存在或路径不是目录的错误，直接返回nil
		if serr, ok := serr.(*os.PathError); ok && (os.IsNotExist(serr.Err) || serr.Err == syscall.ENOTDIR) {
			return nil
		}
		// 不是路径操作错误，返回错误
		return serr
	}
	// os.Lstat没有发生错误，移除不成功且不是目录，则返回移除时候发生的错误
	if !dir.IsDir() {
		// Not a directory; return the error from Remove.
		return err
	}

	// Directory.
	// os.Lstat没有发生错误，且路径是目录
	// 如果file的设备号与父目录的设备号不一样，说明file是挂载点，直接返回false
	// 否则从"/proc/mounts"文件里查找，是否为挂载路径
	isNotMount, err := mounter.IsLikelyNotMountPoint(path)
	if err != nil {
		return err
	}
	// 如果是挂载点，则返回错误
	if !isNotMount {
		return fmt.Errorf("cannot delete directory %s: it is a mount point", path)
	}

	fd, err := os.Open(path)
	if err != nil {
		// path已经不存在，则返回nil
		if os.IsNotExist(err) {
			// Race. It was deleted between the Lstat and Open.
			// Return nil per RemoveAll's docs.
			return nil
		}
		// 打开路径返回错误
		return err
	}

	// Remove contents & return first error.
	err = nil
	for {
		// 读取目录下，每次读取最多100个文件或文件夹
		names, err1 := fd.Readdirnames(100)
		for _, name := range names {
			// 递归的执行本函数RemoveAllOneFilesystem
			err1 := RemoveAllOneFilesystem(mounter, path+string(os.PathSeparator)+name)
			if err == nil {
				err = err1
			}
		}
		// 读取到目录中最后一个文件或文件夹，结束循环
		if err1 == io.EOF {
			break
		}
		// If Readdirnames returned an error, use it.
		if err == nil {
			err = err1
		}
		// 目录下没有文件或文件夹，结束循环
		if len(names) == 0 {
			break
		}
	}

	// Close directory, because windows won't remove opened directory.
	fd.Close()

	// Remove directory.
	err1 := os.Remove(path)
	if err1 == nil || os.IsNotExist(err1) {
		return nil
	}
	if err == nil {
		err = err1
	}
	return err
}
