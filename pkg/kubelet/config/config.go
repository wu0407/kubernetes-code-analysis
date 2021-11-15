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

package config

import (
	"fmt"
	"reflect"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/kubelet/checkpoint"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/util/config"
)

// PodConfigNotificationMode describes how changes are sent to the update channel.
type PodConfigNotificationMode int

const (
	// PodConfigNotificationUnknown is the default value for
	// PodConfigNotificationMode when uninitialized.
	PodConfigNotificationUnknown = iota
	// PodConfigNotificationSnapshot delivers the full configuration as a SET whenever
	// any change occurs.
	PodConfigNotificationSnapshot
	// PodConfigNotificationSnapshotAndUpdates delivers an UPDATE and DELETE message whenever pods are
	// changed, and a SET message if there are any additions or removals.
	PodConfigNotificationSnapshotAndUpdates
	// PodConfigNotificationIncremental delivers ADD, UPDATE, DELETE, REMOVE, RECONCILE to the update channel.
	PodConfigNotificationIncremental
)

// PodConfig is a configuration mux that merges many sources of pod configuration into a single
// consistent structure, and then delivers incremental change notifications to listeners
// in order.
type PodConfig struct {
	pods *podStorage
	// 聚合器
	mux  *config.Mux

	// the channel of denormalized changes passed to listeners
	updates chan kubetypes.PodUpdate

	// contains the list of all configured sources
	sourcesLock       sync.Mutex
	// kubelet配置文件或命令行的podconfig源集合，调用Channel就会添加一个source到这里
	sources           sets.String
	checkpointManager checkpointmanager.CheckpointManager
}

// NewPodConfig creates an object that can merge many configuration sources into a stream
// of normalized updates to a pod configuration.
func NewPodConfig(mode PodConfigNotificationMode, recorder record.EventRecorder) *PodConfig {
	updates := make(chan kubetypes.PodUpdate, 50)
	storage := newPodStorage(updates, mode, recorder)
	podConfig := &PodConfig{
		pods:    storage,
		mux:     config.NewMux(storage),
		updates: updates,
		sources: sets.String{},
	}
	return podConfig
}

// Channel creates or returns a config source channel.  The channel
// only accepts PodUpdates
// 
// sources里添加source
// 创建或返回一个chan接收PodUpdates消息，这个chan保存在mux里sources
// 启动一个goroutine消费这个chan里的PodUpdates消息--进行加工分类，然后发送给updates通道
func (c *PodConfig) Channel(source string) chan<- interface{} {
	c.sourcesLock.Lock()
	defer c.sourcesLock.Unlock()
	c.sources.Insert(source)
	return c.mux.Channel(source)
}

// SeenAllSources returns true if seenSources contains all sources in the
// config, and also this config has received a SET message from each source.
// seenSources源已经注册且都提供了至少一个pod
func (c *PodConfig) SeenAllSources(seenSources sets.String) bool {
	if c.pods == nil {
		return false
	}
	klog.V(5).Infof("Looking for %v, have seen %v", c.sources.List(), seenSources)
	// 提供的source集合（seenSources）与kubelet配置的pod获取源（c.sources）一致
	// 且c.pods（保存各个源的pod集合）（每个源都发送至少一个SET消息）包含了kubelet配置的pod获取源
	return seenSources.HasAll(c.sources.List()...) && c.pods.seenSources(c.sources.List()...)
}

// Updates returns a channel of updates to the configuration, properly denormalized.
func (c *PodConfig) Updates() <-chan kubetypes.PodUpdate {
	return c.updates
}

// Sync requests the full configuration be delivered to the update channel.
func (c *PodConfig) Sync() {
	c.pods.Sync()
}

// Restore restores pods from the checkpoint path, *once*
func (c *PodConfig) Restore(path string, updates chan<- interface{}) error {
	if c.checkpointManager != nil {
		return nil
	}
	var err error
	c.checkpointManager, err = checkpointmanager.NewCheckpointManager(path)
	if err != nil {
		return err
	}
	pods, err := checkpoint.LoadPods(c.checkpointManager)
	if err != nil {
		return err
	}
	updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.RESTORE, Source: kubetypes.ApiserverSource}
	return nil
}

// podStorage manages the current pod state at any point in time and ensures updates
// to the channel are delivered in order.  Note that this object is an in-memory source of
// "truth" and on creation contains zero entries.  Once all previously read sources are
// available, then this object should be considered authoritative.
type podStorage struct {
	podLock sync.RWMutex
	// map of source name to pod uid to pod reference
	pods map[string]map[types.UID]*v1.Pod
	mode PodConfigNotificationMode

	// ensures that updates are delivered in strict order
	// on the updates channel
	updateLock sync.Mutex
	updates    chan<- kubetypes.PodUpdate

	// contains the set of all sources that have sent at least one SET
	sourcesSeenLock sync.RWMutex
	sourcesSeen     sets.String

	// the EventRecorder to use
	recorder record.EventRecorder
}

// TODO: PodConfigNotificationMode could be handled by a listener to the updates channel
// in the future, especially with multiple listeners.
// TODO: allow initialization of the current state of the store with snapshotted version.
func newPodStorage(updates chan<- kubetypes.PodUpdate, mode PodConfigNotificationMode, recorder record.EventRecorder) *podStorage {
	return &podStorage{
		pods:        make(map[string]map[types.UID]*v1.Pod),
		mode:        mode,
		updates:     updates,
		sourcesSeen: sets.String{},
		recorder:    recorder,
	}
}

// Merge normalizes a set of incoming changes from different sources into a map of all Pods
// and ensures that redundant changes are filtered out, and then pushes zero or more minimal
// updates onto the update channel.  Ensures that updates are delivered in order.
// 
// 将输入source的PodUpdate的pod列表去重
// 将PodUpdate与s.pods[source]的pod进行比对，加工出各个OP类型PodUpdate，发送到s.updates通道中
// 输入PodUpdate的OP可能与输出PodUpdate的OP不一样
func (s *podStorage) Merge(source string, change interface{}) error {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()

	seenBefore := s.sourcesSeen.Has(source)
	adds, updates, deletes, removes, reconciles, restores := s.merge(source, change)
	// 当PodUpdate的op为SET时候，firstSet为true
	firstSet := !seenBefore && s.sourcesSeen.Has(source)

	// deliver update notifications
	switch s.mode {
	case PodConfigNotificationIncremental:
		if len(removes.Pods) > 0 {
			s.updates <- *removes
		}
		if len(adds.Pods) > 0 {
			s.updates <- *adds
		}
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}
		if len(deletes.Pods) > 0 {
			s.updates <- *deletes
		}
		if len(restores.Pods) > 0 {
			s.updates <- *restores
		}
		if firstSet && len(adds.Pods) == 0 && len(updates.Pods) == 0 && len(deletes.Pods) == 0 {
			// Send an empty update when first seeing the source and there are
			// no ADD or UPDATE or DELETE pods from the source. This signals kubelet that
			// the source is ready.
			s.updates <- *adds
		}
		// Only add reconcile support here, because kubelet doesn't support Snapshot update now.
		if len(reconciles.Pods) > 0 {
			s.updates <- *reconciles
		}

	case PodConfigNotificationSnapshotAndUpdates:
		if len(removes.Pods) > 0 || len(adds.Pods) > 0 || firstSet {
			s.updates <- kubetypes.PodUpdate{Pods: s.MergedState().([]*v1.Pod), Op: kubetypes.SET, Source: source}
		}
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}
		if len(deletes.Pods) > 0 {
			s.updates <- *deletes
		}

	case PodConfigNotificationSnapshot:
		if len(updates.Pods) > 0 || len(deletes.Pods) > 0 || len(adds.Pods) > 0 || len(removes.Pods) > 0 || firstSet {
			s.updates <- kubetypes.PodUpdate{Pods: s.MergedState().([]*v1.Pod), Op: kubetypes.SET, Source: source}
		}

	case PodConfigNotificationUnknown:
		fallthrough
	default:
		panic(fmt.Sprintf("unsupported PodConfigNotificationMode: %#v", s.mode))
	}

	return nil
}

// 将传进来的change--PodUpdate里的pod，与记录中s.pods[source]的pod进行比对，分类出adds, updates, deletes, removes, reconciles, restores
// 返回各类PodUpdate--包含最终pod信息（有修改过或未修改--对于s.pods[source]的pod来说）
func (s *podStorage) merge(source string, change interface{}) (adds, updates, deletes, removes, reconciles, restores *kubetypes.PodUpdate) {
	s.podLock.Lock()
	defer s.podLock.Unlock()

	addPods := []*v1.Pod{}
	updatePods := []*v1.Pod{}
	deletePods := []*v1.Pod{}
	removePods := []*v1.Pod{}
	reconcilePods := []*v1.Pod{}
	restorePods := []*v1.Pod{}

	pods := s.pods[source]
	if pods == nil {
		pods = make(map[types.UID]*v1.Pod)
	}

	// updatePodFunc is the local function which updates the pod cache *oldPods* with new pods *newPods*.
	// After updated, new pod will be stored in the pod cache *pods*.
	// Notice that *pods* and *oldPods* could be the same cache.
	updatePodsFunc := func(newPods []*v1.Pod, oldPods, pods map[types.UID]*v1.Pod) {
		//过滤重复的pod，返回没有重复的pod
		filtered := filterInvalidPods(newPods, source, s.recorder)
		for _, ref := range filtered {
			// Annotate the pod with the source before any comparison.
			if ref.Annotations == nil {
				ref.Annotations = make(map[string]string)
			}
			// 添加kubernetes.io/config.source的annotation
			ref.Annotations[kubetypes.ConfigSourceAnnotationKey] = source
			if existing, found := oldPods[ref.UID]; found {
				pods[ref.UID] = existing
				//  如果status以外属性相同且status也相同，则不需要任何操作，返回都是false。status以外属性相同，但是status不同则更新existing.status为ref.status、返回needReconcile为true
				//  如果属性不同，则更新existing属性=ref属性，如果ref.DeletionTimestamp存在，则返回needGracefulDelete为true。否则needUpdate为true
				needUpdate, needReconcile, needGracefulDelete := checkAndUpdatePod(existing, ref)
				if needUpdate {
					updatePods = append(updatePods, existing)
				} else if needReconcile {
					reconcilePods = append(reconcilePods, existing)
				} else if needGracefulDelete {
					deletePods = append(deletePods, existing)
				}
				continue
			}
			// 添加kubernetes.io/config.seen的annotation
			recordFirstSeenTime(ref)
			pods[ref.UID] = ref
			addPods = append(addPods, ref)
		}
	}

	update := change.(kubetypes.PodUpdate)
	switch update.Op {
	case kubetypes.ADD, kubetypes.UPDATE, kubetypes.DELETE:
		if update.Op == kubetypes.ADD {
			klog.V(4).Infof("Adding new pods from source %s : %v", source, update.Pods)
		} else if update.Op == kubetypes.DELETE {
			klog.V(4).Infof("Graceful deleting pods from source %s : %v", source, update.Pods)
		} else {
			klog.V(4).Infof("Updating pods from source %s : %v", source, update.Pods)
		}
		// 根据update.Pods里的pod列表，更新pods里的pod信息--添加或修改
		updatePodsFunc(update.Pods, pods, pods)

	case kubetypes.REMOVE:
		klog.V(4).Infof("Removing pods from source %s : %v", source, update.Pods)
		for _, value := range update.Pods {
			// 被remove的pod，在update.Pods里且也在s.pods[source]--pods里
			if existing, found := pods[value.UID]; found {
				// this is a delete
				delete(pods, value.UID)
				removePods = append(removePods, existing)
				continue
			}
			// this is a no-op
		}

	case kubetypes.SET:
		klog.V(4).Infof("Setting pods for source %s", source)
		//添加source到已经发现的sourceSet
		s.markSourceSet(source)
		// Clear the old map entries by just creating a new map
		oldPods := pods
		pods = make(map[types.UID]*v1.Pod)
		// 根据update.Pods, oldPods将更新过的（以update.Pods里的pod同样在oldpods中或者不在oldpods）pod列表保存到最新的pods中
		updatePodsFunc(update.Pods, oldPods, pods)
		// 被remove的pod--在oldPods--s.pods[source]里但是不在update.Pods里的pod
		for uid, existing := range oldPods {
			if _, found := pods[uid]; !found {
				// this is a delete
				removePods = append(removePods, existing)
			}
		}
	case kubetypes.RESTORE:
		klog.V(4).Infof("Restoring pods for source %s", source)
		restorePods = append(restorePods, update.Pods...)

	default:
		klog.Warningf("Received invalid update type: %v", update)

	}

	s.pods[source] = pods

	adds = &kubetypes.PodUpdate{Op: kubetypes.ADD, Pods: copyPods(addPods), Source: source}
	updates = &kubetypes.PodUpdate{Op: kubetypes.UPDATE, Pods: copyPods(updatePods), Source: source}
	// op为ADD、UPDATE、DELETE、SET，pod有DeletionTimestamp
	deletes = &kubetypes.PodUpdate{Op: kubetypes.DELETE, Pods: copyPods(deletePods), Source: source}
	// op为SET，在s.pods[source]--oldPods里但是不在update.Pods里的pod
	// op为REMOVE，在update.Pods里且也在s.pods[source]--pods里
	removes = &kubetypes.PodUpdate{Op: kubetypes.REMOVE, Pods: copyPods(removePods), Source: source}
	reconciles = &kubetypes.PodUpdate{Op: kubetypes.RECONCILE, Pods: copyPods(reconcilePods), Source: source}
	restores = &kubetypes.PodUpdate{Op: kubetypes.RESTORE, Pods: copyPods(restorePods), Source: source}

	return adds, updates, deletes, removes, reconciles, restores
}

func (s *podStorage) markSourceSet(source string) {
	s.sourcesSeenLock.Lock()
	defer s.sourcesSeenLock.Unlock()
	s.sourcesSeen.Insert(source)
}

func (s *podStorage) seenSources(sources ...string) bool {
	s.sourcesSeenLock.RLock()
	defer s.sourcesSeenLock.RUnlock()
	return s.sourcesSeen.HasAll(sources...)
}

// 去掉重复的pod
func filterInvalidPods(pods []*v1.Pod, source string, recorder record.EventRecorder) (filtered []*v1.Pod) {
	names := sets.String{}
	for i, pod := range pods {
		// Pods from each source are assumed to have passed validation individually.
		// This function only checks if there is any naming conflict.
		name := kubecontainer.GetPodFullName(pod)
		if names.Has(name) {
			klog.Warningf("Pod[%d] (%s) from %s failed validation due to duplicate pod name %q, ignoring", i+1, format.Pod(pod), source, pod.Name)
			recorder.Eventf(pod, v1.EventTypeWarning, events.FailedValidation, "Error validating pod %s from %s due to duplicate pod name %q, ignoring", format.Pod(pod), source, pod.Name)
			continue
		} else {
			names.Insert(name)
		}

		filtered = append(filtered, pod)
	}
	return
}

// Annotations that the kubelet adds to the pod.
var localAnnotations = []string{
	kubetypes.ConfigSourceAnnotationKey,
	kubetypes.ConfigMirrorAnnotationKey,
	kubetypes.ConfigFirstSeenAnnotationKey,
}

func isLocalAnnotationKey(key string) bool {
	for _, localKey := range localAnnotations {
		if key == localKey {
			return true
		}
	}
	return false
}

// isAnnotationMapEqual returns true if the existing annotation Map is equal to candidate except
// for local annotations.
func isAnnotationMapEqual(existingMap, candidateMap map[string]string) bool {
	if candidateMap == nil {
		candidateMap = make(map[string]string)
	}
	for k, v := range candidateMap {
		if isLocalAnnotationKey(k) {
			continue
		}
		if existingValue, ok := existingMap[k]; ok && existingValue == v {
			continue
		}
		return false
	}
	for k := range existingMap {
		if isLocalAnnotationKey(k) {
			continue
		}
		// stale entry in existing map.
		if _, exists := candidateMap[k]; !exists {
			return false
		}
	}
	return true
}

// recordFirstSeenTime records the first seen time of this pod.
func recordFirstSeenTime(pod *v1.Pod) {
	klog.V(4).Infof("Receiving a new pod %q", format.Pod(pod))
	pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey] = kubetypes.NewTimestamp().GetString()
}

// updateAnnotations returns an Annotation map containing the api annotation map plus
// locally managed annotations
// 更新existing的annotation为ref的annotation加上原有的LocalAnnotation
func updateAnnotations(existing, ref *v1.Pod) {
	annotations := make(map[string]string, len(ref.Annotations)+len(localAnnotations))
	for k, v := range ref.Annotations {
		annotations[k] = v
	}
	for _, k := range localAnnotations {
		if v, ok := existing.Annotations[k]; ok {
			annotations[k] = v
		}
	}
	existing.Annotations = annotations
}

func podsDifferSemantically(existing, ref *v1.Pod) bool {
	if reflect.DeepEqual(existing.Spec, ref.Spec) &&
		reflect.DeepEqual(existing.Labels, ref.Labels) &&
		reflect.DeepEqual(existing.DeletionTimestamp, ref.DeletionTimestamp) &&
		reflect.DeepEqual(existing.DeletionGracePeriodSeconds, ref.DeletionGracePeriodSeconds) &&
		isAnnotationMapEqual(existing.Annotations, ref.Annotations) {
		return false
	}
	return true
}

// checkAndUpdatePod updates existing, and:
//   * if ref makes a meaningful change, returns needUpdate=true
//   * if ref makes a meaningful change, and this change is graceful deletion, returns needGracefulDelete=true
//   * if ref makes no meaningful change, but changes the pod status, returns needReconcile=true
//   * else return all false
//   Now, needUpdate, needGracefulDelete and needReconcile should never be both true
//  如果status以外属性相同且status也相同，则不需要任何操作，返回都是false。status以外属性相同，但是status不同则更新existing.status为ref.status、返回needReconcile为true
//  如果属性不同，则更新existing属性=ref属性，如果ref.DeletionTimestamp存在，则返回needGracefulDelete为true。否则needUpdate为true
func checkAndUpdatePod(existing, ref *v1.Pod) (needUpdate, needReconcile, needGracefulDelete bool) {

	// 1. this is a reconcile
	// TODO: it would be better to update the whole object and only preserve certain things
	//       like the source annotation or the UID (to ensure safety)
	// pod的Spec、Label、DeletionTimestamp、DeletionGracePeriodSeconds、Annotations（除了内部的localannotation）一样
	if !podsDifferSemantically(existing, ref) {
		// this is not an update
		// Only check reconcile when it is not an update, because if the pod is going to
		// be updated, an extra reconcile is unnecessary
		if !reflect.DeepEqual(existing.Status, ref.Status) {
			// Pod with changed pod status needs reconcile, because kubelet should
			// be the source of truth of pod status.
			existing.Status = ref.Status
			needReconcile = true
		}
		return
	}

	// Overwrite the first-seen time with the existing one. This is our own
	// internal annotation, there is no need to update.
	ref.Annotations[kubetypes.ConfigFirstSeenAnnotationKey] = existing.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]

	existing.Spec = ref.Spec
	existing.Labels = ref.Labels
	existing.DeletionTimestamp = ref.DeletionTimestamp
	existing.DeletionGracePeriodSeconds = ref.DeletionGracePeriodSeconds
	existing.Status = ref.Status
	// 替换existing的annotation为ref的annotation--（除了已有的localAnnotations）
	updateAnnotations(existing, ref)

	// 2. this is an graceful delete
	if ref.DeletionTimestamp != nil {
		needGracefulDelete = true
	} else {
		// 3. this is an update
		needUpdate = true
	}

	return
}

// Sync sends a copy of the current state through the update channel.
func (s *podStorage) Sync() {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()
	s.updates <- kubetypes.PodUpdate{Pods: s.MergedState().([]*v1.Pod), Op: kubetypes.SET, Source: kubetypes.AllSource}
}

// Object implements config.Accessor
// 所有source里的pod列表
func (s *podStorage) MergedState() interface{} {
	s.podLock.RLock()
	defer s.podLock.RUnlock()
	pods := make([]*v1.Pod, 0)
	for _, sourcePods := range s.pods {
		for _, podRef := range sourcePods {
			pods = append(pods, podRef.DeepCopy())
		}
	}
	return pods
}

func copyPods(sourcePods []*v1.Pod) []*v1.Pod {
	pods := []*v1.Pod{}
	for _, source := range sourcePods {
		// Use a deep copy here just in case
		pods = append(pods, source.DeepCopy())
	}
	return pods
}
