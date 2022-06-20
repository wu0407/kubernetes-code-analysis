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

package apparmor

import (
	"bufio"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"

	"k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	utilpath "k8s.io/utils/path"
)

// Whether AppArmor should be disabled by default.
// Set to true if the wrong build tags are set (see validate_disabled.go).
// linux为false，非linux为true，在pkg\security\apparmor\validate_disabled.go里的init()设置
var isDisabledBuild bool

// Validator is a interface for validating that a pod with an AppArmor profile can be run by a Node.
type Validator interface {
	Validate(pod *v1.Pod) error
	ValidateHost() error
}

// NewValidator is in order to find AppArmor FS
func NewValidator(runtime string) Validator {
	// 检查操作系统和内核是否支持AppArmor和featureGate开启AppArmor
	if err := validateHost(runtime); err != nil {
		return &validator{validateHostErr: err}
	}
	// 在"/proc/mounts"里找到"apparmor securityfs"挂载路径
	appArmorFS, err := getAppArmorFS()
	if err != nil {
		return &validator{
			validateHostErr: fmt.Errorf("error finding AppArmor FS: %v", err),
		}
	}
	return &validator{
		appArmorFS: appArmorFS,
	}
}

type validator struct {
	validateHostErr error
	appArmorFS      string
}

// 验证pod的所有container的profile是合法且已加载
func (v *validator) Validate(pod *v1.Pod) error {
	// pod的annotations第一个包含前缀"container.apparmor.security.beta.kubernetes.io/"的annotation的值不为"unconfined"，则返回true，代表需要apparmor验证
	// 其他情况代表不需要验证（第一个包含前缀"container.apparmor.security.beta.kubernetes.io/"的annotation的值为"unconfined"或没有这个前缀的annotation）
	if !isRequired(pod) {
		return nil
	}

	// 检查主机条件检测的时候是否有不匹配的项
	if v.ValidateHost() != nil {
		return v.validateHostErr
	}

	// 从v.appArmorFS +"/profiles"文件中读取出"profile-name"配置
	loadedProfiles, err := v.getLoadedProfiles()
	if err != nil {
		return fmt.Errorf("could not read loaded profiles: %v", err)
	}

	var retErr error
	podutil.VisitContainers(&pod.Spec, func(container *v1.Container) bool {
		// GetProfileName----》pod的annotations["container.apparmor.security.beta.kubernetes.io/"+containerName]
		// 检查profile格式是否合法和是否已经加载了
		retErr = validateProfile(GetProfileName(pod, container.Name), loadedProfiles)
		if retErr != nil {
			return false
		}
		return true
	})

	return retErr
}

func (v *validator) ValidateHost() error {
	return v.validateHostErr
}

// Verify that the host and runtime is capable of enforcing AppArmor profiles.
// 检查操作系统和内核是否支持AppArmor和featureGate开启AppArmor
func validateHost(runtime string) error {
	// Check feature-gates
	// 默认是开启的
	if !utilfeature.DefaultFeatureGate.Enabled(features.AppArmor) {
		return errors.New("AppArmor disabled by feature-gate")
	}

	// Check build support.
	if isDisabledBuild {
		return errors.New("binary not compiled for linux")
	}

	// Check kernel support.
	if !IsAppArmorEnabled() {
		return errors.New("AppArmor is not enabled on the host")
	}

	// Check runtime support. Currently only Docker is supported.
	if runtime != kubetypes.DockerContainerRuntime && runtime != kubetypes.RemoteContainerRuntime {
		return fmt.Errorf("AppArmor is only enabled for 'docker' and 'remote' runtimes. Found: %q", runtime)
	}

	return nil
}

// Verify that the profile is valid and loaded.
// 检查profile格式是否合法和是否已经加载了
func validateProfile(profile string, loadedProfiles map[string]bool) error {
	// 检查profile格式
	if err := ValidateProfileFormat(profile); err != nil {
		return err
	}

	// profile包含"localhost/"前缀，检查profile是否在loadedProfiles里
	if strings.HasPrefix(profile, ProfileNamePrefix) {
		profileName := strings.TrimPrefix(profile, ProfileNamePrefix)
		if !loadedProfiles[profileName] {
			return fmt.Errorf("profile %q is not loaded", profileName)
		}
	}

	return nil
}

// ValidateProfileFormat checks the format of the profile.
// 检查profile格式
// 合法的格式为：
// 1. profile为空或"runtime/default"或"unconfined"
// 2. 包含"localhost/"前缀
func ValidateProfileFormat(profile string) error {
	// profile为空或"runtime/default"或"unconfined"，都是合法的
	if profile == "" || profile == ProfileRuntimeDefault || profile == ProfileNameUnconfined {
		return nil
	}
	// 必须包含"localhost/"前缀
	if !strings.HasPrefix(profile, ProfileNamePrefix) {
		return fmt.Errorf("invalid AppArmor profile name: %q", profile)
	}
	return nil
}

// 从v.appArmorFS +"/profiles"文件中读取出"profile-name"配置
func (v *validator) getLoadedProfiles() (map[string]bool, error) {
	profilesPath := path.Join(v.appArmorFS, "profiles")
	profilesFile, err := os.Open(profilesPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s: %v", profilesPath, err)
	}
	defer profilesFile.Close()

	profiles := map[string]bool{}
	scanner := bufio.NewScanner(profilesFile)
	for scanner.Scan() {
		profileName := parseProfileName(scanner.Text())
		if profileName == "" {
			// Unknown line format; skip it.
			continue
		}
		profiles[profileName] = true
	}
	return profiles, nil
}

// The profiles file is formatted with one profile per line, matching a form:
//   namespace://profile-name (mode)
//   profile-name (mode)
// Where mode is {enforce, complain, kill}. The "namespace://" is only included for namespaced
// profiles. For the purposes of Kubernetes, we consider the namespace part of the profile name.
func parseProfileName(profileLine string) string {
	modeIndex := strings.IndexRune(profileLine, '(')
	if modeIndex < 0 {
		return ""
	}
	return strings.TrimSpace(profileLine[:modeIndex])
}

// 在"/proc/mounts"里找到"apparmor securityfs"挂载路径
func getAppArmorFS() (string, error) {
	mountsFile, err := os.Open("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("could not open /proc/mounts: %v", err)
	}
	defer mountsFile.Close()

	scanner := bufio.NewScanner(mountsFile)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			// Unknown line format; skip it.
			continue
		}
		if fields[2] == "securityfs" {
			appArmorFS := path.Join(fields[1], "apparmor")
			if ok, err := utilpath.Exists(utilpath.CheckFollowSymlink, appArmorFS); !ok {
				msg := fmt.Sprintf("path %s does not exist", appArmorFS)
				if err != nil {
					return "", fmt.Errorf("%s: %v", msg, err)
				}
				return "", errors.New(msg)
			}
			return appArmorFS, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error scanning mounts: %v", err)
	}

	return "", errors.New("securityfs not found")
}

// IsAppArmorEnabled returns true if apparmor is enabled for the host.
// This function is forked from
// https://github.com/opencontainers/runc/blob/1a81e9ab1f138c091fe5c86d0883f87716088527/libcontainer/apparmor/apparmor.go
// to avoid the libapparmor dependency.
func IsAppArmorEnabled() bool {
	if _, err := os.Stat("/sys/kernel/security/apparmor"); err == nil && os.Getenv("container") == "" {
		if _, err = os.Stat("/sbin/apparmor_parser"); err == nil {
			buf, err := ioutil.ReadFile("/sys/module/apparmor/parameters/enabled")
			return err == nil && len(buf) > 1 && buf[0] == 'Y'
		}
	}
	return false
}
