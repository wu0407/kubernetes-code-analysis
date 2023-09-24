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
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/printers"
)

// NoCompatiblePrinterError is a struct that contains error information.
// It will be constructed when a invalid printing format is provided
type NoCompatiblePrinterError struct {
	OutputFormat   *string
	AllowedFormats []string
	Options        interface{}
}

func (e NoCompatiblePrinterError) Error() string {
	output := ""
	if e.OutputFormat != nil {
		output = *e.OutputFormat
	}

	sort.Strings(e.AllowedFormats)
	return fmt.Sprintf("unable to match a printer suitable for the output format %q, allowed formats are: %s", output, strings.Join(e.AllowedFormats, ","))
}

// IsNoCompatiblePrinterError returns true if it is a not a compatible printer
// otherwise it will return false
func IsNoCompatiblePrinterError(err error) bool {
	if err == nil {
		return false
	}

	_, ok := err.(NoCompatiblePrinterError)
	return ok
}

// PrintFlags composes common printer flag structs
// used across all commands, and provides a method
// of retrieving a known printer based on flag values provided.
type PrintFlags struct {
	JSONYamlPrintFlags   *JSONYamlPrintFlags
	NamePrintFlags       *NamePrintFlags
	TemplatePrinterFlags *KubeTemplatePrintFlags

	TypeSetterPrinter *printers.TypeSetterPrinter

	OutputFormat *string

	// OutputFlagSpecified indicates whether the user specifically requested a certain kind of output.
	// Using this function allows a sophisticated caller to change the flag binding logic if they so desire.
	OutputFlagSpecified func() bool
}

// Complete sets NamePrintFlags operation flag from sucessTemplate
// 设置f.NamePrintFlags.Operation为successTemplate与f.NamePrintFlags.Operation渲染出来的字符串
func (f *PrintFlags) Complete(successTemplate string) error {
	return f.NamePrintFlags.Complete(successTemplate)
}

// AllowedFormats returns slice of string of allowed JSONYaml/Name/Template printing format
func (f *PrintFlags) AllowedFormats() []string {
	ret := []string{}
	ret = append(ret, f.JSONYamlPrintFlags.AllowedFormats()...)
	ret = append(ret, f.NamePrintFlags.AllowedFormats()...)
	ret = append(ret, f.TemplatePrinterFlags.AllowedFormats()...)
	return ret
}

// ToPrinter returns a printer capable of
// handling --output or --template printing.
// Returns false if the specified outputFormat does not match a supported format.
// Supported format types can be found in pkg/printers/printers.go
// 根据--output or --template，返回最终的实现了ResourcePrinter接口的对象TypeSetterPrinter{delegate: p, Typer: scheme}
// p可能是JSONPathPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\jsonpath.go
// p可能是GoTemplatePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\gotemplate.go
// p可能是NamePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\name.go
// p可能是JSONPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\json.go
// p可能是YAMLPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\yaml.go
// p可能是OmitManagedFieldsPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\omitmanagedfields.go
// 其中TypeSetterPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\printers.go
func (f *PrintFlags) ToPrinter() (printers.ResourcePrinter, error) {
	outputFormat := ""
	// 命令行指定了--output或-o，或者默认NewPrintFlags初始化为空（不为nil）
	if f.OutputFormat != nil {
		outputFormat = *f.OutputFormat
	}
	// For backwards compatibility we want to support a --template argument given, even when no --output format is provided.
	// If no explicit output format has been provided via the --output flag, fallback
	// to honoring the --template argument.
	// 命令行指定了--template
	templateFlagSpecified := f.TemplatePrinterFlags != nil &&
		f.TemplatePrinterFlags.TemplateArgument != nil &&
		len(*f.TemplatePrinterFlags.TemplateArgument) > 0
	// 命令行指定了--output或-o
	outputFlagSpecified := f.OutputFlagSpecified != nil && f.OutputFlagSpecified()
	// 没有--output或-o，但是指定了--template，则outputFormat = "go-template"
	if templateFlagSpecified && !outputFlagSpecified {
		outputFormat = "go-template"
	}

	// 从NewPrintFlags初始化就不为nil
	if f.JSONYamlPrintFlags != nil {
		// 根据outputFormat返回对应得printers.ResourcePrinter
		// "json"为printers.JSONPrinter{}，如果f.showManagedFields为true，则返回printers.OmitManagedFieldsPrinter（包装printers.JSONPrinter{}）
		// "yaml"为printers.YAMLPrinter{}，如果f.showManagedFields为true，则返回printers.OmitManagedFieldsPrinter（包装printers.JSONPrinter{}）
		// IsNoCompatiblePrinterError(err)返回true说明是不支持的输出格式
		if p, err := f.JSONYamlPrintFlags.ToPrinter(outputFormat); !IsNoCompatiblePrinterError(err) {
			// 返回包装了p的TypeSetterPrinter{delegate: p, Typer: scheme}
			return f.TypeSetterPrinter.WrapToPrinter(p, err)
		}
	}

	// 从NewPrintFlags初始化就不为nil
	if f.NamePrintFlags != nil {
		// outputFormat只支持"name"和""，即命令行--output=name，--output=""
		// 返回printers.NamePrinter{Operation: f.NamePrintFlags.Operation}
		if p, err := f.NamePrintFlags.ToPrinter(outputFormat); !IsNoCompatiblePrinterError(err) {
			// 返回包装了p的TypeSetterPrinter{delegate: p, Typer: scheme}
			return f.TypeSetterPrinter.WrapToPrinter(p, err)
		}
	}

	// 从NewPrintFlags初始化就不为nil
	if f.TemplatePrinterFlags != nil {
		// --output=jsonpath="{xxx}", --output=jsonpath-file="{xxx}", --output=jsonpath-as-json="{xxx}"
		// --output=template="{xxx}", --output=go-template="{xxx}", --output=go-template-file="{xxx}", --output=templatefile="{xxx}"，--template="{xxx}"
		if p, err := f.TemplatePrinterFlags.ToPrinter(outputFormat); !IsNoCompatiblePrinterError(err) {
			// 返回包装了p的TypeSetterPrinter{delegate: p, Typer: scheme}
			return f.TypeSetterPrinter.WrapToPrinter(p, err)
		}
	}

	return nil, NoCompatiblePrinterError{OutputFormat: f.OutputFormat, AllowedFormats: f.AllowedFormats()}
}

// AddFlags receives a *cobra.Command reference and binds
// flags related to JSON/Yaml/Name/Template printing to it
func (f *PrintFlags) AddFlags(cmd *cobra.Command) {
	f.JSONYamlPrintFlags.AddFlags(cmd)
	f.NamePrintFlags.AddFlags(cmd)
	f.TemplatePrinterFlags.AddFlags(cmd)

	if f.OutputFormat != nil {
		cmd.Flags().StringVarP(f.OutputFormat, "output", "o", *f.OutputFormat, fmt.Sprintf("Output format. One of: %s.", strings.Join(f.AllowedFormats(), "|")))
		if f.OutputFlagSpecified == nil {
			f.OutputFlagSpecified = func() bool {
				return cmd.Flag("output").Changed
			}
		}
	}
}

// WithDefaultOutput sets a default output format if one is not provided through a flag value
func (f *PrintFlags) WithDefaultOutput(output string) *PrintFlags {
	f.OutputFormat = &output
	return f
}

// WithTypeSetter sets a wrapper than will surround the returned printer with a printer to type resources
func (f *PrintFlags) WithTypeSetter(scheme *runtime.Scheme) *PrintFlags {
	f.TypeSetterPrinter = printers.NewTypeSetter(scheme)
	return f
}

// NewPrintFlags returns a default *PrintFlags
func NewPrintFlags(operation string) *PrintFlags {
	outputFormat := ""

	return &PrintFlags{
		OutputFormat: &outputFormat,

		JSONYamlPrintFlags:   NewJSONYamlPrintFlags(),
		NamePrintFlags:       NewNamePrintFlags(operation),
		TemplatePrinterFlags: NewKubeTemplatePrintFlags(),
	}
}
