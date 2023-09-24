/*
Copyright 2019 The Kubernetes Authors.

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

package apply

import (
	"context"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/dynamic"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

type pruner struct {
	mapper        meta.RESTMapper
	dynamicClient dynamic.Interface

	visitedUids       sets.String
	visitedNamespaces sets.String
	labelSelector     string
	fieldSelector     string

	cascadingStrategy metav1.DeletionPropagation
	dryRunStrategy    cmdutil.DryRunStrategy
	gracePeriod       int

	toPrinter func(string) (printers.ResourcePrinter, error)

	out io.Writer
}

func newPruner(o *ApplyOptions) pruner {
	return pruner{
		mapper:        o.Mapper,
		dynamicClient: o.DynamicClient,

		labelSelector:     o.Selector,
		visitedUids:       o.VisitedUids,
		visitedNamespaces: o.VisitedNamespaces,

		cascadingStrategy: o.DeleteOptions.CascadingStrategy,
		dryRunStrategy:    o.DryRunStrategy,
		gracePeriod:       o.DeleteOptions.GracePeriod,

		toPrinter: o.ToPrinter,

		out: o.Out,
	}
}

// 默认的o.PruneResources（--prune-whitelist）为空
// 如果pruneResources为空，则pruneResources使用默认的资源类型列表（ConfigMap、Endpoints、Namespace、PersistentVolumeClaim、PersistentVolume、Pod、ReplicationController、Secret、Service、Job、CronJob、Ingress、DaemonSet、Deployment、ReplicaSet、StatefulSet）
// 遍历需要apply的namespaced资源（文件中的命名空间下的对象）的所有命名空间和命名空间下的pruneResources类型的所有资源
// 资源不存在annotations["kubectl.kubernetes.io/last-applied-configuration"]，说明不是kubectl apply创建的资源，则跳过
// 如果资源uid是kubectl apply操作的资源的uid，则跳过
// 如果--dry-run不为client，则执行删除这个资源。否则，不做删除
// 打印出删除资源（默认输出{resource}.{group}/{name} pruned）
func (p *pruner) pruneAll(o *ApplyOptions) error {

	// 默认的o.PruneResources（--prune-whitelist）为空
	// 如果pruneResources为空，则pruneResources使用默认的资源类型列表（ConfigMap、Endpoints、Namespace、PersistentVolumeClaim、PersistentVolume、Pod、ReplicationController、Secret、Service、Job、CronJob、Ingress、DaemonSet、Deployment、ReplicaSet、StatefulSet）
	// 遍历pruneResources，通过mapper获得这个resource对应的meta.RESTMapping
	// 如果resource是namespaced（命名空间的），则对应的meta.RESTMapping添加到namespaced，否则添加到nonNamespaced
	namespacedRESTMappings, nonNamespacedRESTMappings, err := getRESTMappings(o.Mapper, &(o.PruneResources))
	if err != nil {
		return fmt.Errorf("error retrieving RESTMappings to prune: %v", err)
	}

	// p.visitedNamespaces为需要apply的namespaced资源（文件中的命名空间下的对象）的所有命名空间
	for n := range p.visitedNamespaces {
		for _, m := range namespacedRESTMappings {
			// list namespace下的所有resource类型的资源
			// 遍历这个列表
			// 不存在annotations["kubectl.kubernetes.io/last-applied-configuration"]，说明不是kubectl apply创建的资源，则跳过
			// 如果uid是kubectl apply操作的资源的uid，则跳过
			// 如果--dry-run不为client，则执行删除这个资源。否则，不做删除
			// 打印出删除资源（默认输出{resource}.{group}/{name} pruned）
			if err := p.prune(n, m); err != nil {
				return fmt.Errorf("error pruning namespaced object %v: %v", m.GroupVersionKind, err)
			}
		}
	}
	for _, m := range nonNamespacedRESTMappings {
		if err := p.prune(metav1.NamespaceNone, m); err != nil {
			return fmt.Errorf("error pruning nonNamespaced object %v: %v", m.GroupVersionKind, err)
		}
	}

	return nil
}

// list namespace下的所有resource类型的资源
// 遍历这个列表
// 不存在annotations["kubectl.kubernetes.io/last-applied-configuration"]，说明不是kubectl apply创建的资源，则跳过
// 如果uid是kubectl apply操作的资源的uid，则跳过
// 如果--dry-run不为client，则执行删除这个资源。否则，不做删除
// 打印出删除资源（默认输出{resource}.{group}/{name} pruned）
func (p *pruner) prune(namespace string, mapping *meta.RESTMapping) error {
	// namespace下所有的resource类型的资源
	objList, err := p.dynamicClient.Resource(mapping.Resource).
		Namespace(namespace).
		List(context.TODO(), metav1.ListOptions{
			LabelSelector: p.labelSelector,
			FieldSelector: p.fieldSelector,
		})
	if err != nil {
		return err
	}

	objs, err := meta.ExtractList(objList)
	if err != nil {
		return err
	}

	for _, obj := range objs {
		metadata, err := meta.Accessor(obj)
		if err != nil {
			return err
		}
		annots := metadata.GetAnnotations()
		// 不存在annotations["kubectl.kubernetes.io/last-applied-configuration"]，说明不是kubectl apply创建的资源，则跳过
		if _, ok := annots[corev1.LastAppliedConfigAnnotation]; !ok {
			// don't prune resources not created with apply
			continue
		}
		uid := metadata.GetUID()
		// 如果uid是kubectl apply操作的资源的uid，则跳过
		if p.visitedUids.Has(string(uid)) {
			continue
		}
		name := metadata.GetName()
		// --dry-run不为client（可能为none、server，或空）
		if p.dryRunStrategy != cmdutil.DryRunClient {
			// 执行资源删除
			if err := p.delete(namespace, name, mapping); err != nil {
				return err
			}
		}

		printer, err := p.toPrinter("pruned")
		if err != nil {
			return err
		}
		// 默认输出{resource}.{group}/{name} pruned
		printer.PrintObj(obj, p.out)
	}
	return nil
}

func (p *pruner) delete(namespace, name string, mapping *meta.RESTMapping) error {
	return runDelete(namespace, name, mapping, p.dynamicClient, p.cascadingStrategy, p.gracePeriod, p.dryRunStrategy == cmdutil.DryRunServer)
}

func runDelete(namespace, name string, mapping *meta.RESTMapping, c dynamic.Interface, cascadingStrategy metav1.DeletionPropagation, gracePeriod int, serverDryRun bool) error {
	// 设置metav1.DeleteOptions里的GracePeriodSeconds（当gracePeriod大于0）
	// 设置metav1.DeleteOptions里的PropagationPolicy为cascadingStrategy
	options := asDeleteOptions(cascadingStrategy, gracePeriod)
	if serverDryRun {
		options.DryRun = []string{metav1.DryRunAll}
	}
	return c.Resource(mapping.Resource).Namespace(namespace).Delete(context.TODO(), name, options)
}

// 设置metav1.DeleteOptions里的GracePeriodSeconds（当gracePeriod大于0）
// 设置metav1.DeleteOptions里的PropagationPolicy
func asDeleteOptions(cascadingStrategy metav1.DeletionPropagation, gracePeriod int) metav1.DeleteOptions {
	options := metav1.DeleteOptions{}
	if gracePeriod >= 0 {
		options = *metav1.NewDeleteOptions(int64(gracePeriod))
	}
	options.PropagationPolicy = &cascadingStrategy
	return options
}

type pruneResource struct {
	group      string
	version    string
	kind       string
	namespaced bool
}

func (pr pruneResource) String() string {
	return fmt.Sprintf("%v/%v, Kind=%v, Namespaced=%v", pr.group, pr.version, pr.kind, pr.namespaced)
}

// 如果pruneResources为空，则pruneResources使用默认的资源类型列表（ConfigMap、Endpoints、Namespace、PersistentVolumeClaim、PersistentVolume、Pod、ReplicationController、Secret、Service、Job、CronJob、Ingress、DaemonSet、Deployment、ReplicaSet、StatefulSet）
// 遍历pruneResources，通过mapper获得这个resource对应的meta.RESTMapping
// 如果resource是namespaced（命名空间的），则对应的meta.RESTMapping添加到namespaced，否则添加到nonNamespaced
func getRESTMappings(mapper meta.RESTMapper, pruneResources *[]pruneResource) (namespaced, nonNamespaced []*meta.RESTMapping, err error) {
	if len(*pruneResources) == 0 {
		// default allowlist
		*pruneResources = []pruneResource{
			{"", "v1", "ConfigMap", true},
			{"", "v1", "Endpoints", true},
			{"", "v1", "Namespace", false},
			{"", "v1", "PersistentVolumeClaim", true},
			{"", "v1", "PersistentVolume", false},
			{"", "v1", "Pod", true},
			{"", "v1", "ReplicationController", true},
			{"", "v1", "Secret", true},
			{"", "v1", "Service", true},
			{"batch", "v1", "Job", true},
			{"batch", "v1", "CronJob", true},
			{"networking.k8s.io", "v1", "Ingress", true},
			{"apps", "v1", "DaemonSet", true},
			{"apps", "v1", "Deployment", true},
			{"apps", "v1", "ReplicaSet", true},
			{"apps", "v1", "StatefulSet", true},
		}
	}

	for _, resource := range *pruneResources {
		// 获得这个resource的meta.RESTMapping
		addedMapping, err := mapper.RESTMapping(schema.GroupKind{Group: resource.group, Kind: resource.kind}, resource.version)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid resource %v: %v", resource, err)
		}
		if resource.namespaced {
			namespaced = append(namespaced, addedMapping)
		} else {
			nonNamespaced = append(nonNamespaced, addedMapping)
		}
	}

	return namespaced, nonNamespaced, nil
}

// gvks支持的格式为"{group}/{version}/{kind}"，并从apiserver中（meta.RESTMapper）进行获取（验证）相关的信息，返回[]pruneResource（group、version、kind、scope）
func parsePruneResources(mapper meta.RESTMapper, gvks []string) ([]pruneResource, error) {
	pruneResources := []pruneResource{}
	for _, groupVersionKind := range gvks {
		gvk := strings.Split(groupVersionKind, "/")
		if len(gvk) != 3 {
			return nil, fmt.Errorf("invalid GroupVersionKind format: %v, please follow <group/version/kind>", groupVersionKind)
		}

		if gvk[0] == "core" {
			gvk[0] = ""
		}
		// PriorityRESTMapper（staging\src\k8s.io\apimachinery\pkg\api\meta\priority.go）
		// 先从调用m.Delegate.RESTMappings，获得[]*RESTMapping
		// 优先匹配用户提供的version和group
		mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gvk[0], Kind: gvk[2]}, gvk[1])
		if err != nil {
			return pruneResources, err
		}
		var namespaced bool
		namespaceScope := mapping.Scope.Name()
		switch namespaceScope {
		case meta.RESTScopeNameNamespace:
			namespaced = true
		case meta.RESTScopeNameRoot:
			namespaced = false
		default:
			return pruneResources, fmt.Errorf("Unknown namespace scope: %q", namespaceScope)
		}

		pruneResources = append(pruneResources, pruneResource{gvk[0], gvk[1], gvk[2], namespaced})
	}
	return pruneResources, nil
}
