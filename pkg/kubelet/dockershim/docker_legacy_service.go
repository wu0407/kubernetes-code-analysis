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
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/armon/circbuf"
	dockertypes "github.com/docker/docker/api/types"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubetypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/kuberuntime"

	"k8s.io/kubernetes/pkg/kubelet/dockershim/libdocker"
)

// DockerLegacyService interface embeds some legacy methods for backward compatibility.
// This file/interface will be removed in the near future. Do not modify or add
// more functions.
type DockerLegacyService interface {
	// GetContainerLogs gets logs for a specific container.
	GetContainerLogs(context.Context, *v1.Pod, kubecontainer.ContainerID, *v1.PodLogOptions, io.Writer, io.Writer) error

	// IsCRISupportedLogDriver checks whether the logging driver used by docker is
	// supported by native CRI integration.
	// TODO(resouer): remove this when deprecating unsupported log driver
	IsCRISupportedLogDriver() (bool, error)

	kuberuntime.LegacyLogProvider
}

// GetContainerLogs get container logs directly from docker daemon.
func (d *dockerService) GetContainerLogs(_ context.Context, pod *v1.Pod, containerID kubecontainer.ContainerID, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) error {
	// 进行docker inspect
	container, err := d.client.InspectContainer(containerID.ID)
	if err != nil {
		return err
	}

	var since int64
	// 设置了从之前几秒开始的日志
	if logOptions.SinceSeconds != nil {
		t := metav1.Now().Add(-time.Duration(*logOptions.SinceSeconds) * time.Second)
		since = t.Unix()
	}
	// 设置了从某个时间开始的日志
	if logOptions.SinceTime != nil {
		since = logOptions.SinceTime.Unix()
	}
	opts := dockertypes.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      strconv.FormatInt(since, 10),
		Timestamps: logOptions.Timestamps,
		Follow:     logOptions.Follow,
	}
	// 读取最后几行
	if logOptions.TailLines != nil {
		opts.Tail = strconv.FormatInt(*logOptions.TailLines, 10)
	}

	// 限制了读取大小
	if logOptions.LimitBytes != nil {
		// stdout and stderr share the total write limit
		max := *logOptions.LimitBytes
		stderr = sharedLimitWriter(stderr, &max)
		stdout = sharedLimitWriter(stdout, &max)
	}
	sopts := libdocker.StreamOptions{
		OutputStream: stdout,
		ErrorStream:  stderr,
		RawTerminal:  container.Config.Tty,
	}
	// 先包装记录请求
	// 执行docker logs {container id}
	// 如果有tty（sopts.RawTerminal为true）则拷贝resp到outputStream，否则拷贝resp到outputStream和errorStream
	err = d.client.Logs(containerID.ID, opts, sopts)
	if errors.Is(err, errMaximumWrite) {
		klog.V(2).Infof("finished logs, hit byte limit %d", *logOptions.LimitBytes)
		err = nil
	}
	return err
}

// GetContainerLogTail attempts to read up to MaxContainerTerminationMessageLogLength
// from the end of the log when docker is configured with a log driver other than json-log.
// It reads up to MaxContainerTerminationMessageLogLines lines.
// 输出容器的最后80行且最大为2048字节日志
func (d *dockerService) GetContainerLogTail(uid kubetypes.UID, name, namespace string, containerId kubecontainer.ContainerID) (string, error) {
	// 最大80行
	value := int64(kubecontainer.MaxContainerTerminationMessageLogLines)
	// 最大2048字节，环形buffer
	buf, _ := circbuf.NewBuffer(kubecontainer.MaxContainerTerminationMessageLogLength)
	// Although this is not a full spec pod, dockerLegacyService.GetContainerLogs() currently completely ignores its pod param
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       uid,
			Name:      name,
			Namespace: namespace,
		},
	}
	err := d.GetContainerLogs(context.Background(), pod, containerId, &v1.PodLogOptions{TailLines: &value}, buf, buf)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

// criSupportedLogDrivers are log drivers supported by native CRI integration.
var criSupportedLogDrivers = []string{"json-file"}

// IsCRISupportedLogDriver checks whether the logging driver used by docker is
// supported by native CRI integration.
// docker的LoggingDriver为"json-file"，返回true，否则返回false
func (d *dockerService) IsCRISupportedLogDriver() (bool, error) {
	info, err := d.client.Info()
	if err != nil {
		return false, fmt.Errorf("failed to get docker info: %v", err)
	}
	for _, driver := range criSupportedLogDrivers {
		if info.LoggingDriver == driver {
			return true, nil
		}
	}
	return false, nil
}
