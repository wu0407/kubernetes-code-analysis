/*
Copyright 2018 The Kubernetes Authors.

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

package genericclioptions

import (
	"fmt"
	"io/ioutil"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/cli-runtime/pkg/printers"
)

// templates are logically optional for specifying a format.
// this allows a user to specify a template format value
// as --output=jsonpath=
var jsonFormats = map[string]bool{
	"jsonpath":         true,
	"jsonpath-file":    true,
	"jsonpath-as-json": true,
}

// JSONPathPrintFlags provides default flags necessary for template printing.
// Given the following flag values, a printer can be requested that knows
// how to handle printing based on these values.
type JSONPathPrintFlags struct {
	// indicates if it is OK to ignore missing keys for rendering
	// an output template.
	AllowMissingKeys *bool
	TemplateArgument *string
}

// AllowedFormats returns slice of string of allowed JSONPath printing format
func (f *JSONPathPrintFlags) AllowedFormats() []string {
	formats := make([]string, 0, len(jsonFormats))
	for format := range jsonFormats {
		formats = append(formats, format)
	}
	sort.Strings(formats)
	return formats
}

// ToPrinter receives an templateFormat and returns a printer capable of
// handling --template format printing.
// Returns false if the specified templateFormat does not match a template format.
func (f *JSONPathPrintFlags) ToPrinter(templateFormat string) (printers.ResourcePrinter, error) {
	// 从NewJSONPathPrintFlags初始化不为nil，但是一定是*f.TemplateArgument为空
	// f.TemplateArgument为nil，或f.TemplateArgument为空且templateFormat为空，返回不支持错误
	if (f.TemplateArgument == nil || len(*f.TemplateArgument) == 0) && len(templateFormat) == 0 {
		return nil, NoCompatiblePrinterError{Options: f, OutputFormat: &templateFormat}
	}

	templateValue := ""

	// templateFormat不为空
	// f.TemplateArgument为nil，或f.TemplateArgument为空。（这个就是从NewJSONPathPrintFlags初始化）
	if f.TemplateArgument == nil || len(*f.TemplateArgument) == 0 {
		// --output=jsonpath="{xxx}", --output=jsonpath-file="{xxx}", --output=jsonpath-as-json="{xxx}"
		for format := range jsonFormats {
			format = format + "="
			if strings.HasPrefix(templateFormat, format) {
				// "="等号后半段
				templateValue = templateFormat[len(format):]
				// "="等号前半段
				templateFormat = format[:len(format)-1]
				break
			}
		}
	} else {
		templateValue = *f.TemplateArgument
	}

	// 判断格式类型是不是支持的，（支持的类型为jsonpath、jsonpath-file、jsonpath-as-json）
	if _, supportedFormat := jsonFormats[templateFormat]; !supportedFormat {
		return nil, NoCompatiblePrinterError{OutputFormat: &templateFormat, AllowedFormats: f.AllowedFormats()}
	}

	// 如果为--template，但是没有值，则返回错误（这个目前不会在kubectl apply，因为(f *JSONPathPrintFlags) AddFlags(c *cobra.Command)不会被调用）
	if len(templateValue) == 0 {
		return nil, fmt.Errorf("template format specified but no template given")
	}

	// 如果为--output=jsonpath-file，则读取文件内容
	if templateFormat == "jsonpath-file" {
		data, err := ioutil.ReadFile(templateValue)
		if err != nil {
			return nil, fmt.Errorf("error reading --template %s, %v", templateValue, err)
		}

		templateValue = string(data)
	}

	// 
	p, err := printers.NewJSONPathPrinter(templateValue)
	if err != nil {
		return nil, fmt.Errorf("error parsing jsonpath %s, %v", templateValue, err)
	}

	allowMissingKeys := true
	if f.AllowMissingKeys != nil {
		allowMissingKeys = *f.AllowMissingKeys
	}

	p.AllowMissingKeys(allowMissingKeys)

	if templateFormat == "jsonpath-as-json" {
		p.EnableJSONOutput(true)
	}

	return p, nil
}

// AddFlags receives a *cobra.Command reference and binds
// flags related to template printing to it
func (f *JSONPathPrintFlags) AddFlags(c *cobra.Command) {
	if f.TemplateArgument != nil {
		c.Flags().StringVar(f.TemplateArgument, "template", *f.TemplateArgument, "Template string or path to template file to use when --output=jsonpath, --output=jsonpath-file.")
		c.MarkFlagFilename("template")
	}
	if f.AllowMissingKeys != nil {
		c.Flags().BoolVar(f.AllowMissingKeys, "allow-missing-template-keys", *f.AllowMissingKeys, "If true, ignore any errors in templates when a field or map key is missing in the template. Only applies to golang and jsonpath output formats.")
	}
}

// NewJSONPathPrintFlags returns flags associated with
// --template printing, with default values set.
func NewJSONPathPrintFlags(templateValue string, allowMissingKeys bool) *JSONPathPrintFlags {
	return &JSONPathPrintFlags{
		TemplateArgument: &templateValue,
		AllowMissingKeys: &allowMissingKeys,
	}
}
