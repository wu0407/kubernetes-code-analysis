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

package dockershim

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockerfilters "github.com/docker/docker/api/types/filters"
	dockerstrslice "github.com/docker/docker/api/types/strslice"
	"k8s.io/klog"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/kubelet/dockershim/libdocker"
)

// ListContainers lists all containers matching the filter.
// 从docker api中获取提供的过滤条件（如果有的话）和过滤条件Label["io.kubernetes.docker.type"]=="container"的container
// 并从label为"io.kubernetes.sandbox.id"的值，获得sandbox id，容器名中解析出pod中container name名字（不是运行时容器名字）和attempt
func (ds *dockerService) ListContainers(_ context.Context, r *runtimeapi.ListContainersRequest) (*runtimeapi.ListContainersResponse, error) {
	filter := r.GetFilter()
	opts := dockertypes.ContainerListOptions{All: true}

	opts.Filters = dockerfilters.NewArgs()
	f := newDockerFilter(&opts.Filters)
	// Add filter to get *only* (non-sandbox) containers.
	// 添加过滤条件Label["io.kubernetes.docker.type"]=="container"
	f.AddLabel(containerTypeLabelKey, containerTypeLabelContainer)

	if filter != nil {
		if filter.Id != "" {
			f.Add("id", filter.Id)
		}
		if filter.State != nil {
			f.Add("status", toDockerContainerStatus(filter.GetState().State))
		}
		if filter.PodSandboxId != "" {
			f.AddLabel(sandboxIDLabelKey, filter.PodSandboxId)
		}

		if filter.LabelSelector != nil {
			for k, v := range filter.LabelSelector {
				f.AddLabel(k, v)
			}
		}
	}
	// 根据过滤条件获取所有continer
	containers, err := ds.client.ListContainers(opts)
	if err != nil {
		return nil, err
	}
	// Convert docker to runtime api containers.
	result := []*runtimeapi.Container{}
	for i := range containers {
		c := containers[i]

		// 转成runtimeapi格式，包含id、sandboxid、metadata（pod中container name名字和容器重启次数）、image、state、CreatedAt、Labels、Annotations
		// 根据label为"io.kubernetes.sandbox.id"的值，获得sandbox id
		// 从容器名中解析出pod中container name名字（不是运行时容器名字）和attempt
		converted, err := toRuntimeAPIContainer(&c)
		if err != nil {
			klog.V(4).Infof("Unable to convert docker to runtime API container: %v", err)
			continue
		}

		result = append(result, converted)
	}

	return &runtimeapi.ListContainersResponse{Containers: result}, nil
}

// CreateContainer creates a new container in the given PodSandbox
// Docker cannot store the log to an arbitrary location (yet), so we create an
// symlink at LogPath, linking to the actual path of the log.
// TODO: check if the default values returned by the runtime API are ok.
// 这里没有进行确保镜像存在的操作，是因为kubelet 调用cri创建容器，会先确保镜像存在
// 根据*runtimeapi.CreateContainerRequest创建容器
func (ds *dockerService) CreateContainer(_ context.Context, r *runtimeapi.CreateContainerRequest) (*runtimeapi.CreateContainerResponse, error) {
	podSandboxID := r.PodSandboxId
	config := r.GetConfig()
	sandboxConfig := r.GetSandboxConfig()

	if config == nil {
		return nil, fmt.Errorf("container config is nil")
	}
	if sandboxConfig == nil {
		return nil, fmt.Errorf("sandbox config is nil for container %q", config.Metadata.Name)
	}

	// config里的label，包括["io.kubernetes.pod.name"]={pod name}和["io.kubernetes.pod.namespace"]={pod namespace}和["io.kubernetes.pod.uid"]={pod uid}和["io.kubernetes.container.name"]={container name}
	// config里的annotation，包括device plugin annotations
	// ["io.kubernetes.container.hash"]={container hash}
	// ["io.kubernetes.container.restartCount"]={container restart count}
	// ["io.kubernetes.container.terminationMessagePath"]={container.TerminationMessagePath}
	// ["io.kubernetes.container.terminationMessagePolicy"]={container.TerminationMessagePolicy}
	// ["io.kubernetes.pod.deletionGracePeriod"]={pod.DeletionGracePeriodSeconds}，可能有（但是一般不会有，这个在pod被删除时候，应该不会进行创建容器）
	// ["io.kubernetes.pod.terminationGracePeriod"]={pod.Spec.TerminationGracePeriodSeconds}
	// ["io.kubernetes.container.preStopHandler"]={container prestop lifecycle json序列化后}，当container定义了Lifecycle.PreStop
	// ["io.kubernetes.container.ports"]={container.Ports json序列化后}，当container定义了Ports
	// 将annotations的key添加"annotation."前缀，然后与labels进行合并
	labels := makeLabels(config.GetLabels(), config.GetAnnotations())
	// Apply a the container type label.
	// 添加["io.kubernetes.docker.type"]="container"
	labels[containerTypeLabelKey] = containerTypeLabelContainer
	// Write the container log path in the labels.
	// 添加["io.kubernetes.container.logpath"]={一般为"/var/log/pods/{pod namespace}_{pod name}_{pod uid}/{containerName}/{container restartCount}.log"}
	labels[containerLogPathLabelKey] = filepath.Join(sandboxConfig.LogDirectory, config.LogPath)
	// Write the sandbox ID in the labels.
	// 添加["io.kubernetes.sandbox.id"]={podSandboxID}
	labels[sandboxIDLabelKey] = podSandboxID

	// 返回docker的apiversion 
	apiVersion, err := ds.getDockerAPIVersion()
	if err != nil {
		return nil, fmt.Errorf("unable to get the docker API version: %v", err)
	}

	image := ""
	if iSpec := config.GetImage(); iSpec != nil {
		image = iSpec.Image
	}
	// 生成"k8s_{container name}_{pod name}_{pod namespace}_{pod uid}_{container attempt}"
	containerName := makeContainerName(sandboxConfig, config)
	// kubernetes container不需要设置NetworkingConfig，因为不使用docker的自带网络
	createConfig := dockertypes.ContainerCreateConfig{
		Name: containerName,
		Config: &dockercontainer.Config{
			// TODO: set User.
			Entrypoint: dockerstrslice.StrSlice(config.Command),
			Cmd:        dockerstrslice.StrSlice(config.Args),
			// 将[]*runtimeapi.KeyValue转成'<key>=<value>'格式的[]string
			Env:        generateEnvList(config.GetEnvs()),
			Image:      image,
			WorkingDir: config.WorkingDir,
			Labels:     labels,
			// Interactive containers:
			OpenStdin: config.Stdin,
			StdinOnce: config.StdinOnce,
			Tty:       config.Tty,
			// Disable Docker's health check until we officially support it
			// (https://github.com/kubernetes/kubernetes/issues/25829).
			Healthcheck: &dockercontainer.HealthConfig{
				Test: []string{"NONE"},
			},
		},
		HostConfig: &dockercontainer.HostConfig{
			// 将mounts []*runtimeapi.Mount转成'<HostPath>:<ContainerPath>[:options]'格式的[]string
			Binds: generateMountBindings(config.GetMounts()),
			RestartPolicy: dockercontainer.RestartPolicy{
				Name: "no",
			},
		},
	}

	hc := createConfig.HostConfig
	// 设置createConfig.HostConfig里的Resources（包括Memory、MemorySwap、CPUShares、CPUQuota、CPUPeriod，没有CpusetCpus、CpusetMems）和OomScoreAdj
	// 设置createConfig.Config.User和createConfig.HostConfig的GroupAdd、Privileged、ReadonlyRootfs、CapAdd、CapDrop、SecurityOpt（包含selinux、apparmor、NoNewPrivs，但是没有Seccomp）、MaskedPaths、ReadonlyPaths、PidMode、NetworkMode、IpcMode、UTSMode、CgroupParent
	err = ds.updateCreateConfig(&createConfig, config, sandboxConfig, podSandboxID, securityOptSeparator, apiVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to update container create config: %v", err)
	}
	// Set devices for container.
	devices := make([]dockercontainer.DeviceMapping, len(config.Devices))
	for i, device := range config.Devices {
		devices[i] = dockercontainer.DeviceMapping{
			PathOnHost:        device.HostPath,
			PathInContainer:   device.ContainerPath,
			CgroupPermissions: device.Permissions,
		}
	}
	hc.Resources.Devices = devices

	// 返回"seccomp"的SecurityOpts，比如seccompProfile为空，返回[]string{"seccomp=unconfined"}
	securityOpts, err := ds.getSecurityOpts(config.GetLinux().GetSecurityContext().GetSeccompProfilePath(), securityOptSeparator)
	if err != nil {
		return nil, fmt.Errorf("failed to generate security options for container %q: %v", config.Metadata.Name, err)
	}

	// "seccomp"的SecurityOpts添加到hc.SecurityOpt
	hc.SecurityOpt = append(hc.SecurityOpt, securityOpts...)

	// linux系统，不做任何东西
	cleanupInfo, err := ds.applyPlatformSpecificDockerConfig(r, &createConfig)
	if err != nil {
		return nil, err
	}

	// 创建容器
	createResp, createErr := ds.client.CreateContainer(createConfig)
	if createErr != nil {
		// 为了解决docker 1.11版本以及之前版本的问题，如果docker移除容器发生"device or resource busy"问题，这个容器信息可能未被清理掉。导致创建相同名字容器时出现失败"Conflict. {container id} is already in use by container {container id}"。
		// 解决这个问题方法是，则再次执行移除发生冲突的旧容器。如果移除成功，则返回nil和原来的错误。如果移除失败，则对容器名字添加随机数字进行再次创建容器，返回创建的容器的返回和错误
		createResp, createErr = recoverFromCreationConflictIfNeeded(ds.client, createConfig, createErr)
	}

	if createResp != nil {
		containerID := createResp.ID

		if cleanupInfo != nil {
			// we don't perform the clean up just yet at that could destroy information
			// needed for the container to start (e.g. Windows credentials stored in
			// registry keys); instead, we'll clean up when the container gets removed
			ds.containerCleanupInfos[containerID] = cleanupInfo
		}
		return &runtimeapi.CreateContainerResponse{ContainerId: containerID}, nil
	}

	// the creation failed, let's clean up right away - we ignore any errors though,
	// this is best effort
	// createResp为nil，代表容器创建失败
	// linux系统不做任何事情
	ds.performPlatformSpecificContainerCleanupAndLogErrors(containerName, cleanupInfo)

	return nil, createErr
}

// getContainerLogPath returns the container log path specified by kubelet and the real
// path where docker stores the container log.
// 从docker inspect返回中的获取Lables["io.kubernetes.container.logpath"]的值和docker真实的日志路径
func (ds *dockerService) getContainerLogPath(containerID string) (string, string, error) {
	info, err := ds.client.InspectContainer(containerID)
	if err != nil {
		return "", "", fmt.Errorf("failed to inspect container %q: %v", containerID, err)
	}
	return info.Config.Labels[containerLogPathLabelKey], info.LogPath, nil
}

// createContainerLogSymlink creates the symlink for docker container log.
// 创建kubelet的日志文件软链到docker真实的日志路径
func (ds *dockerService) createContainerLogSymlink(containerID string) error {
	// 从docker inspect返回中的获取Lables["io.kubernetes.container.logpath"]的值和docker真实的日志路径
	path, realPath, err := ds.getContainerLogPath(containerID)
	if err != nil {
		return fmt.Errorf("failed to get container %q log path: %v", containerID, err)
	}

	if path == "" {
		klog.V(5).Infof("Container %s log path isn't specified, will not create the symlink", containerID)
		return nil
	}

	// docker真实的日志路径不为空
	if realPath != "" {
		// Only create the symlink when container log path is specified and log file exists.
		// Delete possibly existing file first
		// 先尝试删除kubelet的日志软链文件
		if err = ds.os.Remove(path); err == nil {
			klog.Warningf("Deleted previously existing symlink file: %q", path)
		}
		// 然后进行path（kubelet的日志软链文件）链接realPath（docker真实的日志路径）
		if err = ds.os.Symlink(realPath, path); err != nil {
			return fmt.Errorf("failed to create symbolic link %q to the container log file %q for container %q: %v",
				path, realPath, containerID, err)
		}
	} else {
		// 如果docker真实的日志路径为空

		// docker的LoggingDriver为"json-file"，返回true，否则返回false
		supported, err := ds.IsCRISupportedLogDriver()
		if err != nil {
			klog.Warningf("Failed to check supported logging driver by CRI: %v", err)
			return nil
		}

		// 支持则记录warning日志
		if supported {
			klog.Warningf("Cannot create symbolic link because container log file doesn't exist!")
		} else {
			// 不支持记录info日志
			klog.V(5).Infof("Unsupported logging driver by CRI")
		}
	}

	return nil
}

// removeContainerLogSymlink removes the symlink for docker container log.
// 移除docker inspect的Lables["io.kubernetes.container.logpath"]的值（kubelet创建的软链）
func (ds *dockerService) removeContainerLogSymlink(containerID string) error {
	// 从docker inspect返回中的获取Lables["io.kubernetes.container.logpath"]的值（kubelet创建的软链）
	path, _, err := ds.getContainerLogPath(containerID)
	if err != nil {
		return fmt.Errorf("failed to get container %q log path: %v", containerID, err)
	}
	if path != "" {
		// Only remove the symlink when container log path is specified.
		err := ds.os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove container %q log symlink %q: %v", containerID, path, err)
		}
	}
	return nil
}

// StartContainer starts the container.
// 执行StartContainer操作，然后无论是否启动成功，都创建kubelet的日志文件软链到docker真实的日志路径
func (ds *dockerService) StartContainer(_ context.Context, r *runtimeapi.StartContainerRequest) (*runtimeapi.StartContainerResponse, error) {
	err := ds.client.StartContainer(r.ContainerId)

	// Create container log symlink for all containers (including failed ones).
	// 创建kubelet的日志文件软链到docker真实的日志路径
	if linkError := ds.createContainerLogSymlink(r.ContainerId); linkError != nil {
		// Do not stop the container if we failed to create symlink because:
		//   1. This is not a critical failure.
		//   2. We don't have enough information to properly stop container here.
		// Kubelet will surface this error to user via an event.
		return nil, linkError
	}

	if err != nil {
		err = transformStartContainerError(err)
		return nil, fmt.Errorf("failed to start container %q: %v", r.ContainerId, err)
	}

	return &runtimeapi.StartContainerResponse{}, nil
}

// StopContainer stops a running container with a grace period (i.e., timeout).
func (ds *dockerService) StopContainer(_ context.Context, r *runtimeapi.StopContainerRequest) (*runtimeapi.StopContainerResponse, error) {
	err := ds.client.StopContainer(r.ContainerId, time.Duration(r.Timeout)*time.Second)
	if err != nil {
		return nil, err
	}
	return &runtimeapi.StopContainerResponse{}, nil
}

// RemoveContainer removes the container.
// 先移除kubelet创建的日志软链，然后再执行docker container rm移除容器
func (ds *dockerService) RemoveContainer(_ context.Context, r *runtimeapi.RemoveContainerRequest) (*runtimeapi.RemoveContainerResponse, error) {
	// Ideally, log lifecycle should be independent of container lifecycle.
	// However, docker will remove container log after container is removed,
	// we can't prevent that now, so we also clean up the symlink here.
	// 移除docker inspect的Lables["io.kubernetes.container.logpath"]的值（kubelet创建的软链）
	err := ds.removeContainerLogSymlink(r.ContainerId)
	if err != nil {
		return nil, err
	}
	// linux系统不做任何事情
	errors := ds.performPlatformSpecificContainerForContainer(r.ContainerId)
	if len(errors) != 0 {
		return nil, fmt.Errorf("failed to run platform-specific clean ups for container %q: %v", r.ContainerId, errors)
	}
	// 执行docker container rm
	err = ds.client.RemoveContainer(r.ContainerId, dockertypes.ContainerRemoveOptions{RemoveVolumes: true, Force: true})
	if err != nil {
		return nil, fmt.Errorf("failed to remove container %q: %v", r.ContainerId, err)
	}

	return &runtimeapi.RemoveContainerResponse{}, nil
}

// 返回创建时间和启动时间和完成时间
func getContainerTimestamps(r *dockertypes.ContainerJSON) (time.Time, time.Time, time.Time, error) {
	var createdAt, startedAt, finishedAt time.Time
	var err error

	createdAt, err = libdocker.ParseDockerTimestamp(r.Created)
	if err != nil {
		return createdAt, startedAt, finishedAt, err
	}
	startedAt, err = libdocker.ParseDockerTimestamp(r.State.StartedAt)
	if err != nil {
		return createdAt, startedAt, finishedAt, err
	}
	finishedAt, err = libdocker.ParseDockerTimestamp(r.State.FinishedAt)
	if err != nil {
		return createdAt, startedAt, finishedAt, err
	}
	return createdAt, startedAt, finishedAt, nil
}

// ContainerStatus inspects the docker container and returns the status.
// 返回container的状态（id、运行状态、创建时间、启动时间、完成时间、挂载、退出码、退出Reason、Message、Labels、annotations、log路径）
func (ds *dockerService) ContainerStatus(_ context.Context, req *runtimeapi.ContainerStatusRequest) (*runtimeapi.ContainerStatusResponse, error) {
	containerID := req.ContainerId
	r, err := ds.client.InspectContainer(containerID)
	if err != nil {
		return nil, err
	}

	// Parse the timestamps.
	// 返回创建时间和启动时间和完成时间
	createdAt, startedAt, finishedAt, err := getContainerTimestamps(r)
	if err != nil {
		return nil, fmt.Errorf("failed to parse timestamp for container %q: %v", containerID, err)
	}

	// Convert the image id to a pullable id.
	ir, err := ds.client.InspectImageByID(r.Image)
	if err != nil {
		if !libdocker.IsImageNotFoundError(err) {
			return nil, fmt.Errorf("unable to inspect docker image %q while inspecting docker container %q: %v", r.Image, containerID, err)
		}
		klog.Warningf("ignore error image %q not found while inspecting docker container %q: %v", r.Image, containerID, err)
	}
	// 如果image有RepoDigests，则返回"docker-pullable://"+ image的第一个digest
	// 否则返回 "docker://"+镜像id
	imageID := toPullableImageID(r.Image, ir)

	// Convert the mounts.
	mounts := make([]*runtimeapi.Mount, 0, len(r.Mounts))
	for i := range r.Mounts {
		m := r.Mounts[i]
		readonly := !m.RW
		mounts = append(mounts, &runtimeapi.Mount{
			HostPath:      m.Source,
			ContainerPath: m.Destination,
			Readonly:      readonly,
			// Note: Can't set SeLinuxRelabel
		})
	}
	// Interpret container states.
	var state runtimeapi.ContainerState
	var reason, message string
	if r.State.Running {
		// Container is running.
		state = runtimeapi.ContainerState_CONTAINER_RUNNING
	} else {
		// Container is *not* running. We need to get more details.
		//    * Case 1: container has run and exited with non-zero finishedAt
		//              time.
		//    * Case 2: container has failed to start; it has a zero finishedAt
		//              time, but a non-zero exit code.
		//    * Case 3: container has been created, but not started (yet).
		if !finishedAt.IsZero() { // Case 1
			state = runtimeapi.ContainerState_CONTAINER_EXITED
			switch {
			case r.State.OOMKilled:
				// TODO: consider exposing OOMKilled via the runtimeAPI.
				// Note: if an application handles OOMKilled gracefully, the
				// exit code could be zero.
				reason = "OOMKilled"
			case r.State.ExitCode == 0:
				reason = "Completed"
			default:
				reason = "Error"
			}
		} else if r.State.ExitCode != 0 { // Case 2
			state = runtimeapi.ContainerState_CONTAINER_EXITED
			// Adjust finshedAt and startedAt time to createdAt time to avoid
			// the confusion.
			finishedAt, startedAt = createdAt, createdAt
			reason = "ContainerCannotRun"
		} else { // Case 3
			state = runtimeapi.ContainerState_CONTAINER_CREATED
		}
		message = r.State.Error
	}

	// Convert to unix timestamps.
	ct, st, ft := createdAt.UnixNano(), startedAt.UnixNano(), finishedAt.UnixNano()
	exitCode := int32(r.State.ExitCode)

	// 从docker的容器名中，解析出container在pod中的名字和重启次数
	metadata, err := parseContainerName(r.Name)
	if err != nil {
		return nil, err
	}

	labels, annotations := extractLabels(r.Config.Labels)
	imageName := r.Config.Image
	if ir != nil && len(ir.RepoTags) > 0 {
		imageName = ir.RepoTags[0]
	}
	status := &runtimeapi.ContainerStatus{
		Id:          r.ID,
		Metadata:    metadata,
		Image:       &runtimeapi.ImageSpec{Image: imageName},
		ImageRef:    imageID,
		Mounts:      mounts,
		ExitCode:    exitCode,
		State:       state,
		CreatedAt:   ct,
		StartedAt:   st,
		FinishedAt:  ft,
		Reason:      reason,
		Message:     message,
		Labels:      labels,
		Annotations: annotations,
		LogPath:     r.Config.Labels[containerLogPathLabelKey],
	}
	return &runtimeapi.ContainerStatusResponse{Status: status}, nil
}

func (ds *dockerService) UpdateContainerResources(_ context.Context, r *runtimeapi.UpdateContainerResourcesRequest) (*runtimeapi.UpdateContainerResourcesResponse, error) {
	resources := r.Linux
	updateConfig := dockercontainer.UpdateConfig{
		Resources: dockercontainer.Resources{
			CPUPeriod:  resources.CpuPeriod,
			CPUQuota:   resources.CpuQuota,
			CPUShares:  resources.CpuShares,
			Memory:     resources.MemoryLimitInBytes,
			CpusetCpus: resources.CpusetCpus,
			CpusetMems: resources.CpusetMems,
		},
	}

	err := ds.client.UpdateContainerResources(r.ContainerId, updateConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to update container %q: %v", r.ContainerId, err)
	}
	return &runtimeapi.UpdateContainerResourcesResponse{}, nil
}

// linux系统不做任何事情
func (ds *dockerService) performPlatformSpecificContainerForContainer(containerID string) (errors []error) {
	if cleanupInfo, present := ds.containerCleanupInfos[containerID]; present {
		errors = ds.performPlatformSpecificContainerCleanupAndLogErrors(containerID, cleanupInfo)

		if len(errors) == 0 {
			delete(ds.containerCleanupInfos, containerID)
		}
	}

	return
}

// linux系统不做任何事情
func (ds *dockerService) performPlatformSpecificContainerCleanupAndLogErrors(containerNameOrID string, cleanupInfo *containerCleanupInfo) []error {
	if cleanupInfo == nil {
		return nil
	}

	// linux系统不做任何事情
	errors := ds.performPlatformSpecificContainerCleanup(cleanupInfo)
	for _, err := range errors {
		klog.Warningf("error when cleaning up after container %q: %v", containerNameOrID, err)
	}

	return errors
}
