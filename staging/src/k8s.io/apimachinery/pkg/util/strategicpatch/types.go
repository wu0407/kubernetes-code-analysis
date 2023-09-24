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

package strategicpatch

import (
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/util/mergepatch"
	openapi "k8s.io/kube-openapi/pkg/util/proto"
)

const (
	patchStrategyOpenapiextensionKey = "x-kubernetes-patch-strategy"
	patchMergeKeyOpenapiextensionKey = "x-kubernetes-patch-merge-key"
)

type LookupPatchItem interface {
	openapi.SchemaVisitor

	Error() error
	Path() *openapi.Path
}

type kindItem struct {
	key          string
	path         *openapi.Path
	err          error
	patchmeta    PatchMeta
	subschema    openapi.Schema
	hasVisitKind bool
}

func NewKindItem(key string, path *openapi.Path) *kindItem {
	return &kindItem{
		key:  key,
		path: path,
	}
}

var _ LookupPatchItem = &kindItem{}

func (item *kindItem) Error() error {
	return item.err
}

func (item *kindItem) Path() *openapi.Path {
	return item.path
}

func (item *kindItem) VisitPrimitive(schema *openapi.Primitive) {
	item.err = errors.New("expected kind, but got primitive")
}

func (item *kindItem) VisitArray(schema *openapi.Array) {
	item.err = errors.New("expected kind, but got slice")
}

func (item *kindItem) VisitMap(schema *openapi.Map) {
	item.err = errors.New("expected kind, but got map")
}

// 获得schema.reference（引用的schema）对应的Schema
// 根据引用的Schema，调用(item *kindItem) Visitxxx
func (item *kindItem) VisitReference(schema openapi.Reference) {
	if !item.hasVisitKind {
		// schema.SubSchema()
		// 获得schema.reference（引用的schema）对应的Schema
		// 根据引用的Schema，调用(item *kindItem) Visitxxx
		schema.SubSchema().Accept(item)
	}
}

// 从schema中查找item.key字段对应的openapi.Schema，设置为item.subschema字段
// 获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，设置为item.patchmeta
func (item *kindItem) VisitKind(schema *openapi.Kind) {
	// key字段对应的openapi.Schema
	subschema, ok := schema.Fields[item.key]
	if !ok {
		// 没有找到对应的openapi.Schema，设置item.err
		item.err = FieldNotFoundError{Path: schema.GetPath().String(), Field: item.key}
		return
	}

	// 根据key字段对应的openapi.Schema的Extensions字段
	// 然后获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"
	mergeKey, patchStrategies, err := parsePatchMetadata(subschema.GetExtensions())
	if err != nil {
		item.err = err
		return
	}
	item.patchmeta = PatchMeta{
		patchStrategies: patchStrategies,
		patchMergeKey:   mergeKey,
	}
	item.subschema = subschema
}

type sliceItem struct {
	key          string
	path         *openapi.Path
	err          error
	patchmeta    PatchMeta
	subschema    openapi.Schema
	hasVisitKind bool
}

func NewSliceItem(key string, path *openapi.Path) *sliceItem {
	return &sliceItem{
		key:  key,
		path: path,
	}
}

var _ LookupPatchItem = &sliceItem{}

func (item *sliceItem) Error() error {
	return item.err
}

func (item *sliceItem) Path() *openapi.Path {
	return item.path
}

func (item *sliceItem) VisitPrimitive(schema *openapi.Primitive) {
	item.err = errors.New("expected slice, but got primitive")
}

// 直接调用，设置item.err
// 通过(item *sliceItem) VisitKind调用过来，则设置item.subschema为schema.SubType
func (item *sliceItem) VisitArray(schema *openapi.Array) {
	// 直接调用，设置item.err
	if !item.hasVisitKind {
		item.err = errors.New("expected visit kind first, then visit array")
	}
	// item.hasVisitKind为true（即已经通过访问(item *sliceItem) VisitKind调用过来），则设置item.subschema
	subschema := schema.SubType
	item.subschema = subschema
}

func (item *sliceItem) VisitMap(schema *openapi.Map) {
	item.err = errors.New("expected slice, but got map")
}

// 直接调用的情况，获得schema.reference（引用的schema）对应的Schema。根据引用的Schema，调用(item *sliceItem) Visitxxx
// 通过访问(item *sliceItem) VisitKind调用过来，则设置item.subschema为schema.reference（引用的schema）
func (item *sliceItem) VisitReference(schema openapi.Reference) {
	// 直接调用
	if !item.hasVisitKind {
		// 获得schema.reference（引用的schema）对应的Schema
		// 根据引用的Schema，调用(item *sliceItem) Visitxxx
		schema.SubSchema().Accept(item)
	} else {
		// item.hasVisitKind为true（即已经通过访问(item *sliceItem) VisitKind调用过来）
		// 则设置item.subschema为schema.reference（引用的schema）
		item.subschema = schema.SubSchema()
	}
}

// 从schema中查找item.key字段对应Schema
// 从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成PatchMeta，并设置为item.patchmeta
// 设置item.hasVisitKind为true（标记后面调用Visitxxx是通过VisitKind调用过来的）
// 根据item.key字段的Schema，调用(item *sliceItem) Visitxxx
func (item *sliceItem) VisitKind(schema *openapi.Kind) {
	// 从schema中查找item.key字段对应Schema
	subschema, ok := schema.Fields[item.key]
	// 没有找到，则设置item.err为FieldNotFoundError
	if !ok {
		item.err = FieldNotFoundError{Path: schema.GetPath().String(), Field: item.key}
		return
	}

	// 获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"
	mergeKey, patchStrategies, err := parsePatchMetadata(subschema.GetExtensions())
	if err != nil {
		item.err = err
		return
	}
	item.patchmeta = PatchMeta{
		patchStrategies: patchStrategies,
		patchMergeKey:   mergeKey,
	}
	item.hasVisitKind = true
	// 根据item.key字段的Schema，调用(item *sliceItem) Visitxxx
	subschema.Accept(item)
}

// 获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"
func parsePatchMetadata(extensions map[string]interface{}) (string, []string, error) {
	// 获得extensions里的"x-kubernetes-patch-strategy"
	// 比如"x-kubernetes-patch-strategy": "retainKeys",
	ps, foundPS := extensions[patchStrategyOpenapiextensionKey]
	var patchStrategies []string
	var mergeKey, patchStrategy string
	var ok bool
	// 存在"x-kubernetes-patch-strategy"字段
	if foundPS {
		// 值是字符串，则按照","进行分割。否则返回错误
		patchStrategy, ok = ps.(string)
		if ok {
			patchStrategies = strings.Split(patchStrategy, ",")
		} else {
			return "", nil, mergepatch.ErrBadArgType(patchStrategy, ps)
		}
	}
	// 获得extensions里的"x-kubernetes-patch-merge-key"的值
	mk, foundMK := extensions[patchMergeKeyOpenapiextensionKey]
	if foundMK {
		mergeKey, ok = mk.(string)
		// 存在且不是string类型，则返回错误
		if !ok {
			return "", nil, mergepatch.ErrBadArgType(mergeKey, mk)
		}
	}
	return mergeKey, patchStrategies, nil
}
