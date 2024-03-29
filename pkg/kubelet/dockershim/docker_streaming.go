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
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"k8s.io/client-go/tools/remotecommand"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
	"k8s.io/kubernetes/pkg/kubelet/util/ioutils"
	utilexec "k8s.io/utils/exec"

	"k8s.io/kubernetes/pkg/kubelet/dockershim/libdocker"
)

type streamingRuntime struct {
	client      libdocker.Interface
	execHandler ExecHandler
}

var _ streaming.Runtime = &streamingRuntime{}

const maxMsgSize = 1024 * 1024 * 16

// 执行InspectContainer，检测容器是否在运行
// 执行exec命令，返回后，每2s检测exec是否退出，只等待8s检测exec是否退出。启动一个goroutine，在exec启动之后从resize chan中读取消息，执行ResizeExecTTY。没有超时时间
func (r *streamingRuntime) Exec(containerID string, cmd []string, in io.Reader, out, err io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	return r.exec(containerID, cmd, in, out, err, tty, resize, 0)
}

// Internal version of Exec adds a timeout.
func (r *streamingRuntime) exec(containerID string, cmd []string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize, timeout time.Duration) error {
	// 执行InspectContainer，检测容器是否在运行
	container, err := checkContainerStatus(r.client, containerID)
	if err != nil {
		return err
	}
	// 执行exec命令，返回后，每2s检测exec是否退出，只等待8s检测exec是否退出。启动一个goroutine，在exec启动之后从resize chan中读取消息，执行ResizeExecTTY
	return r.execHandler.ExecInContainer(r.client, container, cmd, in, out, errw, tty, resize, timeout)
}

func (r *streamingRuntime) Attach(containerID string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	// 执行InspectContainer，检测容器是否在运行
	_, err := checkContainerStatus(r.client, containerID)
	if err != nil {
		return err
	}

	// 启动一个goroutine，读取resize里的消息，执行resizeFunc（调用docker客户端进行重置container tty）
	// 调用docker客户端执行attach
	return attachContainer(r.client, containerID, in, out, errw, tty, resize)
}

func (r *streamingRuntime) PortForward(podSandboxID string, port int32, stream io.ReadWriteCloser) error {
	// 校验端口合法性
	if port < 0 || port > math.MaxUint16 {
		return fmt.Errorf("invalid port %d", port)
	}
	// 查询容器是否存在
	// 容器不在运行状态，直接返回错误
	// 从stream中读取数据作为stdin，发送stdout数据到stream
	// 执行nsenter -t {container pid} -n socat - TCP4:localhost:{port}
	return r.portForward(podSandboxID, port, stream)
}

// ExecSync executes a command in the container, and returns the stdout output.
// If command exits with a non-zero exit code, an error is returned.
func (ds *dockerService) ExecSync(_ context.Context, req *runtimeapi.ExecSyncRequest) (*runtimeapi.ExecSyncResponse, error) {
	timeout := time.Duration(req.Timeout) * time.Second
	var stdoutBuffer, stderrBuffer bytes.Buffer
	// 在没有tty和没有resize tty的情况下，执行exec命令（同步调用），返回后，每2s检测exec是否退出，只等待8s检测exec是否退出。
	// timeout参数没有使用，即没有超时时间
	err := ds.streamingRuntime.exec(req.ContainerId, req.Cmd,
		nil, // in
		ioutils.WriteCloserWrapper(ioutils.LimitWriter(&stdoutBuffer, maxMsgSize)),
		ioutils.WriteCloserWrapper(ioutils.LimitWriter(&stderrBuffer, maxMsgSize)),
		false, // tty
		nil,   // resize
		timeout)

	var exitCode int32
	if err != nil {
		exitError, ok := err.(utilexec.ExitError)
		if !ok {
			return nil, err
		}

		exitCode = int32(exitError.ExitStatus())
	}
	return &runtimeapi.ExecSyncResponse{
		Stdout:   stdoutBuffer.Bytes(),
		Stderr:   stderrBuffer.Bytes(),
		ExitCode: exitCode,
	}, nil
}

// Exec prepares a streaming endpoint to execute a command in the container, and returns the address.
func (ds *dockerService) Exec(_ context.Context, req *runtimeapi.ExecRequest) (*runtimeapi.ExecResponse, error) {
	if ds.streamingServer == nil {
		return nil, streaming.NewErrorStreamingDisabled("exec")
	}
	// 执行InspectContainer，检测容器是否在运行
	_, err := checkContainerStatus(ds.client, req.ContainerId)
	if err != nil {
		return nil, err
	}
	// 验证请求的合法性
	//   containerId不能为空
	//   不能同时启用tty和stderr
	//   stdin、stdout、stderr必须有一个启用
	// 缓存中添加这次请求
	// 返回构建好的url，比如"http://localhost:36589/exec/{token}"
	return ds.streamingServer.GetExec(req)
}

// Attach prepares a streaming endpoint to attach to a running container, and returns the address.
// 验证请求的合法性
//   containerId不能为空
//   不能同时启用tty和stderr
//   stdin、stdout、stderr必须有一个启用
// 缓存中添加这次请求
// 返回构建好的url，比如"http://localhost:36589/attach/{token}"
func (ds *dockerService) Attach(_ context.Context, req *runtimeapi.AttachRequest) (*runtimeapi.AttachResponse, error) {
	if ds.streamingServer == nil {
		return nil, streaming.NewErrorStreamingDisabled("attach")
	}
	_, err := checkContainerStatus(ds.client, req.ContainerId)
	if err != nil {
		return nil, err
	}
	return ds.streamingServer.GetAttach(req)
}

// PortForward prepares a streaming endpoint to forward ports from a PodSandbox, and returns the address.
// 验证请求的合法性
//   containerId不能为空
//   不能同时启用tty和stderr
//   stdin、stdout、stderr必须有一个启用
// 缓存中添加这次请求
// 返回构建好的url，比如"http://localhost:36589/portforward/{token}"
func (ds *dockerService) PortForward(_ context.Context, req *runtimeapi.PortForwardRequest) (*runtimeapi.PortForwardResponse, error) {
	if ds.streamingServer == nil {
		return nil, streaming.NewErrorStreamingDisabled("port forward")
	}
	// 执行InspectContainer，检测容器是否在运行
	_, err := checkContainerStatus(ds.client, req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	// TODO(tallclair): Verify that ports are exposed.
	return ds.streamingServer.GetPortForward(req)
}

// 执行InspectContainer，检测容器是否在运行
func checkContainerStatus(client libdocker.Interface, containerID string) (*dockertypes.ContainerJSON, error) {
	container, err := client.InspectContainer(containerID)
	if err != nil {
		return nil, err
	}
	if !container.State.Running {
		return nil, fmt.Errorf("container not running (%s)", container.ID)
	}
	return container, nil
}

// 启动一个goroutine，读取resize里的消息，执行resizeFunc（调用docker客户端进行重置container tty）
// 调用docker客户端执行attach
func attachContainer(client libdocker.Interface, containerID string, stdin io.Reader, stdout, stderr io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	// Have to start this before the call to client.AttachToContainer because client.AttachToContainer is a blocking
	// call :-( Otherwise, resize events don't get processed and the terminal never resizes.
	// 启动一个goroutine，读取resize里的消息，执行resizeFunc（调用docker客户端进行重置container tty）
	kubecontainer.HandleResizing(resize, func(size remotecommand.TerminalSize) {
		client.ResizeContainerTTY(containerID, uint(size.Height), uint(size.Width))
	})

	// TODO(random-liu): Do we really use the *Logs* field here?
	opts := dockertypes.ContainerAttachOptions{
		Stream: true,
		Stdin:  stdin != nil,
		Stdout: stdout != nil,
		Stderr: stderr != nil,
	}
	sopts := libdocker.StreamOptions{
		InputStream:  stdin,
		OutputStream: stdout,
		ErrorStream:  stderr,
		RawTerminal:  tty,
	}
	return client.AttachToContainer(containerID, opts, sopts)
}
