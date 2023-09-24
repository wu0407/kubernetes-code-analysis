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
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/jonboulle/clockwork"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/jsonmergepatch"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/resource"
	oapi "k8s.io/kube-openapi/pkg/util/proto"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/openapi"
)

const (
	// maxPatchRetry is the maximum number of conflicts retry for during a patch operation before returning failure
	maxPatchRetry = 5
	// backOffPeriod is the period to back off when apply patch results in error.
	backOffPeriod = 1 * time.Second
	// how many times we can retry before back off
	triesBeforeBackOff = 1
)

// Patcher defines options to patch OpenAPI objects.
type Patcher struct {
	Mapping *meta.RESTMapping
	Helper  *resource.Helper

	Overwrite bool
	BackOff   clockwork.Clock

	Force             bool
	CascadingStrategy metav1.DeletionPropagation
	Timeout           time.Duration
	GracePeriod       int

	// If set, forces the patch against a specific resourceVersion
	ResourceVersion *string

	// Number of retries to make if the patch fails with conflict
	Retries int

	OpenapiSchema openapi.Resources
}

func newPatcher(o *ApplyOptions, info *resource.Info, helper *resource.Helper) (*Patcher, error) {
	var openapiSchema openapi.Resources
	// 默认就是true
	if o.OpenAPIPatch {
		openapiSchema = o.OpenAPISchema
	}

	return &Patcher{
		Mapping:           info.Mapping,
		Helper:            helper,
		Overwrite:         o.Overwrite,
		BackOff:           clockwork.NewRealClock(),
		Force:             o.DeleteOptions.ForceDeletion,
		CascadingStrategy: o.DeleteOptions.CascadingStrategy,
		Timeout:           o.DeleteOptions.Timeout,
		GracePeriod:       o.DeleteOptions.GracePeriod,
		OpenapiSchema:     openapiSchema,
		Retries:           maxPatchRetry,
	}, nil
}

func (p *Patcher) delete(namespace, name string) error {
	// 设置metav1.DeleteOptions里的GracePeriodSeconds（当gracePeriod大于0）
	// 设置metav1.DeleteOptions里的PropagationPolicy
	options := asDeleteOptions(p.CascadingStrategy, p.GracePeriod)
	// 执行删除
	_, err := p.Helper.DeleteWithOptions(namespace, name, &options)
	return err
}

// crd资源和aggregate apiserver资源，使用json merge patch
// 内置资源使用strategic merge patch
// 移除的字段（值为null），只看以前文件和当文件内容的patch
// 增加和更新字段，只看apiserver和当前的文件内容的patch
// crd资源和aggregate apiservewr资源，同时会判断是否满足preconditions，即不能对"apiVersion"、"kind"、"metadata.name"进行修改
// 如果存在openapi接口（"/openapi/v2"），内置资源会使用进行strategic merge patch（使用openapi信息查找merge path策略），否则使用结构体里字段的json tag信息获取字段，并从tag获取"patchStrategy"和"patchMergeKey"值，查找merge path策略，进行strategic merge patch
func (p *Patcher) patchSimple(obj runtime.Object, modified []byte, source, namespace, name string, errOut io.Writer) ([]byte, runtime.Object, error) {
	// Serialize the current configuration of the object from the server.
	current, err := runtime.Encode(unstructured.UnstructuredJSONScheme, obj)
	if err != nil {
		return nil, nil, cmdutil.AddSourceToErr(fmt.Sprintf("serializing current configuration from:\n%v\nfor:", obj), source, err)
	}

	// Retrieve the original configuration of the object from the annotation.
	// 返回obj的annotation["kubectl.kubernetes.io/last-applied-configuration"]的[]byte
	// 也就是老的obj，这里就是apiserver中的annotation["kubectl.kubernetes.io/last-applied-configuration"]的[]byte（上次apply文件中的内容）
	original, err := util.GetOriginalConfiguration(obj)
	if err != nil {
		return nil, nil, cmdutil.AddSourceToErr(fmt.Sprintf("retrieving original configuration from:\n%v\nfor:", obj), source, err)
	}

	var patchType types.PatchType
	var patch []byte
	var lookupPatchMeta strategicpatch.LookupPatchMeta
	var schema oapi.Schema
	createPatchErrFormat := "creating patch with:\noriginal:\n%s\nmodified:\n%s\ncurrent:\n%s\nfor:"

	// Create the versioned struct from the type defined in the restmapping
	// (which is the API version we'll be submitting the patch to)
	versionedObject, err := scheme.Scheme.New(p.Mapping.GroupVersionKind)
	switch {
	// 对象类型没有注册到scheme.Scheme中，使用json merge patch
	// aggregate apiserver资源或crd资源不在scheme.Scheme中，即crd资源和aggregate apiserver资源使用json merge patch（apiserver支持使用json patch或json merge patch）
	case runtime.IsNotRegisteredError(err):
		// fall back to generic JSON merge patch
		patchType = types.MergePatchType
		// mergepatch.RequireKeyUnchanged
		// 返回函数，key在patch中，key的值被改变，返回false；key不在patch中，key的值没有被改变，返回true
		// 即"apiVersion"是否在patch中，"kind"是否在patch中，"name"是否在patch中
		preconditions := []mergepatch.PreconditionFunc{mergepatch.RequireKeyUnchanged("apiVersion"),
			mergepatch.RequireKeyUnchanged("kind"), mergepatch.RequireMetadataKeyUnchanged("name")}
		// origin是以前的文件内容、modified是现在文件内容（加上现在文件内容的annotation["kubectl.kubernetes.io/last-applied-configuration"]）、current为apiserver中的内容
		// 移除的字段（值为null），只看以前文件和当文件内容的patch
		// 增加和更新字段，只看apiserver和当前的文件内容的patch
		// 同时会判断是否满足preconditions，即不能对"apiVersion"、"kind"、"metadata.name"进行修改
		patch, err = jsonmergepatch.CreateThreeWayJSONMergePatch(original, modified, current, preconditions...)
		if err != nil {
			if mergepatch.IsPreconditionFailed(err) {
				return nil, nil, fmt.Errorf("%s", "At least one of apiVersion, kind and name was changed")
			}
			return nil, nil, cmdutil.AddSourceToErr(fmt.Sprintf(createPatchErrFormat, original, modified, current), source, err)
		}
	case err != nil:
		return nil, nil, cmdutil.AddSourceToErr(fmt.Sprintf("getting instance of versioned object for %v:", p.Mapping.GroupVersionKind), source, err)
	case err == nil:
		// 内置资源，非crd资源和aggregate apiserver资源
		// Compute a three way strategic merge patch to send to server.
		patchType = types.StrategicMergePatchType

		// Try to use openapi first if the openapi spec is available and can successfully calculate the patch.
		// Otherwise, fall back to baked-in types.
		// apiserver有openapi接口（"/openapi/v2"）(默认有这个接口)
		if p.OpenapiSchema != nil {
			// 根据schema.GroupVersionKind查找对应的proto.Schema
			if schema = p.OpenapiSchema.LookupResource(p.Mapping.GroupVersionKind); schema != nil {
				lookupPatchMeta = strategicpatch.PatchMetaFromOpenAPI{Schema: schema}
				// 对于kubectl apply（client apply）
				// origin是以前的文件内容、modified是现在文件内容（加上现在文件内容的annotation["kubectl.kubernetes.io/last-applied-configuration"]）、current为apiserver中的内容
				// 将original，modified，current进行json.Unmarshal为map[string]interface{}
				// 对currentMap和modifiedMap进行diff，计算出所有增加和更新的Patch
				// 对originalMap和modifiedMap进行diff，计算出所有删除的patch
				// 然后将两个patch进行合并，生成最终的patch
				// 执行fns（这里没有），验证patch是否符合条件
				// 如果overwrite为false，则检验是否存在冲突，现在存在的差异和最终patch存在冲突（两个对同一个key同时进行修改）。
				//   对originalMap和currentMap进行diff，计算出现在存在的差异
				//   对最终的patch和现在存在的差异进行比较，是否存在冲突
				//   存在冲突返回错误
				//   比如original "{k: a}", current为"{k: b}"， modified为"{k: c}"
				//   其中"{k: b}"是非kubectl apply修改的，而现在基于original修改成"{k: c}"，会存在冲突
				// 返回最终的patch（json.marshal转成[]byte）
				if openapiPatch, err := strategicpatch.CreateThreeWayMergePatch(original, modified, current, lookupPatchMeta, p.Overwrite); err != nil {
					fmt.Fprintf(errOut, "warning: error calculating patch from openapi spec: %v\n", err)
				} else {
					patchType = types.StrategicMergePatchType
					patch = openapiPatch
				}
			}
		}

		// 根据openapi信息进行CreateThreeWayMergePatch发生错误，或apiserver没有openapi接口（p.OpenapiSchema为nil）
		if patch == nil {
			lookupPatchMeta, err = strategicpatch.NewPatchMetaFromStruct(versionedObject)
			if err != nil {
				return nil, nil, cmdutil.AddSourceToErr(fmt.Sprintf(createPatchErrFormat, original, modified, current), source, err)
			}
			// 使用结构体里字段的json tag信息获取字段，并从tag获取"patchStrategy"和"patchMergeKey"值，查找merge path策略
			// 重新进行CreateThreeWayMergePatch
			patch, err = strategicpatch.CreateThreeWayMergePatch(original, modified, current, lookupPatchMeta, p.Overwrite)
			if err != nil {
				return nil, nil, cmdutil.AddSourceToErr(fmt.Sprintf(createPatchErrFormat, original, modified, current), source, err)
			}
		}
	}

	// patch为空，说明original, modified, current（都是一样的，可能有一些操作符字段"$setElementOrder"、"$deleteFromPrimitiveList"、"$retainKeys","$patch"不生效）没有实质上的不一样
	if string(patch) == "{}" {
		return patch, obj, nil
	}

	if p.ResourceVersion != nil {
		// patch中添加"ResourceVersion"字段值为rv
		patch, err = addResourceVersion(patch, *p.ResourceVersion)
		if err != nil {
			return nil, nil, cmdutil.AddSourceToErr("Failed to insert resourceVersion in patch", source, err)
		}
	}

	patchedObj, err := p.Helper.Patch(namespace, name, patchType, patch, nil)
	return patch, patchedObj, err
}

// Patch tries to patch an OpenAPI resource. On success, returns the merge patch as well
// the final patched object. On failure, returns an error.
// crd资源和aggregate apiserver资源，使用json merge patch
// 内置资源使用strategic merge patch
// 移除的字段（值为null），只看以前文件和当文件内容的patch
// 增加和更新字段，只看apiserver和当前的文件内容的patch
// crd资源和aggregate apiservewr资源，同时会判断是否满足preconditions，即不能对"apiVersion"、"kind"、"metadata.name"进行修改
// 如果存在openapi接口（"/openapi/v2"），内置资源会使用进行strategic merge patch（使用openapi信息查找merge path策略），否则使用结构体里字段的json tag信息获取字段，并从tag获取"patchStrategy"和"patchMergeKey"值，查找merge path策略，进行strategic merge patch
// 进行patch，如果成功直接返回。如果遇到冲突错误，则进行重试，重试次数为p.Retries（如果为0，则为5）
// 重试次数用完后，还是冲突错误或IsInvalid错误，且命令行设置了--force，则进行删除然后创建
// 先执行删除，然后再执行创建modified，如果执行modified创建失败，则创建original
// 返回modified和创建完后返回的资源
func (p *Patcher) Patch(current runtime.Object, modified []byte, source, namespace, name string, errOut io.Writer) ([]byte, runtime.Object, error) {
	var getErr error
	// crd资源和aggregate apiserver资源，使用json merge patch
	// 内置资源使用strategic merge patch
	// 移除的字段（值为null），只看以前文件和当文件内容的patch
	// 增加和更新字段，只看apiserver和当前的文件内容的patch
	// crd资源和aggregate apiservewr资源，同时会判断是否满足preconditions，即不能对"apiVersion"、"kind"、"metadata.name"进行修改
	// 如果存在openapi接口（"/openapi/v2"），内置资源会使用进行strategic merge patch（使用openapi信息查找merge path策略），否则使用结构体里字段的json tag信息获取字段，并从tag获取"patchStrategy"和"patchMergeKey"值，查找merge path策略，进行strategic merge patch
	patchBytes, patchObject, err := p.patchSimple(current, modified, source, namespace, name, errOut)
	if p.Retries == 0 {
		p.Retries = maxPatchRetry
	}
	for i := 1; i <= p.Retries && errors.IsConflict(err); i++ {
		if i > triesBeforeBackOff {
			p.BackOff.Sleep(backOffPeriod)
		}
		current, getErr = p.Helper.Get(namespace, name)
		if getErr != nil {
			return nil, nil, getErr
		}
		patchBytes, patchObject, err = p.patchSimple(current, modified, source, namespace, name, errOut)
	}
	// 重试次数用完后，还是冲突错误或IsInvalid错误，且命令行设置了--force，则进行删除然后创建
	if err != nil && (errors.IsConflict(err) || errors.IsInvalid(err)) && p.Force {
		// 先执行删除，然后再执行创建modified，如果执行modified创建失败，则创建original
		// 返回modified和创建完后返回的资源
		patchBytes, patchObject, err = p.deleteAndCreate(current, modified, namespace, name)
	}
	return patchBytes, patchObject, err
}

// 先执行删除，然后再执行创建modified，如果执行modified创建失败，则创建original
// 返回modified和创建完后返回的资源
func (p *Patcher) deleteAndCreate(original runtime.Object, modified []byte, namespace, name string) ([]byte, runtime.Object, error) {
	// 执行删除
	if err := p.delete(namespace, name); err != nil {
		return modified, nil, err
	}
	// TODO: use wait
	if err := wait.PollImmediate(1*time.Second, p.Timeout, func() (bool, error) {
		if _, err := p.Helper.Get(namespace, name); !errors.IsNotFound(err) {
			return false, err
		}
		return true, nil
	}); err != nil {
		return modified, nil, err
	}
	versionedObject, _, err := unstructured.UnstructuredJSONScheme.Decode(modified, nil, nil)
	if err != nil {
		return modified, nil, err
	}
	// 执行modified创建
	createdObject, err := p.Helper.Create(namespace, true, versionedObject)
	if err != nil {
		// restore the original object if we fail to create the new one
		// but still propagate and advertise error to user
		// 执行modified创建失败，则创建original
		recreated, recreateErr := p.Helper.Create(namespace, true, original)
		if recreateErr != nil {
			err = fmt.Errorf("An error occurred force-replacing the existing object with the newly provided one:\n\n%v.\n\nAdditionally, an error occurred attempting to restore the original object:\n\n%v", err, recreateErr)
		} else {
			createdObject = recreated
		}
	}
	return modified, createdObject, err
}

// patch中添加"ResourceVersion"字段值为rv
func addResourceVersion(patch []byte, rv string) ([]byte, error) {
	var patchMap map[string]interface{}
	err := json.Unmarshal(patch, &patchMap)
	if err != nil {
		return nil, err
	}
	u := unstructured.Unstructured{Object: patchMap}
	a, err := meta.Accessor(&u)
	if err != nil {
		return nil, err
	}
	a.SetResourceVersion(rv)

	return json.Marshal(patchMap)
}
