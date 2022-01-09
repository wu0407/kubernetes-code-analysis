// +build linux

/*
Copyright 2015 The Kubernetes Authors.

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
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fs"
	"github.com/opencontainers/runc/libcontainer/configs"
	"k8s.io/klog"
	utilio "k8s.io/utils/io"
	"k8s.io/utils/mount"
	utilpath "k8s.io/utils/path"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	internalapi "k8s.io/cri-api/pkg/apis"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	podresourcesapi "k8s.io/kubernetes/pkg/kubelet/apis/podresources/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/cadvisor"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/containermap"
	cputopology "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	cmutil "k8s.io/kubernetes/pkg/kubelet/cm/util"
	"k8s.io/kubernetes/pkg/kubelet/config"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/qos"
	"k8s.io/kubernetes/pkg/kubelet/stats/pidlimit"
	"k8s.io/kubernetes/pkg/kubelet/status"
	schedulernodeinfo "k8s.io/kubernetes/pkg/scheduler/nodeinfo"
	"k8s.io/kubernetes/pkg/util/oom"
	"k8s.io/kubernetes/pkg/util/procfs"
	utilsysctl "k8s.io/kubernetes/pkg/util/sysctl"
)

const (
	dockerProcessName     = "dockerd"
	dockerPidFile         = "/var/run/docker.pid"
	containerdProcessName = "docker-containerd"
	containerdPidFile     = "/run/docker/libcontainerd/docker-containerd.pid"
	maxPidFileLength      = 1 << 10 // 1KB
)

var (
	// The docker version in which containerd was introduced.
	containerdAPIVersion = utilversion.MustParseGeneric("1.23")
)

// A non-user container tracked by the Kubelet.
type systemContainer struct {
	// Absolute name of the container.
	name string

	// CPU limit in millicores.
	cpuMillicores int64

	// Function that ensures the state of the container.
	// m is the cgroup manager for the specified container.
	ensureStateFunc func(m *fs.Manager) error

	// Manager for the cgroups of the external container.
	manager *fs.Manager
}

func newSystemCgroups(containerName string) *systemContainer {
	return &systemContainer{
		name:    containerName,
		manager: createManager(containerName),
	}
}

type containerManagerImpl struct {
	sync.RWMutex
	cadvisorInterface cadvisor.Interface
	mountUtil         mount.Interface
	NodeConfig
	status Status
	// External containers being managed.
	systemContainers []*systemContainer
	// Tasks that are run periodically
	periodicTasks []func()
	// Holds all the mounted cgroup subsystems
	subsystems *CgroupSubsystems
	nodeInfo   *v1.Node
	// Interface for cgroup management
	cgroupManager CgroupManager
	// Capacity of this node.
	capacity v1.ResourceList
	// Capacity of this node, including internal resources.
	internalCapacity v1.ResourceList
	// Absolute cgroupfs path to a cgroup that Kubelet needs to place all pods under.
	// This path include a top level container for enforcing Node Allocatable.
	cgroupRoot CgroupName
	// Event recorder interface.
	recorder record.EventRecorder
	// Interface for QoS cgroup management
	qosContainerManager QOSContainerManager
	// Interface for exporting and allocating devices reported by device plugins.
	deviceManager devicemanager.Manager
	// Interface for CPU affinity management.
	cpuManager cpumanager.Manager
	// Interface for Topology resource co-ordination
	topologyManager topologymanager.Manager
}

type features struct {
	cpuHardcapping bool
}

var _ ContainerManager = &containerManagerImpl{}

// checks if the required cgroups subsystems are mounted.
// As of now, only 'cpu' and 'memory' are required.
// cpu quota is a soft requirement.
func validateSystemRequirements(mountUtil mount.Interface) (features, error) {
	const (
		cgroupMountType = "cgroup"
		localErr        = "system validation failed"
	)
	var (
		cpuMountPoint string
		f             features
	)
	mountPoints, err := mountUtil.List()
	if err != nil {
		return f, fmt.Errorf("%s - %v", localErr, err)
	}

	expectedCgroups := sets.NewString("cpu", "cpuacct", "cpuset", "memory")
	for _, mountPoint := range mountPoints {
		if mountPoint.Type == cgroupMountType {
			for _, opt := range mountPoint.Opts {
				if expectedCgroups.Has(opt) {
					expectedCgroups.Delete(opt)
				}
				if opt == "cpu" {
					cpuMountPoint = mountPoint.Path
				}
			}
		}
	}

	if expectedCgroups.Len() > 0 {
		return f, fmt.Errorf("%s - Following Cgroup subsystem not mounted: %v", localErr, expectedCgroups.List())
	}

	// Check if cpu quota is available.
	// CPU cgroup is required and so it expected to be mounted at this point.
	periodExists, err := utilpath.Exists(utilpath.CheckFollowSymlink, path.Join(cpuMountPoint, "cpu.cfs_period_us"))
	if err != nil {
		klog.Errorf("failed to detect if CPU cgroup cpu.cfs_period_us is available - %v", err)
	}
	quotaExists, err := utilpath.Exists(utilpath.CheckFollowSymlink, path.Join(cpuMountPoint, "cpu.cfs_quota_us"))
	if err != nil {
		klog.Errorf("failed to detect if CPU cgroup cpu.cfs_quota_us is available - %v", err)
	}
	if quotaExists && periodExists {
		f.cpuHardcapping = true
	}
	return f, nil
}

// TODO(vmarmol): Add limits to the system containers.
// Takes the absolute name of the specified containers.
// Empty container name disables use of the specified container.
func NewContainerManager(mountUtil mount.Interface, cadvisorInterface cadvisor.Interface, nodeConfig NodeConfig, failSwapOn bool, devicePluginEnabled bool, recorder record.EventRecorder) (ContainerManager, error) {
	// 系统所有cgroup子系统的在主机的挂载点，比如cpu在/sys/fs/cgroup/cpu,cpuacct
	// 包括挂载点的root根路径、挂载点、子系统名字
	subsystems, err := GetCgroupSubsystems()
	if err != nil {
		return nil, fmt.Errorf("failed to get mounted cgroup subsystems: %v", err)
	}

	if failSwapOn {
		// Check whether swap is enabled. The Kubelet does not support running with swap enabled.
		swapData, err := ioutil.ReadFile("/proc/swaps")
		if err != nil {
			return nil, err
		}
		swapData = bytes.TrimSpace(swapData) // extra trailing \n
		swapLines := strings.Split(string(swapData), "\n")

		// If there is more than one line (table headers) in /proc/swaps, swap is enabled and we should
		// error out unless --fail-swap-on is set to false.
		if len(swapLines) > 1 {
			return nil, fmt.Errorf("running with swap on is not supported, please disable swap! or set --fail-swap-on flag to false. /proc/swaps contained: %v", swapLines)
		}
	}

	var internalCapacity = v1.ResourceList{}
	// It is safe to invoke `MachineInfo` on cAdvisor before logically initializing cAdvisor here because
	// machine info is computed and cached once as part of cAdvisor object creation.
	// But `RootFsInfo` and `ImagesFsInfo` are not available at this moment so they will be called later during manager starts
	machineInfo, err := cadvisorInterface.MachineInfo()
	if err != nil {
		return nil, err
	}
	// Correct NUMA information is currently missing from cadvisor's
	// MachineInfo struct, so we use the CPUManager's internal logic for
	// gathering NUMANodeInfo to pass to components that care about it.
	numaNodeInfo, err := cputopology.GetNUMANodeInfo()
	if err != nil {
		return nil, err
	}
	// 包括cpu、内存、hugepage
	capacity := cadvisor.CapacityFromMachineInfo(machineInfo)
	for k, v := range capacity {
		internalCapacity[k] = v
	}
	// 读取/proc/sys/kernel/pid_max获得系统最大的运行的pid数量和调用syscall.Sysinfo获得当前运行的进程数
	pidlimits, err := pidlimit.Stats()
	if err == nil && pidlimits != nil && pidlimits.MaxPID != nil {
		internalCapacity[pidlimit.PIDs] = *resource.NewQuantity(
			int64(*pidlimits.MaxPID),
			resource.DecimalSI)
	}

	// Turn CgroupRoot from a string (in cgroupfs path format) to internal CgroupName
	// 默认nodeConfig.CgroupRoot为 "/"，则cgroupRoot这里为空
	cgroupRoot := ParseCgroupfsToCgroupName(nodeConfig.CgroupRoot)
	cgroupManager := NewCgroupManager(subsystems, nodeConfig.CgroupDriver)
	// Check if Cgroup-root actually exists on the node
	// 默认为true
	if nodeConfig.CgroupsPerQOS {
		// this does default to / when enabled, but this tests against regressions.
		if nodeConfig.CgroupRoot == "" {
			return nil, fmt.Errorf("invalid configuration: cgroups-per-qos was specified and cgroup-root was not specified. To enable the QoS cgroup hierarchy you need to specify a valid cgroup-root")
		}

		// we need to check that the cgroup root actually exists for each subsystem
		// of note, we always use the cgroupfs driver when performing this check since
		// the input is provided in that format.
		// this is important because we do not want any name conversion to occur.
		if !cgroupManager.Exists(cgroupRoot) {
			return nil, fmt.Errorf("invalid configuration: cgroup-root %q doesn't exist", cgroupRoot)
		}
		klog.Infof("container manager verified user specified cgroup-root exists: %v", cgroupRoot)
		// Include the top level cgroup for enforcing node allocatable into cgroup-root.
		// This way, all sub modules can avoid having to understand the concept of node allocatable.
		// 这里cgroupRoot变成["kubepods"]
		cgroupRoot = NewCgroupName(cgroupRoot, defaultNodeAllocatableCgroupName)
	}
	klog.Infof("Creating Container Manager object based on Node Config: %+v", nodeConfig)

	qosContainerManager, err := NewQOSContainerManager(subsystems, cgroupRoot, nodeConfig, cgroupManager)
	if err != nil {
		return nil, err
	}

	cm := &containerManagerImpl{
		cadvisorInterface:   cadvisorInterface,
		mountUtil:           mountUtil,
		NodeConfig:          nodeConfig,
		subsystems:          subsystems,
		cgroupManager:       cgroupManager,
		// 包括cpu、内存、hugepage
		capacity:            capacity,
		// 包括cpu 内存、hugepage、pid
		internalCapacity:    internalCapacity,
		// 默认CgroupsPerQOS是true，所以就变成["kubepods"]
		cgroupRoot:          cgroupRoot,
		recorder:            recorder,
		qosContainerManager: qosContainerManager,
	}

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.TopologyManager) {
		cm.topologyManager, err = topologymanager.NewManager(
			numaNodeInfo,
			// 默认为none
			nodeConfig.ExperimentalTopologyManagerPolicy,
		)

		if err != nil {
			return nil, err
		}

		klog.Infof("[topologymanager] Initializing Topology Manager with %s policy", nodeConfig.ExperimentalTopologyManagerPolicy)
	} else {
		cm.topologyManager = topologymanager.NewFakeManager()
	}

	klog.Infof("Creating device plugin manager: %t", devicePluginEnabled)
	// 默认为true
	if devicePluginEnabled {
		cm.deviceManager, err = devicemanager.NewManagerImpl(numaNodeInfo, cm.topologyManager)
		cm.topologyManager.AddHintProvider(cm.deviceManager)
	} else {
		cm.deviceManager, err = devicemanager.NewManagerStub()
	}
	if err != nil {
		return nil, err
	}

	// Initialize CPU manager
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUManager) {
		cm.cpuManager, err = cpumanager.NewManager(
			// 可以设置为none或static
			nodeConfig.ExperimentalCPUManagerPolicy,
			// 默认为10s
			nodeConfig.ExperimentalCPUManagerReconcilePeriod,
			machineInfo,
			numaNodeInfo,
			// 默认为空
			nodeConfig.NodeAllocatableConfig.ReservedSystemCPUs,
			// 各类型资源（cpu、内存、hugepage）需要保留大小
			cm.GetNodeAllocatableReservation(),
			nodeConfig.KubeletRootDir,
			cm.topologyManager,
		)
		if err != nil {
			klog.Errorf("failed to initialize cpu manager: %v", err)
			return nil, err
		}
		cm.topologyManager.AddHintProvider(cm.cpuManager)
	}

	return cm, nil
}

// NewPodContainerManager is a factory method returns a PodContainerManager object
// If qosCgroups are enabled then it returns the general pod container manager
// otherwise it returns a no-op manager which essentially does nothing
func (cm *containerManagerImpl) NewPodContainerManager() PodContainerManager {
	if cm.NodeConfig.CgroupsPerQOS {
		return &podContainerManagerImpl{
			qosContainersInfo: cm.GetQOSContainersInfo(),
			subsystems:        cm.subsystems,
			cgroupManager:     cm.cgroupManager,
			podPidsLimit:      cm.ExperimentalPodPidsLimit,
			enforceCPULimits:  cm.EnforceCPULimits,
			cpuCFSQuotaPeriod: uint64(cm.CPUCFSQuotaPeriod / time.Microsecond),
		}
	}
	return &podContainerManagerNoop{
		cgroupRoot: cm.cgroupRoot,
	}
}

func (cm *containerManagerImpl) InternalContainerLifecycle() InternalContainerLifecycle {
	return &internalContainerLifecycleImpl{cm.cpuManager, cm.topologyManager}
}

// Create a cgroup container manager.
func createManager(containerName string) *fs.Manager {
	allowAllDevices := true
	return &fs.Manager{
		Cgroups: &configs.Cgroup{
			Parent: "/",
			Name:   containerName,
			Resources: &configs.Resources{
				AllowAllDevices: &allowAllDevices,
			},
		},
	}
}

type KernelTunableBehavior string

const (
	KernelTunableWarn   KernelTunableBehavior = "warn"
	KernelTunableError  KernelTunableBehavior = "error"
	KernelTunableModify KernelTunableBehavior = "modify"
)

// setupKernelTunables validates kernel tunable flags are set as expected
// depending upon the specified option, it will either warn, error, or modify the kernel tunable flags
func setupKernelTunables(option KernelTunableBehavior) error {
	desiredState := map[string]int{
		utilsysctl.VMOvercommitMemory: utilsysctl.VMOvercommitMemoryAlways,
		utilsysctl.VMPanicOnOOM:       utilsysctl.VMPanicOnOOMInvokeOOMKiller,
		utilsysctl.KernelPanic:        utilsysctl.KernelPanicRebootTimeout,
		utilsysctl.KernelPanicOnOops:  utilsysctl.KernelPanicOnOopsAlways,
		utilsysctl.RootMaxKeys:        utilsysctl.RootMaxKeysSetting,
		utilsysctl.RootMaxBytes:       utilsysctl.RootMaxBytesSetting,
	}

	sysctl := utilsysctl.New()

	errList := []error{}
	for flag, expectedValue := range desiredState {
		val, err := sysctl.GetSysctl(flag)
		if err != nil {
			errList = append(errList, err)
			continue
		}
		if val == expectedValue {
			continue
		}

		switch option {
		case KernelTunableError:
			errList = append(errList, fmt.Errorf("invalid kernel flag: %v, expected value: %v, actual value: %v", flag, expectedValue, val))
		case KernelTunableWarn:
			klog.V(2).Infof("Invalid kernel flag: %v, expected value: %v, actual value: %v", flag, expectedValue, val)
		case KernelTunableModify:
			klog.V(2).Infof("Updating kernel flag: %v, expected value: %v, actual value: %v", flag, expectedValue, val)
			err = sysctl.SetSysctl(flag, expectedValue)
			if err != nil {
				errList = append(errList, err)
			}
		}
	}
	return utilerrors.NewAggregate(errList)
}

// 主要做以下事情：
// 1. 检查系统里cgroup是否开启"cpu"、"cpuacct", "cpuset", "memory"子系统，cpu子系统开启cfs
// 2. 设置一些关键的sysctl项
// 3. 确保各个cgroup子系统挂载目录下kubepods或kubepods.slice文件夹存在
//    设置各个cgroup系统（memory、pid、cpu share、hugepage）的属性值--这个值是根据cm.internalCapacity列表，各个资源类型的值减去SystemReserved和KubeReserved
//    启动qosmanager
//      确保Burstable和BestEffort的cgroup目录存在，Burstable目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/burstable，BestEffort目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/besteffort
//      启动一个gorutine 每一份钟更新Burstable和BestEffort的cgroup目录的cgroup资源属性
// 4. 应用--enforce-node-allocatable的配置，设置各个（cpu、memeory、pid、hugepage）cgroup的限制值
//    为pods，则设置在/sys/fs/cgroup/{cgroup sub system}/kubepods.slice，限制值为各个类型capacity减去SystemReserved和KubeReserved
//    为system-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename system-reserved-cgroup}，限制值为各个类型system-reserved值
//    为kube-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename kube-reserved-cgroup}，限制值为各个类型kube-reserved
// 5. 设置cm.periodicTasks和cm.systemContainers
//   cm.periodicTasks里可能有
//     当运行时为docker，设置cm.RuntimeCgroupsName为docker的cgroup路径
//     当cm.KubeletCgroupsName为空（没有配置kubelet cgroup），设置kubelet进程的oom_score_adj为-999
//   cm.systemContainers里可能有
//    当cm.SystemCgroupsName不为空， cm.SystemCgroupsName: newSystemCgroups(cm.SystemCgroupsName), ensureStateFunc: 将非内核pid或非init进程的pid移动到cm.SystemCgroupsNamecgroup中
//    当cm.kubeletCgroupsName不为空，cm.kubeletCgroupsName: newSystemCgroups(cm.KubeletCgroupsName), ensureStateFunc: 设置kubelet进程的oom_score_adj为-999,将kubelet进程移动到kubeletCgroupsName的cgroup路径中
func (cm *containerManagerImpl) setupNode(activePods ActivePodsFunc) error {
	// /proc/mount里cgroup挂载目录下是否有"cpu", "cpuacct", "cpuset", "memory"且cpu的cgroup是否有cpu.cfs_period_us和cpu.cfs_quota_us文件
	f, err := validateSystemRequirements(cm.mountUtil)
	if err != nil {
		return err
	}
	// cpu的cgroup有cpu.cfs_period_us和cpu.cfs_quota_us文件，则f.cpuHardcapping为true
	if !f.cpuHardcapping {
		cm.status.SoftRequirements = fmt.Errorf("CPU hardcapping unsupported")
	}
	b := KernelTunableModify
	// 默认ProtectKernelDefaults为false
	if cm.GetNodeConfig().ProtectKernelDefaults {
		b = KernelTunableError
	}
	// 设置一些关键的sysctl项
	// /sys/fs/vm/overcommit_memory=1
	// /sys/fs/vm/panic_on_oom=0
	// /sys/fs/kernel/panic=10
	// /sys/fs/kernel/panic_on_oops=1
	// /sys/fs/kernel/keys/root_maxkeys=1000000
	// /sys/fs/kernel/keys/root_maxbytes=25000000
	if err := setupKernelTunables(b); err != nil {
		return err
	}

	// Setup top level qos containers only if CgroupsPerQOS flag is specified as true
	if cm.NodeConfig.CgroupsPerQOS {
		// 确保各个cgroup子系统挂载目录下kubepods或kubepods.slice文件夹存在
		// 设置各个cgroup系统（memory、pid、cpu share、hugepage）的属性值--这个值是根据cm.internalCapacity列表，各个资源类型的值减去SystemReserved和KubeReserved
		if err := cm.createNodeAllocatableCgroups(); err != nil {
			return err
		}
		// 1. 确保Burstable和BestEffort的cgroup目录存在，Burstable目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/burstable，BestEffort目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/besteffort
		// 2. 启动一个goroutine 每一份钟更新Burstable和BestEffort的cgroup目录的cgroup资源属性 
		err = cm.qosContainerManager.Start(cm.getNodeAllocatableAbsolute, activePods)
		if err != nil {
			return fmt.Errorf("failed to initialize top level QOS containers: %v", err)
		}
	}

	// Enforce Node Allocatable (if required)
	// 应用--enforce-node-allocatable的配置，设置各个（cpu、memory、pid、hugepage）cgroup的限制值
	// 为pods，则设置在/sys/fs/cgroup/{cgroup sub system}/kubepods.slice，限制值为各个类型capacity减去SystemReserved和KubeReserved
	// 为system-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename system-reserved-cgroup}，限制值为各个类型system-reserved值
	// 为kube-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename kube-reserved-cgroup}，限制值为各个类型kube-reserved
	if err := cm.enforceNodeAllocatableCgroups(); err != nil {
		return err
	}

	systemContainers := []*systemContainer{}
	// 添加任务（设置cm.RuntimeCgroupsName为docker的cgroup路径）到cm.periodicTasks
	if cm.ContainerRuntime == "docker" {
		// With the docker-CRI integration, dockershim manages the cgroups
		// and oom score for the docker processes.
		// Check the cgroup for docker periodically, so kubelet can serve stats for the docker runtime.
		// TODO(KEP#866): remove special processing for CRI "docker" enablement
		cm.periodicTasks = append(cm.periodicTasks, func() {
			klog.V(4).Infof("[ContainerManager]: Adding periodic tasks for docker CRI integration")
			// 从进程名或pid文件中查找进程pid，读取/proc/{pid}/cgroup，获得进程的cgroup路径，比如/system.slice/kubelet.service
			cont, err := getContainerNameForProcess(dockerProcessName, dockerPidFile)
			if err != nil {
				klog.Error(err)
				return
			}
			klog.V(2).Infof("[ContainerManager]: Discovered runtime cgroups name: %s", cont)
			cm.Lock()
			defer cm.Unlock()
			cm.RuntimeCgroupsName = cont
		})
	}

	// 添加cm.SystemCgroupsName的systemContainer到systemContainers
	// 命令行参数解释
	// --system-cgroups string   Optional absolute name of cgroups in which to place all non-kernel processes that are not already inside a cgroup under '/'. Empty for no container. Rolling back the flag requires a reboot. 
	// 如果配置了--system-cgroups或配置文件systemCgroups，则将非内核pid或非init进程的pid移动到指定的cgroup路径中。默认是空
	if cm.SystemCgroupsName != "" {
		if cm.SystemCgroupsName == "/" {
			return fmt.Errorf("system container cannot be root (\"/\")")
		}
		cont := newSystemCgroups(cm.SystemCgroupsName)
		cont.ensureStateFunc = func(manager *fs.Manager) error {
			// 将非内核pid或非init进程的pid移动到指定的cgroup中
			return ensureSystemCgroups("/", manager)
		}
		systemContainers = append(systemContainers, cont)
	}

	// 添加cm.KubeletCgroupsName的systemContainer到systemContainers或添加任务（设置cm.KubeletCgroupsName为kubelet进程的cgroup路径和设置kubelet进程的oom_score_adj为-999）到cm.periodicTasks
	// 设置kubelet进程的oom_score_adj为-999
	// 如果配置了--kubelet-cgroups或配置文件KubeletCgroups，则将kubelet进程移动到指定的cgroup路径中
	if cm.KubeletCgroupsName != "" {
		cont := newSystemCgroups(cm.KubeletCgroupsName)
		allowAllDevices := true
		manager := fs.Manager{
			Cgroups: &configs.Cgroup{
				Parent: "/",
				Name:   cm.KubeletCgroupsName,
				Resources: &configs.Resources{
					AllowAllDevices: &allowAllDevices,
				},
			},
		}
		cont.ensureStateFunc = func(_ *fs.Manager) error {
			return ensureProcessInContainerWithOOMScore(os.Getpid(), qos.KubeletOOMScoreAdj, &manager)
		}
		systemContainers = append(systemContainers, cont)
	} else {
		cm.periodicTasks = append(cm.periodicTasks, func() {
			if err := ensureProcessInContainerWithOOMScore(os.Getpid(), qos.KubeletOOMScoreAdj, nil); err != nil {
				klog.Error(err)
				return
			}
			cont, err := getContainer(os.Getpid())
			if err != nil {
				klog.Errorf("failed to find cgroups of kubelet - %v", err)
				return
			}
			cm.Lock()
			defer cm.Unlock()

			cm.KubeletCgroupsName = cont
		})
	}

	// cm.periodicTasks里可能有
	// 当运行时为docker，设置cm.RuntimeCgroupsName为docker的cgroup路径
	// 当cm.KubeletCgroupsName为空（没有配置kubelet cgroup），设置kubelet进程的oom_score_adj为-999

	// cm.systemContainers里可能有
	// cm.SystemCgroupsName: newSystemCgroups(cm.SystemCgroupsName), ensureStateFunc: 将非内核pid或非init进程的pid移动到cm.SystemCgroupsName的cgroup中
	// cm.kubeletCgroupsName: newSystemCgroups(cm.KubeletCgroupsName), ensureStateFunc: 设置kubelet进程的oom_score_adj为-999,将kubelet进程移动到kubeletCgroupsName的cgroup路径中
	cm.systemContainers = systemContainers
	return nil
}

// 从进程名或pid文件中查找进程pid，读取/proc/{pid}/cgroup，获得进程的cgroup路径，比如/system.slice/kubelet.service
func getContainerNameForProcess(name, pidFile string) (string, error) {
	// 获得pid从进程名或pid文件中
	pids, err := getPidsForProcess(name, pidFile)
	if err != nil {
		return "", fmt.Errorf("failed to detect process id for %q - %v", name, err)
	}
	if len(pids) == 0 {
		return "", nil
	}
	// 获得进程的cgroup路径，比如/system.slice/kubelet.service
	cont, err := getContainer(pids[0])
	if err != nil {
		return "", err
	}
	return cont, nil
}

func (cm *containerManagerImpl) GetNodeConfig() NodeConfig {
	cm.RLock()
	defer cm.RUnlock()
	return cm.NodeConfig
}

// GetPodCgroupRoot returns the literal cgroupfs value for the cgroup containing all pods.
func (cm *containerManagerImpl) GetPodCgroupRoot() string {
	return cm.cgroupManager.Name(cm.cgroupRoot)
}

func (cm *containerManagerImpl) GetMountedSubsystems() *CgroupSubsystems {
	return cm.subsystems
}

func (cm *containerManagerImpl) GetQOSContainersInfo() QOSContainersInfo {
	return cm.qosContainerManager.GetQOSContainersInfo()
}

// 根据所有active pod来统计Burstable和BestEffort的cgroup属性
// 设置Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup属性值
// cpu group设置cpu share值
// hugepage设置 hugepage.limit_in_bytes为int64最大值
// memory设置memory.limit_in_bytes
func (cm *containerManagerImpl) UpdateQOSCgroups() error {
	return cm.qosContainerManager.UpdateCgroups()
}

func (cm *containerManagerImpl) Status() Status {
	cm.RLock()
	defer cm.RUnlock()
	return cm.status
}

// 在pkg\kubelet\kubelet.go里的initializeRuntimeDependentModules会执行Start
// 启动cpu manager
//   1. 进行容器的垃圾回收，不存在的容器分配的cpu回收（在checkpoint和stateMemory中回收），containerMap列表清理（让容器的cpu分配状态里容器与activePods一致）
//   2. 创建checkpoint和stateMemory来保存当前的分配策略、分配状态
//   3. 如果cpu manager的policy为static policy会校验当前分配状态是否合法
//   3.1 创建一个goroutine周期性（默认10s）周期同步m.state中容器的cpuset到容器的cgroup设置，会同步activepods返回的pod的容器集合，清理不存在的容器的cpu分配
// 校验（cpu、内存、hugepage、ephemeral-storage）资源的保留值是否大于kubelet自身的能力
// 维护cgroup目录
//   1. 检查系统里cgroup是否开启"cpu"、"cpuacct", "cpuset", "memory"子系统，cpu子系统开启cfs
//   2. 设置一些关键的sysctl项
//   3. 确保各个cgroup子系统挂载目录下kubepods或kubepods.slice文件夹存在
//      设置各个cgroup系统（memory、pid、cpu share、hugepage）的属性值--这个值是根据cm.internalCapacity列表，各个资源类型的值减去SystemReserved和KubeReserved
//   4.启动qosmanager
//        确保Burstable和BestEffort的cgroup目录存在，Burstable目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/burstable，BestEffort目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/besteffort
//        启动一个gorutine 每一份钟更新Burstable和BestEffort的cgroup目录的cgroup资源属性
//   5. 应用--enforce-node-allocatable的配置，设置各个（cpu、memeory、pid、hugepage）cgroup的限制值
//      为pods，则设置在/sys/fs/cgroup/{cgroup sub system}/kubepods.slice，限制值为各个类型capacity减去SystemReserved和KubeReserved
//      为system-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename system-reserved-cgroup}，限制值为各个类型system-reserved值
//      为kube-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename kube-reserved-cgroup}，限制值为各个类型kube-reserved
// 执行周期性任务
//   1. 启动一个goroutine，每一分钟执行cm.systemContainers的ensureStateFunc
//      当cm.SystemCgroupsName不为空， cm.SystemCgroupsName: newSystemCgroups(cm.SystemCgroupsName), ensureStateFunc: 将非内核pid或非init进程的pid移动到cm.SystemCgroupsNamecgroup中
//      当cm.kubeletCgroupsName不为空，cm.kubeletCgroupsName: newSystemCgroups(cm.KubeletCgroupsName), ensureStateFunc: 设置kubelet进程的oom_score_adj为-999,将kubelet进程移动到kubeletCgroupsName的cgroup路径中
//   2. 启动一个goroutine，每5分钟执行cm.periodicTasks里的task
//     当运行时为docker，设置cm.RuntimeCgroupsName为docker的cgroup路径
//     当cm.KubeletCgroupsName为空（没有配置kubelet cgroup），设置kubelet进程的oom_score_adj为-999
// 启动device manager
//   1. 读取/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint文件，恢复状态
//   2. 移除/var/lib/kubelet/device-plugins/文件夹下非"kubelet_internal_checkpoint"文件
//   3. 启动grpc服务，监听socket为/var/lib/kubelet/device-plugins/kubelet.socket
func (cm *containerManagerImpl) Start(node *v1.Node,
	activePods ActivePodsFunc,
	sourcesReady config.SourcesReady,
	podStatusProvider status.PodStatusProvider,
	runtimeService internalapi.RuntimeService) error {

	// Initialize CPU manager
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.CPUManager) {
		// 获得所有runtime里的container，返回ContainerMap（容器id与对应的容器name和pod的uid）
		containerMap, err := buildContainerMapFromRuntime(runtimeService)
		if err != nil {
			return fmt.Errorf("failed to build map of initial containers from runtime: %v", err)
		}
		// 进行容器的垃圾回收，不存在的容器分配的cpu回收（在checkpoint和stateMemory中回收），containerMap列表清理（让容器的cpu分配状态里容器与activePods一致）
		// 创建checkpoint和stateMemory来保存当前的分配策略、分配状态
		// 如果cpu manager的policy为static policy会校验当前分配状态是否合法
		//   创建一个goroutine周期性（默认10s）周期同步m.state中容器的cpuset到容器的cgroup设置，会同步activepods返回的pod的容器集合，清理不存在的容器的cpu分配 
		err = cm.cpuManager.Start(cpumanager.ActivePodsFunc(activePods), sourcesReady, podStatusProvider, runtimeService, containerMap)
		if err != nil {
			return fmt.Errorf("start cpu manager error: %v", err)
		}
	}

	// cache the node Info including resource capacity and
	// allocatable of the node
	cm.nodeInfo = node

	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.LocalStorageCapacityIsolation) {
		// 返回kubelet root目录所在挂载设备的最近状态（磁盘设备名、状态的时间、磁盘大小、可用大小、使用量、label列表
		rootfs, err := cm.cadvisorInterface.RootFsInfo()
		if err != nil {
			return fmt.Errorf("failed to get rootfs info: %v", err)
		}
		// cm.capacity添加"ephemeral-storage"，rname为"ephemeral-storage"，rcap为磁盘大小
		for rName, rCap := range cadvisor.EphemeralStorageCapacityFromFsInfo(rootfs) {
			cm.capacity[rName] = rCap
		}
	}

	// Ensure that node allocatable configuration is valid.
	// 校验（cpu、内存、hugepage、ephemeral-storage）资源的保留值是否大于kubelet自身的能力
	if err := cm.validateNodeAllocatable(); err != nil {
		return err
	}

	// Setup the node
	// 1. 检查系统里cgroup是否开启"cpu"、"cpuacct", "cpuset", "memory"子系统，cpu子系统开启cfs
	// 2. 设置一些关键的sysctl项
	// 3. 确保各个cgroup子系统挂载目录下kubepods或kubepods.slice文件夹存在
	//    设置各个cgroup系统（memory、pid、cpu share、hugepage）的属性值--这个值是根据cm.internalCapacity列表，各个资源类型的值减去SystemReserved和KubeReserved
	//    启动qosmanager
	//      确保Burstable和BestEffort的cgroup目录存在，Burstable目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/burstable，BestEffort目录为/sys/fs/cgroup/{cgroup system}/kubepods.slice/besteffort
	//      启动一个gorutine 每一份钟更新Burstable和BestEffort的cgroup目录的cgroup资源属性
	// 4. 应用--enforce-node-allocatable的配置，设置各个（cpu、memeory、pid、hugepage）cgroup的限制值
	//    为pods，则设置在/sys/fs/cgroup/{cgroup sub system}/kubepods.slice，限制值为各个类型capacity减去SystemReserved和KubeReserved
	//    为system-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename system-reserved-cgroup}，限制值为各个类型system-reserved值
	//    为kube-reserved， 则设置在/sys/fs/cgroup/{cgroup sub system}/{basename kube-reserved-cgroup}，限制值为各个类型kube-reserved
	// 5. 设置cm.periodicTasks和cm.systemContainers
	//   cm.periodicTasks里可能有
	//     当运行时为docker，设置cm.RuntimeCgroupsName为docker的cgroup路径
	//     当cm.KubeletCgroupsName为空（没有配置kubelet cgroup），设置kubelet进程的oom_score_adj为-999
	//   cm.systemContainers里可能有
	//    当cm.SystemCgroupsName不为空， cm.SystemCgroupsName: newSystemCgroups(cm.SystemCgroupsName), ensureStateFunc: 将非内核pid或非init进程的pid移动到cm.SystemCgroupsNamecgroup中
	//    当cm.kubeletCgroupsName不为空，cm.kubeletCgroupsName: newSystemCgroups(cm.KubeletCgroupsName), ensureStateFunc: 设置kubelet进程的oom_score_adj为-999,将kubelet进程移动到kubeletCgroupsName的cgroup路径中
	if err := cm.setupNode(activePods); err != nil {
		return err
	}

	// Don't run a background thread if there are no ensureStateFuncs.
	hasEnsureStateFuncs := false
	for _, cont := range cm.systemContainers {
		if cont.ensureStateFunc != nil {
			hasEnsureStateFuncs = true
			break
		}
	}
	if hasEnsureStateFuncs {
		// Run ensure state functions every minute.
		// 启动一个goroutine，每一分钟执行cm.systemContainers的ensureStateFunc
		// 即会执行上面的cm.setupNode里添加到cm.systemContainers的systemContainer
		//    当cm.SystemCgroupsName不为空， cm.SystemCgroupsName: newSystemCgroups(cm.SystemCgroupsName), ensureStateFunc: 将非内核pid或非init进程的pid移动到cm.SystemCgroupsNamecgroup中
		//    当cm.kubeletCgroupsName不为空，cm.kubeletCgroupsName: newSystemCgroups(cm.KubeletCgroupsName), ensureStateFunc: 设置kubelet进程的oom_score_adj为-999,将kubelet进程移动到kubeletCgroupsName的cgroup路径中
		go wait.Until(func() {
			for _, cont := range cm.systemContainers {
				if cont.ensureStateFunc != nil {
					if err := cont.ensureStateFunc(cont.manager); err != nil {
						klog.Warningf("[ContainerManager] Failed to ensure state of %q: %v", cont.name, err)
					}
				}
			}
		}, time.Minute, wait.NeverStop)

	}

	// 启动一个goroutine，每5分钟执行cm.periodicTasks里的task
	// 即执行上面的cm.setupNode里设置cm.periodicTasks
	// 当运行时为docker，设置cm.RuntimeCgroupsName为docker的cgroup路径
	// 当cm.KubeletCgroupsName为空（没有配置kubelet cgroup），设置kubelet进程的oom_score_adj为-999
	if len(cm.periodicTasks) > 0 {
		go wait.Until(func() {
			for _, task := range cm.periodicTasks {
				if task != nil {
					task()
				}
			}
		}, 5*time.Minute, wait.NeverStop)
	}

	// Starts device manager.
	// 读取/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint文件，恢复状态
	// 移除/var/lib/kubelet/device-plugins/文件夹下非"kubelet_internal_checkpoint"文件
	// 启动grpc服务，监听socket为/var/lib/kubelet/device-plugins/kubelet.socket
	if err := cm.deviceManager.Start(devicemanager.ActivePodsFunc(activePods), sourcesReady); err != nil {
		return err
	}

	return nil
}

func (cm *containerManagerImpl) GetPluginRegistrationHandler() cache.PluginHandler {
	return cm.deviceManager.GetWatcherHandler()
}

// TODO: move the GetResources logic to PodContainerManager.
func (cm *containerManagerImpl) GetResources(pod *v1.Pod, container *v1.Container) (*kubecontainer.RunContainerOptions, error) {
	opts := &kubecontainer.RunContainerOptions{}
	// Allocate should already be called during predicateAdmitHandler.Admit(),
	// just try to fetch device runtime information from cached state here
	devOpts, err := cm.deviceManager.GetDeviceRunContainerOptions(pod, container)
	if err != nil {
		return nil, err
	} else if devOpts == nil {
		return opts, nil
	}
	opts.Devices = append(opts.Devices, devOpts.Devices...)
	opts.Mounts = append(opts.Mounts, devOpts.Mounts...)
	opts.Envs = append(opts.Envs, devOpts.Envs...)
	opts.Annotations = append(opts.Annotations, devOpts.Annotations...)
	return opts, nil
}

func (cm *containerManagerImpl) UpdatePluginResources(node *schedulernodeinfo.NodeInfo, attrs *lifecycle.PodAdmitAttributes) error {
	return cm.deviceManager.UpdatePluginResources(node, attrs)
}

func (cm *containerManagerImpl) GetAllocateResourcesPodAdmitHandler() lifecycle.PodAdmitHandler {
	if utilfeature.DefaultFeatureGate.Enabled(kubefeatures.TopologyManager) {
		return cm.topologyManager
	}
	// TODO: we need to think about a better way to do this. This will work for
	// now so long as we have only the cpuManager and deviceManager relying on
	// allocations here. However, going forward it is not generalized enough to
	// work as we add more and more hint providers that the TopologyManager
	// needs to call Allocate() on (that may not be directly intstantiated
	// inside this component).
	return &resourceAllocator{cm.cpuManager, cm.deviceManager}
}

type resourceAllocator struct {
	cpuManager    cpumanager.Manager
	deviceManager devicemanager.Manager
}

func (m *resourceAllocator) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	pod := attrs.Pod

	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		err := m.deviceManager.Allocate(pod, &container)
		if err != nil {
			return lifecycle.PodAdmitResult{
				Message: fmt.Sprintf("Allocate failed due to %v, which is unexpected", err),
				Reason:  "UnexpectedAdmissionError",
				Admit:   false,
			}
		}

		if m.cpuManager != nil {
			err = m.cpuManager.Allocate(pod, &container)
			if err != nil {
				return lifecycle.PodAdmitResult{
					Message: fmt.Sprintf("Allocate failed due to %v, which is unexpected", err),
					Reason:  "UnexpectedAdmissionError",
					Admit:   false,
				}
			}
		}
	}

	return lifecycle.PodAdmitResult{Admit: true}
}

func (cm *containerManagerImpl) SystemCgroupsLimit() v1.ResourceList {
	cpuLimit := int64(0)

	// Sum up resources of all external containers.
	for _, cont := range cm.systemContainers {
		cpuLimit += cont.cpuMillicores
	}

	return v1.ResourceList{
		v1.ResourceCPU: *resource.NewMilliQuantity(
			cpuLimit,
			resource.DecimalSI),
	}
}

// 获得所有runtime里的container，返回ContainerMap（容器id与对应的容器name和pod的uid）
func buildContainerMapFromRuntime(runtimeService internalapi.RuntimeService) (containermap.ContainerMap, error) {
	podSandboxMap := make(map[string]string)
	// 如果runtime是dockershim，过滤出Label["io.kubernetes.docker.type"]="podsandbox"的container，并从/var/lib/dockershim/sandbox获取这个容器的所属的pod namespace和pod name
	podSandboxList, _ := runtimeService.ListPodSandbox(nil)
	for _, p := range podSandboxList {
		podSandboxMap[p.Id] = p.Metadata.Uid
	}

	containerMap := containermap.NewContainerMap()
	// 如果runtime是dockershim，过滤出Label["io.kubernetes.docker.type"]=="container"的container
	containerList, _ := runtimeService.ListContainers(nil)
	for _, c := range containerList {
		if _, exists := podSandboxMap[c.PodSandboxId]; !exists {
			return nil, fmt.Errorf("no PodsandBox found with Id '%s'", c.PodSandboxId)
		}
		containerMap.Add(podSandboxMap[c.PodSandboxId], c.Metadata.Name, c.Id)
	}

	return containerMap, nil
}

func isProcessRunningInHost(pid int) (bool, error) {
	// Get init pid namespace.
	initPidNs, err := os.Readlink("/proc/1/ns/pid")
	if err != nil {
		return false, fmt.Errorf("failed to find pid namespace of init process")
	}
	klog.V(10).Infof("init pid ns is %q", initPidNs)
	processPidNs, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		return false, fmt.Errorf("failed to find pid namespace of process %q", pid)
	}
	klog.V(10).Infof("Pid %d pid ns is %q", pid, processPidNs)
	return initPidNs == processPidNs, nil
}

func getPidFromPidFile(pidFile string) (int, error) {
	file, err := os.Open(pidFile)
	if err != nil {
		return 0, fmt.Errorf("error opening pid file %s: %v", pidFile, err)
	}
	defer file.Close()

	data, err := utilio.ReadAtMost(file, maxPidFileLength)
	if err != nil {
		return 0, fmt.Errorf("error reading pid file %s: %v", pidFile, err)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("error parsing %s as a number: %v", string(data), err)
	}

	return pid, nil
}

func getPidsForProcess(name, pidFile string) ([]int, error) {
	if len(pidFile) == 0 {
		// 通过命令来查找pid
		return procfs.PidOf(name)
	}

	pid, err := getPidFromPidFile(pidFile)
	if err == nil {
		return []int{pid}, nil
	}

	// Try to lookup pid by process name
	// 如果找不到进程，返回pids为空，error为nil
	pids, err2 := procfs.PidOf(name)
	if err2 == nil {
		return pids, nil
	}

	// Return error from getPidFromPidFile since that should have worked
	// and is the real source of the problem.
	klog.V(4).Infof("unable to get pid from %s: %v", pidFile, err)
	return []int{}, err
}

// Ensures that the Docker daemon is in the desired container.
// Temporarily export the function to be used by dockershim.
// TODO(yujuhong): Move this function to dockershim once kubelet migrates to
// dockershim as the default.
func EnsureDockerInContainer(dockerAPIVersion *utilversion.Version, oomScoreAdj int, manager *fs.Manager) error {
	type process struct{ name, file string }
	dockerProcs := []process{{dockerProcessName, dockerPidFile}}
	if dockerAPIVersion.AtLeast(containerdAPIVersion) {
		dockerProcs = append(dockerProcs, process{containerdProcessName, containerdPidFile})
	}
	var errs []error
	for _, proc := range dockerProcs {
		pids, err := getPidsForProcess(proc.name, proc.file)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get pids for %q: %v", proc.name, err))
			continue
		}

		// Move if the pid is not already in the desired container.
		for _, pid := range pids {
			// 如果manager不为空，则会将dockerd进程移动到manager.Cgroups.Name为路径的cgroup，否则跳过
			// 然后设置dockerd的oom_score_adj为-999
			if err := ensureProcessInContainerWithOOMScore(pid, oomScoreAdj, manager); err != nil {
				errs = append(errs, fmt.Errorf("errors moving %q pid: %v", proc.name, err))
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}

// 如果manager不为空，则会将kubelet进程移动到manager.Cgroups.Name为路径的cgroup，否则跳过
// 然后设置kubelet的oom_score_adj为-999
// 如果manager不为空，则会将dockerd进程移动到manager.Cgroups.Name为路径的cgroup，否则跳过
// 然后设置dockerd的oom_score_adj为-999
func ensureProcessInContainerWithOOMScore(pid int, oomScoreAdj int, manager *fs.Manager) error {
	if runningInHost, err := isProcessRunningInHost(pid); err != nil {
		// Err on the side of caution. Avoid moving the docker daemon unless we are able to identify its context.
		return err
	} else if !runningInHost {
		// Process is running inside a container. Don't touch that.
		klog.V(2).Infof("pid %d is not running in the host namespaces", pid)
		return nil
	}

	var errs []error
	if manager != nil {
		cont, err := getContainer(pid)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to find container of PID %d: %v", pid, err))
		}

		if cont != manager.Cgroups.Name {
			err = manager.Apply(pid)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to move PID %d (in %q) to %q: %v", pid, cont, manager.Cgroups.Name, err))
			}
		}
	}

	// Also apply oom-score-adj to processes
	oomAdjuster := oom.NewOOMAdjuster()
	klog.V(5).Infof("attempting to apply oom_score_adj of %d to pid %d", oomScoreAdj, pid)
	if err := oomAdjuster.ApplyOOMScoreAdj(pid, oomScoreAdj); err != nil {
		klog.V(3).Infof("Failed to apply oom_score_adj %d for pid %d: %v", oomScoreAdj, pid, err)
		errs = append(errs, fmt.Errorf("failed to apply oom score %d to PID %d: %v", oomScoreAdj, pid, err))
	}
	return utilerrors.NewAggregate(errs)
}

// getContainer returns the cgroup associated with the specified pid.
// It enforces a unified hierarchy for memory and cpu cgroups.
// On systemd environments, it uses the name=systemd cgroup for the specified pid.
// 获得进程的cgroup路径，比如/system.slice/kubelet.service
func getContainer(pid int) (string, error) {
	cgs, err := cgroups.ParseCgroupFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}

	cpu, found := cgs["cpu"]
	if !found {
		return "", cgroups.NewNotFoundError("cpu")
	}
	memory, found := cgs["memory"]
	if !found {
		return "", cgroups.NewNotFoundError("memory")
	}

	// since we use this container for accounting, we need to ensure its a unified hierarchy.
	if cpu != memory {
		return "", fmt.Errorf("cpu and memory cgroup hierarchy not unified.  cpu: %s, memory: %s", cpu, memory)
	}

	// on systemd, every pid is in a unified cgroup hierarchy (name=systemd as seen in systemd-cgls)
	// cpu and memory accounting is off by default, users may choose to enable it per unit or globally.
	// users could enable CPU and memory accounting globally via /etc/systemd/system.conf (DefaultCPUAccounting=true DefaultMemoryAccounting=true).
	// users could also enable CPU and memory accounting per unit via CPUAccounting=true and MemoryAccounting=true
	// we only warn if accounting is not enabled for CPU or memory so as to not break local development flows where kubelet is launched in a terminal.
	// for example, the cgroup for the user session will be something like /user.slice/user-X.slice/session-X.scope, but the cpu and memory
	// cgroup will be the closest ancestor where accounting is performed (most likely /) on systems that launch docker containers.
	// as a result, on those systems, you will not get cpu or memory accounting statistics for kubelet.
	// in addition, you would not get memory or cpu accounting for the runtime unless accounting was enabled on its unit (or globally).
	if systemd, found := cgs["name=systemd"]; found {
		if systemd != cpu {
			klog.Warningf("CPUAccounting not enabled for pid: %d", pid)
		}
		if systemd != memory {
			klog.Warningf("MemoryAccounting not enabled for pid: %d", pid)
		}
		return systemd, nil
	}

	return cpu, nil
}

// Ensures the system container is created and all non-kernel threads and process 1
// without a container are moved to it.
//
// The reason of leaving kernel threads at root cgroup is that we don't want to tie the
// execution of these threads with to-be defined /system quota and create priority inversions.
//
// 将非内核pid或非init进程的pid移动到指定的cgroup中
func ensureSystemCgroups(rootCgroupPath string, manager *fs.Manager) error {
	// Move non-kernel PIDs to the system container.
	// Only keep errors on latest attempt.
	var finalErr error
	for i := 0; i <= 10; i++ {
		allPids, err := cmutil.GetPids(rootCgroupPath)
		if err != nil {
			finalErr = fmt.Errorf("failed to list PIDs for root: %v", err)
			continue
		}

		// Remove kernel pids and other protected PIDs (pid 1, PIDs already in system & kubelet containers)
		pids := make([]int, 0, len(allPids))
		for _, pid := range allPids {
			if pid == 1 || isKernelPid(pid) {
				continue
			}

			pids = append(pids, pid)
		}
		klog.Infof("Found %d PIDs in root, %d of them are not to be moved", len(allPids), len(allPids)-len(pids))

		// Check if we have moved all the non-kernel PIDs.
		if len(pids) == 0 {
			return nil
		}

		klog.Infof("Moving non-kernel processes: %v", pids)
		for _, pid := range pids {
			err := manager.Apply(pid)
			if err != nil {
				finalErr = fmt.Errorf("failed to move PID %d into the system container %q: %v", pid, manager.Cgroups.Name, err)
			}
		}

	}

	return finalErr
}

// Determines whether the specified PID is a kernel PID.
func isKernelPid(pid int) bool {
	// Kernel threads have no associated executable.
	_, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return err != nil && os.IsNotExist(err)
}

func (cm *containerManagerImpl) GetCapacity() v1.ResourceList {
	return cm.capacity
}

func (cm *containerManagerImpl) GetDevicePluginResourceCapacity() (v1.ResourceList, v1.ResourceList, []string) {
	return cm.deviceManager.GetCapacity()
}

func (cm *containerManagerImpl) GetDevices(podUID, containerName string) []*podresourcesapi.ContainerDevices {
	return cm.deviceManager.GetDevices(podUID, containerName)
}

func (cm *containerManagerImpl) ShouldResetExtendedResourceCapacity() bool {
	return cm.deviceManager.ShouldResetExtendedResourceCapacity()
}

func (cm *containerManagerImpl) UpdateAllocatedDevices() {
	cm.deviceManager.UpdateAllocatedDevices()
}
