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

package dockershim

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/blang/semver"
	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	"k8s.io/api/core/v1"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

func DefaultMemorySwap() int64 {
	return 0
}

// 返回"seccomp"的SecurityOpts，比如seccompProfile为空，返回[]string{"seccomp=unconfined"}
func (ds *dockerService) getSecurityOpts(seccompProfile string, separator rune) ([]string, error) {
	// Apply seccomp options.
	// 返回"seccomp"的SecurityOpts，比如seccompProfile为空，返回[]string{"seccomp=unconfined"}
	seccompSecurityOpts, err := getSeccompSecurityOpts(seccompProfile, separator)
	if err != nil {
		return nil, fmt.Errorf("failed to generate seccomp security options for container: %v", err)
	}

	return seccompSecurityOpts, nil
}

// seccompProfile为空""或unconfined，返回[]dockerOpt{{"seccomp", "unconfined", ""}}
// "runtime/default"或"docker/default",返回nil
// "localhost/<filepath>" 返回[]dockerOpt{{"seccomp","<file content>","<filepath>(md5:<md5sum>)"}}
func getSeccompDockerOpts(seccompProfile string) ([]dockerOpt, error) {
	if seccompProfile == "" || seccompProfile == "unconfined" {
		// return early the default
		// []dockerOpt{{"seccomp", "unconfined", ""}}
		return defaultSeccompOpt, nil
	}

	if seccompProfile == v1.SeccompProfileRuntimeDefault || seccompProfile == v1.DeprecatedSeccompProfileDockerDefault {
		// return nil so docker will load the default seccomp profile
		return nil, nil
	}

	if !strings.HasPrefix(seccompProfile, "localhost/") {
		return nil, fmt.Errorf("unknown seccomp profile option: %s", seccompProfile)
	}

	// get the full path of seccomp profile when prefixed with 'localhost/'.
	fname := strings.TrimPrefix(seccompProfile, "localhost/")
	if !filepath.IsAbs(fname) {
		return nil, fmt.Errorf("seccomp profile path must be absolute, but got relative path %q", fname)
	}
	file, err := ioutil.ReadFile(filepath.FromSlash(fname))
	if err != nil {
		return nil, fmt.Errorf("cannot load seccomp profile %q: %v", fname, err)
	}

	b := bytes.NewBuffer(nil)
	if err := json.Compact(b, file); err != nil {
		return nil, err
	}
	// Rather than the full profile, just put the filename & md5sum in the event log.
	msg := fmt.Sprintf("%s(md5:%x)", fname, md5.Sum(file))

	return []dockerOpt{{"seccomp", b.String(), msg}}, nil
}

// getSeccompSecurityOpts gets container seccomp options from container seccomp profile.
// It is an experimental feature and may be promoted to official runtime api in the future.
// 返回"seccomp"的SecurityOpts，比如[]string{"seccomp=unconfined"}
func getSeccompSecurityOpts(seccompProfile string, separator rune) ([]string, error) {
	// seccompProfile为空""或unconfined，返回[]dockerOpt{{"seccomp", "unconfined", ""}}
	// "runtime/default"或"docker/default",返回nil
	// "localhost/<filepath>" 返回[]dockerOpt{{"seccomp","<file content>","<filepath>(md5:<md5sum>)"}}
	seccompOpts, err := getSeccompDockerOpts(seccompProfile)
	if err != nil {
		return nil, err
	}
	// 返回[]string{"{opt.key}{sep}{opt.value}", "{opt.key}{sep}{opt.value}"}风格输出
	// 比如[]dockerOpt{{"seccomp", "unconfined", ""}}，返回[]string{"seccomp=unconfined"}
	return fmtDockerOpts(seccompOpts, separator), nil
}

// 设置createConfig.HostConfig里的Resources（包括Memory、MemorySwap、CPUShares、CPUQuota、CPUPeriod，没有CpusetCpus、CpusetMems）和OomScoreAdj
// 设置createConfig.Config.User和createConfig.HostConfig的GroupAdd、Privileged、ReadonlyRootfs、CapAdd、CapDrop、SecurityOpt（包含selinux、apparmor、NoNewPrivs，但是没有Seccomp）、MaskedPaths、ReadonlyPaths、PidMode、NetworkMode、IpcMode、UTSMode、CgroupParent
func (ds *dockerService) updateCreateConfig(
	createConfig *dockertypes.ContainerCreateConfig,
	config *runtimeapi.ContainerConfig,
	sandboxConfig *runtimeapi.PodSandboxConfig,
	podSandboxID string, securityOptSep rune, apiVersion *semver.Version) error {
	// Apply Linux-specific options if applicable.
	if lc := config.GetLinux(); lc != nil {
		// TODO: Check if the units are correct.
		// TODO: Can we assume the defaults are sane?
		rOpts := lc.GetResources()
		if rOpts != nil {
			createConfig.HostConfig.Resources = dockercontainer.Resources{
				// Memory and MemorySwap are set to the same value, this prevents containers from using any swap.
				Memory:     rOpts.MemoryLimitInBytes,
				MemorySwap: rOpts.MemoryLimitInBytes,
				CPUShares:  rOpts.CpuShares,
				CPUQuota:   rOpts.CpuQuota,
				CPUPeriod:  rOpts.CpuPeriod,
			}
			createConfig.HostConfig.OomScoreAdj = int(rOpts.OomScoreAdj)
		}
		// Note: ShmSize is handled in kube_docker_client.go

		// Apply security context.
		// 设置createConfig.Config.User和createConfig.HostConfig的GroupAdd、Privileged、ReadonlyRootfs、CapAdd、CapDrop、SecurityOpt（包含selinux、apparmor、NoNewPrivs，但是没有Seccomp）、MaskedPaths、ReadonlyPaths、PidMode、NetworkMode、IpcMode、UTSMode
		if err := applyContainerSecurityContext(lc, podSandboxID, createConfig.Config, createConfig.HostConfig, securityOptSep); err != nil {
			return fmt.Errorf("failed to apply container security context for container %q: %v", config.Metadata.Name, err)
		}
	}

	// Apply cgroupsParent derived from the sandbox config.
	if lc := sandboxConfig.GetLinux(); lc != nil {
		// Apply Cgroup options.
		// cgroup driver为systemd，则返回最后一个路径。cgroup driver为cgroupfs，则返回原始值
		// 比如cgroup driver为systemd，"kubepods-burstable-podec7bb47a_07ef_48ff_9201_687474994eab.slice"
		cgroupParent, err := ds.GenerateExpectedCgroupParent(lc.CgroupParent)
		if err != nil {
			return fmt.Errorf("failed to generate cgroup parent in expected syntax for container %q: %v", config.Metadata.Name, err)
		}
		createConfig.HostConfig.CgroupParent = cgroupParent
	}

	return nil
}

func (ds *dockerService) determinePodIPBySandboxID(uid string) []string {
	return nil
}

func getNetworkNamespace(c *dockertypes.ContainerJSON) (string, error) {
	if c.State.Pid == 0 {
		// Docker reports pid 0 for an exited container.
		return "", fmt.Errorf("cannot find network namespace for the terminated container %q", c.ID)
	}
	return fmt.Sprintf(dockerNetNSFmt, c.State.Pid), nil
}

// applyExperimentalCreateConfig applys experimental configures from sandbox annotations.
func applyExperimentalCreateConfig(createConfig *dockertypes.ContainerCreateConfig, annotations map[string]string) {
}
