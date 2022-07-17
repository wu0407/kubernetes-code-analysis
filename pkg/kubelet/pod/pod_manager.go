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

package pod

import (
	"sync"

	"k8s.io/klog"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/kubelet/checkpoint"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/configmap"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/secret"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// Manager stores and manages access to pods, maintaining the mappings
// between static pods and mirror pods.
//
// The kubelet discovers pod updates from 3 sources: file, http, and
// apiserver. Pods from non-apiserver sources are called static pods, and API
// server is not aware of the existence of static pods. In order to monitor
// the status of such pods, the kubelet creates a mirror pod for each static
// pod via the API server.
//
// A mirror pod has the same pod full name (name and namespace) as its static
// counterpart (albeit different metadata such as UID, etc). By leveraging the
// fact that the kubelet reports the pod status using the pod full name, the
// status of the mirror pod always reflects the actual status of the static
// pod. When a static pod gets deleted, the associated orphaned mirror pod
// will also be removed.
type Manager interface {
	// GetPods returns the regular pods bound to the kubelet and their spec.
	GetPods() []*v1.Pod
	// GetPodByFullName returns the (non-mirror) pod that matches full name, as well as
	// whether the pod was found.
	GetPodByFullName(podFullName string) (*v1.Pod, bool)
	// GetPodByName provides the (non-mirror) pod that matches namespace and
	// name, as well as whether the pod was found.
	GetPodByName(namespace, name string) (*v1.Pod, bool)
	// GetPodByUID provides the (non-mirror) pod that matches pod UID, as well as
	// whether the pod is found.
	GetPodByUID(types.UID) (*v1.Pod, bool)
	// GetPodByMirrorPod returns the static pod for the given mirror pod and
	// whether it was known to the pod manager.
	GetPodByMirrorPod(*v1.Pod) (*v1.Pod, bool)
	// GetMirrorPodByPod returns the mirror pod for the given static pod and
	// whether it was known to the pod manager.
	GetMirrorPodByPod(*v1.Pod) (*v1.Pod, bool)
	// GetPodsAndMirrorPods returns the both regular and mirror pods.
	GetPodsAndMirrorPods() ([]*v1.Pod, []*v1.Pod)
	// SetPods replaces the internal pods with the new pods.
	// It is currently only used for testing.
	SetPods(pods []*v1.Pod)
	// AddPod adds the given pod to the manager.
	AddPod(pod *v1.Pod)
	// UpdatePod updates the given pod in the manager.
	UpdatePod(pod *v1.Pod)
	// DeletePod deletes the given pod from the manager.  For mirror pods,
	// this means deleting the mappings related to mirror pods.  For non-
	// mirror pods, this means deleting from indexes for all non-mirror pods.
	DeletePod(pod *v1.Pod)
	// DeleteOrphanedMirrorPods deletes all mirror pods which do not have
	// associated static pods. This method sends deletion requests to the API
	// server, but does NOT modify the internal pod storage in basicManager.
	DeleteOrphanedMirrorPods()
	// TranslatePodUID returns the actual UID of a pod. If the UID belongs to
	// a mirror pod, returns the UID of its static pod. Otherwise, returns the
	// original UID.
	//
	// All public-facing functions should perform this translation for UIDs
	// because user may provide a mirror pod UID, which is not recognized by
	// internal Kubelet functions.
	TranslatePodUID(uid types.UID) kubetypes.ResolvedPodUID
	// GetUIDTranslations returns the mappings of static pod UIDs to mirror pod
	// UIDs and mirror pod UIDs to static pod UIDs.
	GetUIDTranslations() (podToMirror map[kubetypes.ResolvedPodUID]kubetypes.MirrorPodUID, mirrorToPod map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID)
	// IsMirrorPodOf returns true if mirrorPod is a correct representation of
	// pod; false otherwise.
	IsMirrorPodOf(mirrorPod, pod *v1.Pod) bool

	MirrorClient
}

// basicManager is a functional Manager.
//
// All fields in basicManager are read-only and are updated calling SetPods,
// AddPod, UpdatePod, or DeletePod.
type basicManager struct {
	// Protects all internal maps.
	lock sync.RWMutex

	// Regular pods indexed by UID.
	// Regular pods包括普通pod和static pod
	podByUID map[kubetypes.ResolvedPodUID]*v1.Pod
	// Mirror pods indexed by UID.
	mirrorPodByUID map[kubetypes.MirrorPodUID]*v1.Pod

	// Pods indexed by full name for easy access.
	// Regular pod
	podByFullName       map[string]*v1.Pod
	// Mirror pod
	mirrorPodByFullName map[string]*v1.Pod

	// Mirror pod UID to pod UID map.
	translationByUID map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID

	// basicManager is keeping secretManager and configMapManager up-to-date.
	secretManager     secret.Manager
	configMapManager  configmap.Manager
	checkpointManager checkpointmanager.CheckpointManager

	// A mirror pod client to create/delete mirror pods.
	MirrorClient
}

// NewBasicPodManager returns a functional Manager.
func NewBasicPodManager(client MirrorClient, secretManager secret.Manager, configMapManager configmap.Manager, cpm checkpointmanager.CheckpointManager) Manager {
	pm := &basicManager{}
	pm.secretManager = secretManager
	pm.configMapManager = configMapManager
	pm.checkpointManager = cpm
	pm.MirrorClient = client
	pm.SetPods(nil)
	return pm
}

// Set the internal pods based on the new pods.
func (pm *basicManager) SetPods(newPods []*v1.Pod) {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	pm.podByUID = make(map[kubetypes.ResolvedPodUID]*v1.Pod)
	pm.podByFullName = make(map[string]*v1.Pod)
	pm.mirrorPodByUID = make(map[kubetypes.MirrorPodUID]*v1.Pod)
	pm.mirrorPodByFullName = make(map[string]*v1.Pod)
	pm.translationByUID = make(map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID)

	pm.updatePodsInternal(newPods...)
}

// 让kubelet更新secret和configmap的manager里引用计数（决定启动reflector或停止reflector），将pod添加到pm对应的字段
// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
func (pm *basicManager) AddPod(pod *v1.Pod) {
	pm.UpdatePod(pod)
}

// 让kubelet更新secret和configmap的的manager里引用计数（决定启动reflector或停止reflector），将pod添加到pm中对应的字段
// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
// 如果设置了bootstrapCheckpointPath，则将pod信息保存到pod的checkpoint文件中
func (pm *basicManager) UpdatePod(pod *v1.Pod) {
	pm.lock.Lock()
	defer pm.lock.Unlock()
	pm.updatePodsInternal(pod)
	if pm.checkpointManager != nil {
		// 将pod信息写入到pod的checkpoint文件中
		if err := checkpoint.WritePod(pm.checkpointManager, pod); err != nil {
			klog.Errorf("Error writing checkpoint for pod: %v", pod.GetName())
		}
	}
}

// 判断pod是否在Terminated状态
func isPodInTerminatedState(pod *v1.Pod) bool {
	return pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded
}

// updatePodsInternal replaces the given pods in the current state of the
// manager, updating the various indices. The caller is assumed to hold the
// lock.
// 让kubelet更新secret和configmap的manager里引用计数（决定启动reflector或停止reflector），将添加pod到对应的字段
// pod是普通pod，则添加到podByUID、podByFullName、mirrorPodByFullName
// pod是mirror pod，则添加到mirrorPodByUID、mirrorPodByFullName、mirrorPodByFullName
func (pm *basicManager) updatePodsInternal(pods ...*v1.Pod) {
	for _, pod := range pods {
		if pm.secretManager != nil {
			// 判断pod是否在Terminated状态
			if isPodInTerminatedState(pod) {
				// Pods that are in terminated state and no longer running can be
				// ignored as they no longer require access to secrets.
				// It is especially important in watch-based manager, to avoid
				// unnecessary watches for terminated pods waiting for GC.
				// 根据objectKey（name和namespace构成）从registeredPods字段中找到这个pod
				// 如果pod不为空，则减少相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
				pm.secretManager.UnregisterPod(pod)
			} else {
				// TODO: Consider detecting only status update and in such case do
				// not register pod, as it doesn't really matter.
				// 将pod添加到registeredPods字段中，增加相关secret或configmap的引用计数（已经存在额pod不增加引用次数），如果这个对象不存在，则创建一个新的reflector（只watch和list这个资源）
				pm.secretManager.RegisterPod(pod)
			}
		}
		if pm.configMapManager != nil {
			if isPodInTerminatedState(pod) {
				// Pods that are in terminated state and no longer running can be
				// ignored as they no longer require access to configmaps.
				// It is especially important in watch-based manager, to avoid
				// unnecessary watches for terminated pods waiting for GC.
				// 跟secret一样
				pm.configMapManager.UnregisterPod(pod)
			} else {
				// TODO: Consider detecting only status update and in such case do
				// not register pod, as it doesn't really matter.
				// 跟secret一样
				pm.configMapManager.RegisterPod(pod)
			}
		}
		// {name}_{namespace}
		podFullName := kubecontainer.GetPodFullName(pod)
		// This logic relies on a static pod and its mirror to have the same name.
		// It is safe to type convert here due to the IsMirrorPod guard.
		if kubetypes.IsMirrorPod(pod) {
			mirrorPodUID := kubetypes.MirrorPodUID(pod.UID)
			pm.mirrorPodByUID[mirrorPodUID] = pod
			pm.mirrorPodByFullName[podFullName] = pod
			if p, ok := pm.podByFullName[podFullName]; ok {
				// mirror pod对应的普通的pod存在，translationByUID中添加mirrorPodUID到ResolvedPodUID（普通pod uid转换过）
				pm.translationByUID[mirrorPodUID] = kubetypes.ResolvedPodUID(p.UID)
			}
		} else {
			resolvedPodUID := kubetypes.ResolvedPodUID(pod.UID)
			pm.podByUID[resolvedPodUID] = pod
			pm.podByFullName[podFullName] = pod
			if mirror, ok := pm.mirrorPodByFullName[podFullName]; ok {
				// 普通的pod对应的mirror pod存在，translationByUID中添加mirrorPodUID到ResolvedPodUID（普通pod uid转换过）
				pm.translationByUID[kubetypes.MirrorPodUID(mirror.UID)] = resolvedPodUID
			}
		}
	}
}

// 在pm.secretManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
// 在pm.configMapManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
// 如果是mirror pod，则从pm.mirrorPodByUID、pm.mirrorPodByFullName、pm.translationByUID删除对应的pod
// 不是mirror pod，则从pm.podByUID、pm.podByFullName中删除这个pod
// 如果启用pod checkpoint manager，则删除pod的checkpoint文件
func (pm *basicManager) DeletePod(pod *v1.Pod) {
	pm.lock.Lock()
	defer pm.lock.Unlock()
	if pm.secretManager != nil {
		// 在pm.secretManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
		pm.secretManager.UnregisterPod(pod)
	}
	if pm.configMapManager != nil {
		// 在pm.configMapManager.manager.objectCache减少pod相关secret或configmap的引用计数，相关的secret或configmap次数为0，则停止这secret或configmap的reflector
		pm.configMapManager.UnregisterPod(pod)
	}
	// "{pod name}_{pod namespace}"
	podFullName := kubecontainer.GetPodFullName(pod)
	// It is safe to type convert here due to the IsMirrorPod guard.
	// 如果是mirror pod，则pm.mirrorPodByUID、pm.mirrorPodByFullName、pm.translationByUID删除对应的pod
	if kubetypes.IsMirrorPod(pod) {
		mirrorPodUID := kubetypes.MirrorPodUID(pod.UID)
		delete(pm.mirrorPodByUID, mirrorPodUID)
		delete(pm.mirrorPodByFullName, podFullName)
		delete(pm.translationByUID, mirrorPodUID)
	} else {
		// 不是mirror pod，则从pm.podByUID、pm.podByFullName中删除这个pod
		delete(pm.podByUID, kubetypes.ResolvedPodUID(pod.UID))
		delete(pm.podByFullName, podFullName)
	}
	// 如果启用pod checkpoint manager，则删除pod的checkpoint文件
	if pm.checkpointManager != nil {
		if err := checkpoint.DeletePod(pm.checkpointManager, pod); err != nil {
			klog.Errorf("Error deleting checkpoint for pod: %v", pod.GetName())
		}
	}
}

// 返回所有普通pod和static pod
func (pm *basicManager) GetPods() []*v1.Pod {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	return podsMapToPods(pm.podByUID)
}

// 返回普通pod和static pod、mirror pod
func (pm *basicManager) GetPodsAndMirrorPods() ([]*v1.Pod, []*v1.Pod) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pods := podsMapToPods(pm.podByUID)
	mirrorPods := mirrorPodsMapToMirrorPods(pm.mirrorPodByUID)
	return pods, mirrorPods
}

// 根据uid返回非mirror pod（普通pod和static pod）
func (pm *basicManager) GetPodByUID(uid types.UID) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pod, ok := pm.podByUID[kubetypes.ResolvedPodUID(uid)] // Safe conversion, map only holds non-mirrors.
	return pod, ok
}

func (pm *basicManager) GetPodByName(namespace, name string) (*v1.Pod, bool) {
	podFullName := kubecontainer.BuildPodFullName(name, namespace)
	return pm.GetPodByFullName(podFullName)
}

func (pm *basicManager) GetPodByFullName(podFullName string) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pod, ok := pm.podByFullName[podFullName]
	return pod, ok
}

// 首先尝试将uid转成MirrorPodUID在translationByUID中查找static uid
// 如果未找到，则返回原始的uid
func (pm *basicManager) TranslatePodUID(uid types.UID) kubetypes.ResolvedPodUID {
	// It is safe to type convert to a resolved UID because type conversion is idempotent.
	if uid == "" {
		return kubetypes.ResolvedPodUID(uid)
	}

	pm.lock.RLock()
	defer pm.lock.RUnlock()
	if translated, ok := pm.translationByUID[kubetypes.MirrorPodUID(uid)]; ok {
		return translated
	}
	return kubetypes.ResolvedPodUID(uid)
}

// 返回所有普通pod uid对应mirror pod uid的podToMirror（static pod没有对应的mirror pod则对应的mirror pod uid值为""）和
// mirror pod uid对应static pod uid的mirrorToPod
func (pm *basicManager) GetUIDTranslations() (podToMirror map[kubetypes.ResolvedPodUID]kubetypes.MirrorPodUID,
	mirrorToPod map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()

	podToMirror = make(map[kubetypes.ResolvedPodUID]kubetypes.MirrorPodUID, len(pm.translationByUID))
	mirrorToPod = make(map[kubetypes.MirrorPodUID]kubetypes.ResolvedPodUID, len(pm.translationByUID))
	// Insert empty translation mapping for all static pods.
	for uid, pod := range pm.podByUID {
		if !kubetypes.IsStaticPod(pod) {
			continue
		}
		podToMirror[uid] = ""
	}
	// Fill in translations. Notice that if there is no mirror pod for a
	// static pod, its uid will be translated into empty string "". This
	// is WAI, from the caller side we can know that the static pod doesn't
	// have a corresponding mirror pod instead of using static pod uid directly.
	for k, v := range pm.translationByUID {
		mirrorToPod[k] = v
		// mirror pod里找到static pod，就会覆盖上面的podToMirror[uid] = ""
		// 即static pod没有mirror pod，则podToMirror[uid] = ""
		podToMirror[v] = k
	}
	return podToMirror, mirrorToPod
}

// 返回所有没有对应static pod的mirror pod
func (pm *basicManager) getOrphanedMirrorPodNames() []string {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	var podFullNames []string
	for podFullName := range pm.mirrorPodByFullName {
		if _, ok := pm.podByFullName[podFullName]; !ok {
			podFullNames = append(podFullNames, podFullName)
		}
	}
	return podFullNames
}

// 从apiserver中删除所有没有对应static pod的mirror pod
func (pm *basicManager) DeleteOrphanedMirrorPods() {
	// 返回所有没有对应static pod的mirror pod
	podFullNames := pm.getOrphanedMirrorPodNames()
	for _, podFullName := range podFullNames {
		// 调用api删除mirror pod（从podFullName解析出name和namespace）
		pm.MirrorClient.DeleteMirrorPod(podFullName, nil)
	}
}

// mirrorPod是否是pod的mirror pod
func (pm *basicManager) IsMirrorPodOf(mirrorPod, pod *v1.Pod) bool {
	// Check name and namespace first.
	if pod.Name != mirrorPod.Name || pod.Namespace != mirrorPod.Namespace {
		return false
	}
	// 返回mirrorPod的annotations["kubernetes.io/config.mirror"]值
	hash, ok := getHashFromMirrorPod(mirrorPod)
	if !ok {
		return false
	}
	// mirrorPod的annotations["kubernetes.io/config.mirror"]值是否与pod的annotations["kubernetes.io/config.hash"]值一样
	return hash == getPodHash(pod)
}

func podsMapToPods(UIDMap map[kubetypes.ResolvedPodUID]*v1.Pod) []*v1.Pod {
	pods := make([]*v1.Pod, 0, len(UIDMap))
	for _, pod := range UIDMap {
		pods = append(pods, pod)
	}
	return pods
}

func mirrorPodsMapToMirrorPods(UIDMap map[kubetypes.MirrorPodUID]*v1.Pod) []*v1.Pod {
	pods := make([]*v1.Pod, 0, len(UIDMap))
	for _, pod := range UIDMap {
		pods = append(pods, pod)
	}
	return pods
}

// 根据static pod获取mirror pod
func (pm *basicManager) GetMirrorPodByPod(pod *v1.Pod) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	mirrorPod, ok := pm.mirrorPodByFullName[kubecontainer.GetPodFullName(pod)]
	return mirrorPod, ok
}

// 根据mirror pod获取static pod
func (pm *basicManager) GetPodByMirrorPod(mirrorPod *v1.Pod) (*v1.Pod, bool) {
	pm.lock.RLock()
	defer pm.lock.RUnlock()
	pod, ok := pm.podByFullName[kubecontainer.GetPodFullName(mirrorPod)]
	return pod, ok
}
