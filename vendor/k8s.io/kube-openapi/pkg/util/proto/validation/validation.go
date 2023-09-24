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
	"k8s.io/kube-openapi/pkg/util/proto"
)

// 基于v的类型返回，对应的validationItem接口的实现
// 支持类型为bool型、各种整形、各种浮点型、字符串、数组和切片、map
// 基于各个字段类型的validationItem接口进行验证（进行schema.Accept(SchemaVisitor)）
func ValidateModel(obj interface{}, schema proto.Schema, name string) []error {
	// 基于v的类型返回，对应的validationItem接口的实现
	// 支持类型为bool型、各种整形、各种浮点型、字符串、数组和切片、map
	rootValidation, err := itemFactory(proto.NewPath(name), obj)
	if err != nil {
		return []error{err}
	}
	// 基于各个字段类型的validationItem接口进行验证
	// vendor\k8s.io\kube-openapi\pkg\util\proto\validation\types.go
	schema.Accept(rootValidation)
	return rootValidation.Errors()
}
