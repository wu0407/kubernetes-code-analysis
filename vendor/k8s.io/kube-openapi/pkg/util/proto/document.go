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

package proto

import (
	"fmt"
	"sort"
	"strings"

	openapi_v2 "github.com/googleapis/gnostic/openapiv2"
	"gopkg.in/yaml.v2"
)

func newSchemaError(path *Path, format string, a ...interface{}) error {
	err := fmt.Sprintf(format, a...)
	if path.Len() == 0 {
		return fmt.Errorf("SchemaError: %v", err)
	}
	return fmt.Errorf("SchemaError(%v): %v", path, err)
}

// VendorExtensionToMap converts openapi VendorExtension to a map.
// []*openapi_v2.NamedAny转成map[string]interface{}，key为name，值为将value里的Yaml字段值解析成interface{}
func VendorExtensionToMap(e []*openapi_v2.NamedAny) map[string]interface{} {
	values := map[string]interface{}{}

	for _, na := range e {
		// 跳过name为空和value为nil
		if na.GetName() == "" || na.GetValue() == nil {
			continue
		}
		// 跳过value里的Yaml字段值为空
		if na.GetValue().GetYaml() == "" {
			continue
		}
		var value interface{}
		// 将value里的Yaml字段值解析成interface{}
		err := yaml.Unmarshal([]byte(na.GetValue().GetYaml()), &value)
		if err != nil {
			continue
		}

		values[na.GetName()] = value
	}

	return values
}

// Definitions is an implementation of `Models`. It looks for
// models in an openapi Schema.
type Definitions struct {
	models map[string]Schema
}

var _ Models = &Definitions{}

// NewOpenAPIData creates a new `Models` out of the openapi document.
func NewOpenAPIData(doc *openapi_v2.Document) (Models, error) {
	definitions := Definitions{
		models: map[string]Schema{},
	}

	// Save the list of all models first. This will allow us to
	// validate that we don't have any dangling reference.
	// 比如namedSchema.GetName()为 "io.k8s.api.apps.v1.DaemonSet"的内容(这个是/openapi/v2返回json格式，不是*openapi_v2.Document，*openapi_v2.Document为/openapi/v2返回的proto buffer格式转成openapi_v2.Document)为
	// "io.k8s.api.apps.v1.DaemonSet": {
	// 	"description": "DaemonSet represents the configuration of a daemon set.",
	// 	"type": "object",
	// 	"properties": {
	// 		"apiVersion": {
	// 			"description": "APIVersion defines the versioned schema of this representation of an object. Servers should convert recognized schemas to the latest internal value, and may reject unrecognized values. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources",
	// 			"type": "string"
	// 		},
	// 		"kind": {
	// 			"description": "Kind is a string value representing the REST resource this object represents. Servers may infer this from the endpoint the client submits requests to. Cannot be updated. In CamelCase. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds",
	// 			"type": "string"
	// 		},
	// 		"metadata": {
	// 			"description": "Standard object's metadata. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata",
	// 			"$ref": "#/definitions/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"
	// 		},
	// 		"spec": {
	// 			"description": "The desired behavior of this daemon set. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status",
	// 			"$ref": "#/definitions/io.k8s.api.apps.v1.DaemonSetSpec"
	// 		},
	// 		"status": {
	// 			"description": "The current status of this daemon set. This data may be out of date by some window of time. Populated by the system. Read-only. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#spec-and-status",
	// 			"$ref": "#/definitions/io.k8s.api.apps.v1.DaemonSetStatus"
	// 		}
	// 	},
	// 	"x-kubernetes-group-version-kind": [
	// 		{
	// 			"group": "apps",
	// 			"kind": "DaemonSet",
	// 			"version": "v1"
	// 		}
	// 	]
	// },
	for _, namedSchema := range doc.GetDefinitions().GetAdditionalProperties() {
		definitions.models[namedSchema.GetName()] = nil
	}

	// Now, parse each model. We can validate that references exists.
	for _, namedSchema := range doc.GetDefinitions().GetAdditionalProperties() {
		path := NewPath(namedSchema.GetName())
		// 解析namedSchema.GetValue()（Schema类型），比如"io.k8s.api.apps.v1.DaemonSet"字段的内容
		schema, err := definitions.ParseSchema(namedSchema.GetValue(), &path)
		if err != nil {
			return nil, err
		}
		definitions.models[namedSchema.GetName()] = schema
	}

	return &definitions, nil
}

// We believe the schema is a reference, verify that and returns a new
// Schema
func (d *Definitions) parseReference(s *openapi_v2.Schema, path *Path) (Schema, error) {
	// TODO(wrong): a schema with a $ref can have properties. We can ignore them (would be incomplete), but we cannot return an error.
	// 在openapi_v2.Schema（在openapi_v2.NamedSchema.Value里）下面的"properties"里还有 []*openapi_v2.NamedSchema（字段的定义）
	if len(s.GetProperties().GetAdditionalProperties()) > 0 {
		return nil, newSchemaError(path, "unallowed embedded type definition")
	}
	// TODO(wrong): a schema with a $ref can have a type. We can ignore it (would be incomplete), but we cannot return an error.
	// 存在"type"字段，则返回错误
	if len(s.GetType().GetValue()) > 0 {
		return nil, newSchemaError(path, "definition reference can't have a type")
	}

	// TODO(wrong): $refs outside of the definitions are completely valid. We can ignore them (would be incomplete), but we cannot return an error.
	// "$ref"的值的前缀不是"#/definitions/"
	if !strings.HasPrefix(s.GetXRef(), "#/definitions/") {
		return nil, newSchemaError(path, "unallowed reference to non-definition %q", s.GetXRef())
	}
	// "$ref": "#/definitions/io.k8s.api.apps.v1.DaemonSetSpec"
	reference := strings.TrimPrefix(s.GetXRef(), "#/definitions/")
	// 查找reference是否存在，不存在返回错误
	if _, ok := d.models[reference]; !ok {
		return nil, newSchemaError(path, "unknown model in reference: %q", reference)
	}
	// 生成baseSchema
	base, err := d.parseBaseSchema(s, path)
	if err != nil {
		return nil, err
	}
	return &Ref{
		BaseSchema:  base,
		reference:   reference,
		definitions: d,
	}, nil
}

func parseDefault(def *openapi_v2.Any) (interface{}, error) {
	if def == nil {
		return nil, nil
	}
	var i interface{}
	if err := yaml.Unmarshal([]byte(def.Yaml), &i); err != nil {
		return nil, err
	}
	return i, nil
}

// 返回BaseSchema（没有"properties"字段）
func (d *Definitions) parseBaseSchema(s *openapi_v2.Schema, path *Path) (BaseSchema, error) {
	// s.GetDefault()获得"default"字段值
	// 比如"io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.JSONSchemaProps": {
	//     "description": "JSONSchemaProps is a JSON-Schema following Specification Draft 4 (http://json-schema.org/).",
	//     "type": "object",
	//     "properties": {
	//			"default": {
	//				"description": "default is a default value for undefined object fields. Defaulting is a beta feature under the CustomResourceDefaulting feature gate. Defaulting requires spec.preserveUnknownFields to be false.",
	//				"$ref": "#/definitions/io.k8s.apiextensions-apiserver.pkg.apis.apiextensions.v1.JSON"
	//			},
	// 将"default"字段值里的Yaml字段解析为interface{}
	def, err := parseDefault(s.GetDefault())
	if err != nil {
		return BaseSchema{}, err
	}
	return BaseSchema{
		// "description"字段值
		Description: s.GetDescription(),
		Default:     def,
		// s.GetVendorExtension()获取扩展字段
		// 比如"x-kubernetes-preserve-unknown-fields": true
		// 比如 "x-kubernetes-group-version-kind": [
		// 	{
		// 		"group": "operator.victoriametrics.com",
		// 		"kind": "VMAuthList",
		// 		"version": "v1beta1"
		// 	}
		// ]
		// []*openapi_v2.NamedAny转成map[string]interface{}，key为name，值为将value里的Yaml字段值解析成interface{}
		Extensions:  VendorExtensionToMap(s.GetVendorExtension()),
		Path:        *path,
	}, nil
}

// We believe the schema is a map, verify and return a new schema
// 如果有"additionalProperties"字段且类型是AdditionalPropertiesItem_Schema类型，则解析解析"additionalProperties"字段的值，作为返回Map类型的Schema的SubType字段
// 否则没有"additionalProperties"，则解析成BaseSchema，作为Arbitrary的BaseSchema字段，然后Arbitrary作为返回Map类型的Schema的BaseSchema字段
// 然后s按照BaseSchema进行解析，最为作为返回Map类型的Schema的BaseSchema字段
func (d *Definitions) parseMap(s *openapi_v2.Schema, path *Path) (Schema, error) {
	// 存在"type"字段，且"type"字段的值不为"object"
	if len(s.GetType().GetValue()) != 0 && s.GetType().GetValue()[0] != object {
		return nil, newSchemaError(path, "invalid object type")
	}
	var sub Schema
	// TODO(incomplete): this misses the boolean case as AdditionalProperties is a bool+schema sum type.
	// 比如"additionalProperties"为
	// "matchLabels": {
	// 	"description": "matchLabels is a map of {key,value} pairs. A single {key,value} in the matchLabels map is equivalent to an element of matchExpressions, whose key field is \"key\", the operator is \"In\", and the values array contains only \"value\". The requirements are ANDed.",
	// 	"type": "object",
	// 	"additionalProperties": {
	// 		"type": "string"
	// 	}
	// }
	// s.AdditionalProperties的Oneof字段是AdditionalPropertiesItem_Boolean（不是AdditionalPropertiesItem_Schema类型），或者是nil（s.AdditionalProperties为nil)
	// "additionalProperties"里没有Schema（各个字段），目前kubernetes里没有这种情况。或没有"additionalProperties"字段
	if s.GetAdditionalProperties().GetSchema() == nil {
		// 返回BaseSchema（没有"properties"字段）
		base, err := d.parseBaseSchema(s, path)
		if err != nil {
			return nil, err
		}
		sub = &Arbitrary{
			BaseSchema: base,
		}
	} else {
		var err error
		// s.AdditionalProperties不为nil，且s.AdditionalProperties的Oneof字段是AdditionalPropertiesItem_Schema类型（不是AdditionalPropertiesItem_Boolean）
		// 解析"additionalProperties"字段的值（是Schema类型）
		sub, err = d.ParseSchema(s.GetAdditionalProperties().GetSchema(), path)
		if err != nil {
			return nil, err
		}
	}
	// 返回BaseSchema（没有"properties"字段）
	base, err := d.parseBaseSchema(s, path)
	if err != nil {
		return nil, err
	}
	return &Map{
		BaseSchema: base,
		SubType:    sub,
	}, nil
}

// 按照BaseSchema对s进行解析，结果作为Primitive（返回值）的BaseSchema
// Primitive的Type字段为s的"type"的字段值
// Primitive的Format字段为"format"字段的值
func (d *Definitions) parsePrimitive(s *openapi_v2.Schema, path *Path) (Schema, error) {
	var t string
	// "type"的字段值长度大于一个，返回错误
	if len(s.GetType().GetValue()) > 1 {
		return nil, newSchemaError(path, "primitive can't have more than 1 type")
	}
	if len(s.GetType().GetValue()) == 1 {
		t = s.GetType().GetValue()[0]
	}
	switch t {
	case String: // do nothing
	case Number: // do nothing
	case Integer: // do nothing
	case Boolean: // do nothing
	// TODO(wrong): this misses "null". Would skip the null case (would be incomplete), but we cannot return an error.
	default:
		return nil, newSchemaError(path, "Unknown primitive type: %q", t)
	}
	// 返回BaseSchema
	base, err := d.parseBaseSchema(s, path)
	if err != nil {
		return nil, err
	}
	return &Primitive{
		BaseSchema: base,
		Type:       t,
		// "format"字段的值
		Format:     s.GetFormat(),
	}, nil
}

// 解析"items"字段值（Schema类型），为返回的Array的SubType字段
// 按照BaseSchema对s进行解析，结果作为Array的BaseSchema字段
func (d *Definitions) parseArray(s *openapi_v2.Schema, path *Path) (Schema, error) {
	// "type"的字段值长度不为一个，返回错误
	if len(s.GetType().GetValue()) != 1 {
		return nil, newSchemaError(path, "array should have exactly one type")
	}
	// "type"的字段值不为"array"
	if s.GetType().GetValue()[0] != array {
		return nil, newSchemaError(path, `array should have type "array"`)
	}
	// "items"下面不止有一个item，则返回错误
	if len(s.GetItems().GetSchema()) != 1 {
		// TODO(wrong): Items can have multiple elements. We can ignore Items then (would be incomplete), but we cannot return an error.
		// TODO(wrong): "type: array" witohut any items at all is completely valid.
		return nil, newSchemaError(path, "array should have exactly one sub-item")
	}
	// 解析"items"（Schema类型）
	sub, err := d.ParseSchema(s.GetItems().GetSchema()[0], path)
	if err != nil {
		return nil, err
	}
	// 返回BaseSchema（没有"properties"字段）
	base, err := d.parseBaseSchema(s, path)
	if err != nil {
		return nil, err
	}
	return &Array{
		BaseSchema: base,
		SubType:    sub,
	}, nil
}

func (d *Definitions) parseKind(s *openapi_v2.Schema, path *Path) (Schema, error) {
	// 有"type"字段，且"type"不为"object"，则返回错误
	if len(s.GetType().GetValue()) != 0 && s.GetType().GetValue()[0] != object {
		return nil, newSchemaError(path, "invalid object type")
	}
	// "properties"字段不存在
	if s.GetProperties() == nil {
		return nil, newSchemaError(path, "object doesn't have properties")
	}

	// "type": "object"，且"properties"字段存在

	fields := map[string]Schema{}
	fieldOrder := []string{}

	// 遍历所有"properties"字段下的值
	for _, namedSchema := range s.GetProperties().GetAdditionalProperties() {
		var err error
		// 字段名字
		name := namedSchema.GetName()
		// 返回Path{
		// 	parent: path,
		// 	key:    fmt.Sprintf(".%s", name),
		// }
		path := path.FieldPath(name)
		// 解析值的schema
		fields[name], err = d.ParseSchema(namedSchema.GetValue(), &path)
		if err != nil {
			return nil, err
		}
		fieldOrder = append(fieldOrder, name)
	}

	// 返回BaseSchema（没有"properties"字段）
	base, err := d.parseBaseSchema(s, path)
	if err != nil {
		return nil, err
	}
	return &Kind{
		BaseSchema:     base,
		// 获取"required"字段值（需要设置的字段列表）
		RequiredFields: s.GetRequired(),
		Fields:         fields,
		FieldOrder:     fieldOrder,
	}, nil
}

// 返回Arbitrary（里面嵌套了BaseSchema）类型的Schema
func (d *Definitions) parseArbitrary(s *openapi_v2.Schema, path *Path) (Schema, error) {
	// 返回BaseSchema（没有"properties"字段）
	base, err := d.parseBaseSchema(s, path)
	if err != nil {
		return nil, err
	}
	return &Arbitrary{
		BaseSchema: base,
	}, nil
}

// ParseSchema creates a walkable Schema from an openapi schema. While
// this function is public, it doesn't leak through the interface.
func (d *Definitions) ParseSchema(s *openapi_v2.Schema, path *Path) (Schema, error) {
	// 如果存在"$ref"的key，返回Ref类型
	if s.GetXRef() != "" {
		// TODO(incomplete): ignoring the rest of s is wrong. As long as there are no conflict, everything from s must be considered
		// Reference: https://github.com/OAI/OpenAPI-Specification/blob/master/versions/2.0.md#path-item-object
		return d.parseReference(s, path)
	}
	objectTypes := s.GetType().GetValue()
	switch len(objectTypes) {
	// "type"字段为空
	case 0:
		// in the OpenAPI schema served by older k8s versions, object definitions created from structs did not include
		// the type:object property (they only included the "properties" property), so we need to handle this case
		// TODO: validate that we ever published empty, non-nil properties. JSON roundtripping nils them.
		// 有"properties"字段
		if s.GetProperties() != nil {
			// TODO(wrong): when verifying a non-object later against this, it will be rejected as invalid type.
			// TODO(CRD validation schema publishing): we have to filter properties (empty or not) if type=object is not given
			// 返回kind类型的Schema
			return d.parseKind(s, path)
		} else {
			// Definition has no type and no properties. Treat it as an arbitrary value
			// TODO(incomplete): what if it has additionalProperties=false or patternProperties?
			// ANSWER: parseArbitrary is less strict than it has to be with patternProperties (which is ignored). So this is correct (of course not complete).
			// 没有"properties"字段
			// 返回Arbitrary（里面嵌套了BaseSchema）类型的Schema
			return d.parseArbitrary(s, path)
		}
	case 1:
		t := objectTypes[0]
		switch t {
		// "type"字段值为"object"
		case object:
			// 有"properties"字段
			if s.GetProperties() != nil {
				// 返回kind类型的Schema
				return d.parseKind(s, path)
			} else {
				// 没有"properties"字段
				// 如果有"additionalProperties"字段且类型是AdditionalPropertiesItem_Schema类型，则解析解析"additionalProperties"字段的值，作为返回Map类型的Schema的SubType字段
				// 否则没有"additionalProperties"，则解析成BaseSchema，作为Arbitrary的BaseSchema字段，然后Arbitrary作为返回Map类型的Schema的BaseSchema字段
				// 然后s按照BaseSchema进行解析，最为作为返回Map类型的Schema的BaseSchema字段
				return d.parseMap(s, path)
			}
		// "type"字段值为"array"
		case array:
			// 解析"items"字段值（Schema类型），为返回的Array的SubType字段
			// 按照BaseSchema对s进行解析，结果作为Array的BaseSchema字段
			return d.parseArray(s, path)
		}
		// "type"字段值为其他值（"boolean"，"string"，"integer"，"number"）
		// 按照BaseSchema对s进行解析，结果作为Primitive（返回值）的BaseSchema
		// Primitive的Type字段为s的"type"的字段值
		// Primitive的Format字段为"format"字段的值
		return d.parsePrimitive(s, path)
	default:
		// the OpenAPI generator never generates (nor it ever did in the past) OpenAPI type definitions with multiple types
		// TODO(wrong): this is rejecting a completely valid OpenAPI spec
		// TODO(CRD validation schema publishing): filter these out
		return nil, newSchemaError(path, "definitions with multiple types aren't supported")
	}
}

// LookupModel is public through the interface of Models. It
// returns a visitable schema from the given model name.
func (d *Definitions) LookupModel(model string) Schema {
	return d.models[model]
}

func (d *Definitions) ListModels() []string {
	models := []string{}

	for model := range d.models {
		models = append(models, model)
	}

	sort.Strings(models)
	return models
}

type Ref struct {
	BaseSchema

	reference   string
	definitions *Definitions
}

var _ Reference = &Ref{}

func (r *Ref) Reference() string {
	return r.reference
}

// 获得r.reference（引用的schema）对应的Schema
func (r *Ref) SubSchema() Schema {
	return r.definitions.models[r.reference]
}

func (r *Ref) Accept(v SchemaVisitor) {
	v.VisitReference(r)
}

func (r *Ref) GetName() string {
	return fmt.Sprintf("Reference to %q", r.reference)
}
