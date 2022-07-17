/*
Copyright 2016 The Kubernetes Authors.

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

package kubelet

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/util/removeall"
	"k8s.io/kubernetes/pkg/volume"
	volumetypes "k8s.io/kubernetes/pkg/volume/util/types"
)

// ListVolumesForPod returns a map of the mounted volumes for the given pod.
// The key in the map is the OuterVolumeSpecName (i.e. pod.Spec.Volumes[x].Name)
// pod的outerVolumeSpecName（比如pod.Spec.Volumes[x].Name）对应的volume.Mounter
func (kl *Kubelet) ListVolumesForPod(podUID types.UID) (map[string]volume.Volume, bool) {
	volumesToReturn := make(map[string]volume.Volume)
	// 获取pod已经挂载的volume
	podVolumes := kl.volumeManager.GetMountedVolumesForPod(
		volumetypes.UniquePodName(podUID))
	for outerVolumeSpecName, volume := range podVolumes {
		// TODO: volume.Mounter could be nil if volume object is recovered
		// from reconciler's sync state process. PR 33616 will fix this problem
		// to create Mounter object when recovering volume state.
		if volume.Mounter == nil {
			continue
		}
		volumesToReturn[outerVolumeSpecName] = volume.Mounter
	}

	return volumesToReturn, len(volumesToReturn) > 0
}

// podVolumesExist checks with the volume manager and returns true any of the
// pods for the specified volume are mounted.
// 在kl.volumeManager里获取到pod的已经挂载的volume，返回true
// kubelet数据目录里pod的volumes目录下存在挂载的目录，或获取pod的volumes目录下存在挂载的目录出现错误，返回true
func (kl *Kubelet) podVolumesExist(podUID types.UID) bool {
	if mountedVolumes :=
		// 获取pod的已经挂载的volume
		kl.volumeManager.GetMountedVolumesForPod(
			volumetypes.UniquePodName(podUID)); len(mountedVolumes) > 0 {
		return true
	}
	// TODO: This checks pod volume paths and whether they are mounted. If checking returns error, podVolumesExist will return true
	// which means we consider volumes might exist and requires further checking.
	// There are some volume plugins such as flexvolume might not have mounts. See issue #61229
	// 还有可能pod不存在，但是pod的volume目录还存在，且还存在挂载
	// 获取kubelet数据目录里pod的volumes目录下的所有有挂载的目录
	volumePaths, err := kl.getMountedVolumePathListFromDisk(podUID)
	if err != nil {
		klog.Errorf("pod %q found, but error %v occurred during checking mounted volumes from disk", podUID, err)
		return true
	}
	if len(volumePaths) > 0 {
		klog.V(4).Infof("pod %q found, but volumes are still mounted on disk %v", podUID, volumePaths)
		return true
	}

	return false
}

// newVolumeMounterFromPlugins attempts to find a plugin by volume spec, pod
// and volume options and then creates a Mounter.
// Returns a valid mounter or an error.
func (kl *Kubelet) newVolumeMounterFromPlugins(spec *volume.Spec, pod *v1.Pod, opts volume.VolumeOptions) (volume.Mounter, error) {
	plugin, err := kl.volumePluginMgr.FindPluginBySpec(spec)
	if err != nil {
		return nil, fmt.Errorf("can't use volume plugins for %s: %v", spec.Name(), err)
	}
	physicalMounter, err := plugin.NewMounter(spec, pod, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate mounter for volume: %s using plugin: %s with a root cause: %v", spec.Name(), plugin.GetPluginName(), err)
	}
	klog.V(10).Infof("Using volume plugin %q to mount %s", plugin.GetPluginName(), spec.Name())
	return physicalMounter, nil
}

// cleanupOrphanedPodDirs removes the volumes of pods that should not be
// running and that have no containers running.  Note that we roll up logs here since it runs in the main loop.
// pod目录下的pod文件夹的pod（不在在运行的pod或apiserver中的普通pod和static pod）
// 如果目录"/var/lib/kubelet/pods/{podUID}/volume"或volume-subpaths目录存在，则记录错误日志，提示“pod不存在，但是pod目录下volume目录存在或还存在挂载”
// 否则，移除目录"/var/lib/kubelet/pods/{podUID}"
func (kl *Kubelet) cleanupOrphanedPodDirs(pods []*v1.Pod, runningPods []*kubecontainer.Pod) error {
	allPods := sets.NewString()
	for _, pod := range pods {
		allPods.Insert(string(pod.UID))
	}
	for _, pod := range runningPods {
		allPods.Insert(string(pod.ID))
	}

	// 从pod目录（默认为"/var/lib/kubelet/pods"）中遍历出所有pod uid
	found, err := kl.listPodsFromDisk()
	if err != nil {
		return err
	}

	orphanRemovalErrors := []error{}
	orphanVolumeErrors := []error{}

	// 遍历pod目录下所有pod
	for _, uid := range found {
		// 跳过在运行的pod或apiserver中的普通pod和static pod
		if allPods.Has(string(uid)) {
			continue
		}
		// pod不在在运行的pod和apiserver中的普通pod和static pod

		// If volumes have not been unmounted/detached, do not delete directory.
		// Doing so may result in corruption of data.
		// TODO: getMountedVolumePathListFromDisk() call may be redundant with
		// kl.getPodVolumePathListFromDisk(). Can this be cleaned up?
		// 在kl.volumeManager里获取到pod的已经挂载的volume，返回true
		// kubelet数据目录里pod的volumes目录下存在挂载的目录，或获取pod的volumes目录下存在挂载的目录出现错误，返回true
		// 跳过kl.volumeManager里获取到pod的已经挂载的volume，或kubelet数据目录里pod的volumes目录下存在挂载的目录
		if podVolumesExist := kl.podVolumesExist(uid); podVolumesExist {
			klog.V(3).Infof("Orphaned pod %q found, but volumes are not cleaned up", uid)
			continue
		}

		// volumes目录下不存在挂载的目录（有可能存在目录或不存在目录）
		// If there are still volume directories, do not delete directory
		// 返回pod volume目录下（两层）的所有（pod的）文件和文件夹
		volumePaths, err := kl.getPodVolumePathListFromDisk(uid)
		// 发生错误，append错误到orphanVolumeErrors，继续下一个pod
		if err != nil {
			orphanVolumeErrors = append(orphanVolumeErrors, fmt.Errorf("orphaned pod %q found, but error %v occurred during reading volume dir from disk", uid, err))
			continue
		}
		// pod volume下有文件或文件夹，append 存在孤儿volume错误到orphanVolumeErrors，继续下一个pod
		if len(volumePaths) > 0 {
			orphanVolumeErrors = append(orphanVolumeErrors, fmt.Errorf("orphaned pod %q found, but volume paths are still present on disk", uid))
			continue
		}

		// If there are any volume-subpaths, do not cleanup directories
		// 返回"/var/lib/kubelet/pods/{podUID}/volume-subpaths"目录是否存在
		// 如果存在且路径是坏的挂载点，则返回true和错误
		volumeSubpathExists, err := kl.podVolumeSubpathsDirExists(uid)
		if err != nil {
			// 如果"/var/lib/kubelet/pods/{podUID}/volume-subpaths"目录存在且路径是坏的挂载点，则append 存在孤儿volume-subpaths且volume-subpaths读取错误，继续下一个pod
			orphanVolumeErrors = append(orphanVolumeErrors, fmt.Errorf("orphaned pod %q found, but error %v occurred during reading of volume-subpaths dir from disk", uid, err))
			continue
		}
		// 如果"/var/lib/kubelet/pods/{podUID}/volume-subpaths"目录存在，则存在孤儿volume-subpaths且volume-subpaths读取错误，继续下一个pod
		if volumeSubpathExists {
			orphanVolumeErrors = append(orphanVolumeErrors, fmt.Errorf("orphaned pod %q found, but volume subpaths are still present on disk", uid))
			continue
		}

		// pod不存在"/var/lib/kubelet/pods/{podUID}/volume"和volume-subpaths目录
		klog.V(3).Infof("Orphaned pod %q found, removing", uid)
		// 删除目录"/var/lib/kubelet/pods/{podUID}"，等同于执行rm -rf --one-file-system "/var/lib/kubelet/pods/{podUID}"
		if err := removeall.RemoveAllOneFilesystem(kl.mounter, kl.getPodDir(uid)); err != nil {
			klog.Errorf("Failed to remove orphaned pod %q dir; err: %v", uid, err)
			// 如果删除出错，则append错误到orphanRemovalErrors
			orphanRemovalErrors = append(orphanRemovalErrors, err)
		}
	}

	logSpew := func(errs []error) {
		if len(errs) > 0 {
			klog.Errorf("%v : There were a total of %v errors similar to this. Turn up verbosity to see them.", errs[0], len(errs))
			for _, err := range errs {
				klog.V(5).Infof("Orphan pod: %v", err)
			}
		}
	}
	logSpew(orphanVolumeErrors)
	logSpew(orphanRemovalErrors)
	return utilerrors.NewAggregate(orphanRemovalErrors)
}
