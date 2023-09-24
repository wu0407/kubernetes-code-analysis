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
// as --output=go-template=
var templateFormats = map[string]bool{
	"template":         true,
	"go-template":      true,
	"go-template-file": true,
	"templatefile":     true,
}

// GoTemplatePrintFlags provides default flags necessary for template printing.
// Given the following flag values, a printer can be requested that knows
// how to handle printing based on these values.
type GoTemplatePrintFlags struct {
	// indicates if it is OK to ignore missing keys for rendering
	// an output template.
	AllowMissingKeys *bool
	TemplateArgument *string
}

// AllowedFormats returns slice of string of allowed GoTemplatePrint printing format
func (f *GoTemplatePrintFlags) AllowedFormats() []string {
	formats := make([]string, 0, len(templateFormats))
	for format := range templateFormats {
		formats = append(formats, format)
	}
	sort.Strings(formats)
	return formats
}

// ToPrinter receives an templateFormat and returns a printer capable of
// handling --template format printing.
// Returns false if the specified templateFormat does not match a template format.

func (f *GoTemplatePrintFlags) ToPrinter(templateFormat string) (printers.ResourcePrinter, error) {
	// 从NewKubeTemplatePrintFlags初始化的GoTemplatePrintFlags的话，这里的f.TemplateArgument不为nil
	// f.TemplateArgument为nil，或者f.TemplateArgument不为nil但是长度为0，直接返回不支持格式错误
	if (f.TemplateArgument == nil || len(*f.TemplateArgument) == 0) && len(templateFormat) == 0 {
		return nil, NoCompatiblePrinterError{Options: f, OutputFormat: &templateFormat}
	}

	templateValue := ""

	// 如果f.TemplateArgument不为nil，且长度不为0，即没有指定--template="{xxx}"
	if f.TemplateArgument == nil || len(*f.TemplateArgument) == 0 {
		// 支持的格式为template=、go-template=、go-template-file=、templatefile=，即--output=template="{xxx}", --output=go-template="{xxx}", --output=go-template-file="{xxx}", --output=templatefile="{xxx}"
		for format := range templateFormats {
			format = format + "="
			if strings.HasPrefix(templateFormat, format) {
				// templateFormat为{template format}="{templateValue}"，则templateValue为"{templateValue}",即"="后半部分
				templateValue = templateFormat[len(format):]
				// "="前半部分
				templateFormat = format[:len(format)-1]
				break
			}
		}
	} else {
		// 命令行指定了--template="{xxx}"
		// 如果命令行未指定了--output或-o，这里传入templateFormat为"go-template"。在staging\src\k8s.io\cli-runtime\pkg\genericclioptions\print_flags.go里(f *PrintFlags) ToPrinter()处理
		templateValue = *f.TemplateArgument
	}

	// 如果templateFormat不在支持的格式里，返回不支持格式错误
	// 主要针对同时指定了--output="{xxx}"和--template="{xxx}"，但是--output的值不在支持的格式里
	if _, supportedFormat := templateFormats[templateFormat]; !supportedFormat {
		return nil, NoCompatiblePrinterError{OutputFormat: &templateFormat, AllowedFormats: f.AllowedFormats()}
	}

	if len(templateValue) == 0 {
		return nil, fmt.Errorf("template format specified but no template given")
	}

	if templateFormat == "templatefile" || templateFormat == "go-template-file" {
		data, err := ioutil.ReadFile(templateValue)
		if err != nil {
			return nil, fmt.Errorf("error reading --template %s, %v", templateValue, err)
		}

		templateValue = string(data)
	}

	p, err := printers.NewGoTemplatePrinter([]byte(templateValue))
	if err != nil {
		return nil, fmt.Errorf("error parsing template %s, %v", templateValue, err)
	}

	allowMissingKeys := true
	if f.AllowMissingKeys != nil {
		allowMissingKeys = *f.AllowMissingKeys
	}

	p.AllowMissingKeys(allowMissingKeys)
	return p, nil
}

// AddFlags receives a *cobra.Command reference and binds
// flags related to template printing to it
func (f *GoTemplatePrintFlags) AddFlags(c *cobra.Command) {
	if f.TemplateArgument != nil {
		c.Flags().StringVar(f.TemplateArgument, "template", *f.TemplateArgument, "Template string or path to template file to use when -o=go-template, -o=go-template-file. The template format is golang templates [http://golang.org/pkg/text/template/#pkg-overview].")
		c.MarkFlagFilename("template")
	}
	if f.AllowMissingKeys != nil {
		c.Flags().BoolVar(f.AllowMissingKeys, "allow-missing-template-keys", *f.AllowMissingKeys, "If true, ignore any errors in templates when a field or map key is missing in the template. Only applies to golang and jsonpath output formats.")
	}
}

// NewGoTemplatePrintFlags returns flags associated with
// --template printing, with default values set.
func NewGoTemplatePrintFlags() *GoTemplatePrintFlags {
	allowMissingKeysPtr := true
	templateValuePtr := ""

	return &GoTemplatePrintFlags{
		TemplateArgument: &templateValuePtr,
		AllowMissingKeys: &allowMissingKeysPtr,
	}
}
