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

package sysctl

import (
	"fmt"
	"strings"

	"k8s.io/kubernetes/pkg/apis/core/validation"
	policyvalidation "k8s.io/kubernetes/pkg/apis/policy/validation"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
)

const (
	AnnotationInvalidReason = "InvalidSysctlAnnotation"
	ForbiddenReason         = "SysctlForbidden"
)

// patternWhitelist takes a list of sysctls or sysctl patterns (ending in *) and
// checks validity via a sysctl and prefix map, rejecting those which are not known
// to be namespaced.
type patternWhitelist struct {
	sysctls  map[string]Namespace
	prefixes map[string]Namespace
}

var _ lifecycle.PodAdmitHandler = &patternWhitelist{}

// NewWhitelist creates a new Whitelist from a list of sysctls and sysctl pattern (ending in *).
// patterns内置的是
// "kernel.shm_rmid_forced",
// "net.ipv4.ip_local_port_range",
// "net.ipv4.tcp_syncookies",
// "net.ipv4.ping_group_range",
// patterns只能是（都满足关系）
// 1. 长度超过253，或不匹配"^([a-z0-9]([-_a-z0-9]*[a-z0-9])?\\.)*([a-z0-9][-_a-z0-9]*)?[a-z0-9*]$"
// 2. 完整的kernel.sem*、前缀kernel.shm{...}*、kernel.msg{...}*、fs.mqueue.{...}* 、net.{...}*，或完整的kernel.sem、前缀kernel.shm、前缀kernel.msg、前缀fs.mqueue. 、前缀net.
func NewWhitelist(patterns []string) (*patternWhitelist, error) {
	w := &patternWhitelist{
		sysctls:  map[string]Namespace{},
		prefixes: map[string]Namespace{},
	}

	for _, s := range patterns {
		// 长度超过253，或不匹配"^([a-z0-9]([-_a-z0-9]*[a-z0-9])?\\.)*([a-z0-9][-_a-z0-9]*)?[a-z0-9*]$"
		if !policyvalidation.IsValidSysctlPattern(s) {
			return nil, fmt.Errorf("sysctl %q must have at most %d characters and match regex %s",
				s,
				validation.SysctlMaxLength,
				policyvalidation.SysctlPatternFmt,
			)
		}
		// 完整的kernel.sem*、前缀kernel.shm{...}*、kernel.msg{...}*、fs.mqueue.{...}* 、net.{...}*
		if strings.HasSuffix(s, "*") {
			prefix := s[:len(s)-1]
			// val是"kernel.sem"，返回"ipc"
			// val包含"kernel.shm"、kernel.msg"、"fs.mqueue.前缀，"返回"ipc"
			// val包含前缀"net."，则返回"net"
			// 其他情况返回空
			ns := NamespacedBy(prefix)
			if ns == UnknownNamespace {
				return nil, fmt.Errorf("the sysctls %q are not known to be namespaced", s)
			}
			w.prefixes[prefix] = ns
		// 完整的kernel.sem、前缀kernel.shm、前缀kernel.msg、前缀fs.mqueue. 、前缀net.
		} else {
			ns := NamespacedBy(s)
			if ns == UnknownNamespace {
				return nil, fmt.Errorf("the sysctl %q are not known to be namespaced", s)
			}
			w.sysctls[s] = ns
		}
	}
	return w, nil
}

// validateSysctl checks that a sysctl is whitelisted because it is known
// to be namespaced by the Linux kernel. Note that being whitelisted is required, but not
// sufficient: the container runtime might have a stricter check and refuse to launch a pod.
//
// The parameters hostNet and hostIPC are used to forbid sysctls for pod sharing the
// respective namespaces with the host. This check is only possible for sysctls on
// the static default whitelist, not those on the custom whitelist provided by the admin.
// 三种情况返回错误
// 1. sysctl的namespace group为"ipc"且设置了hostIPC共享，返回错误
// 2. sysctl的namespace group为"net"且设置了hostNet共享，返回错误
// 3. sysctl不在白名单里
func (w *patternWhitelist) validateSysctl(sysctl string, hostNet, hostIPC bool) error {
	nsErrorFmt := "%q not allowed with host %s enabled"
	// 检测完整匹配
	if ns, found := w.sysctls[sysctl]; found {
		// sysctl的namespace group为"ipc"且设置了hostIPC共享，返回错误
		if ns == IpcNamespace && hostIPC {
			return fmt.Errorf(nsErrorFmt, sysctl, ns)
		}
		// sysctl的namespace group为"net"且设置了hostNet共享，返回错误
		if ns == NetNamespace && hostNet {
			return fmt.Errorf(nsErrorFmt, sysctl, ns)
		}
		return nil
	}
	// 检测前缀
	for p, ns := range w.prefixes {
		if strings.HasPrefix(sysctl, p) {
			// sysctl的namespace group为"ipc"且设置了hostIPC共享，返回错误
			if ns == IpcNamespace && hostIPC {
				return fmt.Errorf(nsErrorFmt, sysctl, ns)
			}
			// sysctl的namespace group为"net"且设置了hostNet共享，返回错误
			if ns == NetNamespace && hostNet {
				return fmt.Errorf(nsErrorFmt, sysctl, ns)
			}
			return nil
		}
	}
	return fmt.Errorf("%q not whitelisted", sysctl)
}

// Admit checks that all sysctls given in pod's security context
// are valid according to the whitelist.
// 三种情况不通过admit
// 1. sysctl的namespace group为"ipc"且设置了hostIPC共享
// 2. sysctl的namespace group为"net"且设置了hostNet共享
// 3. sysctl不在白名单里
func (w *patternWhitelist) Admit(attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	pod := attrs.Pod
	// pod没有设置SecurityContext或SecurityContext.Sysctls，直接admit通过
	if pod.Spec.SecurityContext == nil || len(pod.Spec.SecurityContext.Sysctls) == 0 {
		return lifecycle.PodAdmitResult{
			Admit: true,
		}
	}

	var hostNet, hostIPC bool
	if pod.Spec.SecurityContext != nil {
		hostNet = pod.Spec.HostNetwork
		hostIPC = pod.Spec.HostIPC
	}
	for _, s := range pod.Spec.SecurityContext.Sysctls {
		// 三种情况返回错误
		// 1. sysctl的namespace group为"ipc"且设置了hostIPC共享，返回错误
		// 2. sysctl的namespace group为"net"且设置了hostNet共享，返回错误
		// 3. sysctl不在白名单里
		if err := w.validateSysctl(s.Name, hostNet, hostIPC); err != nil {
			return lifecycle.PodAdmitResult{
				Admit:   false,
				Reason:  ForbiddenReason,
				Message: fmt.Sprintf("forbidden sysctl: %v", err),
			}
		}
	}

	return lifecycle.PodAdmitResult{
		Admit: true,
	}
}
