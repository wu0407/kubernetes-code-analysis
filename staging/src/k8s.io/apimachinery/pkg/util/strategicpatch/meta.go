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
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/util/mergepatch"
	forkedjson "k8s.io/apimachinery/third_party/forked/golang/json"
	openapi "k8s.io/kube-openapi/pkg/util/proto"
)

type PatchMeta struct {
	patchStrategies []string
	patchMergeKey   string
}

func (pm *PatchMeta) GetPatchStrategies() []string {
	if pm.patchStrategies == nil {
		return []string{}
	}
	return pm.patchStrategies
}

func (pm *PatchMeta) SetPatchStrategies(ps []string) {
	pm.patchStrategies = ps
}

func (pm *PatchMeta) GetPatchMergeKey() string {
	return pm.patchMergeKey
}

func (pm *PatchMeta) SetPatchMergeKey(pmk string) {
	pm.patchMergeKey = pmk
}

type LookupPatchMeta interface {
	// LookupPatchMetadataForStruct gets subschema and the patch metadata (e.g. patch strategy and merge key) for map.
	LookupPatchMetadataForStruct(key string) (LookupPatchMeta, PatchMeta, error)
	// LookupPatchMetadataForSlice get subschema and the patch metadata for slice.
	LookupPatchMetadataForSlice(key string) (LookupPatchMeta, PatchMeta, error)
	// Get the type name of the field
	Name() string
}

type PatchMetaFromStruct struct {
	T reflect.Type
}

func NewPatchMetaFromStruct(dataStruct interface{}) (PatchMetaFromStruct, error) {
	// 返回dataStruct的类型
	t, err := getTagStructType(dataStruct)
	return PatchMetaFromStruct{T: t}, err
}

var _ LookupPatchMeta = PatchMetaFromStruct{}

func (s PatchMetaFromStruct) LookupPatchMetadataForStruct(key string) (LookupPatchMeta, PatchMeta, error) {
	// t的类型或底层类型不是struct，返回错误
	// 从t类型的对应的结构体里查找json tag名字为jsonField字段
	// 获取字段tag里面的"patchStrategy"的值，并按逗号进行分隔
	// 获取字段tag里面"patchMergeKey"的值
	// 返回字段的类型reflect.Type，"patchStrategy"的分隔后的值，"patchMergeKey"的值
	fieldType, fieldPatchStrategies, fieldPatchMergeKey, err := forkedjson.LookupPatchMetadataForStruct(s.T, key)
	if err != nil {
		return nil, PatchMeta{}, err
	}

	return PatchMetaFromStruct{T: fieldType},
		PatchMeta{
			patchStrategies: fieldPatchStrategies,
			patchMergeKey:   fieldPatchMergeKey,
		}, nil
}

func (s PatchMetaFromStruct) LookupPatchMetadataForSlice(key string) (LookupPatchMeta, PatchMeta, error) {
	// t的类型或底层类型不是struct，返回错误
	// 从t类型的对应的结构体里查找json tag名字为jsonField字段
	// 获取字段tag里面的"patchStrategy"的值，并按逗号进行分隔
	// 获取字段tag里面"patchMergeKey"的值
	// 返回字段的类型reflect.Type，"patchStrategy"的分隔后的值，"patchMergeKey"的值
	subschema, patchMeta, err := s.LookupPatchMetadataForStruct(key)
	if err != nil {
		return nil, PatchMeta{}, err
	}
	elemPatchMetaFromStruct := subschema.(PatchMetaFromStruct)
	t := elemPatchMetaFromStruct.T

	var elemType reflect.Type
	switch t.Kind() {
	// If t is an array or a slice, get the element type.
	// If element is still an array or a slice, return an error.
	// Otherwise, return element type.
	// 如果t类型是是slice或array，且元素类型是slice或array，返回错误
	case reflect.Array, reflect.Slice:
		elemType = t.Elem()
		if elemType.Kind() == reflect.Array || elemType.Kind() == reflect.Slice {
			return nil, PatchMeta{}, errors.New("unexpected slice of slice")
		}
	// If t is an pointer, get the underlying element.
	// If the underlying element is neither an array nor a slice, the pointer is pointing to a slice,
	// e.g. https://github.com/kubernetes/kubernetes/blob/bc22e206c79282487ea0bf5696d5ccec7e839a76/staging/src/k8s.io/apimachinery/pkg/util/strategicpatch/patch_test.go#L2782-L2822
	// If the underlying element is either an array or a slice, return its element type.
	// t类型是指针，如果底层类型不是slice或array，则t为底层类型。否则为slice或array的元素类型。
	case reflect.Ptr:
		t = t.Elem()
		if t.Kind() == reflect.Array || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		elemType = t
	default:
		return nil, PatchMeta{}, fmt.Errorf("expected slice or array type, but got: %s", s.T.Kind().String())
	}

	return PatchMetaFromStruct{T: elemType}, patchMeta, nil
}

func (s PatchMetaFromStruct) Name() string {
	return s.T.Kind().String()
}

// 返回dataStruct的类型
func getTagStructType(dataStruct interface{}) (reflect.Type, error) {
	if dataStruct == nil {
		return nil, mergepatch.ErrBadArgKind(struct{}{}, nil)
	}

	t := reflect.TypeOf(dataStruct)
	// Get the underlying type for pointers
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return nil, mergepatch.ErrBadArgKind(struct{}{}, dataStruct)
	}

	return t, nil
}

func GetTagStructTypeOrDie(dataStruct interface{}) reflect.Type {
	t, err := getTagStructType(dataStruct)
	if err != nil {
		panic(err)
	}
	return t
}

type PatchMetaFromOpenAPI struct {
	Schema openapi.Schema
}

func NewPatchMetaFromOpenAPI(s openapi.Schema) PatchMetaFromOpenAPI {
	return PatchMetaFromOpenAPI{Schema: s}
}

var _ LookupPatchMeta = PatchMetaFromOpenAPI{}

// 从s.schema中查找key字段对应的openapi.Schema，设置为返回的第一个字段(PatchMetaFromOpenAPI{Schema: {key字段对应的openapi.Schema}})
// 获得extensions里的"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，设置返回的第二个字段PatchMeta的patchMergeKey和patchStrategies字段
func (s PatchMetaFromOpenAPI) LookupPatchMetadataForStruct(key string) (LookupPatchMeta, PatchMeta, error) {
	if s.Schema == nil {
		return nil, PatchMeta{}, nil
	}
	// 生成&kindItem{
	// 	key:  key,
	// 	path: s.Schema.GetPath(),
	// }
	kindItem := NewKindItem(key, s.Schema.GetPath())
	// 根据s.Schema类型，执行不同的方法
	// s.Schema类型是proto.*Primitive，则调用kindItem.VisitPrimitive(s.Schema)（这里不支持，设置kindItem.err错误）
	// s.Schema类型是proto.*Arbitrary，则调用kindItem.VisitArbitrary(s.Schema), 这里不支持，kindItem并没有实现（bug）
	// s.Schema类型是proto.*Map，则调用kindItem.VisitMap(s.Schema)（这里不支持，设置kindItem.err错误）
	// s.Schema类型是proto.*Kind，则调用kindItem.VisitKind(s.Schema)
	// s.Schema类型是proto.*Array，则调用kindItem.VisitArray(s.Schema)（这里不支持，设置kindItem.err错误）
	// s.Schema类型是proto.*Reference，则调用kindItem.VisitReference(s.Schema)
	s.Schema.Accept(kindItem)

	// 返回kindItem.err字段
	err := kindItem.Error()
	if err != nil {
		return nil, PatchMeta{}, err
	}
	return PatchMetaFromOpenAPI{Schema: kindItem.subschema},
		kindItem.patchmeta, nil
}

// 返回slice的item类型Schema，组装成第一个返回值PatchMetaFromOpenAPI{Schema: sliceItem.subschema}
// s.Schema类型是proto.*Kind，则调用sliceItem.VisitKind(s.Schema)
//     从s.Schema中查找sliceItem.key字段对应Schema
//     从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成第二个返回值PatchMeta
// 否则，第二个参数为PatchMeta{}（空的PatchMeta）
func (s PatchMetaFromOpenAPI) LookupPatchMetadataForSlice(key string) (LookupPatchMeta, PatchMeta, error) {
	if s.Schema == nil {
		return nil, PatchMeta{}, nil
	}
	// 返回&sliceItem{
	// 	key:  key,
	// 	path: s.Schema.GetPath(),
	// }
	sliceItem := NewSliceItem(key, s.Schema.GetPath())
	// 根据s.Schema类型，执行不同的方法
	// s.Schema类型是proto.*Primitive，则调用sliceItem.VisitPrimitive(s.Schema)（这里不支持，设置sliceItem.err错误）
	// s.Schema类型是proto.*Arbitrary，则调用sliceItem.VisitArbitrary(s.Schema), 这里不支持，sliceItem并没有实现（bug）
	// s.Schema类型是proto.*Map，则调用sliceItem.VisitMap(s.Schema)（这里不支持，设置sliceItem.err错误）
	// s.Schema类型是proto.*Kind，则调用sliceItem.VisitKind(s.Schema)
	//     从s.Schema中查找sliceItem.key字段对应Schema
	//     从对应Schema中的extensions获得"x-kubernetes-patch-merge-key"的值和extensions里的"x-kubernetes-patch-strategy"，组装成PatchMeta，并设置为sliceItem.patchmeta
	//     设置sliceItem.hasVisitKind为true（标记后面调用Visitxxx是通过VisitKind调用过来的）
	//     根据sliceItem.key字段的Schema，调用(item *sliceItem) Visitxxx
	// s.Schema类型是proto.*Array，则调用sliceItem.VisitArray(s.Schema)（直接调用不支持，设置sliceItem.err错误。通过sliceItem.VisitKind调用，设置sliceItem.subschema为s.schema.SubType）
	// s.Schema类型是proto.*Reference，则调用sliceItem.VisitReference(s.Schema)
	//     直接调用的情况，获得s.Schema.reference（引用的schema）对应的Schema。根据引用的Schema，调用(item *sliceItem) Visitxxx
	//     通过访问(item *sliceItem) VisitKind调用过来，则设置sliceItem.subschema为schema.reference（引用的schema）
	s.Schema.Accept(sliceItem)

	err := sliceItem.Error()
	if err != nil {
		return nil, PatchMeta{}, err
	}
	return PatchMetaFromOpenAPI{Schema: sliceItem.subschema},
		sliceItem.patchmeta, nil
}

func (s PatchMetaFromOpenAPI) Name() string {
	schema := s.Schema
	return schema.GetName()
}
