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
	"fmt"

	"k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/kubernetes/pkg/security/apparmor"
	"k8s.io/kubernetes/pkg/securitycontext"
)

// determineEffectiveSecurityContext gets container's security context from v1.Pod and v1.Container.
// 根据pod和container生成*runtimeapi.LinuxContainerSecurityContext
// 包括Capabilities、SELinuxOptions、RunAsUser、RunAsGroup、Privileged、ReadOnlyRootfs、MaskedPaths、ReadonlyPaths、SeccompProfilePath、ApparmorProfile、RunAsUsername、NamespaceOptions、SupplementalGroups、NoNewPrivs字段
func (m *kubeGenericRuntimeManager) determineEffectiveSecurityContext(pod *v1.Pod, container *v1.Container, uid *int64, username string) *runtimeapi.LinuxContainerSecurityContext {
	// 将pod里的SecurityContext和container里SecurityContext进行合并，以container里SecurityContext为主（全部字段）。
	// 如果container要使用pod.Spec.SecurityContext，则只会使用SELinuxOptions、WindowsOptions、RunAsUser、RunAsGroup、RunAsNonRoot字段
	effectiveSc := securitycontext.DetermineEffectiveSecurityContext(pod, container)
	// 将v1.SecurityContext转成runtimeapi.SecurityContext
	// 只用了v1.SecurityContext里的Capabilities、SELinuxOptions、RunAsUser、RunAsGroup、Privileged、ReadOnlyRootFilesystem，而WindowsOptions、RunAsNonRoot、AllowPrivilegeEscalation、ProcMount没有使用
	// 后面代码会用到effectiveSc里的AllowPrivilegeEscalation、ProcMount
	synthesized := convertToRuntimeSecurityContext(effectiveSc)
	// 如果pod和container都没有定义SecurityContext，则添加MaskedPaths和ReadonlyPaths为默认的路径。让synthesized不再为nil
	// MaskedPaths为
	// 则/proc额外挂载这些
	// "/proc/acpi",
	// "/proc/kcore",
	// "/proc/keys",
	// "/proc/latency_stats",
	// "/proc/timer_list",
	// "/proc/timer_stats",
	// "/proc/sched_debug",
	// "/proc/scsi",
	// "/sys/firmware",
	//
	// ReadonlyPaths为：
	// "/proc/asound",
	// "/proc/bus",
	// "/proc/fs",
	// "/proc/irq",
	// "/proc/sys",
	// "/proc/sysrq-trigger",
	if synthesized == nil {
		// 让synthesized不再为nil，后面执行不会panic
		synthesized = &runtimeapi.LinuxContainerSecurityContext{
			// v1.ProcMountType转成/proc需要额外挂载路径
			MaskedPaths:   securitycontext.ConvertToRuntimeMaskedPaths(effectiveSc.ProcMount),
			// v1.ProcMountType转成/proc下readonly路径
			ReadonlyPaths: securitycontext.ConvertToRuntimeReadonlyPaths(effectiveSc.ProcMount),
		}
	}

	// set SeccompProfilePath.
	// 从pod.annotations["seccomp.security.alpha.kubernetes.io/pod"]或pod.annotations["container.seccomp.security.alpha.kubernetes.io/"+{containerName}]里获得seccompProfile路径
	// 如果pod.annotation的值里包含"localhost/"前缀，则替换为"localhost//var/lib/kubelet/seccomp"+ filepath.FromSlash({name去除"localhost/"})
	synthesized.SeccompProfilePath = m.getSeccompProfileFromAnnotations(pod.Annotations, container.Name)

	// set ApparmorProfile.
	// 返回pod.annotations["container.apparmor.security.beta.kubernetes.io/"+containerName]值
	synthesized.ApparmorProfile = apparmor.GetProfileNameFromPodAnnotations(pod.Annotations, container.Name)

	// set RunAsUser.
	if synthesized.RunAsUser == nil {
		if uid != nil {
			synthesized.RunAsUser = &runtimeapi.Int64Value{Value: *uid}
		}
		synthesized.RunAsUsername = username
	}

	// set namespace options and supplemental groups.
	// 返回pod的ipc、网络、pid模式
	// 根据pod.Spec.HostIP，返回pod的ipc模式
	// 根据pod.Spec.HostNetwork，返回网络模式
	// 根据pod.Spec.HostPID和pod.Spec.ShareProcessNamespace，返回pid模式
	synthesized.NamespaceOptions = namespacesForPod(pod)
	podSc := pod.Spec.SecurityContext
	// pod.Spec.SecurityContext不为nil，synthesized.SupplementalGroups为pod.Spec.SecurityContext.FSGroup和pod.Spec.SecurityContext.SupplementalGroups并集
	if podSc != nil {
		// synthesized.SupplementalGroups添加pod.Spec.SecurityContext.FSGroup
		if podSc.FSGroup != nil {
			synthesized.SupplementalGroups = append(synthesized.SupplementalGroups, int64(*podSc.FSGroup))
		}

		// synthesized.SupplementalGroups添加pod.Spec.SecurityContext.SupplementalGroups
		if podSc.SupplementalGroups != nil {
			for _, sg := range podSc.SupplementalGroups {
				synthesized.SupplementalGroups = append(synthesized.SupplementalGroups, int64(sg))
			}
		}
	}
	// pod的挂载pv里annotation["pv.beta.kubernetes.io/gid"]里定义的gid（不在pod.Spec.SecurityContext.SupplementalGroups里），添加到synthesized.SupplementalGroups
	if groups := m.runtimeHelper.GetExtraSupplementalGroupsForPod(pod); len(groups) > 0 {
		synthesized.SupplementalGroups = append(synthesized.SupplementalGroups, groups...)
	}

	// effectiveSc.AllowPrivilegeEscalation为false，返回true，其他情况（effectiveSc为nil或effectiveSc没有定义了AllowPrivilegeEscalation，或effectiveSc.AllowPrivilegeEscalation为true）返回false
	synthesized.NoNewPrivs = securitycontext.AddNoNewPrivileges(effectiveSc)

	// v1.ProcMountType转成/proc需要额外挂载路径
	synthesized.MaskedPaths = securitycontext.ConvertToRuntimeMaskedPaths(effectiveSc.ProcMount)
	// v1.ProcMountType转成/proc下readonly路径
	synthesized.ReadonlyPaths = securitycontext.ConvertToRuntimeReadonlyPaths(effectiveSc.ProcMount)

	return synthesized
}

// verifyRunAsNonRoot verifies RunAsNonRoot.
// 验证传入的参数（镜像里的uid和username）和最终container应用到SecurityContext.RunAsUser，是否符合最终container应用到SecurityContext.RunAsNonRoot设置
func verifyRunAsNonRoot(pod *v1.Pod, container *v1.Container, uid *int64, username string) error {
	// 将pod里的SecurityContext和container里SecurityContext进行合并，以container里SecurityContext为主。
	// 如果container要使用pod.Spec.SecurityContext，则只会使用SELinuxOptions、WindowsOptions、RunAsUser、RunAsGroup、RunAsNonRoot字段
	effectiveSc := securitycontext.DetermineEffectiveSecurityContext(pod, container)
	// If the option is not set, or if running as root is allowed, return nil.
	// pod和container都没有定义SecurityContext，或最终container应用到SecurityContext.RunAsNonRoot为nil（pod和container的SecurityContext.RunAsNonRoot都为nil），或最终container应用到SecurityContext.RunAsNonRoot为false，则返回nil，说明能够运行在root
	if effectiveSc == nil || effectiveSc.RunAsNonRoot == nil || !*effectiveSc.RunAsNonRoot {
		return nil
	}

	// 最终container应用到SecurityContext.RunAsNonRoot为true

	// 最终container应用到SecurityContext.RunAsUser不为nil，检查SecurityContext.RunAsUser是否为0（0为root的uid），为0说明运行在root的uid，则返回错误。否则返回nil
	if effectiveSc.RunAsUser != nil {
		// SecurityContext.RunAsUser为0，说明运行在root的uid，则返回错误
		if *effectiveSc.RunAsUser == 0 {
			return fmt.Errorf("container's runAsUser breaks non-root policy")
		}
		return nil
	}

	// 最终container应用到SecurityContext.RunAsUser为nil，则检查传入的uid（镜像里的uid）和username（镜像里的用户名）
	switch {
	// uid不为nil，则uid为0，说明运行在root的uid，则返回错误
	case uid != nil && *uid == 0:
		return fmt.Errorf("container has runAsNonRoot and image will run as root")
	// uid为nil，用户名不为空，则返回错误（因为无法根据用户名判断是否为root，root用户可以是任意字符串，但是uid都为0）
	case uid == nil && len(username) > 0:
		return fmt.Errorf("container has runAsNonRoot and image has non-numeric user (%s), cannot verify user is non-root", username)
	// uid不为0，或uid为nil且username为空
	default:
		return nil
	}
}

// convertToRuntimeSecurityContext converts v1.SecurityContext to runtimeapi.SecurityContext.
// 将v1.SecurityContext转成runtimeapi.SecurityContext
// 只用了v1.SecurityContext里的Capabilities、SELinuxOptions、RunAsUser、RunAsGroup、Privileged、ReadOnlyRootFilesystem，而WindowsOptions、RunAsNonRoot、AllowPrivilegeEscalation、ProcMount没有使用
func convertToRuntimeSecurityContext(securityContext *v1.SecurityContext) *runtimeapi.LinuxContainerSecurityContext {
	if securityContext == nil {
		return nil
	}

	sc := &runtimeapi.LinuxContainerSecurityContext{
		// 将v1.Capabilities转成runtimeapi.Capability
		Capabilities:   convertToRuntimeCapabilities(securityContext.Capabilities),
		// 将v1.SELinuxOptions转成runtimeapi.SELinuxOption
		SelinuxOptions: convertToRuntimeSELinuxOption(securityContext.SELinuxOptions),
	}
	if securityContext.RunAsUser != nil {
		sc.RunAsUser = &runtimeapi.Int64Value{Value: int64(*securityContext.RunAsUser)}
	}
	if securityContext.RunAsGroup != nil {
		sc.RunAsGroup = &runtimeapi.Int64Value{Value: int64(*securityContext.RunAsGroup)}
	}
	if securityContext.Privileged != nil {
		sc.Privileged = *securityContext.Privileged
	}
	if securityContext.ReadOnlyRootFilesystem != nil {
		sc.ReadonlyRootfs = *securityContext.ReadOnlyRootFilesystem
	}

	return sc
}

// convertToRuntimeSELinuxOption converts v1.SELinuxOptions to runtimeapi.SELinuxOption.
// 将v1.SELinuxOptions转成runtimeapi.SELinuxOption.
func convertToRuntimeSELinuxOption(opts *v1.SELinuxOptions) *runtimeapi.SELinuxOption {
	if opts == nil {
		return nil
	}

	return &runtimeapi.SELinuxOption{
		User:  opts.User,
		Role:  opts.Role,
		Type:  opts.Type,
		Level: opts.Level,
	}
}

// convertToRuntimeCapabilities converts v1.Capabilities to runtimeapi.Capability.
// 将v1.Capabilities转成runtimeapi.Capability.
func convertToRuntimeCapabilities(opts *v1.Capabilities) *runtimeapi.Capability {
	if opts == nil {
		return nil
	}

	capabilities := &runtimeapi.Capability{
		AddCapabilities:  make([]string, len(opts.Add)),
		DropCapabilities: make([]string, len(opts.Drop)),
	}
	for index, value := range opts.Add {
		capabilities.AddCapabilities[index] = string(value)
	}
	for index, value := range opts.Drop {
		capabilities.DropCapabilities[index] = string(value)
	}

	return capabilities
}
