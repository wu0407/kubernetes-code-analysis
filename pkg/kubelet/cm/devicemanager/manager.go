/*
Copyright 2017 The Kubernetes Authors.

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

package devicemanager

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"google.golang.org/grpc"
	"k8s.io/klog"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	errorsutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"
	"k8s.io/kubernetes/pkg/features"
	podresourcesapi "k8s.io/kubernetes/pkg/kubelet/apis/podresources/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
	cputopology "k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/checkpoint"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager"
	"k8s.io/kubernetes/pkg/kubelet/cm/topologymanager/bitmask"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	schedulernodeinfo "k8s.io/kubernetes/pkg/scheduler/nodeinfo"
	"k8s.io/kubernetes/pkg/util/selinux"
)

// ActivePodsFunc is a function that returns a list of pods to reconcile.
type ActivePodsFunc func() []*v1.Pod

// monitorCallback is the function called when a device's health state changes,
// or new devices are reported, or old devices are deleted.
// Updated contains the most recent state of the Device.
type monitorCallback func(resourceName string, devices []pluginapi.Device)

// ManagerImpl is the structure in charge of managing Device Plugins.
type ManagerImpl struct {
	socketname string
	socketdir  string

	endpoints map[string]endpointInfo // Key is ResourceName
	mutex     sync.Mutex

	server *grpc.Server
	wg     sync.WaitGroup

	// activePods is a method for listing active pods on the node
	// so the amount of pluginResources requested by existing pods
	// could be counted when updating allocated devices
	activePods ActivePodsFunc

	// sourcesReady provides the readiness of kubelet configuration sources such as apiserver update readiness.
	// We use it to determine when we can purge inactive pods from checkpointed state.
	sourcesReady config.SourcesReady

	// callback is used for updating devices' states in one time call.
	// e.g. a new device is advertised, two old devices are deleted and a running device fails.
	callback monitorCallback

	// allDevices is a map by resource name of all the devices currently registered to the device manager
	// 所有已经注册的device，第一个key是resource，第二个key是device id
	allDevices map[string]map[string]pluginapi.Device

	// healthyDevices contains all of the registered healthy resourceNames and their exported device IDs.
	healthyDevices map[string]sets.String

	// unhealthyDevices contains all of the unhealthy devices and their exported device IDs.
	unhealthyDevices map[string]sets.String

	// allocatedDevices contains allocated deviceIds, keyed by resourceName.
	// 已经分配的devices，资源名和对应的deviceIds
	allocatedDevices map[string]sets.String

	// podDevices contains pod to allocated device mapping.
	// pod uid与pod里每个container已分配的资源
	podDevices        podDevices
	checkpointManager checkpointmanager.CheckpointManager

	// List of NUMA Nodes available on the underlying machine
	numaNodes []int

	// Store of Topology Affinties that the Device Manager can query.
	topologyAffinityStore topologymanager.Store

	// devicesToReuse contains devices that can be reused as they have been allocated to
	// init containers.
	// 分配给init container的可以重用资源
	// PodReusableDevices是pod uid与resource与device id
	devicesToReuse PodReusableDevices
}

type endpointInfo struct {
	e    endpoint
	opts *pluginapi.DevicePluginOptions
}

type sourcesReadyStub struct{}

// PodReusableDevices is a map by pod name of devices to reuse.
// pod uid与resource与device id
type PodReusableDevices map[string]map[string]sets.String

func (s *sourcesReadyStub) AddSource(source string) {}
func (s *sourcesReadyStub) AllReady() bool          { return true }

// NewManagerImpl creates a new manager.
func NewManagerImpl(numaNodeInfo cputopology.NUMANodeInfo, topologyAffinityStore topologymanager.Store) (*ManagerImpl, error) {
	return newManagerImpl(pluginapi.KubeletSocket, numaNodeInfo, topologyAffinityStore)
}

func newManagerImpl(socketPath string, numaNodeInfo cputopology.NUMANodeInfo, topologyAffinityStore topologymanager.Store) (*ManagerImpl, error) {
	klog.V(2).Infof("Creating Device Plugin manager at %s", socketPath)

	if socketPath == "" || !filepath.IsAbs(socketPath) {
		return nil, fmt.Errorf(errBadSocket+" %s", socketPath)
	}

	var numaNodes []int
	for node := range numaNodeInfo {
		numaNodes = append(numaNodes, node)
	}

	dir, file := filepath.Split(socketPath)
	manager := &ManagerImpl{
		endpoints: make(map[string]endpointInfo),

		socketname:            file,
		socketdir:             dir,
		allDevices:            make(map[string]map[string]pluginapi.Device),
		healthyDevices:        make(map[string]sets.String),
		unhealthyDevices:      make(map[string]sets.String),
		allocatedDevices:      make(map[string]sets.String),
		podDevices:            make(podDevices),
		numaNodes:             numaNodes,
		topologyAffinityStore: topologyAffinityStore,
		devicesToReuse:        make(PodReusableDevices),
	}
	manager.callback = manager.genericDeviceUpdateCallback

	// The following structures are populated with real implementations in manager.Start()
	// Before that, initializes them to perform no-op operations.
	manager.activePods = func() []*v1.Pod { return []*v1.Pod{} }
	manager.sourcesReady = &sourcesReadyStub{}
	checkpointManager, err := checkpointmanager.NewCheckpointManager(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize checkpoint manager: %v", err)
	}
	manager.checkpointManager = checkpointManager

	return manager, nil
}

// device plugin的device发生变化，就会调用这个方法
// 用新的device替换m.healthyDevices、m.unhealthyDevices、m.allDevices里的resource，然后将resource分配状态和健康的resource资源（m.healthyDevices和m.podDevices转换格式后数据）写入到"/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"文件
func (m *ManagerImpl) genericDeviceUpdateCallback(resourceName string, devices []pluginapi.Device) {
	m.mutex.Lock()
	// 先执行置空操作，因为有可能devices为空（代表device manager没有device）
	m.healthyDevices[resourceName] = sets.NewString()
	m.unhealthyDevices[resourceName] = sets.NewString()
	m.allDevices[resourceName] = make(map[string]pluginapi.Device)
	// 只在devices不为空的情况下执行，即device manager变成没有device情况是不执行的
	for _, dev := range devices {
		m.allDevices[resourceName][dev.ID] = dev
		if dev.Health == pluginapi.Healthy {
			m.healthyDevices[resourceName].Insert(dev.ID)
		} else {
			m.unhealthyDevices[resourceName].Insert(dev.ID)
		}
	}
	m.mutex.Unlock()
	// 将数据（resource分配状态和健康的resource资源）写入到"/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"文件
	if err := m.writeCheckpoint(); err != nil {
		klog.Errorf("writing checkpoint encountered %v", err)
	}
}

// 移除dir目录下非checkpoint文件（不包括目录）
func (m *ManagerImpl) removeContents(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range names {
		filePath := filepath.Join(dir, name)
		if filePath == m.checkpointFile() {
			continue
		}
		stat, err := os.Stat(filePath)
		if err != nil {
			klog.Errorf("Failed to stat file %s: %v", filePath, err)
			continue
		}
		if stat.IsDir() {
			continue
		}
		err = os.RemoveAll(filePath)
		if err != nil {
			errs = append(errs, err)
			klog.Errorf("Failed to remove file %s: %v", filePath, err)
			continue
		}
	}
	return errorsutil.NewAggregate(errs)
}

// checkpointFile returns device plugin checkpoint file path.
func (m *ManagerImpl) checkpointFile() string {
	return filepath.Join(m.socketdir, kubeletDeviceManagerCheckpoint)
}

// Start starts the Device Plugin Manager and start initialization of
// podDevices and allocatedDevices information from checkpointed state and
// starts device plugin registration service.
// 读取/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint文件，恢复状态
// 移除/var/lib/kubelet/device-plugins/文件夹下非"kubelet_internal_checkpoint"文件
// 启动grpc服务，监听socket为/var/lib/kubelet/device-plugins/kubelet.socket
func (m *ManagerImpl) Start(activePods ActivePodsFunc, sourcesReady config.SourcesReady) error {
	klog.V(2).Infof("Starting Device Plugin manager")

	m.activePods = activePods
	m.sourcesReady = sourcesReady

	// Loads in allocatedDevices information from disk.
	// 从checkpoint读取数据，设置m.podDevices、m.allocatedDevices、m.healthyDevices、m.unhealthyDevices、m.endpoints
	err := m.readCheckpoint()
	if err != nil {
		klog.Warningf("Continue after failing to read checkpoint file. Device allocation info may NOT be up-to-date. Err: %v", err)
	}

	socketPath := filepath.Join(m.socketdir, m.socketname)
	if err = os.MkdirAll(m.socketdir, 0750); err != nil {
		return err
	}
	if selinux.SELinuxEnabled() {
		if err := selinux.SetFileLabel(m.socketdir, config.KubeletPluginsDirSELinuxLabel); err != nil {
			klog.Warningf("Unprivileged containerized plugins might not work. Could not set selinux context on %s: %v", m.socketdir, err)
		}
	}

	// Removes all stale sockets in m.socketdir. Device plugins can monitor
	// this and use it as a signal to re-register with the new Kubelet.
	// 移除m.socketdir目录下非checkpoint文件
	if err := m.removeContents(m.socketdir); err != nil {
		klog.Errorf("Fail to clean up stale contents under %s: %v", m.socketdir, err)
	}

	s, err := net.Listen("unix", socketPath)
	if err != nil {
		klog.Errorf(errListenSocket+" %v", err)
		return err
	}

	m.wg.Add(1)
	m.server = grpc.NewServer([]grpc.ServerOption{}...)

	// 注册m.Register方法用于服务grpc请求
	// device plugin会调用"/v1beta1.Registration/Register"来注册自己
	pluginapi.RegisterRegistrationServer(m.server, m)
	go func() {
		defer m.wg.Done()
		m.server.Serve(s)
	}()

	klog.V(2).Infof("Serving device plugin registration server on %q", socketPath)

	return nil
}

// GetWatcherHandler returns the plugin handler
func (m *ManagerImpl) GetWatcherHandler() cache.PluginHandler {
	if f, err := os.Create(m.socketdir + "DEPRECATION"); err != nil {
		klog.Errorf("Failed to create deprecation file at %s", m.socketdir)
	} else {
		f.Close()
		klog.V(4).Infof("created deprecation file %s", f.Name())
	}

	return cache.PluginHandler(m)
}

// ValidatePlugin validates a plugin if the version is correct and the name has the format of an extended resource
func (m *ManagerImpl) ValidatePlugin(pluginName string, endpoint string, versions []string) error {
	klog.V(2).Infof("Got Plugin %s at endpoint %s with versions %v", pluginName, endpoint, versions)

	if !m.isVersionCompatibleWithPlugin(versions) {
		return fmt.Errorf("manager version, %s, is not among plugin supported versions %v", pluginapi.Version, versions)
	}

	if !v1helper.IsExtendedResourceName(v1.ResourceName(pluginName)) {
		return fmt.Errorf("invalid name of device plugin socket: %s", fmt.Sprintf(errInvalidResourceName, pluginName))
	}

	return nil
}

// RegisterPlugin starts the endpoint and registers it
// TODO: Start the endpoint and wait for the First ListAndWatch call
//       before registering the plugin
// plugin manager会注册
func (m *ManagerImpl) RegisterPlugin(pluginName string, endpoint string, versions []string) error {
	klog.V(2).Infof("Registering Plugin %s at endpoint %s", pluginName, endpoint)

	e, err := newEndpointImpl(endpoint, pluginName, m.callback)
	if err != nil {
		return fmt.Errorf("failed to dial device plugin with socketPath %s: %v", endpoint, err)
	}

	options, err := e.client.GetDevicePluginOptions(context.Background(), &pluginapi.Empty{})
	if err != nil {
		return fmt.Errorf("failed to get device plugin options: %v", err)
	}

	m.registerEndpoint(pluginName, options, e)
	go m.runEndpoint(pluginName, e)

	return nil
}

// DeRegisterPlugin deregisters the plugin
// TODO work on the behavior for deregistering plugins
// e.g: Should we delete the resource
// plugin manager会注册
func (m *ManagerImpl) DeRegisterPlugin(pluginName string) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// Note: This will mark the resource unhealthy as per the behavior
	// in runEndpoint
	if eI, ok := m.endpoints[pluginName]; ok {
		eI.e.stop()
	}
}

func (m *ManagerImpl) isVersionCompatibleWithPlugin(versions []string) bool {
	// TODO(vikasc): Currently this is fine as we only have a single supported version. When we do need to support
	// multiple versions in the future, we may need to extend this function to return a supported version.
	// E.g., say kubelet supports v1beta1 and v1beta2, and we get v1alpha1 and v1beta1 from a device plugin,
	// this function should return v1beta1
	for _, version := range versions {
		for _, supportedVersion := range pluginapi.SupportedVersions {
			if version == supportedVersion {
				return true
			}
		}
	}
	return false
}

// Allocate is the call that you can use to allocate a set of devices
// from the registered device plugins.
// 从注册的device plugins，分配container的limit中所有需要的资源
func (m *ManagerImpl) Allocate(pod *v1.Pod, container *v1.Container) error {
	if _, ok := m.devicesToReuse[string(pod.UID)]; !ok {
		m.devicesToReuse[string(pod.UID)] = make(map[string]sets.String)
	}
	// If pod entries to m.devicesToReuse other than the current pod exist, delete them.
	// 删除m.devicesToReuse里所有key不为pod.UID的item
	for podUID := range m.devicesToReuse {
		if podUID != string(pod.UID) {
			delete(m.devicesToReuse, podUID)
		}
	}
	// Allocate resources for init containers first as we know the caller always loops
	// through init containers before looping through app containers. Should the caller
	// ever change those semantics, this logic will need to be amended.
	for _, initContainer := range pod.Spec.InitContainers {
		// 如果container是init container，
		if container.Name == initContainer.Name {
			// 分配container请求的extend资源
			// 先在manager内部进行分配resource的device id（优先使用可重用资源），然后通过grpc调用device plugin进行分配device id
			if err := m.allocateContainerResources(pod, container, m.devicesToReuse[string(pod.UID)]); err != nil {
				return err
			}
			// m.devicesToReuse[string(pod.UID)]（pod的可重用resource的device id）添加从m.podDevices中获取所有已经分配给pod里这个init container的resource对应的device id，然后返回
			m.podDevices.addContainerAllocatedResources(string(pod.UID), container.Name, m.devicesToReuse[string(pod.UID)])
			return nil
		}
	}
	// 不是init container

	// 分配container请求的extend资源
	// 先在manager内部进行分配resource的device id（优先使用可重用资源），然后通过grpc调用device plugin进行分配device id
	if err := m.allocateContainerResources(pod, container, m.devicesToReuse[string(pod.UID)]); err != nil {
		return err
	}
	// m.devicesToReuse[string(pod.UID)]（pod的可重用resource的device id）移除分配给pod的这个container的resource对应的device id
	m.podDevices.removeContainerAllocatedResources(string(pod.UID), container.Name, m.devicesToReuse[string(pod.UID)])
	return nil

}

// UpdatePluginResources updates node resources based on devices already allocated to pods.
// 让所有已经分配的device resource，在node的allocatableResource里的resource数量等于已经分配的数量
func (m *ManagerImpl) UpdatePluginResources(node *schedulernodeinfo.NodeInfo, attrs *lifecycle.PodAdmitAttributes) error {
	pod := attrs.Pod

	m.mutex.Lock()
	defer m.mutex.Unlock()

	// quick return if no pluginResources requested
	// pod里的所有container都没有请求device resource，直接返回
	if _, podRequireDevicePluginResource := m.podDevices[string(pod.UID)]; !podRequireDevicePluginResource {
		return nil
	}

	// 让所有已经分配的device resource，在node的allocatableResource里的resource数量等于已经分配的数量
	m.sanitizeNodeAllocatable(node)
	return nil
}

// Register registers a device plugin.
// 注册device plugin
// 1. 验证版本是否兼容
// 2. 验证提供的resource名字是否合法
// 3. 启动一个goroutine，进行device plugin的生命周期管理
func (m *ManagerImpl) Register(ctx context.Context, r *pluginapi.RegisterRequest) (*pluginapi.Empty, error) {
	klog.Infof("Got registration request from device plugin with resource name %q", r.ResourceName)
	metrics.DevicePluginRegistrationCount.WithLabelValues(r.ResourceName).Inc()
	var versionCompatible bool
	for _, v := range pluginapi.SupportedVersions {
		if r.Version == v {
			versionCompatible = true
			break
		}
	}
	// 版本不兼容，返回错误
	if !versionCompatible {
		errorString := fmt.Sprintf(errUnsupportedVersion, r.Version, pluginapi.SupportedVersions)
		klog.Infof("Bad registration request from device plugin with resource name %q: %s", r.ResourceName, errorString)
		return &pluginapi.Empty{}, fmt.Errorf(errorString)
	}

	// r.ResourceName名字是否合法
	// 没有包含"/"或“kubernetes.io/”为前缀或“request.”为前缀或"requests."+r.ResourceName组成的字符串不合法，则直接返回错误
	if !v1helper.IsExtendedResourceName(v1.ResourceName(r.ResourceName)) {
		errorString := fmt.Sprintf(errInvalidResourceName, r.ResourceName)
		klog.Infof("Bad registration request from device plugin: %s", errorString)
		return &pluginapi.Empty{}, fmt.Errorf(errorString)
	}

	// TODO: for now, always accepts newest device plugin. Later may consider to
	// add some policies here, e.g., verify whether an old device plugin with the
	// same resource name is still alive to determine whether we want to accept
	// the new registration.
	// 1. m.endpoints里增加一个endpoint
	// 2. 启动一个goroutine，进行device plugin的生命周期管理，监视device plugin的device变更（增加、减少、消失）
	go m.addEndpoint(r)

	return &pluginapi.Empty{}, nil
}

// Stop is the function that can stop the gRPC server.
// Can be called concurrently, more than once, and is safe to call
// without a prior Start.
func (m *ManagerImpl) Stop() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	for _, eI := range m.endpoints {
		eI.e.stop()
	}

	if m.server == nil {
		return nil
	}
	m.server.Stop()
	m.wg.Wait()
	m.server = nil
	return nil
}

// resource的endpoint添加m.endpoints里
func (m *ManagerImpl) registerEndpoint(resourceName string, options *pluginapi.DevicePluginOptions, e endpoint) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.endpoints[resourceName] = endpointInfo{e: e, opts: options}
	klog.V(2).Infof("Registered endpoint %v", e)
}

// 1. 与device plugin建立stream流，发送ListAndWatch请求给device plugin，device plugin会返回所有device，并进行类似watch操作。
// 2. 当发生任何变化或第一次通信，则执行callback（更新resource在ManagerImpl的相关字段里状态，然后将resource分配状态和健康的resource资源写入checkpoints文件）
// 3. 当与device plugin进行grpc通信发生错误或写入checkpoint发生错误，代表这个device plugin注销了，则将resource标记成unhealthy
func (m *ManagerImpl) runEndpoint(resourceName string, e endpoint) {
	// 与device plugin建立stream流，发送ListAndWatch请求给device plugin，device plugin会返回所有device，并进行类似watch操作。当device plugin的device发生变化时候，device plugin会返回新的device列表。
	// 当发生任何变化或第一次通信，则执行callback（更新resource在ManagerImpl的相关字段里状态，然后将resource分配状态和健康的resource资源写入checkpoints文件）
	// 一般不发生错误，e.run()是不返回的。当与device plugin进行grpc通信发生错误（有可能device plugin退出）或写入checkpoint发生错误，e.run()会返回。
	e.run()
	// 当e.run()退出，代表这个device plugin注销了。
	// 设置e.stopTime时间为当前时间
	e.stop()

	m.mutex.Lock()
	defer m.mutex.Unlock()

	// 将resource标记成unhealthy
	// m.healthyDevices里的resource置空，m.unhealthyDevices添加resource
	if old, ok := m.endpoints[resourceName]; ok && old.e == e {
		m.markResourceUnhealthy(resourceName)
	}

	klog.V(2).Infof("Endpoint (%s, %v) became unhealthy", resourceName, e)
}

// 1. m.endpoints里增加一个endpoint
// 2. 启动一个goroutine，进行device plugin的生命周期管理，监视device plugin的device变更（增加、减少、消失）
func (m *ManagerImpl) addEndpoint(r *pluginapi.RegisterRequest) {
	new, err := newEndpointImpl(filepath.Join(m.socketdir, r.Endpoint), r.ResourceName, m.callback)
	if err != nil {
		klog.Errorf("Failed to dial device plugin with request %v: %v", r, err)
		return
	}
	// resource的endpoint添加m.endpoints里
	m.registerEndpoint(r.ResourceName, r.Options, new)
	// 启动一个goroutine，与device plugin建立ListAndWatch请求（发现device plugin的所有device，监听device plugin的device变化），每次变化都会将resource分配状态和健康的resource资源写入checkpoints文件
	// 处理device plugin进程退出，或通信发生错误，或写入checkpoint发生错误，则执行注销操作
	go func() {
		m.runEndpoint(r.ResourceName, new)
	}()
}

// 将resource标记成unhealthy
// m.healthyDevices里的resource置空，m.unhealthyDevices添加resource的所有device id
func (m *ManagerImpl) markResourceUnhealthy(resourceName string) {
	klog.V(2).Infof("Mark all resources Unhealthy for resource %s", resourceName)
	healthyDevices := sets.NewString()
	if _, ok := m.healthyDevices[resourceName]; ok {
		healthyDevices = m.healthyDevices[resourceName]
		m.healthyDevices[resourceName] = sets.NewString()
	}
	if _, ok := m.unhealthyDevices[resourceName]; !ok {
		m.unhealthyDevices[resourceName] = sets.NewString()
	}
	m.unhealthyDevices[resourceName] = m.unhealthyDevices[resourceName].Union(healthyDevices)
}

// GetCapacity is expected to be called when Kubelet updates its node status.
// The first returned variable contains the registered device plugin resource capacity.
// The second returned variable contains the registered device plugin resource allocatable.
// The third returned variable contains previously registered resources that are no longer active.
// Kubelet uses this information to update resource capacity/allocatable in its node status.
// After the call, device plugin can remove the inactive resources from its internal list as the
// change is already reflected in Kubelet node status.
// Note in the special case after Kubelet restarts, device plugin resource capacities can
// temporarily drop to zero till corresponding device plugins re-register. This is OK because
// cm.UpdatePluginResource() run during predicate Admit guarantees we adjust nodeinfo
// capacity for already allocated pods so that they can continue to run. However, new pods
// requiring device plugin resources will not be scheduled till device plugin re-registers.
// 这个会在更新node status时候被调用，在pkg\kubelet\cm\container_manager_linux.go (cm *containerManagerImpl) GetDevicePluginResourceCapacity()
// 返回第一个参数，所有resource的capacity（统计所有未停止或停止未超过5分钟的device plugin，包括m.healthyDevices和m.unhealthyDevices）
// 返回第二个参数，所有resource的allocatable（统计所有未停止或停止未超过5分钟的device plugin，只包括m.healthyDevices）
// 返回第三个参数，所有删除掉的resource（停止超过5分钟的device plugin，和在m.healthyDevices且不在m.endpoints，和在m.unhealthyDevices且不在m.endpoints）
func (m *ManagerImpl) GetCapacity() (v1.ResourceList, v1.ResourceList, []string) {
	needsUpdateCheckpoint := false
	var capacity = v1.ResourceList{}
	var allocatable = v1.ResourceList{}
	deletedResources := sets.NewString()
	m.mutex.Lock()
	// 处理m.healthyDevices
	for resourceName, devices := range m.healthyDevices {
		eI, ok := m.endpoints[resourceName]
		// resource在m.endpoints里且device plugin已经停止且停止时间超过5分钟（这种情况发生在device plugin发生故障断联了，断联了不会调用callback（不会更新m.healthyDevices和m.unhealthyDevices和m.allDevices，即还在m.healthyDevices和m.unhealthyDevices和m.allDevices里），但会设置eI.e.stopTime，这个是在m.runEndpoint处理逻辑），或resource不在m.endpoints里
		// 从m.endpoints和m.healthyDevices删除这个resource
		if (ok && eI.e.stopGracePeriodExpired()) || !ok {
			// The resources contained in endpoints and (un)healthyDevices
			// should always be consistent. Otherwise, we run with the risk
			// of failing to garbage collect non-existing resources or devices.
			if !ok {
				klog.Errorf("unexpected: healthyDevices and endpoints are out of sync")
			}
			delete(m.endpoints, resourceName)
			delete(m.healthyDevices, resourceName)
			deletedResources.Insert(resourceName)
			needsUpdateCheckpoint = true
		} else {
			// 否则进行统计capacity和allocatable
			capacity[v1.ResourceName(resourceName)] = *resource.NewQuantity(int64(devices.Len()), resource.DecimalSI)
			allocatable[v1.ResourceName(resourceName)] = *resource.NewQuantity(int64(devices.Len()), resource.DecimalSI)
		}
	}
	// 处理m.unhealthyDevices
	for resourceName, devices := range m.unhealthyDevices {
		eI, ok := m.endpoints[resourceName]
		// resource在m.endpoints里且device plugin已经停止且停止时间超过5分钟（这种情况发生在device plugin发生故障断联了，断联了不会调用callback（不会更新m.healthyDevices和m.unhealthyDevices和m.allDevices，即还在m.healthyDevices和m.unhealthyDevices和m.allDevices里），但会设置eI.e.stopTime，这个是在m.runEndpoint处理逻辑），或resource不在m.endpoints里
		// 从m.endpoints和m.healthyDevices删除这个resource
		if (ok && eI.e.stopGracePeriodExpired()) || !ok {
			if !ok {
				klog.Errorf("unexpected: unhealthyDevices and endpoints are out of sync")
			}
			delete(m.endpoints, resourceName)
			delete(m.unhealthyDevices, resourceName)
			deletedResources.Insert(resourceName)
			needsUpdateCheckpoint = true
		} else {
			// 否则进行统计capacity（如果resource的device id分配给pod变成unhealthy，统计在capacity，不会统计在allocatable）
			capacityCount := capacity[v1.ResourceName(resourceName)]
			unhealthyCount := *resource.NewQuantity(int64(devices.Len()), resource.DecimalSI)
			capacityCount.Add(unhealthyCount)
			capacity[v1.ResourceName(resourceName)] = capacityCount
		}
	}
	m.mutex.Unlock()
	if needsUpdateCheckpoint {
		// 将数据（resource分配状态和健康的resource资源）写入到"/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"文件
		if err := m.writeCheckpoint(); err != nil {
			klog.Errorf("writing checkpoint encountered %v", err)
		}
	}
	return capacity, allocatable, deletedResources.UnsortedList()
}

// Checkpoints device to container allocation information to disk.
// 将数据（resource分配状态和健康的resource资源）写入到"/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"文件
func (m *ManagerImpl) writeCheckpoint() error {
	m.mutex.Lock()
	registeredDevs := make(map[string][]string)
	for resource, devices := range m.healthyDevices {
		registeredDevs[resource] = devices.UnsortedList()
	}
	// m.podDevices.toCheckpointData()是podDevices转成[]checkpoint.PodDevicesEntry
	// 生成checkpoint数据
	data := checkpoint.New(m.podDevices.toCheckpointData(),
		registeredDevs)
	m.mutex.Unlock()
	// 将数据（resource分配状态和resource资源）写入到"/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"文件
	err := m.checkpointManager.CreateCheckpoint(kubeletDeviceManagerCheckpoint, data)
	if err != nil {
		err2 := fmt.Errorf("failed to write checkpoint file %q: %v", kubeletDeviceManagerCheckpoint, err)
		klog.Warning(err2)
		return err2
	}
	return nil
}

// Reads device to container allocation information from disk, and populates
// m.allocatedDevices accordingly.
// 从checkpoint读取数据，设置m.podDevices、m.allocatedDevices、m.healthyDevices、m.unhealthyDevices、m.endpoints
func (m *ManagerImpl) readCheckpoint() error {
	registeredDevs := make(map[string][]string)
	devEntries := make([]checkpoint.PodDevicesEntry, 0)
	cp := checkpoint.New(devEntries, registeredDevs)
	err := m.checkpointManager.GetCheckpoint(kubeletDeviceManagerCheckpoint, cp)
	if err != nil {
		if err == errors.ErrCheckpointNotFound {
			klog.Warningf("Failed to retrieve checkpoint for %q: %v", kubeletDeviceManagerCheckpoint, err)
			return nil
		}
		return err
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	podDevices, registeredDevs := cp.GetData()
	// checkpoint的podDevices插入到m.podDevices
	m.podDevices.fromCheckpointData(podDevices)
	// 返回所有资源和对应的deviceIds
	m.allocatedDevices = m.podDevices.devices()
	for resource := range registeredDevs {
		// During start up, creates empty healthyDevices list so that the resource capacity
		// will stay zero till the corresponding device plugin re-registers.
		m.healthyDevices[resource] = sets.NewString()
		m.unhealthyDevices[resource] = sets.NewString()
		m.endpoints[resource] = endpointInfo{e: newStoppedEndpointImpl(resource), opts: nil}
	}
	return nil
}

// UpdateAllocatedDevices frees any Devices that are bound to terminated pods.
// 移除m.podDevices中已经terminated的pod，更新m.podDevices（pod与pod各个container分配的资源）和m.allocatedDevices（已经分配资源和device id）
func (m *ManagerImpl) UpdateAllocatedDevices() {
	// 返回所有pods中（普通pod和static pod），pod的Phase不是"Failed"和"Succeeded"，或pod没有被删除，或pod被删除且至少一个container为running
	activePods := m.activePods()
	// 不是所有pod来源都已经ready，直接返回
	if !m.sourcesReady.AllReady() {
		return
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	// 返回所有已经分配了的资源的pod uid
	podsToBeRemoved := m.podDevices.pods()
	// podsToBeRemoved移除掉所有active的pod，podsToBeRemoved里剩下非active pod（代表需要被移除的pod uid）
	for _, pod := range activePods {
		podsToBeRemoved.Delete(string(pod.UID))
	}
	// 需要被移除的pod uid个数小于等于0，直接返回
	if len(podsToBeRemoved) <= 0 {
		return
	}
	klog.V(3).Infof("pods to be removed: %v", podsToBeRemoved.List())
	// m.podDevices（已经分配的资源列表）里移除非active pod
	m.podDevices.delete(podsToBeRemoved.List())
	// Regenerated allocatedDevices after we update pod allocation information.
	// 更新已经分配的资源和对应的deviceIds
	m.allocatedDevices = m.podDevices.devices()
}

// Returns list of device Ids we need to allocate with Allocate rpc call.
// Returns empty list in case we don't need to issue the Allocate rpc call.
// 返回分配给container的device id集合
func (m *ManagerImpl) devicesToAllocate(podUID, contName, resource string, required int, reusableDevices sets.String) (sets.String, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	needed := required
	// Gets list of devices that have already been allocated.
	// This can happen if a container restarts for example.
	// 返回已经分配给container的resource的所有device id
	devices := m.podDevices.containerDevices(podUID, contName, resource)
	// container的resource已经分配过
	if devices != nil {
		klog.V(3).Infof("Found pre-allocated devices for resource %s container %q in Pod %q: %v", resource, contName, podUID, devices.List())
		needed = needed - devices.Len()
		// A pod's resource is not expected to change once admitted by the API server,
		// so just fail loudly here. We can revisit this part if this no longer holds.
		// 如果container已经分配的resource不满足需要的，则直接返回错误
		if needed != 0 {
			return nil, fmt.Errorf("pod %q container %q changed request for resource %q from %d to %d", podUID, contName, resource, devices.Len(), required)
		}
	}
	// container的resource未分配过且container需要的resource为0，或container已经分配的resource满足需要的，则直接返回
	if needed == 0 {
		// No change, no work.
		return nil, nil
	}
	klog.V(3).Infof("Needs to allocate %d %q for pod %q container %q", needed, resource, podUID, contName)
	// Needs to allocate additional devices.
	// 需要的resource不在m.healthyDevices里返回错误
	if _, ok := m.healthyDevices[resource]; !ok {
		return nil, fmt.Errorf("can't allocate unregistered device %s", resource)
	}
	devices = sets.NewString()
	// Allocates from reusableDevices list first.
	// 先从reusableDevices里分配资源
	for device := range reusableDevices {
		devices.Insert(device)
		needed--
		// 满足需求则直接返回分配的device id
		if needed == 0 {
			return devices, nil
		}
	}
	// Needs to allocate additional devices.
	if m.allocatedDevices[resource] == nil {
		m.allocatedDevices[resource] = sets.NewString()
	}
	// Gets Devices in use.
	devicesInUse := m.allocatedDevices[resource]
	// Gets a list of available devices.
	// 从m.healthyDevices获取resource未分配的device id列表
	available := m.healthyDevices[resource].Difference(devicesInUse)
	// resource未分配的device id列表不满足需求，则返回错误
	if available.Len() < needed {
		return nil, fmt.Errorf("requested number of devices unavailable for %s. Requested: %d, Available: %d", resource, needed, available.Len())
	}
	// By default, pull devices from the unsorted list of available devices.
	allocated := available.UnsortedList()[:needed]
	// If topology alignment is desired, update allocated to the set of devices
	// with the best alignment.
	// 从topology manager中获取container的TopologyHint
	// 怎么生成TopologyHint？
	// 在pod的创建过程，会先有kl.podAdmitHandlers admit过程（kl.canAdmitPod，pkg\kubelet\kubelet.go），进行计算container最佳的TopologyHint，保存到topology manager（ pkg\kubelet\cm\topologymanager\topology_manager.go）的podTopologyHints里
	hint := m.topologyAffinityStore.GetAffinity(podUID, contName)
	// resource定义了Topology且TopologyHint里有NUMANodeAffinity，则根据hint.NUMANodeAffinity进行分配
	if m.deviceHasTopologyAlignment(resource) && hint.NUMANodeAffinity != nil {
		// 根据affinity和available（可用的device id）进行request分配
		// 先分配affinity匹配的，再分配affinity不匹配，再分配没有关联任何numa node
		allocated = m.takeByTopology(resource, available, hint.NUMANodeAffinity, needed)
	}
	// Updates m.allocatedDevices with allocated devices to prevent them
	// from being allocated to other pods/containers, given that we are
	// not holding lock during the rpc call.
	// 更新m.allocatedDevices（resource已经分配的device id集合）
	for _, device := range allocated {
		m.allocatedDevices[resource].Insert(device)
		devices.Insert(device)
	}
	return devices, nil
}

// 根据affinity和available（可用的device id）进行request分配
// 先分配affinity匹配的，再分配affinity不匹配，再分配没有关联任何numa node
func (m *ManagerImpl) takeByTopology(resource string, available sets.String, affinity bitmask.BitMask, request int) []string {
	// Build a map of NUMA Nodes to the devices associated with them. A
	// device may be associated to multiple NUMA nodes at the same time. If an
	// available device does not have any NUMA Nodes associated with it, add it
	// to a list of NUMA Nodes for the fake NUMANode -1.
	perNodeDevices := make(map[int]sets.String)
	nodeWithoutTopology := -1
	for d := range available {
		// resource可用的device id的Topology为nil或resource可用的device id的Topology.Nodes为空，说明这个device id没有关联任何numa node
		// 设置key为-1
		if m.allDevices[resource][d].Topology == nil || len(m.allDevices[resource][d].Topology.Nodes) == 0 {
			if _, ok := perNodeDevices[nodeWithoutTopology]; !ok {
				perNodeDevices[nodeWithoutTopology] = sets.NewString()
			}
			perNodeDevices[nodeWithoutTopology].Insert(d)
			continue
		}

		// 否则key设置为相应的node id
		for _, node := range m.allDevices[resource][d].Topology.Nodes {
			if _, ok := perNodeDevices[int(node.ID)]; !ok {
				perNodeDevices[int(node.ID)] = sets.NewString()
			}
			perNodeDevices[int(node.ID)].Insert(d)
		}
	}

	// Get a flat list of all of the nodes associated with available devices.
	var nodes []int
	for node := range perNodeDevices {
		nodes = append(nodes, node)
	}

	// Sort the list of nodes by how many devices they contain.
	// 按照node包含的device id从少到大进行排序
	sort.Slice(nodes, func(i, j int) bool {
		return perNodeDevices[i].Len() < perNodeDevices[j].Len()
	})

	// Generate three sorted lists of devices. Devices in the first list come
	// from valid NUMA Nodes contained in the affinity mask. Devices in the
	// second list come from valid NUMA Nodes not in the affinity mask. Devices
	// in the third list come from devices with no NUMA Node association (i.e.
	// those mapped to the fake NUMA Node -1). Because we loop through the
	// sorted list of NUMA nodes in order, within each list, devices are sorted
	// by their connection to NUMA Nodes with more devices on them.
	var fromAffinity []string
	var notFromAffinity []string
	var withoutTopology []string
	for d := range available {
		// Since the same device may be associated with multiple NUMA Nodes. We
		// need to be careful not to add each device to multiple lists. The
		// logic below ensures this by breaking after the first NUMA node that
		// has the device is encountered.
		for _, n := range nodes {
			// d（可用device id）在n（cpu node）的device id集合，进行归类，而且一个device id只匹配一个node（不继续遍历nodes，这样可以避免分配device id是一个device id关联多个node）
			if perNodeDevices[n].Has(d) {
				if n == nodeWithoutTopology {
					withoutTopology = append(withoutTopology, d)
				// node n在affinity中
				} else if affinity.IsSet(n) {
					fromAffinity = append(fromAffinity, d)
				} else {
					notFromAffinity = append(notFromAffinity, d)
				}
				break
			}
		}
	}

	// Concatenate the lists above return the first 'request' devices from it..
	// 先分配affinity匹配的，再分配affinity不匹配，再分配没有关联任何numa node
	return append(append(fromAffinity, notFromAffinity...), withoutTopology...)[:request]
}

// allocateContainerResources attempts to allocate all of required device
// plugin resources for the input container, issues an Allocate rpc request
// for each new device resource requirement, processes their AllocateResponses,
// and updates the cached containerDevices on success.
// 分配container请求的extend资源
// 先在manager内部进行分配resource的device id，然后通过grpc调用device plugin进行分配device id
func (m *ManagerImpl) allocateContainerResources(pod *v1.Pod, container *v1.Container, devicesToReuse map[string]sets.String) error {
	podUID := string(pod.UID)
	contName := container.Name
	allocatedDevicesUpdated := false
	// Extended resources are not allowed to be overcommitted.
	// Since device plugin advertises extended resources,
	// therefore Requests must be equal to Limits and iterating
	// over the Limits should be sufficient.
	for k, v := range container.Resources.Limits {
		resource := string(k)
		needed := int(v.Value())
		klog.V(3).Infof("needs %d %s", needed, resource)
		// container请求的resource不在m.healthyDevices（registered healthy devices）里，也不在m.allocatedDevices（allocated devices）里
		if !m.isDevicePluginResource(resource) {
			continue
		}
		// Updates allocatedDevices to garbage collect any stranded resources
		// before doing the device plugin allocation.
		// 第一次循环，先更新m.podDevices和m.allocatedDevices
		if !allocatedDevicesUpdated {
			// 移除m.podDevices中已经terminated的pod，更新m.podDevices（pod与pod各个container分配的资源）和m.allocatedDevices（已经分配资源和device id）
			m.UpdateAllocatedDevices()
			allocatedDevicesUpdated = true
		}
		// 返回分配的device id集合，这个是加锁操作
		allocDevices, err := m.devicesToAllocate(podUID, contName, resource, needed, devicesToReuse[resource])
		// 分配过程发生错误，比如资源不足，或已经分配过了但是container修改需要的resource
		if err != nil {
			return err
		}
		// 不需要分配资源，则继续下一个资源
		if allocDevices == nil || len(allocDevices) <= 0 {
			continue
		}

		startRPCTime := time.Now()
		// Manager.Allocate involves RPC calls to device plugin, which
		// could be heavy-weight. Therefore we want to perform this operation outside
		// mutex lock. Note if Allocate call fails, we may leave container resources
		// partially allocated for the failed container. We rely on UpdateAllocatedDevices()
		// to garbage collect these resources later. Another side effect is that if
		// we have X resource A and Y resource B in total, and two containers, container1
		// and container2 both require X resource A and Y resource B. Both allocation
		// requests may fail if we serve them in mixed order.
		// TODO: may revisit this part later if we see inefficient resource allocation
		// in real use as the result of this. Should also consider to parallelize device
		// plugin Allocate grpc calls if it becomes common that a container may require
		// resources from multiple device plugins.
		m.mutex.Lock()
		eI, ok := m.endpoints[resource]
		m.mutex.Unlock()
		// resource不在m.endpoints，更新m.allocatedDevices为m.podDevices.devices()（所有pod里每个container已分配的资源），重置m.allocatedDevices为未分配资源前的状态，然后直接返回错误
		if !ok {
			m.mutex.Lock()
			m.allocatedDevices = m.podDevices.devices()
			m.mutex.Unlock()
			return fmt.Errorf("unknown Device Plugin %s", resource)
		}

		devs := allocDevices.UnsortedList()
		// TODO: refactor this part of code to just append a ContainerAllocationRequest
		// in a passed in AllocateRequest pointer, and issues a single Allocate call per pod.
		klog.V(3).Infof("Making allocation request for devices %v for device plugin %s", devs, resource)
		// 调用grpc客户端，让device plugin分配device id
		resp, err := eI.e.allocate(devs)
		metrics.DevicePluginAllocationDuration.WithLabelValues(resource).Observe(metrics.SinceInSeconds(startRPCTime))
		if err != nil {
			// In case of allocation failure, we want to restore m.allocatedDevices
			// to the actual allocated state from m.podDevices.
			// 调用device plugin分配device id失败，则更新m.allocatedDevices为m.podDevices.devices()（所有pod里每个container已分配的资源），重置m.allocatedDevices为未分配资源前的状态，然后直接返回错误
			m.mutex.Lock()
			m.allocatedDevices = m.podDevices.devices()
			m.mutex.Unlock()
			return err
		}

		// device plugin返回分配结果为空，返回错误
		if len(resp.ContainerResponses) == 0 {
			return fmt.Errorf("no containers return in allocation response %v", resp)
		}

		// Update internal cached podDevices state.
		// 更新m.podDevices
		m.mutex.Lock()
		m.podDevices.insert(podUID, contName, resource, allocDevices, resp.ContainerResponses[0])
		m.mutex.Unlock()
	}

	// Checkpoints device to container allocation information.
	// 将数据（resource分配状态和resource资源）写入到"/var/lib/kubelet/device-plugins/kubelet_internal_checkpoint"文件
	return m.writeCheckpoint()
}

// GetDeviceRunContainerOptions checks whether we have cached containerDevices
// for the passed-in <pod, container> and returns its DeviceRunContainerOptions
// for the found one. An empty struct is returned in case no cached state is found.
// 从pod分配记录中（m.podDevices）判断容器是否已经分配过需要的资源，没有就进行分配
// 返回分配过或新分配资源转成container runtime运行参数（*DeviceRunContainerOptions）
func (m *ManagerImpl) GetDeviceRunContainerOptions(pod *v1.Pod, container *v1.Container) (*DeviceRunContainerOptions, error) {
	podUID := string(pod.UID)
	contName := container.Name
	needsReAllocate := false
	for k := range container.Resources.Limits {
		resource := string(k)
		// container请求的resource不在m.healthyDevices（registered healthy devices）里，也不在m.allocatedDevices（allocated devices）里
		if !m.isDevicePluginResource(resource) {
			continue
		}
		// 根据m.endpoints里*pluginapi.DevicePluginOptions判断是否需要执行PreStartContainer，如果要执行，则通过grpc调用让resource的device plugin执行PreStartContainer
		err := m.callPreStartContainerIfNeeded(podUID, contName, resource)
		if err != nil {
			return nil, err
		}
		// This is a device plugin resource yet we don't have cached
		// resource state. This is likely due to a race during node
		// restart. We re-issue allocate request to cover this race.
		// 比如容器重启了，之前已经分配了resource给container，在m.podDevices里会有记录
		// resource未分配给container，则设置needsReAllocate为true
		if m.podDevices.containerDevices(podUID, contName, resource) == nil {
			needsReAllocate = true
		}
	}
	// 
	if needsReAllocate {
		klog.V(2).Infof("needs re-allocate device plugin resources for pod %s, container %s", podUID, container.Name)
		// 从注册的device plugins，分配container的limit中所有需要的资源
		if err := m.Allocate(pod, container); err != nil {
			return nil, err
		}
	}
	m.mutex.Lock()
	defer m.mutex.Unlock()
	// 遍历所有device plugin返回给pod里container的resource的分配响应（allocResp *pluginapi.ContainerAllocateResponse）转成容器runtime运行参数（*DeviceRunContainerOptions）
	return m.podDevices.deviceRunContainerOptions(string(pod.UID), container.Name), nil
}

// callPreStartContainerIfNeeded issues PreStartContainer grpc call for device plugin resource
// with PreStartRequired option set.
// 根据m.endpoints里*pluginapi.DevicePluginOptions判断是否需要执行PreStartContainer，如果要执行，则通过grpc调用让resource的device plugin执行PreStartContainer
func (m *ManagerImpl) callPreStartContainerIfNeeded(podUID, contName, resource string) error {
	m.mutex.Lock()
	// 是否在m.endpoints里
	eI, ok := m.endpoints[resource]
	if !ok {
		m.mutex.Unlock()
		return fmt.Errorf("endpoint not found in cache for a registered resource: %s", resource)
	}

	if eI.opts == nil || !eI.opts.PreStartRequired {
		m.mutex.Unlock()
		klog.V(4).Infof("Plugin options indicate to skip PreStartContainer for resource: %s", resource)
		return nil
	}

	// 返回已经分配给container的resource的所有device id
	devices := m.podDevices.containerDevices(podUID, contName, resource)
	if devices == nil {
		m.mutex.Unlock()
		return fmt.Errorf("no devices found allocated in local cache for pod %s, container %s, resource %s", podUID, contName, resource)
	}

	m.mutex.Unlock()
	devs := devices.UnsortedList()
	klog.V(4).Infof("Issuing an PreStartContainer call for container, %s, of pod %s", contName, podUID)
	// 让device plugin执行PreStartContainer
	_, err := eI.e.preStartContainer(devs)
	if err != nil {
		return fmt.Errorf("device plugin PreStartContainer rpc failed with err: %v", err)
	}
	// TODO: Add metrics support for init RPC
	return nil
}

// sanitizeNodeAllocatable scans through allocatedDevices in the device manager
// and if necessary, updates allocatableResource in nodeInfo to at least equal to
// the allocated capacity. This allows pods that have already been scheduled on
// the node to pass GeneralPredicates admission checking even upon device plugin failure.
// 让所有已经分配的device resource，在node的allocatableResource里的resource数量等于已经分配的数量
func (m *ManagerImpl) sanitizeNodeAllocatable(node *schedulernodeinfo.NodeInfo) {
	var newAllocatableResource *schedulernodeinfo.Resource
	// node可分配的资源列表
	allocatableResource := node.AllocatableResource()
	if allocatableResource.ScalarResources == nil {
		allocatableResource.ScalarResources = make(map[v1.ResourceName]int64)
	}
	// 遍历已经分配的device
	for resource, devices := range m.allocatedDevices {
		needed := devices.Len()
		// node可分配的resource资源数量大于等于已分配数量，则跳过
		quant, ok := allocatableResource.ScalarResources[v1.ResourceName(resource)]
		if ok && int(quant) >= needed {
			continue
		}

		// node可分配的resource资源数量小于已分配数量

		// Needs to update nodeInfo.AllocatableResource to make sure
		// NodeInfo.allocatableResource at least equal to the capacity already allocated.
		if newAllocatableResource == nil {
			newAllocatableResource = allocatableResource.Clone()
		}
		// 让node的allocatableResource里的resource数量等于需要的数量
		newAllocatableResource.ScalarResources[v1.ResourceName(resource)] = int64(needed)
	}
	if newAllocatableResource != nil {
		// 更新node.allocatableResource为allocatableResource，并让node.generation加一
		node.SetAllocatableResource(newAllocatableResource)
	}
}

// 返回resource是否在m.healthyDevices（registered healthy devices）里或m.allocatedDevices（allocated devices）里
func (m *ManagerImpl) isDevicePluginResource(resource string) bool {
	// resource是否在registered healthy devices里
	_, registeredResource := m.healthyDevices[resource]
	// resource是否在allocated devices里
	_, allocatedResource := m.allocatedDevices[resource]
	// Return true if this is either an active device plugin resource or
	// a resource we have previously allocated.
	if registeredResource || allocatedResource {
		return true
	}
	return false
}

// GetDevices returns the devices used by the specified container
func (m *ManagerImpl) GetDevices(podUID, containerName string) []*podresourcesapi.ContainerDevices {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.podDevices.getContainerDevices(podUID, containerName)
}

// ShouldResetExtendedResourceCapacity returns whether the extended resources should be zeroed or not,
// depending on whether the node has been recreated. Absence of the checkpoint file strongly indicates the node
// has been recreated.
func (m *ManagerImpl) ShouldResetExtendedResourceCapacity() bool {
	if utilfeature.DefaultFeatureGate.Enabled(features.DevicePlugins) {
		checkpoints, err := m.checkpointManager.ListCheckpoints()
		if err != nil {
			return false
		}
		return len(checkpoints) == 0
	}
	return false
}
