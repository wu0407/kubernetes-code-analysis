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
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"

	grpcstatus "google.golang.org/grpc/status"

	"github.com/armon/circbuf"
	"k8s.io/klog"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/util/selinux"
	"k8s.io/kubernetes/pkg/util/tail"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
)

var (
	// ErrCreateContainerConfig - failed to create container config
	ErrCreateContainerConfig = errors.New("CreateContainerConfigError")
	// ErrCreateContainer - failed to create container
	ErrCreateContainer = errors.New("CreateContainerError")
	// ErrPreStartHook - failed to execute PreStartHook
	ErrPreStartHook = errors.New("PreStartHookError")
	// ErrPostStartHook - failed to execute PostStartHook
	ErrPostStartHook = errors.New("PostStartHookError")
)

// recordContainerEvent should be used by the runtime manager for all container related events.
// it has sanity checks to ensure that we do not write events that can abuse our masters.
// in particular, it ensures that a containerID never appears in an event message as that
// is prone to causing a lot of distinct events that do not count well.
// it replaces any reference to a containerID with the containerName which is stable, and is what users know.
func (m *kubeGenericRuntimeManager) recordContainerEvent(pod *v1.Pod, container *v1.Container, containerID, eventType, reason, message string, args ...interface{}) {
	// 获取v1.ObjectReference 并添加FieldPath字段
	ref, err := kubecontainer.GenerateContainerRef(pod, container)
	if err != nil {
		klog.Errorf("Can't make a ref to pod %q, container %v: %v", format.Pod(pod), container.Name, err)
		return
	}
	eventMessage := message
	if len(args) > 0 {
		eventMessage = fmt.Sprintf(message, args...)
	}
	// this is a hack, but often the error from the runtime includes the containerID
	// which kills our ability to deduplicate events.  this protection makes a huge
	// difference in the number of unique events
	if containerID != "" {
		eventMessage = strings.Replace(eventMessage, containerID, container.Name, -1)
	}
	m.recorder.Event(ref, eventType, reason, eventMessage)
}

// startSpec wraps the spec required to start a container, either a regular/init container
// or an ephemeral container. Ephemeral containers contain all the fields of regular/init
// containers, plus some additional fields. In both cases startSpec.container will be set.
type startSpec struct {
	container          *v1.Container
	ephemeralContainer *v1.EphemeralContainer
}

func containerStartSpec(c *v1.Container) *startSpec {
	return &startSpec{container: c}
}

func ephemeralContainerStartSpec(ec *v1.EphemeralContainer) *startSpec {
	return &startSpec{
		container:          (*v1.Container)(&ec.EphemeralContainerCommon),
		ephemeralContainer: ec,
	}
}

// getTargetID returns the kubecontainer.ContainerID for ephemeral container namespace
// targeting. The target is stored as EphemeralContainer.TargetContainerName, which must be
// resolved to a ContainerID using podStatus. The target container must already exist, which
// usually isn't a problem since ephemeral containers aren't allowed at pod creation time.
// This always returns nil when the EphemeralContainers feature is disabled.
// 返回s.ephemeralContainer.TargetContainerName的container id
func (s *startSpec) getTargetID(podStatus *kubecontainer.PodStatus) (*kubecontainer.ContainerID, error) {
	if s.ephemeralContainer == nil || s.ephemeralContainer.TargetContainerName == "" || !utilfeature.DefaultFeatureGate.Enabled(features.EphemeralContainers) {
		return nil, nil
	}

	// 从podStatus.ContainerStatuses里，返回第一个名为containerName的ContainerStatus
	targetStatus := podStatus.FindContainerStatusByName(s.ephemeralContainer.TargetContainerName)
	if targetStatus == nil {
		return nil, fmt.Errorf("unable to find target container %v", s.ephemeralContainer.TargetContainerName)
	}

	return &targetStatus.ID, nil
}

// startContainer starts a container and returns a message indicates why it is failed on error.
// It starts the container through the following steps:
// * pull the image
// * create the container
// * start the container
// * run the post start lifecycle hooks (if applicable)
// 1. 确保container镜像存在，imagePuller是并行拉取或串行拉取
// 2. 创建container
// 3. 启动container
// 4. 执行post start lifecycle
func (m *kubeGenericRuntimeManager) startContainer(podSandboxID string, podSandboxConfig *runtimeapi.PodSandboxConfig, spec *startSpec, pod *v1.Pod, podStatus *kubecontainer.PodStatus, pullSecrets []v1.Secret, podIP string, podIPs []string) (string, error) {
	container := spec.container

	// Step 1: pull the image.
	// 确保container镜像存在（拉取镜像或本地已经存在不拉取或强制拉取）
	imageRef, msg, err := m.imagePuller.EnsureImageExists(pod, container, pullSecrets, podSandboxConfig)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return msg, err
	}

	// Step 2: create the container.
	// 生成container属于pod的ObjectReference
	ref, err := kubecontainer.GenerateContainerRef(pod, container)
	if err != nil {
		klog.Errorf("Can't make a ref to pod %q, container %v: %v", format.Pod(pod), container.Name, err)
	}
	klog.V(4).Infof("Generating ref for container %s: %#v", container.Name, ref)

	// For a new container, the RestartCount should be 0
	restartCount := 0
	// 从podStatus.ContainerStatuses里，返回第一个名为containerName的ContainerStatus
	containerStatus := podStatus.FindContainerStatusByName(container.Name)
	if containerStatus != nil {
		restartCount = containerStatus.RestartCount + 1
	}

	// 返回spec.ephemeralContainer.TargetContainerName的container id
	target, err := spec.getTargetID(podStatus)
	// 在spec.ephemeralContainer不为nil，且从podStatus里获取不到spec.ephemeralContainer.TargetContainerName的container id，直接返回
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return s.Message(), ErrCreateContainerConfig
	}

	// 1. 先根据container、pod、podIP、podIPs生成opt *kubecontainer.RunContainerOptions
	// opt *kubecontainer.RunContainerOptions只用了Env、PodContainerDir、Devices、Mounts、Annotations，没有用PortMappings、EnableHostUserNamespace、Hostname、ReadOnly（字段在m.runtimeHelper.GenerateRunContainerOptions没有设置）
	// 2. 根据opt生成runtimeapi.ContainerConfig
	containerConfig, cleanupAction, err := m.generateContainerConfig(container, pod, restartCount, podIP, imageRef, podIPs, target)
	if cleanupAction != nil {
		defer cleanupAction()
	}
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, "", v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return s.Message(), ErrCreateContainerConfig
	}

	// 调用cri接口创建容器
	containerID, err := m.runtimeService.CreateContainer(podSandboxID, containerConfig, podSandboxConfig)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, containerID, v1.EventTypeWarning, events.FailedToCreateContainer, "Error: %v", s.Message())
		return s.Message(), ErrCreateContainer
	}
	// 调用cri更新容器的cpuset cgroup
	// 添加container id到m.topologyManager.podMap
	err = m.internalLifecycle.PreStartContainer(pod, container, containerID)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, containerID, v1.EventTypeWarning, events.FailedToStartContainer, "Internal PreStartContainer hook failed: %v", s.Message())
		return s.Message(), ErrPreStartHook
	}
	m.recordContainerEvent(pod, container, containerID, v1.EventTypeNormal, events.CreatedContainer, fmt.Sprintf("Created container %s", container.Name))

	// 将container id和对应的ref添加到m.containerRefManager.containerIDToRef
	if ref != nil {
		m.containerRefManager.SetRef(kubecontainer.ContainerID{
			Type: m.runtimeName,
			ID:   containerID,
		}, ref)
	}

	// Step 3: start the container.
	err = m.runtimeService.StartContainer(containerID)
	if err != nil {
		s, _ := grpcstatus.FromError(err)
		m.recordContainerEvent(pod, container, containerID, v1.EventTypeWarning, events.FailedToStartContainer, "Error: %v", s.Message())
		return s.Message(), kubecontainer.ErrRunContainer
	}
	m.recordContainerEvent(pod, container, containerID, v1.EventTypeNormal, events.StartedContainer, fmt.Sprintf("Started container %s", container.Name))

	// Symlink container logs to the legacy container log location for cluster logging
	// support.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	// 包含container name和重启次数
	containerMeta := containerConfig.GetMetadata()
	// 包含pod name、pod namespace、pod uid、sandbox重启次数
	sandboxMeta := podSandboxConfig.GetMetadata()
	// 返回"/var/log/containers/{podName}_{podNamespace}_{containerName}-{containerID}.log"
	legacySymlink := legacyLogSymlink(containerID, containerMeta.Name, sandboxMeta.Name,
		sandboxMeta.Namespace)
	// "/var/log/pods/{pod namespace}_{pod name}_{pod uid}/{containerName}/{container restartCount}.log"
	containerLog := filepath.Join(podSandboxConfig.LogDirectory, containerConfig.LogPath)
	// only create legacy symlink if containerLog path exists (or the error is not IsNotExist).
	// Because if containerLog path does not exist, only dandling legacySymlink is created.
	// This dangling legacySymlink is later removed by container gc, so it does not make sense
	// to create it in the first place. it happens when journald logging driver is used with docker.
	if _, err := m.osInterface.Stat(containerLog); !os.IsNotExist(err) {
		// 当containerLog存在的时候，创建legacySymlink软链containerLog
		if err := m.osInterface.Symlink(containerLog, legacySymlink); err != nil {
			klog.Errorf("Failed to create legacy symbolic link %q to container %q log %q: %v",
				legacySymlink, containerID, containerLog, err)
		}
	}

	// Step 4: execute the post start hook.
	if container.Lifecycle != nil && container.Lifecycle.PostStart != nil {
		kubeContainerID := kubecontainer.ContainerID{
			Type: m.runtimeName,
			ID:   containerID,
		}
		// 执行container.Lifecycle.PostStart
		msg, handlerErr := m.runner.Run(kubeContainerID, pod, container, container.Lifecycle.PostStart)
		if handlerErr != nil {
			m.recordContainerEvent(pod, container, kubeContainerID.ID, v1.EventTypeWarning, events.FailedPostStartHook, msg)
			// container.Lifecycle.PostStart执行出错，则停止container
			// 停止container
			// 1. 执行prestophook
			// 2. 调用runtime停止container
			// 3. 删除掉m.containerRefManager里containerIDToRef里有关这个container id的记录
			if err := m.killContainer(pod, kubeContainerID, container.Name, "FailedPostStartHook", nil); err != nil {
				klog.Errorf("Failed to kill container %q(id=%q) in pod %q: %v, %v",
					container.Name, kubeContainerID.String(), format.Pod(pod), ErrPostStartHook, err)
			}
			return msg, fmt.Errorf("%s: %v", ErrPostStartHook, handlerErr)
		}
	}

	return "", nil
}

// generateContainerConfig generates container config for kubelet runtime v1.
// 1. 先根据container、pod、podIP、podIPs生成opt *kubecontainer.RunContainerOptions
// opt *kubecontainer.RunContainerOptions只用了Env、PodContainerDir、Devices、Mounts、Annotations，没有用PortMappings、EnableHostUserNamespace、Hostname、ReadOnly（字段在m.runtimeHelper.GenerateRunContainerOptions没有设置）
// 2. 根据opt生成runtimeapi.ContainerConfig
func (m *kubeGenericRuntimeManager) generateContainerConfig(container *v1.Container, pod *v1.Pod, restartCount int, podIP, imageRef string, podIPs []string, nsTarget *kubecontainer.ContainerID) (*runtimeapi.ContainerConfig, func(), error) {
	// m.runtimeHelper为*Kubelet
	// 1. 从device manager中（分配或重用）获得container需要的资源，生成Devices、Mounts、Envs、Annotations
	// 2. 根据container中port定义生成PortMappings
	// 3. 根据container.VolumeDevices，生成container里包含块设备的信息Devices
	// 4. 根据container.EnvFrom和container.Env，生成container的环境变量Envs，包括service环境变量、EnvFrom、Env（包括了ValueFrom、普通的key value）
	// 5. 根据container.VolumeMounts，并从volume manager中获取pod已经挂载的volume在宿主机中的路径作为源路径，生成挂载信息Mounts（对subpath的挂载，生成subpath目录和bind挂载目录，并进行bind挂载，使用bind挂载路径作为源路径），并添加"/etc/hosts"挂载信息，生成挂载源文件"/var/lib/kubelet/pods/{pod uid}/etc-hosts"
	// 6. 创建container目录"/var/lib/kubelet/pods/{podUID}/containers/{container name}"，设置PodContainerDir
	// 7. 根据container里的设置和kl.experimentalHostUserNamespaceDefaulting，判断是否设置EnableHostUserNamespace
	// 返回的cleanupAction为nil
	opts, cleanupAction, err := m.runtimeHelper.GenerateRunContainerOptions(pod, container, podIP, podIPs)
	if err != nil {
		return nil, nil, err
	}

	// inspect image获得镜像里设置user id或用户名（docker镜像里没有定义用户，返回0, "", nil）
	uid, username, err := m.getImageUser(container.Image)
	if err != nil {
		return nil, cleanupAction, err
	}

	// Verify RunAsNonRoot. Non-root verification only supports numeric user.
	// 验证传入的参数（镜像里的uid和username）和最终container应用到SecurityContext.RunAsUser，是否符合最终container应用到SecurityContext.RunAsNonRoot设置
	if err := verifyRunAsNonRoot(pod, container, uid, username); err != nil {
		return nil, cleanupAction, err
	}

	// 处理container里Command和Args里类似"$(var)"格式，从envs查找变量值进行替换。或"$$(var)"格式则进行转义，输出"$(var)"
	command, args := kubecontainer.ExpandContainerCommandAndArgs(container, opts.Envs)
	// 返回"/var/log/pods/{pod namespace}_{pod name}_{pod uid}/{container name}" 
	logDir := BuildContainerLogsDirectory(pod.Namespace, pod.Name, pod.UID, container.Name)
	// 创建container日志目录
	err = m.osInterface.MkdirAll(logDir, 0755)
	if err != nil {
		return nil, cleanupAction, fmt.Errorf("create container log directory for container %s failed: %v", container.Name, err)
	}
	// 返回"{containerName}/{restartCount}.log"
	containerLogsPath := buildContainerLogsPath(container.Name, restartCount)
	restartCountUint32 := uint32(restartCount)
	config := &runtimeapi.ContainerConfig{
		Metadata: &runtimeapi.ContainerMetadata{
			Name:    container.Name,
			Attempt: restartCountUint32,
		},
		Image:       &runtimeapi.ImageSpec{Image: imageRef},
		Command:     command,
		Args:        args,
		WorkingDir:  container.WorkingDir,
		// 返回label，包括["io.kubernetes.pod.name"]={pod name}和["io.kubernetes.pod.namespace"]={pod namespace}和["io.kubernetes.pod.uid"]={pod uid}和["io.kubernetes.container.name"]={container name}
		Labels:      newContainerLabels(container, pod),
		// 返回annotation包括device plugin annotations
		// ["io.kubernetes.container.hash"]={container hash}
		// ["io.kubernetes.container.restartCount"]={container restart count}
		// ["io.kubernetes.container.terminationMessagePath"]={container.TerminationMessagePath}
		// ["io.kubernetes.container.terminationMessagePolicy"]={container.TerminationMessagePolicy}
		// ["io.kubernetes.pod.deletionGracePeriod"]={pod.DeletionGracePeriodSeconds}，可能有（但是一般不会有，这个在pod被删除时候，应该不会进行创建容器）
		// ["io.kubernetes.pod.terminationGracePeriod"]={pod.Spec.TerminationGracePeriodSeconds}
		// ["io.kubernetes.container.preStopHandler"]={container prestop lifecycle json序列化后}，当container定义了Lifecycle.PreStop
		// ["io.kubernetes.container.ports"]={container.Ports json序列化后}，当container定义了Ports
		Annotations: newContainerAnnotations(container, pod, restartCount, opts),
		// 将opts.Devices转成[]*runtimeapi.Device
		Devices:     makeDevices(opts),
		// 将opts.Mounts []kubecontainer.Mount转成[]*runtimeapi.Mount，并添加terminationMessagePath挂载，container.TerminationMessagePath默认为"/dev/termination-log"
		Mounts:      m.makeMounts(opts, container),
		LogPath:     containerLogsPath,
		// 默认false
		Stdin:       container.Stdin,
		// 默认false
		StdinOnce:   container.StdinOnce,
		// 默认false
		Tty:         container.TTY,
	}

	// set platform specific configurations.
	// linux操作系统，设置config.Linux
	// 生成container的*runtimeapi.LinuxContainerConfig，包括SecurityContext和Resources（CpuPeriod、CpuQuota、CpuShares、MemoryLimitInBytes、OomScoreAdj、CpusetCpus（没有设置）、CpusetMems（没有设置）、HugepageLimits）
	// linux系统里的container的*runtimeapi.LinuxContainerConfig.SecurityContext，不使用container.SecurityContext里的WindowsOptions、RunAsNonRoot
	if err := m.applyPlatformSpecificContainerConfig(config, container, pod, uid, username, nsTarget); err != nil {
		return nil, cleanupAction, err
	}

	// set environment variables
	// 将opts.Envs []kubecontainer.EnvVar转成[]*runtimeapi.KeyValue
	envs := make([]*runtimeapi.KeyValue, len(opts.Envs))
	for idx := range opts.Envs {
		e := opts.Envs[idx]
		envs[idx] = &runtimeapi.KeyValue{
			Key:   e.Name,
			Value: e.Value,
		}
	}
	config.Envs = envs

	return config, cleanupAction, nil
}

// makeDevices generates container devices for kubelet runtime v1.
// 将opts.Devices []kubecontainer.DeviceInfo转成[]*runtimeapi.Device
func makeDevices(opts *kubecontainer.RunContainerOptions) []*runtimeapi.Device {
	devices := make([]*runtimeapi.Device, len(opts.Devices))

	for idx := range opts.Devices {
		device := opts.Devices[idx]
		devices[idx] = &runtimeapi.Device{
			HostPath:      device.PathOnHost,
			ContainerPath: device.PathInContainer,
			Permissions:   device.Permissions,
		}
	}

	return devices
}

// makeMounts generates container volume mounts for kubelet runtime v1.
// 将opts.Mounts []kubecontainer.Mount转成[]*runtimeapi.Mount，并添加terminationMessagePath挂载，container.TerminationMessagePath默认为"/dev/termination-log"
func (m *kubeGenericRuntimeManager) makeMounts(opts *kubecontainer.RunContainerOptions, container *v1.Container) []*runtimeapi.Mount {
	volumeMounts := []*runtimeapi.Mount{}

	for idx := range opts.Mounts {
		v := opts.Mounts[idx]
		// volume需要selinux relabel且host启用selinux，才会设置selinux relabel
		selinuxRelabel := v.SELinuxRelabel && selinux.SELinuxEnabled()
		mount := &runtimeapi.Mount{
			HostPath:       v.HostPath,
			ContainerPath:  v.ContainerPath,
			Readonly:       v.ReadOnly,
			SelinuxRelabel: selinuxRelabel,
			Propagation:    v.Propagation,
		}

		volumeMounts = append(volumeMounts, mount)
	}

	// The reason we create and mount the log file in here (not in kubelet) is because
	// the file's location depends on the ID of the container, and we need to create and
	// mount the file before actually starting the container.
	// we can only mount individual files (e.g.: /etc/hosts, termination-log files) on Windows only if we're using Containerd.
	// 非windows系统都返回true，windows系统runtime不为"docker"返回true
	supportsSingleFileMapping := m.SupportsSingleFileMapping()
	// opt.PodContainerDir 一般为"/var/lib/kubelet/pods/{podUID}/containers/{container name}"
	// container.TerminationMessagePath默认为"/dev/termination-log"
	if opts.PodContainerDir != "" && len(container.TerminationMessagePath) != 0 && supportsSingleFileMapping {
		// Because the PodContainerDir contains pod uid and container name which is unique enough,
		// here we just add a random id to make the path unique for different instances
		// of the same container.
		// 8位16进制随机数
		cid := makeUID()
		containerLogPath := filepath.Join(opts.PodContainerDir, cid)
		// 创建文件"/var/lib/kubelet/pods/{podUID}/containers/{container name}/{cid}"
		fs, err := m.osInterface.Create(containerLogPath)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("error on creating termination-log file %q: %v", containerLogPath, err))
		} else {
			fs.Close()

			// Chmod is needed because ioutil.WriteFile() ends up calling
			// open(2) to create the file, so the final mode used is "mode &
			// ~umask". But we want to make sure the specified mode is used
			// in the file no matter what the umask is.
			if err := m.osInterface.Chmod(containerLogPath, 0666); err != nil {
				utilruntime.HandleError(fmt.Errorf("unable to set termination-log file permissions %q: %v", containerLogPath, err))
			}

			// Volume Mounts fail on Windows if it is not of the form C:/
			// containerLogPath格式化成当前操作系统风格的路径
			containerLogPath = volumeutil.MakeAbsolutePath(goruntime.GOOS, containerLogPath)
			// container.TerminationMessagePath格式化成当前操作系统风格的路径
			terminationMessagePath := volumeutil.MakeAbsolutePath(goruntime.GOOS, container.TerminationMessagePath)
			// 系统是否启用selinux
			selinuxRelabel := selinux.SELinuxEnabled()
			// 添加terminationMessagePath挂载
			volumeMounts = append(volumeMounts, &runtimeapi.Mount{
				HostPath:       containerLogPath,
				ContainerPath:  terminationMessagePath,
				SelinuxRelabel: selinuxRelabel,
			})
		}
	}

	return volumeMounts
}

// getKubeletContainers lists containers managed by kubelet.
// The boolean parameter specifies whether returns all containers including
// those already exited and dead containers (used for garbage collection).
// 如果allContainers为false，则只获取runtime里running的container
// 如果allContainers为true，则获取runtime里所有container（包括running和非running的）
func (m *kubeGenericRuntimeManager) getKubeletContainers(allContainers bool) ([]*runtimeapi.Container, error) {
	filter := &runtimeapi.ContainerFilter{}
	// allcontainers为false，则只list running的container
	if !allContainers {
		filter.State = &runtimeapi.ContainerStateValue{
			State: runtimeapi.ContainerState_CONTAINER_RUNNING,
		}
	}

	// runtime是dockershim
	// 根据过滤条件Label["io.kubernetes.docker.type"]=="container"的container
	containers, err := m.runtimeService.ListContainers(filter)
	if err != nil {
		klog.Errorf("getKubeletContainers failed: %v", err)
		return nil, err
	}

	return containers, nil
}

// makeUID returns a randomly generated string.
// 8位16进制随机数
func makeUID() string {
	return fmt.Sprintf("%08x", rand.Uint32())
}

// getTerminationMessage looks on the filesystem for the provided termination message path, returning a limited
// amount of those bytes, or returns true if the logs should be checked.
func getTerminationMessage(status *runtimeapi.ContainerStatus, terminationMessagePath string, fallbackToLogs bool) (string, bool) {
	if len(terminationMessagePath) == 0 {
		return "", fallbackToLogs
	}
	// Volume Mounts fail on Windows if it is not of the form C:/
	terminationMessagePath = volumeutil.MakeAbsolutePath(goruntime.GOOS, terminationMessagePath)
	// 找到容器里terminationMessagePath的挂载路径
	for _, mount := range status.Mounts {
		if mount.ContainerPath != terminationMessagePath {
			continue
		}
		path := mount.HostPath
		// 最大读取4096字节，从文件尾部开始计算（相对于结尾的长度）
		data, _, err := tail.ReadAtMost(path, kubecontainer.MaxContainerTerminationMessageLength)
		if err != nil {
			if os.IsNotExist(err) {
				return "", fallbackToLogs
			}
			return fmt.Sprintf("Error on reading termination log %s: %v", path, err), false
		}
		return string(data), (fallbackToLogs && len(data) == 0)
	}
	return "", fallbackToLogs
}

// readLastStringFromContainerLogs attempts to read up to the max log length from the end of the CRI log represented
// by path. It reads up to max log lines.
// 从path中读取日志的最后80行且输出最大2048字节
func (m *kubeGenericRuntimeManager) readLastStringFromContainerLogs(path string) string {
	// 最大80行
	value := int64(kubecontainer.MaxContainerTerminationMessageLogLines)
	// 最多2048字节环形buffer
	buf, _ := circbuf.NewBuffer(kubecontainer.MaxContainerTerminationMessageLogLength)
	// 读取容器日志文件的最后80行，发送到buf中
	if err := m.ReadLogs(context.Background(), path, "", &v1.PodLogOptions{TailLines: &value}, buf, buf); err != nil {
		return fmt.Sprintf("Error on reading termination message from logs: %v", err)
	}
	return buf.String()
}

// getPodContainerStatuses gets all containers' statuses for the pod.
// 获取pod里所有container的状态（id、运行状态、创建时间、启动时间、完成时间、挂载、退出码、退出Reason、Message、Labels、annotations、log路径）
func (m *kubeGenericRuntimeManager) getPodContainerStatuses(uid kubetypes.UID, name, namespace string) ([]*kubecontainer.ContainerStatus, error) {
	// Select all containers of the given pod.
	// runtime是dockershim
	// 根据过滤条件Label["io.kubernetes.docker.type"]=="container"的container和Label["io.kubernetes.pod.uid"]=={uid}获得所有容器
	containers, err := m.runtimeService.ListContainers(&runtimeapi.ContainerFilter{
		LabelSelector: map[string]string{types.KubernetesPodUIDLabel: string(uid)},
	})
	if err != nil {
		klog.Errorf("ListContainers error: %v", err)
		return nil, err
	}

	statuses := make([]*kubecontainer.ContainerStatus, len(containers))
	// TODO: optimization: set maximum number of containers per container name to examine.
	for i, c := range containers {
		// 如果runtime为dockershim，返回container的状态（id、运行状态、创建时间、启动时间、完成时间、挂载、退出码、退出Reason、Message、Labels、annotations、log路径）
		status, err := m.runtimeService.ContainerStatus(c.Id)
		if err != nil {
			// Merely log this here; GetPodStatus will actually report the error out.
			klog.V(4).Infof("ContainerStatus for %s error: %v", c.Id, err)
			return nil, err
		}
		cStatus := toKubeContainerStatus(status, m.runtimeName)
		if status.State == runtimeapi.ContainerState_CONTAINER_EXITED {
			// Populate the termination message if needed.
			// 获取TerminationMessagePolicy、TerminationMessagePath、hash、RestartCount、PodDeletionGracePeriod、PodTerminationGracePeriod、preStopHandler、containerPorts
			annotatedInfo := getContainerInfoFromAnnotations(status.Annotations)
			// If a container cannot even be started, it certainly does not have logs, so no need to fallbackToLogs.
			fallbackToLogs := annotatedInfo.TerminationMessagePolicy == v1.TerminationMessageFallbackToLogsOnError &&
				cStatus.ExitCode != 0 && cStatus.Reason != "ContainerCannotRun"
			// 从TerminationMessagePath文件中读取内容，最大读取4096字节，从文件尾部开始计算（相对于结尾的长度）
			tMessage, checkLogs := getTerminationMessage(status, annotatedInfo.TerminationMessagePath, fallbackToLogs)
			// TerminationMessagePath文件中没有内容且fallbackToLogs为true，则需要读取容器日志
			if checkLogs {
				// if dockerLegacyService is populated, we're supposed to use it to fetch logs
				// 只有runtime是dockershim，当docker日志格式不是"json-file"，则m.legacyLogProvider为dockershim.DockerService
				// m.legacyLogProvider不为nil，则使用dockershim（docker logs）获取容器日志
				if m.legacyLogProvider != nil {
					// 输出容器的最后80行且最大为2048字节日志
					tMessage, err = m.legacyLogProvider.GetContainerLogTail(uid, name, namespace, kubecontainer.ContainerID{Type: m.runtimeName, ID: c.Id})
					if err != nil {
						tMessage = fmt.Sprintf("Error reading termination message from logs: %v", err)
					}
				} else {
					// 否则读取容器日志文件的最后80行且输出最大2048字节
					tMessage = m.readLastStringFromContainerLogs(status.GetLogPath())
				}
			}
			// Enrich the termination message written by the application is not empty
			// termination日志不为空，则添加到containerstatus里message中
			if len(tMessage) != 0 {
				if len(cStatus.Message) != 0 {
					cStatus.Message += ": "
				}
				cStatus.Message += tMessage
			}
		}
		statuses[i] = cStatus
	}

	// 根据container创建时间，从早到晚进行排序
	sort.Sort(containerStatusByCreated(statuses))
	return statuses, nil
}

// runtimeapi格式的ContainerStatus转成kubecontainer格式的ContainerStatus
func toKubeContainerStatus(status *runtimeapi.ContainerStatus, runtimeName string) *kubecontainer.ContainerStatus {
	annotatedInfo := getContainerInfoFromAnnotations(status.Annotations)
	labeledInfo := getContainerInfoFromLabels(status.Labels)
	cStatus := &kubecontainer.ContainerStatus{
		ID: kubecontainer.ContainerID{
			Type: runtimeName,
			ID:   status.Id,
		},
		Name:         labeledInfo.ContainerName,
		Image:        status.Image.Image,
		ImageID:      status.ImageRef,
		Hash:         annotatedInfo.Hash,
		RestartCount: annotatedInfo.RestartCount,
		State:        toKubeContainerState(status.State),
		CreatedAt:    time.Unix(0, status.CreatedAt),
	}

	if status.State != runtimeapi.ContainerState_CONTAINER_CREATED {
		// If container is not in the created state, we have tried and
		// started the container. Set the StartedAt time.
		cStatus.StartedAt = time.Unix(0, status.StartedAt)
	}
	if status.State == runtimeapi.ContainerState_CONTAINER_EXITED {
		cStatus.Reason = status.Reason
		cStatus.Message = status.Message
		cStatus.ExitCode = int(status.ExitCode)
		cStatus.FinishedAt = time.Unix(0, status.FinishedAt)
	}
	return cStatus
}

// executePreStopHook runs the pre-stop lifecycle hooks if applicable and returns the duration it takes.
// 启动一个gorutine执行lifecycle prestop，在gracePeriod周期内等待prestop执行完成
func (m *kubeGenericRuntimeManager) executePreStopHook(pod *v1.Pod, containerID kubecontainer.ContainerID, containerSpec *v1.Container, gracePeriod int64) int64 {
	klog.V(3).Infof("Running preStop hook for container %q", containerID.String())

	start := metav1.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer utilruntime.HandleCrash()

		// 如果是执行命令，则在容器中exec（同步调用），没有超时时间
		// 或者执行http请求，默认使用http.Client{}，也是没有超时时间
		if msg, err := m.runner.Run(containerID, pod, containerSpec, containerSpec.Lifecycle.PreStop); err != nil {
			klog.Errorf("preStop hook for container %q failed: %v", containerSpec.Name, err)
			m.recordContainerEvent(pod, containerSpec, containerID.ID, v1.EventTypeWarning, events.FailedPreStopHook, msg)
		}
	}()

	select {
	case <-time.After(time.Duration(gracePeriod) * time.Second):
		klog.V(2).Infof("preStop hook for container %q did not complete in %d seconds", containerID, gracePeriod)
	case <-done:
		klog.V(3).Infof("preStop hook for container %q completed", containerID)
	}

	return int64(metav1.Now().Sub(start.Time).Seconds())
}

// restoreSpecsFromContainerLabels restores all information needed for killing a container. In some
// case we may not have pod and container spec when killing a container, e.g. pod is deleted during
// kubelet restart.
// To solve this problem, we've already written necessary information into container labels. Here we
// just need to retrieve them from container labels and restore the specs.
// TODO(random-liu): Add a node e2e test to test this behaviour.
// TODO(random-liu): Change the lifecycle handler to just accept information needed, so that we can
// just pass the needed function not create the fake object.
// 从容器信息里的labels，提取出有关pod字段和pod里的container字段
func (m *kubeGenericRuntimeManager) restoreSpecsFromContainerLabels(containerID kubecontainer.ContainerID) (*v1.Pod, *v1.Container, error) {
	var pod *v1.Pod
	var container *v1.Container
	// 获取这个container的所有信息，如果runtime是dockershim话，执行的是dockerClient.InspectContainer
	s, err := m.runtimeService.ContainerStatus(containerID.ID)
	if err != nil {
		return nil, nil, err
	}

	// 获取pod name、pod namespace、podUID、pod里面的container name
	l := getContainerInfoFromLabels(s.Labels)
	// 获取TerminationMessagePolicy、TerminationMessagePath、hash、RestartCount、PodDeletionGracePeriod、PodTerminationGracePeriod、preStopHandler、containerPorts
	a := getContainerInfoFromAnnotations(s.Annotations)
	// Notice that the followings are not full spec. The container killing code should not use
	// un-restored fields.
	pod = &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:                        l.PodUID,
			Name:                       l.PodName,
			Namespace:                  l.PodNamespace,
			DeletionGracePeriodSeconds: a.PodDeletionGracePeriod,
		},
		Spec: v1.PodSpec{
			TerminationGracePeriodSeconds: a.PodTerminationGracePeriod,
		},
	}
	container = &v1.Container{
		Name:                   l.ContainerName,
		Ports:                  a.ContainerPorts,
		TerminationMessagePath: a.TerminationMessagePath,
	}
	if a.PreStopHandler != nil {
		container.Lifecycle = &v1.Lifecycle{
			PreStop: a.PreStopHandler,
		}
	}
	return pod, container, nil
}

// killContainer kills a container through the following steps:
// * Run the pre-stop lifecycle hooks (if applicable).
// * Stop the container.
// 从kl.Podkiller()、m.startContainer、m.containerGC.removeOldestN调用这个，则gracePeriodOverride为nil
// 停止container
// 1. 执行prestophook
// 2. 调用runtime停止container
// 3. 删除掉m.containerRefManager里containerIDToRef里有关这个container id的记录
func (m *kubeGenericRuntimeManager) killContainer(pod *v1.Pod, containerID kubecontainer.ContainerID, containerName string, message string, gracePeriodOverride *int64) error {
	var containerSpec *v1.Container
	if pod != nil {
		// 从pod的spec中获取container信息
		if containerSpec = kubecontainer.GetContainerSpec(pod, containerName); containerSpec == nil {
			return fmt.Errorf("failed to get containerSpec %q(id=%q) in pod %q when killing container for reason %q",
				containerName, containerID.String(), format.Pod(pod), message)
		}
	} else {
		// Restore necessary information if one of the specs is nil.
		// 当pod被删除了，但是kubelet重启了，pod的container还在，则从容器信息里的labels，提取出有关pod字段和pod里的container字段
		restoredPod, restoredContainer, err := m.restoreSpecsFromContainerLabels(containerID)
		if err != nil {
			return err
		}
		pod, containerSpec = restoredPod, restoredContainer
	}

	// From this point, pod and container must be non-nil.
	// 默认为2s，pod有DeletionGracePeriodSeconds则使用这个值，否则使用pod.Spec.TerminationGracePeriodSeconds
	gracePeriod := int64(minimumGracePeriodInSeconds)
	switch {
	case pod.DeletionGracePeriodSeconds != nil:
		gracePeriod = *pod.DeletionGracePeriodSeconds
	case pod.Spec.TerminationGracePeriodSeconds != nil:
		gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
	}

	if len(message) == 0 {
		message = fmt.Sprintf("Stopping container %s", containerSpec.Name)
	}
	m.recordContainerEvent(pod, containerSpec, containerID.ID, v1.EventTypeNormal, events.KillingContainer, message)

	// Run internal pre-stop lifecycle hook
	// 默认不做任何事情
	if err := m.internalLifecycle.PreStopContainer(containerID.ID); err != nil {
		return err
	}

	// Run the pre-stop lifecycle hooks if applicable and if there is enough time to run it
	if containerSpec.Lifecycle != nil && containerSpec.Lifecycle.PreStop != nil && gracePeriod > 0 {
		// 启动一个goroutine执行lifecycle prestop，在gracePeriod周期内等待prestop执行完成
		gracePeriod = gracePeriod - m.executePreStopHook(pod, containerID, containerSpec, gracePeriod)
	}
	// always give containers a minimal shutdown window to avoid unnecessary SIGKILLs
	// 剩下的gracePeriod时间小于2s，则gracePeriod设为2s，避免不必要的SIGKILL
	if gracePeriod < minimumGracePeriodInSeconds {
		gracePeriod = minimumGracePeriodInSeconds
	}
	// 从kl.Podkiller()调用这个，则gracePeriodOverride为nil
	if gracePeriodOverride != nil {
		gracePeriod = *gracePeriodOverride
		klog.V(3).Infof("Killing container %q, but using %d second grace period override", containerID, gracePeriod)
	}

	klog.V(2).Infof("Killing container %q with %d second grace period", containerID.String(), gracePeriod)

	// gracePeriod为调用超时时间（在runtimeService里，默认为2分钟+gracePeriod），如果runtime为dockershim，这个gracePeriod超时时间会传给docker作为docker stop的graceful时间
	err := m.runtimeService.StopContainer(containerID.ID, gracePeriod)
	if err != nil {
		klog.Errorf("Container %q termination failed with gracePeriod %d: %v", containerID.String(), gracePeriod, err)
	} else {
		klog.V(3).Infof("Container %q exited normally", containerID.String())
	}

	// 删除掉containerRefManager里containerIDToRef（有关这个container id的ObjectReference）
	m.containerRefManager.ClearRef(containerID)

	return err
}

// killContainersWithSyncResult kills all pod's containers with sync results.
// 从kl.Podkiller()调用这个，则gracePeriodOverride为nil
// 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container），返回所有killContainer结果
func (m *kubeGenericRuntimeManager) killContainersWithSyncResult(pod *v1.Pod, runningPod kubecontainer.Pod, gracePeriodOverride *int64) (syncResults []*kubecontainer.SyncResult) {
	containerResults := make(chan *kubecontainer.SyncResult, len(runningPod.Containers))
	wg := sync.WaitGroup{}

	wg.Add(len(runningPod.Containers))
	for _, container := range runningPod.Containers {
		go func(container *kubecontainer.Container) {
			defer utilruntime.HandleCrash()
			defer wg.Done()

			// 创建一个"KillContainer"的SyncResult，用于报告killContainer的结果。
			killContainerResult := kubecontainer.NewSyncResult(kubecontainer.KillContainer, container.Name)
			// 停止container
			// 1. 执行prestophook
			// 2. 调用runtime停止container
			// 3. 删除掉m.containerRefManager里containerIDToRef里有关这个container id的记录
			if err := m.killContainer(pod, container.ID, container.Name, "", gracePeriodOverride); err != nil {
				killContainerResult.Fail(kubecontainer.ErrKillContainer, err.Error())
			}
			containerResults <- killContainerResult
		}(container)
	}
	wg.Wait()
	close(containerResults)

	for containerResult := range containerResults {
		syncResults = append(syncResults, containerResult)
	}
	return
}

// pruneInitContainersBeforeStart ensures that before we begin creating init
// containers, we have reduced the number of outstanding init containers still
// present. This reduces load on the container garbage collector by only
// preserving the most recent terminated init container.
// 移除多余的同名的container且处于"exited"或"unknown"状态，只保留最近一个启动过的container
func (m *kubeGenericRuntimeManager) pruneInitContainersBeforeStart(pod *v1.Pod, podStatus *kubecontainer.PodStatus) {
	// only the last execution of each init container should be preserved, and only preserve it if it is in the
	// list of init containers to keep.
	initContainerNames := sets.NewString()
	for _, container := range pod.Spec.InitContainers {
		initContainerNames.Insert(container.Name)
	}
	for name := range initContainerNames {
		count := 0
		// 移除处于"exited"或"unknown"的容器且不是第一个相同名字的init container（在podStatus有可能多个container对应一个名字）
		// 移除多余的同名的container且处于"exited"或"unknown"状态
		for _, status := range podStatus.ContainerStatuses {
			if status.Name != name ||
				(status.State != kubecontainer.ContainerStateExited &&
					status.State != kubecontainer.ContainerStateUnknown) {
				continue
			}
			// Remove init containers in unknown state. It should have
			// been stopped before pruneInitContainersBeforeStart is
			// called.
			count++
			// keep the first init container for this name
			if count == 1 {
				continue
			}
			// prune all other init containers that match this container name
			klog.V(4).Infof("Removing init container %q instance %q %d", status.Name, status.ID.ID, count)
			// 先释放container ID在m.internalLifecycle.topologyManager.podTopologyHints分配资源
			// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
			// 调用runtime移除容器
			if err := m.removeContainer(status.ID.ID); err != nil {
				utilruntime.HandleError(fmt.Errorf("failed to remove pod init container %q: %v; Skipping pod %q", status.Name, err, format.Pod(pod)))
				continue
			}

			// remove any references to this container
			// 删除掉m.containerRefManager里containerIDToRef里有关这个container id的记录
			if _, ok := m.containerRefManager.GetRef(status.ID); ok {
				m.containerRefManager.ClearRef(status.ID)
			} else {
				klog.Warningf("No ref for container %q", status.ID)
			}
		}
	}
}

// Remove all init containres. Note that this function does not check the state
// of the container because it assumes all init containers have been stopped
// before the call happens.
// 遍历pod的所有init container
// 先释放container ID在m.internalLifecycle.topologyManager.podTopologyHints分配资源
// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
// 调用runtime移除容器
// 清除container id对应的ObjectReference
func (m *kubeGenericRuntimeManager) purgeInitContainers(pod *v1.Pod, podStatus *kubecontainer.PodStatus) {
	initContainerNames := sets.NewString()
	for _, container := range pod.Spec.InitContainers {
		initContainerNames.Insert(container.Name)
	}
	for name := range initContainerNames {
		count := 0
		for _, status := range podStatus.ContainerStatuses {
			if status.Name != name {
				continue
			}
			count++
			// Purge all init containers that match this container name
			klog.V(4).Infof("Removing init container %q instance %q %d", status.Name, status.ID.ID, count)
			// 先释放container ID在m.internalLifecycle.topologyManager.podTopologyHints分配资源
			// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
			// 调用runtime移除容器
			if err := m.removeContainer(status.ID.ID); err != nil {
				utilruntime.HandleError(fmt.Errorf("failed to remove pod init container %q: %v; Skipping pod %q", status.Name, err, format.Pod(pod)))
				continue
			}
			// Remove any references to this container
			// 返回container id对应的ObjectReference
			if _, ok := m.containerRefManager.GetRef(status.ID); ok {
				// 清除container id对应的ObjectReference
				m.containerRefManager.ClearRef(status.ID)
			} else {
				klog.Warningf("No ref for container %q", status.ID)
			}
		}
	}
}

// findNextInitContainerToRun returns the status of the last failed container, the
// index of next init container to start, or done if there are no further init containers.
// Status is only returned if an init container is failed, in which case next will
// point to the current container.
// 返回pod中最后一个失败的init container状态，和最后一个失败的init container，和是否所有init container都执行成功
func findNextInitContainerToRun(pod *v1.Pod, podStatus *kubecontainer.PodStatus) (status *kubecontainer.ContainerStatus, next *v1.Container, done bool) {
	if len(pod.Spec.InitContainers) == 0 {
		return nil, nil, true
	}

	// If there are failed containers, return the status of the last failed one.
	// 如果有失败（或未执行）的init container，返回最后一个执行失败（或未执行）的container或状态为"unknown"的container的状态和init container和false
	for i := len(pod.Spec.InitContainers) - 1; i >= 0; i-- {
		container := &pod.Spec.InitContainers[i]
		// 从podStatus.ContainerStatuses里，返回第一个名为containerName的ContainerStatus
		status := podStatus.FindContainerStatusByName(container.Name)
		// container状态处于"exited"且ExitCode不为0，或状态为"unknown"，返回true
		if status != nil && isInitContainerFailed(status) {
			return status, container, false
		}
	}

	// There are no failed containers now.
	// 没有失败的container，有可能没有状态、处于"exited"状态、处于"running"状态
	for i := len(pod.Spec.InitContainers) - 1; i >= 0; i-- {
		container := &pod.Spec.InitContainers[i]
		status := podStatus.FindContainerStatusByName(container.Name)
		if status == nil {
			continue
		}

		// container is still running, return not done.
		// 有container处于"running"状态
		if status.State == kubecontainer.ContainerStateRunning {
			return nil, nil, false
		}

		if status.State == kubecontainer.ContainerStateExited {
			// all init containers successful
			// container处于"exited"状态且container是最后一个init container，即所有init container都成功执行，直接返回
			if i == (len(pod.Spec.InitContainers) - 1) {
				return nil, nil, true
			}

			// all containers up to i successful, go to i+1
			// container处于"exited"状态，后面一个container的status为nil，则返回下一个container的状态（nil）和下一个container和false
			return nil, &pod.Spec.InitContainers[i+1], false
		}
	}

	// 所有的container状态都为nil，则返回nil和第一个container和false
	return nil, &pod.Spec.InitContainers[0], false
}

// GetContainerLogs returns logs of a specific container.
// 调用cri获得容器的日志路径，读取容器日志文件，发送到stdout或stderr
func (m *kubeGenericRuntimeManager) GetContainerLogs(ctx context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) (err error) {
	// 调用cri获得容器的ContainerStatus
	status, err := m.runtimeService.ContainerStatus(containerID.ID)
	if err != nil {
		klog.V(4).Infof("failed to get container status for %v: %v", containerID.String(), err)
		return fmt.Errorf("unable to retrieve container logs for %v", containerID.String())
	}
	// 读取容器日志文件，发送到stdout或stderr
	return m.ReadLogs(ctx, status.GetLogPath(), containerID.ID, logOptions, stdout, stderr)
}

// GetExec gets the endpoint the runtime will serve the exec request from.
// 调用cri接口，返回执行exec的url
func (m *kubeGenericRuntimeManager) GetExec(id kubecontainer.ContainerID, cmd []string, stdin, stdout, stderr, tty bool) (*url.URL, error) {
	req := &runtimeapi.ExecRequest{
		ContainerId: id.ID,
		Cmd:         cmd,
		Tty:         tty,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
	}
	resp, err := m.runtimeService.Exec(req)
	if err != nil {
		return nil, err
	}

	return url.Parse(resp.Url)
}

// GetAttach gets the endpoint the runtime will serve the attach request from.
func (m *kubeGenericRuntimeManager) GetAttach(id kubecontainer.ContainerID, stdin, stdout, stderr, tty bool) (*url.URL, error) {
	req := &runtimeapi.AttachRequest{
		ContainerId: id.ID,
		Stdin:       stdin,
		Stdout:      stdout,
		Stderr:      stderr,
		Tty:         tty,
	}
	resp, err := m.runtimeService.Attach(req)
	if err != nil {
		return nil, err
	}
	return url.Parse(resp.Url)
}

// RunInContainer synchronously executes the command in the container, and returns the output.
// 调用runtime（同步调用）执行cmd，设置执行的timeout，并将stderr append到stdout（也就是说命令输出没有保证顺序）
// 当timeout为0时候，执行m.runtimeService.ExecSync没有超时时间。
// 对于dockershim来说（服务端来说），timeout这个参数会传给dockershim，但是dockershim没有使用。
func (m *kubeGenericRuntimeManager) RunInContainer(id kubecontainer.ContainerID, cmd []string, timeout time.Duration) ([]byte, error) {
	stdout, stderr, err := m.runtimeService.ExecSync(id.ID, cmd, timeout)
	// NOTE(tallclair): This does not correctly interleave stdout & stderr, but should be sufficient
	// for logging purposes. A combined output option will need to be added to the ExecSyncRequest
	// if more precise output ordering is ever required.
	return append(stdout, stderr...), err
}

// removeContainer removes the container and the container logs.
// Notice that we remove the container logs first, so that container will not be removed if
// container logs are failed to be removed, and kubelet will retry this later. This guarantees
// that container logs to be removed with the container.
// Notice that we assume that the container should only be removed in non-running state, and
// it will not write container logs anymore in that state.
// 先释放container ID在m.internalLifecycle.topologyManager.podTopologyHints分配资源
// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
// 调用runtime移除容器
func (m *kubeGenericRuntimeManager) removeContainer(containerID string) error {
	klog.V(4).Infof("Removing container %q", containerID)
	// Call internal container post-stop lifecycle hook.
	// 释放container ID在m.internalLifecycle.topologyManager.podTopologyHints分配资源
	if err := m.internalLifecycle.PostStopContainer(containerID); err != nil {
		return err
	}

	// Remove the container log.
	// TODO: Separate log and container lifecycle management.
	// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
	if err := m.removeContainerLog(containerID); err != nil {
		return err
	}
	// Remove the container.
	// 调用runtime移除容器，如果是dockershim，则会再次移除为docker inspect里的Labels里的"io.kubernetes.container.logpath"，然后再移除container
	return m.runtimeService.RemoveContainer(containerID)
}

// removeContainerLog removes the container log.
// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
func (m *kubeGenericRuntimeManager) removeContainerLog(containerID string) error {
	// Remove the container log.
	// 从runtime中获取container的状态，如果是dockershim，则返回docker inspect信息
	status, err := m.runtimeService.ContainerStatus(containerID)
	if err != nil {
		return fmt.Errorf("failed to get container status %q: %v", containerID, err)
	}
	// 获取pod name、pod namespace、podUID、pod里面的container name
	labeledInfo := getContainerInfoFromLabels(status.Labels)
	// 获取container的日志路径（比如"/var/log/pods/default_nginx-685d7445f4-26jnx_e3a18e85-005b-4042-8f29-57a9a8292ea3/nginx/0.log"）。如果为dockershim，则为docker container的Labels里的"io.kubernetes.container.logpath"
	path := status.GetLogPath()
	// 删除这个路径
	if err := m.osInterface.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove container %q log %q: %v", containerID, path, err)
	}

	// Remove the legacy container log symlink.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	// 比如路径/var/log/containers/xiaoke-marketing-service-java-dev-standard-2-7899db54d8-mkshh_xiaoke-java-dev_app-3d0411e62e43e0ef970498fb1b6a5a097fd494d904ff839c49ebe1f9d94454a0.log
	legacySymlink := legacyLogSymlink(containerID, labeledInfo.ContainerName, labeledInfo.PodName,
		labeledInfo.PodNamespace)
	if err := m.osInterface.Remove(legacySymlink); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove container %q log legacy symbolic link %q: %v",
			containerID, legacySymlink, err)
	}
	return nil
}

// DeleteContainer removes a container.
// 先释放container ID在m.internalLifecycle.topologyManager.podTopologyHints分配资源
// 移除runtime里返回的日志路径（/var/log/pods目录下，文件是软链）和/var/log/containers目录下的容器日志软链接
// 调用runtime移除容器
func (m *kubeGenericRuntimeManager) DeleteContainer(containerID kubecontainer.ContainerID) error {
	return m.removeContainer(containerID.ID)
}
