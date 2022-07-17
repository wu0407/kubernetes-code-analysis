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

package cm

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	kubefeatures "k8s.io/kubernetes/pkg/features"
)

const (
	podCgroupNamePrefix = "pod"
)

// podContainerManagerImpl implements podContainerManager interface.
// It is the general implementation which allows pod level container
// management if qos Cgroup is enabled.
type podContainerManagerImpl struct {
	// qosContainersInfo hold absolute paths of the top level qos containers
	qosContainersInfo QOSContainersInfo
	// Stores the mounted cgroup subsystems
	subsystems *CgroupSubsystems
	// cgroupManager is the cgroup Manager Object responsible for managing all
	// pod cgroups.
	cgroupManager CgroupManager
	// Maximum number of pids in a pod
	podPidsLimit int64
	// enforceCPULimits controls whether cfs quota is enforced or not
	enforceCPULimits bool
	// cpuCFSQuotaPeriod is the cfs period value, cfs_period_us, setting per
	// node for all containers in usec
	cpuCFSQuotaPeriod uint64
}

// Make sure that podContainerManagerImpl implements the PodContainerManager interface
var _ PodContainerManager = &podContainerManagerImpl{}

// applyLimits sets pod cgroup resource limits
// It also updates the resource limits on top level qos containers.
func (m *podContainerManagerImpl) applyLimits(pod *v1.Pod) error {
	// This function will house the logic for setting the resource parameters
	// on the pod container config and updating top level qos container configs
	return nil
}

// Exists checks if the pod's cgroup already exists
// 在所有cgroup子系统中，pod的cgroup路径都存在。有一个子系统的路径不存在，则为false。
func (m *podContainerManagerImpl) Exists(pod *v1.Pod) bool {
	// 获得pod的cgroup路径，比如"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podec7bb47a_07ef_48ff_9201_687474994eab.slice"
	podContainerName, _ := m.GetPodContainerName(pod)
	// 在所有cgroup子系统中，pod的cgroup路径都存在。有一个子系统的路径不存在，则为false
	return m.cgroupManager.Exists(podContainerName)
}

// EnsureExists takes a pod as argument and makes sure that
// pod cgroup exists if qos cgroup hierarchy flag is enabled.
// If the pod level container doesn't already exist it is created.
// 在所有cgroup子系统，创建pod的cgroup路径，并设置相应cgroup（cpu、memory、pid、hugepage在系统中开启的）资源属性的限制
func (m *podContainerManagerImpl) EnsureExists(pod *v1.Pod) error {
	// 返回pod的cgroupName和cgroup路径
	podContainerName, _ := m.GetPodContainerName(pod)
	// check if container already exist
	// 在所有cgroup子系统中，pod的cgroup路径都存在。有一个子系统的路径不存在，则为false。
	alreadyExists := m.Exists(pod)
	if !alreadyExists {
		// Create the pod container
		containerConfig := &CgroupConfig{
			Name:               podContainerName,
			// 计算pod里所有container的各个类型的资源总和
			// m.enforceCPULimits默认为true
			// m.cpuCFSQuotaPeriod默认为100ms
			ResourceParameters: ResourceConfigForPod(pod, m.enforceCPULimits, m.cpuCFSQuotaPeriod),
		}
		if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.SupportPodPidsLimit) && m.podPidsLimit > 0 {
			// 启用"SupportPodPidsLimit"功能，且设置了m.podPidsLimit，则设置PidsLimit
			containerConfig.ResourceParameters.PidsLimit = &m.podPidsLimit
		}
		// cgroup driver为systemd
		//   1. 调用systemdDbus执行systemd临时unit（类似执行systemd-run命令）来创建相应的cgroup目录，并设置相应的资源属性值（MemoryLimit、"CPUShares"、"CPUQuotaPerSecUSec"、"BlockIOWeight"、"TasksMax"）这里没有支持cpuset设置，这个需要systemd244版本
		//   2. 设置各个cgroup系统（cpu、memory、pid、hugepage在系统中开启的）的属性
		// cgroup driver为cgroupfs
		//   1. 创建各个cgroup系统目录，cpuset和memory和cpu子系统设置一些基本的cgroup子系统属性
		//   2. 设置各个cgroup系统（cpu、memory、pid、hugepage在系统中开启的）的属性
		if err := m.cgroupManager.Create(containerConfig); err != nil {
			return fmt.Errorf("failed to create container for %v : %v", podContainerName, err)
		}
	}
	// Apply appropriate resource limits on the pod container
	// Top level qos containers limits are not updated
	// until we figure how to maintain the desired state in the kubelet.
	// Because maintaining the desired state is difficult without checkpointing.
	// 目前代码里不做任何事情
	if err := m.applyLimits(pod); err != nil {
		return fmt.Errorf("failed to apply resource limits on container for %v : %v", podContainerName, err)
	}
	return nil
}

// GetPodContainerName returns the CgroupName identifier, and its literal cgroupfs form on the host.
// 返回pod的cgroupName和cgroup路径
func (m *podContainerManagerImpl) GetPodContainerName(pod *v1.Pod) (CgroupName, string) {
	// 获取pod的qos class类别
	podQOS := v1qos.GetPodQOS(pod)
	// Get the parent QOS container name
	var parentContainer CgroupName
	switch podQOS {
	case v1.PodQOSGuaranteed:
		parentContainer = m.qosContainersInfo.Guaranteed
	case v1.PodQOSBurstable:
		parentContainer = m.qosContainersInfo.Burstable
	case v1.PodQOSBestEffort:
		parentContainer = m.qosContainersInfo.BestEffort
	}
	// 返回字符串"pod"+{pod uid}
	podContainer := GetPodCgroupNameSuffix(pod.UID)

	// Get the absolute path of the cgroup
	// 比如["kubepods", "besteffort", "podec7bb47a-07ef-48ff-9201-687474994eab"]
	cgroupName := NewCgroupName(parentContainer, podContainer)
	// Get the literal cgroupfs name
	// /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podec7bb47a_07ef_48ff_9201_687474994eab.slice
	cgroupfsName := m.cgroupManager.Name(cgroupName)

	return cgroupName, cgroupfsName
}

// Kill one process ID
// kill pid进程
func (m *podContainerManagerImpl) killOnePid(pid int) error {
	// os.FindProcess never returns an error on POSIX
	// https://go-review.googlesource.com/c/go/+/19093
	p, _ := os.FindProcess(pid)
	if err := p.Kill(); err != nil {
		// If the process already exited, that's fine.
		if strings.Contains(err.Error(), "process already finished") {
			// Hate parsing strings, but
			// vendor/github.com/opencontainers/runc/libcontainer/
			// also does this.
			klog.V(3).Infof("process with pid %v no longer exists", pid)
			return nil
		}
		return err
	}
	return nil
}

// Scan through the whole cgroup directory and kill all processes either
// attached to the pod cgroup or to a container cgroup under the pod cgroup
// kill podCgroup的cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
// 如果kill失败，最多进行5次重试
func (m *podContainerManagerImpl) tryKillingCgroupProcesses(podCgroup CgroupName) error {
	// 获得所有cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
	pidsToKill := m.cgroupManager.Pids(podCgroup)
	// No pids charged to the terminated pod cgroup return
	if len(pidsToKill) == 0 {
		return nil
	}

	var errlist []error
	// os.Kill often errors out,
	// We try killing all the pids multiple times
	removed := map[int]bool{}
	for i := 0; i < 5; i++ {
		if i != 0 {
			klog.V(3).Infof("Attempt %v failed to kill all unwanted process from cgroup: %v. Retyring", i, podCgroup)
		}
		errlist = []error{}
		for _, pid := range pidsToKill {
			// 过滤掉pidsToKill中已经被kill的pid
			if _, ok := removed[pid]; ok {
				continue
			}
			klog.V(3).Infof("Attempt to kill process with pid: %v from cgroup: %v", pid, podCgroup)
			// kill pid进程
			if err := m.killOnePid(pid); err != nil {
				klog.V(3).Infof("failed to kill process with pid: %v from cgroup: %v", pid, podCgroup)
				errlist = append(errlist, err)
			} else {
				removed[pid] = true
			}
		}
		if len(errlist) == 0 {
			klog.V(3).Infof("successfully killed all unwanted processes from cgroup: %v", podCgroup)
			return nil
		}
	}
	return utilerrors.NewAggregate(errlist)
}

// Destroy destroys the pod container cgroup paths
// kill podCgroup的cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
// 移除所有cgroup子系统里的cgroup路径
func (m *podContainerManagerImpl) Destroy(podCgroup CgroupName) error {
	// Try killing all the processes attached to the pod cgroup
	// kill podCgroup的cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
	if err := m.tryKillingCgroupProcesses(podCgroup); err != nil {
		klog.V(3).Infof("failed to kill all the processes attached to the %v cgroups", podCgroup)
		return fmt.Errorf("failed to kill all the processes attached to the %v cgroups : %v", podCgroup, err)
	}

	// Now its safe to remove the pod's cgroup
	containerConfig := &CgroupConfig{
		Name:               podCgroup,
		ResourceParameters: &ResourceConfig{},
	}
	// 移除所有cgroup子系统里的cgroup路径
	if err := m.cgroupManager.Destroy(containerConfig); err != nil {
		return fmt.Errorf("failed to delete cgroup paths for %v : %v", podCgroup, err)
	}
	return nil
}

// ReduceCPULimits reduces the CPU CFS values to the minimum amount of shares.
// 设置cgroup name的cpu share的值为2
func (m *podContainerManagerImpl) ReduceCPULimits(podCgroup CgroupName) error {
	return m.cgroupManager.ReduceCPULimits(podCgroup)
}

// IsPodCgroup returns true if the literal cgroupfs name corresponds to a pod
func (m *podContainerManagerImpl) IsPodCgroup(cgroupfs string) (bool, types.UID) {
	// convert the literal cgroupfs form to the driver specific value
	cgroupName := m.cgroupManager.CgroupName(cgroupfs)
	qosContainersList := [3]CgroupName{m.qosContainersInfo.BestEffort, m.qosContainersInfo.Burstable, m.qosContainersInfo.Guaranteed}
	basePath := ""
	for _, qosContainerName := range qosContainersList {
		// a pod cgroup is a direct child of a qos node, so check if its a match
		if len(cgroupName) == len(qosContainerName)+1 {
			basePath = cgroupName[len(qosContainerName)]
		}
	}
	if basePath == "" {
		return false, types.UID("")
	}
	if !strings.HasPrefix(basePath, podCgroupNamePrefix) {
		return false, types.UID("")
	}
	parts := strings.Split(basePath, podCgroupNamePrefix)
	if len(parts) != 2 {
		return false, types.UID("")
	}
	return true, types.UID(parts[1])
}

// GetAllPodsFromCgroups scans through all the subsystems of pod cgroups
// Get list of pods whose cgroup still exist on the cgroup mounts
// 遍历所有pod的cgroup目录，获得pod uid与对应的CgroupName，比如"ec7bb47a-07ef-48ff-9201-687474994eab"对应["kubepods", "besteffort", "podec7bb47a-07ef-48ff-9201-687474994eab"]
func (m *podContainerManagerImpl) GetAllPodsFromCgroups() (map[types.UID]CgroupName, error) {
	// Map for storing all the found pods on the disk
	foundPods := make(map[types.UID]CgroupName)
	qosContainersList := [3]CgroupName{m.qosContainersInfo.BestEffort, m.qosContainersInfo.Burstable, m.qosContainersInfo.Guaranteed}
	// Scan through all the subsystem mounts
	// and through each QoS cgroup directory for each subsystem mount
	// If a pod cgroup exists in even a single subsystem mount
	// we will attempt to delete it
	// 遍历系统所有cgroup子系统的在主机的挂载点，比如cpu在/sys/fs/cgroup/cpu,cpuacct
	for _, val := range m.subsystems.MountPoints {
		// 遍历cgroup子目录下的qos路径, 比如["kubepods", "besteffort"]对应/kubepods.slice/kubepods-burstable.slice
		for _, qosContainerName := range qosContainersList {
			// get the subsystems QoS cgroup absolute name
			// 比如["kubepods", "besteffort"]对应/kubepods.slice/kubepods-burstable.slice
			qcConversion := m.cgroupManager.Name(qosContainerName)
			// 路径为/sys/fs/cgroup/cpu,cpuacct/kubepods.slice/kubepods-burstable.slice
			qc := path.Join(val, qcConversion)
			dirInfo, err := ioutil.ReadDir(qc)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, fmt.Errorf("failed to read the cgroup directory %v : %v", qc, err)
			}
			// 遍历所有下面的cgroup目录
			for i := range dirInfo {
				// its not a directory, so continue on...
				if !dirInfo[i].IsDir() {
					continue
				}
				// convert the concrete cgroupfs name back to an internal identifier
				// this is needed to handle path conversion for systemd environments.
				// we pass the fully qualified path so decoding can work as expected
				// since systemd encodes the path in each segment.
				// 路径是/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podec7bb47a_07ef_48ff_9201_687474994eab.slice
				cgroupfsPath := path.Join(qcConversion, dirInfo[i].Name())
				// 解析成["kubepods", "besteffort", "podec7bb47a-07ef-48ff-9201-687474994eab"]
				internalPath := m.cgroupManager.CgroupName(cgroupfsPath)
				// we only care about base segment of the converted path since that
				// is what we are reading currently to know if it is a pod or not.
				// basePath为"podec7bb47a-07ef-48ff-9201-687474994eab"
				basePath := internalPath[len(internalPath)-1]
				// basePath不包含"pod"前缀，则继续（说明不是pod cgroup）
				if !strings.Contains(basePath, podCgroupNamePrefix) {
					continue
				}
				// we then split the name on the pod prefix to determine the uid
				parts := strings.Split(basePath, podCgroupNamePrefix)
				// the uid is missing, so we log the unexpected cgroup not of form pod<uid>
				if len(parts) != 2 {
					klog.Errorf("pod cgroup manager ignoring unexpected cgroup %v because it is not a pod", cgroupfsPath)
					continue
				}
				// "ec7bb47a-07ef-48ff-9201-687474994eab"
				podUID := parts[1]
				// pod uid "ec7bb47a-07ef-48ff-9201-687474994eab"对应["kubepods", "besteffort", "podec7bb47a-07ef-48ff-9201-687474994eab"]
				foundPods[types.UID(podUID)] = internalPath
			}
		}
	}
	return foundPods, nil
}

// podContainerManagerNoop implements podContainerManager interface.
// It is a no-op implementation and basically does nothing
// podContainerManagerNoop is used in case the QoS cgroup Hierarchy is not
// enabled, so Exists() returns true always as the cgroupRoot
// is expected to always exist.
type podContainerManagerNoop struct {
	cgroupRoot CgroupName
}

// Make sure that podContainerManagerStub implements the PodContainerManager interface
var _ PodContainerManager = &podContainerManagerNoop{}

func (m *podContainerManagerNoop) Exists(_ *v1.Pod) bool {
	return true
}

func (m *podContainerManagerNoop) EnsureExists(_ *v1.Pod) error {
	return nil
}

func (m *podContainerManagerNoop) GetPodContainerName(_ *v1.Pod) (CgroupName, string) {
	return m.cgroupRoot, ""
}

func (m *podContainerManagerNoop) GetPodContainerNameForDriver(_ *v1.Pod) string {
	return ""
}

// Destroy destroys the pod container cgroup paths
func (m *podContainerManagerNoop) Destroy(_ CgroupName) error {
	return nil
}

func (m *podContainerManagerNoop) ReduceCPULimits(_ CgroupName) error {
	return nil
}

func (m *podContainerManagerNoop) GetAllPodsFromCgroups() (map[types.UID]CgroupName, error) {
	return nil, nil
}

func (m *podContainerManagerNoop) IsPodCgroup(cgroupfs string) (bool, types.UID) {
	return false, types.UID("")
}
