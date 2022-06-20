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

package images

import (
	"fmt"

	"k8s.io/api/core/v1"
	"k8s.io/client-go/util/flowcontrol"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// throttleImagePulling wraps kubecontainer.ImageService to throttle image
// pulling based on the given QPS and burst limits. If QPS is zero, defaults
// to no throttling.
// 这里imageService 一般是*kubeGenericRuntimeManager（在pkg\kubelet\kuberuntime\kuberuntime_manager.go）
func throttleImagePulling(imageService kubecontainer.ImageService, qps float32, burst int) kubecontainer.ImageService {
	if qps == 0.0 {
		return imageService
	}
	return &throttledImageService{
		ImageService: imageService,
		limiter:      flowcontrol.NewTokenBucketRateLimiter(qps, burst),
	}
}

type throttledImageService struct {
	kubecontainer.ImageService
	limiter flowcontrol.RateLimiter
}

// 没有达到限速限制，则进行拉取镜像，返回镜像id或digest。否则返回空和"pull QPS exceeded"错误
func (ts throttledImageService) PullImage(image kubecontainer.ImageSpec, secrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, error) {
	if ts.limiter.TryAccept() {
		// 尝试先从pullSecrets里查找docker仓库凭证，没有找到。则从云厂商provider或文件系统里查找docker仓库凭证。
		// 找到凭证则使用凭证进行拉取镜像。上面方式中都没有找到凭证，则不使用凭证进行拉取镜像
		// 从云厂商provider或文件系统查找顺序：
		// 遍历所有注册并启用的provider提供的凭证（pod的pull secret没有使用），找到匹配的image仓库地址的凭证列表和是否找到匹配凭证（sandbox没有使用pod里定义pull secret）
		// 目前内置云厂商（aws、gcp、azure）和dockerconfig provider
		// 如果是dockerconfig provider
		// 先从["/var/lib/kubelet/config.json", "当前目录下的config.json", "~/.docker/config.json", "/.docker/config.json"]返回第一个存在且正确配置文件里的内容
		// 没有读取到，则从["/var/lib/kubelet/.dockercfg", "当前目录/.dockercfg", "~/.dockercfg", "/.dockercfg"]，返回第一个存在且正确配置文件里的内容
		return ts.ImageService.PullImage(image, secrets, podSandboxConfig)
	}
	return "", fmt.Errorf("pull QPS exceeded")
}
