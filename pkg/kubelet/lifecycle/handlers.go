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

package lifecycle

import (
	"fmt"
	"net"
	"net/http"
	"strconv"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/security/apparmor"
	utilio "k8s.io/utils/io"
)

const (
	maxRespBodyLength = 10 * 1 << 10 // 10KB
)

type HandlerRunner struct {
	httpGetter       kubetypes.HTTPGetter
	commandRunner    kubecontainer.ContainerCommandRunner
	containerManager podStatusProvider
}

type podStatusProvider interface {
	GetPodStatus(uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error)
}

func NewHandlerRunner(httpGetter kubetypes.HTTPGetter, commandRunner kubecontainer.ContainerCommandRunner, containerManager podStatusProvider) kubecontainer.HandlerRunner {
	return &HandlerRunner{
		httpGetter:       httpGetter,
		commandRunner:    commandRunner,
		containerManager: containerManager,
	}
}

// 执行container.Lifecycle的PreStop和PostStart
func (hr *HandlerRunner) Run(containerID kubecontainer.ContainerID, pod *v1.Pod, container *v1.Container, handler *v1.Handler) (string, error) {
	switch {
	case handler.Exec != nil:
		var msg string
		// TODO(tallclair): Pass a proper timeout value.
		// 如果在container里执行相应的命令，没有执行超时时间，而且是同步调用
		output, err := hr.commandRunner.RunInContainer(containerID, handler.Exec.Command, 0)
		if err != nil {
			msg = fmt.Sprintf("Exec lifecycle hook (%v) for Container %q in Pod %q failed - error: %v, message: %q", handler.Exec.Command, container.Name, format.Pod(pod), err, string(output))
			klog.V(1).Infof(msg)
		}
		return msg, err
	case handler.HTTPGet != nil:
		// 执行http请求，返回response body（最大为10kb），使用空的http.client{}来请求，没有超时时间
		msg, err := hr.runHTTPHandler(pod, container, handler)
		if err != nil {
			msg = fmt.Sprintf("Http lifecycle hook (%s) for Container %q in Pod %q failed - error: %v, message: %q", handler.HTTPGet.Path, container.Name, format.Pod(pod), err, msg)
			klog.V(1).Infof(msg)
		}
		return msg, err
	default:
		err := fmt.Errorf("invalid handler: %v", handler)
		msg := fmt.Sprintf("Cannot run handler: %v", err)
		klog.Errorf(msg)
		return msg, err
	}
}

// resolvePort attempts to turn an IntOrString port reference into a concrete port number.
// If portReference has an int value, it is treated as a literal, and simply returns that value.
// If portReference is a string, an attempt is first made to parse it as an integer.  If that fails,
// an attempt is made to find a port with the same name in the container spec.
// If a port with the same name is found, it's ContainerPort value is returned.  If no matching
// port is found, an error is returned.
func resolvePort(portReference intstr.IntOrString, container *v1.Container) (int, error) {
	if portReference.Type == intstr.Int {
		return portReference.IntValue(), nil
	}
	// 如果端口号是字符串
	portName := portReference.StrVal
	// 尝试对端口名称转成int
	port, err := strconv.Atoi(portName)
	// 如果成功则返回端口号
	if err == nil {
		return port, nil
	}
	// 否则从container中找到相应的端口名称，返回相应的端口号
	for _, portSpec := range container.Ports {
		if portSpec.Name == portName {
			return int(portSpec.ContainerPort), nil
		}
	}
	return -1, fmt.Errorf("couldn't find port: %v in %v", portReference, container)
}

func (hr *HandlerRunner) runHTTPHandler(pod *v1.Pod, container *v1.Container, handler *v1.Handler) (string, error) {
	host := handler.HTTPGet.Host
	if len(host) == 0 {
		// 返回pod的uid、name、namespace、以及所有sanbox状态、ips、所有容器状态
		// 为了获得pod ip就要获取所有sanbox和容器状态，耗费太大（杀鸡用牛刀），可以进行优化
		status, err := hr.containerManager.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
		if err != nil {
			klog.Errorf("Unable to get pod info, event handlers may be invalid.")
			return "", err
		}
		if len(status.IPs) == 0 {
			return "", fmt.Errorf("failed to find networking container: %v", status)
		}
		// pod的主要ip
		host = status.IPs[0]
	}
	var port int
	// port type为string且StrVal为空（即设置port为""），则默认为80
	if handler.HTTPGet.Port.Type == intstr.String && len(handler.HTTPGet.Port.StrVal) == 0 {
		port = 80
	} else {
		var err error
		// 根据设置解析出端口号
		// 1. handler.HTTPGet.Port是数字，直接使用其值
		// 2. handler.HTTPGet.Port是字符串，尝试解析成数字，成功则使用这个值。否则从container的port定义中查找端口号
		port, err = resolvePort(handler.HTTPGet.Port, container)
		if err != nil {
			return "", err
		}
	}
	url := fmt.Sprintf("http://%s/%s", net.JoinHostPort(host, strconv.Itoa(port)), handler.HTTPGet.Path)
	resp, err := hr.httpGetter.Get(url)
	return getHttpRespBody(resp), err
}

func getHttpRespBody(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	defer resp.Body.Close()
	// 最大读取10kb大小
	bytes, err := utilio.ReadAtMost(resp.Body, maxRespBodyLength)
	if err == nil || err == utilio.ErrLimitReached {
		return string(bytes)
	}
	return ""
}

func NewAppArmorAdmitHandler(validator apparmor.Validator) PodAdmitHandler {
	return &appArmorAdmitHandler{
		Validator: validator,
	}
}

type appArmorAdmitHandler struct {
	apparmor.Validator
}

// 验证pod的container的"AppArmor" profile是合法且已加载
func (a *appArmorAdmitHandler) Admit(attrs *PodAdmitAttributes) PodAdmitResult {
	// If the pod is already running or terminated, no need to recheck AppArmor.
	// pod处于"Pending"状态才需要检查AppArmor
	if attrs.Pod.Status.Phase != v1.PodPending {
		return PodAdmitResult{Admit: true}
	}

	// 验证pod的所有container的profile是合法且已加载
	err := a.Validate(attrs.Pod)
	if err == nil {
		return PodAdmitResult{Admit: true}
	}
	return PodAdmitResult{
		Admit:   false,
		Reason:  "AppArmor",
		Message: fmt.Sprintf("Cannot enforce AppArmor: %v", err),
	}
}

func NewNoNewPrivsAdmitHandler(runtime kubecontainer.Runtime) PodAdmitHandler {
	return &noNewPrivsAdmitHandler{
		Runtime: runtime,
	}
}

type noNewPrivsAdmitHandler struct {
	kubecontainer.Runtime
}

// admit不通过，下面条件都要满足：
// 1. pod处于"Pending"状态
// 2. pod.Spec.Containers有一个container定义了SecurityContext且配置了SecurityContext.AllowPrivilegeEscalation且值为false
// 3. runtime为docker
// 4. docker的apiserver版本小于1.23.0
func (a *noNewPrivsAdmitHandler) Admit(attrs *PodAdmitAttributes) PodAdmitResult {
	// If the pod is already running or terminated, no need to recheck NoNewPrivs.
	// pod处于"Pending"状态才需要检查NoNewPrivs
	if attrs.Pod.Status.Phase != v1.PodPending {
		return PodAdmitResult{Admit: true}
	}

	// If the containers in a pod do not require no-new-privs, admit it.
	// 只要pod.Spec.Containers有一个container定义了SecurityContext且配置了SecurityContext.AllowPrivilegeEscalation且值为false，则返回true
	// 其他情况返回false（比如所有container都没有配置SecurityContext、比如配置SecurityContext.AllowPrivilegeEscalation为true）
	if !noNewPrivsRequired(attrs.Pod) {
		return PodAdmitResult{Admit: true}
	}

	// Always admit runtimes except docker.
	// 非docker的runtime允许
	if a.Runtime.Type() != kubetypes.DockerContainerRuntime {
		return PodAdmitResult{Admit: true}
	}

	// Make sure docker api version is valid.
	// 获取docker的ApiVersion
	rversion, err := a.Runtime.APIVersion()
	if err != nil {
		return PodAdmitResult{
			Admit:   false,
			Reason:  "NoNewPrivs",
			Message: fmt.Sprintf("Cannot enforce NoNewPrivs: %v", err),
		}
	}
	v, err := rversion.Compare("1.23.0")
	if err != nil {
		return PodAdmitResult{
			Admit:   false,
			Reason:  "NoNewPrivs",
			Message: fmt.Sprintf("Cannot enforce NoNewPrivs: %v", err),
		}
	}
	// If the version is less than 1.23 it will return -1 above.
	// docker ApiVersion小于1.23
	if v == -1 {
		return PodAdmitResult{
			Admit:   false,
			Reason:  "NoNewPrivs",
			Message: fmt.Sprintf("Cannot enforce NoNewPrivs: docker runtime API version %q must be greater than or equal to 1.23", rversion.String()),
		}
	}

	return PodAdmitResult{Admit: true}
}

// 只要pod.Spec.Containers有一个container定义了SecurityContext且配置了SecurityContext.AllowPrivilegeEscalation且值为false，则返回true
// 其他情况返回false
func noNewPrivsRequired(pod *v1.Pod) bool {
	// Iterate over pod containers and check if we added no-new-privs.
	for _, c := range pod.Spec.Containers {
		// 配置了SecurityContext且配置了c.SecurityContext.AllowPrivilegeEscalation且值为false，则返回true
		if c.SecurityContext != nil && c.SecurityContext.AllowPrivilegeEscalation != nil && !*c.SecurityContext.AllowPrivilegeEscalation {
			return true
		}
	}
	return false
}

func NewProcMountAdmitHandler(runtime kubecontainer.Runtime) PodAdmitHandler {
	return &procMountAdmitHandler{
		Runtime: runtime,
	}
}

type procMountAdmitHandler struct {
	kubecontainer.Runtime
}

// admit不通过，下面条件都要满足：
// 1. pod处于"Pending"状态
// 2. pod.Spec.Containers有一个container定义了SecurityContext且配置了SecurityContext.ProcMount的值不是"Default"
// 3. runtime为docker
// 4. docker的apiserver版本小于1.38.0 
func (a *procMountAdmitHandler) Admit(attrs *PodAdmitAttributes) PodAdmitResult {
	// If the pod is already running or terminated, no need to recheck NoNewPrivs.
	// pod处于"Pending"状态才需要检查
	if attrs.Pod.Status.Phase != v1.PodPending {
		return PodAdmitResult{Admit: true}
	}

	// If the containers in a pod only need the default ProcMountType, admit it.
	// pod.Spec.Containers的SecurityContext都未设置，或设置c.SecurityContext.ProcMount的值是"Default"
	if procMountIsDefault(attrs.Pod) {
		return PodAdmitResult{Admit: true}
	}

	// Always admit runtimes except docker.
	// 非docker的runtime允许
	if a.Runtime.Type() != kubetypes.DockerContainerRuntime {
		return PodAdmitResult{Admit: true}
	}

	// Make sure docker api version is valid.
	// Merged in https://github.com/moby/moby/pull/36644
	rversion, err := a.Runtime.APIVersion()
	if err != nil {
		return PodAdmitResult{
			Admit:   false,
			Reason:  "ProcMount",
			Message: fmt.Sprintf("Cannot enforce ProcMount: %v", err),
		}
	}
	v, err := rversion.Compare("1.38.0")
	if err != nil {
		return PodAdmitResult{
			Admit:   false,
			Reason:  "ProcMount",
			Message: fmt.Sprintf("Cannot enforce ProcMount: %v", err),
		}
	}
	// If the version is less than 1.38 it will return -1 above.
	// docker的apiversion小于1.38.0
	if v == -1 {
		return PodAdmitResult{
			Admit:   false,
			Reason:  "ProcMount",
			Message: fmt.Sprintf("Cannot enforce ProcMount: docker runtime API version %q must be greater than or equal to 1.38", rversion.String()),
		}
	}

	return PodAdmitResult{Admit: true}
}

// 只要pod.Spec.Containers里有一个container，设置c.SecurityContext.ProcMount的值不是"Default"，则返回false
// 否则返回true
func procMountIsDefault(pod *v1.Pod) bool {
	// Iterate over pod containers and check if we are using the DefaultProcMountType
	// for all containers.
	for _, c := range pod.Spec.Containers {
		if c.SecurityContext != nil {
			if c.SecurityContext.ProcMount != nil && *c.SecurityContext.ProcMount != v1.DefaultProcMount {
				return false
			}
		}
	}

	return true
}
