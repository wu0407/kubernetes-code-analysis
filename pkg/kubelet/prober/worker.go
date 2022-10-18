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
	"math/rand"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/metrics"
	"k8s.io/klog"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
)

// worker handles the periodic probing of its assigned container. Each worker has a go-routine
// associated with it which runs the probe loop until the container permanently terminates, or the
// stop channel is closed. The worker uses the probe Manager's statusManager to get up-to-date
// container IDs.
type worker struct {
	// Channel for stopping the probe.
	stopCh chan struct{}

	// The pod containing this probe (read-only)
	pod *v1.Pod

	// The container to probe (read-only)
	container v1.Container

	// Describes the probe configuration (read-only)
	spec *v1.Probe

	// The type of the worker.
	probeType probeType

	// The probe value during the initial delay.
	initialValue results.Result

	// Where to store this workers results.
	// 可能是readinessManager、livenessManager、startupManager
	resultsManager results.Manager
	probeManager   *manager

	// The last known container ID for this worker.
	containerID kubecontainer.ContainerID
	// The last probe result for this worker.
	lastResult results.Result
	// How many times in a row the probe has returned the same result.
	resultRun int

	// If set, skip probing.
	onHold bool

	// proberResultsMetricLabels holds the labels attached to this worker
	// for the ProberResults metric by result.
	proberResultsSuccessfulMetricLabels metrics.Labels
	proberResultsFailedMetricLabels     metrics.Labels
	proberResultsUnknownMetricLabels    metrics.Labels
}

// Creates and starts a new probe worker.
func newWorker(
	m *manager,
	probeType probeType,
	pod *v1.Pod,
	container v1.Container) *worker {

	w := &worker{
		stopCh:       make(chan struct{}, 1), // Buffer so stop() can be non-blocking.
		pod:          pod,
		container:    container,
		probeType:    probeType,
		probeManager: m,
	}

	switch probeType {
	case readiness:
		w.spec = container.ReadinessProbe
		w.resultsManager = m.readinessManager
		w.initialValue = results.Failure
	case liveness:
		w.spec = container.LivenessProbe
		w.resultsManager = m.livenessManager
		w.initialValue = results.Success
	case startup:
		w.spec = container.StartupProbe
		w.resultsManager = m.startupManager
		w.initialValue = results.Unknown
	}

	basicMetricLabels := metrics.Labels{
		"probe_type": w.probeType.String(),
		"container":  w.container.Name,
		"pod":        w.pod.Name,
		"namespace":  w.pod.Namespace,
		"pod_uid":    string(w.pod.UID),
	}

	w.proberResultsSuccessfulMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsSuccessfulMetricLabels["result"] = probeResultSuccessful

	w.proberResultsFailedMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsFailedMetricLabels["result"] = probeResultFailed

	w.proberResultsUnknownMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsUnknownMetricLabels["result"] = probeResultUnknown

	return w
}

// run periodically probes the container.
// 周期性执行probe，直到w.stopCh收到消息或w.doProbe()返回false（停止执行probe）
// 执行结果有变化，则写入w.resultsManager.cache中，则发送Update消息到w.resultsManager.updates通道
func (w *worker) run() {
	probeTickerPeriod := time.Duration(w.spec.PeriodSeconds) * time.Second

	// If kubelet restarted the probes could be started in rapid succession.
	// Let the worker wait for a random portion of tickerPeriod before probing.
	time.Sleep(time.Duration(rand.Float64() * float64(probeTickerPeriod)))

	probeTicker := time.NewTicker(probeTickerPeriod)

	defer func() {
		// Clean up.
		probeTicker.Stop()
		if !w.containerID.IsEmpty() {
			// 从w.resultsManage.cache中移除这个containerID
			w.resultsManager.Remove(w.containerID)
		}

		// 从w.probeManager.workers中移除这个worker
		w.probeManager.removeWorker(w.pod.UID, w.container.Name, w.probeType)
		// 移除相同label的metrics
		ProberResults.Delete(w.proberResultsSuccessfulMetricLabels)
		ProberResults.Delete(w.proberResultsFailedMetricLabels)
		ProberResults.Delete(w.proberResultsUnknownMetricLabels)
	}()

probeLoop:
	// 执行probe，执行结果有变化，则写入w.resultsManager.cache中，则发送Update消息到w.resultsManager.updates通道
	// 返回下次是否继续probe
	for w.doProbe() {
		// Wait for next probe tick.
		select {
		case <-w.stopCh:
			break probeLoop
		case <-probeTicker.C:
			// continue
		}
	}
}

// stop stops the probe worker. The worker handles cleanup and removes itself from its manager.
// It is safe to call stop multiple times.
// 发送信号给w.stopCh，让周期执行的probe停止
func (w *worker) stop() {
	select {
	case w.stopCh <- struct{}{}:
	default: // Non-blocking.
	}
}

// doProbe probes the container once and records the result.
// Returns whether the worker should continue.
// 执行probe，执行结果有变化，则写入w.resultsManager.cache中，则发送Update消息到w.resultsManager.updates通道。返回下次是否继续probe
func (w *worker) doProbe() (keepGoing bool) {
	defer func() { recover() }() // Actually eat panics (HandleCrash takes care of logging)
	defer runtime.HandleCrash(func(_ interface{}) { keepGoing = true })

	status, ok := w.probeManager.statusManager.GetPodStatus(w.pod.UID)
	// 没有status，则返回true
	if !ok {
		// Either the pod has not been created yet, or it was already deleted.
		klog.V(3).Infof("No status for pod: %v", format.Pod(w.pod))
		return true
	}

	// Worker should terminate if pod is terminated.
	// pod的phase为"Failed"或"Succeeded"，直接返回false，让w.run()直接退出
	if status.Phase == v1.PodFailed || status.Phase == v1.PodSucceeded {
		klog.V(3).Infof("Pod %v %v, exiting probe worker",
			format.Pod(w.pod), status.Phase)
		return false
	}

	// 从status.ContainerStatuses找到w.container.Name的container的container status
	c, ok := podutil.GetContainerStatus(status.ContainerStatuses, w.container.Name)
	// 找不到container status，或ContainerID为空，直接返回true
	if !ok || len(c.ContainerID) == 0 {
		// Either the container has not been created yet, or it was deleted.
		klog.V(3).Infof("Probe target container not found: %v - %v",
			format.Pod(w.pod), w.container.Name)
		return true // Wait for more information.
	}

	// worker里的container id跟container status里的container id不一样，说明container发生了重启或container新生成
	// 更新w.containerID
	if w.containerID.String() != c.ContainerID {
		// worker里的container id不为空
		if !w.containerID.IsEmpty() {
			// 从w.resultsManager.cache中移除这个w.containerID
			w.resultsManager.Remove(w.containerID)
		}
		// w.containerID更新为container status里的container id
		w.containerID = kubecontainer.ParseContainerID(c.ContainerID)
		// w.containerID在w.resultsManager.cache中的result，发生了变化
		// 则发送Update消息到w.resultsManager.updates通道，w.probeManager消费这个通道消息
		w.resultsManager.Set(w.containerID, w.initialValue, w.pod)
		// We've got a new container; resume probing.
		w.onHold = false
	}

	// 没有新的container生成，或之前liveness probe或startup probe结果失败，返回true
	if w.onHold {
		// Worker is on hold until there is a new container.
		return true
	}

	// container没有启动时间，说明container还未启动
	if c.State.Running == nil {
		klog.V(3).Infof("Non-running container probed: %v - %v",
			format.Pod(w.pod), w.container.Name)
		// worker里的container id不为空
		if !w.containerID.IsEmpty() {
			// w.containerID在w.resultsManager.cache中的result，发生了变化（在w.resultsManager.cache中的result不是results.Failure或之前是空的）
			// 则发送Update消息到w.resultsManager.updates通道，w.probeManager消费这个通道消息
			w.resultsManager.Set(w.containerID, results.Failure, w.pod)
		}
		// Abort if the container will not be restarted.
		// 如果container的status不为Terminated，或重启策略不为"Never"，返回true
		// 其他情况，返回false
		return c.State.Terminated == nil ||
			w.pod.Spec.RestartPolicy != v1.RestartPolicyNever
	}

	// Probe disabled for InitialDelaySeconds.
	// 容器启动时间在w.spec.InitialDelaySeconds内，返回true
	if int32(time.Since(c.State.Running.StartedAt.Time).Seconds()) < w.spec.InitialDelaySeconds {
		return true
	}

	// container没有定义startup probe，或container定义startup probe且之前startup probe通过
	if c.Started != nil && *c.Started {
		// Stop probing for startup once container has started.
		// 这个为startup probe，则返回true（因为新的container启动，即重启pod，不会重新进行startup probe）
		if w.probeType == startup {
			return true
		}
	} else {
		// 定义startup probe且之前startup probe没通过

		// Disable other probes until container has started.
		// 这个不为startup probe，则返回true，即其他probe都在startup probe通过之后执行
		if w.probeType != startup {
			return true
		}
	}

	// TODO: in order for exec probes to correctly handle downward API env, we must be able to reconstruct
	// the full container environment here, OR we must make a call to the CRI in order to get those environment
	// values from the running container.
	// 执行probe
	result, err := w.probeManager.prober.probe(w.probeType, w.pod, status, w.container, w.containerID)
	if err != nil {
		// Prober error, throw away the result.
		return true
	}

	switch result {
	case results.Success:
		ProberResults.With(w.proberResultsSuccessfulMetricLabels).Inc()
	case results.Failure:
		ProberResults.With(w.proberResultsFailedMetricLabels).Inc()
	default:
		ProberResults.With(w.proberResultsUnknownMetricLabels).Inc()
	}

	// 如果probe result没有发生变化，则w.resultRun累加
	if w.lastResult == result {
		w.resultRun++
	} else {
		// 否则重置w.lastResult为result，w.resultRun为1
		w.lastResult = result
		w.resultRun = 1
	}

	// probe失败且probe次数小于FailureThreshold，或probe成功且小于SuccessThreshold，则返回true（让worker继续probe）
	if (result == results.Failure && w.resultRun < int(w.spec.FailureThreshold)) ||
		(result == results.Success && w.resultRun < int(w.spec.SuccessThreshold)) {
		// Success or failure is below threshold - leave the probe state unchanged.
		return true
	}

	// w.containerID在w.resultsManager.cache中的result与现在result不一样，或w.containerID在w.resultsManager.cache中的没有result，则发送Update消息到w.resultsManager.updates通道
	w.resultsManager.Set(w.containerID, result, w.pod)

	// liveness probe或startup probe结果失败，则w.onHold为true，则不让worker继续probe，重置w.resultRun为0
	if (w.probeType == liveness || w.probeType == startup) && result == results.Failure {
		// The container fails a liveness/startup check, it will need to be restarted.
		// Stop probing until we see a new container ID. This is to reduce the
		// chance of hitting #21751, where running `docker exec` when a
		// container is being stopped may lead to corrupted container state.
		w.onHold = true
		w.resultRun = 0
	}

	return true
}

func deepCopyPrometheusLabels(m metrics.Labels) metrics.Labels {
	ret := make(metrics.Labels, len(m))
	for k, v := range m {
		ret[k] = v
	}
	return ret
}
