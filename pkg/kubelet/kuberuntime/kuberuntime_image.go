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
	"k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	"k8s.io/klog"
	credentialprovidersecrets "k8s.io/kubernetes/pkg/credentialprovider/secrets"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/util/parsers"
)

// PullImage pulls an image from the network to local storage using the supplied
// secrets if necessary.
// 尝试先从pullSecrets里查找docker仓库凭证，没有找到。则从云厂商provider或文件系统里查找docker仓库凭证。
// 找到凭证则使用凭证进行拉取镜像。上面方式中都没有找到凭证，则不使用凭证进行拉取镜像
// 从云厂商provider或文件系统查找顺序：
// 遍历所有注册并启用的provider提供的凭证（pod的pull secret没有使用），找到匹配的image仓库地址的凭证列表和是否找到匹配凭证（sandbox没有使用pod里定义pull secret）
// 目前内置云厂商（aws、gcp、azure）和dockerconfig provider
// 如果是dockerconfig provider
// 先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
// 没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
func (m *kubeGenericRuntimeManager) PullImage(image kubecontainer.ImageSpec, pullSecrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	img := image.Image
	// 从镜像地址解析出仓库地址、镜像tag、镜像digest
	repoToPull, _, _, err := parsers.ParseImageName(img)
	if err != nil {
		return "", err
	}

	// 尝试从pullSecrets里查找docker仓库凭证，如果secret里有，则返回联合secret里docker凭证与提供的m.keyring。否则返回提供的m.keyring
	// m.keyring是 返回注册且启用的dockerConfigProvider，credentialprovider.defaultDockerConfigProvider（遍历查找文件系统里docker凭证文件）一直启用
	keyring, err := credentialprovidersecrets.MakeDockerKeyring(pullSecrets, m.keyring)
	if err != nil {
		return "", err
	}

	imgSpec := &runtimeapi.ImageSpec{Image: img}
	// 先从pullSecrets里查找docker仓库凭证，有就返回
	// 否则没有找到，则从云厂商provider或文件系统里查找docker仓库凭证
	// 查找顺序:
	// 遍历所有注册并启用的provider提供的凭证（pod的pull secret没有使用），找到匹配的image仓库地址的凭证列表和是否找到匹配凭证（sandbox没有使用pod里定义pull secret）
	// 目前内置云厂商（aws、gcp、azure）和dockerconfig provider
	// 如果是dockerconfig provider
	// 先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
	// 没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
	creds, withCredentials := keyring.Lookup(repoToPull)
	if !withCredentials {
		klog.V(3).Infof("Pulling image %q without credentials", img)

		// 没有找到凭证，则尝试不使用凭证拉取镜像
		// 如果runtime是dockershim
		// 拉取镜像，并启动一个goroutine 每10s记录拉取状态（进度条）到日志。
		// 执行docker image inspect，并校验镜像地址与返回的结果是否一致，如果inspect结果中有RepoDigests，则返回digest，否则返回镜像id
		imageRef, err := m.imageService.PullImage(imgSpec, nil, podSandboxConfig)
		if err != nil {
			klog.Errorf("Pull image %q failed: %v", img, err)
			return "", err
		}

		return imageRef, nil
	}

	var pullErrs []error
	for _, currentCreds := range creds {
		// 从文件系统中获取docker凭证，只会有Username和Password
		auth := &runtimeapi.AuthConfig{
			Username:      currentCreds.Username,
			Password:      currentCreds.Password,
			Auth:          currentCreds.Auth,
			ServerAddress: currentCreds.ServerAddress,
			IdentityToken: currentCreds.IdentityToken,
			RegistryToken: currentCreds.RegistryToken,
		}

		// 找到凭证，使用凭证进行拉取
		// 如果runtime是dockershim
		// 拉取镜像，并启动一个goroutine 每10s记录拉取状态（进度条）到日志。
		// 执行docker image inspect，并校验镜像地址与返回的结果是否一致，如果inspect结果中有RepoDigests，则返回digest，否则返回镜像id
		imageRef, err := m.imageService.PullImage(imgSpec, auth, podSandboxConfig)
		// If there was no error, return success
		if err == nil {
			return imageRef, nil
		}

		pullErrs = append(pullErrs, err)
	}

	return "", utilerrors.NewAggregate(pullErrs)
}

// GetImageRef gets the ID of the image which has already been in
// the local storage. It returns ("", nil) if the image isn't in the local storage.
// 检查本地是否存在image镜像
func (m *kubeGenericRuntimeManager) GetImageRef(image kubecontainer.ImageSpec) (string, error) {
	// 获取镜像状态
	status, err := m.imageService.ImageStatus(&runtimeapi.ImageSpec{Image: image.Image})
	if err != nil {
		klog.Errorf("ImageStatus for image %q failed: %v", image, err)
		return "", err
	}
	if status == nil {
		return "", nil
	}
	return status.Id, nil
}

// ListImages gets all images currently on the machine.
func (m *kubeGenericRuntimeManager) ListImages() ([]kubecontainer.Image, error) {
	var images []kubecontainer.Image

	allImages, err := m.imageService.ListImages(nil)
	if err != nil {
		klog.Errorf("ListImages failed: %v", err)
		return nil, err
	}

	for _, img := range allImages {
		images = append(images, kubecontainer.Image{
			ID:          img.Id,
			Size:        int64(img.Size_),
			RepoTags:    img.RepoTags,
			RepoDigests: img.RepoDigests,
		})
	}

	return images, nil
}

// RemoveImage removes the specified image.
func (m *kubeGenericRuntimeManager) RemoveImage(image kubecontainer.ImageSpec) error {
	err := m.imageService.RemoveImage(&runtimeapi.ImageSpec{Image: image.Image})
	if err != nil {
		klog.Errorf("Remove image %q failed: %v", image.Image, err)
		return err
	}

	return nil
}

// ImageStats returns the statistics of the image.
// Notice that current logic doesn't really work for images which share layers (e.g. docker image),
// this is a known issue, and we'll address this by getting imagefs stats directly from CRI.
// TODO: Get imagefs stats directly from CRI.
// 获得node节点上所有镜像的总大小
func (m *kubeGenericRuntimeManager) ImageStats() (*kubecontainer.ImageStats, error) {
	// node节点上的所有镜像
	allImages, err := m.imageService.ListImages(nil)
	if err != nil {
		klog.Errorf("ListImages failed: %v", err)
		return nil, err
	}
	stats := &kubecontainer.ImageStats{}
	for _, img := range allImages {
		stats.TotalStorageBytes += img.Size_
	}
	return stats, nil
}
