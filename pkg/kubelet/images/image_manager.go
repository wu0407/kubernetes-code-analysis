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

	dockerref "github.com/docker/distribution/reference"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/klog"

	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/util/parsers"
)

// imageManager provides the functionalities for image pulling.
type imageManager struct {
	recorder     record.EventRecorder
	imageService kubecontainer.ImageService
	backOff      *flowcontrol.Backoff
	// It will check the presence of the image, and report the 'image pulling', image pulled' events correspondingly.
	puller imagePuller
}

var _ ImageManager = &imageManager{}

// NewImageManager instantiates a new ImageManager object.
func NewImageManager(recorder record.EventRecorder, imageService kubecontainer.ImageService, imageBackOff *flowcontrol.Backoff, serialized bool, qps float32, burst int) ImageManager {
	// 判断是否达到了限速请求，是否进行拉取镜像
	imageService = throttleImagePulling(imageService, qps, burst)

	var puller imagePuller
	if serialized {
		// 使用通道进行串行拉取镜像
		puller = newSerialImagePuller(imageService)
	} else {
		// 每个拉取请求启动一个goroutine进行拉取
		puller = newParallelImagePuller(imageService)
	}
	return &imageManager{
		recorder:     recorder,
		imageService: imageService,
		backOff:      imageBackOff,
		puller:       puller,
	}
}

// shouldPullImage returns whether we should pull an image according to
// the presence and pull policy of the image.
// 根据container里的ImagePullPolicy和本地是否有image，来判断是否要拉取镜像
func shouldPullImage(container *v1.Container, imagePresent bool) bool {
	if container.ImagePullPolicy == v1.PullNever {
		return false
	}

	if container.ImagePullPolicy == v1.PullAlways ||
		(container.ImagePullPolicy == v1.PullIfNotPresent && (!imagePresent)) {
		return true
	}

	return false
}

// records an event using ref, event msg.  log to glog using prefix, msg, logFn
func (m *imageManager) logIt(ref *v1.ObjectReference, eventtype, event, prefix, msg string, logFn func(args ...interface{})) {
	if ref != nil {
		m.recorder.Event(ref, eventtype, event, msg)
	} else {
		logFn(fmt.Sprint(prefix, " ", msg))
	}
}

// EnsureImageExists pulls the image for the specified pod and container, and returns
// (imageRef, error message, error).
// 拉取镜像前会检查所有条件（且关系）
// 1. 本地是否已经有镜像
// 2. 根据pull policy是否要进行拉取
// 3. 是否达到（所有镜像拉取请求）拉取频率限制
// 4. 是否在回退周期内，不在会退周期内会进行拉取
// 拉取失败会执行指数回退策略
func (m *imageManager) EnsureImageExists(pod *v1.Pod, container *v1.Container, pullSecrets []v1.Secret, podSandboxConfig *runtimeapi.PodSandboxConfig) (string, string, error) {
	logPrefix := fmt.Sprintf("%s/%s", pod.Name, container.Image)
	// 生成container属于pod的ObjectReference
	ref, err := kubecontainer.GenerateContainerRef(pod, container)
	if err != nil {
		klog.Errorf("Couldn't make a ref to pod %v, container %v: '%v'", pod.Name, container.Name, err)
	}

	// If the image contains no tag or digest, a default tag should be applied.
	// 解析镜像地址，如果没有tag，则添加"latest" tag
	image, err := applyDefaultImageTag(container.Image)
	if err != nil {
		msg := fmt.Sprintf("Failed to apply default image tag %q: %v", container.Image, err)
		m.logIt(ref, v1.EventTypeWarning, events.FailedToInspectImage, logPrefix, msg, klog.Warning)
		return "", msg, ErrInvalidImageName
	}

	spec := kubecontainer.ImageSpec{Image: image}
	// 检查本地是否存在image镜像
	imageRef, err := m.imageService.GetImageRef(spec)
	// 发生错误，代表执行inspect image发生错误
	if err != nil {
		msg := fmt.Sprintf("Failed to inspect image %q: %v", container.Image, err)
		m.logIt(ref, v1.EventTypeWarning, events.FailedToInspectImage, logPrefix, msg, klog.Warning)
		return "", msg, ErrImageInspect
	}

	// 镜像是否存在（imageRef为空代表不存在镜像）
	present := imageRef != ""
	// 根据container里的ImagePullPolicy和本地是否有image，来判断是否要拉取镜像
	// 不需要拉取镜像情况，直接返回
	if !shouldPullImage(container, present) {
		if present {
			msg := fmt.Sprintf("Container image %q already present on machine", container.Image)
			m.logIt(ref, v1.EventTypeNormal, events.PulledImage, logPrefix, msg, klog.Info)
			return imageRef, "", nil
		}
		msg := fmt.Sprintf("Container image %q is not present with pull policy of Never", container.Image)
		m.logIt(ref, v1.EventTypeWarning, events.ErrImageNeverPullPolicy, logPrefix, msg, klog.Warning)
		return "", msg, ErrImageNeverPull
	}

	backOffKey := fmt.Sprintf("%s_%s", pod.UID, container.Image)
	// m.backOff.Clock.Now()在回退周期内，直接返回"ImagePullBackOff"错误
	if m.backOff.IsInBackOffSinceUpdate(backOffKey, m.backOff.Clock.Now()) {
		msg := fmt.Sprintf("Back-off pulling image %q", container.Image)
		m.logIt(ref, v1.EventTypeNormal, events.BackOffPullImage, logPrefix, msg, klog.Info)
		return "", msg, ErrImagePullBackOff
	}
	m.logIt(ref, v1.EventTypeNormal, events.PullingImage, logPrefix, fmt.Sprintf("Pulling image %q", container.Image), klog.Info)
	pullChan := make(chan pullResult)
	// 根据kubelet镜像拉取配置，判断是否在达到限速限制，执行拉取（并行拉取或串行拉取）或不拉取，拉取结果会发送到pullChan
	m.puller.pullImage(spec, pullSecrets, pullChan, podSandboxConfig)
	imagePullResult := <-pullChan
	// 如果发生限速或拉取错误，重新计算回退周期，然后返回错误
	if imagePullResult.err != nil {
		m.logIt(ref, v1.EventTypeWarning, events.FailedToPullImage, logPrefix, fmt.Sprintf("Failed to pull image %q: %v", container.Image, imagePullResult.err), klog.Warning)
		// 重新计算回退周期 
		m.backOff.Next(backOffKey, m.backOff.Clock.Now())
		// 返回"RegistryUnavailable"错误
		if imagePullResult.err == ErrRegistryUnavailable {
			msg := fmt.Sprintf("image pull failed for %s because the registry is unavailable.", container.Image)
			return "", msg, imagePullResult.err
		}

		// 返回"ErrImagePull"错误
		return "", imagePullResult.err.Error(), ErrImagePull
	}
	m.logIt(ref, v1.EventTypeNormal, events.PulledImage, logPrefix, fmt.Sprintf("Successfully pulled image %q", container.Image), klog.Info)
	// 清理开始回退时间离现在已经超过2倍最大回收周期的item
	m.backOff.GC()
	return imagePullResult.imageRef, "", nil
}

// applyDefaultImageTag parses a docker image string, if it doesn't contain any tag or digest,
// a default tag will be applied.
// 解析镜像地址，如果没有tag，则添加"latest" tag
func applyDefaultImageTag(image string) (string, error) {
	// 验证并解析镜像地址为Named对象
	named, err := dockerref.ParseNormalizedNamed(image)
	if err != nil {
		return "", fmt.Errorf("couldn't parse image reference %q: %v", image, err)
	}
	// 是否包含tag
	_, isTagged := named.(dockerref.Tagged)
	// 是否包含digest
	_, isDigested := named.(dockerref.Digested)
	// 即没有tag也没有digest，那么默认为"latest" tag
	if !isTagged && !isDigested {
		// we just concatenate the image name with the default tag here instead
		// of using dockerref.WithTag(named, ...) because that would cause the
		// image to be fully qualified as docker.io/$name if it's a short name
		// (e.g. just busybox). We don't want that to happen to keep the CRI
		// agnostic wrt image names and default hostnames.
		image = image + ":" + parsers.DefaultImageTag
	}
	return image, nil
}
