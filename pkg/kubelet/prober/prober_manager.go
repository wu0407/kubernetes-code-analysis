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

package prober

import (
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

// ProberResults stores the cumulative number of a probe by result as prometheus metrics.
var ProberResults = metrics.NewCounterVec(
	&metrics.CounterOpts{
		Subsystem:      "prober",
		Name:           "probe_total",
		Help:           "Cumulative number of a liveness, readiness or startup probe for a container by result.",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"probe_type",
		"result",
		"container",
		"pod",
		"namespace",
		"pod_uid"},
)

// Manager manages pod probing. It creates a probe "worker" for every container that specifies a
// probe (AddPod). The worker periodically probes its assigned container and caches the results. The
// manager use the cached probe results to set the appropriate Ready state in the PodStatus when
// requested (UpdatePodStatus). Updating probe parameters is not currently supported.
// TODO: Move liveness probing out of the runtime, to here.
type Manager interface {
	// AddPod creates new probe workers for every container probe. This should be called for every
	// pod created.
	AddPod(pod *v1.Pod)

	// RemovePod handles cleaning up the removed pod state, including terminating probe workers and
	// deleting cached results.
	RemovePod(pod *v1.Pod)

	// CleanupPods handles cleaning up pods which should no longer be running.
	// It takes a map of "desired pods" which should not be cleaned up.
	CleanupPods(desiredPods map[types.UID]sets.Empty)

	// UpdatePodStatus modifies the given PodStatus with the appropriate Ready state for each
	// container based on container running status, cached probe results and worker states.
	UpdatePodStatus(types.UID, *v1.PodStatus)

	// Start starts the Manager sync loops.
	Start()
}

type manager struct {
	// Map of active workers for probes
	workers map[probeKey]*worker
	// Lock for accessing & mutating workers
	workerLock sync.RWMutex

	// The statusManager cache provides pod IP and container IDs for probing.
	statusManager status.Manager

	// readinessManager manages the results of readiness probes
	readinessManager results.Manager

	// livenessManager manages the results of liveness probes
	livenessManager results.Manager

	// startupManager manages the results of startup probes
	startupManager results.Manager

	// prober executes the probe actions.
	prober *prober
}

// NewManager creates a Manager for pod probing.
func NewManager(
	statusManager status.Manager,
	livenessManager results.Manager,
	startupManager results.Manager,
	runner kubecontainer.ContainerCommandRunner,
	refManager *kubecontainer.RefManager,
	recorder record.EventRecorder) Manager {

	prober := newProber(runner, refManager, recorder)
	readinessManager := results.NewManager()
	return &manager{
		statusManager:    statusManager,
		prober:           prober,
		readinessManager: readinessManager,
		livenessManager:  livenessManager,
		startupManager:   startupManager,
		workers:          make(map[probeKey]*worker),
	}
}

// Start syncing probe status. This should only be called once.
func (m *manager) Start() {
	// Start syncing readiness.
	// m.readinessManager.updates通道中读取消息
	// 消息中（readiness是否成功结果）跟m.statusManager里的container status里的Ready字段值一样，则不做任何东西
	// 否则，同步readiness结果到m.statusManager.podStatuses，并更新apiserver中pod status
	go wait.Forever(m.updateReadiness, 0)
	// Start syncing startup.
	// 从m.startupManager.updates通道中读取消息
	// 消息中startup probe是否成功结果，跟m.statusManager里的container status里的Started字段值一样，则不做任何东西
	// 否则，更改container status里Started字段的值（同步startup probe结果）到m.statusManager.podStatuses，并更新apiserver中pod status
	go wait.Forever(m.updateStartup, 0)
	// livenessManager.Updates()在kl.syncLoopIteration里被消费
}

// Key uniquely identifying container probes
type probeKey struct {
	podUID        types.UID
	containerName string
	probeType     probeType
}

// Type of probe (liveness, readiness or startup)
type probeType int

const (
	liveness probeType = iota
	readiness
	startup

	probeResultSuccessful string = "successful"
	probeResultFailed     string = "failed"
	probeResultUnknown    string = "unknown"
)

// For debugging.
func (t probeType) String() string {
	switch t {
	case readiness:
		return "Readiness"
	case liveness:
		return "Liveness"
	case startup:
		return "Startup"
	default:
		return "UNKNOWN"
	}
}

// pod里所有有定义的probe的container，每个container添加一个worker到m.workers，
// 并启动一个goroutine，周期性执行probe
// 执行结果有变化，则写入worker.resultsManager.cache中，则发送Update消息到worker.resultsManager.updates通道
// worker.resultsManager，是对应类型probe的manager，即m.readinessManager、m.livenessManager、m.startupManager
func (m *manager) AddPod(pod *v1.Pod) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()

	key := probeKey{podUID: pod.UID}
	// 遍历pod里所有普通container
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name

		// container定义了StartupProbe且启用"StartupProbe"功能
		if c.StartupProbe != nil && utilfeature.DefaultFeatureGate.Enabled(features.StartupProbe) {
			key.probeType = startup
			if _, ok := m.workers[key]; ok {
				klog.Errorf("Startup probe already exists! %v - %v",
					format.Pod(pod), c.Name)
				return
			}
			w := newWorker(m, startup, pod, c)
			// 添加一个worker到m.workers
			m.workers[key] = w
			// 周期性执行probe，直到w.stopCh收到消息或w.doProbe()返回false（停止执行probe）
			// 执行结果有变化，则写入w.resultsManager.cache中，则发送Update消息到w.resultsManager.updates通道
			// w.resultsManager，是m.readinessManager
			go w.run()
		}

		if c.ReadinessProbe != nil {
			key.probeType = readiness
			if _, ok := m.workers[key]; ok {
				klog.Errorf("Readiness probe already exists! %v - %v",
					format.Pod(pod), c.Name)
				return
			}
			w := newWorker(m, readiness, pod, c)
			m.workers[key] = w
			go w.run()
		}

		if c.LivenessProbe != nil {
			key.probeType = liveness
			if _, ok := m.workers[key]; ok {
				klog.Errorf("Liveness probe already exists! %v - %v",
					format.Pod(pod), c.Name)
				return
			}
			w := newWorker(m, liveness, pod, c)
			m.workers[key] = w
			go w.run()
		}
	}
}

// 停止pod所有container（有定义probe）下的probe的worker
func (m *manager) RemovePod(pod *v1.Pod) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	key := probeKey{podUID: pod.UID}
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name
		for _, probeType := range [...]probeType{readiness, liveness, startup} {
			key.probeType = probeType
			if worker, ok := m.workers[key]; ok {
				// 发送信号给w.stopCh，让周期执行的probe停止
				worker.stop()
			}
		}
	}
}

// m.workers中的key的pod uid不在desiredPods中，则发送信号给worker.stopCh，让周期执行的probe停止
func (m *manager) CleanupPods(desiredPods map[types.UID]sets.Empty) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	for key, worker := range m.workers {
		if _, ok := desiredPods[key.podUID]; !ok {
			// 发送信号给w.stopCh，让周期执行的probe停止
			worker.stop()
		}
	}
}

// 根据probe状态，设置podStatus里的containerstatus的started和ready字段，和根据init container处于Terminated状态且exitcode为0 设置initContainerstatus里的ready字段
func (m *manager) UpdatePodStatus(podUID types.UID, podStatus *v1.PodStatus) {
	for i, c := range podStatus.ContainerStatuses {
		var started bool
		if c.State.Running == nil {
			started = false
		} else if !utilfeature.DefaultFeatureGate.Enabled(features.StartupProbe) {
			// the container is running, assume it is started if the StartupProbe feature is disabled
			started = true
		// 从startupManager的缓存中获取container相关结果
		} else if result, ok := m.startupManager.Get(kubecontainer.ParseContainerID(c.ContainerID)); ok {
			// 发现了结果，判断是否成功
			started = result == results.Success
		} else {
			// The check whether there is a probe which hasn't run yet.
			// 是否已经有未运行startup探测worker
			_, exists := m.getWorker(podUID, c.Name, startup)
			started = !exists
		}
		podStatus.ContainerStatuses[i].Started = &started

		// 已经启动，再判断是否ready
		if started {
			var ready bool
			if c.State.Running == nil {
				ready = false
			// 从readinessManager的缓存中获取container相关结果
			} else if result, ok := m.readinessManager.Get(kubecontainer.ParseContainerID(c.ContainerID)); ok {
				ready = result == results.Success
			} else {
				// The check whether there is a probe which hasn't run yet.
				// 是否已经有未运行readiness探测的worker
				_, exists := m.getWorker(podUID, c.Name, readiness)
				ready = !exists
			}
			podStatus.ContainerStatuses[i].Ready = ready
		}
	}
	// init containers are ready if they have exited with success or if a readiness probe has
	// succeeded.
	for i, c := range podStatus.InitContainerStatuses {
		var ready bool
		// init container处于Terminated状态且exitcode为0，则为ready
		if c.State.Terminated != nil && c.State.Terminated.ExitCode == 0 {
			ready = true
		}
		podStatus.InitContainerStatuses[i].Ready = ready
	}
}

func (m *manager) getWorker(podUID types.UID, containerName string, probeType probeType) (*worker, bool) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	worker, ok := m.workers[probeKey{podUID, containerName, probeType}]
	return worker, ok
}

// Called by the worker after exiting.
// 从m.workers中移除这个worker
func (m *manager) removeWorker(podUID types.UID, containerName string, probeType probeType) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()
	delete(m.workers, probeKey{podUID, containerName, probeType})
}

// workerCount returns the total number of probe workers. For testing.
func (m *manager) workerCount() int {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	return len(m.workers)
}

func (m *manager) updateReadiness() {
	// m.readinessManager.updates通道中读取消息
	update := <-m.readinessManager.Updates()

	ready := update.Result == results.Success
	// ready（readiness结果）跟m.statusManager里的container status里的Ready字段值一样，则不做任何东西
	// 否则，同步readiness结果到m.statusManager.podStatuses，并更新apiserver中pod status
	m.statusManager.SetContainerReadiness(update.PodUID, update.ContainerID, ready)
}

func (m *manager) updateStartup() {
	// 从m.startupManager.updates通道中读取消息
	update := <-m.startupManager.Updates()

	started := update.Result == results.Success
	// started（startup probe结果）跟m.statusManager里的container status里的Started字段值一样，则不做任何东西
	// 否则，更改container status里Started字段的值（同步startup probe结果）到m.statusManager.podStatuses，并更新apiserver中pod status
	m.statusManager.SetContainerStartup(update.PodUID, update.ContainerID, started)
}
