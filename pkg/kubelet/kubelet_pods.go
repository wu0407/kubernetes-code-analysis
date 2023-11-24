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

package kubelet

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/api/v1/resource"
	podshelper "k8s.io/kubernetes/pkg/apis/core/pods"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	v1qos "k8s.io/kubernetes/pkg/apis/core/v1/helper/qos"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/fieldpath"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/envvars"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/images"
	"k8s.io/kubernetes/pkg/kubelet/server/portforward"
	remotecommandserver "k8s.io/kubernetes/pkg/kubelet/server/remotecommand"
	"k8s.io/kubernetes/pkg/kubelet/status"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	volumeutil "k8s.io/kubernetes/pkg/volume/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
	"k8s.io/kubernetes/pkg/volume/util/volumepathhandler"
	volumevalidation "k8s.io/kubernetes/pkg/volume/validation"
	"k8s.io/kubernetes/third_party/forked/golang/expansion"
)

const (
	managedHostsHeader                = "# Kubernetes-managed hosts file.\n"
	managedHostsHeaderWithHostNetwork = "# Kubernetes-managed hosts file (host network).\n"
)

// Get a list of pods that have data directories.
// 从pod目录（默认为"/var/lib/kubelet/pods"）中遍历出所有pod uid
func (kl *Kubelet) listPodsFromDisk() ([]types.UID, error) {
	podInfos, err := ioutil.ReadDir(kl.getPodsDir())
	if err != nil {
		return nil, err
	}
	pods := []types.UID{}
	for i := range podInfos {
		if podInfos[i].IsDir() {
			pods = append(pods, types.UID(podInfos[i].Name()))
		}
	}
	return pods, nil
}

// GetActivePods returns non-terminal pods
// 返回所有pods中（普通pod和static pod），pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除，或pod被删除且至少一个container为running
func (kl *Kubelet) GetActivePods() []*v1.Pod {
	// 从pod manger中获取所有普通pod和static pod
	allPods := kl.podManager.GetPods()
	// 返回所有pods中，pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除或pod被删除且至少一个container为running
	activePods := kl.filterOutTerminatedPods(allPods)
	return activePods
}

// makeBlockVolumes maps the raw block devices specified in the path of the container
// Experimental
// 根据container.VolumeDevices，生成container里包含块设备的信息（映射路径（如果是csi block，/var/lib/kubelet/pods/{pod uid}/volumeDevices/kubernetes.io~csi"/{vol name}）、在container里的路径、权限）
func (kl *Kubelet) makeBlockVolumes(pod *v1.Pod, container *v1.Container, podVolumes kubecontainer.VolumeMap, blkutil volumepathhandler.BlockVolumePathHandler) ([]kubecontainer.DeviceInfo, error) {
	var devices []kubecontainer.DeviceInfo
	for _, device := range container.VolumeDevices {
		// check path is absolute
		if !filepath.IsAbs(device.DevicePath) {
			return nil, fmt.Errorf("error DevicePath `%s` must be an absolute path", device.DevicePath)
		}
		vol, ok := podVolumes[device.Name]
		// 块设备不在pod已经挂载的volume里或vol.BlockVolumeMapper为空，直接返回错误
		if !ok || vol.BlockVolumeMapper == nil {
			klog.Errorf("Block volume cannot be satisfied for container %q, because the volume is missing or the volume mapper is nil: %+v", container.Name, device)
			return nil, fmt.Errorf("cannot find volume %q to pass into container %q", device.Name, container.Name)
		}
		// Get a symbolic link associated to a block device under pod device path
		// 如果是csi block，返回"/var/lib/kubelet/pods/{pod uid}/volumeDevices/kubernetes.io~csi"和vol.BlockVolumeMapper.specName
		dirPath, volName := vol.BlockVolumeMapper.GetPodDeviceMapPath()
		symlinkPath := path.Join(dirPath, volName)
		// 判断路径是否是symbolik link，和是否获取symbolik link信息发生错误
		if islinkExist, checkErr := blkutil.IsSymlinkExist(symlinkPath); checkErr != nil {
			return nil, checkErr
		} else if islinkExist {
			// Check readOnly in PVCVolumeSource and set read only permission if it's true.
			// 如果是symbolik link，则生成权限
			permission := "mrw"
			if vol.ReadOnly {
				permission = "r"
			}
			klog.V(4).Infof("Device will be attached to container %q. Path on host: %v", container.Name, symlinkPath)
			devices = append(devices, kubecontainer.DeviceInfo{PathOnHost: symlinkPath, PathInContainer: device.DevicePath, Permissions: permission})
		}
	}

	return devices, nil
}

// makeMounts determines the mount points for the given container.
// 遍历所有container.VolumeMounts，从volume manager中获取pod已经挂载的volume在宿主机中的路径作为源路径，生成挂载信息
// 如果是subpath挂载，创建bind挂载"/var/lib/kubelet/pods/{pod uid}/volume-subpaths/{volume name}/{container name}/{subpath.VolumeMountIndex}目录
// 打开volume在宿主机上挂载路径获得fd，将"/proc/{kubelet pid}/fd/{subpath.Path fd}" bind mount到这个bind挂载目录
// 添加"/etc/hosts"挂载信息，生成挂载源文件"/var/lib/kubelet/pods/{pod uid}/etc-hosts"
// 返回的cleanupAction为nil
func makeMounts(pod *v1.Pod, podDir string, container *v1.Container, hostName, hostDomain string, podIPs []string, podVolumes kubecontainer.VolumeMap, hu hostutil.HostUtils, subpather subpath.Interface, expandEnvs []kubecontainer.EnvVar) ([]kubecontainer.Mount, func(), error) {
	// Kubernetes only mounts on /etc/hosts if:
	// - container is not an infrastructure (pause) container
	// - container is not already mounting on /etc/hosts
	// - OS is not Windows
	// Kubernetes will not mount /etc/hosts if:
	// - when the Pod sandbox is being created, its IP is still unknown. Hence, PodIP will not have been set.
	mountEtcHostsFile := len(podIPs) > 0 && runtime.GOOS != "windows"
	klog.V(3).Infof("container: %v/%v/%v podIPs: %q creating hosts mount: %v", pod.Namespace, pod.Name, container.Name, podIPs, mountEtcHostsFile)
	mounts := []kubecontainer.Mount{}
	var cleanupAction func()
	// 遍历container的所有挂载
	for i, mount := range container.VolumeMounts {
		// do not mount /etc/hosts if container is already mounting on the path
		// 如果container.VolumeMounts定义了/etc/hosts挂载，则不进行/etc/hosts挂载
		mountEtcHostsFile = mountEtcHostsFile && (mount.MountPath != etcHostsPath)
		vol, ok := podVolumes[mount.Name]
		// 不在pod已挂载的volumes里或Mounter不存在，直接返回错误
		if !ok || vol.Mounter == nil {
			klog.Errorf("Mount cannot be satisfied for container %q, because the volume is missing or the volume mounter is nil: %+v", container.Name, mount)
			return nil, cleanupAction, fmt.Errorf("cannot find volume %q to mount into container %q", mount.Name, container.Name)
		}

		relabelVolume := false
		// If the volume supports SELinux and it has not been
		// relabeled already and it is not a read-only volume,
		// relabel it and mark it as labeled
		if vol.Mounter.GetAttributes().Managed && vol.Mounter.GetAttributes().SupportsSELinux && !vol.SELinuxLabeled {
			vol.SELinuxLabeled = true
			relabelVolume = true
		}
		// 返回特定插件在宿主机上挂载路径，比如local volume，路径为 linux系统默认为"/var/lib/kubelet/pods"+{podUID}+"/volumes/kubernetes.io~local-volume"+{volumeName}
		hostPath, err := volumeutil.GetPath(vol.Mounter)
		if err != nil {
			return nil, cleanupAction, err
		}

		// 子路径挂载

		subPath := mount.SubPath
		// mount定义了SubPathExpr（使用替换符$(var)来表示子路径）
		if mount.SubPathExpr != "" {
			if !utilfeature.DefaultFeatureGate.Enabled(features.VolumeSubpath) {
				return nil, cleanupAction, fmt.Errorf("volume subpaths are disabled")
			}

			if !utilfeature.DefaultFeatureGate.Enabled(features.VolumeSubpathEnvExpansion) {
				return nil, cleanupAction, fmt.Errorf("volume subpath expansion is disabled")
			}

			// 处理mount.SubPathExpr里类似"$(var)"格式，从expandEnvs里查找var对应值进行替换，和"$$"格式转义，返回mount.SubPathExpr处理后的值
			subPath, err = kubecontainer.ExpandContainerVolumeMounts(mount, expandEnvs)

			if err != nil {
				return nil, cleanupAction, err
			}
		}

		if subPath != "" {
			if !utilfeature.DefaultFeatureGate.Enabled(features.VolumeSubpath) {
				return nil, cleanupAction, fmt.Errorf("volume subpaths are disabled")
			}

			// subPath不能是绝对路径
			if filepath.IsAbs(subPath) {
				return nil, cleanupAction, fmt.Errorf("error SubPath `%s` must not be an absolute path", subPath)
			}

			// 路径中每一层不能包含".."
			err = volumevalidation.ValidatePathNoBacksteps(subPath)
			if err != nil {
				return nil, cleanupAction, fmt.Errorf("unable to provision SubPath `%s`: %v", subPath, err)
			}

			volumePath := hostPath
			// 比如local volume，"/var/lib/kubelet/pods"+{podUID}+"/volumes/kubernetes.io~local-volume"+{volumeName}+{subpath}
			hostPath = filepath.Join(volumePath, subPath)

			// 判断是否hostPath是否存在
			// 执行os.Stat发生错误，记录错误
			if subPathExists, err := hu.PathExists(hostPath); err != nil {
				klog.Errorf("Could not determine if subPath %s exists; will not attempt to change its permissions", hostPath)
			} else if !subPathExists {
				// Create the sub path now because if it's auto-created later when referenced, it may have an
				// incorrect ownership and mode. For example, the sub path directory must have at least g+rwx
				// when the pod specifies an fsGroup, and if the directory is not created here, Docker will
				// later auto-create it with the incorrect mode 0750
				// Make extra care not to escape the volume!
				// 获取文件的权限
				perm, err := hu.GetMode(volumePath)
				if err != nil {
					return nil, cleanupAction, err
				}
				// 安全的创建subPath目录（确保subPath在volumePath里，即不能是链接）
				if err := subpather.SafeMakeDir(subPath, volumePath, perm); err != nil {
					// Don't pass detailed error back to the user because it could give information about host filesystem
					klog.Errorf("failed to create subPath directory for volumeMount %q of container %q: %v", mount.Name, container.Name, err)
					return nil, cleanupAction, fmt.Errorf("failed to create subPath directory for volumeMount %q of container %q", mount.Name, container.Name)
				}
			}
			// 创建subpath bind挂载的目标路径，默认为"/var/lib/kubelet/pods/{pod uid}/volume-subpaths/{volume name}/{container name}/{subpath.VolumeMountIndex}"
			// 将"/proc/{kubelet pid}/fd/{subpath.Path fd}" bind mount到bindPathTarget（默认为"/var/lib/kubelet/pods/{pod uid}/volume-subpaths/{volume name}/{container name}/{subpath.VolumeMountIndex}"）
			// 这里返回cleanupAction为nil
			hostPath, cleanupAction, err = subpather.PrepareSafeSubpath(subpath.Subpath{
				VolumeMountIndex: i,
				Path:             hostPath,
				VolumeName:       vol.InnerVolumeSpecName,
				VolumePath:       volumePath,
				PodDir:           podDir,
				ContainerName:    container.Name,
			})
			if err != nil {
				// Don't pass detailed error back to the user because it could give information about host filesystem
				klog.Errorf("failed to prepare subPath for volumeMount %q of container %q: %v", mount.Name, container.Name, err)
				return nil, cleanupAction, fmt.Errorf("failed to prepare subPath for volumeMount %q of container %q", mount.Name, container.Name)
			}
		}

		// Docker Volume Mounts fail on Windows if it is not of the form C:/
		// windows系统，hostPath转成windows风格路径
		if volumeutil.IsWindowsLocalPath(runtime.GOOS, hostPath) {
			hostPath = volumeutil.MakeAbsolutePath(runtime.GOOS, hostPath)
		}

		containerPath := mount.MountPath
		// IsAbs returns false for UNC path/SMB shares/named pipes in Windows. So check for those specifically and skip MakeAbsolutePath
		// 不是windows系统下，或是windows系统且containerPath没有`\\`前缀，且containerPath不是绝对路径
		if !volumeutil.IsWindowsUNCPath(runtime.GOOS, containerPath) && !filepath.IsAbs(containerPath) {
			// 将containerPath转成对应的系统风格路径
			containerPath = volumeutil.MakeAbsolutePath(runtime.GOOS, containerPath)
		}

		// v1.MountPropagationMode转成runtimeapi.MountPropagation
		propagation, err := translateMountPropagation(mount.MountPropagation)
		if err != nil {
			return nil, cleanupAction, err
		}
		klog.V(5).Infof("Pod %q container %q mount %q has propagation %q", format.Pod(pod), container.Name, mount.Name, propagation)

		// volume是否read only
		mustMountRO := vol.Mounter.GetAttributes().ReadOnly

		mounts = append(mounts, kubecontainer.Mount{
			Name:           mount.Name,
			ContainerPath:  containerPath,
			HostPath:       hostPath,
			ReadOnly:       mount.ReadOnly || mustMountRO,
			SELinuxRelabel: relabelVolume,
			Propagation:    propagation,
		})
	}
	// 所有container都没有定义/etc/hosts挂载，且pod有ip，且不为windows系统，则需要生成hosts文件挂载到容器里的/etc/hosts
	if mountEtcHostsFile {
		hostAliases := pod.Spec.HostAliases
		// 生成hosts文件内容写入到"/var/lib/kubelet/pods/{pod uid}/etc-hosts"，并返回容器"/etc/hosts"挂载信息
		hostsMount, err := makeHostsMount(podDir, podIPs, hostName, hostDomain, hostAliases, pod.Spec.HostNetwork)
		if err != nil {
			return nil, cleanupAction, err
		}
		mounts = append(mounts, *hostsMount)
	}
	return mounts, cleanupAction, nil
}

// translateMountPropagation transforms v1.MountPropagationMode to
// runtimeapi.MountPropagation.
// v1.MountPropagationMode转成runtimeapi.MountPropagation
func translateMountPropagation(mountMode *v1.MountPropagationMode) (runtimeapi.MountPropagation, error) {
	if runtime.GOOS == "windows" {
		// Windows containers doesn't support mount propagation, use private for it.
		// Refer https://docs.docker.com/storage/bind-mounts/#configure-bind-propagation.
		return runtimeapi.MountPropagation_PROPAGATION_PRIVATE, nil
	}

	switch {
	case mountMode == nil:
		// PRIVATE is the default
		return runtimeapi.MountPropagation_PROPAGATION_PRIVATE, nil
	case *mountMode == v1.MountPropagationHostToContainer:
		return runtimeapi.MountPropagation_PROPAGATION_HOST_TO_CONTAINER, nil
	case *mountMode == v1.MountPropagationBidirectional:
		return runtimeapi.MountPropagation_PROPAGATION_BIDIRECTIONAL, nil
	case *mountMode == v1.MountPropagationNone:
		return runtimeapi.MountPropagation_PROPAGATION_PRIVATE, nil
	default:
		return 0, fmt.Errorf("invalid MountPropagation mode: %q", *mountMode)
	}
}

// getEtcHostsPath returns the full host-side path to a pod's generated /etc/hosts file
// 默认为"/var/lib/kubelet/pods/{pod uid}/etc-hosts"
func getEtcHostsPath(podDir string) string {
	return path.Join(podDir, "etc-hosts")
}

// makeHostsMount makes the mountpoint for the hosts file that the containers
// in a pod are injected with. podIPs is provided instead of podIP as podIPs
// are present even if dual-stack feature flag is not enabled.
// 生成hosts文件内容写入到"/var/lib/kubelet/pods/{pod uid}/etc-hosts"，并返回容器"/etc/hosts"挂载信息
func makeHostsMount(podDir string, podIPs []string, hostName, hostDomainName string, hostAliases []v1.HostAlias, useHostNetwork bool) (*kubecontainer.Mount, error) {
	// podDir默认是"/var/lib/kubelet/pods/{pod uid}"，hostsFilePath为"/var/lib/kubelet/pods/{pod uid}/etc-hosts"
	hostsFilePath := getEtcHostsPath(podDir)
	// 生成hosts文件内容写入到hostsFilePath
	if err := ensureHostsFile(hostsFilePath, podIPs, hostName, hostDomainName, hostAliases, useHostNetwork); err != nil {
		return nil, err
	}
	return &kubecontainer.Mount{
		Name:           "k8s-managed-etc-hosts",
		ContainerPath:  etcHostsPath,
		HostPath:       hostsFilePath,
		ReadOnly:       false,
		SELinuxRelabel: true,
	}, nil
}

// ensureHostsFile ensures that the given host file has an up-to-date ip, host
// name, and domain name.
func ensureHostsFile(fileName string, hostIPs []string, hostName, hostDomainName string, hostAliases []v1.HostAlias, useHostNetwork bool) error {
	var hostsFileContent []byte
	var err error

	// 如果是host网络
	if useHostNetwork {
		// if Pod is using host network, read hosts file from the node's filesystem.
		// `etcHostsPath` references the location of the hosts file on the node.
		// `/etc/hosts` for *nix systems.
		// 读取宿主机的"/etc/hosts"文件内容，加入hostAliases内容
		hostsFileContent, err = nodeHostsFileContent(etcHostsPath, hostAliases)
		if err != nil {
			return err
		}
	} else {
		// if Pod is not using host network, create a managed hosts file with Pod IP and other information.
		// 不是host网络，生成host文件里面包含了pod ip与pod name之间的对应关系、localhost、ipv6、hostAliases内容
		hostsFileContent = managedHostsFileContent(hostIPs, hostName, hostDomainName, hostAliases)
	}

	return ioutil.WriteFile(fileName, hostsFileContent, 0644)
}

// nodeHostsFileContent reads the content of node's hosts file.
// 返回类似这样
// `# Kubernetes-managed hosts file (host network).\n
// {hostsFilePath}
// \n
// # Entries added by HostAliases.\n
// {hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n
// {hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n
// `
func nodeHostsFileContent(hostsFilePath string, hostAliases []v1.HostAlias) ([]byte, error) {
	hostsFileContent, err := ioutil.ReadFile(hostsFilePath)
	if err != nil {
		return nil, err
	}
	var buffer bytes.Buffer
	buffer.WriteString(managedHostsHeaderWithHostNetwork)
	buffer.Write(hostsFileContent)
	// 返回类似这样 `\n # Entries added by HostAliases.\n{hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n{hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n`
	buffer.Write(hostsEntriesFromHostAliases(hostAliases))
	return buffer.Bytes(), nil
}

// managedHostsFileContent generates the content of the managed etc hosts based on Pod IPs and other
// information.
// 返回类似
// `# Kubernetes-managed hosts file.\n
// 127.0.0.1\tlocalhost\n
// ::1\tlocalhost ip6-localhost ip6-loopback\n
// fe00::0\tip6-localnet\n
// fe00::0\tip6-mcastprefix\n
// fe00::1\tip6-allnodes\n
// fe00::2\tip6-allrouters\n
// {hostIP[0]}\t{hostName}.{hostDomainName}\t{hostName}
// {hostIP[1]}\t{hostName}.{hostDomainName}\t{hostName}
// 或
// {hostIP[0]}\t{hostName}
// {hostIP[1]}\t{hostName}
// \n
// # Entries added by HostAliases.\n
// {hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n
// {hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n`
func managedHostsFileContent(hostIPs []string, hostName, hostDomainName string, hostAliases []v1.HostAlias) []byte {
	var buffer bytes.Buffer
	buffer.WriteString(managedHostsHeader)
	buffer.WriteString("127.0.0.1\tlocalhost\n")                      // ipv4 localhost
	buffer.WriteString("::1\tlocalhost ip6-localhost ip6-loopback\n") // ipv6 localhost
	buffer.WriteString("fe00::0\tip6-localnet\n")
	buffer.WriteString("fe00::0\tip6-mcastprefix\n")
	buffer.WriteString("fe00::1\tip6-allnodes\n")
	buffer.WriteString("fe00::2\tip6-allrouters\n")
	if len(hostDomainName) > 0 {
		// host entry generated for all IPs in podIPs
		// podIPs field is populated for clusters even
		// dual-stack feature flag is not enabled.
		for _, hostIP := range hostIPs {
			buffer.WriteString(fmt.Sprintf("%s\t%s.%s\t%s\n", hostIP, hostName, hostDomainName, hostName))
		}
	} else {
		for _, hostIP := range hostIPs {
			buffer.WriteString(fmt.Sprintf("%s\t%s\n", hostIP, hostName))
		}
	}
	buffer.Write(hostsEntriesFromHostAliases(hostAliases))
	return buffer.Bytes()
}

// 返回类似这样
// `\n
// # Entries added by HostAliases.\n
// {hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n
// {hostAlias.IP}\t{hostAlias.Hostnames[0]}\t{hostAlias.Hostnames[1]}\n`
func hostsEntriesFromHostAliases(hostAliases []v1.HostAlias) []byte {
	if len(hostAliases) == 0 {
		return []byte{}
	}

	var buffer bytes.Buffer
	buffer.WriteString("\n")
	buffer.WriteString("# Entries added by HostAliases.\n")
	// for each IP, write all aliases onto single line in hosts file
	for _, hostAlias := range hostAliases {
		buffer.WriteString(fmt.Sprintf("%s\t%s\n", hostAlias.IP, strings.Join(hostAlias.Hostnames, "\t")))
	}
	return buffer.Bytes()
}

// truncatePodHostnameIfNeeded truncates the pod hostname if it's longer than 63 chars.
// 验证hostname是否合法，超出部分直接截断，去除尾部'-' or '.'
func truncatePodHostnameIfNeeded(podName, hostname string) (string, error) {
	// Cap hostname at 63 chars (specification is 64bytes which is 63 chars and the null terminating char).
	const hostnameMaxLen = 63
	// 长度小于63，直接返回
	if len(hostname) <= hostnameMaxLen {
		return hostname, nil
	}
	// 超出部分直接截断
	truncated := hostname[:hostnameMaxLen]
	klog.Errorf("hostname for pod:%q was longer than %d. Truncated hostname to :%q", podName, hostnameMaxLen, truncated)
	// hostname should not end with '-' or '.'
	// 去除尾部'-' or '.'
	truncated = strings.TrimRight(truncated, "-.")
	if len(truncated) == 0 {
		// This should never happen.
		return "", fmt.Errorf("hostname for pod %q was invalid: %q", podName, hostname)
	}
	return truncated, nil
}

// GeneratePodHostNameAndDomain creates a hostname and domain name for a pod,
// given that pod's spec and annotations or returns an error.
// 返回pod的主机名和主机域
// 主机名默认为pod名
// pod里定义了主机名，如果合法，则使用这个主机名为pod主机名
// 主机域默认为空
// pod里定义了subdomain，验证subdomain是否合法。如果合法，则设置主机域为{Subdomain}.{pod.Namespace}.svc.{clusterDomain}。
func (kl *Kubelet) GeneratePodHostNameAndDomain(pod *v1.Pod) (string, string, error) {
	clusterDomain := kl.dnsConfigurer.ClusterDomain

	hostname := pod.Name
	// pod里定义了主机名，如果合法，则使用这个主机名为pod主机名
	if len(pod.Spec.Hostname) > 0 {
		// 验证主机名是否合法
		if msgs := utilvalidation.IsDNS1123Label(pod.Spec.Hostname); len(msgs) != 0 {
			return "", "", fmt.Errorf("pod Hostname %q is not a valid DNS label: %s", pod.Spec.Hostname, strings.Join(msgs, ";"))
		}
		hostname = pod.Spec.Hostname
	}

	// 验证hostname是否合法，超出部分直接截断，去除尾部'-' or '.'
	hostname, err := truncatePodHostnameIfNeeded(pod.Name, hostname)
	if err != nil {
		return "", "", err
	}

	hostDomain := ""
	// pod里定义了subdomain，验证subdomain是否合法。如果合法，则设置主机域为{Subdomain}.{pod.Namespace}.svc.{clusterDomain}。
	if len(pod.Spec.Subdomain) > 0 {
		if msgs := utilvalidation.IsDNS1123Label(pod.Spec.Subdomain); len(msgs) != 0 {
			return "", "", fmt.Errorf("pod Subdomain %q is not a valid DNS label: %s", pod.Spec.Subdomain, strings.Join(msgs, ";"))
		}
		hostDomain = fmt.Sprintf("%s.%s.svc.%s", pod.Spec.Subdomain, pod.Namespace, clusterDomain)
	}

	return hostname, hostDomain, nil
}

// GetPodCgroupParent gets pod cgroup parent from container manager.
// 生成pod的cgroup目录（比如"/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-podec7bb47a_07ef_48ff_9201_687474994eab.slice"）
func (kl *Kubelet) GetPodCgroupParent(pod *v1.Pod) string {
	pcm := kl.containerManager.NewPodContainerManager()
	// 返回pod的cgroupName和cgroup路径
	_, cgroupParent := pcm.GetPodContainerName(pod)
	return cgroupParent
}

// GenerateRunContainerOptions generates the RunContainerOptions, which can be used by
// the container runtime to set parameters for launching a container.
// 1. 从device manager中（分配或重用）获得container需要的资源，生成Devices、Mounts、Envs、Annotations
// 2. 根据container中port定义生成PortMappings
// 3. 根据container.VolumeDevices，生成container里包含块设备的信息Devices
// 4. 根据container.EnvFrom和container.Env，生成container的环境变量Envs，包括service环境变量、EnvFrom、Env（包括了ValueFrom、普通的key value）
// 5. 根据container.VolumeMounts，并从volume manager中获取pod已经挂载的volume在宿主机中的路径作为源路径，生成挂载信息Mounts（对subpath的挂载，生成subpath目录和bind挂载目录，并进行bind挂载，使用bind挂载路径作为源路径），并添加"/etc/hosts"挂载信息，生成挂载源文件"/var/lib/kubelet/pods/{pod uid}/etc-hosts"
// 6. 创建container目录"/var/lib/kubelet/pods/{podUID}/containers/{container name}"，设置PodContainerDir
// 7. 根据container里的设置和kl.experimentalHostUserNamespaceDefaulting，判断是否设置EnableHostUserNamespace
// 返回的cleanupAction为nil
// ReadOnly字段没有设置
func (kl *Kubelet) GenerateRunContainerOptions(pod *v1.Pod, container *v1.Container, podIP string, podIPs []string) (*kubecontainer.RunContainerOptions, func(), error) {
	// 从pod分配记录中（kl.containerManager.deviceManager.podDevices）判断容器是否已经分配过需要的资源（有可能已经分配过比如container重启），没有就进行分配
	// 返回分配过或新分配资源转成container runtime运行参数（*kubecontainer.RunContainerOptions），包含Devices、Mounts、Envs、Annotations
	opts, err := kl.containerManager.GetResources(pod, container)
	if err != nil {
		return nil, nil, err
	}

	// 返回pod的主机名和主机域
	// 主机名默认为pod名
	// pod里定义了主机名，如果合法，则使用这个主机名为pod主机名
	// 主机域默认为空
	// pod里定义了subdomain，验证subdomain是否合法。如果合法，则设置主机域为{Subdomain}.{pod.Namespace}.svc.{clusterDomain}。
	hostname, hostDomainName, err := kl.GeneratePodHostNameAndDomain(pod)
	if err != nil {
		return nil, nil, err
	}
	opts.Hostname = hostname
	// pod uid转成types.UniquePodName
	podName := volumeutil.GetUniquePodName(pod)
	// 获取pod已经挂载的volume
	volumes := kl.volumeManager.GetMountedVolumesForPod(podName)

	// 生成container的portmapping
	opts.PortMappings = kubecontainer.MakePortMappings(container)

	blkutil := volumepathhandler.NewBlockVolumePathHandler()
	// 根据container.VolumeDevices，生成container里包含块设备的信息（映射路径（如果是csi block，/var/lib/kubelet/pods/{pod uid}/volumeDevices/kubernetes.io~csi"/{vol name}、在container里的路径、权限）
	blkVolumes, err := kl.makeBlockVolumes(pod, container, volumes, blkutil)
	if err != nil {
		return nil, nil, err
	}
	opts.Devices = append(opts.Devices, blkVolumes...)

	// 生成container的环境变量，包括service环境变量、EnvFrom、Env（包括了ValueFrom、普通的key value）
	envs, err := kl.makeEnvironmentVariables(pod, container, podIP, podIPs)
	if err != nil {
		return nil, nil, err
	}
	opts.Envs = append(opts.Envs, envs...)

	// only podIPs is sent to makeMounts, as podIPs is populated even if dual-stack feature flag is not enabled.
	// 遍历所有container.VolumeMounts，从volume manager中获取pod已经挂载的volume在宿主机中的路径作为源路径，生成挂载信息
	// 如果是subpath挂载，创建subpath目录（volume目录加subpath目录）和创建bind挂载"/var/lib/kubelet/pods/{pod uid}/volume-subpaths/{volume name}/{container name}/{subpath.VolumeMountIndex}目录
	// 打开volume在宿主机上挂载路径获得fd，将"/proc/{kubelet pid}/fd/{subpath.Path fd}" bind mount到这个bind挂载目录
	// 添加"/etc/hosts"挂载信息，生成挂载源文件"/var/lib/kubelet/pods/{pod uid}/etc-hosts"
	mounts, cleanupAction, err := makeMounts(pod, kl.getPodDir(pod.UID), container, hostname, hostDomainName, podIPs, volumes, kl.hostutil, kl.subpather, opts.Envs)
	if err != nil {
		return nil, cleanupAction, err
	}
	opts.Mounts = append(opts.Mounts, mounts...)

	// adding TerminationMessagePath on Windows is only allowed if ContainerD is used. Individual files cannot
	// be mounted as volumes using Docker for Windows.
	// 非windows系统都返回true，windows系统runtime不为"docker"返回true
	supportsSingleFileMapping := kl.containerRuntime.SupportsSingleFileMapping()
	// container.TerminationMessagePath默认为"/dev/termination-log"
	if len(container.TerminationMessagePath) != 0 && supportsSingleFileMapping {
		// 默认为"/var/lib/kubelet/pods/{podUID}/containers/{container name}"
		p := kl.getPodContainerDir(pod.UID, container.Name)
		// 创建目录
		if err := os.MkdirAll(p, 0750); err != nil {
			klog.Errorf("Error on creating %q: %v", p, err)
		} else {
			opts.PodContainerDir = p
		}
	}

	// only do this check if the experimental behavior is enabled, otherwise allow it to default to false
	// kl.experimentalHostUserNamespaceDefaulting默认为false
	if kl.experimentalHostUserNamespaceDefaulting {
		// 下面条件满足一个返回true
		// 只要pod里的一个container的SecurityContext.Privileged为true
		// 或pod.Spec.SecurityContext不为为空且pod设置了HostIPC、HostNetwork、HostPID为true
		// 或pod里定义了HostPath挂载
		// 或container里设置了SecurityContext.Capabilities，且添加了"MKNOD"或"SYS_TIME"或"SYS_MODULE"的Capabilities
		// 或pod使用了pvc，且pvc使用了HostPath volume
		opts.EnableHostUserNamespace = kl.enableHostUserNamespace(pod)
	}

	return opts, cleanupAction, nil
}

var masterServices = sets.NewString("kubernetes")

// getServiceEnvVarMap makes a map[string]string of env vars for services a
// pod in namespace ns should see.
// 生成apiserver的service的环境变量，如果enableServiceLinks为true，则还会根据ns命名空间里里所有service生成环境变量
func (kl *Kubelet) getServiceEnvVarMap(ns string, enableServiceLinks bool) (map[string]string, error) {
	var (
		serviceMap = make(map[string]*v1.Service)
		m          = make(map[string]string)
	)

	// Get all service resources from the master (via a cache),
	// and populate them into service environment variables.
	if kl.serviceLister == nil {
		// Kubelets without masters (e.g. plain GCE ContainerVM) don't set env vars.
		return m, nil
	}
	services, err := kl.serviceLister.List(labels.Everything())
	if err != nil {
		return m, fmt.Errorf("failed to list services when setting up env vars")
	}

	// project the services in namespace ns onto the master services
	for i := range services {
		service := services[i]
		// ignore services where ClusterIP is "None" or empty
		// 忽略没有分配cluster ip的service
		if !v1helper.IsServiceIPSet(service) {
			continue
		}
		serviceName := service.Name

		// We always want to add environment variabled for master services
		// from the master service namespace, even if enableServiceLinks is false.
		// We also add environment variables for other services in the same
		// namespace, if enableServiceLinks is true.
		// 如果service的namespace是masterServiceNamespace（默认为default），且service名字为"kubernetes"
		// 添加到serviceMap，即apiserver的service一定会生成环境变量，无论pod.Spec.EnableServiceLinks值
		if service.Namespace == kl.masterServiceNamespace && masterServices.Has(serviceName) {
			// 以第一次发现的为准
			if _, exists := serviceMap[serviceName]; !exists {
				serviceMap[serviceName] = service
			}
		// 如果service的namespace是ns且enableServiceLinks为true，则添加到serviceMap
		} else if service.Namespace == ns && enableServiceLinks {
			serviceMap[serviceName] = service
		}
	}

	mappedServices := []*v1.Service{}
	for key := range serviceMap {
		mappedServices = append(mappedServices, serviceMap[key])
	}

	// 生成service环境变量
	for _, e := range envvars.FromServices(mappedServices) {
		m[e.Name] = e.Value
	}
	return m, nil
}

// Make the environment variables for a pod in the given namespace.
// 生成container的service环境变量、EnvFrom、Env（包括了ValueFrom、普通的key value）
func (kl *Kubelet) makeEnvironmentVariables(pod *v1.Pod, container *v1.Container, podIP string, podIPs []string) ([]kubecontainer.EnvVar, error) {
	if pod.Spec.EnableServiceLinks == nil {
		return nil, fmt.Errorf("nil pod.spec.enableServiceLinks encountered, cannot construct envvars")
	}

	var result []kubecontainer.EnvVar
	// Note:  These are added to the docker Config, but are not included in the checksum computed
	// by kubecontainer.HashContainer(...).  That way, we can still determine whether an
	// v1.Container is already running by its hash. (We don't want to restart a container just
	// because some service changed.)
	//
	// Note that there is a race between Kubelet seeing the pod and kubelet seeing the service.
	// To avoid this users can: (1) wait between starting a service and starting; or (2) detect
	// missing service env var and exit and be restarted; or (3) use DNS instead of env vars
	// and keep trying to resolve the DNS name of the service (recommended).
	// 生成apiserver的service的环境变量，如果enableServiceLinks为true，则还会根据ns命名空间里里所有service生成环境变量
	serviceEnv, err := kl.getServiceEnvVarMap(pod.Namespace, *pod.Spec.EnableServiceLinks)
	if err != nil {
		return result, err
	}

	var (
		configMaps = make(map[string]*v1.ConfigMap)
		secrets    = make(map[string]*v1.Secret)
		tmpEnv     = make(map[string]string)
	)

	// Env will override EnvFrom variables.
	// Process EnvFrom first then allow Env to replace existing values.
	// 先执行container里定义的EnvFrom
	for _, envFrom := range container.EnvFrom {
		switch {
		case envFrom.ConfigMapRef != nil:
			cm := envFrom.ConfigMapRef
			name := cm.Name
			configMap, ok := configMaps[name]
			if !ok {
				if kl.kubeClient == nil {
					return result, fmt.Errorf("couldn't get configMap %v/%v, no kubeClient defined", pod.Namespace, name)
				}
				optional := cm.Optional != nil && *cm.Optional
				// 如果是watch方式，从kl.configMapManager.manager.items获取对象的objectCacheItem，并在objectCacheItem的store中获取对象的资源（secret或configmap）。如果secret或configmap是不可修改的，则停止这个对象的reflector。
				configMap, err = kl.configMapManager.GetConfigMap(pod.Namespace, name)
				if err != nil {
					// 如果没有configmap且可选的optional，则跳过
					if errors.IsNotFound(err) && optional {
						// ignore error when marked optional
						continue
					}
					return result, err
				}
				configMaps[name] = configMap
			}

			invalidKeys := []string{}
			for k, v := range configMap.Data {
				// 定义了prefix，则key添加这个前缀
				if len(envFrom.Prefix) > 0 {
					k = envFrom.Prefix + k
				}
				// 验证key是否是合法的环境变量名
				// k必须匹配"^[-._a-zA-Z][-._a-zA-Z0-9]*$"，且value不是"."、".."，且前缀不是".."，则返回为空
				// 不是合法的环境变量名，则跳过
				if errMsgs := utilvalidation.IsEnvVarName(k); len(errMsgs) != 0 {
					invalidKeys = append(invalidKeys, k)
					continue
				}
				tmpEnv[k] = v
			}
			if len(invalidKeys) > 0 {
				sort.Strings(invalidKeys)
				kl.recorder.Eventf(pod, v1.EventTypeWarning, "InvalidEnvironmentVariableNames", "Keys [%s] from the EnvFrom configMap %s/%s were skipped since they are considered invalid environment variable names.", strings.Join(invalidKeys, ", "), pod.Namespace, name)
			}
		case envFrom.SecretRef != nil:
			s := envFrom.SecretRef
			name := s.Name
			secret, ok := secrets[name]
			if !ok {
				if kl.kubeClient == nil {
					return result, fmt.Errorf("couldn't get secret %v/%v, no kubeClient defined", pod.Namespace, name)
				}
				optional := s.Optional != nil && *s.Optional
				// 如果是watch方式，从kl.secretManager.manager.items获取对象的objectCacheItem，并在objectCacheItem的store中获取对象的资源（secret或configmap）。如果secret或configmap是不可修改的，则停止这个对象的reflector。
				secret, err = kl.secretManager.GetSecret(pod.Namespace, name)
				if err != nil {
					// 如果没有secret且可选的optional，则跳过
					if errors.IsNotFound(err) && optional {
						// ignore error when marked optional
						continue
					}
					return result, err
				}
				secrets[name] = secret
			}

			invalidKeys := []string{}
			for k, v := range secret.Data {
				// 定义了prefix，则key添加这个前缀
				if len(envFrom.Prefix) > 0 {
					k = envFrom.Prefix + k
				}
				// 验证key是否是合法的环境变量名
				// k必须匹配"^[-._a-zA-Z][-._a-zA-Z0-9]*$"，且value不是"."、".."，且前缀不是".."，则返回为空
				// 不是合法的环境变量名，则跳过
				if errMsgs := utilvalidation.IsEnvVarName(k); len(errMsgs) != 0 {
					invalidKeys = append(invalidKeys, k)
					continue
				}
				tmpEnv[k] = string(v)
			}
			if len(invalidKeys) > 0 {
				sort.Strings(invalidKeys)
				kl.recorder.Eventf(pod, v1.EventTypeWarning, "InvalidEnvironmentVariableNames", "Keys [%s] from the EnvFrom secret %s/%s were skipped since they are considered invalid environment variable names.", strings.Join(invalidKeys, ", "), pod.Namespace, name)
			}
		}
	}

	// Determine the final values of variables:
	//
	// 1.  Determine the final value of each variable:
	//     a.  If the variable's Value is set, expand the `$(var)` references to other
	//         variables in the .Value field; the sources of variables are the declared
	//         variables of the container and the service environment variables
	//     b.  If a source is defined for an environment variable, resolve the source
	// 2.  Create the container's environment in the order variables are declared
	// 3.  Add remaining service environment vars
	var (
		// 返回函数，在tmpEnv（envFrom里定义的环境变量）和serviceEnv里查找input，找到就返回，否则返回"$({input})"，比如"$(var)"
		mappingFunc = expansion.MappingFuncFor(tmpEnv, serviceEnv)
	)
	// 处理container.Env
	for _, envVar := range container.Env {
		runtimeVal := envVar.Value
		if runtimeVal != "" {
			// Step 1a: expand variable references
			// 使用Value字段设置变量值且变量值不为空，则解析变量值（处理$(value)值需要被替换的情况和"$$"格式转义）
			runtimeVal = expansion.Expand(runtimeVal, mappingFunc)
		} else if envVar.ValueFrom != nil {
			// Step 1b: resolve alternate env var sources
			// 使用ValueFrom字段设置变量值
			switch {
			// 从metadata.name, metadata.namespace, metadata.labels, metadata.annotations, "metadata.uid", spec.nodeName, spec.serviceAccountName, status.hostIP, status.podIP, status.podIPs里获取变量值
			case envVar.ValueFrom.FieldRef != nil:
				// 返回envVar.ValueFrom.FieldRef.FieldPath在pod中相应字段的值
				runtimeVal, err = kl.podFieldSelectorRuntimeValue(envVar.ValueFrom.FieldRef, pod, podIP, podIPs)
				if err != nil {
					return result, err
				}
			// 使用container的resource字段设置环境变量值
			case envVar.ValueFrom.ResourceFieldRef != nil:
				// 设置pod里所有普通container的resource limit和参数里的container的resource limit，如果container某种resource没有设置limit，则设置为node allocatable里对应resource的值
				defaultedPod, defaultedContainer, err := kl.defaultPodLimitsForDownwardAPI(pod, container)
				if err != nil {
					return result, err
				}
				// 当ResourceFieldSelector指定了container name，则从defaultedPod里name为containerName的container中获取ResourceFieldSelector.Resource的值
				// 否则从defaultedContainer获取ResourceFieldSelector.Resource的值
				runtimeVal, err = containerResourceRuntimeValue(envVar.ValueFrom.ResourceFieldRef, defaultedPod, defaultedContainer)
				if err != nil {
					return result, err
				}
			// 从configmap中的获取环境变量的值
			case envVar.ValueFrom.ConfigMapKeyRef != nil:
				cm := envVar.ValueFrom.ConfigMapKeyRef
				name := cm.Name
				key := cm.Key
				optional := cm.Optional != nil && *cm.Optional
				// 是否在前面envFrom里获取过的configmap
				configMap, ok := configMaps[name]
				// 不在在envFrom里获取过的configmap
				if !ok {
					if kl.kubeClient == nil {
						return result, fmt.Errorf("couldn't get configMap %v/%v, no kubeClient defined", pod.Namespace, name)
					}
					// 如果是watch方式，从kl.configMapManager.manager.items获取对象的objectCacheItem，并在objectCacheItem的store中获取对象的资源（secret或configmap）。如果secret或configmap是不可修改的，则停止这个对象的reflector。
					configMap, err = kl.configMapManager.GetConfigMap(pod.Namespace, name)
					if err != nil {
						// 如果没有configmap且可选的optional，则跳过
						if errors.IsNotFound(err) && optional {
							// ignore error when marked optional
							continue
						}
						return result, err
					}
					configMaps[name] = configMap
				}
				runtimeVal, ok = configMap.Data[key]
				if !ok {
					if optional {
						continue
					}
					return result, fmt.Errorf("couldn't find key %v in ConfigMap %v/%v", key, pod.Namespace, name)
				}
			// 从secret中获取环境变量的值
			case envVar.ValueFrom.SecretKeyRef != nil:
				s := envVar.ValueFrom.SecretKeyRef
				name := s.Name
				key := s.Key
				optional := s.Optional != nil && *s.Optional
				// 是否在前面envFrom里获取过的secret
				secret, ok := secrets[name]
				// 不在前面envFrom里获取过的secret
				if !ok {
					if kl.kubeClient == nil {
						return result, fmt.Errorf("couldn't get secret %v/%v, no kubeClient defined", pod.Namespace, name)
					}
					// 如果是watch方式，从kl.secretManager.manager.items获取对象的objectCacheItem，并在objectCacheItem的store中获取对象的资源（secret或configmap）。如果secret或configmap是不可修改的，则停止这个对象的reflector。
					secret, err = kl.secretManager.GetSecret(pod.Namespace, name)
					if err != nil {
						// 如果没有secret且可选的optional，则跳过
						if errors.IsNotFound(err) && optional {
							// ignore error when marked optional
							continue
						}
						return result, err
					}
					secrets[name] = secret
				}
				runtimeValBytes, ok := secret.Data[key]
				if !ok {
					if optional {
						continue
					}
					return result, fmt.Errorf("couldn't find key %v in Secret %v/%v", key, pod.Namespace, name)
				}
				runtimeVal = string(runtimeValBytes)
			}
		}
		// Accesses apiserver+Pods.
		// So, the master may set service env vars, or kubelet may.  In case both are doing
		// it, we delete the key from the kubelet-generated ones so we don't have duplicate
		// env vars.
		// TODO: remove this next line once all platforms use apiserver+Pods.
		// 从service环境变量里删除envVar定义的环境变量（防止envVar也定义的service环境变量，出现重复情况）
		delete(serviceEnv, envVar.Name)

		tmpEnv[envVar.Name] = runtimeVal
	}

	// Append the env vars
	for k, v := range tmpEnv {
		result = append(result, kubecontainer.EnvVar{Name: k, Value: v})
	}

	// Append remaining service env vars.
	for k, v := range serviceEnv {
		// Accesses apiserver+Pods.
		// So, the master may set service env vars, or kubelet may.  In case both are doing
		// it, we skip the key from the kubelet-generated ones so we don't have duplicate
		// env vars.
		// TODO: remove this next line once all platforms use apiserver+Pods.
		// 再次去重（防止envFrom也定义的service环境变量，出现重复情况）
		if _, present := tmpEnv[k]; !present {
			result = append(result, kubecontainer.EnvVar{Name: k, Value: v})
		}
	}
	return result, nil
}

// podFieldSelectorRuntimeValue returns the runtime value of the given
// selector for a pod.
// 返回fs.FieldPath在pod中的值
func (kl *Kubelet) podFieldSelectorRuntimeValue(fs *v1.ObjectFieldSelector, pod *v1.Pod, podIP string, podIPs []string) (string, error) {
	// 验证fs.APIVersion和fs.FieldPath是否合法
	// internalFieldPath为fs.FieldPath
	internalFieldPath, _, err := podshelper.ConvertDownwardAPIFieldLabel(fs.APIVersion, fs.FieldPath, "")
	// 如果fs.APIVersion或fs.FieldPath值是不支持设置，返回错误
	if err != nil {
		return "", err
	}
	switch internalFieldPath {
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.hostIP":
		hostIP, err := kl.getHostIPAnyWay()
		if err != nil {
			return "", err
		}
		return hostIP.String(), nil
	case "status.podIP":
		return podIP, nil
	case "status.podIPs":
		return strings.Join(podIPs, ","), nil
	}
	// "metadata.annotations" "metadata.labels" "metadata.name" "metadata.namespace" "metadata.uid"
	return fieldpath.ExtractFieldPathAsString(pod, internalFieldPath)
}

// containerResourceRuntimeValue returns the value of the provided container resource
// 当ResourceFieldSelector指定了container name，则从pod里name为containerName的container中获取ResourceFieldSelector.Resource的值
// 否则从参数里的container获取ResourceFieldSelector.Resource的值
func containerResourceRuntimeValue(fs *v1.ResourceFieldSelector, pod *v1.Pod, container *v1.Container) (string, error) {
	containerName := fs.ContainerName
	// ResourceFieldSelector里没有指定container，则从参数里的container获取ResourceFieldSelector.Resource的值
	if len(containerName) == 0 {
		return resource.ExtractContainerResourceValue(fs, container)
	}
	// 从pod里的name为containerName的container中获取ResourceFieldSelector.Resource的值
	return resource.ExtractResourceValueByContainerName(fs, pod, containerName)
}

// One of the following arguments must be non-nil: runningPod, status.
// TODO: Modify containerRuntime.KillPod() to accept the right arguments.
// 从kl.Podkiller()调用这个，则gracePeriodOverride为nil，从kl.syncPod里调用可能不为nil
// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
func (kl *Kubelet) killPod(pod *v1.Pod, runningPod *kubecontainer.Pod, status *kubecontainer.PodStatus, gracePeriodOverride *int64) error {
	var p kubecontainer.Pod
	if runningPod != nil {
		p = *runningPod
	} else if status != nil {
		// 一般kl.getRuntime().Type()是"docker"
		// 返回pod正在运行的container和sandbox
		p = kubecontainer.ConvertPodStatusToRunningPod(kl.getRuntime().Type(), status)
	} else {
		return fmt.Errorf("one of the two arguments must be non-nil: runningPod, status")
	}

	// Call the container runtime KillPod method which stops all running containers of the pod
	// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container），返回所有killContainer结果
	// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container，添加执行结果到result
	if err := kl.containerRuntime.KillPod(pod, p, gracePeriodOverride); err != nil {
		return err
	}
	// 根据所有active pod来统计Burstable和BestEffort的cgroup属性
	// 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
	// cpu group设置cpu share值
	// hugepage设置 hugepage.limit_in_bytes为int64最大值
	// memory设置memory.limit_in_bytes
	if err := kl.containerManager.UpdateQOSCgroups(); err != nil {
		klog.V(2).Infof("Failed to update QoS cgroups while killing pod: %v", err)
	}
	return nil
}

// makePodDataDirs creates the dirs for the pod datas.
// 创建pod相关的目录
func (kl *Kubelet) makePodDataDirs(pod *v1.Pod) error {
	uid := pod.UID
	// 创建pod目录，默认为"/var/lib/kubelet/pods"+{uid}
	if err := os.MkdirAll(kl.getPodDir(uid), 0750); err != nil && !os.IsExist(err) {
		return err
	}
	// 创建pod volumes目录，默认为"/var/lib/kubelet/pods/"+{uid}+"/volumes"
	if err := os.MkdirAll(kl.getPodVolumesDir(uid), 0750); err != nil && !os.IsExist(err) {
		return err
	}
	// 创建pod的插件目录，默认为"/var/lib/kubelet/pods/"+{uid}+"/plugins"
	if err := os.MkdirAll(kl.getPodPluginsDir(uid), 0750); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// getPullSecretsForPod inspects the Pod and retrieves the referenced pull
// secrets.
// 获取pod相关的所有ImagePullSecrets
func (kl *Kubelet) getPullSecretsForPod(pod *v1.Pod) []v1.Secret {
	pullSecrets := []v1.Secret{}

	for _, secretRef := range pod.Spec.ImagePullSecrets {
		// 如果是watch方式，从kl.secretManager.manager.items获取对象的objectCacheItem，在objectCacheItem的store中获取对象的资源（secret或configmap）。如果secret或configmap是不可修改的，则停止这个对象的reflector。
		secret, err := kl.secretManager.GetSecret(pod.Namespace, secretRef.Name)
		if err != nil {
			klog.Warningf("Unable to retrieve pull secret %s/%s for %s/%s due to %v.  The image pull may not succeed.", pod.Namespace, secretRef.Name, pod.Namespace, pod.Name, err)
			continue
		}

		pullSecrets = append(pullSecrets, *secret)
	}

	return pullSecrets
}

// podStatusIsTerminal reports when the specified pod has no running containers or is no longer accepting
// spec changes.
// containersTerminal：是否所有container的state都处于Terminated或Waiting
// podWorkerTerminal：是否pod的Phase为"Failed"或"Succeeded"，或pod被删除且所有container都处于Terminated或Waiting
func (kl *Kubelet) podAndContainersAreTerminal(pod *v1.Pod) (containersTerminal, podWorkerTerminal bool) {
	// Check the cached pod status which was set after the last sync.
	// 在statusManager中通过pod uid（如果是mirror pod，则通过对应的static pod uid来查找）查找pod status
	status, ok := kl.statusManager.GetPodStatus(pod.UID)
	if !ok {
		// If there is no cached status, use the status from the
		// apiserver. This is useful if kubelet has recently been
		// restarted.
		status = pod.Status
	}
	// A pod transitions into failed or succeeded from either container lifecycle (RestartNever container
	// fails) or due to external events like deletion or eviction. A terminal pod *should* have no running
	// containers, but to know that the pod has completed its lifecycle you must wait for containers to also
	// be terminal.
	// 是否所有container的state都处于Terminated或Waiting
	containersTerminal = notRunning(status.ContainerStatuses)
	// The kubelet must accept config changes from the pod spec until it has reached a point where changes would
	// have no effect on any running container.
	// pod work终止了，当pod的Phase为"Failed"或"Succeeded"，或pod被删除状态且所有container都处于Terminated或Waiting
	podWorkerTerminal = status.Phase == v1.PodFailed || status.Phase == v1.PodSucceeded || (pod.DeletionTimestamp != nil && containersTerminal)
	return
}

// podIsTerminated returns true if the provided pod is in a terminal phase ("Failed", "Succeeded") or
// has been deleted and has no running containers. This corresponds to when a pod must accept changes to
// its pod spec (e.g. terminating containers allow grace period to be shortened).
// 是否pod的Phase为"Failed"或"Succeeded"，或pod被删除且所有container都处于Terminated或Waiting
func (kl *Kubelet) podIsTerminated(pod *v1.Pod) bool {
	_, podWorkerTerminal := kl.podAndContainersAreTerminal(pod)
	return podWorkerTerminal
}

// IsPodTerminated returns true if the pod with the provided UID is in a terminal phase ("Failed",
// "Succeeded") or has been deleted and has no running containers. This corresponds to when a pod must
// accept changes to its pod spec (e.g. terminating containers allow grace period to be shortened)
// 返回为true的情况（下面是或）
// pod manager里没有发现uid的pod
// pod的Phase为"Failed"或"Succeeded"
// pod被删除且所有container都处于Terminated或Waiting
func (kl *Kubelet) IsPodTerminated(uid types.UID) bool {
	pod, podFound := kl.podManager.GetPodByUID(uid)
	// pod manager里没有发现uid的pod，返回true
	if !podFound {
		return true
	}
	// 是否pod的Phase为"Failed"或"Succeeded"，或pod被删除且所有container都处于Terminated或Waiting
	return kl.podIsTerminated(pod)
}

// IsPodDeleted returns true if the pod is deleted.  For the pod to be deleted, either:
// 1. The pod object is deleted
// 2. The pod's status is evicted
// 3. The pod's deletion timestamp is set, and containers are not running
// 返回为true的情况（下面是或）
// pod manager里没有发现uid的pod
// pod的Phase为"Failed"且reason为"Evicted"
// pod被删除且所有container的state都处于Terminated或Waiting
func (kl *Kubelet) IsPodDeleted(uid types.UID) bool {
	pod, podFound := kl.podManager.GetPodByUID(uid)
	// pod manager里没有发现uid的pod，返回true
	if !podFound {
		return true
	}
	status, statusFound := kl.statusManager.GetPodStatus(pod.UID)
	// 在status manager里没有发现status，则以pod.Status为准
	if !statusFound {
		status = pod.Status
	}
	// pod的Phase为"Failed"且reason为"Evicted"，或pod被删除且所有container的state都处于Terminated或Waiting
	return eviction.PodIsEvicted(status) || (pod.DeletionTimestamp != nil && notRunning(status.ContainerStatuses))
}

// PodResourcesAreReclaimed returns true if all required node-level resources that a pod was consuming have
// been reclaimed by the kubelet.  Reclaiming resources is a prerequisite to deleting a pod from the API server.
// pod资源已经被回收，下面条件全都要满足
// 提供的status里的container都不再运行
// kl.podCache里没有container运行状态
// pod在node上没有已挂载的volume
// 如果启用了CgroupsPerQOS，则pod的cgroup路径在所有cgroup子系统必须都不存在
func (kl *Kubelet) PodResourcesAreReclaimed(pod *v1.Pod, status v1.PodStatus) bool {
	// 不是所有container的state都处于Terminated或Waiting
	if !notRunning(status.ContainerStatuses) {
		// We shouldn't delete pods that still have running containers
		klog.V(3).Infof("Pod %q is terminated, but some containers are still running", format.Pod(pod))
		return false
	}
	// pod's containers should be deleted
	runtimeStatus, err := kl.podCache.Get(pod.UID)
	if err != nil {
		klog.V(3).Infof("Pod %q is terminated, Error getting runtimeStatus from the podCache: %s", format.Pod(pod), err)
		return false
	}
	if len(runtimeStatus.ContainerStatuses) > 0 {
		var statusStr string
		for _, status := range runtimeStatus.ContainerStatuses {
			statusStr += fmt.Sprintf("%+v ", *status)
		}
		klog.V(3).Infof("Pod %q is terminated, but some containers have not been cleaned up: %s", format.Pod(pod), statusStr)
		return false
	}
	// 判断pod是否在node上存在已挂载的volume
	if kl.podVolumesExist(pod.UID) && !kl.keepTerminatedPodVolumes {
		// We shouldn't delete pods whose volumes have not been cleaned up if we are not keeping terminated pod volumes
		klog.V(3).Infof("Pod %q is terminated, but some volumes have not been cleaned up", format.Pod(pod))
		return false
	}
	if kl.kubeletConfiguration.CgroupsPerQOS {
		// 判断pod的cgroup路径是否存在，比如cpu子系统对应的/sys/fs/cgroup/cpu/kubepods.slice/kubepods-besteffort.slice/kubepods-besteffort-pod6464569d_54d9_4367_a791_745b44623b7b.slice
		pcm := kl.containerManager.NewPodContainerManager()
		// 在所有cgroup子系统中，pod的cgroup路径都存在
		if pcm.Exists(pod) {
			klog.V(3).Infof("Pod %q is terminated, but pod cgroup sandbox has not been cleaned up", format.Pod(pod))
			return false
		}
	}
	return true
}

// podResourcesAreReclaimed simply calls PodResourcesAreReclaimed with the most up-to-date status.
// 判断pod资源是否已经被回收
// 先从status manager中podStatuses缓存中获取uid的相关的pod的status，（如果是mirror pod uid则通过static pod uid来查找）
// 没有找到则使用提供的pod的status
func (kl *Kubelet) podResourcesAreReclaimed(pod *v1.Pod) bool {
	// 从status manager中podStatuses缓存中获取uid的相关的pod的status，（如果是mirror pod uid则通过static pod uid来查找）
	status, ok := kl.statusManager.GetPodStatus(pod.UID)
	if !ok {
		status = pod.Status
	}
	// pod资源已经被回收，下面条件全都要满足
	// 提供的status里的container都不再运行
	// kl.podCache里没有container运行状态
	// pod在node上没有已挂载的volume
	// 如果启用了CgroupsPerQOS，则pod的cgroup路径在所有cgroup子系统必须都不存在
	return kl.PodResourcesAreReclaimed(pod, status)
}

// notRunning returns true if every status is terminated or waiting, or the status list
// is empty.
// 所有container的state都处于Terminated或Waiting
func notRunning(statuses []v1.ContainerStatus) bool {
	for _, status := range statuses {
		if status.State.Terminated == nil && status.State.Waiting == nil {
			return false
		}
	}
	return true
}

// filterOutTerminatedPods returns the given pods which the status manager
// does not consider failed or succeeded.
// 返回所有pods中，pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除或pod被删除且至少一个container为running
func (kl *Kubelet) filterOutTerminatedPods(pods []*v1.Pod) []*v1.Pod {
	var filteredPods []*v1.Pod
	for _, p := range pods {
		// 是否pod的Phase为"Failed"或"Succeeded"，或pod被删除且所有container都处于Terminated或Waiting
		if kl.podIsTerminated(p) {
			continue
		}
		filteredPods = append(filteredPods, p)
	}
	return filteredPods
}

// removeOrphanedPodStatuses removes obsolete entries in podStatus where
// the pod is no longer considered bound to this node.
// kl.statusManager.podStatuses里的uid不在podUIDs（所有mirror pod和static pod和普通pod）里，则从kl.statusManager.podStatuses删除这个uid
func (kl *Kubelet) removeOrphanedPodStatuses(pods []*v1.Pod, mirrorPods []*v1.Pod) {
	podUIDs := make(map[types.UID]bool)
	for _, pod := range pods {
		podUIDs[pod.UID] = true
	}
	for _, pod := range mirrorPods {
		podUIDs[pod.UID] = true
	}
	kl.statusManager.RemoveOrphanedStatuses(podUIDs)
}

// HandlePodCleanups performs a series of cleanup work, including terminating
// pod workers, killing unwanted pods, and removing orphaned volumes/pod
// directories.
// NOTE: This function is executed by the main sync loop, so it
// should not contain any blocking calls.
// 停止不存在的pod的pod worker，并停止周期性执行container的readniess、liveness、start probe的worker
// 移除不存在pod的容器（容器还在运行，但是不在apiserver中）
// 移除不存在pod的volume和pod目录，当"/var/lib/kubelet/pods/{podUID}/volume"或volume-subpaths目录存在，则记录错误日志
// 从apiserver中删除所有没有对应static pod的mirror pod
// 启用cgroupsPerQOS，则处理cgroup路径下不存在的pod
//   如果pod还存在volume，且不启用keepTerminatedPodVolumes，则pod的cpu cgroup里的cpu share的值为2
//   否则启动一个goroutine 移除所有cgroup子系统里的cgroup路径
// 容器重启的回退周期清理，清理开始回退时间离现在已经超过2倍最大回收周期的item
func (kl *Kubelet) HandlePodCleanups() error {
	// The kubelet lacks checkpointing, so we need to introspect the set of pods
	// in the cgroup tree prior to inspecting the set of pods in our pod manager.
	// this ensures our view of the cgroup tree does not mistakenly observe pods
	// that are added after the fact...
	var (
		cgroupPods map[types.UID]cm.CgroupName
		err        error
	)
	// kl.cgroupsPerQOS默认为true
	if kl.cgroupsPerQOS {
		pcm := kl.containerManager.NewPodContainerManager()
		// 遍历所有pod的cgroup目录，获得pod uid与对应的CgroupName
		// 比如"ec7bb47a-07ef-48ff-9201-687474994eab"对应["kubepods", "besteffort", "podec7bb47a-07ef-48ff-9201-687474994eab"]
		cgroupPods, err = pcm.GetAllPodsFromCgroups()
		if err != nil {
			return fmt.Errorf("failed to get list of pods that still exist on cgroup mounts: %v", err)
		}
	}

	// 返回普通pod和static pod、mirror pod
	allPods, mirrorPods := kl.podManager.GetPodsAndMirrorPods()
	// Pod phase progresses monotonically. Once a pod has reached a final state,
	// it should never leave regardless of the restart policy. The statuses
	// of such pods should not be changed, and there is no need to sync them.
	// TODO: the logic here does not handle two cases:
	//   1. If the containers were removed immediately after they died, kubelet
	//      may fail to generate correct statuses, let alone filtering correctly.
	//   2. If kubelet restarted before writing the terminated status for a pod
	//      to the apiserver, it could still restart the terminated pod (even
	//      though the pod was not considered terminated by the apiserver).
	// These two conditions could be alleviated by checkpointing kubelet.
	// 返回所有pods中，pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除或pod被删除且至少一个container为running
	activePods := kl.filterOutTerminatedPods(allPods)

	desiredPods := make(map[types.UID]sets.Empty)
	for _, pod := range activePods {
		desiredPods[pod.UID] = sets.Empty{}
	}
	// Stop the workers for no-longer existing pods.
	// TODO: is here the best place to forget pod workers?
	// kl.podWorkers.podUpdates（UpdatePodOptions chan）里的pod uid不在desiredPods，则关闭这个chan，并从p.podUpdates中移除。在kl.podWorkers.lastUndeliveredWorkUpdate里有pod未处理的事件，直接删除
	kl.podWorkers.ForgetNonExistingPodWorkers(desiredPods)
	// kl.probeManager.workers中的key的pod uid不在desiredPods中，则发送信号给worker.stopCh，让周期执行的probe停止 
	kl.probeManager.CleanupPods(desiredPods)

	// 缓存未过期，则返回缓存中（r.pods）的pod
	// 缓存过期了，则从runtime中获得所有的running pod，并更新r.pods和r.cacheTime
	runningPods, err := kl.runtimeCache.GetPods()
	if err != nil {
		klog.Errorf("Error listing containers: %#v", err)
		return err
	}
	// 移除不存在的pod（容器还在运行，但是不在apiserver中）
	// 发送PodPair消息给kl.podKillingCh，让kl.podkiller进行移除pod
	for _, pod := range runningPods {
		if _, found := desiredPods[pod.ID]; !found {
			kl.podKillingCh <- &kubecontainer.PodPair{APIPod: nil, RunningPod: pod}
		}
	}

	// kl.statusManager.podStatuses里的uid不在podUIDs（所有mirror pod和static pod和普通pod）里，则从kl.statusManager.podStatuses删除这个uid
	kl.removeOrphanedPodStatuses(allPods, mirrorPods)
	// Note that we just killed the unwanted pods. This may not have reflected
	// in the cache. We need to bypass the cache to get the latest set of
	// running pods to clean up the volumes.
	// TODO: Evaluate the performance impact of bypassing the runtime cache.
	// 直接通过runtime获取所有在运行的pods
	runningPods, err = kl.containerRuntime.GetPods(false)
	if err != nil {
		klog.Errorf("Error listing containers: %#v", err)
		return err
	}

	// Remove any orphaned volumes.
	// Note that we pass all pods (including terminated pods) to the function,
	// so that we don't remove volumes associated with terminated but not yet
	// deleted pods.
	// pod目录下的pod文件夹的pod（不在在运行的pod或apiserver中的普通pod和static pod）
	// 如果"/var/lib/kubelet/pods/{podUID}/volume"或volume-subpaths目录存在，则记录错误日志，提示“pod不存在，但是pod目录下volume目录存在或还存在挂载”
	// 否则，移除目录"/var/lib/kubelet/pods/{podUID}"
	err = kl.cleanupOrphanedPodDirs(allPods, runningPods)
	if err != nil {
		// We want all cleanup tasks to be run even if one of them failed. So
		// we just log an error here and continue other cleanup tasks.
		// This also applies to the other clean up tasks.
		klog.Errorf("Failed cleaning up orphaned pod directories: %v", err)
	}

	// Remove any orphaned mirror pods.
	// 从apiserver中删除所有没有对应static pod的mirror pod
	kl.podManager.DeleteOrphanedMirrorPods()

	// Remove any cgroups in the hierarchy for pods that are no longer running.
	// 默认cgroupsPerQOS为true
	if kl.cgroupsPerQOS {
		// cgroup路径下的pod不在activePods中
		// 如果pod还存在volume，且不启用keepTerminatedPodVolumes，则pod的cpu cgroup里的cpu share的值为2
		// 否则启动一个goroutine 移除所有cgroup子系统里的cgroup路径
		kl.cleanupOrphanedPodCgroups(cgroupPods, activePods)
	}

	// 清理开始回退时间离现在已经超过2倍最大回收周期的item（格式{pod name}_{pod namespace}_{pod uid}_{container name}_{container hash）
	// 即超过最大回退周期2倍的容器，重启的回退周期重置
	kl.backOff.GC()
	return nil
}

// podKiller launches a goroutine to kill a pod received from the channel if
// another goroutine isn't already in action.
// 从kl.podKillingCh取出一条pod删除消息，判断是否重复消息。非重复消息，则启动一个goroutine，执行kl.killPod
// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
func (kl *Kubelet) podKiller() {
	killing := sets.NewString()
	// guard for the killing set
	lock := sync.Mutex{}
	for podPair := range kl.podKillingCh {
		runningPod := podPair.RunningPod
		apiPod := podPair.APIPod

		lock.Lock()
		exists := killing.Has(string(runningPod.ID))
		if !exists {
			// 不在killing的set里，添加到killing里。有可能重复pod在kl.podKillingCh
			killing.Insert(string(runningPod.ID))
		}
		lock.Unlock()
		// 已经在killing里的pod，代表已经执行过killpod，则不做任何东西

		if !exists {
			// 启动一个goroutine来执行kl.killPod
			go func(apiPod *v1.Pod, runningPod *kubecontainer.Pod) {
				klog.V(2).Infof("Killing unwanted pod %q", runningPod.Name)
				// 1. 停止pod里所有的container，每个container都启动一个goroutine进行killContainer（执行prestop和stop container）
				// 2. 调用网络插件释放容器的网卡，顺序停止pod里所有的sandbox container
				// 3. 更新Burstable和BestEffort qos class的cpu、memory、hugepage的cgroup目录属性值
				err := kl.killPod(apiPod, runningPod, nil, nil)
				if err != nil {
					klog.Errorf("Failed killing the pod %q: %v", runningPod.Name, err)
				}
				lock.Lock()
				killing.Delete(string(runningPod.ID))
				lock.Unlock()
			}(apiPod, runningPod)
		}
	}
}

// validateContainerLogStatus returns the container ID for the desired container to retrieve logs for, based on the state
// of the container. The previous flag will only return the logs for the last terminated container, otherwise, the current
// running container is preferred over a previous termination. If info about the container is not available then a specific
// error is returned to the end user.
// 根据输入参数，从podStatus中获得container id
func (kl *Kubelet) validateContainerLogStatus(podName string, podStatus *v1.PodStatus, containerName string, previous bool) (containerID kubecontainer.ContainerID, err error) {
	var cID string

	// 从podStatus.ContainerStatuses中，查找containerName的container的container status，返回true代表找到，false代表未找到
	cStatus, found := podutil.GetContainerStatus(podStatus.ContainerStatuses, containerName)
	if !found {
		// 如果未找到，从podStatus.InitContainerStatuses中，查找containerName的container的container status
		cStatus, found = podutil.GetContainerStatus(podStatus.InitContainerStatuses, containerName)
	}
	// 还是没有找到，且启用了"EphemeralContainers"特性，则从podStatus.EphemeralContainerStatuses中，查找containerName的container的container status
	if !found && utilfeature.DefaultFeatureGate.Enabled(features.EphemeralContainers) {
		cStatus, found = podutil.GetContainerStatus(podStatus.EphemeralContainerStatuses, containerName)
	}
	// 都没有找到container status，则返回错误
	if !found {
		return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is not available", containerName, podName)
	}
	lastState := cStatus.LastTerminationState
	waiting, running, terminated := cStatus.State.Waiting, cStatus.State.Running, cStatus.State.Terminated

	switch {
	// 获取之前container的日志
	case previous:
		// lastState.Terminated为nil或ContainerID为空，则返回错误
		if lastState.Terminated == nil || lastState.Terminated.ContainerID == "" {
			return kubecontainer.ContainerID{}, fmt.Errorf("previous terminated container %q in pod %q not found", containerName, podName)
		}
		cID = lastState.Terminated.ContainerID

	// 获取正在运行容器的日志
	case running != nil:
		cID = cStatus.ContainerID

	// 当前容器处于terminated状态
	case terminated != nil:
		// in cases where the next container didn't start, terminated.ContainerID will be empty, so get logs from the lastState.Terminated.
		// terminated的container id为空
		if terminated.ContainerID == "" {
			// 有最后退出容器，则获得最后退出容器的id
			if lastState.Terminated != nil && lastState.Terminated.ContainerID != "" {
				cID = lastState.Terminated.ContainerID
			} else {
				// 没有最后退出的容器，则返回错误
				return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is terminated", containerName, podName)
			}
		} else {
			// terminated的container id不为空，则container id为terminated.ContainerID
			cID = terminated.ContainerID
		}

	// 当前容器不在运行状态、也不在terminated状态，且有最后退出容器状态
	case lastState.Terminated != nil:
		// 最后退出的容器状态为空，则返回错误
		if lastState.Terminated.ContainerID == "" {
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is terminated", containerName, podName)
		}
		// container id为terminated.ContainerID
		cID = lastState.Terminated.ContainerID

	// 当前在运行状态、也不在terminated状态、没有最后退出容器状态，且处于waiting状态
	case waiting != nil:
		// output some info for the most common pending failures
		switch reason := waiting.Reason; reason {
		case images.ErrImagePull.Error():
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start: image can't be pulled", containerName, podName)
		case images.ErrImagePullBackOff.Error():
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start: trying and failing to pull image", containerName, podName)
		default:
			return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start: %v", containerName, podName, reason)
		}
	default:
		// unrecognized state
		return kubecontainer.ContainerID{}, fmt.Errorf("container %q in pod %q is waiting to start - no logs yet", containerName, podName)
	}

	// 根据格式应该是"{type}://{id}"，从字符串解析出ContainerID
	return kubecontainer.ParseContainerID(cID), nil
}

// GetKubeletContainerLogs returns logs from the container
// TODO: this method is returning logs of random container attempts, when it should be returning the most recent attempt
// or all of them.
// 先从kl.PodManager获得pod uid，再从kl.StatusManager里获取container id
// 根据container id
// 如果runtime为docker，且非json-file格式的日志，则dockerLegacyService不为nil
//   执行docker logs {container id}
//   如果容器有tty则拷贝resp到outputStream，否则拷贝resp到outputStream和errorStream
// 如果runtime为cri，则调用cri获得容器的日志路径，读取容器日志文件，发送到stdout或stderr
func (kl *Kubelet) GetKubeletContainerLogs(ctx context.Context, podFullName, containerName string, logOptions *v1.PodLogOptions, stdout, stderr io.Writer) error {
	// Pod workers periodically write status to statusManager. If status is not
	// cached there, something is wrong (or kubelet just restarted and hasn't
	// caught up yet). Just assume the pod is not ready yet.
	// 从"{pod name}_{pod namespace}"，解析出pod name和pod namespace
	name, namespace, err := kubecontainer.ParsePodFullName(podFullName)
	if err != nil {
		return fmt.Errorf("unable to parse pod full name %q: %v", podFullName, err)
	}

	// 从kl.podManager中获得pod
	pod, ok := kl.GetPodByName(namespace, name)
	if !ok {
		return fmt.Errorf("pod %q cannot be found - no logs available", name)
	}

	podUID := pod.UID
	// 根据static pod获取mirror pod
	if mirrorPod, ok := kl.podManager.GetMirrorPodByPod(pod); ok {
		podUID = mirrorPod.UID
	}
	// 从kl.statusManager获得v1.PodStatus
	podStatus, found := kl.statusManager.GetPodStatus(podUID)
	if !found {
		// If there is no cached status, use the status from the
		// apiserver. This is useful if kubelet has recently been
		// restarted.
		// 没有从kl.statusManager获得v1.PodStatus，则使用apiserver里的v1.PodStatus
		podStatus = pod.Status
	}

	// TODO: Consolidate the logic here with kuberuntime.GetContainerLogs, here we convert container name to containerID,
	// but inside kuberuntime we convert container id back to container name and restart count.
	// TODO: After separate container log lifecycle management, we should get log based on the existing log files
	// instead of container status.
	// 根据输入参数，从podStatus中获得container id
	containerID, err := kl.validateContainerLogStatus(pod.Name, &podStatus, containerName, logOptions.Previous)
	if err != nil {
		return err
	}

	// Do a zero-byte write to stdout before handing off to the container runtime.
	// This ensures at least one Write call is made to the writer when copying starts,
	// even if we then block waiting for log output from the container.
	if _, err := stdout.Write([]byte{}); err != nil {
		return err
	}

	// runtime为docker，且非json-file格式的日志，则dockerLegacyService不为nil
	if kl.dockerLegacyService != nil {
		// dockerLegacyService should only be non-nil when we actually need it, so
		// inject it into the runtimeService.
		// TODO(random-liu): Remove this hack after deprecating unsupported log driver.
		// 执行docker logs {container id}
		// 如果容器有tty则拷贝resp到outputStream，否则拷贝resp到outputStream和errorStream
		return kl.dockerLegacyService.GetContainerLogs(ctx, pod, containerID, logOptions, stdout, stderr)
	}
	// 调用cri获得容器的日志路径，读取容器日志文件，发送到stdout或stderr
	return kl.containerRuntime.GetContainerLogs(ctx, pod, containerID, logOptions, stdout, stderr)
}

// getPhase returns the phase of a pod given its container info.
func getPhase(spec *v1.PodSpec, info []v1.ContainerStatus) v1.PodPhase {
	pendingInitialization := 0
	failedInitialization := 0
	for _, container := range spec.InitContainers {
		// 返回container的container status，ok为true代表找到，ok为false代表未找到
		containerStatus, ok := podutil.GetContainerStatus(info, container.Name)
		if !ok {
			pendingInitialization++
			continue
		}

		switch {
		case containerStatus.State.Running != nil:
			// 处于running状态
			pendingInitialization++
		case containerStatus.State.Terminated != nil:
			if containerStatus.State.Terminated.ExitCode != 0 {
				// 处于Terminated且exitcode不为0
				failedInitialization++
			}
		case containerStatus.State.Waiting != nil:
			if containerStatus.LastTerminationState.Terminated != nil {
				if containerStatus.LastTerminationState.Terminated.ExitCode != 0 {
					// container处于waiting状态（重启中），且上个容器LastTerminationState处于Terminated，且exitcode不为0
					failedInitialization++
				}
			} else {
				// container处于waiting状态（重启中），且上个容器LastTerminationState处于非Terminated
				pendingInitialization++
			}
		default:
			pendingInitialization++
		}
	}

	unknown := 0
	running := 0
	waiting := 0
	stopped := 0
	succeeded := 0
	for _, container := range spec.Containers {
		containerStatus, ok := podutil.GetContainerStatus(info, container.Name)
		if !ok {
			unknown++
			continue
		}

		switch {
		case containerStatus.State.Running != nil:
			running++
		case containerStatus.State.Terminated != nil:
			stopped++
			if containerStatus.State.Terminated.ExitCode == 0 {
				succeeded++
			}
		case containerStatus.State.Waiting != nil:
			if containerStatus.LastTerminationState.Terminated != nil {
				stopped++
			} else {
				waiting++
			}
		default:
			unknown++
		}
	}

	if failedInitialization > 0 && spec.RestartPolicy == v1.RestartPolicyNever {
		return v1.PodFailed
	}

	switch {
	case pendingInitialization > 0:
		fallthrough
	case waiting > 0:
		klog.V(5).Infof("pod waiting > 0, pending")
		// One or more containers has not been started
		return v1.PodPending
	case running > 0 && unknown == 0:
		// All containers have been started, and at least
		// one container is running
		return v1.PodRunning
	case running == 0 && stopped > 0 && unknown == 0:
		// All containers are terminated
		if spec.RestartPolicy == v1.RestartPolicyAlways {
			// All containers are in the process of restarting
			return v1.PodRunning
		}
		if stopped == succeeded {
			// RestartPolicy is not Always, and all
			// containers are terminated in success
			return v1.PodSucceeded
		}
		if spec.RestartPolicy == v1.RestartPolicyNever {
			// RestartPolicy is Never, and all containers are
			// terminated with at least one in failure
			return v1.PodFailed
		}
		// RestartPolicy is OnFailure, and at least one in failure
		// and in the process of restarting
		return v1.PodRunning
	default:
		klog.V(5).Infof("pod default case, pending")
		return v1.PodPending
	}
}

// generateAPIPodStatus creates the final API pod status for a pod, given the
// internal pod status.
// 根据runtime的status生成pod status
func (kl *Kubelet) generateAPIPodStatus(pod *v1.Pod, podStatus *kubecontainer.PodStatus) v1.PodStatus {
	klog.V(3).Infof("Generating status for %q", format.Pod(pod))

	// 将pod的runtime status（kubecontainer.PodStatus）转成*v1.PodStatus，同时保留不是kubelet控制的condition（readiness gateway）
	s := kl.convertStatusToAPIStatus(pod, podStatus)

	// check if an internal module has requested the pod is evicted.
	// 目前只有一个activeDeadlineHandler，在pkg\kubelet\active_deadline.go
	for _, podSyncHandler := range kl.PodSyncHandlers {
		// 如果是activeDeadlineHandler
		// 判断pod的active时间长度没有超过pod.Spec.ActiveDeadlineSeconds，超过了则会返回false
		if result := podSyncHandler.ShouldEvict(pod); result.Evict {
			s.Phase = v1.PodFailed
			s.Reason = result.Reason
			s.Message = result.Message
			return *s
		}
	}

	// Assume info is ready to process
	spec := &pod.Spec
	allStatus := append(append([]v1.ContainerStatus{}, s.ContainerStatuses...), s.InitContainerStatuses...)
	// 根据pod的里的container status来判断pod的Phase
	s.Phase = getPhase(spec, allStatus)
	// Check for illegal phase transition
	if pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
		// API server shows terminal phase; transitions are not allowed
		// apiserver里的pod的Phase为"Failed"或"Succeeded"，则以apiserver的pod的Phase为主
		if s.Phase != pod.Status.Phase {
			klog.Errorf("Pod attempted illegal phase transition from %s to %s: %v", pod.Status.Phase, s.Phase, s)
			// Force back to phase from the API server
			s.Phase = pod.Status.Phase
		}
	}
	// 根据probe状态，设置podStatus里的containerstatus的started和ready字段，和根据init container处于Terminated状态且exitcode为0 设置initContainerstatus里的ready字段
	kl.probeManager.UpdatePodStatus(pod.UID, s)
	// 根据initcontainer status的Ready字段，设置type为"Initialized"的condition
	s.Conditions = append(s.Conditions, status.GeneratePodInitializedCondition(spec, s.InitContainerStatuses, s.Phase))
	// 如果所有container为ready且ReadinessGates为ready则type为"Ready"的condition为"True"
	s.Conditions = append(s.Conditions, status.GeneratePodReadyCondition(spec, s.Conditions, s.ContainerStatuses, s.Phase))
	// 生成type为"ContainersReady"的condition，这个在status.GeneratePodReadyCondition里也会调用
	s.Conditions = append(s.Conditions, status.GenerateContainersReadyCondition(spec, s.ContainerStatuses, s.Phase))
	// Status manager will take care of the LastTransitionTimestamp, either preserve
	// the timestamp from apiserver, or set a new one. When kubelet sees the pod,
	// `PodScheduled` condition must be true.
	// 生成type为"PodScheduled"的condition，而LastTransitionTimestamp会在status manager中的updateStatusInternal中被设置
	s.Conditions = append(s.Conditions, v1.PodCondition{
		Type:   v1.PodScheduled,
		Status: v1.ConditionTrue,
	})

	if kl.kubeClient != nil {
		// 获取kubelet的ip
		hostIP, err := kl.getHostIPAnyWay()
		if err != nil {
			klog.V(4).Infof("Cannot get host IP: %v", err)
		} else {
			// 设置pod的HostIP
			s.HostIP = hostIP.String()
			// 如果pod网络是Host模式，则设置pod的PodIP和PodIPs为宿主机ip
			if kubecontainer.IsHostNetworkPod(pod) && s.PodIP == "" {
				s.PodIP = hostIP.String()
				s.PodIPs = []v1.PodIP{{IP: s.PodIP}}
			}
		}
	}

	return *s
}

// convertStatusToAPIStatus creates an api PodStatus for the given pod from
// the given internal pod status.  It is purely transformative and does not
// alter the kubelet state at all.
// 将pod的runtime status（kubecontainer.PodStatus）转成*v1.PodStatus，同时保留不是kubelet控制的condition（readiness gateway）
func (kl *Kubelet) convertStatusToAPIStatus(pod *v1.Pod, podStatus *kubecontainer.PodStatus) *v1.PodStatus {
	var apiPodStatus v1.PodStatus
	apiPodStatus.PodIPs = make([]v1.PodIP, 0, len(podStatus.IPs))
	for _, ip := range podStatus.IPs {
		apiPodStatus.PodIPs = append(apiPodStatus.PodIPs, v1.PodIP{
			IP: ip,
		})
	}

	if len(apiPodStatus.PodIPs) > 0 {
		apiPodStatus.PodIP = apiPodStatus.PodIPs[0].IP
	}
	// set status for Pods created on versions of kube older than 1.6
	apiPodStatus.QOSClass = v1qos.GetPodQOS(pod)

	// 从status manager获取pod状态
	oldPodStatus, found := kl.statusManager.GetPodStatus(pod.UID)
	if !found {
		oldPodStatus = pod.Status
	}

	// 转换runtime container的status为[]v1.ContainerStatus
	apiPodStatus.ContainerStatuses = kl.convertToAPIContainerStatuses(
		pod, podStatus,
		oldPodStatus.ContainerStatuses,
		pod.Spec.Containers,
		len(pod.Spec.InitContainers) > 0,
		false,
	)
	// 转换runtime initcontainer的status为[]v1.ContainerStatus
	apiPodStatus.InitContainerStatuses = kl.convertToAPIContainerStatuses(
		pod, podStatus,
		oldPodStatus.InitContainerStatuses,
		pod.Spec.InitContainers,
		len(pod.Spec.InitContainers) > 0,
		true,
	)
	if utilfeature.DefaultFeatureGate.Enabled(features.EphemeralContainers) {
		var ecSpecs []v1.Container
		for i := range pod.Spec.EphemeralContainers {
			ecSpecs = append(ecSpecs, v1.Container(pod.Spec.EphemeralContainers[i].EphemeralContainerCommon))
		}

		// #80875: By now we've iterated podStatus 3 times. We could refactor this to make a single
		// pass through podStatus.ContainerStatuses
		// 转换runtime Ephemeralcontainer的status为[]v1.ContainerStatus
		apiPodStatus.EphemeralContainerStatuses = kl.convertToAPIContainerStatuses(
			pod, podStatus,
			oldPodStatus.EphemeralContainerStatuses,
			ecSpecs,
			len(pod.Spec.InitContainers) > 0,
			false,
		)
	}

	// Preserves conditions not controlled by kubelet
	for _, c := range pod.Status.Conditions {
		if !kubetypes.PodConditionByKubelet(c.Type) {
			apiPodStatus.Conditions = append(apiPodStatus.Conditions, c)
		}
	}

	return &apiPodStatus
}

// convertToAPIContainerStatuses converts the given internal container
// statuses into API container statuses.
// 转换runtime container的status为[]v1.ContainerStatus
func (kl *Kubelet) convertToAPIContainerStatuses(pod *v1.Pod, podStatus *kubecontainer.PodStatus, previousStatus []v1.ContainerStatus, containers []v1.Container, hasInitContainers, isInitContainer bool) []v1.ContainerStatus {
	// 将runtime的container status（*kubecontainer.ContainerStatus）转成*v1.ContainerStatus
	convertContainerStatus := func(cs *kubecontainer.ContainerStatus) *v1.ContainerStatus {
		cid := cs.ID.String()
		status := &v1.ContainerStatus{
			Name:         cs.Name,
			RestartCount: int32(cs.RestartCount),
			Image:        cs.Image,
			ImageID:      cs.ImageID,
			ContainerID:  cid,
		}
		switch cs.State {
		case kubecontainer.ContainerStateRunning:
			status.State.Running = &v1.ContainerStateRunning{StartedAt: metav1.NewTime(cs.StartedAt)}
		case kubecontainer.ContainerStateCreated:
			// Treat containers in the "created" state as if they are exited.
			// The pod workers are supposed start all containers it creates in
			// one sync (syncPod) iteration. There should not be any normal
			// "created" containers when the pod worker generates the status at
			// the beginning of a sync iteration.
			fallthrough
		case kubecontainer.ContainerStateExited:
			status.State.Terminated = &v1.ContainerStateTerminated{
				ExitCode:    int32(cs.ExitCode),
				Reason:      cs.Reason,
				Message:     cs.Message,
				StartedAt:   metav1.NewTime(cs.StartedAt),
				FinishedAt:  metav1.NewTime(cs.FinishedAt),
				ContainerID: cid,
			}
		default:
			status.State.Waiting = &v1.ContainerStateWaiting{}
		}
		return status
	}

	// Fetch old containers statuses from old pod status.
	oldStatuses := make(map[string]v1.ContainerStatus, len(containers))
	for _, status := range previousStatus {
		oldStatuses[status.Name] = status
	}

	// Set all container statuses to default waiting state
	statuses := make(map[string]*v1.ContainerStatus, len(containers))
	defaultWaitingState := v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	if hasInitContainers {
		defaultWaitingState = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "PodInitializing"}}
	}

	// 先遍历一遍pod里的containers，设置container的默认的status
	for _, container := range containers {
		status := &v1.ContainerStatus{
			Name:  container.Name,
			Image: container.Image,
			State: defaultWaitingState,
		}
		oldStatus, found := oldStatuses[container.Name]
		if found {
			if oldStatus.State.Terminated != nil {
				// Do not update status on terminated init containers as
				// they be removed at any time.
				// 不再更新container状态为Terminated的status
				status = &oldStatus
			} else {
				// Apply some values from the old statuses as the default values.
				status.RestartCount = oldStatus.RestartCount
				status.LastTerminationState = oldStatus.LastTerminationState
			}
		}
		statuses[container.Name] = status
	}

	// 再将遍历pod的runtime status（podStatus.ContainerStatuses）里所有container status，替换statuses里相应状态

	// Make the latest container status comes first.
	// 根据创建时间进行反向排序，可以让一个pod中的container name对应多个容器，只处理第一个pod中的container name容器状态，最后的创建的排在前面会被处理
	sort.Sort(sort.Reverse(kubecontainer.SortContainerStatusesByCreationTime(podStatus.ContainerStatuses)))
	// Set container statuses according to the statuses seen in pod status
	containerSeen := map[string]int{}
	for _, cStatus := range podStatus.ContainerStatuses {
		cName := cStatus.Name
		if _, ok := statuses[cName]; !ok {
			// This would also ignore the infra container.
			// 当转换的是普通 container状态时，（忽略sandbox容器状态）和initContainer状态和EphemeralContainer
			// 当isInitContainer为true，意味着转换的是initcontainer状态，忽略普通容器和（sanbox容器）和EphemeralContainer
			// 当转换EphemeralContainer状态时，忽略普通容器和（sanbox容器）和initcontainer
			continue
		}
		// 有多个退出的容器，这个时候一个pod中的container name对应多个容器
		// 这里已经是至少第三个cName容器了
		if containerSeen[cName] >= 2 {
			continue
		}
		status := convertContainerStatus(cStatus)
		if containerSeen[cName] == 0 {
			// 第一次遇到这个容器名，直接设置为这个容器状态
			statuses[cName] = status
		} else {
			// 第二次遇到这个容器名，那么这个container status为最后退出的容器状态
			statuses[cName].LastTerminationState = status.State
		}
		containerSeen[cName] = containerSeen[cName] + 1
	}

	// 处理应该被重启的容器状态为Waiting

	// Handle the containers failed to be started, which should be in Waiting state.
	for _, container := range containers {
		if isInitContainer {
			// If the init container is terminated with exit code 0, it won't be restarted.
			// TODO(random-liu): Handle this in a cleaner way.
			// 从podStatus.ContainerStatuses里，返回第一个名为container.Name的ContainerStatus
			s := podStatus.FindContainerStatusByName(container.Name)
			if s != nil && s.State == kubecontainer.ContainerStateExited && s.ExitCode == 0 {
				// initcontainer已经处于"exited"退出状态，则忽略
				continue
			}
		}
		// If a container should be restarted in next syncpod, it is *Waiting*.
		// container不需要被重启
		if !kubecontainer.ShouldContainerBeRestarted(&container, pod, podStatus) {
			continue
		}
		status := statuses[container.Name]
		// 从reasonCache中获取container 运行的reason和error message
		reason, ok := kl.reasonCache.Get(pod.UID, container.Name)
		if !ok {
			// In fact, we could also apply Waiting state here, but it is less informative,
			// and the container will be restarted soon, so we prefer the original state here.
			// Note that with the current implementation of ShouldContainerBeRestarted the original state here
			// could be:
			//   * Waiting: There is no associated historical container and start failure reason record.
			//   * Terminated: The container is terminated.
			// 获取不到数据，直接忽略
			continue
		}
		// container处于Terminated状态，需要设置LastTerminationState，则设置status.LastTerminationState为status.State
		// 这里会覆盖上面container.Name有多个container运行时状态，设置第二个额container状态为LastTerminationState
		if status.State.Terminated != nil {
			status.LastTerminationState = status.State
		}
		// 设置container的状态为Waiting
		status.State = v1.ContainerState{
			Waiting: &v1.ContainerStateWaiting{
				Reason:  reason.Err.Error(),
				Message: reason.Message,
			},
		}
		statuses[container.Name] = status
	}

	var containerStatuses []v1.ContainerStatus
	for _, status := range statuses {
		containerStatuses = append(containerStatuses, *status)
	}

	// Sort the container statuses since clients of this interface expect the list
	// of containers in a pod has a deterministic order.
	if isInitContainer {
		kubetypes.SortInitContainerStatuses(pod, containerStatuses)
	} else {
		sort.Sort(kubetypes.SortedContainerStatuses(containerStatuses))
	}
	return containerStatuses
}

// ServeLogs returns logs of current machine.
// 提供访问节点上日志（/var/log/目录）
func (kl *Kubelet) ServeLogs(w http.ResponseWriter, req *http.Request) {
	// TODO: whitelist logs we are willing to serve
	kl.logServer.ServeHTTP(w, req)
}

// findContainer finds and returns the container with the given pod ID, full name, and container name.
// It returns nil if not found.
// 从runtime中查找属于podFullName下的container name为containerName
func (kl *Kubelet) findContainer(podFullName string, podUID types.UID, containerName string) (*kubecontainer.Container, error) {
	// 从runtime中获得所有pod的容器列表（运行中）
	pods, err := kl.containerRuntime.GetPods(false)
	if err != nil {
		return nil, err
	}
	// Resolve and type convert back again.
	// We need the static pod UID but the kubecontainer API works with types.UID.
	// 首先尝试将uid转成MirrorPodUID，然后通过这个id在kl.podManager.translationByUID中查找static uid
	// 如果未找到，则返回原始的uid
	podUID = types.UID(kl.podManager.TranslatePodUID(podUID))
	// podFullName不为空，则从根据podFullName查找pod
	// 否则，根据pod uid查找pod
	pod := kubecontainer.Pods(pods).FindPod(podFullName, podUID)
	// 根据containerName在pod.Containers中查找container
	return pod.FindContainerByName(containerName), nil
}

// RunInContainer runs a command in a container, returns the combined stdout, stderr as an array of bytes
func (kl *Kubelet) RunInContainer(podFullName string, podUID types.UID, containerName string, cmd []string) ([]byte, error) {
	// 从runtime中查找属于podFullName下的container name为containerName
	container, err := kl.findContainer(podFullName, podUID, containerName)
	if err != nil {
		return nil, err
	}
	if container == nil {
		return nil, fmt.Errorf("container not found (%q)", containerName)
	}
	// TODO(tallclair): Pass a proper timeout value.
	// 调用runtime（同步调用）执行cmd，设置执行的timeout，并将stderr append到stdout（也就是说命令输出没有保证顺序）
	// 当timeout为0时候，执行m.runtimeService.ExecSync没有超时时间。
	// 对于dockershim来说（服务端来说），timeout这个参数会传给dockershim，但是dockershim没有使用。
	return kl.runner.RunInContainer(container.ID, cmd, 0)
}

// GetExec gets the URL the exec will be served from, or nil if the Kubelet will serve it.
// 调用cri接口，获得并返回执行exec的url
func (kl *Kubelet) GetExec(podFullName string, podUID types.UID, containerName string, cmd []string, streamOpts remotecommandserver.Options) (*url.URL, error) {
	// 从runtime中查找属于podFullName下的container name为containerName的container
	container, err := kl.findContainer(podFullName, podUID, containerName)
	if err != nil {
		return nil, err
	}
	if container == nil {
		return nil, fmt.Errorf("container not found (%q)", containerName)
	}
	// 调用cri接口，返回执行exec的url
	return kl.streamingRuntime.GetExec(container.ID, cmd, streamOpts.Stdin, streamOpts.Stdout, streamOpts.Stderr, streamOpts.TTY)
}

// GetAttach gets the URL the attach will be served from, or nil if the Kubelet will serve it.
func (kl *Kubelet) GetAttach(podFullName string, podUID types.UID, containerName string, streamOpts remotecommandserver.Options) (*url.URL, error) {
	container, err := kl.findContainer(podFullName, podUID, containerName)
	if err != nil {
		return nil, err
	}
	if container == nil {
		return nil, fmt.Errorf("container %s not found in pod %s", containerName, podFullName)
	}

	// The TTY setting for attach must match the TTY setting in the initial container configuration,
	// since whether the process is running in a TTY cannot be changed after it has started.  We
	// need the api.Pod to get the TTY status.
	pod, found := kl.GetPodByFullName(podFullName)
	if !found || (string(podUID) != "" && pod.UID != podUID) {
		return nil, fmt.Errorf("pod %s not found", podFullName)
	}
	containerSpec := kubecontainer.GetContainerSpec(pod, containerName)
	if containerSpec == nil {
		return nil, fmt.Errorf("container %s not found in pod %s", containerName, podFullName)
	}
	tty := containerSpec.TTY

	return kl.streamingRuntime.GetAttach(container.ID, streamOpts.Stdin, streamOpts.Stdout, streamOpts.Stderr, tty)
}

// GetPortForward gets the URL the port-forward will be served from, or nil if the Kubelet will serve it.
func (kl *Kubelet) GetPortForward(podName, podNamespace string, podUID types.UID, portForwardOpts portforward.V4Options) (*url.URL, error) {
	pods, err := kl.containerRuntime.GetPods(false)
	if err != nil {
		return nil, err
	}
	// Resolve and type convert back again.
	// We need the static pod UID but the kubecontainer API works with types.UID.
	podUID = types.UID(kl.podManager.TranslatePodUID(podUID))
	podFullName := kubecontainer.BuildPodFullName(podName, podNamespace)
	pod := kubecontainer.Pods(pods).FindPod(podFullName, podUID)
	if pod.IsEmpty() {
		return nil, fmt.Errorf("pod not found (%q)", podFullName)
	}

	return kl.streamingRuntime.GetPortForward(podName, podNamespace, podUID, portForwardOpts.Ports)
}

// cleanupOrphanedPodCgroups removes cgroups that should no longer exist.
// it reconciles the cached state of cgroupPods with the specified list of runningPods
// cgroup路径下的pod不在activePods中
// 如果pod还存在volume，且不启用keepTerminatedPodVolumes，则pod的cpu cgroup里的cpu share的值为2
// 否则启动一个goroutine 移除所有cgroup子系统里的cgroup路径
func (kl *Kubelet) cleanupOrphanedPodCgroups(cgroupPods map[types.UID]cm.CgroupName, activePods []*v1.Pod) {
	// Add all running pods to the set that we want to preserve
	podSet := sets.NewString()
	for _, pod := range activePods {
		podSet.Insert(string(pod.UID))
	}
	pcm := kl.containerManager.NewPodContainerManager()

	// Iterate over all the found pods to verify if they should be running
	// 遍历所有cgroup下的pod
	for uid, val := range cgroupPods {
		// if the pod is in the running set, its not a candidate for cleanup
		if podSet.Has(string(uid)) {
			continue
		}

		// cgroup下的pod不在activePods中
		// If volumes have not been unmounted/detached, do not delete the cgroup
		// so any memory backed volumes don't have their charges propagated to the
		// parent croup.  If the volumes still exist, reduce the cpu shares for any
		// process in the cgroup to the minimum value while we wait.  if the kubelet
		// is configured to keep terminated volumes, we will delete the cgroup and not block.
		// kl.podVolumesExist：
		//   在kl.volumeManager里获取到pod的已经挂载的volume，返回true
		//   kubelet数据目录里pod的volumes目录下存在挂载的目录，或获取pod的volumes目录下存在挂载的目录出现错误，返回true
		// 如果pod还存在volume，且不启用keepTerminatedPodVolumes，则pod的cpu cgroup里的cpu share的值为2，然后继续下一个pod
		if podVolumesExist := kl.podVolumesExist(uid); podVolumesExist && !kl.keepTerminatedPodVolumes {
			klog.V(3).Infof("Orphaned pod %q found, but volumes not yet removed.  Reducing cpu to minimum", uid)
			// 设置cgroup name的cpu share的值为2
			if err := pcm.ReduceCPULimits(val); err != nil {
				klog.Warningf("Failed to reduce cpu time for pod %q pending volume cleanup due to %v", uid, err)
			}
			continue
		}
		klog.V(3).Infof("Orphaned pod %q found, removing pod cgroups", uid)
		// Destroy all cgroups of pod that should not be running,
		// by first killing all the attached processes to these cgroups.
		// We ignore errors thrown by the method, as the housekeeping loop would
		// again try to delete these unwanted pod cgroups
		// kill podCgroup的cgroup子系统的cgroup路径下，所有attached pid（包括子cgroup下attached pid）
		// 移除所有cgroup子系统里的cgroup路径
		go pcm.Destroy(val)
	}
}

// enableHostUserNamespace determines if the host user namespace should be used by the container runtime.
// Returns true if the pod is using a host pid, pic, or network namespace, the pod is using a non-namespaced
// capability, the pod contains a privileged container, or the pod has a host path volume.
//
// NOTE: when if a container shares any namespace with another container it must also share the user namespace
// or it will not have the correct capabilities in the namespace.  This means that host user namespace
// is enabled per pod, not per container.
// 下面条件满足一个返回true
// 只要pod里的一个container的SecurityContext.Privileged为true
// 或pod.Spec.SecurityContext不为为空且pod设置了HostIPC、HostNetwork、HostPID为true
// 或pod里定义了HostPath挂载
// 或container里设置了SecurityContext.Capabilities，且添加了"MKNOD"或"SYS_TIME"或"SYS_MODULE"的Capabilities
// 或pod使用了pvc，且pvc使用了HostPath volume
func (kl *Kubelet) enableHostUserNamespace(pod *v1.Pod) bool {
	// 只要pod里的一个container的SecurityContext.Privileged为true
	// 或pod.Spec.SecurityContext不为为空且pod设置了HostIPC、HostNetwork、HostPID为true
	// 或pod里定义了HostPath挂载
	// 或container里设置了SecurityContext.Capabilities，且添加了"MKNOD"或"SYS_TIME"或"SYS_MODULE"的Capabilities
	// 或pod使用了pvc，且pvc使用了HostPath volume
	if kubecontainer.HasPrivilegedContainer(pod) || hasHostNamespace(pod) ||
		hasHostVolume(pod) || hasNonNamespacedCapability(pod) || kl.hasHostMountPVC(pod) {
		return true
	}
	return false
}

// hasNonNamespacedCapability returns true if MKNOD, SYS_TIME, or SYS_MODULE is requested for any container.
// container里设置了SecurityContext.Capabilities，且添加了"MKNOD"或"SYS_TIME"或"SYS_MODULE"的Capabilities，返回true
func hasNonNamespacedCapability(pod *v1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.SecurityContext != nil && c.SecurityContext.Capabilities != nil {
			for _, cap := range c.SecurityContext.Capabilities.Add {
				if cap == "MKNOD" || cap == "SYS_TIME" || cap == "SYS_MODULE" {
					return true
				}
			}
		}
	}

	return false
}

// hasHostVolume returns true if the pod spec has a HostPath volume.
// pod里定义了HostPath挂载
func hasHostVolume(pod *v1.Pod) bool {
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			return true
		}
	}
	return false
}

// hasHostNamespace returns true if hostIPC, hostNetwork, or hostPID are set to true.
// 如果pod.Spec.SecurityContext为空，返回false
// pod设置了HostIPC、HostNetwork、HostPID为true，则返回true
func hasHostNamespace(pod *v1.Pod) bool {
	if pod.Spec.SecurityContext == nil {
		return false
	}
	return pod.Spec.HostIPC || pod.Spec.HostNetwork || pod.Spec.HostPID
}

// hasHostMountPVC returns true if a PVC is referencing a HostPath volume.
// pod使用了pvc，且pvc使用了HostPath volume，则返回true
func (kl *Kubelet) hasHostMountPVC(pod *v1.Pod) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			pvc, err := kl.kubeClient.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(context.TODO(), volume.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("unable to retrieve pvc %s:%s - %v", pod.Namespace, volume.PersistentVolumeClaim.ClaimName, err)
				continue
			}
			if pvc != nil {
				referencedVolume, err := kl.kubeClient.CoreV1().PersistentVolumes().Get(context.TODO(), pvc.Spec.VolumeName, metav1.GetOptions{})
				if err != nil {
					klog.Warningf("unable to retrieve pv %s - %v", pvc.Spec.VolumeName, err)
					continue
				}
				// pv为host storage类型提供的
				if referencedVolume != nil && referencedVolume.Spec.HostPath != nil {
					return true
				}
			}
		}
	}
	return false
}
