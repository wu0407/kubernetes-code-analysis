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

package validation

import (
	"reflect"
	"sort"

	"k8s.io/kube-openapi/pkg/util/proto"
)

type validationItem interface {
	proto.SchemaVisitor

	Errors() []error
	Path() *proto.Path
}

type baseItem struct {
	errors errors
	path   proto.Path
}

// Errors returns the list of errors found for this item.
func (item *baseItem) Errors() []error {
	return item.errors.Errors()
}

// AddValidationError wraps the given error into a ValidationError and
// attaches it to this item.
func (item *baseItem) AddValidationError(err error) {
	item.errors.AppendErrors(ValidationError{Path: item.path.String(), Err: err})
}

// AddError adds a regular (non-validation related) error to the list.
func (item *baseItem) AddError(err error) {
	item.errors.AppendErrors(err)
}

// CopyErrors adds a list of errors to this item. This is useful to copy
// errors from subitems.
func (item *baseItem) CopyErrors(errs []error) {
	item.errors.AppendErrors(errs...)
}

// Path returns the path of this item, helps print useful errors.
func (item *baseItem) Path() *proto.Path {
	return &item.path
}

// mapItem represents a map entry in the yaml.
type mapItem struct {
	baseItem

	Map map[string]interface{}
}

// 对item.Map进行排序，返回排序后的key列表
func (item *mapItem) sortedKeys() []string {
	sortedKeys := []string{}
	for key := range item.Map {
		sortedKeys = append(sortedKeys, key)
	}
	sort.Strings(sortedKeys)
	return sortedKeys
}

var _ validationItem = &mapItem{}

func (item *mapItem) VisitPrimitive(schema *proto.Primitive) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: schema.Type, Actual: "map"})
}

func (item *mapItem) VisitArray(schema *proto.Array) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: "array", Actual: "map"})
}

// 这个由schema.Accept(SchemaVisitor)调用
// 其中schema为proto.Map，SchemaVisitor为mapItem
// 对item.Map进行排序，遍历排序后的key列表
// value对应的validationItem接口的实现（如果不支持的类型，设置错误，跳过这个key）
// 调用元素的Schema.Accept(subItem（元素的validationItem接口的实现）)
// 拷贝元素的错误到父对象mapItem
func (item *mapItem) VisitMap(schema *proto.Map) {
	// 对item.Map进行排序，返回排序后的key列表
	for _, key := range item.sortedKeys() {
		// item.Path().FieldPath(key)生成一个FieldPath
		// value对应的validationItem接口的实现（如果不支持的类型，设置错误）
		subItem, err := itemFactory(item.Path().FieldPath(key), item.Map[key])
		if err != nil {
			item.AddError(err)
			continue
		}
		// 调用元素的Schema.Accept(subItem（元素的validationItem接口的实现）)
		schema.SubType.Accept(subItem)
		item.CopyErrors(subItem.Errors())
	}
}

// 这个由schema.Accept(SchemaVisitor)调用
// 其中schema为proto.Kind（结构体），SchemaVisitor为mapItem
// 对item.Map进行排序，遍历排序后的key列表
//   如果value为nil，则跳过这个key
//   获得value对应的validationItem接口的实现（如果不支持的类型，设置错误，跳过这个key）
//   检验字段是否存在schema，如果没有这个字段Field的Schema，则设置错误，跳过这个key
//   调用字段Field的Schema.Accept(subItem（字段的validationItem接口的实现）)，拷贝错误到父对象mapItem
// 检验必须设置的字段
// 如果有必须字段不存在或字段值为nil，则设置错误
func (item *mapItem) VisitKind(schema *proto.Kind) {
	// Verify each sub-field.
	// 对item.Map进行排序，返回排序后的key列表
	for _, key := range item.sortedKeys() {
		// 如果value为nil，则跳过这个key
		if item.Map[key] == nil {
			continue
		}
		// item.Path().FieldPath(key)生成一个FieldPath
		// value对应的validationItem接口的实现（如果不支持的类型，设置错误）
		subItem, err := itemFactory(item.Path().FieldPath(key), item.Map[key])
		if err != nil {
			item.AddError(err)
			continue
		}
		// 没有这个字段Field的Schema，则设置错误
		if _, ok := schema.Fields[key]; !ok {
			item.AddValidationError(UnknownFieldError{Path: schema.GetPath().String(), Field: key})
			continue
		}
		// 调用字段Field的Schema.Accept(subItem（字段的validationItem接口的实现）)
		schema.Fields[key].Accept(subItem)
		item.CopyErrors(subItem.Errors())
	}

	// Verify that all required fields are present.
	// 检验必须设置的字段
	// 如果有必须字段不存在或字段值为nil，则设置错误
	for _, required := range schema.RequiredFields {
		if v, ok := item.Map[required]; !ok || v == nil {
			item.AddValidationError(MissingRequiredFieldError{Path: schema.GetPath().String(), Field: required})
		}
	}
}

func (item *mapItem) VisitArbitrary(schema *proto.Arbitrary) {
}

// 这个由schema.Accept(SchemaVisitor)调用
// 其中schema为proto.Reference，SchemaVisitor为mapItem
func (item *mapItem) VisitReference(schema proto.Reference) {
	// passthrough
	// schema.SubSchema()只能是proto.Map（合法）
	schema.SubSchema().Accept(item)
}

// arrayItem represents a yaml array.
type arrayItem struct {
	baseItem

	Array []interface{}
}

var _ validationItem = &arrayItem{}

func (item *arrayItem) VisitPrimitive(schema *proto.Primitive) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: schema.Type, Actual: "array"})
}

// 这个由schema.Accept(SchemaVisitor)调用
// 其中schema为proto.Array，SchemaVisitor为arrayItem
// 遍历item里所有的元素，如果元素为nil，则设置错误
// 否则，对每个元素执行itemFactory获得元素对应的validationItem接口的实现（如果不支持的类型，设置错误，跳过这个元素）
// 再执行元素类型schema.Accept(SchemaVisitor)，拷贝error到item里（检验有任何问题，反馈到父对象中）
func (item *arrayItem) VisitArray(schema *proto.Array) {
	for i, v := range item.Array {
		// 在item.Path()生成新的path
		path := item.Path().ArrayPath(i)
		// 元素为nil，则设置错误
		if v == nil {
			item.AddValidationError(InvalidObjectTypeError{Type: "nil", Path: path.String()})
			continue
		}
		// 元素对应的validationItem接口的实现
		subItem, err := itemFactory(path, v)
		if err != nil {
			item.AddError(err)
			continue
		}
		// 元素类型schema调用Accept
		schema.SubType.Accept(subItem)
		item.CopyErrors(subItem.Errors())
	}
}

func (item *arrayItem) VisitMap(schema *proto.Map) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: "map", Actual: "array"})
}

func (item *arrayItem) VisitKind(schema *proto.Kind) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: "map", Actual: "array"})
}

func (item *arrayItem) VisitArbitrary(schema *proto.Arbitrary) {
}

// 这个由schema.Accept(SchemaVisitor)调用
// 其中schema为proto.Reference，SchemaVisitor为arrayItem
func (item *arrayItem) VisitReference(schema proto.Reference) {
	// passthrough
	// schema.SubSchema()只能是proto.Array（合法）
	schema.SubSchema().Accept(item)
}

// primitiveItem represents a yaml value.
type primitiveItem struct {
	baseItem

	Value interface{}
	Kind  string
}

var _ validationItem = &primitiveItem{}

// 合法的情况：
// item和schema都是bool
// schema是"integer"，而item类型是"integer"或"number"
// schema是"integer"，而item类型为"integer"或"number"
// schema是"string"，而item可以是其他任意的primitives（bool型、各种整形、各种浮点型、字符串）
func (item *primitiveItem) VisitPrimitive(schema *proto.Primitive) {
	// Some types of primitives can match more than one (a number
	// can be a string, but not the other way around). Return from
	// the switch if we have a valid possible type conversion
	// NOTE(apelisse): This logic is blindly copied from the
	// existing swagger logic, and I'm not sure I agree with it.
	switch schema.Type {
	case proto.Boolean:
		switch item.Kind {
		// item和schema都是bool，合法
		case proto.Boolean:
			return
		}
	case proto.Integer:
		switch item.Kind {
		// schema是"integer"，而item类型是"integer"或"number"
		case proto.Integer, proto.Number:
			return
		}
	case proto.Number:
		switch item.Kind {
		// schema是"integer"，而item类型为"integer"或"number"
		case proto.Integer, proto.Number:
			return
		}
	case proto.String:
		// schema是"string"，而item可以是其他任意的primitives（bool型、各种整形、各种浮点型、字符串）
		return
	}
	// TODO(wrong): this misses "null"

	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: schema.Type, Actual: item.Kind})
}

// 这个由schema.Accept(SchemaVisitor)调用
// 设置错误，primitiveItem只能是schema.Accept（schema为Primitive 在vendor\k8s.io\kube-openapi\pkg\util\proto\openapi.go）实现里只能调VisitPrimitive，或schema为Ref在vendor\k8s.io\kube-openapi\pkg\util\proto\document.go，调VisitReference）
func (item *primitiveItem) VisitArray(schema *proto.Array) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: "array", Actual: item.Kind})
}

// 同上
func (item *primitiveItem) VisitMap(schema *proto.Map) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: "map", Actual: item.Kind})
}

// 同上
func (item *primitiveItem) VisitKind(schema *proto.Kind) {
	item.AddValidationError(InvalidTypeError{Path: schema.GetPath().String(), Expected: "map", Actual: item.Kind})
}

// 当前kubernetes没有schema是proto.Arbitrary，所以这里没有实现
func (item *primitiveItem) VisitArbitrary(schema *proto.Arbitrary) {
}

// 这个由schema.Accept(SchemaVisitor)调用
// 设置错误，primitiveItem只能是schema.Accept（schema为Primitive 在vendor\k8s.io\kube-openapi\pkg\util\proto\openapi.go）实现里只能调VisitPrimitive，或schema为Ref在vendor\k8s.io\kube-openapi\pkg\util\proto\document.go，调VisitReference）
func (item *primitiveItem) VisitReference(schema proto.Reference) {
	// passthrough
	// schema.SubSchema()只能是proto.Primitive（合法）
	schema.SubSchema().Accept(item)
}

// itemFactory creates the relevant item type/visitor based on the current yaml type.
// 基于v的类型返回，对应的validationItem接口的实现
// 支持类型为bool型、各种整形、各种浮点型、字符串、数组和切片、map
func itemFactory(path proto.Path, v interface{}) (validationItem, error) {
	// We need to special case for no-type fields in yaml (e.g. empty item in list)
	if v == nil {
		return nil, InvalidObjectTypeError{Type: "nil", Path: path.String()}
	}
	kind := reflect.TypeOf(v).Kind()
	switch kind {
	// bool型
	case reflect.Bool:
		return &primitiveItem{
			baseItem: baseItem{path: path},
			Value:    v,
			Kind:     proto.Boolean,
		}, nil
	// 各种整形
	case reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64,
		reflect.Uint,
		reflect.Uint8,
		reflect.Uint16,
		reflect.Uint32,
		reflect.Uint64:
		return &primitiveItem{
			baseItem: baseItem{path: path},
			Value:    v,
			Kind:     proto.Integer,
		}, nil
	// 浮点型
	case reflect.Float32,
		reflect.Float64:
		return &primitiveItem{
			baseItem: baseItem{path: path},
			Value:    v,
			Kind:     proto.Number,
		}, nil
	// 字符串
	case reflect.String:
		return &primitiveItem{
			baseItem: baseItem{path: path},
			Value:    v,
			Kind:     proto.String,
		}, nil
	// 数组和切片
	case reflect.Array,
		reflect.Slice:
		return &arrayItem{
			baseItem: baseItem{path: path},
			Array:    v.([]interface{}),
		}, nil
	// map
	case reflect.Map:
		return &mapItem{
			baseItem: baseItem{path: path},
			Map:      v.(map[string]interface{}),
		}, nil
	}
	return nil, InvalidObjectTypeError{Type: kind.String(), Path: path.String()}
}
