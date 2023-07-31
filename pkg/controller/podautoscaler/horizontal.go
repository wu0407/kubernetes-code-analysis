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

package podautoscaler

import (
	"context"
	"fmt"
	"math"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v2"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	autoscalingclient "k8s.io/client-go/kubernetes/typed/autoscaling/v2"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v2"
	corelisters "k8s.io/client-go/listers/core/v1"
	scaleclient "k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller"
	metricsclient "k8s.io/kubernetes/pkg/controller/podautoscaler/metrics"
)

var (
	scaleUpLimitFactor  = 2.0
	scaleUpLimitMinimum = 4.0
)

type timestampedRecommendation struct {
	recommendation int32
	timestamp      time.Time
}

type timestampedScaleEvent struct {
	// 一定是大于等于0，注释不是实现
	replicaChange int32 // positive for scaleUp, negative for scaleDown
	timestamp     time.Time
	outdated      bool
}

// HorizontalController is responsible for the synchronizing HPA objects stored
// in the system with the actual deployments/replication controllers they
// control.
type HorizontalController struct {
	scaleNamespacer scaleclient.ScalesGetter
	hpaNamespacer   autoscalingclient.HorizontalPodAutoscalersGetter
	mapper          apimeta.RESTMapper

	replicaCalc   *ReplicaCalculator
	eventRecorder record.EventRecorder

	downscaleStabilisationWindow time.Duration

	// hpaLister is able to list/get HPAs from the shared cache from the informer passed in to
	// NewHorizontalController.
	hpaLister       autoscalinglisters.HorizontalPodAutoscalerLister
	hpaListerSynced cache.InformerSynced

	// podLister is able to list/get Pods from the shared cache from the informer passed in to
	// NewHorizontalController.
	podLister       corelisters.PodLister
	podListerSynced cache.InformerSynced

	// Controllers that need to be synced
	queue workqueue.RateLimitingInterface

	// Latest unstabilized recommendations for each autoscaler.
	recommendations map[string][]timestampedRecommendation

	// Latest autoscaler events
	scaleUpEvents   map[string][]timestampedScaleEvent
	scaleDownEvents map[string][]timestampedScaleEvent
}

// NewHorizontalController creates a new HorizontalController.
func NewHorizontalController(
	evtNamespacer v1core.EventsGetter,
	scaleNamespacer scaleclient.ScalesGetter,
	hpaNamespacer autoscalingclient.HorizontalPodAutoscalersGetter,
	mapper apimeta.RESTMapper,
	metricsClient metricsclient.MetricsClient,
	hpaInformer autoscalinginformers.HorizontalPodAutoscalerInformer,
	podInformer coreinformers.PodInformer,
	resyncPeriod time.Duration,
	downscaleStabilisationWindow time.Duration,
	tolerance float64,
	cpuInitializationPeriod,
	delayOfInitialReadinessStatus time.Duration,

) *HorizontalController {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: evtNamespacer.Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "horizontal-pod-autoscaler"})

	hpaController := &HorizontalController{
		eventRecorder:                recorder,
		scaleNamespacer:              scaleNamespacer,
		hpaNamespacer:                hpaNamespacer,
		downscaleStabilisationWindow: downscaleStabilisationWindow,
		// 默认15秒处理一个
		queue:                        workqueue.NewNamedRateLimitingQueue(NewDefaultHPARateLimiter(resyncPeriod), "horizontalpodautoscaler"),
		mapper:                       mapper,
		recommendations:              map[string][]timestampedRecommendation{},
		scaleUpEvents:                map[string][]timestampedScaleEvent{},
		scaleDownEvents:              map[string][]timestampedScaleEvent{},
	}

	hpaInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    hpaController.enqueueHPA,
			UpdateFunc: hpaController.updateHPA,
			DeleteFunc: hpaController.deleteHPA,
		},
		resyncPeriod,
	)
	hpaController.hpaLister = hpaInformer.Lister()
	hpaController.hpaListerSynced = hpaInformer.Informer().HasSynced

	hpaController.podLister = podInformer.Lister()
	hpaController.podListerSynced = podInformer.Informer().HasSynced

	replicaCalc := NewReplicaCalculator(
		metricsClient,
		hpaController.podLister,
		// 默认为0.1
		tolerance,
		// 默认为5分钟
		cpuInitializationPeriod,
		// 默认为30s
		delayOfInitialReadinessStatus,
	)
	hpaController.replicaCalc = replicaCalc

	return hpaController
}

// Run begins watching and syncing.
func (a *HorizontalController) Run(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer a.queue.ShutDown()

	klog.Infof("Starting HPA controller")
	defer klog.Infof("Shutting down HPA controller")

	if !cache.WaitForNamedCacheSync("HPA", ctx.Done(), a.hpaListerSynced, a.podListerSynced) {
		return
	}

	// start a single worker (we may wish to start more in the future)
	go wait.UntilWithContext(ctx, a.worker, time.Second)

	<-ctx.Done()
}

// obj could be an *v1.HorizontalPodAutoscaler, or a DeletionFinalStateUnknown marker item.
func (a *HorizontalController) updateHPA(old, cur interface{}) {
	a.enqueueHPA(cur)
}

// obj could be an *v1.HorizontalPodAutoscaler, or a DeletionFinalStateUnknown marker item.
func (a *HorizontalController) enqueueHPA(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	// Requests are always added to queue with resyncPeriod delay.  If there's already
	// request for the HPA in the queue then a new request is always dropped. Requests spend resync
	// interval in queue so HPAs are processed every resync interval.
	a.queue.AddRateLimited(key)
}

func (a *HorizontalController) deleteHPA(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %+v: %v", obj, err))
		return
	}

	// TODO: could we leak if we fail to get the key?
	a.queue.Forget(key)
}

func (a *HorizontalController) worker(ctx context.Context) {
	for a.processNextWorkItem(ctx) {
	}
	klog.Infof("horizontal pod autoscaler controller worker shutting down")
}

func (a *HorizontalController) processNextWorkItem(ctx context.Context) bool {
	key, quit := a.queue.Get()
	if quit {
		return false
	}
	defer a.queue.Done(key)

	deleted, err := a.reconcileKey(ctx, key.(string))
	if err != nil {
		utilruntime.HandleError(err)
	}
	// Add request processing HPA to queue with resyncPeriod delay.
	// Requests are always added to queue with resyncPeriod delay. If there's already request
	// for the HPA in the queue then a new request is always dropped. Requests spend resyncPeriod
	// in queue so HPAs are processed every resyncPeriod.
	// Request is added here just in case last resync didn't insert request into the queue. This
	// happens quite often because there is race condition between adding request after resyncPeriod
	// and removing them from queue. Request can be added by resync before previous request is
	// removed from queue. If we didn't add request here then in this case one request would be dropped
	// and HPA would processed after 2 x resyncPeriod.
	if !deleted {
		a.queue.AddRateLimited(key)
	}

	return true
}

// computeReplicasForMetrics computes the desired number of replicas for the metric specifications listed in the HPA,
// returning the maximum  of the computed replica counts, a description of the associated metric, and the statuses of
// all metrics computed.
// 遍历所有的metricSpecs，对metricSpec进行计算副本数。取最大的副本数
// 所有metricSpec执行评估都报错，或有一个metricSpec执行评估报错且需要进行缩容，则不扩缩容，返回错误
// 返回最终副本数，metric名字、所有metricSpecs计算出的statuses，metric的timestamp
func (a *HorizontalController) computeReplicasForMetrics(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, scale *autoscalingv1.Scale,
	metricSpecs []autoscalingv2.MetricSpec) (replicas int32, metric string, statuses []autoscalingv2.MetricStatus, timestamp time.Time, err error) {

	if scale.Status.Selector == "" {
		errMsg := "selector is required"
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "SelectorRequired", errMsg)
		setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionFalse, "InvalidSelector", "the HPA target's scale is missing a selector")
		return 0, "", nil, time.Time{}, fmt.Errorf(errMsg)
	}

	selector, err := labels.Parse(scale.Status.Selector)
	if err != nil {
		errMsg := fmt.Sprintf("couldn't convert selector into a corresponding internal selector object: %v", err)
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "InvalidSelector", errMsg)
		setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionFalse, "InvalidSelector", errMsg)
		return 0, "", nil, time.Time{}, fmt.Errorf(errMsg)
	}

	specReplicas := scale.Spec.Replicas
	statusReplicas := scale.Status.Replicas
	statuses = make([]autoscalingv2.MetricStatus, len(metricSpecs))

	invalidMetricsCount := 0
	var invalidMetricError error
	var invalidMetricCondition autoscalingv2.HorizontalPodAutoscalerCondition

	for i, metricSpec := range metricSpecs {
		// 最终副本数，metric名字组合，metric时间，（如果成功，就是空condition，否则不为空）
		replicaCountProposal, metricNameProposal, timestampProposal, condition, err := a.computeReplicasForMetric(ctx, hpa, metricSpec, specReplicas, statusReplicas, selector, &statuses[i])

		if err != nil {
			if invalidMetricsCount <= 0 {
				invalidMetricCondition = condition
				invalidMetricError = err
			}
			invalidMetricsCount++
		}
		// 取最大的最终副本数
		if err == nil && (replicas == 0 || replicaCountProposal > replicas) {
			timestamp = timestampProposal
			replicas = replicaCountProposal
			metric = metricNameProposal
		}
	}

	// If all metrics are invalid or some are invalid and we would scale down,
	// return an error and set the condition of the hpa based on the first invalid metric.
	// Otherwise set the condition as scaling active as we're going to scale
	// 所有metricSpec执行评估都报错，或有一个metricSpec执行评估报错且需要进行缩容，则不扩缩容，返回错误
	if invalidMetricsCount >= len(metricSpecs) || (invalidMetricsCount > 0 && replicas < specReplicas) {
		setCondition(hpa, invalidMetricCondition.Type, invalidMetricCondition.Status, invalidMetricCondition.Reason, invalidMetricCondition.Message)
		return 0, "", statuses, time.Time{}, fmt.Errorf("invalid metrics (%v invalid out of %v), first error is: %v", invalidMetricsCount, len(metricSpecs), invalidMetricError)
	}
	setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionTrue, "ValidMetricFound", "the HPA was able to successfully calculate a replica count from %s", metric)
	return replicas, metric, statuses, timestamp, nil
}

// Computes the desired number of replicas for a specific hpa and metric specification,
// returning the metric status and a proposed condition to be set on the HPA object.
func (a *HorizontalController) computeReplicasForMetric(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, spec autoscalingv2.MetricSpec,
	specReplicas, statusReplicas int32, selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, metricNameProposal string,
	timestampProposal time.Time, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {

	switch spec.Type {
	case autoscalingv2.ObjectMetricSourceType:
		metricSelector, err := metav1.LabelSelectorAsSelector(spec.Object.Metric.Selector)
		if err != nil {
			// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get object metric value: %v", err)
		}
		// 这里的objectRef为metricSpec.Object.DescribedObject
		// 1.
		// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
		// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
		// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
		// 如果Target.Type为"Value"
		//   usageRatio为当前值/目标值
		//   usageRatio在[usageRatio-c.tolerance, usageRatio+c.tolerance]范围内，就不进行扩缩容，返回现在的副本数
		//   否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
		//   当前的副本数为0，则最终副本数为usageRatio向上取整
		//   返回最终的副本数，当前metrics值，空的timestamp，错误
		// 如果Target.Type为"AverageValue"
		//   usageRatio是聚合值（所有pod的总值）/(目标平均利用率*副本数)
		//   如果usageRatio不在[usageRatio-c.tolerance, usageRatio+c.tolerance]范围内，则最终副本数为副本数为总使用的值/目标平均利用率，然后向上取整。否则，为现在副本数（不进行扩缩容）
		//   现在的平均利用率是总使用的值/当前副本数
		//   返回最终副本数、现在的平均利用率、metric的时间戳
		// 否则，返回错误
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForObjectMetric(specReplicas, statusReplicas, spec, hpa, selector, status, metricSelector)
		if err != nil {
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get object metric value: %v", err)
		}
	case autoscalingv2.PodsMetricSourceType:
		metricSelector, err := metav1.LabelSelectorAsSelector(spec.Pods.Metric.Selector)
		if err != nil {
			// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetPodsMetric", err)
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get pods metric value: %v", err)
		}
		// 最终副本数，第一个metric的timestamp，第一个metric的timestamp，metric名字（经过拼接），（如果成功，就是空condition，否则不为空）
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForPodsMetric(specReplicas, spec, hpa, selector, status, metricSelector)
		if err != nil {
			return 0, "", time.Time{}, condition, fmt.Errorf("failed to get pods metric value: %v", err)
		}
	case autoscalingv2.ResourceMetricSourceType:
		// 最终副本数，第一个metric的timestamp，metric名字（经过拼接），（如果成功，就是空condition，否则不为空）
		// status为（如果是AverageUtilization，当前的平均使用率（相对request）和当前平均使用值（原始值）。如果是"AverageValue"，平均使用值）
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForResourceMetric(ctx, specReplicas, spec, hpa, selector, status)
		if err != nil {
			return 0, "", time.Time{}, condition, err
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		// 最终副本数，第一个metric的timestamp，metric名字（经过拼接），（如果成功，就是空condition，否则不为空）
		// status为（如果是AverageUtilization，当前的平均使用率（相对request）和当前平均使用值（原始值）。如果是"AverageValue"，平均使用值）
		// 这个跟a.computeStatusForResourceMetric一样，只是metric数据里，只取container相关的数据
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForContainerResourceMetric(ctx, specReplicas, spec, hpa, selector, status)
		if err != nil {
			return 0, "", time.Time{}, condition, err
		}
	case autoscalingv2.ExternalMetricSourceType:
		replicaCountProposal, timestampProposal, metricNameProposal, condition, err = a.computeStatusForExternalMetric(specReplicas, statusReplicas, spec, hpa, selector, status)
		if err != nil {
			return 0, "", time.Time{}, condition, err
		}
	default:
		errMsg := fmt.Sprintf("unknown metric source type %q", string(spec.Type))
		err = fmt.Errorf(errMsg)
		// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
		condition := a.getUnableComputeReplicaCountCondition(hpa, "InvalidMetricSourceType", err)
		return 0, "", time.Time{}, condition, err
	}
	return replicaCountProposal, metricNameProposal, timestampProposal, autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
}

func (a *HorizontalController) reconcileKey(ctx context.Context, key string) (deleted bool, err error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return true, err
	}

	hpa, err := a.hpaLister.HorizontalPodAutoscalers(namespace).Get(name)
	if errors.IsNotFound(err) {
		klog.Infof("Horizontal Pod Autoscaler %s has been deleted in %s", name, namespace)
		delete(a.recommendations, key)
		delete(a.scaleUpEvents, key)
		delete(a.scaleDownEvents, key)
		return true, nil
	}
	if err != nil {
		return false, err
	}

	return false, a.reconcileAutoscaler(ctx, hpa, key)
}

// computeStatusForObjectMetric computes the desired number of replicas for the specified metric of type ObjectMetricSourceType.
// 这里的objectRef为metricSpec.Object.DescribedObject
// 1.
// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
// 如果Target.Type为"Value"
//   usageRatio为当前值/目标值
//   usageRatio在[usageRatio-c.tolerance, usageRatio+c.tolerance]范围内，就不进行扩缩容，返回现在的副本数
//   否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
//   当前的副本数为0，则最终副本数为usageRatio向上取整
//   返回最终的副本数，当前metrics值，空的timestamp，错误
// 如果Target.Type为"AverageValue"
//   usageRatio是聚合值（所有pod的总值）/(目标平均利用率*副本数)
//   如果usageRatio不在[usageRatio-c.tolerance, usageRatio+c.tolerance]范围内，则最终副本数为副本数为总使用的值/目标平均利用率，然后向上取整。否则，为现在副本数（不进行扩缩容）
//   现在的平均利用率是总使用的值/当前副本数
//   返回最终副本数、现在的平均利用率、metric的时间戳
// 否则，返回错误
func (a *HorizontalController) computeStatusForObjectMetric(specReplicas, statusReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler, selector labels.Selector, status *autoscalingv2.MetricStatus, metricSelector labels.Selector) (replicas int32, timestamp time.Time, metricName string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	// Target.Type为"Value"
	if metricSpec.Object.Target.Type == autoscalingv2.ValueMetricType {
		// 这里的objectRef为metricSpec.Object.DescribedObject
		// 1.
		// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
		// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
		// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()
		// 2.
		// usageRatio为当前值/目标值
		// usageRatio在[usageRatio-c.tolerance, usageRatio+c.tolerance]范围内，就不进行扩缩容，返回现在的副本数
		// 否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
		// 当前的副本数为0，则最终副本数为usageRatio向上取整
		// 返回最终的副本数，当前metrics值，空的timestamp，错误
		replicaCountProposal, utilizationProposal, timestampProposal, err := a.replicaCalc.GetObjectMetricReplicas(specReplicas, metricSpec.Object.Target.Value.MilliValue(), metricSpec.Object.Metric.Name, hpa.Namespace, &metricSpec.Object.DescribedObject, selector, metricSelector)
		if err != nil {
			// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
			return 0, timestampProposal, "", condition, err
		}
		*status = autoscalingv2.MetricStatus{
			Type: autoscalingv2.ObjectMetricSourceType,
			Object: &autoscalingv2.ObjectMetricStatus{
				DescribedObject: metricSpec.Object.DescribedObject,
				Metric: autoscalingv2.MetricIdentifier{
					Name:     metricSpec.Object.Metric.Name,
					Selector: metricSpec.Object.Metric.Selector,
				},
				Current: autoscalingv2.MetricValueStatus{
					Value: resource.NewMilliQuantity(utilizationProposal, resource.DecimalSI),
				},
			},
		}
		return replicaCountProposal, timestampProposal, fmt.Sprintf("%s metric %s", metricSpec.Object.DescribedObject.Kind, metricSpec.Object.Metric.Name), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	// Target.Type为"AverageValue"
	} else if metricSpec.Object.Target.Type == autoscalingv2.AverageValueMetricType {
		// 从apiVersion解析出group version, 如果解析成功，则返回group version kind。否则，返回GroupVersionKind{Kind: kind}
		// 当objectRef为autoscaling.CrossVersionObjectReference{Kind:"Namespace", Name: "xxx", APIVersion: "v1"}，则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/metrics/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
		// 否则访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{namespace}/{objectRef对应的resource}/{objectRef.Name}/{metricName}?metricLabelSelector={metricSelector}"，获得v1beta2.MetricValueList[0].Value.MilliValue()和v1beta2.MetricValueList[0].Timestamp.Time
		// usageRatio是聚合值（所有pod的总值）/(目标平均利用率*副本数)
		// 如果usageRatio不在[usageRatio-c.tolerance, usageRatio+c.tolerance]范围内，则最终副本数为副本数为总使用的值/目标平均利用率，然后向上取整。否则，为现在副本数（不进行扩缩容）
		// 现在的平均利用率是总使用的值/当前副本数
		// 返回最终副本数、现在的平均利用率、metric的时间戳
		replicaCountProposal, utilizationProposal, timestampProposal, err := a.replicaCalc.GetObjectPerPodMetricReplicas(statusReplicas, metricSpec.Object.Target.AverageValue.MilliValue(), metricSpec.Object.Metric.Name, hpa.Namespace, &metricSpec.Object.DescribedObject, metricSelector)
		if err != nil {
			// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
			condition := a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
			return 0, time.Time{}, "", condition, fmt.Errorf("failed to get %s object metric: %v", metricSpec.Object.Metric.Name, err)
		}
		*status = autoscalingv2.MetricStatus{
			Type: autoscalingv2.ObjectMetricSourceType,
			Object: &autoscalingv2.ObjectMetricStatus{
				Metric: autoscalingv2.MetricIdentifier{
					Name:     metricSpec.Object.Metric.Name,
					Selector: metricSpec.Object.Metric.Selector,
				},
				Current: autoscalingv2.MetricValueStatus{
					AverageValue: resource.NewMilliQuantity(utilizationProposal, resource.DecimalSI),
				},
			},
		}
		return replicaCountProposal, timestampProposal, fmt.Sprintf("external metric %s(%+v)", metricSpec.Object.Metric.Name, metricSpec.Object.Metric.Selector), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}
	errMsg := "invalid object metric source: neither a value target nor an average value target was set"
	err = fmt.Errorf(errMsg)
	// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
	condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetObjectMetric", err)
	return 0, time.Time{}, "", condition, err
}

// computeStatusForPodsMetric computes the desired number of replicas for the specified metric of type PodsMetricSourceType.
func (a *HorizontalController) computeStatusForPodsMetric(currentReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler, selector labels.Selector, status *autoscalingv2.MetricStatus, metricSelector labels.Selector) (replicaCountProposal int32, timestampProposal time.Time, metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	// 访问"/apis/custom.metrics.k8s.io/v1beta2/namespaces/{m.namespace}/pod/{name}/{metricName}?metricLabelSelector={metricSelector}"，返回v1beta2.MetricValueList[0]，第一个metric的timestamp
	// 进行一系列复杂的数据计算和metric数据修复逻辑，返回最终副本数和平均使用值
	replicaCountProposal, utilizationProposal, timestampProposal, err := a.replicaCalc.GetMetricReplicas(currentReplicas, metricSpec.Pods.Target.AverageValue.MilliValue(), metricSpec.Pods.Metric.Name, hpa.Namespace, selector, metricSelector)
	if err != nil {
		// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
		condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetPodsMetric", err)
		return 0, timestampProposal, "", condition, err
	}
	*status = autoscalingv2.MetricStatus{
		Type: autoscalingv2.PodsMetricSourceType,
		Pods: &autoscalingv2.PodsMetricStatus{
			Metric: autoscalingv2.MetricIdentifier{
				Name:     metricSpec.Pods.Metric.Name,
				Selector: metricSpec.Pods.Metric.Selector,
			},
			Current: autoscalingv2.MetricValueStatus{
				AverageValue: resource.NewMilliQuantity(utilizationProposal, resource.DecimalSI),
			},
		},
	}

	return replicaCountProposal, timestampProposal, fmt.Sprintf("pods metric %s", metricSpec.Pods.Metric.Name), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
}

func (a *HorizontalController) computeStatusForResourceMetricGeneric(ctx context.Context, currentReplicas int32, target autoscalingv2.MetricTarget,
	resourceName v1.ResourceName, namespace string, container string, selector labels.Selector) (replicaCountProposal int32,
	metricStatus *autoscalingv2.MetricValueStatus, timestampProposal time.Time, metricNameProposal string,
	condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	// Target类型为"AverageValue"
	if target.AverageValue != nil {
		var rawProposal int64
		// 根据namespace和selector list所有podMetrics资源
		// 如果container不为空，则从所有metrics.Items里（metricsapi.podMetrics的Containers里）找到resource类型的metric，保存到PodMetricsInfo
		// 如果container为空，则遍历rawMetrics（[]metricsapi.podMetrics）里的所有Containers的resource之和，保存在PodMetricsInfo（pod名称对应的PodMetric）
		// 获得PodMetricsInfo（所有pod metric）和第一个pod metric的Timestamp
		// 进行一系列复杂的数据计算和metric数据修复逻辑，获得最终副本数和平均使用值
		// 返回最终副本数，平均使用值，第一个pod metric的Timestamp
		replicaCountProposal, rawProposal, timestampProposal, err := a.replicaCalc.GetRawResourceReplicas(ctx, currentReplicas, target.AverageValue.MilliValue(), resourceName, namespace, selector, container)
		if err != nil {
			return 0, nil, time.Time{}, "", condition, fmt.Errorf("failed to get %s utilization: %v", resourceName, err)
		}
		metricNameProposal = fmt.Sprintf("%s resource", resourceName.String())
		status := autoscalingv2.MetricValueStatus{
			AverageValue: resource.NewMilliQuantity(rawProposal, resource.DecimalSI),
		}
		return replicaCountProposal, &status, timestampProposal, metricNameProposal, autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}

	// target.AverageUtilization和target.AverageValue都为nil
	if target.AverageUtilization == nil {
		errMsg := "invalid resource metric source: neither a utilization target nor a value target was set"
		return 0, nil, time.Time{}, "", condition, fmt.Errorf(errMsg)
	}

	targetUtilization := *target.AverageUtilization
	// 进行一系列复杂的数据计算和metric数据修复逻辑
	// 当前平均利用率为总的metrics * 100 / 总的request
	// 返回当前平均利用率/目标平均利用率的比值，当前平均利用率（相对总request），总的metric/总的pod数量（原始值），第一个metric的Timestamp
	replicaCountProposal, percentageProposal, rawProposal, timestampProposal, err := a.replicaCalc.GetResourceReplicas(ctx, currentReplicas, targetUtilization, resourceName, namespace, selector, container)
	if err != nil {
		return 0, nil, time.Time{}, "", condition, fmt.Errorf("failed to get %s utilization: %v", resourceName, err)
	}

	metricNameProposal = fmt.Sprintf("%s resource utilization (percentage of request)", resourceName)
	status := autoscalingv2.MetricValueStatus{
		AverageUtilization: &percentageProposal,
		AverageValue:       resource.NewMilliQuantity(rawProposal, resource.DecimalSI),
	}
	return replicaCountProposal, &status, timestampProposal, metricNameProposal, autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
}

// computeStatusForResourceMetric computes the desired number of replicas for the specified metric of type ResourceMetricSourceType.
func (a *HorizontalController) computeStatusForResourceMetric(ctx context.Context, currentReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler,
	selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, timestampProposal time.Time,
	metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	// 最终副本数，（如果是AverageUtilization，当前的平均使用率（相对request）和当前平均使用值（原始值）。如果是"AverageValue"，平均使用值），第一个metric的timestamp，metric名字（经过拼接），（如果成功，就是空condition，否则不为空）
	replicaCountProposal, metricValueStatus, timestampProposal, metricNameProposal, condition, err := a.computeStatusForResourceMetricGeneric(ctx, currentReplicas, metricSpec.Resource.Target, metricSpec.Resource.Name, hpa.Namespace, "", selector)
	if err != nil {
		// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
		condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetResourceMetric", err)
		return replicaCountProposal, timestampProposal, metricNameProposal, condition, err
	}
	*status = autoscalingv2.MetricStatus{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricStatus{
			Name:    metricSpec.Resource.Name,
			Current: *metricValueStatus,
		},
	}
	return replicaCountProposal, timestampProposal, metricNameProposal, condition, nil
}

// computeStatusForContainerResourceMetric computes the desired number of replicas for the specified metric of type ResourceMetricSourceType.
func (a *HorizontalController) computeStatusForContainerResourceMetric(ctx context.Context, currentReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler,
	selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, timestampProposal time.Time,
	metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	// 最终副本数，（如果是AverageUtilization，当前的平均使用率（相对request）和当前平均使用值（原始值）。如果是"AverageValue"，平均使用值），第一个metric的timestamp，metric名字（经过拼接），（如果成功，就是空condition，否则不为空）
	replicaCountProposal, metricValueStatus, timestampProposal, metricNameProposal, condition, err := a.computeStatusForResourceMetricGeneric(ctx, currentReplicas, metricSpec.ContainerResource.Target, metricSpec.ContainerResource.Name, hpa.Namespace, metricSpec.ContainerResource.Container, selector)
	if err != nil {
		// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
		condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetContainerResourceMetric", err)
		return replicaCountProposal, timestampProposal, metricNameProposal, condition, err
	}
	*status = autoscalingv2.MetricStatus{
		Type: autoscalingv2.ContainerResourceMetricSourceType,
		ContainerResource: &autoscalingv2.ContainerResourceMetricStatus{
			Name:      metricSpec.ContainerResource.Name,
			Container: metricSpec.ContainerResource.Container,
			Current:   *metricValueStatus,
		},
	}
	return replicaCountProposal, timestampProposal, metricNameProposal, condition, nil
}

// computeStatusForExternalMetric computes the desired number of replicas for the specified metric of type ExternalMetricSourceType.
func (a *HorizontalController) computeStatusForExternalMetric(specReplicas, statusReplicas int32, metricSpec autoscalingv2.MetricSpec, hpa *autoscalingv2.HorizontalPodAutoscaler, selector labels.Selector, status *autoscalingv2.MetricStatus) (replicaCountProposal int32, timestampProposal time.Time, metricNameProposal string, condition autoscalingv2.HorizontalPodAutoscalerCondition, err error) {
	// AverageValue类型
	if metricSpec.External.Target.AverageValue != nil {
		// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{namespace}/{metricName}?labelSelector={selector}"
		// 获得所有metrics值列表
		// 对所有metrics值列表相加，计算总使用值
		// usageRatio为总使用值/(目标平均使用值*当前pod数量)
		// 当usageRatio不在[1-c.tolerance，1+c.tolerance]范围内，最终副本数为总使用值/目标平均使用值。否则最终副本数为当前副本数（不扩缩容）
		// 平均使用值为总使用值/当前副本数
		// 返回最终副本数，平均使用值，第一个metric的timestamp
		replicaCountProposal, utilizationProposal, timestampProposal, err := a.replicaCalc.GetExternalPerPodMetricReplicas(statusReplicas, metricSpec.External.Target.AverageValue.MilliValue(), metricSpec.External.Metric.Name, hpa.Namespace, metricSpec.External.Metric.Selector)
		if err != nil {
			// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
			condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetExternalMetric", err)
			return 0, time.Time{}, "", condition, fmt.Errorf("failed to get %s external metric: %v", metricSpec.External.Metric.Name, err)
		}
		*status = autoscalingv2.MetricStatus{
			Type: autoscalingv2.ExternalMetricSourceType,
			External: &autoscalingv2.ExternalMetricStatus{
				Metric: autoscalingv2.MetricIdentifier{
					Name:     metricSpec.External.Metric.Name,
					Selector: metricSpec.External.Metric.Selector,
				},
				Current: autoscalingv2.MetricValueStatus{
					AverageValue: resource.NewMilliQuantity(utilizationProposal, resource.DecimalSI),
				},
			},
		}
		return replicaCountProposal, timestampProposal, fmt.Sprintf("external metric %s(%+v)", metricSpec.External.Metric.Name, metricSpec.External.Metric.Selector), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}
	// value类型
	if metricSpec.External.Target.Value != nil {
		// 访问"/apis/external.metrics.k8s.io/v1beta1/namespaces/{hpa.namespace}/{metricSpec.External.Metric.Name}?labelSelector={metricSpec.External.Metric.Selector}"
		// 获得所有metrics值列表
		// 对所有metrics值列表相加，计算总使用值
		// usageRatio为总使用值/目标使用值
		// usageRatio在[1-c.tolerance, 1+c.tolerance]范围内，就不进行扩缩容，最终副本数为现在的副本数
		// 否则，如果当前的副本数不为0，根据namespace和selector，过滤出所有ready的pod，最终副本数为usageRatio*readyPodCount，然后向上取整
		// 当前的副本数为0，则为usageRatio向上取整
		// 返回最终副本数，现在的总使用值，空时间
		replicaCountProposal, utilizationProposal, timestampProposal, err := a.replicaCalc.GetExternalMetricReplicas(specReplicas, metricSpec.External.Target.Value.MilliValue(), metricSpec.External.Metric.Name, hpa.Namespace, metricSpec.External.Metric.Selector, selector)
		if err != nil {
			// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
			condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetExternalMetric", err)
			return 0, time.Time{}, "", condition, fmt.Errorf("failed to get external metric %s: %v", metricSpec.External.Metric.Name, err)
		}
		*status = autoscalingv2.MetricStatus{
			Type: autoscalingv2.ExternalMetricSourceType,
			External: &autoscalingv2.ExternalMetricStatus{
				Metric: autoscalingv2.MetricIdentifier{
					Name:     metricSpec.External.Metric.Name,
					Selector: metricSpec.External.Metric.Selector,
				},
				Current: autoscalingv2.MetricValueStatus{
					Value: resource.NewMilliQuantity(utilizationProposal, resource.DecimalSI),
				},
			},
		}
		return replicaCountProposal, timestampProposal, fmt.Sprintf("external metric %s(%+v)", metricSpec.External.Metric.Name, metricSpec.External.Metric.Selector), autoscalingv2.HorizontalPodAutoscalerCondition{}, nil
	}
	errMsg := "invalid external metric source: neither a value target nor an average value target was set"
	err = fmt.Errorf(errMsg)
	// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
	condition = a.getUnableComputeReplicaCountCondition(hpa, "FailedGetExternalMetric", err)
	return 0, time.Time{}, "", condition, fmt.Errorf(errMsg)
}

// 如果key不在a.recommendations里，则key为key，value为[]timestampedRecommendation{{currentReplicas, time.Now()}}，保存到a.recommendations
// 否则不做任何事情
func (a *HorizontalController) recordInitialRecommendation(currentReplicas int32, key string) {
	if a.recommendations[key] == nil {
		a.recommendations[key] = []timestampedRecommendation{{currentReplicas, time.Now()}}
	}
}

func (a *HorizontalController) reconcileAutoscaler(ctx context.Context, hpaShared *autoscalingv2.HorizontalPodAutoscaler, key string) error {
	// make a copy so that we never mutate the shared informer cache (conversion can mutate the object)
	hpa := hpaShared.DeepCopy()
	hpaStatusOriginal := hpa.Status.DeepCopy()

	reference := fmt.Sprintf("%s/%s/%s", hpa.Spec.ScaleTargetRef.Kind, hpa.Namespace, hpa.Spec.ScaleTargetRef.Name)

	targetGV, err := schema.ParseGroupVersion(hpa.Spec.ScaleTargetRef.APIVersion)
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedGetScale", "the HPA controller was unable to get the target's current scale: %v", err)
		a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa)
		return fmt.Errorf("invalid API version in scale target reference: %v", err)
	}

	targetGK := schema.GroupKind{
		Group: targetGV.Group,
		Kind:  hpa.Spec.ScaleTargetRef.Kind,
	}

	// 直接的实现在staging\src\k8s.io\client-go\restmapper\discovery.go DeferredDiscoveryRESTMapper
	// 然后staging\src\k8s.io\apimachinery\pkg\api\meta\priority.go里的PriorityRESTMapper
	// staging\src\k8s.io\apimachinery\pkg\api\meta\multirestmapper.go里MultiRESTMapper
	// 最终实现在staging\src\k8s.io\apimachinery\pkg\api\meta\restmapper.go里的DefaultRESTMapper
	// 
	// 如果没有提供versions，或者version没有在DefaultRESTMapper.kindToPluralResource里，或version为"__internal"
	// 则使用DefaultRESTMapper.defaultGroupVersions查找group与gk的group一样，将所有version与gk组成group version kind
	// 根据这个group version kind，从DefaultRESTMapper.kindToPluralResource查找GroupVersionResource，从DefaultRESTMapper.kindToScope查找scope。最后组成RESTMapping
	mappings, err := a.mapper.RESTMappings(targetGK)
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedGetScale", "the HPA controller was unable to get the target's current scale: %v", err)
		a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa)
		return fmt.Errorf("unable to determine resource for scale target reference: %v", err)
	}

	// 比如说是deployment的mappings为apimeta.RESTMapping{Resource: {Group: "apps", "Version": "v1", Resource: "deployments"}, GroupVersionKind: {Group: "apps", "Version": "v1", Kind: "Deployment"}, Scope: RESTScopeRoot}
	// 那么就是访问"/apis/apps/v1/namespaces/{namespace}/deployment/{name}/scale"，获得*autoscalingv1.Scale
	scale, targetGR, err := a.scaleForResourceMappings(ctx, hpa.Namespace, hpa.Spec.ScaleTargetRef.Name, mappings)
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedGetScale", err.Error())
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedGetScale", "the HPA controller was unable to get the target's current scale: %v", err)
		a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa)
		return fmt.Errorf("failed to query scale subresource for %s: %v", reference, err)
	}
	setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "SucceededGetScale", "the HPA controller was able to get the target's current scale")
	currentReplicas := scale.Spec.Replicas
	// 如果key不在a.recommendations里，则key为key，value为[]timestampedRecommendation{{currentReplicas, time.Now()}}，保存到a.recommendations
	// 否则不做任何事情
	a.recordInitialRecommendation(currentReplicas, key)

	var (
		metricStatuses        []autoscalingv2.MetricStatus
		metricDesiredReplicas int32
		metricName            string
	)

	desiredReplicas := int32(0)
	rescaleReason := ""

	var minReplicas int32

	if hpa.Spec.MinReplicas != nil {
		minReplicas = *hpa.Spec.MinReplicas
	} else {
		// Default value
		// hpa.Spec.MinReplicas没有设置就是1
		minReplicas = 1
	}

	rescale := true

	// workload的replicas为0，但是hpa的minReplicas不为0，则停止scale
	if scale.Spec.Replicas == 0 && minReplicas != 0 {
		// Autoscaling is disabled for this resource
		desiredReplicas = 0
		rescale = false
		setCondition(hpa, autoscalingv2.ScalingActive, v1.ConditionFalse, "ScalingDisabled", "scaling is disabled since the replica count of the target is zero")
	} else if currentReplicas > hpa.Spec.MaxReplicas {
		rescaleReason = "Current number of replicas above Spec.MaxReplicas"
		// workload的replicas超过hpa.Spec.MaxReplicas，需要scale down到hpa.Spec.MaxReplicas
		desiredReplicas = hpa.Spec.MaxReplicas
	} else if currentReplicas < minReplicas {
		// workload的replicas小于minReplicas，需要scale up到hpa.Spec.MinReplicas
		rescaleReason = "Current number of replicas below Spec.MinReplicas"
		desiredReplicas = minReplicas
	} else {
		// workload的replicas大于等于minReplicas，小于等于hpa.Spec.MaxReplicas
		var metricTimestamp time.Time
		// 遍历所有的metricSpecs，对metricSpec进行计算副本数。取最大的副本数
		// 所有metricSpec执行评估都报错，或有一个metricSpec执行评估报错且需要进行缩容，则不扩缩容，返回错误
		// 返回最终副本数，metric名字、所有metricSpecs计算出的statuses，metric的timestamp（都是最大副本数对应的最终副本数，metrics名字和timestamp）
		metricDesiredReplicas, metricName, metricStatuses, metricTimestamp, err = a.computeReplicasForMetrics(ctx, hpa, scale, hpa.Spec.Metrics)
		if err != nil {
			// 设置hpa.Status里currentReplicas
			a.setCurrentReplicasInStatus(hpa, currentReplicas)
			if err := a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
				utilruntime.HandleError(err)
			}
			a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedComputeMetricsReplicas", err.Error())
			return fmt.Errorf("failed to compute desired number of replicas based on listed metrics for %s: %v", reference, err)
		}

		klog.V(4).Infof("proposing %v desired replicas (based on %s from %s) for %s", metricDesiredReplicas, metricName, metricTimestamp, reference)

		rescaleMetric := ""
		if metricDesiredReplicas > desiredReplicas {
			desiredReplicas = metricDesiredReplicas
			rescaleMetric = metricName
		}
		if desiredReplicas > currentReplicas {
			rescaleReason = fmt.Sprintf("%s above target", rescaleMetric)
		}
		if desiredReplicas < currentReplicas {
			rescaleReason = "All metrics below target"
		}
		if hpa.Spec.Behavior == nil {
			// 查找稳定窗口期里最大的副本数（包括当前的desiredReplicas），同时进行记录当前的desiredReplicas到a.recommendations[key]
			// 稳定窗口期里最大的副本数上下限检查：
			// stabilizedRecommendation为稳定窗口a.recommendations[key]里最大副本数maxRecommendation
			// 最大扩容上限scaleUpLimit为max(2*当前副本数, 4)
			// 扩容上限maximumAllowedReplicas：
			//   hpaMaxReplicas大于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为scaleUpLimit
			//   hpaMaxReplicas小于等于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为hpaMaxReplicas
			// 期望的副本数stabilizedRecommendation小于最小副本数，返回最终副本数为hpa最小副本数minimumAllowedReplicas
			// 期望的副本数stabilizedRecommendation大于maximumAllowedReplicas扩容上限，则返回maximumAllowedReplicas
			// 期望副本数stabilizedRecommendation大于最小副本数，小于等于maximumAllowedReplicas扩容上限，返回desiredReplicas
			desiredReplicas = a.normalizeDesiredReplicas(hpa, key, currentReplicas, desiredReplicas, minReplicas)
		} else {
			// 1. 对hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds设置默认值
			// hpa.Spec.Behavior.ScaleDown不为nil，但是hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为nil
			// 则设置hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为a.downscaleStabilisationWindow
			// 2. 进行稳定周期内的推荐值计算
			// 遍历所有a.recommendations[args.Key]
			// 发现即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内，则替换最后一个（即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内）index，为DesiredReplicas的timestampedRecommendation
			// 否则，没有发现，则将DesiredReplicas的timestampedRecommendation append到a.recommendations[args.Key]
			// 当前副本数CurrentReplicas小于在ScaleUp稳定周期内期望副本的最小值（包括DesiredReplicas）（才会扩容），返回recommendation为ScaleUp稳定周期内期望副本的最小值。
			// 当前副本数CurrentReplicas大于在ScaleDown稳定周期内期望副本的最大值（包括DesiredReplicas）（才会缩容），返回recommendation为ScaleDown稳定周期内期望副本的最大值。
			// 否则，返回当前副本数CurrentReplicas
			// 3.进行Behavior策略限制
			// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
			// 需要扩容（不能扩太多）：
			//   遍历所有scalingRules.Policies
			//     遍历所有a.scaleUpEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
			//     周期开始时候的副本数为当前副本数-replicas变化量
			//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
			//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
			//   根据SelectPolicy为"Min"或"Max"，筛选出scaleUpLimit策略最终副本数限制最大值或最小值
			//   当前副本数已经大于scaleUpLimit策略最终副本数限制，则策略最终副本数限制为当前副本数（这个周期内不能扩容）
			//   maximumAllowedReplicas为min(scaleUpLimit, args.MaxReplicas)
			//   如果期望的副本数大于最大副本数限制maximumAllowedReplicas，返回最大副本数限制maximumAllowedReplicas
			//   否则，期望的副本数小于等于最大副本数限制maximumAllowedReplicas，返回期望的副本数
			// 需要缩容的情况（不能缩太多）
			//   遍历所有scalingRules.Policies
			//     遍历所有a.scaleDownEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
			//     周期开始时候的副本数为当前副本数+replicas变化量
			//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数-policy.Value
			//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1-policy.Value/100)然后向上取整
			//     根据SelectPolicy为"Min"或"Max"，策略最终副本数限制scaleUpLimit最大值或最小值
			//     策略最终副本数限制scaleUpLimit大于当前副本数，不需要进行缩容（已经比要缩的值还小了）
			//   maximumAllowedReplicas为max(scaleUpLimit, args.MinReplicas)
			//   如果期望的副本数小于最小副本数限制minimumAllowedReplicas，则返回最小副本数限制minimumAllowedReplicas
			//   否则，期望副本数大于等于最小副本数限制minimumAllowedReplicas，返回期望副本数
			desiredReplicas = a.normalizeDesiredReplicasWithBehaviors(hpa, key, currentReplicas, desiredReplicas, minReplicas)
		}
		rescale = desiredReplicas != currentReplicas
	}

	// 当前副本数不等于期望副本数
	if rescale {
		scale.Spec.Replicas = desiredReplicas
		_, err = a.scaleNamespacer.Scales(hpa.Namespace).Update(ctx, targetGR, scale, metav1.UpdateOptions{})
		if err != nil {
			a.eventRecorder.Eventf(hpa, v1.EventTypeWarning, "FailedRescale", "New size: %d; reason: %s; error: %v", desiredReplicas, rescaleReason, err.Error())
			setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionFalse, "FailedUpdateScale", "the HPA controller was unable to update the target scale: %v", err)
			// 设置hpa.Status里currentReplicas
			a.setCurrentReplicasInStatus(hpa, currentReplicas)
			if err := a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa); err != nil {
				utilruntime.HandleError(err)
			}
			return fmt.Errorf("failed to rescale %s: %v", reference, err)
		}
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "SucceededRescale", "the HPA controller was able to update the target scale to %d", desiredReplicas)
		a.eventRecorder.Eventf(hpa, v1.EventTypeNormal, "SuccessfulRescale", "New size: %d; reason: %s", desiredReplicas, rescaleReason)
		// 遍历所有scalingRules.Policies，找到最长的policy.PeriodSeconds
		// 遍历所有scaleEvents []timestampedScaleEvent，不在longestPolicyPeriod周期内的timestampedScaleEvent，设置timestampedScaleEvent.outdated为true
		// 找到最后一个过期的timestampedScaleEvent的index
		// 如果newReplicas > prevReplicas（扩容），则replicaChange为newReplicas - prevReplicas。否则replicaChange为prevReplicas-newReplicas
		// 生成新的newEvent timestampedScaleEvent{replicaChange, time.Now(), false}
		// 如果发现过期的timestampedScaleEvent，则newEvent替换老的。否则，newEvent append到a.scaleDownEvents[key]
		a.storeScaleEvent(hpa.Spec.Behavior, key, currentReplicas, desiredReplicas)
		klog.Infof("Successful rescale of %s, old size: %d, new size: %d, reason: %s",
			hpa.Name, currentReplicas, desiredReplicas, rescaleReason)
	} else {
		// workload的replicas为0，但是hpa的minReplicas不为0，则停止scale，desiredReplicas为0
		// 或desiredReplicas == currentReplicas
		klog.V(4).Infof("decided not to scale %s to %v (last scale time was %s)", reference, desiredReplicas, hpa.Status.LastScaleTime)
		desiredReplicas = currentReplicas
	}

	a.setStatus(hpa, currentReplicas, desiredReplicas, metricStatuses, rescale)
	return a.updateStatusIfNeeded(ctx, hpaStatusOriginal, hpa)
}

// stabilizeRecommendation:
// - replaces old recommendation with the newest recommendation,
// - returns max of recommendations that are not older than downscaleStabilisationWindow.
// 跟prenormalizedDesiredReplicas比较，重新从a.recommendations[key]查找最大副本数maxRecommendation，和查找不在a.downscaleStabilisationWindow稳定窗口里的timestampedRecommendation
// 如果发现不在a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，最后一个不在的index，替换成timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()}
// 如果没有发现a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，则将timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()} append到a.recommendations[key]
func (a *HorizontalController) stabilizeRecommendation(key string, prenormalizedDesiredReplicas int32) int32 {
	maxRecommendation := prenormalizedDesiredReplicas
	foundOldSample := false
	oldSampleIndex := 0
	cutoff := time.Now().Add(-a.downscaleStabilisationWindow)
	// 包括prenormalizedDesiredReplicas后，重新进行查找最大副本数maxRecommendation，和查找不在a.downscaleStabilisationWindow稳定窗口里的timestampedRecommendation
	// a.recommendations[key]第一次reconcile，只有{当前副本数currentReplicas, reconcile时间time.Now()}
	// 后续reconcile，a.recommendations[key]就是上次结果
	for i, rec := range a.recommendations[key] {
		// 不在稳定窗口里
		if rec.timestamp.Before(cutoff) {
			foundOldSample = true
			oldSampleIndex = i
		// 有记录大于maxRecommendation，则设置maxRecommendation为这个rec.recommendation
		} else if rec.recommendation > maxRecommendation {
			maxRecommendation = rec.recommendation
		}
	}
	// 如果发现不在a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，最后一个不在的index，替换成timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()}
	if foundOldSample {
		a.recommendations[key][oldSampleIndex] = timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()}
	} else {
		// 如果没有发现a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，则将timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()} append到a.recommendations[key]
		a.recommendations[key] = append(a.recommendations[key], timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()})
	}
	return maxRecommendation
}

// normalizeDesiredReplicas takes the metrics desired replicas value and normalizes it based on the appropriate conditions (i.e. < maxReplicas, >
// minReplicas, etc...)
// 跟现在最终副本数prenormalizedDesiredReplicas比较，重新从a.recommendations[key]查找最大副本数maxRecommendation，和查找不在a.downscaleStabilisationWindow稳定窗口里的timestampedRecommendation
// 如果发现不在a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，最后一个不在的index，替换成timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()}
// 如果没有发现a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，则将timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()} append到a.recommendations[key]
// stabilizedRecommendation为稳定窗口里最大副本数maxRecommendation
// 最大扩容上限scaleUpLimit为max(2*当前副本数, 4)
// 扩容上限maximumAllowedReplicas：
//   hpaMaxReplicas大于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为scaleUpLimit
//   hpaMaxReplicas小于等于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为hpaMaxReplicas
// 期望的副本数stabilizedRecommendation小于最小副本数，返回最终副本数为hpa最小副本数minimumAllowedReplicas
// 期望的副本数stabilizedRecommendation大于maximumAllowedReplicas扩容上限，则返回maximumAllowedReplicas
// 期望副本数stabilizedRecommendation大于最小副本数，小于等于maximumAllowedReplicas扩容上限，返回desiredReplicas
func (a *HorizontalController) normalizeDesiredReplicas(hpa *autoscalingv2.HorizontalPodAutoscaler, key string, currentReplicas int32, prenormalizedDesiredReplicas int32, minReplicas int32) int32 {
	// 跟prenormalizedDesiredReplicas比较，重新从a.recommendations[key]查找最大副本数maxRecommendation，和查找不在a.downscaleStabilisationWindow稳定窗口里的timestampedRecommendation
	// 如果发现不在a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，最后一个不在的index，替换成timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()}
	// 如果没有发现a.downscaleStabilisationWindow稳定窗口里timestampedRecommendation，则将timestampedRecommendation{prenormalizedDesiredReplicas, time.Now()} append到a.recommendations[key]
	stabilizedRecommendation := a.stabilizeRecommendation(key, prenormalizedDesiredReplicas)
	// 稳定窗口里的最大的副本数不是现在的最终副本数
	if stabilizedRecommendation != prenormalizedDesiredReplicas {
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "ScaleDownStabilized", "recent recommendations were higher than current one, applying the highest recent recommendation")
	} else {
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "ReadyForNewScale", "recommended size matches current size")
	}

	// 最大扩容上限scaleUpLimit为max(2*当前副本数, 4)
	// 扩容上限maximumAllowedReplicas：
	//   hpaMaxReplicas大于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为scaleUpLimit
	//   hpaMaxReplicas小于等于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为hpaMaxReplicas
	// 期望的副本数小于最小副本数，返回最终副本数为hpa最小副本数minimumAllowedReplicas
	// 期望的副本数大于maximumAllowedReplicas扩容上限，则返回maximumAllowedReplicas
	// 期望副本数大于最小副本数，小于等于maximumAllowedReplicas扩容上限，返回desiredReplicas
	desiredReplicas, condition, reason := convertDesiredReplicasWithRules(currentReplicas, stabilizedRecommendation, minReplicas, hpa.Spec.MaxReplicas)

	// 期望副本数大于最小副本数，小于等于maximumAllowedReplicas扩容上限（没有触及上下限制）
	if desiredReplicas == stabilizedRecommendation {
		// 设置hpa的"ScalingLimited" condition为false
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionFalse, condition, reason)
	} else {
		// 设置hpa的"ScalingLimited" condition为true
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionTrue, condition, reason)
	}

	return desiredReplicas
}

// NormalizationArg is used to pass all needed information between functions as one structure
type NormalizationArg struct {
	Key               string
	ScaleUpBehavior   *autoscalingv2.HPAScalingRules
	ScaleDownBehavior *autoscalingv2.HPAScalingRules
	MinReplicas       int32
	MaxReplicas       int32
	CurrentReplicas   int32
	DesiredReplicas   int32
}

// normalizeDesiredReplicasWithBehaviors takes the metrics desired replicas value and normalizes it:
// 1. Apply the basic conditions (i.e. < maxReplicas, > minReplicas, etc...)
// 2. Apply the scale up/down limits from the hpaSpec.Behaviors (i.e. add no more than 4 pods)
// 3. Apply the constraints period (i.e. add no more than 4 pods per minute)
// 4. Apply the stabilization (i.e. add no more than 4 pods per minute, and pick the smallest recommendation during last 5 minutes)
// 1. 对hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds设置默认值
// hpa.Spec.Behavior.ScaleDown不为nil，但是hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为nil
// 则设置hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为a.downscaleStabilisationWindow
// 2. 进行稳定周期内的推荐值计算
// 遍历所有a.recommendations[args.Key]
// 发现即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内，则替换最后一个（即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内）index，为DesiredReplicas的timestampedRecommendation
// 否则，没有发现，则将DesiredReplicas的timestampedRecommendation append到a.recommendations[args.Key]
// 当前副本数CurrentReplicas小于在ScaleUp稳定周期内期望副本的最小值（包括DesiredReplicas）（才会扩容），返回recommendation为ScaleUp稳定周期内期望副本的最小值。
// 当前副本数CurrentReplicas大于在ScaleDown稳定周期内期望副本的最大值（包括DesiredReplicas）（才会缩容），返回recommendation为ScaleDown稳定周期内期望副本的最大值。
// 否则，返回当前副本数CurrentReplicas
// 3.进行Behavior策略限制
// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
// 需要扩容（不能扩太多）：
//   遍历所有scalingRules.Policies
//     遍历所有a.scaleUpEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
//     周期开始时候的副本数为当前副本数-replicas变化量
//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
//   根据SelectPolicy为"Min"或"Max"，筛选出scaleUpLimit策略最终副本数限制最大值或最小值
//   当前副本数已经大于scaleUpLimit策略最终副本数限制，则策略最终副本数限制为当前副本数（这个周期内不能扩容）
//   maximumAllowedReplicas为min(scaleUpLimit, args.MaxReplicas)
//   如果期望的副本数大于最大副本数限制maximumAllowedReplicas，返回最大副本数限制maximumAllowedReplicas
//   否则，期望的副本数小于等于最大副本数限制maximumAllowedReplicas，返回期望的副本数
// 需要缩容的情况（不能缩太多）
//   遍历所有scalingRules.Policies
//     遍历所有a.scaleDownEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
//     周期开始时候的副本数为当前副本数+replicas变化量
//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数-policy.Value
//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1-policy.Value/100)然后向上取整
//     根据SelectPolicy为"Min"或"Max"，策略最终副本数限制scaleUpLimit最大值或最小值
//     策略最终副本数限制scaleUpLimit大于当前副本数，不需要进行缩容（已经比要缩的值还小了）
//   maximumAllowedReplicas为max(scaleUpLimit, args.MinReplicas)
//   如果期望的副本数小于最小副本数限制minimumAllowedReplicas，则返回最小副本数限制minimumAllowedReplicas
//   否则，期望副本数大于等于最小副本数限制minimumAllowedReplicas，返回期望副本数
func (a *HorizontalController) normalizeDesiredReplicasWithBehaviors(hpa *autoscalingv2.HorizontalPodAutoscaler, key string, currentReplicas, prenormalizedDesiredReplicas, minReplicas int32) int32 {
	// hpa.Spec.Behavior.ScaleDown不为nil，但是hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为nil
	// 则设置hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为a.downscaleStabilisationWindow
	a.maybeInitScaleDownStabilizationWindow(hpa)
	normalizationArg := NormalizationArg{
		Key:               key,
		ScaleUpBehavior:   hpa.Spec.Behavior.ScaleUp,
		// 在pkg\apis\autoscaling\v2\defaults.go里SetDefaults_HorizontalPodAutoscalerBehavior保证这里不会是nil
		ScaleDownBehavior: hpa.Spec.Behavior.ScaleDown,
		MinReplicas:       minReplicas,
		MaxReplicas:       hpa.Spec.MaxReplicas,
		CurrentReplicas:   currentReplicas,
		DesiredReplicas:   prenormalizedDesiredReplicas}
	// DesiredReplicas的timestampedRecommendation为{args.DesiredReplicas, time.Now()}
	// 遍历所有a.recommendations[args.Key]
	// 发现即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内，则替换最后一个（即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内）index，为DesiredReplicas的timestampedRecommendation
	// 否则，没有发现，则将DesiredReplicas的timestampedRecommendation append到a.recommendations[args.Key]
	// 当前副本数CurrentReplicas小于在ScaleUp稳定周期内期望副本的最小值（包括DesiredReplicas）（才会扩容），返回recommendation为ScaleUp稳定周期内期望副本的最小值。
	// 当前副本数CurrentReplicas大于在ScaleDown稳定周期内期望副本的最大值（包括DesiredReplicas）（才会缩容），返回recommendation为ScaleDown稳定周期内期望副本的最大值。
	// 否则，返回当前副本数CurrentReplicas
	stabilizedRecommendation, reason, message := a.stabilizeRecommendationWithBehaviors(normalizationArg)
	normalizationArg.DesiredReplicas = stabilizedRecommendation
	if stabilizedRecommendation != prenormalizedDesiredReplicas {
		// "ScaleUpStabilized" || "ScaleDownStabilized"
		// （当前副本数CurrentReplicas小于在ScaleUp稳定周期内期望副本的最小值，或当前副本数CurrentReplicas大于在ScaleDown稳定周期内期望副本的最大值），且（prenormalizedDesiredReplicas不是在ScaleUp稳定周期内是最小值，或prenormalizedDesiredReplicas不是ScaleDown稳定周期内是最大值）
		// 或（当前副本数CurrentReplicas大于等于ScaleUp稳定周期内是最小值，且当前副本数CurrentReplicas小于等于ScaleDown稳定周期内是最大值）
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, reason, message)
	} else {
		// 特殊情况：当期望副本数prenormalizedDesiredReplicas是在ScaleUp稳定周期内是最小值，当前副本数CurrentReplicas小于期望副本数prenormalizedDesiredReplicas，返回期望副本数prenormalizedDesiredReplicas
		// 特殊情况：当期望副本数prenormalizedDesiredReplicas是在ScaleDown稳定周期内是最大值，当前副本数CurrentReplicas大于期望副本数prenormalizedDesiredReplicas，返回期望副本数prenormalizedDesiredReplicas
		// 当前副本数CurrentReplicas等于当期望副本数prenormalizedDesiredReplicas
		// 即不扩容也不缩容
		setCondition(hpa, autoscalingv2.AbleToScale, v1.ConditionTrue, "ReadyForNewScale", "recommended size matches current size")
	}
	// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
	// 需要扩容（不能扩太多）：
	//   遍历所有scalingRules.Policies
	//     遍历所有a.scaleUpEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
	//     周期开始时候的副本数为当前副本数-replicas变化量
	//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
	//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
	//   根据SelectPolicy为"Min"或"Max"，筛选出scaleUpLimit策略最终副本数限制最大值或最小值
	//   当前副本数已经大于scaleUpLimit策略最终副本数限制，则策略最终副本数限制为当前副本数（这个周期内不能扩容）
	//   maximumAllowedReplicas为min(scaleUpLimit, args.MaxReplicas)
	//   如果期望的副本数大于最大副本数限制maximumAllowedReplicas，返回最大副本数限制maximumAllowedReplicas
	//   否则，期望的副本数小于等于最大副本数限制maximumAllowedReplicas，返回期望的副本数
	// 需要缩容的情况（不能缩太多）
	//   遍历所有scalingRules.Policies
	//     遍历所有a.scaleDownEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
	//     周期开始时候的副本数为当前副本数+replicas变化量
	//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数-policy.Value
	//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1-policy.Value/100)然后向上取整
	//     根据SelectPolicy为"Min"或"Max"，策略最终副本数限制scaleUpLimit最大值或最小值
	//     策略最终副本数限制scaleUpLimit大于当前副本数，不需要进行缩容（已经比要缩的值还小了）
	//   maximumAllowedReplicas为max(scaleUpLimit, args.MinReplicas)
	//   如果期望的副本数小于最小副本数限制minimumAllowedReplicas，则返回最小副本数限制minimumAllowedReplicas
	//   否则，期望副本数大于等于最小副本数限制minimumAllowedReplicas，返回期望副本数
	desiredReplicas, reason, message := a.convertDesiredReplicasWithBehaviorRate(normalizationArg)
	// desiredReplicas == stabilizedRecommendation，说明不受策略限制
	if desiredReplicas == stabilizedRecommendation {
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionFalse, reason, message)
	} else {
		setCondition(hpa, autoscalingv2.ScalingLimited, v1.ConditionTrue, reason, message)
	}

	return desiredReplicas
}

func (a *HorizontalController) maybeInitScaleDownStabilizationWindow(hpa *autoscalingv2.HorizontalPodAutoscaler) {
	behavior := hpa.Spec.Behavior
	// hpa.Spec.Behavior.ScaleDown不为nil，但是hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为nil
	// 则设置hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds为a.downscaleStabilisationWindow
	if behavior != nil && behavior.ScaleDown != nil && behavior.ScaleDown.StabilizationWindowSeconds == nil {
		stabilizationWindowSeconds := (int32)(a.downscaleStabilisationWindow.Seconds())
		hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = &stabilizationWindowSeconds
	}
}

// getReplicasChangePerPeriod function find all the replica changes per period
// 遍历所有scaleEvents []timestampedScaleEvent，如果timestampedScaleEvent在periodSeconds周期内，则进行replicas变化量累加
// 返回总的replicas变化量
func getReplicasChangePerPeriod(periodSeconds int32, scaleEvents []timestampedScaleEvent) int32 {
	period := time.Second * time.Duration(periodSeconds)
	cutoff := time.Now().Add(-period)
	var replicas int32
	for _, rec := range scaleEvents {
		// rec在periodSeconds周期内，则进行replicas变化量累加
		if rec.timestamp.After(cutoff) {
			replicas += rec.replicaChange
		}
	}
	return replicas

}

// 返回Type为"ScalingActive"，Status为false，Reason为reason，Message为"the HPA was unable to compute the replica count: {Error()}"
func (a *HorizontalController) getUnableComputeReplicaCountCondition(hpa runtime.Object, reason string, err error) (condition autoscalingv2.HorizontalPodAutoscalerCondition) {
	a.eventRecorder.Event(hpa, v1.EventTypeWarning, reason, err.Error())
	return autoscalingv2.HorizontalPodAutoscalerCondition{
		Type:    autoscalingv2.ScalingActive,
		Status:  v1.ConditionFalse,
		Reason:  reason,
		Message: fmt.Sprintf("the HPA was unable to compute the replica count: %v", err),
	}
}

// storeScaleEvent stores (adds or replaces outdated) scale event.
// outdated events to be replaced were marked as outdated in the `markScaleEventsOutdated` function
// 遍历所有scalingRules.Policies，找到最长的policy.PeriodSeconds
// 遍历所有scaleEvents []timestampedScaleEvent，不在longestPolicyPeriod周期内的timestampedScaleEvent，设置timestampedScaleEvent.outdated为true
// 找到最后一个过期的timestampedScaleEvent的index
// 如果newReplicas > prevReplicas（扩容），则replicaChange为newReplicas - prevReplicas。否则replicaChange为prevReplicas-newReplicas
// 生成新的newEvent timestampedScaleEvent{replicaChange, time.Now(), false}
// 如果发现过期的timestampedScaleEvent，则newEvent替换老的。否则，newEvent append到a.scaleDownEvents[key]
func (a *HorizontalController) storeScaleEvent(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior, key string, prevReplicas, newReplicas int32) {
	if behavior == nil {
		return // we should not store any event as they will not be used
	}
	var oldSampleIndex int
	var longestPolicyPeriod int32
	foundOldSample := false
	// 需要扩容情况
	if newReplicas > prevReplicas {
		// 遍历所有scalingRules.Policies，找到最长的policy.PeriodSeconds
		longestPolicyPeriod = getLongestPolicyPeriod(behavior.ScaleUp)
		// 遍历所有scaleEvents []timestampedScaleEvent，不在longestPolicyPeriod周期内的timestampedScaleEvent，设置timestampedScaleEvent.outdated为true
		markScaleEventsOutdated(a.scaleUpEvents[key], longestPolicyPeriod)
		// 扩的数量，一定是大于等于0
		replicaChange := newReplicas - prevReplicas
		// 找到最后一个过期的timestampedScaleEvent的index
		for i, event := range a.scaleUpEvents[key] {
			if event.outdated {
				foundOldSample = true
				oldSampleIndex = i
			}
		}
		newEvent := timestampedScaleEvent{replicaChange, time.Now(), false}
		// 如果发现过期的timestampedScaleEvent，则newEvent替换老的。否则，newEvent append到a.scaleDownEvents[key]
		if foundOldSample {
			a.scaleUpEvents[key][oldSampleIndex] = newEvent
		} else {
			a.scaleUpEvents[key] = append(a.scaleUpEvents[key], newEvent)
		}
	} else {
		// 需要缩容，或不扩缩容情况
		// 遍历所有scalingRules.Policies，找到最长的policy.PeriodSeconds
		longestPolicyPeriod = getLongestPolicyPeriod(behavior.ScaleDown)
		// 遍历所有scaleEvents []timestampedScaleEvent，不在longestPolicyPeriod周期内的timestampedScaleEvent，设置timestampedScaleEvent.outdated为true
		markScaleEventsOutdated(a.scaleDownEvents[key], longestPolicyPeriod)
		// 缩的数量，一定是大于等于0
		replicaChange := prevReplicas - newReplicas
		// 找到最后一个过期的timestampedScaleEvent的index
		for i, event := range a.scaleDownEvents[key] {
			if event.outdated {
				foundOldSample = true
				oldSampleIndex = i
			}
		}
		newEvent := timestampedScaleEvent{replicaChange, time.Now(), false}
		// 如果发现过期的timestampedScaleEvent，则newEvent替换老的。否则，newEvent append到a.scaleDownEvents[key]
		if foundOldSample {
			a.scaleDownEvents[key][oldSampleIndex] = newEvent
		} else {
			a.scaleDownEvents[key] = append(a.scaleDownEvents[key], newEvent)
		}
	}
}

// stabilizeRecommendationWithBehaviors:
// - replaces old recommendation with the newest recommendation,
// - returns {max,min} of recommendations that are not older than constraints.Scale{Up,Down}.DelaySeconds
// DesiredReplicas的timestampedRecommendation为{args.DesiredReplicas, time.Now()}
// 遍历所有a.recommendations[args.Key]
// 发现即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内，则替换最后一个（即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内）index，为DesiredReplicas的timestampedRecommendation
// 否则，没有发现，则将DesiredReplicas的timestampedRecommendation append到a.recommendations[args.Key]
// 当前副本数CurrentReplicas小于在ScaleUp稳定周期内期望副本的最小值（才会扩容），返回recommendation为ScaleUp稳定周期内期望副本的最小值。
// 特殊情况：当期望副本数DesiredReplicas是在ScaleUp稳定周期内是最小值，当前副本数CurrentReplicas小于期望副本数DesiredReplicas，返回期望副本数DesiredReplicas
// 当前副本数CurrentReplicas大于在ScaleDown稳定周期内期望副本的最大值（才会缩容），返回recommendation为ScaleDown稳定周期内期望副本的最大值。
// 特殊情况：当期望副本数DesiredReplicas是在ScaleDown稳定周期内是最大值，当前副本数CurrentReplicas大于期望副本数DesiredReplicas，返回期望副本数DesiredReplicas
// 否则，返回当前副本数CurrentReplicas
func (a *HorizontalController) stabilizeRecommendationWithBehaviors(args NormalizationArg) (int32, string, string) {
	now := time.Now()

	foundOldSample := false
	oldSampleIndex := 0

	upRecommendation := args.DesiredReplicas
	// 在pkg\apis\autoscaling\v2\defaults.go里SetDefaults_HorizontalPodAutoscalerBehavior保证这里不会是nil
	upDelaySeconds := *args.ScaleUpBehavior.StabilizationWindowSeconds
	upCutoff := now.Add(-time.Second * time.Duration(upDelaySeconds))

	downRecommendation := args.DesiredReplicas
	// a.maybeInitScaleDownStabilizationWindow保证这里不会是nil
	downDelaySeconds := *args.ScaleDownBehavior.StabilizationWindowSeconds
	downCutoff := now.Add(-time.Second * time.Duration(downDelaySeconds))

	// Calculate the upper and lower stabilization limits.
	for i, rec := range a.recommendations[args.Key] {
		// timestampedRecommendation在ScaleUp稳定周期内
		if rec.timestamp.After(upCutoff) {
			// upRecommendation为最小的副本数（包括args.DesiredReplicas）
			upRecommendation = min(rec.recommendation, upRecommendation)
		}
		// timestampedRecommendation在ScaleDown稳定周期内
		if rec.timestamp.After(downCutoff) {
			// downRecommendation为最大的副本数（包括args.DesiredReplicas）
			downRecommendation = max(rec.recommendation, downRecommendation)
		}
		// timestampedRecommendation即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内
		if rec.timestamp.Before(upCutoff) && rec.timestamp.Before(downCutoff) {
			foundOldSample = true
			oldSampleIndex = i
		}
	}

	// Bring the recommendation to within the upper and lower limits (stabilize).
	// 确保recommendation在ScaleUp稳定周期内最小的副本数或ScaleDown稳定周期内最大副本数边界内
	// 下面两个只会执行一个判断
	recommendation := args.CurrentReplicas
	// 当前副本数小于ScaleUp稳定周期内最小的副本数，则recommendation为ScaleUp稳定周期内最小的副本数
	if recommendation < upRecommendation {
		recommendation = upRecommendation
	}
	// 当前副本数recommendation大于ScaleDown稳定周期内最大副本数，则recommendation为ScaleDown稳定周期内最大副本数
	if recommendation > downRecommendation {
		recommendation = downRecommendation
	}

	// Record the unstabilized recommendation.
	// 发现即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内，则替换最后一个（即不在ScaleUp稳定周期内，也不在ScaleDown稳定周期内）index
	if foundOldSample {
		a.recommendations[args.Key][oldSampleIndex] = timestampedRecommendation{args.DesiredReplicas, time.Now()}
	} else {
		// 进行append
		a.recommendations[args.Key] = append(a.recommendations[args.Key], timestampedRecommendation{args.DesiredReplicas, time.Now()})
	}

	// Determine a human-friendly message.
	var reason, message string
	// 需要扩容情况，或不需要扩缩容
	if args.DesiredReplicas >= args.CurrentReplicas {
		reason = "ScaleUpStabilized"
		message = "recent recommendations were lower than current one, applying the lowest recent recommendation"
	} else {
		// 缩容情况
		reason = "ScaleDownStabilized"
		message = "recent recommendations were higher than current one, applying the highest recent recommendation"
	}
	return recommendation, reason, message
}

// convertDesiredReplicasWithBehaviorRate performs the actual normalization, given the constraint rate
// It doesn't consider the stabilizationWindow, it is done separately
// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
// 需要扩容（不能扩太多）：
//   遍历所有scalingRules.Policies
//     遍历所有a.scaleUpEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
//     周期开始时候的副本数为当前副本数-replicas变化量
//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
//   根据SelectPolicy为"Min"或"Max"，筛选出scaleUpLimit策略最终副本数限制最大值或最小值
//   当前副本数已经大于scaleUpLimit策略最终副本数限制，则策略最终副本数限制为当前副本数（这个周期内不能扩容）
//   maximumAllowedReplicas为min(scaleUpLimit, args.MaxReplicas)
//   如果期望的副本数大于最大副本数限制maximumAllowedReplicas，返回最大副本数限制maximumAllowedReplicas
//   否则，期望的副本数小于等于最大副本数限制maximumAllowedReplicas，返回期望的副本数
// 需要缩容的情况（不能缩太多）
//   遍历所有scalingRules.Policies
//     遍历所有a.scaleDownEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
//     周期开始时候的副本数为当前副本数+replicas变化量
//     如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数-policy.Value
//     如果policy.Type为"Percent"，则最终副本数为周期开始时候的副本数*(1-policy.Value/100)然后向上取整
//     根据SelectPolicy为"Min"或"Max"，策略最终副本数限制scaleUpLimit最大值或最小值
//     策略最终副本数限制scaleUpLimit大于当前副本数，不需要进行缩容（已经比要缩的值还小了）
//   maximumAllowedReplicas为max(scaleUpLimit, args.MinReplicas)
//   如果期望的副本数小于最小副本数限制minimumAllowedReplicas，则返回最小副本数限制minimumAllowedReplicas
//   否则，期望副本数大于等于最小副本数限制minimumAllowedReplicas，返回期望副本数
func (a *HorizontalController) convertDesiredReplicasWithBehaviorRate(args NormalizationArg) (int32, string, string) {
	var possibleLimitingReason, possibleLimitingMessage string

	// 需要扩容（不能扩太多）
	if args.DesiredReplicas > args.CurrentReplicas {
		// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
		// 遍历所有scalingRules.Policies
		//   遍历所有a.scaleUpEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
		//   周期开始时候的副本数为当前副本数-replicas变化量
		//   如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
		//   policy.Type为"Percent"
		//   最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
		// 根据SelectPolicy为"Min"或"Max"，筛选出策略最终副本数最大值或最小值
		scaleUpLimit := calculateScaleUpLimitWithScalingRules(args.CurrentReplicas, a.scaleUpEvents[args.Key], args.ScaleUpBehavior)
		// 当前副本数已经大于策略最终副本数限制，则策略最终副本数限制为当前副本数（这个周期内不能扩容）
		if scaleUpLimit < args.CurrentReplicas {
			// We shouldn't scale up further until the scaleUpEvents will be cleaned up
			scaleUpLimit = args.CurrentReplicas
		}
		maximumAllowedReplicas := args.MaxReplicas
		// maximumAllowedReplicas为min(scaleUpLimit, args.MaxReplicas)
		// 策略最终副本数限制scaleUpLimit小于hpa的最大副本数maximumAllowedReplicas，则最大副本数限制maximumAllowedReplicas为scaleUpLimit
		if maximumAllowedReplicas > scaleUpLimit {
			maximumAllowedReplicas = scaleUpLimit
			possibleLimitingReason = "ScaleUpLimit"
			possibleLimitingMessage = "the desired replica count is increasing faster than the maximum scale rate"
		} else {
			// 策略最终副本数限制scaleUpLimit大于等于hpa的最大副本数maximumAllowedReplicas
			// maximumAllowedReplicas就是自己
			possibleLimitingReason = "TooManyReplicas"
			possibleLimitingMessage = "the desired replica count is more than the maximum replica count"
		}
		// 期望的副本数大于最大副本数限制maximumAllowedReplicas，返回最大副本数限制maximumAllowedReplicas
		if args.DesiredReplicas > maximumAllowedReplicas {
			return maximumAllowedReplicas, possibleLimitingReason, possibleLimitingMessage
		}
	} else if args.DesiredReplicas < args.CurrentReplicas {
		// 需要缩容的情况（不能缩太多）
		// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
		// 遍历所有scalingRules.Policies
		//   遍历所有a.scaleDownEvents[args.Key] []timestampedScaleEvent，计算出replicas总的变化量。
		//   周期开始时候的副本数为当前副本数+replicas变化量
		//   如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数-policy.Value
		//   policy.Type为"Percent"
		//   最终副本数为周期开始时候的副本数*(1-policy.Value/100)然后向上取整
		// 根据SelectPolicy为"Min"或"Max"，筛选出最终副本数最大值或最小值
		scaleDownLimit := calculateScaleDownLimitWithBehaviors(args.CurrentReplicas, a.scaleDownEvents[args.Key], args.ScaleDownBehavior)
		// 策略最终副本数限制scaleUpLimit大于当前副本数，不需要进行缩容（已经比要缩的值还小了）
		if scaleDownLimit > args.CurrentReplicas {
			// We shouldn't scale down further until the scaleDownEvents will be cleaned up
			scaleDownLimit = args.CurrentReplicas
		}
		// hpa最小副本数
		minimumAllowedReplicas := args.MinReplicas
		// maximumAllowedReplicas为max(scaleUpLimit, args.MinReplicas)
		// hpa最小副本数小于策略最终副本数限制，则最小副本数限制minimumAllowedReplicas为策略最终副本数限制（不超出策略缩的数量限制）
		if minimumAllowedReplicas < scaleDownLimit {
			minimumAllowedReplicas = scaleDownLimit
			possibleLimitingReason = "ScaleDownLimit"
			possibleLimitingMessage = "the desired replica count is decreasing faster than the maximum scale rate"
		} else {
			// hpa最小副本数大于等于策略最终副本数限制，则最小副本数限制minimumAllowedReplicas为hpa最小副本数（策略最终副本数限制，缩的太多，比最小副本数还小）
			possibleLimitingMessage = "the desired replica count is less than the minimum replica count"
			possibleLimitingReason = "TooFewReplicas"
		}
		// 期望的副本数小于最小副本数限制minimumAllowedReplicas，则返回最小副本数限制minimumAllowedReplicas
		if args.DesiredReplicas < minimumAllowedReplicas {
			return minimumAllowedReplicas, possibleLimitingReason, possibleLimitingMessage
		}
	}
	// 需要扩容，期望的副本数小于等于最大副本数限制maximumAllowedReplicas，返回期望的副本数
	// 需要缩容，期望副本数大于等于最小副本数限制minimumAllowedReplicas，返回期望副本数
	return args.DesiredReplicas, "DesiredWithinRange", "the desired count is within the acceptable range"
}

// convertDesiredReplicas performs the actual normalization, without depending on `HorizontalController` or `HorizontalPodAutoscaler`
// 最大扩容上限scaleUpLimit为max(2*当前副本数, 4)
// 扩容上限maximumAllowedReplicas：
//   hpaMaxReplicas大于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为scaleUpLimit
//   hpaMaxReplicas小于等于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为hpaMaxReplicas
// 期望的副本数小于最小副本数，返回最终副本数为hpa最小副本数minimumAllowedReplicas
// 期望的副本数大于maximumAllowedReplicas扩容上限，则返回maximumAllowedReplicas
// 期望副本数大于最小副本数，小于等于maximumAllowedReplicas扩容上限，返回desiredReplicas
func convertDesiredReplicasWithRules(currentReplicas, desiredReplicas, hpaMinReplicas, hpaMaxReplicas int32) (int32, string, string) {

	var minimumAllowedReplicas int32
	var maximumAllowedReplicas int32

	var possibleLimitingCondition string
	var possibleLimitingReason string

	minimumAllowedReplicas = hpaMinReplicas

	// Do not upscale too much to prevent incorrect rapid increase of the number of master replicas caused by
	// bogus CPU usage report from heapster/kubelet (like in issue #32304).
	// max(2*当前副本数, 4)
	scaleUpLimit := calculateScaleUpLimit(currentReplicas)

	// hpaMaxReplicas大于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为scaleUpLimit
	if hpaMaxReplicas > scaleUpLimit {
		maximumAllowedReplicas = scaleUpLimit
		possibleLimitingCondition = "ScaleUpLimit"
		possibleLimitingReason = "the desired replica count is increasing faster than the maximum scale rate"
	} else {
		// hpaMaxReplicas小于等于scaleUpLimit（最大扩容上限），则maximumAllowedReplicas扩容上限为hpaMaxReplicas
		maximumAllowedReplicas = hpaMaxReplicas
		possibleLimitingCondition = "TooManyReplicas"
		possibleLimitingReason = "the desired replica count is more than the maximum replica count"
	}

	// 期望的副本数小于最小副本数，返回最终副本数为minimumAllowedReplicas
	if desiredReplicas < minimumAllowedReplicas {
		possibleLimitingCondition = "TooFewReplicas"
		possibleLimitingReason = "the desired replica count is less than the minimum replica count"

		return minimumAllowedReplicas, possibleLimitingCondition, possibleLimitingReason
	} else if desiredReplicas > maximumAllowedReplicas {
		// 期望的副本数大于maximumAllowedReplicas扩容上限，则返回maximumAllowedReplicas
		return maximumAllowedReplicas, possibleLimitingCondition, possibleLimitingReason
	}

	// 期望副本数大于最小副本数，小于等于maximumAllowedReplicas扩容上限，返回desiredReplicas
	return desiredReplicas, "DesiredWithinRange", "the desired count is within the acceptable range"
}

// max(2*当前副本数, 4)
func calculateScaleUpLimit(currentReplicas int32) int32 {
	return int32(math.Max(scaleUpLimitFactor*float64(currentReplicas), scaleUpLimitMinimum))
}

// markScaleEventsOutdated set 'outdated=true' flag for all scale events that are not used by any HPA object
// 遍历所有scaleEvents []timestampedScaleEvent，不在longestPolicyPeriod周期内的timestampedScaleEvent，设置timestampedScaleEvent.outdated为true
func markScaleEventsOutdated(scaleEvents []timestampedScaleEvent, longestPolicyPeriod int32) {
	period := time.Second * time.Duration(longestPolicyPeriod)
	cutoff := time.Now().Add(-period)
	for i, event := range scaleEvents {
		if event.timestamp.Before(cutoff) {
			// outdated scale event are marked for later reuse
			scaleEvents[i].outdated = true
		}
	}
}

// 遍历所有scalingRules.Policies，找到最长的policy.PeriodSeconds
func getLongestPolicyPeriod(scalingRules *autoscalingv2.HPAScalingRules) int32 {
	var longestPolicyPeriod int32
	for _, policy := range scalingRules.Policies {
		if policy.PeriodSeconds > longestPolicyPeriod {
			longestPolicyPeriod = policy.PeriodSeconds
		}
	}
	return longestPolicyPeriod
}

// calculateScaleUpLimitWithScalingRules returns the maximum number of pods that could be added for the given HPAScalingRules
// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
// 遍历所有scalingRules.Policies
//   遍历所有scaleEvents []timestampedScaleEvent，计算出replicas总的变化量。
//   周期开始时候的副本数为当前副本数-replicas变化量
//   如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
//   policy.Type为"Percent"
//   最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
// 根据SelectPolicy为"Min"或"Max"，筛选出最终副本数最大值或最小值
func calculateScaleUpLimitWithScalingRules(currentReplicas int32, scaleEvents []timestampedScaleEvent, scalingRules *autoscalingv2.HPAScalingRules) int32 {
	var result int32
	var proposed int32
	var selectPolicyFn func(int32, int32) int32
	// SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
	if *scalingRules.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return currentReplicas // Scaling is disabled
	} else if *scalingRules.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		// SelectPolicy为"Min"
		result = math.MaxInt32
		selectPolicyFn = min // For scaling up, the lowest change ('min' policy) produces a minimum value
	} else {
		// SelectPolicy为"Max"
		result = math.MinInt32
		selectPolicyFn = max // Use the default policy otherwise to produce a highest possible change
	}
	// 遍历所有scalingRules.Policies
	//   遍历所有scaleEvents []timestampedScaleEvent，计算出replicas总的变化量。
	//   周期开始时候的副本数为当前副本数-replicas变化量
	//   如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
	//   policy.Type为"Percent"
	//   最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
	// 根据SelectPolicy为"Min"或"Max"，筛选出最大值或最小值
	for _, policy := range scalingRules.Policies {
		// 遍历所有scaleEvents []timestampedScaleEvent，如果timestampedScaleEvent在periodSeconds周期内，则进行replicas变化量累加
		// 返回总的replicas变化量
		replicasAddedInCurrentPeriod := getReplicasChangePerPeriod(policy.PeriodSeconds, scaleEvents)
		// 周期开始时候的副本数为当前副本数-replicas变化量
		periodStartReplicas := currentReplicas - replicasAddedInCurrentPeriod
		// policy.Type为"Pods"
		if policy.Type == autoscalingv2.PodsScalingPolicy {
			// 最终副本数为周期开始时候的副本数+policy.Value
			proposed = periodStartReplicas + policy.Value
		} else if policy.Type == autoscalingv2.PercentScalingPolicy {
			// the proposal has to be rounded up because the proposed change might not increase the replica count causing the target to never scale up
			// policy.Type为"Percent"
			// 最终副本数为周期开始时候的副本数*(1+policy.Value/100)然后向上取整
			proposed = int32(math.Ceil(float64(periodStartReplicas) * (1 + float64(policy.Value)/100)))
		}
		// 执行策略的最大值或最小值
		result = selectPolicyFn(result, proposed)
	}
	return result
}

// calculateScaleDownLimitWithBehavior returns the maximum number of pods that could be deleted for the given HPAScalingRules
// 如果SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
// 遍历所有scalingRules.Policies
//   遍历所有scaleEvents []timestampedScaleEvent，计算出replicas总的变化量。
//   周期开始时候的副本数为当前副本数+replicas变化量
//   如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数-policy.Value
//   policy.Type为"Percent"
//   最终副本数为周期开始时候的副本数*(1-policy.Value/100)然后向上取整
// 根据SelectPolicy为"Min"或"Max"，筛选出最终副本数最大值或最小值
func calculateScaleDownLimitWithBehaviors(currentReplicas int32, scaleEvents []timestampedScaleEvent, scalingRules *autoscalingv2.HPAScalingRules) int32 {
	var result int32
	var proposed int32
	var selectPolicyFn func(int32, int32) int32
	// SelectPolicy为"Disabled"，则不进行扩缩容，返回当前副本数
	if *scalingRules.SelectPolicy == autoscalingv2.DisabledPolicySelect {
		return currentReplicas // Scaling is disabled
	} else if *scalingRules.SelectPolicy == autoscalingv2.MinChangePolicySelect {
		// SelectPolicy为"Min"，选择所有Policies中最大值，缩最小数量
		result = math.MinInt32
		selectPolicyFn = max // For scaling down, the lowest change ('min' policy) produces a maximum value
	} else {
		// SelectPolicy为"Max"，选择所有Policies中最小值，缩最大数量
		result = math.MaxInt32
		selectPolicyFn = min // Use the default policy otherwise to produce a highest possible change
	}
	// 遍历所有scalingRules.Policies
	//   遍历所有scaleEvents []timestampedScaleEvent，计算出replicas总的变化量。
	//   周期开始时候的副本数为当前副本数+replicas变化量
	//   如果policy.Type为"Pods"，则最终副本数为周期开始时候的副本数+policy.Value
	//   policy.Type为"Percent"
	//   最终副本数为周期开始时候的副本数*(1-policy.Value/100)然后向上取整
	// 根据SelectPolicy为"Min"或"Max"，筛选出最大值或最小值
	for _, policy := range scalingRules.Policies {
		// 遍历所有scaleEvents []timestampedScaleEvent，如果timestampedScaleEvent在periodSeconds周期内，则进行replicas变化量累加
		// 返回总的replicas变化量
		replicasDeletedInCurrentPeriod := getReplicasChangePerPeriod(policy.PeriodSeconds, scaleEvents)
		// 周期开始时候的副本数为当前副本数+总的replicas变化量（这里没bug，replicasDeletedInCurrentPeriod一定是大于0，字段注释与实际不符）
		periodStartReplicas := currentReplicas + replicasDeletedInCurrentPeriod
		// policy.Type为"Pods"，最终副本数为周期开始时候的副本数-policy.Value
		if policy.Type == autoscalingv2.PodsScalingPolicy {
			proposed = periodStartReplicas - policy.Value
		// policy.Type为"Percent"，最终副本数为周期开始时候的副本数 * (1 - policy.Value/100)
		} else if policy.Type == autoscalingv2.PercentScalingPolicy {
			proposed = int32(float64(periodStartReplicas) * (1 - float64(policy.Value)/100))
		}
		result = selectPolicyFn(result, proposed)
	}
	return result
}

// scaleForResourceMappings attempts to fetch the scale for the
// resource with the given name and namespace, trying each RESTMapping
// in turn until a working one is found.  If none work, the first error
// is returned.  It returns both the scale, as well as the group-resource from
// the working mapping.
// 比如说是deployment的mappings为apimeta.RESTMapping{Resource: {Group: "apps", "Version": "v1", Resource: "deployments"}, GroupVersionKind: {Group: "apps", "Version": "v1", Kind: "Deployment"}, Scope: RESTScopeRoot}
// 那么就是访问"/apis/apps/v1/namespaces/{namespace}/deployment/{name}/scale"，获得*autoscalingv1.Scale
func (a *HorizontalController) scaleForResourceMappings(ctx context.Context, namespace, name string, mappings []*apimeta.RESTMapping) (*autoscalingv1.Scale, schema.GroupResource, error) {
	var firstErr error
	for i, mapping := range mappings {
		// {Group: "apps", Resource: "deployments"}
		targetGR := mapping.Resource.GroupResource()
		scale, err := a.scaleNamespacer.Scales(namespace).Get(ctx, targetGR, name, metav1.GetOptions{})
		if err == nil {
			return scale, targetGR, nil
		}

		// if this is the first error, remember it,
		// then go on and try other mappings until we find a good one
		if i == 0 {
			firstErr = err
		}
	}

	// make sure we handle an empty set of mappings
	if firstErr == nil {
		firstErr = fmt.Errorf("unrecognized resource")
	}

	return nil, schema.GroupResource{}, firstErr
}

// setCurrentReplicasInStatus sets the current replica count in the status of the HPA.
// 设置hpa.Status里currentReplicas
func (a *HorizontalController) setCurrentReplicasInStatus(hpa *autoscalingv2.HorizontalPodAutoscaler, currentReplicas int32) {
	a.setStatus(hpa, currentReplicas, hpa.Status.DesiredReplicas, hpa.Status.CurrentMetrics, false)
}

// setStatus recreates the status of the given HPA, updating the current and
// desired replicas, as well as the metric statuses
func (a *HorizontalController) setStatus(hpa *autoscalingv2.HorizontalPodAutoscaler, currentReplicas, desiredReplicas int32, metricStatuses []autoscalingv2.MetricStatus, rescale bool) {
	hpa.Status = autoscalingv2.HorizontalPodAutoscalerStatus{
		CurrentReplicas: currentReplicas,
		DesiredReplicas: desiredReplicas,
		LastScaleTime:   hpa.Status.LastScaleTime,
		CurrentMetrics:  metricStatuses,
		Conditions:      hpa.Status.Conditions,
	}

	if rescale {
		now := metav1.NewTime(time.Now())
		hpa.Status.LastScaleTime = &now
	}
}

// updateStatusIfNeeded calls updateStatus only if the status of the new HPA is not the same as the old status
func (a *HorizontalController) updateStatusIfNeeded(ctx context.Context, oldStatus *autoscalingv2.HorizontalPodAutoscalerStatus, newHPA *autoscalingv2.HorizontalPodAutoscaler) error {
	// skip a write if we wouldn't need to update
	if apiequality.Semantic.DeepEqual(oldStatus, &newHPA.Status) {
		return nil
	}
	return a.updateStatus(ctx, newHPA)
}

// updateStatus actually does the update request for the status of the given HPA
func (a *HorizontalController) updateStatus(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler) error {
	_, err := a.hpaNamespacer.HorizontalPodAutoscalers(hpa.Namespace).UpdateStatus(ctx, hpa, metav1.UpdateOptions{})
	if err != nil {
		a.eventRecorder.Event(hpa, v1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		return fmt.Errorf("failed to update status for %s: %v", hpa.Name, err)
	}
	klog.V(2).Infof("Successfully updated status for %s", hpa.Name)
	return nil
}

// setCondition sets the specific condition type on the given HPA to the specified value with the given reason
// and message.  The message and args are treated like a format string.  The condition will be added if it is
// not present.
func setCondition(hpa *autoscalingv2.HorizontalPodAutoscaler, conditionType autoscalingv2.HorizontalPodAutoscalerConditionType, status v1.ConditionStatus, reason, message string, args ...interface{}) {
	hpa.Status.Conditions = setConditionInList(hpa.Status.Conditions, conditionType, status, reason, message, args...)
}

// setConditionInList sets the specific condition type on the given HPA to the specified value with the given
// reason and message.  The message and args are treated like a format string.  The condition will be added if
// it is not present.  The new list will be returned.
func setConditionInList(inputList []autoscalingv2.HorizontalPodAutoscalerCondition, conditionType autoscalingv2.HorizontalPodAutoscalerConditionType, status v1.ConditionStatus, reason, message string, args ...interface{}) []autoscalingv2.HorizontalPodAutoscalerCondition {
	resList := inputList
	var existingCond *autoscalingv2.HorizontalPodAutoscalerCondition
	for i, condition := range resList {
		if condition.Type == conditionType {
			// can't take a pointer to an iteration variable
			existingCond = &resList[i]
			break
		}
	}

	if existingCond == nil {
		resList = append(resList, autoscalingv2.HorizontalPodAutoscalerCondition{
			Type: conditionType,
		})
		existingCond = &resList[len(resList)-1]
	}

	if existingCond.Status != status {
		existingCond.LastTransitionTime = metav1.Now()
	}

	existingCond.Status = status
	existingCond.Reason = reason
	existingCond.Message = fmt.Sprintf(message, args...)

	return resList
}

func max(a, b int32) int32 {
	if a >= b {
		return a
	}
	return b
}

func min(a, b int32) int32 {
	if a <= b {
		return a
	}
	return b
}
