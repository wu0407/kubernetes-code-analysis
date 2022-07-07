/*
Copyright 2014 The Kubernetes Authors.

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

package prober

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/probe"
	execprobe "k8s.io/kubernetes/pkg/probe/exec"
	httpprobe "k8s.io/kubernetes/pkg/probe/http"
	tcpprobe "k8s.io/kubernetes/pkg/probe/tcp"
	"k8s.io/utils/exec"

	"k8s.io/klog"
)

const maxProbeRetries = 3

// Prober helps to check the liveness/readiness/startup of a container.
type prober struct {
	exec execprobe.Prober
	// probe types needs different httprobe instances so they don't
	// share a connection pool which can cause collsions to the
	// same host:port and transient failures. See #49740.
	readinessHTTP httpprobe.Prober
	livenessHTTP  httpprobe.Prober
	startupHTTP   httpprobe.Prober
	tcp           tcpprobe.Prober
	runner        kubecontainer.ContainerCommandRunner

	refManager *kubecontainer.RefManager
	recorder   record.EventRecorder
}

// NewProber creates a Prober, it takes a command runner and
// several container info managers.
func newProber(
	runner kubecontainer.ContainerCommandRunner,
	refManager *kubecontainer.RefManager,
	recorder record.EventRecorder) *prober {

	const followNonLocalRedirects = false
	return &prober{
		exec:          execprobe.New(),
		readinessHTTP: httpprobe.New(followNonLocalRedirects),
		livenessHTTP:  httpprobe.New(followNonLocalRedirects),
		startupHTTP:   httpprobe.New(followNonLocalRedirects),
		tcp:           tcpprobe.New(),
		runner:        runner,
		refManager:    refManager,
		recorder:      recorder,
	}
}

// recordContainerEvent should be used by the prober for all container related events.
// 发送probe事件
func (pb *prober) recordContainerEvent(pod *v1.Pod, container *v1.Container, containerID kubecontainer.ContainerID, eventType, reason, message string, args ...interface{}) {
	var err error
	// 尝试pb.refManager.containerIDToRef获得container id对应的ObjectReference
	ref, hasRef := pb.refManager.GetRef(containerID)
	// 没有获取到，则生成container属于pod的ObjectReference
	if !hasRef {
		ref, err = kubecontainer.GenerateContainerRef(pod, container)
		if err != nil {
			klog.Errorf("Can't make a ref to pod %q, container %v: %v", format.Pod(pod), container.Name, err)
			return
		}
	}
	pb.recorder.Eventf(ref, eventType, reason, message, args...)
}

// probe probes the container.
func (pb *prober) probe(probeType probeType, pod *v1.Pod, status v1.PodStatus, container v1.Container, containerID kubecontainer.ContainerID) (results.Result, error) {
	var probeSpec *v1.Probe
	switch probeType {
	case readiness:
		probeSpec = container.ReadinessProbe
	case liveness:
		probeSpec = container.LivenessProbe
	case startup:
		probeSpec = container.StartupProbe
	default:
		return results.Failure, fmt.Errorf("unknown probe type: %q", probeType)
	}

	// format.Pod(pod)返回 "{podName}_{podNamespace}({podUID})"
	ctrName := fmt.Sprintf("%s:%s", format.Pod(pod), container.Name)
	// 没有定义probeSpec
	if probeSpec == nil {
		klog.Warningf("%s probe for %s is nil", probeType, ctrName)
		return results.Success, nil
	}

	// 执行probe，probe失败则进行重试，最多进行三次重试
	result, output, err := pb.runProbeWithRetries(probeType, probeSpec, pod, status, container, containerID, maxProbeRetries)
	// probe发生错误，或result为probe.Failure或probe.Unknown，说明probe失败
	if err != nil || (result != probe.Success && result != probe.Warning) {
		// Probe failed in one way or another.
		if err != nil {
			klog.V(1).Infof("%s probe for %q errored: %v", probeType, ctrName, err)
			// 发送probe失败事件
			pb.recordContainerEvent(pod, &container, containerID, v1.EventTypeWarning, events.ContainerUnhealthy, "%s probe errored: %v", probeType, err)
		} else { // result != probe.Success
			klog.V(1).Infof("%s probe for %q failed (%v): %s", probeType, ctrName, result, output)
			// result为probe.Failure或probe.Unknown，发送probe失败事件
			pb.recordContainerEvent(pod, &container, containerID, v1.EventTypeWarning, events.ContainerUnhealthy, "%s probe failed: %v", probeType, output)
		}
		return results.Failure, err
	}
	if result == probe.Warning {
		// 发送"ProbeWarning"事件
		pb.recordContainerEvent(pod, &container, containerID, v1.EventTypeWarning, events.ContainerProbeWarning, "%s probe warning: %v", probeType, output)
		klog.V(3).Infof("%s probe for %q succeeded with a warning: %s", probeType, ctrName, output)
	} else {
		klog.V(3).Infof("%s probe for %q succeeded", probeType, ctrName)
	}
	return results.Success, nil
}

// runProbeWithRetries tries to probe the container in a finite loop, it returns the last result
// if it never succeeds.
func (pb *prober) runProbeWithRetries(probeType probeType, p *v1.Probe, pod *v1.Pod, status v1.PodStatus, container v1.Container, containerID kubecontainer.ContainerID, retries int) (probe.Result, string, error) {
	var err error
	var result probe.Result
	var output string
	for i := 0; i < retries; i++ {
		// 执行probe
		result, output, err = pb.runProbe(probeType, p, pod, status, container, containerID)
		if err == nil {
			return result, output, nil
		}
	}
	return result, output, err
}

// buildHeaderMap takes a list of HTTPHeader <name, value> string
// pairs and returns a populated string->[]string http.Header map.
func buildHeader(headerList []v1.HTTPHeader) http.Header {
	headers := make(http.Header)
	for _, header := range headerList {
		headers[header.Name] = append(headers[header.Name], header.Value)
	}
	return headers
}

func (pb *prober) runProbe(probeType probeType, p *v1.Probe, pod *v1.Pod, status v1.PodStatus, container v1.Container, containerID kubecontainer.ContainerID) (probe.Result, string, error) {
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if p.Exec != nil {
		klog.V(4).Infof("Exec-Probe Pod: %v, Container: %v, Command: %v", pod, container, p.Exec.Command)
		// containerCommand里有类似"$(var)"格式，则从envs查找对应的环境变量进行替换var，找不到对应的变量则为替换成空。
		// 如果有"$$"格式转义成"$"
		command := kubecontainer.ExpandContainerCommandOnlyStatic(p.Exec.Command, container.Env)
		// pb.newExecInContainer返回执行函数
		// 调用cri（同步调用）执行cmd，设置执行的timeout，并将stderr append到stdout（也就是说命令输出没有保证顺序）

		// 执行命令的probe，返回probe结果、输出（最大输出10k）、错误
		return pb.exec.Probe(pb.newExecInContainer(container, containerID, command, timeout))
	}
	if p.HTTPGet != nil {
		scheme := strings.ToLower(string(p.HTTPGet.Scheme))
		host := p.HTTPGet.Host
		// 没有probe配置里没有设置host，则设置host为pod ip
		if host == "" {
			host = status.PodIP
		}
		// 从获得container中获得端口，或直接返回端口，并验证端口合法性
		port, err := extractPort(p.HTTPGet.Port, container)
		if err != nil {
			return probe.Unknown, "", err
		}
		path := p.HTTPGet.Path
		klog.V(4).Infof("HTTP-Probe Host: %v://%v, Port: %v, Path: %v", scheme, host, port, path)
		url := formatURL(scheme, host, port, path)
		headers := buildHeader(p.HTTPGet.HTTPHeaders)
		klog.V(4).Infof("HTTP-Probe Headers: %v", headers)
		switch probeType {
		case liveness:
			return pb.livenessHTTP.Probe(url, headers, timeout)
		case startup:
			return pb.startupHTTP.Probe(url, headers, timeout)
		default:
			return pb.readinessHTTP.Probe(url, headers, timeout)
		}
	}
	if p.TCPSocket != nil {
		// 从获得container中获得端口，或直接返回端口，并验证端口合法性
		port, err := extractPort(p.TCPSocket.Port, container)
		if err != nil {
			return probe.Unknown, "", err
		}
		host := p.TCPSocket.Host
		// 如果probe配置里没有设置host，则使用pod ip
		if host == "" {
			host = status.PodIP
		}
		klog.V(4).Infof("TCP-Probe Host: %v, Port: %v, Timeout: %v", host, port, timeout)
		return pb.tcp.Probe(host, port, timeout)
	}
	klog.Warningf("Failed to find probe builder for container: %v", container)
	return probe.Unknown, "", fmt.Errorf("missing probe handler for %s:%s", format.Pod(pod), container.Name)
}

// 从获得container中获得端口，或直接返回端口，并验证端口合法性
func extractPort(param intstr.IntOrString, container v1.Container) (int, error) {
	port := -1
	var err error
	switch param.Type {
	case intstr.Int:
		port = param.IntValue()
	case intstr.String:
		// 从container中找到portName对应的ContainerPort，没有找到port，则尝试字符串转int
		if port, err = findPortByName(container, param.StrVal); err != nil {
			// Last ditch effort - maybe it was an int stored as string?
			if port, err = strconv.Atoi(param.StrVal); err != nil {
				return port, err
			}
		}
	default:
		return port, fmt.Errorf("intOrString had no kind: %+v", param)
	}
	// 验证端口在范围内
	if port > 0 && port < 65536 {
		return port, nil
	}
	return port, fmt.Errorf("invalid port number: %v", port)
}

// findPortByName is a helper function to look up a port in a container by name.
// 从container中找到portName对应的ContainerPort
func findPortByName(container v1.Container, portName string) (int, error) {
	for _, port := range container.Ports {
		if port.Name == portName {
			return int(port.ContainerPort), nil
		}
	}
	return 0, fmt.Errorf("port %s not found", portName)
}

// formatURL formats a URL from args.  For testability.
func formatURL(scheme string, host string, port int, path string) *url.URL {
	u, err := url.Parse(path)
	// Something is busted with the path, but it's too late to reject it. Pass it along as is.
	if err != nil {
		u = &url.URL{
			Path: path,
		}
	}
	u.Scheme = scheme
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	return u
}

type execInContainer struct {
	// run executes a command in a container. Combined stdout and stderr output is always returned. An
	// error is returned if one occurred.
	run    func() ([]byte, error)
	writer io.Writer
}

func (pb *prober) newExecInContainer(container v1.Container, containerID kubecontainer.ContainerID, cmd []string, timeout time.Duration) exec.Cmd {
	return &execInContainer{run: func() ([]byte, error) {
		// 这里的runner为klet.runner，pkg\kubelet\kuberuntime\kuberuntime_container.go里的(m *kubeGenericRuntimeManager) RunInContainer
		// 调用runtime（同步调用）执行cmd，设置执行的timeout，并将stderr append到stdout（也就是说命令输出没有保证顺序）
		// 当timeout为0时候，执行m.runtimeService.ExecSync没有超时时间。
		// 对于dockershim来说（服务端来说），timeout这个参数会传给dockershim，但是dockershim没有使用。
		return pb.runner.RunInContainer(containerID, cmd, timeout)
	}}
}

func (eic *execInContainer) Run() error {
	return nil
}

func (eic *execInContainer) CombinedOutput() ([]byte, error) {
	return eic.run()
}

func (eic *execInContainer) Output() ([]byte, error) {
	return nil, fmt.Errorf("unimplemented")
}

func (eic *execInContainer) SetDir(dir string) {
	//unimplemented
}

func (eic *execInContainer) SetStdin(in io.Reader) {
	//unimplemented
}

func (eic *execInContainer) SetStdout(out io.Writer) {
	eic.writer = out
}

func (eic *execInContainer) SetStderr(out io.Writer) {
	eic.writer = out
}

func (eic *execInContainer) SetEnv(env []string) {
	//unimplemented
}

func (eic *execInContainer) Stop() {
	//unimplemented
}

func (eic *execInContainer) Start() error {
	// 执行命令，data为执行输出
	data, err := eic.run()
	if eic.writer != nil {
		eic.writer.Write(data)
	}
	return err
}

func (eic *execInContainer) Wait() error {
	return nil
}

func (eic *execInContainer) StdoutPipe() (io.ReadCloser, error) {
	return nil, fmt.Errorf("unimplemented")
}

func (eic *execInContainer) StderrPipe() (io.ReadCloser, error) {
	return nil, fmt.Errorf("unimplemented")
}
