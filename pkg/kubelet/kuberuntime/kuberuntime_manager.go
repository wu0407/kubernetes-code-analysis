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

package kuberuntime

import (
	"errors"
	"fmt"
	"os"
	goruntime "runtime"
	"time"

	cadvisorapi "github.com/google/cadvisor/info/v1"
	"k8s.io/klog"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilversion "k8s.io/apimachinery/pkg/util/version"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/flowcontrol"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	proberesults "k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/runtimeclass"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/cache"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/kubelet/util/logreduction"
)

const (
	// The api version of kubelet runtime api
	kubeRuntimeAPIVersion = "0.1.0"
	// The root directory for pod logs
	podLogsRootDirectory = "/var/log/pods"
	// A minimal shutdown window for avoiding unnecessary SIGKILLs
	minimumGracePeriodInSeconds = 2

	// The expiration time of version cache.
	versionCacheTTL = 60 * time.Second
	// How frequently to report identical errors
	identicalErrorDelay = 1 * time.Minute
)

var (
	// ErrVersionNotSupported is returned when the api version of runtime interface is not supported
	ErrVersionNotSupported = errors.New("runtime api version is not supported")
)

// podStateProvider can determine if a pod is deleted ir terminated
type podStateProvider interface {
	IsPodDeleted(kubetypes.UID) bool
	IsPodTerminated(kubetypes.UID) bool
}

type kubeGenericRuntimeManager struct {
	runtimeName         string
	recorder            record.EventRecorder
	osInterface         kubecontainer.OSInterface
	containerRefManager *kubecontainer.RefManager

	// machineInfo contains the machine information.
	machineInfo *cadvisorapi.MachineInfo

	// Container GC manager
	containerGC *containerGC

	// Keyring for pulling images
	keyring credentialprovider.DockerKeyring

	// Runner of lifecycle events.
	runner kubecontainer.HandlerRunner

	// RuntimeHelper that wraps kubelet to generate runtime container options.
	runtimeHelper kubecontainer.RuntimeHelper

	// Health check results.
	livenessManager proberesults.Manager
	startupManager  proberesults.Manager

	// If true, enforce container cpu limits with CFS quota support
	cpuCFSQuota bool

	// CPUCFSQuotaPeriod sets the CPU CFS quota period value, cpu.cfs_period_us, defaults to 100ms
	cpuCFSQuotaPeriod metav1.Duration

	// wrapped image puller.
	imagePuller images.ImageManager

	// gRPC service clients
	runtimeService internalapi.RuntimeService
	imageService   internalapi.ImageManagerService

	// The version cache of runtime daemon.
	versionCache *cache.ObjectCache

	// The directory path for seccomp profiles.
	seccompProfileRoot string

	// Internal lifecycle event handlers for container resource management.
	internalLifecycle cm.InternalContainerLifecycle

	// A shim to legacy functions for backward compatibility.
	legacyLogProvider LegacyLogProvider

	// Manage RuntimeClass resources.
	runtimeClassManager *runtimeclass.Manager

	// Cache last per-container error message to reduce log spam
	logReduction *logreduction.LogReduction
}

// KubeGenericRuntime is a interface contains interfaces for container runtime and command.
type KubeGenericRuntime interface {
	kubecontainer.Runtime
	kubecontainer.StreamingRuntime
	kubecontainer.ContainerCommandRunner
}

// LegacyLogProvider gives the ability to use unsupported docker log drivers (e.g. journald)
type LegacyLogProvider interface {
	// Get the last few lines of the logs for a specific container.
	GetContainerLogTail(uid kubetypes.UID, name, namespace string, containerID kubecontainer.ContainerID) (string, error)
}

// NewKubeGenericRuntimeManager creates a new kubeGenericRuntimeManager
func NewKubeGenericRuntimeManager(
	recorder record.EventRecorder,
	livenessManager proberesults.Manager,
	startupManager proberesults.Manager,
	seccompProfileRoot string,
	containerRefManager *kubecontainer.RefManager,
	machineInfo *cadvisorapi.MachineInfo,
	podStateProvider podStateProvider,
	osInterface kubecontainer.OSInterface,
	runtimeHelper kubecontainer.RuntimeHelper,
	httpClient types.HTTPGetter,
	imageBackOff *flowcontrol.Backoff,
	serializeImagePulls bool,
	imagePullQPS float32,
	imagePullBurst int,
	cpuCFSQuota bool,
	cpuCFSQuotaPeriod metav1.Duration,
	runtimeService internalapi.RuntimeService,
	imageService internalapi.ImageManagerService,
	internalLifecycle cm.InternalContainerLifecycle,
	legacyLogProvider LegacyLogProvider,
	runtimeClassManager *runtimeclass.Manager,
) (KubeGenericRuntime, error) {
	kubeRuntimeManager := &kubeGenericRuntimeManager{
		recorder:            recorder,
		cpuCFSQuota:         cpuCFSQuota,
		cpuCFSQuotaPeriod:   cpuCFSQuotaPeriod,
		seccompProfileRoot:  seccompProfileRoot,
		livenessManager:     livenessManager,
		startupManager:      startupManager,
		containerRefManager: containerRefManager,
		machineInfo:         machineInfo,
		osInterface:         osInterface,
		runtimeHelper:       runtimeHelper,
		runtimeService:      newInstrumentedRuntimeService(runtimeService),
		imageService:        newInstrumentedImageManagerService(imageService),
		// 返回注册且启用的dockerConfigProvider，credentialprovider.defaultDockerConfigProvider一直启用
		keyring:             credentialprovider.NewDockerKeyring(),
		internalLifecycle:   internalLifecycle,
		legacyLogProvider:   legacyLogProvider,
		runtimeClassManager: runtimeClassManager,
		logReduction:        logreduction.NewLogReduction(identicalErrorDelay),
	}

	//cri grpc server version info
	typedVersion, err := kubeRuntimeManager.getTypedVersion()
	if err != nil {
		klog.Errorf("Get runtime version failed: %v", err)
		return nil, err
	}

	// Only matching kubeRuntimeAPIVersion is supported now
	// TODO: Runtime API machinery is under discussion at https://github.com/kubernetes/kubernetes/issues/28642
	if typedVersion.Version != kubeRuntimeAPIVersion {
		klog.Errorf("Runtime api version %s is not supported, only %s is supported now",
			typedVersion.Version,
			kubeRuntimeAPIVersion)
		return nil, ErrVersionNotSupported
	}

	// 运行时是docker，typedVersion.RuntimeName是docker，typedVersion.RuntimeVersion是docker版本（比如19.03.15），typedVersion.RuntimeApiVersion是docker api版本（比如1.40.0）
	kubeRuntimeManager.runtimeName = typedVersion.RuntimeName
	klog.Infof("Container runtime %s initialized, version: %s, apiVersion: %s",
		typedVersion.RuntimeName,
		typedVersion.RuntimeVersion,
		typedVersion.RuntimeApiVersion)

	// If the container logs directory does not exist, create it.
	// TODO: create podLogsRootDirectory at kubelet.go when kubelet is refactored to
	// new runtime interface
	// /var/log/pods不存在，则创建目录，权限为0755
	if _, err := osInterface.Stat(podLogsRootDirectory); os.IsNotExist(err) {
		if err := osInterface.MkdirAll(podLogsRootDirectory, 0755); err != nil {
			klog.Errorf("Failed to create directory %q: %v", podLogsRootDirectory, err)
		}
	}

	kubeRuntimeManager.imagePuller = images.NewImageManager(
		kubecontainer.FilterEventRecorder(recorder),
		kubeRuntimeManager,
		imageBackOff,
		serializeImagePulls,
		imagePullQPS,
		imagePullBurst)
	kubeRuntimeManager.runner = lifecycle.NewHandlerRunner(httpClient, kubeRuntimeManager, kubeRuntimeManager)
	kubeRuntimeManager.containerGC = newContainerGC(runtimeService, podStateProvider, kubeRuntimeManager)

	kubeRuntimeManager.versionCache = cache.NewObjectCache(
		func() (interface{}, error) {
			return kubeRuntimeManager.getTypedVersion()
		},
		versionCacheTTL,
	)

	return kubeRuntimeManager, nil
}

// Type returns the type of the container runtime.
func (m *kubeGenericRuntimeManager) Type() string {
	return m.runtimeName
}

// SupportsSingleFileMapping returns whether the container runtime supports single file mappings or not.
// It is supported on Windows only if the container runtime is containerd.
// 非windows系统都返回true，windows系统runtime不为"docker"返回true
func (m *kubeGenericRuntimeManager) SupportsSingleFileMapping() bool {
	switch goruntime.GOOS {
	case "windows":
		return m.Type() != types.DockerContainerRuntime
	default:
		return true
	}
}

// 将字符串version转换成utilversion.Version
func newRuntimeVersion(version string) (*utilversion.Version, error) {
	if ver, err := utilversion.ParseSemantic(version); err == nil {
		return ver, err
	}
	return utilversion.ParseGeneric(version)
}

// 返回runtime版本
func (m *kubeGenericRuntimeManager) getTypedVersion() (*runtimeapi.VersionResponse, error) {
	// 如果运行时是dockershim，则返回 Version协议版本"0.1.0"、RuntimeName为"docker"、RuntimeVersion为docker版本、RuntimeApiVersion为docker api版本
	typedVersion, err := m.runtimeService.Version(kubeRuntimeAPIVersion)
	if err != nil {
		return nil, fmt.Errorf("get remote runtime typed version failed: %v", err)
	}
	return typedVersion, nil
}

// Version returns the version information of the container runtime.
func (m *kubeGenericRuntimeManager) Version() (kubecontainer.Version, error) {
	typedVersion, err := m.getTypedVersion()
	if err != nil {
		return nil, err
	}

	return newRuntimeVersion(typedVersion.RuntimeVersion)
}

// APIVersion returns the cached API version information of the container
// runtime. Implementation is expected to update this cache periodically.
// This may be different from the runtime engine's version.
// 获取runtime的RuntimeApiVersion
func (m *kubeGenericRuntimeManager) APIVersion() (kubecontainer.Version, error) {
	// 先从m.versionCache中获取，取到且未过期就返回，否则执行c.updater获取数据，然后放入m.versionCache中
	versionObject, err := m.versionCache.Get(m.machineInfo.MachineID)
	if err != nil {
		return nil, err
	}
	typedVersion := versionObject.(*runtimeapi.VersionResponse)

	// 将字符串version转换成utilversion.Version
	return newRuntimeVersion(typedVersion.RuntimeApiVersion)
}

// Status returns the status of the runtime. An error is returned if the Status
// function itself fails, nil otherwise.
func (m *kubeGenericRuntimeManager) Status() (*kubecontainer.RuntimeStatus, error) {
	status, err := m.runtimeService.Status()
	if err != nil {
		return nil, err
	}
	return toKubeRuntimeStatus(status), nil
}

// GetPods returns a list of containers grouped by pods. The boolean parameter
// specifies whether the runtime returns all containers including those already
// exited and dead containers (used for garbage collection).
// 获得所有pod的容器列表
func (m *kubeGenericRuntimeManager) GetPods(all bool) ([]*kubecontainer.Pod, error) {
	pods := make(map[kubetypes.UID]*kubecontainer.Pod)
	// 获得所有的sandbox容器
	sandboxes, err := m.getKubeletSandboxes(all)
	if err != nil {
		return nil, err
	}
	for i := range sandboxes {
		s := sandboxes[i]
		if s.Metadata == nil {
			klog.V(4).Infof("Sandbox does not have metadata: %+v", s)
			continue
		}
		podUID := kubetypes.UID(s.Metadata.Uid)
		if _, ok := pods[podUID]; !ok {
			pods[podUID] = &kubecontainer.Pod{
				ID:        podUID,
				Name:      s.Metadata.Name,
				Namespace: s.Metadata.Namespace,
			}
		}
		p := pods[podUID]
		converted, err := m.sandboxToKubeContainer(s)
		if err != nil {
			klog.V(4).Infof("Convert %q sandbox %v of pod %q failed: %v", m.runtimeName, s, podUID, err)
			continue
		}
		p.Sandboxes = append(p.Sandboxes, converted)
	}

	// 获得所有非sandbox容器
	containers, err := m.getKubeletContainers(all)
	if err != nil {
		return nil, err
	}
	for i := range containers {
		c := containers[i]
		if c.Metadata == nil {
			klog.V(4).Infof("Container does not have metadata: %+v", c)
			continue
		}

		// 从Label中获取pod name、namespace、pod UID、container name
		labelledInfo := getContainerInfoFromLabels(c.Labels)
		pod, found := pods[labelledInfo.PodUID]
		if !found {
			pod = &kubecontainer.Pod{
				ID:        labelledInfo.PodUID,
				Name:      labelledInfo.PodName,
				Namespace: labelledInfo.PodNamespace,
			}
			pods[labelledInfo.PodUID] = pod
		}

		converted, err := m.toKubeContainer(c)
		if err != nil {
			klog.V(4).Infof("Convert %s container %v of pod %q failed: %v", m.runtimeName, c, labelledInfo.PodUID, err)
			continue
		}

		pod.Containers = append(pod.Containers, converted)
	}

	// Convert map to list.
	var result []*kubecontainer.Pod
	for _, pod := range pods {
		result = append(result, pod)
	}

	return result, nil
}

// containerToKillInfo contains necessary information to kill a container.
type containerToKillInfo struct {
	// The spec of the container.
	container *v1.Container
	// The name of the container.
	name string
	// The message indicates why the container will be killed.
	message string
}

// podActions keeps information what to do for a pod.
type podActions struct {
	// Stop all running (regular, init and ephemeral) containers and the sandbox for the pod.
	KillPod bool
	// Whether need to create a new sandbox. If needed to kill pod and create
	// a new pod sandbox, all init containers need to be purged (i.e., removed).
	CreateSandbox bool
	// The id of existing sandbox. It is used for starting containers in ContainersToStart.
	SandboxID string
	// The attempt number of creating sandboxes for the pod.
	Attempt uint32

	// The next init container to start.
	NextInitContainerToStart *v1.Container
	// ContainersToStart keeps a list of indexes for the containers to start,
	// where the index is the index of the specific container in the pod spec (
	// pod.Spec.Containers.
	ContainersToStart []int
	// ContainersToKill keeps a map of containers that need to be killed, note that
	// the key is the container ID of the container, while
	// the value contains necessary information to kill a container.
	ContainersToKill map[kubecontainer.ContainerID]containerToKillInfo
	// EphemeralContainersToStart is a list of indexes for the ephemeral containers to start,
	// where the index is the index of the specific container in pod.Spec.EphemeralContainers.
	EphemeralContainersToStart []int
}

// podSandboxChanged checks whether the spec of the pod is changed and returns
// (changed, new attempt, original sandboxID if exist).
// 检查sanbox是否发生变化（是否ready，网络模式一致、是否有ip），返回sandbox是否改变、新的attempt重启次数和sanbox id
func (m *kubeGenericRuntimeManager) podSandboxChanged(pod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, uint32, string) {
	// podStatus里没有SandboxStatuses，说明需要创建一个新的
	if len(podStatus.SandboxStatuses) == 0 {
		klog.V(2).Infof("No sandbox for pod %q can be found. Need to start a new one", format.Pod(pod))
		return true, 0, ""
	}

	readySandboxCount := 0
	for _, s := range podStatus.SandboxStatuses {
		if s.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			readySandboxCount++
		}
	}

	// Needs to create a new sandbox when readySandboxCount > 1 or the ready sandbox is not the latest one.
	sandboxStatus := podStatus.SandboxStatuses[0]
	// 多个sandbox都处于ready状态，返回true，Attempt + 1，第一个sandbox id
	if readySandboxCount > 1 {
		klog.V(2).Infof("Multiple sandboxes are ready for Pod %q. Need to reconcile them", format.Pod(pod))
		return true, sandboxStatus.Metadata.Attempt + 1, sandboxStatus.Id
	}
	// 唯一一个sandbox的状态不为ready状态，Attempt + 1，返回true
	if sandboxStatus.State != runtimeapi.PodSandboxState_SANDBOX_READY {
		klog.V(2).Infof("No ready sandbox for pod %q can be found. Need to start a new one", format.Pod(pod))
		return true, sandboxStatus.Metadata.Attempt + 1, sandboxStatus.Id
	}

	// Needs to create a new sandbox when network namespace changed.
	// 唯一一个sandbox的状态为ready状态，但是在linux系统中网络命名空间跟pod的网络模式不一样，返回true，空的sanbox id
	if sandboxStatus.GetLinux().GetNamespaces().GetOptions().GetNetwork() != networkNamespaceForPod(pod) {
		klog.V(2).Infof("Sandbox for pod %q has changed. Need to start a new one", format.Pod(pod))
		return true, sandboxStatus.Metadata.Attempt + 1, ""
	}

	// Needs to create a new sandbox when the sandbox does not have an IP address.
	// pod网络模式不是host模式且没有ip
	if !kubecontainer.IsHostNetworkPod(pod) && sandboxStatus.Network.Ip == "" {
		klog.V(2).Infof("Sandbox for pod %q has no IP address. Need to start a new one", format.Pod(pod))
		return true, sandboxStatus.Metadata.Attempt + 1, sandboxStatus.Id
	}

	return false, sandboxStatus.Metadata.Attempt, sandboxStatus.Id
}

// 返回期望的hash值和container runtime status里hash值和两个值是否相等
func containerChanged(container *v1.Container, containerStatus *kubecontainer.ContainerStatus) (uint64, uint64, bool) {
	// 计算container hash值
	expectedHash := kubecontainer.HashContainer(container)
	// 如果runtime是dockershim，则containerStatus.Hash值是docker container里的Labels["annotation.io.kubernetes.container.hash"]的值
	return expectedHash, containerStatus.Hash, containerStatus.Hash != expectedHash
}

func shouldRestartOnFailure(pod *v1.Pod) bool {
	return pod.Spec.RestartPolicy != v1.RestartPolicyNever
}

// container是否为Succeeded（不在运行中且exitcode为0）
func containerSucceeded(c *v1.Container, podStatus *kubecontainer.PodStatus) bool {
	// 从podStatus.ContainerStatuses里，返回第一个名为c.Name的ContainerStatus
	cStatus := podStatus.FindContainerStatusByName(c.Name)
	if cStatus == nil || cStatus.State == kubecontainer.ContainerStateRunning {
		return false
	}
	return cStatus.ExitCode == 0
}

// computePodActions checks whether the pod spec has changed and returns the changes if true.
// 根基runtime种container的状态，计算出需要采取的动作
func (m *kubeGenericRuntimeManager) computePodActions(pod *v1.Pod, podStatus *kubecontainer.PodStatus) podActions {
	// format.Pod(pod)输出 "{pod.Name}_{pod.Namespace}_({pod.UID})"
	klog.V(5).Infof("Syncing Pod %q: %+v", format.Pod(pod), pod)

	// 检查sanbox是否发生变化（是否ready，网络模式一致、是否有ip），返回sandbox是否改变、新的attempt sandbox重启次数和sanbox id
	createPodSandbox, attempt, sandboxID := m.podSandboxChanged(pod, podStatus)
	changes := podActions{
		KillPod:           createPodSandbox,
		CreateSandbox:     createPodSandbox,
		SandboxID:         sandboxID,
		Attempt:           attempt,
		ContainersToStart: []int{},
		ContainersToKill:  make(map[kubecontainer.ContainerID]containerToKillInfo),
	}

	// sandbox需要被创建的情况下，需要处理三种情况
	// 第一种. pod的restart policy为"Never，且sandbox重启次数不为0，且pod里有已经创建的container，则不需要创建sandbox，只需要KillPod（停止所有容器包括sandbox）
	// 第二种. 如果有initcontainer，则重新执行init container
	// 第三种，如果没有initcontainer，则重新启动所有普通container（除了container为Succeeded（不在运行中且exitcode为0）且pod的重启策略为"OnFailure"）

	// If we need to (re-)create the pod sandbox, everything will need to be
	// killed and recreated, and init containers should be purged.
	if createPodSandbox {
		// pod的restart policy为"Never"，且sandbox重启次数不为0，且pod里有已经创建的container，则设置CreateSandbox为false（意味着不需要重新创建sandbox）
		if !shouldRestartOnFailure(pod) && attempt != 0 && len(podStatus.ContainerStatuses) != 0 {
			// Should not restart the pod, just return.
			// we should not create a sandbox for a pod if it is already done.
			// if all containers are done and should not be started, there is no need to create a new sandbox.
			// this stops confusing logs on pods whose containers all have exit codes, but we recreate a sandbox before terminating it.
			//
			// If ContainerStatuses is empty, we assume that we've never
			// successfully created any containers. In this case, we should
			// retry creating the sandbox.
			changes.CreateSandbox = false
			return changes
		}
		// 如果有init container则设置changes.NextInitContainerToStart为第一个initcontainer，并返回
		if len(pod.Spec.InitContainers) != 0 {
			// Pod has init containers, return the first one.
			changes.NextInitContainerToStart = &pod.Spec.InitContainers[0]
			return changes
		}
		// Start all containers by default but exclude the ones that succeeded if
		// RestartPolicy is OnFailure.
		// 设置需要启动普通的container（container为Succeeded（不在运行中且exitcode为0）且pod的重启策略为"OnFailure"不需要启动）
		for idx, c := range pod.Spec.Containers {
			// container为Succeeded（不在运行中且exitcode为0）且pod的重启策略为"OnFailure"，则跳过（不设置在ContainersToStart列表，不需要启动这个container）
			if containerSucceeded(&c, podStatus) && pod.Spec.RestartPolicy == v1.RestartPolicyOnFailure {
				continue
			}
			changes.ContainersToStart = append(changes.ContainersToStart, idx)
		}
		return changes
	}

	// 不需要重新创建sandbox的情况
	// 对于Ephemeral containers，只启动未启动的Ephemeral Container
	// init container是执行不成功且pod的restart policy为"Never"，则是设置KillPod为true（需要停止所有容器）
	// 执行失败的init container处于"unknown"状态，添加这个container到changes.ContainersToKill（容器需要停止）
	// 设置最后执行失败init容器到changes.NextInitContainerToStart
	// 如果initcontainer都未执行，则设置第一个initcontainer到changes.NextInitContainerToStart
	// 有initcontainer未执行完成或失败，不需要检查普通container，直接返回

	// Ephemeral containers may be started even if initialization is not yet complete.
	// 只启动未启动的Ephemeral Container
	if utilfeature.DefaultFeatureGate.Enabled(features.EphemeralContainers) {
		for i := range pod.Spec.EphemeralContainers {
			c := (*v1.Container)(&pod.Spec.EphemeralContainers[i].EphemeralContainerCommon)

			// Ephemeral Containers are never restarted
			// 只启动未启动的Ephemeral Container
			if podStatus.FindContainerStatusByName(c.Name) == nil {
				changes.EphemeralContainersToStart = append(changes.EphemeralContainersToStart, i)
			}
		}
	}

	// Check initialization progress.
	// 返回pod中最后一个失败（或未执行）的init container状态，和最后一个失败（或未执行）的init container，和是否所有init container都执行成功
	initLastStatus, next, done := findNextInitContainerToRun(pod, podStatus)
	// 有的init container执行不成功或未执行，则直接返回（不检查普通container）
	if !done {
		// 这个init container不为nil（init container不处于"running"状态或"exited"状态）
		if next != nil {
			initFailed := initLastStatus != nil && isInitContainerFailed(initLastStatus)
			// init container是执行不成功且pod的restart policy为"Never"，则是设置KillPod为true（需要停止所有容器）
			if initFailed && !shouldRestartOnFailure(pod) {
				changes.KillPod = true
			} else {
				// Always try to stop containers in unknown state first.
				// 执行失败的init container处于"unknown"状态，添加这个container到changes.ContainersToKill（容器需要停止）
				if initLastStatus != nil && initLastStatus.State == kubecontainer.ContainerStateUnknown {
					changes.ContainersToKill[initLastStatus.ID] = containerToKillInfo{
						name:      next.Name,
						container: next,
						message: fmt.Sprintf("Init container is in %q state, try killing it before restart",
							initLastStatus.State),
					}
				}
				// 无论是未执行还是执行失败，都设置这个失败容器到changes.NextInitContainerToStart
				changes.NextInitContainerToStart = next
			}
		}
		// Initialization failed or still in progress. Skip inspecting non-init
		// containers.
		// 不是所有init container都执行完成，则直接返回
		return changes
	}

	// 检查普通container情况

	// container不处于running状态
	// 1. 先释放已经启动但是不处于"running"状态container分配资源
	// 2. container应该被重启（container未启动或状态为"unknown"或"created"，或"exited"且exitcode不为0且restart policy不为"Never"），则将container添加到changes。ContainersToStart列表里。
	// 3. container已经启动且container处于"unknown"状态，则添加container到changes.ContainersToKill（容器需要停止）

	// container处于running状态
	// 以下三种情况，需要被停止（添加container到changes.ContainersToKill）。在restart policy不为"Never"，需要重启（添加container到changes.ContainersToStart）
	//   1. 期望的hash值和container runtime status里hash值不相等
	//   2. liveness probe结果是失败
	//   3. startup probe结果是失败
	// 如果普通的container都未启动，或都需要重启，则设置changes.KillPod为true（需要停止所有container包括sandbox）

	// Number of running containers to keep.
	keepCount := 0
	// check the status of containers.
	for idx, container := range pod.Spec.Containers {
		// 从podStatus.ContainerStatuses里，返回第一个名为containerName的ContainerStatus
		containerStatus := podStatus.FindContainerStatusByName(container.Name)

		// Call internal container post-stop lifecycle hook for any non-running container so that any
		// allocated cpus are released immediately. If the container is restarted, cpus will be re-allocated
		// to it.
		// 释放topologyManager分配资源
		// container启动了，但是container不处于"running"状态，则执行清理container ID在i.topologyManager.podTopologyHints分配资源
		if containerStatus != nil && containerStatus.State != kubecontainer.ContainerStateRunning {
			// 清理container ID在i.topologyManager.podTopologyHints分配资源
			if err := m.internalLifecycle.PostStopContainer(containerStatus.ID.ID); err != nil {
				klog.Errorf("internal container post-stop lifecycle hook failed for container %v in pod %v with error %v",
					container.Name, pod.Name, err)
			}
		}

		// If container does not exist, or is not running, check whether we
		// need to restart it.
		// container未启动或container不处于running状态
		if containerStatus == nil || containerStatus.State != kubecontainer.ContainerStateRunning {
			// container应该被重启（container未启动或状态为"unknown"或"created"，或"exited"且exitcode不为0且restart policy不为"Never"），则将container添加到changes。ContainersToStart列表里。container已经启动且container处于"unknown"状态，则添加container到changes.ContainersToKill（容器需要停止）
			if kubecontainer.ShouldContainerBeRestarted(&container, pod, podStatus) {
				message := fmt.Sprintf("Container %+v is dead, but RestartPolicy says that we should restart it.", container)
				klog.V(3).Infof(message)
				changes.ContainersToStart = append(changes.ContainersToStart, idx)
				// container已经启动且container处于"unknown"状态，则添加container到changes.ContainersToKill（容器需要停止）
				if containerStatus != nil && containerStatus.State == kubecontainer.ContainerStateUnknown {
					// If container is in unknown state, we don't know whether it
					// is actually running or not, always try killing it before
					// restart to avoid having 2 running instances of the same container.
					changes.ContainersToKill[containerStatus.ID] = containerToKillInfo{
						name:      containerStatus.Name,
						container: &pod.Spec.Containers[idx],
						message: fmt.Sprintf("Container is in %q state, try killing it before restart",
							containerStatus.State),
					}
				}
			}
			continue
		}

		// container处于running状态
		// The container is running, but kill the container if any of the following condition is met.
		var message string
		// pod的restart policy不为"Never"则返回为true，即container启动失败是否要重启
		restart := shouldRestartOnFailure(pod)
		// 期望的hash值和container runtime status里hash值不相等，需要重启
		if _, _, changed := containerChanged(&container, containerStatus); changed {
			message = fmt.Sprintf("Container %s definition changed", container.Name)
			// Restart regardless of the restart policy because the container
			// spec changed.
			restart = true
		// liveness probe结果是失败
		} else if liveness, found := m.livenessManager.Get(containerStatus.ID); found && liveness == proberesults.Failure {
			// If the container failed the liveness probe, we should kill it.
			message = fmt.Sprintf("Container %s failed liveness probe", container.Name)
		// startup probe结果是失败
		} else if startup, found := m.startupManager.Get(containerStatus.ID); found && startup == proberesults.Failure {
			// If the container failed the startup probe, we should kill it.
			message = fmt.Sprintf("Container %s failed startup probe", container.Name)
		} else {
			// Keep the container.
			// container处于ready状态，则增加keepCount计数
			keepCount++
			continue
		}

		// We need to kill the container, but if we also want to restart the
		// container afterwards, make the intent clear in the message. Also do
		// not kill the entire pod since we expect container to be running eventually.
		// liveness probe或startup probe结果是失败，只要restart policy不为"Never"，则需要重启
		// container hash值不一致，也需要重启
		if restart {
			message = fmt.Sprintf("%s, will be restarted", message)
			changes.ContainersToStart = append(changes.ContainersToStart, idx)
		}

		// 添加container到changes.ContainersToKill
		changes.ContainersToKill[containerStatus.ID] = containerToKillInfo{
			name:      containerStatus.Name,
			container: &pod.Spec.Containers[idx],
			message:   message,
		}
		klog.V(2).Infof("Container %q (%q) of pod %s: %s", container.Name, containerStatus.ID, format.Pod(pod), message)
	}

	// 如果普通的container都未启动，或都需要重启，则设置changes.KillPod为true（需要停止所有container包括sandbox）
	if keepCount == 0 && len(changes.ContainersToStart) == 0 {
		changes.KillPod = true
	}

	return changes
}

// SyncPod syncs the running pod into the desired pod by executing following steps:
//
//  1. Compute sandbox and container changes.
//  2. Kill pod sandbox if necessary.
//  3. Kill any containers that should not be running.
//  4. Create sandbox if necessary.
//  5. Create ephemeral containers.
//  6. Create init containers.
//  7. Create normal containers.
// 1. 根据runtime种container和sandbox的状态，计算出需要采取的动作
// 2. 执行相关的动作，Kill pod sandbox、Kill any containers、 Create sandbox、Create ephemeral containers、Create init containers、Create normal containers
func (m *kubeGenericRuntimeManager) SyncPod(pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, backOff *flowcontrol.Backoff) (result kubecontainer.PodSyncResult) {
	// Step 1: Compute sandbox and container changes.
	// 根据runtime种container的状态，计算出需要采取的动作
	podContainerChanges := m.computePodActions(pod, podStatus)
	klog.V(3).Infof("computePodActions got %+v for pod %q", podContainerChanges, format.Pod(pod))
	// 需要创建sandbox情况，原来有sandboxID，则发生sandbox改变的事件，否则记录日志（说明创建新的pod）
	if podContainerChanges.CreateSandbox {
		// 获取pod的reference
		ref, err := ref.GetReference(legacyscheme.Scheme, pod)
		if err != nil {
			klog.Errorf("Couldn't make a ref to pod %q: '%v'", format.Pod(pod), err)
		}
		if podContainerChanges.SandboxID != "" {
			m.recorder.Eventf(ref, v1.EventTypeNormal, events.SandboxChanged, "Pod sandbox changed, it will be killed and re-created.")
		} else {
			klog.V(4).Infof("SyncPod received new pod %q, will create a sandbox for it", format.Pod(pod))
		}
	}

	// Step 2: Kill the pod if the sandbox has changed.
	// 需要停止所有容器和sanbox
	if podContainerChanges.KillPod {
		if podContainerChanges.CreateSandbox {
			klog.V(4).Infof("Stopping PodSandbox for %q, will start new one", format.Pod(pod))
		} else {
			klog.V(4).Infof("Stopping PodSandbox for %q because all other containers are dead.", format.Pod(pod))
		}

		// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container），返回所有killContainer结果
		// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有sandbox container，添加执行结果到result
		killResult := m.killPodWithSyncResult(pod, kubecontainer.ConvertPodStatusToRunningPod(m.runtimeName, podStatus), nil)
		result.AddPodSyncResult(killResult)
		if killResult.Error() != nil {
			klog.Errorf("killPodWithSyncResult failed: %v", killResult.Error())
			return
		}

		// 需要停止所有容器和sanbox且需要创建sandbox情况，则移除所有的init container
		if podContainerChanges.CreateSandbox {
			// 遍历pod的所有init container
			// 先释放container ID在m.internalLifecycle.topologyManager.podTopologyHints分配资源
			// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
			// 调用runtime移除容器
			// 删除掉m.containerRefManager里containerIDToRef里有关这个container id的记录
			m.purgeInitContainers(pod, podStatus)
		}
	} else {
		// Step 3: kill any running containers in this pod which are not to keep.
		// 只需要停止部分容器
		for containerID, containerInfo := range podContainerChanges.ContainersToKill {
			klog.V(3).Infof("Killing unwanted container %q(id=%q) for pod %q", containerInfo.name, containerID, format.Pod(pod))
			killContainerResult := kubecontainer.NewSyncResult(kubecontainer.KillContainer, containerInfo.name)
			result.AddSyncResult(killContainerResult)
			// 停止container
			// 1. 执行prestophook
			// 2. 调用runtime停止container
			// 3. 删除掉m.containerRefManager里containerIDToRef里有关这个container id的记录
			if err := m.killContainer(pod, containerID, containerInfo.name, containerInfo.message, nil); err != nil {
				killContainerResult.Fail(kubecontainer.ErrKillContainer, err.Error())
				klog.Errorf("killContainer %q(id=%q) for pod %q failed: %v", containerInfo.name, containerID, format.Pod(pod), err)
				return
			}
		}
	}

	// Keep terminated init containers fairly aggressively controlled
	// This is an optimization because container removals are typically handled
	// by container garbage collector.
	// 移除多余的同名的container且处于"exited"或"unknown"状态，只保留最近一个启动过的container
	m.pruneInitContainersBeforeStart(pod, podStatus)

	// We pass the value of the PRIMARY podIP and list of podIPs down to
	// generatePodSandboxConfig and generateContainerConfig, which in turn
	// passes it to various other functions, in order to facilitate functionality
	// that requires this value (hosts file and downward API) and avoid races determining
	// the pod IP in cases where a container requires restart but the
	// podIP isn't in the status manager yet. The list of podIPs is used to
	// generate the hosts file.
	//
	// We default to the IPs in the passed-in pod status, and overwrite them if the
	// sandbox needs to be (re)started.
	var podIPs []string
	if podStatus != nil {
		podIPs = podStatus.IPs
	}

	// Step 4: Create a sandbox for the pod if necessary.
	// 创建sandbox
	podSandboxID := podContainerChanges.SandboxID
	if podContainerChanges.CreateSandbox {
		var msg string
		var err error

		klog.V(4).Infof("Creating sandbox for pod %q", format.Pod(pod))
		createSandboxResult := kubecontainer.NewSyncResult(kubecontainer.CreatePodSandbox, format.Pod(pod))
		result.AddSyncResult(createSandboxResult)
		// 1. 生成runtimeapi的sandbox配置
		// 2. 创建pod日志目录"/var/log/pods/"+{pod namespace}_{pod name}_{pod uid}
		// 3. 调用cri接口创建sandbox容器（runtime具体实现会调用cni来分配ip、网卡）
		podSandboxID, msg, err = m.createPodSandbox(pod, podContainerChanges.Attempt)
		if err != nil {
			createSandboxResult.Fail(kubecontainer.ErrCreatePodSandbox, msg)
			klog.Errorf("createPodSandbox for pod %q failed: %v", format.Pod(pod), err)
			ref, referr := ref.GetReference(legacyscheme.Scheme, pod)
			if referr != nil {
				klog.Errorf("Couldn't make a ref to pod %q: '%v'", format.Pod(pod), referr)
			}
			m.recorder.Eventf(ref, v1.EventTypeWarning, events.FailedCreatePodSandBox, "Failed to create pod sandbox: %v", err)
			return
		}
		klog.V(4).Infof("Created PodSandbox %q for pod %q", podSandboxID, format.Pod(pod))

		// 如果runtime是dockershim，返回podsandbox的pod的元信息（name、namespace、uid、attempt（重启次数）和容器的信息（id、state运行状态、创建时间、Labels、Annotations、ip、内核命名空间、额外ip）
		podSandboxStatus, err := m.runtimeService.PodSandboxStatus(podSandboxID)
		if err != nil {
			ref, referr := ref.GetReference(legacyscheme.Scheme, pod)
			if referr != nil {
				klog.Errorf("Couldn't make a ref to pod %q: '%v'", format.Pod(pod), referr)
			}
			m.recorder.Eventf(ref, v1.EventTypeWarning, events.FailedStatusPodSandBox, "Unable to get pod sandbox status: %v", err)
			klog.Errorf("Failed to get pod sandbox status: %v; Skipping pod %q", err, format.Pod(pod))
			result.Fail(err)
			return
		}

		// If we ever allow updating a pod from non-host-network to
		// host-network, we may use a stale IP.
		// pod不是host网络模式，验证podSandbox里的所有ip是否合法，并更新podIPs
		if !kubecontainer.IsHostNetworkPod(pod) {
			// Overwrite the podIPs passed in the pod status, since we just started the pod sandbox.
			// 验证podSandbox里的所有ip是否合法，并返回podSandbox里的所有ip（primary IP和additional ips）
			podIPs = m.determinePodSandboxIPs(pod.Namespace, pod.Name, podSandboxStatus)
			klog.V(4).Infof("Determined the ip %v for pod %q after sandbox changed", podIPs, format.Pod(pod))
		}
	}

	// the start containers routines depend on pod ip(as in primary pod ip)
	// instead of trying to figure out if we have 0 < len(podIPs)
	// everytime, we short circuit it here
	podIP := ""
	if len(podIPs) != 0 {
		podIP = podIPs[0]
	}

	// Get podSandboxConfig for containers to start.
	configPodSandboxResult := kubecontainer.NewSyncResult(kubecontainer.ConfigPodSandbox, podSandboxID)
	result.AddSyncResult(configPodSandboxResult)
	// 再次生成runtimeapi的sandbox配置（之前在m.createPodSandbox里已经调用过，这里是为了生成runtimeapi的container配置）
	podSandboxConfig, err := m.generatePodSandboxConfig(pod, podContainerChanges.Attempt)
	if err != nil {
		message := fmt.Sprintf("GeneratePodSandboxConfig for pod %q failed: %v", format.Pod(pod), err)
		klog.Error(message)
		configPodSandboxResult.Fail(kubecontainer.ErrConfigPodSandbox, message)
		return
	}

	// Helper containing boilerplate common to starting all types of containers.
	// typeName is a label used to describe this type of container in log messages,
	// currently: "container", "init container" or "ephemeral container"
	start := func(typeName string, spec *startSpec) error {
		startContainerResult := kubecontainer.NewSyncResult(kubecontainer.StartContainer, spec.container.Name)
		result.AddSyncResult(startContainerResult)

		// 判断container是否要进行回退操作
		isInBackOff, msg, err := m.doBackOff(pod, spec.container, podStatus, backOff)
		// 在回退周期内，则直接返回"CrashLoopBackOff"错误
		if isInBackOff {
			startContainerResult.Fail(err, msg)
			klog.V(4).Infof("Backing Off restarting %v %+v in pod %v", typeName, spec.container, format.Pod(pod))
			return err
		}

		klog.V(4).Infof("Creating %v %+v in pod %v", typeName, spec.container, format.Pod(pod))
		// NOTE (aramase) podIPs are populated for single stack and dual stack clusters. Send only podIPs.
		// 不在回退周期内，则启动container
		// 1. 确保container镜像存在，imagePuller是并行拉取或串行拉取
		// 2. 创建container
		// 3. 启动container
		// 4. 执行post start lifecycle
		if msg, err := m.startContainer(podSandboxID, podSandboxConfig, spec, pod, podStatus, pullSecrets, podIP, podIPs); err != nil {
			startContainerResult.Fail(err, msg)
			// known errors that are logged in other places are logged at higher levels here to avoid
			// repetitive log spam
			switch {
			case err == images.ErrImagePullBackOff:
				klog.V(3).Infof("%v start failed: %v: %s", typeName, err, msg)
			default:
				utilruntime.HandleError(fmt.Errorf("%v start failed: %v: %s", typeName, err, msg))
			}
			return err
		}

		return nil
	}

	// Step 5: start ephemeral containers
	// These are started "prior" to init containers to allow running ephemeral containers even when there
	// are errors starting an init container. In practice init containers will start first since ephemeral
	// containers cannot be specified on pod creation.
	if utilfeature.DefaultFeatureGate.Enabled(features.EphemeralContainers) {
		for _, idx := range podContainerChanges.EphemeralContainersToStart {
			start("ephemeral container", ephemeralContainerStartSpec(&pod.Spec.EphemeralContainers[idx]))
		}
	}

	// Step 6: start the init container.
	// podContainerChanges.NextInitContainerToStart有容器，则podContainerChanges.ContainersToStart一定为空，即initcontainer需要启动，这个函数里一定不会启动普通容器
	// 一次SyncPod只启动一个init container，启动完之后，下次SyncPod再启动一个init container
	if container := podContainerChanges.NextInitContainerToStart; container != nil {
		// Start the next init container.
		if err := start("init container", containerStartSpec(container)); err != nil {
			// 发生错误返回错误
			return
		}

		// Successfully started the container; clear the entry in the failure
		klog.V(4).Infof("Completed init container %q for pod %q", container.Name, format.Pod(pod))
	}

	// Step 7: start containers in podContainerChanges.ContainersToStart.
	for _, idx := range podContainerChanges.ContainersToStart {
		// 忽略任何错误
		start("container", containerStartSpec(&pod.Spec.Containers[idx]))
	}

	return
}

// If a container is still in backoff, the function will return a brief backoff error and
// a detailed error message.
// 判断container是否要进行回退操作
// 判断container是否在"exited"状态，不为"exited"状态返回false, "", nil
// container为"exited"状态，则判断是否在回退周期内。在回退周期内，则false，backoff错误信息和"CrashLoopBackOff"
// 不在回退周期内，重新计算回退周期。
func (m *kubeGenericRuntimeManager) doBackOff(pod *v1.Pod, container *v1.Container, podStatus *kubecontainer.PodStatus, backOff *flowcontrol.Backoff) (bool, string, error) {
	var cStatus *kubecontainer.ContainerStatus
	// 从podStatus.ContainerStatuses查询container是否处于"exited"状态
	for _, c := range podStatus.ContainerStatuses {
		if c.Name == container.Name && c.State == kubecontainer.ContainerStateExited {
			cStatus = c
			break
		}
	}

	// container不存于"exited"状态
	if cStatus == nil {
		return false, "", nil
	}

	klog.V(3).Infof("checking backoff for container %q in pod %q", container.Name, format.Pod(pod))
	// Use the finished time of the latest exited container as the start point to calculate whether to do back-off.
	ts := cStatus.FinishedAt
	// backOff requires a unique key to identify the container.
	// 返回{pod name}_{pod namespace}_{pod uid}_{container name}_{container hash}
	key := getStableKey(pod, container)
	// 以cStatus.FinishedAt为起始点，现在处于回退周期内，返回true和backoff错误信息和"CrashLoopBackOff"
	if backOff.IsInBackOffSince(key, ts) {
		// 生成container属于pod的ObjectReference
		if ref, err := kubecontainer.GenerateContainerRef(pod, container); err == nil {
			m.recorder.Eventf(ref, v1.EventTypeWarning, events.BackOffStartContainer, "Back-off restarting failed container")
		}
		// backOff.Get(key) 返回id的回退周期
		err := fmt.Errorf("back-off %s restarting failed container=%s pod=%s", backOff.Get(key), container.Name, format.Pod(pod))
		klog.V(3).Infof("%s", err.Error())
		return true, err.Error(), kubecontainer.ErrCrashLoopBackOff
	}

	// 不在回退周期内，更新回退周期
	backOff.Next(key, ts)
	return false, "", nil
}

// KillPod kills all the containers of a pod. Pod may be nil, running pod must not be.
// gracePeriodOverride if specified allows the caller to override the pod default grace period.
// only hard kill paths are allowed to specify a gracePeriodOverride in the kubelet in order to not corrupt user data.
// it is useful when doing SIGKILL for hard eviction scenarios, or max grace period during soft eviction scenarios.
// 从kl.Podkiller()调用这个，则gracePeriodOverride为nil
// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container），返回所有killContainer结果
// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container，添加执行结果到result
func (m *kubeGenericRuntimeManager) KillPod(pod *v1.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) error {
	err := m.killPodWithSyncResult(pod, runningPod, gracePeriodOverride)
	return err.Error()
}

// killPodWithSyncResult kills a runningPod and returns SyncResult.
// Note: The pod passed in could be *nil* when kubelet restarted.
// 从kl.Podkiller()调用这个，则gracePeriodOverride为nil
// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container），返回所有killContainer结果
// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有sandbox container，添加执行结果到result
func (m *kubeGenericRuntimeManager) killPodWithSyncResult(pod *v1.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) (result kubecontainer.PodSyncResult) {
	// 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container），返回所有killContainer结果
	killContainerResults := m.killContainersWithSyncResult(pod, runningPod, gracePeriodOverride)
	for _, containerResult := range killContainerResults {
		result.AddSyncResult(containerResult)
	}

	// stop sandbox, the sandbox will be removed in GarbageCollect
	killSandboxResult := kubecontainer.NewSyncResult(kubecontainer.KillPodSandbox, runningPod.ID)
	result.AddSyncResult(killSandboxResult)
	// Stop all sandboxes belongs to same pod
	for _, podSandbox := range runningPod.Sandboxes {
		// runtime为dockershim
		// 1. 获取pod namespace、pod name和是否为host网络模式，先尝试从docker inspect信息中获取，如果出错则dockershim的checkpoint中获取
		// 2. 调用网络插件释放容器的网卡
		// 3. 停止sandbox container，10s的优雅关闭时间
		if err := m.runtimeService.StopPodSandbox(podSandbox.ID.ID); err != nil {
			killSandboxResult.Fail(kubecontainer.ErrKillPodSandbox, err.Error())
			klog.Errorf("Failed to stop sandbox %q", podSandbox.ID)
		}
	}

	return
}

// GetPodStatus retrieves the status of the pod, including the
// information of all containers in the pod that are visible in Runtime.
// 返回pod的uid、name、namespace、以及所有sanbox状态、ips、所有容器状态
func (m *kubeGenericRuntimeManager) GetPodStatus(uid kubetypes.UID, name, namespace string) (*kubecontainer.PodStatus, error) {
	// Now we retain restart count of container as a container label. Each time a container
	// restarts, pod will read the restart count from the registered dead container, increment
	// it to get the new restart count, and then add a label with the new restart count on
	// the newly started container.
	// However, there are some limitations of this method:
	//	1. When all dead containers were garbage collected, the container status could
	//	not get the historical value and would be *inaccurate*. Fortunately, the chance
	//	is really slim.
	//	2. When working with old version containers which have no restart count label,
	//	we can only assume their restart count is 0.
	// Anyhow, we only promised "best-effort" restart count reporting, we can just ignore
	// these limitations now.
	// TODO: move this comment to SyncPod.
	// 根据pod的uid和运行状态state，获取所有sandbox容器的id列表，按照创建时间倒序排（最新创建的排前面）
	podSandboxIDs, err := m.getSandboxIDByPodUID(uid, nil)
	if err != nil {
		return nil, err
	}

	// 输出"{name}_{namespace}_{uid}"
	podFullName := format.Pod(&v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       uid,
		},
	})
	klog.V(4).Infof("getSandboxIDByPodUID got sandbox IDs %q for pod %q", podSandboxIDs, podFullName)

	sandboxStatuses := make([]*runtimeapi.PodSandboxStatus, len(podSandboxIDs))
	podIPs := []string{}
	for idx, podSandboxID := range podSandboxIDs {
		// 如果为dockershim，则返回podsandbox的pod的元信息（name、namespace、uid、attempt（重启次数）和容器的信息（id、state运行状态、创建时间、Labels、Annotations、ip、内核命名空间、额外ip）
		podSandboxStatus, err := m.runtimeService.PodSandboxStatus(podSandboxID)
		if err != nil {
			klog.Errorf("PodSandboxStatus of sandbox %q for pod %q error: %v", podSandboxID, podFullName, err)
			return nil, err
		}
		sandboxStatuses[idx] = podSandboxStatus

		// Only get pod IP from latest sandbox
		if idx == 0 && podSandboxStatus.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			// 验证并返回podSandbox里的所有ip（primary IP和additional ips）
			podIPs = m.determinePodSandboxIPs(namespace, name, podSandboxStatus)
		}
	}

	// Get statuses of all containers visible in the pod.
	// 获取pod里所有container的状态（id、运行状态、创建时间、启动时间、完成时间、挂载、退出码、退出Reason、Message、Labels、annotations、log路径）
	containerStatuses, err := m.getPodContainerStatuses(uid, name, namespace)
	if err != nil {
		if m.logReduction.ShouldMessageBePrinted(err.Error(), podFullName) {
			klog.Errorf("getPodContainerStatuses for pod %q failed: %v", podFullName, err)
		}
		return nil, err
	}
	m.logReduction.ClearID(podFullName)

	return &kubecontainer.PodStatus{
		ID:                uid,
		Name:              name,
		Namespace:         namespace,
		IPs:               podIPs,
		SandboxStatuses:   sandboxStatuses,
		ContainerStatuses: containerStatuses,
	}, nil
}

// GarbageCollect removes dead containers using the specified container gc policy.
// 移除可以移除的container
// 获取非running container且container createdAt时间距离现在已经经过minAge的container
// allSourcesReady为true，则移除所有可以驱逐的container
// 根据gcPolicy.MaxPerPodContainer，移除每个pod里老的可以移除的container
// 根据gcPolicy.MaxContainers，移除pod里或所有container里老的可以移除的container
//
// 移除可以移除的sandbox
// 如果pod删除了或pod处于Terminated状态且evictNonDeletedPods为true，则移除所有不为running且没有container属于它的sandbox容器
// 如果pod没有被删除了，且pod不处于Terminated状态或pod处于Terminated状态且evictNonDeletedPods为false，则保留pod的最近一个sanbox容器（移除其他老的、不为running且没有container属于它的pod）
//
// 移除pod日志目录
// allSourcesReady为true，且pod（pod uid从目录名中提取）不为删除状态，则递归移除目录"/var/log/pods/{pod namespace}_{pod name}_{pod uid}"
// "/var/log/containers/*.log"文件，如果链接的目标文件不存在，则移除这个链接
func (m *kubeGenericRuntimeManager) GarbageCollect(gcPolicy kubecontainer.ContainerGCPolicy, allSourcesReady bool, evictNonDeletedPods bool) error {
	return m.containerGC.GarbageCollect(gcPolicy, allSourcesReady, evictNonDeletedPods)
}

// UpdatePodCIDR is just a passthrough method to update the runtimeConfig of the shim
// with the podCIDR supplied by the kubelet.
//发送grpc给--container-runtime-endpoint监听的grpc服务，比如dockershim（docker_service），让它更新cni配置的cidr
func (m *kubeGenericRuntimeManager) UpdatePodCIDR(podCIDR string) error {
	// TODO(#35531): do we really want to write a method on this manager for each
	// field of the config?
	klog.Infof("updating runtime config through cri with podcidr %v", podCIDR)
	return m.runtimeService.UpdateRuntimeConfig(
		&runtimeapi.RuntimeConfig{
			NetworkConfig: &runtimeapi.NetworkConfig{
				PodCidr: podCIDR,
			},
		})
}
