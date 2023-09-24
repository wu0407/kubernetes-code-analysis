/*
Copyright 2014 The Kubernetes Authors.

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
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/klog/v2"
	"k8s.io/kubectl/pkg/cmd/delete"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/scheme"
	"k8s.io/kubectl/pkg/util"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/openapi"
	"k8s.io/kubectl/pkg/util/templates"
	"k8s.io/kubectl/pkg/validation"
)

// ApplyFlags directly reflect the information that CLI is gathering via flags.  They will be converted to Options, which
// reflect the runtime requirements for the command.  This structure reduces the transformation to wiring and makes
// the logic itself easy to unit test
type ApplyFlags struct {
	Factory cmdutil.Factory

	RecordFlags *genericclioptions.RecordFlags
	PrintFlags  *genericclioptions.PrintFlags

	DeleteFlags *delete.DeleteFlags

	FieldManager   string
	Selector       string
	Prune          bool
	PruneResources []pruneResource
	All            bool
	Overwrite      bool
	OpenAPIPatch   bool
	PruneWhitelist []string

	genericclioptions.IOStreams
}

// ApplyOptions defines flags and other configuration parameters for the `apply` command
type ApplyOptions struct {
	Recorder genericclioptions.Recorder

	PrintFlags *genericclioptions.PrintFlags
	ToPrinter  func(string) (printers.ResourcePrinter, error)

	DeleteOptions *delete.DeleteOptions

	ServerSideApply bool
	ForceConflicts  bool
	FieldManager    string
	Selector        string
	DryRunStrategy  cmdutil.DryRunStrategy
	DryRunVerifier  *resource.DryRunVerifier
	Prune           bool
	PruneResources  []pruneResource
	cmdBaseName     string
	All             bool
	Overwrite       bool
	OpenAPIPatch    bool
	PruneWhitelist  []string

	Validator     validation.Schema
	Builder       *resource.Builder
	Mapper        meta.RESTMapper
	DynamicClient dynamic.Interface
	OpenAPISchema openapi.Resources

	Namespace        string
	EnforceNamespace bool

	genericclioptions.IOStreams

	// Objects (and some denormalized data) which are to be
	// applied. The standard way to fill in this structure
	// is by calling "GetObjects()", which will use the
	// resource builder if "objectsCached" is false. The other
	// way to set this field is to use "SetObjects()".
	// Subsequent calls to "GetObjects()" after setting would
	// not call the resource builder; only return the set objects.
	objects       []*resource.Info
	objectsCached bool

	// Stores visited objects/namespaces for later use
	// calculating the set of objects to prune.
	VisitedUids       sets.String
	VisitedNamespaces sets.String

	// Function run after the objects are generated and
	// stored in the "objects" field, but before the
	// apply is run on these objects.
	PreProcessorFn func() error
	// Function run after all objects have been applied.
	// The standard PostProcessorFn is "PrintAndPrunePostProcessor()".
	PostProcessorFn func() error
}

var (
	applyLong = templates.LongDesc(i18n.T(`
		Apply a configuration to a resource by file name or stdin.
		The resource name must be specified. This resource will be created if it doesn't exist yet.
		To use 'apply', always create the resource initially with either 'apply' or 'create --save-config'.

		JSON and YAML formats are accepted.

		Alpha Disclaimer: the --prune functionality is not yet complete. Do not use unless you are aware of what the current state is. See https://issues.k8s.io/34274.`))

	applyExample = templates.Examples(i18n.T(`
		# Apply the configuration in pod.json to a pod
		kubectl apply -f ./pod.json

		# Apply resources from a directory containing kustomization.yaml - e.g. dir/kustomization.yaml
		kubectl apply -k dir/

		# Apply the JSON passed into stdin to a pod
		cat pod.json | kubectl apply -f -

		# Note: --prune is still in Alpha
		# Apply the configuration in manifest.yaml that matches label app=nginx and delete all other resources that are not in the file and match label app=nginx
		kubectl apply --prune -f manifest.yaml -l app=nginx

		# Apply the configuration in manifest.yaml and delete all the other config maps that are not in the file
		kubectl apply --prune -f manifest.yaml --all --prune-whitelist=core/v1/ConfigMap`))

	warningNoLastAppliedConfigAnnotation = "Warning: resource %[1]s is missing the %[2]s annotation which is required by %[3]s apply. %[3]s apply should only be used on resources created declaratively by either %[3]s create --save-config or %[3]s apply. The missing annotation will be patched automatically.\n"
	warningChangesOnDeletingResource     = "Warning: Detected changes to resource %[1]s which is currently being deleted.\n"
)

// NewApplyFlags returns a default ApplyFlags
func NewApplyFlags(f cmdutil.Factory, streams genericclioptions.IOStreams) *ApplyFlags {
	return &ApplyFlags{
		Factory:     f,
		RecordFlags: genericclioptions.NewRecordFlags(),
		DeleteFlags: delete.NewDeleteFlags("that contains the configuration to apply"),
		PrintFlags:  genericclioptions.NewPrintFlags("created").WithTypeSetter(scheme.Scheme),

		Overwrite:    true,
		OpenAPIPatch: true,

		IOStreams: streams,
	}
}

// NewCmdApply creates the `apply` command
func NewCmdApply(baseName string, f cmdutil.Factory, ioStreams genericclioptions.IOStreams) *cobra.Command {
	flags := NewApplyFlags(f, ioStreams)

	cmd := &cobra.Command{
		Use:                   "apply (-f FILENAME | -k DIRECTORY)",
		DisableFlagsInUseLine: true,
		Short:                 i18n.T("Apply a configuration to a resource by file name or stdin"),
		Long:                  applyLong,
		Example:               applyExample,
		Run: func(cmd *cobra.Command, args []string) {
			o, err := flags.ToOptions(cmd, baseName, args)
			cmdutil.CheckErr(err)
			cmdutil.CheckErr(o.Validate(cmd, args))
			cmdutil.CheckErr(o.Run())
		},
	}

	flags.AddFlags(cmd)

	// apply subcommands
	cmd.AddCommand(NewCmdApplyViewLastApplied(flags.Factory, flags.IOStreams))
	cmd.AddCommand(NewCmdApplySetLastApplied(flags.Factory, flags.IOStreams))
	cmd.AddCommand(NewCmdApplyEditLastApplied(flags.Factory, flags.IOStreams))

	return cmd
}

// AddFlags registers flags for a cli
func (flags *ApplyFlags) AddFlags(cmd *cobra.Command) {
	// bind flag structs
	flags.DeleteFlags.AddFlags(cmd)
	flags.RecordFlags.AddFlags(cmd)
	flags.PrintFlags.AddFlags(cmd)

	cmdutil.AddValidateFlags(cmd)
	cmdutil.AddDryRunFlag(cmd)
	cmdutil.AddServerSideApplyFlags(cmd)
	cmdutil.AddFieldManagerFlagVar(cmd, &flags.FieldManager, FieldManagerClientSideApply)

	cmd.Flags().BoolVar(&flags.Overwrite, "overwrite", flags.Overwrite, "Automatically resolve conflicts between the modified and live configuration by using values from the modified configuration")
	cmd.Flags().BoolVar(&flags.Prune, "prune", flags.Prune, "Automatically delete resource objects, that do not appear in the configs and are created by either apply or create --save-config. Should be used with either -l or --all.")
	cmd.Flags().StringVarP(&flags.Selector, "selector", "l", flags.Selector, "Selector (label query) to filter on, supports '=', '==', and '!='.(e.g. -l key1=value1,key2=value2)")
	cmd.Flags().BoolVar(&flags.All, "all", flags.All, "Select all resources in the namespace of the specified resource types.")
	cmd.Flags().StringArrayVar(&flags.PruneWhitelist, "prune-whitelist", flags.PruneWhitelist, "Overwrite the default whitelist with <group/version/kind> for --prune")
	cmd.Flags().BoolVar(&flags.OpenAPIPatch, "openapi-patch", flags.OpenAPIPatch, "If true, use openapi to calculate diff when the openapi presents and the resource can be found in the openapi spec. Otherwise, fall back to use baked-in types.")
}

// ToOptions converts from CLI inputs to runtime inputs
func (flags *ApplyFlags) ToOptions(cmd *cobra.Command, baseName string, args []string) (*ApplyOptions, error) {
	serverSideApply := cmdutil.GetServerSideApplyFlag(cmd)
	forceConflicts := cmdutil.GetForceConflictsFlag(cmd)
	// --dry-run命令行参数，支持client、server、none、空值和bool值（已经废弃）
	dryRunStrategy, err := cmdutil.GetDryRunStrategy(cmd)
	if err != nil {
		return nil, err
	}

	dynamicClient, err := flags.Factory.DynamicClient()
	if err != nil {
		return nil, err
	}

	// 返回DryRunVerifier
	dryRunVerifier := resource.NewDryRunVerifier(dynamicClient, flags.Factory.OpenAPIGetter())
	// 设置了--field-manager那就使用这个值
	// 否则，如果启用了server-side apply，那么field manager值为"kubectl"（为了向前兼容）
	// 其他情况，field manager值为"kubectl-client-side-apply"
	fieldManager := GetApplyFieldManagerFlag(cmd, serverSideApply)

	// allow for a success message operation to be specified at print time
	// 当启用dry-run
	//   client端dry-run，设置flags.printFlags.NamePrintFlags.Operation为"%s (dry run)"与f.NamePrintFlags.Operation渲染出来的字符串，返回flags.printFlags
	//   server端dry-run，设置flags.printFlags.NamePrintFlags.Operation为"%s (server dry run)"与f.NamePrintFlags.Operation渲染出来的字符串，返回flags.printFlags
	// 不启用，设置flags.PrintFlags.NamePrintFlags.Operation为operation
	toPrinter := func(operation string) (printers.ResourcePrinter, error) {
		flags.PrintFlags.NamePrintFlags.Operation = operation
		// 当启用dry-run
		//   client端dry-run，设置flags.printFlags.NamePrintFlags.Operation为"%s (dry run)"与f.NamePrintFlags.Operation渲染出来的字符串，返回flags.printFlags
		//   server端dry-run，设置flags.printFlags.NamePrintFlags.Operation为"%s (server dry run)"与f.NamePrintFlags.Operation渲染出来的字符串，返回flags.printFlags
		// 不启用，则直接返回flags.printFlags
		cmdutil.PrintFlagsWithDryRunStrategy(flags.PrintFlags, dryRunStrategy)
		// 根据--output or --template，返回最终的实现了ResourcePrinter接口的对象TypeSetterPrinter{delegate: p, Typer: scheme}
		// p可能是JSONPathPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\jsonpath.go
		// p可能是GoTemplatePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\gotemplate.go
		// p可能是NamePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\name.go
		// p可能是JSONPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\json.go
		// p可能是YAMLPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\yaml.go
		// p可能是OmitManagedFieldsPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\omitmanagedfields.go
		// 其中TypeSetterPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\printers.go
		return flags.PrintFlags.ToPrinter()
	}

	// 设置flags.RecordFlags.changeCause为{命令行路径}+{non-flag arguments...}+{flag arguments (--xxx=xxx,-xxx=xxx)}
	flags.RecordFlags.Complete(cmd)
	// --record=false或没有这个命令参数，则recorder为NoopRecorder{}
	// 否则返回ChangeCauseRecorder{changeCause: flags.RecordFlags.changeCause}
	recorder, err := flags.RecordFlags.ToRecorder()
	if err != nil {
		return nil, err
	}

	deleteOptions, err := flags.DeleteFlags.ToOptions(dynamicClient, flags.IOStreams)
	if err != nil {
		return nil, err
	}

	// 检查必须设置-f、--file，或-k、--kustomize
	err = deleteOptions.FilenameOptions.RequireFilenameOrKustomize()
	if err != nil {
		return nil, err
	}

	// 访问"/openapi/v2"，获得openapi_v2.Document
	// 从openapi_v2.Document解析出document包含models字段（apiserver中所有类型的定义）和resources（key为GroupVersionKind，value为modelName）
	openAPISchema, _ := flags.Factory.OpenAPISchema()
	// 返回validation.ConjunctiveSchema，包含验证各个字段的openapivalidation.SchemaValidation和validation.NoDoubleKeySchema（检查metadata.labels和metadata.annotations是否有重复的key）
	validator, err := flags.Factory.Validator(cmdutil.GetFlagBool(cmd, "validate"))
	if err != nil {
		return nil, err
	}
	// 实现在staging\src\k8s.io\kubectl\pkg\cmd\util\factory_client_access.go factoryImpl
	builder := flags.Factory.NewBuilder()
	mapper, err := flags.Factory.ToRESTMapper()
	if err != nil {
		return nil, err
	}

	// kubeconfig里默认配置的namespace
	namespace, enforceNamespace, err := flags.Factory.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return nil, err
	}

	// 命令行指定了--prune
	if flags.Prune {
		// gvks支持的格式为"{group}/{version}/{kind}"，并从apiserver中（meta.RESTMapper）进行获取（验证）相关的信息，返回[]pruneResource（group、version、kind、scope）
		flags.PruneResources, err = parsePruneResources(mapper, flags.PruneWhitelist)
		if err != nil {
			return nil, err
		}
	}

	o := &ApplyOptions{
		// 	Store baseName for use in printing warnings / messages involving the base command name.
		// 	This is useful for downstream command that wrap this one.
		cmdBaseName: baseName,

		PrintFlags: flags.PrintFlags,

		DeleteOptions:   deleteOptions,
		ToPrinter:       toPrinter,
		ServerSideApply: serverSideApply,
		ForceConflicts:  forceConflicts,
		FieldManager:    fieldManager,
		Selector:        flags.Selector,
		DryRunStrategy:  dryRunStrategy,
		DryRunVerifier:  dryRunVerifier,
		Prune:           flags.Prune,
		PruneResources:  flags.PruneResources,
		All:             flags.All,
		Overwrite:       flags.Overwrite,
		OpenAPIPatch:    flags.OpenAPIPatch,
		PruneWhitelist:  flags.PruneWhitelist,

		Recorder:         recorder,
		Namespace:        namespace,
		EnforceNamespace: enforceNamespace,
		Validator:        validator,
		Builder:          builder,
		Mapper:           mapper,
		DynamicClient:    dynamicClient,
		OpenAPISchema:    openAPISchema,

		IOStreams: flags.IOStreams,

		objects:       []*resource.Info{},
		objectsCached: false,

		VisitedUids:       sets.NewString(),
		VisitedNamespaces: sets.NewString(),
	}

	o.PostProcessorFn = o.PrintAndPrunePostProcessor()

	return o, nil
}

// Validate verifies if ApplyOptions are valid and without conflicts.
func (o *ApplyOptions) Validate(cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		return cmdutil.UsageErrorf(cmd, "Unexpected args: %v", args)
	}

	if o.ForceConflicts && !o.ServerSideApply {
		return fmt.Errorf("--force-conflicts only works with --server-side")
	}

	if o.DryRunStrategy == cmdutil.DryRunClient && o.ServerSideApply {
		return fmt.Errorf("--dry-run=client doesn't work with --server-side (did you mean --dry-run=server instead?)")
	}

	if o.ServerSideApply && o.DeleteOptions.ForceDeletion {
		return fmt.Errorf("--force cannot be used with --server-side")
	}

	if o.DryRunStrategy == cmdutil.DryRunServer && o.DeleteOptions.ForceDeletion {
		return fmt.Errorf("--dry-run=server cannot be used with --force")
	}

	if o.All && len(o.Selector) > 0 {
		return fmt.Errorf("cannot set --all and --selector at the same time")
	}

	if o.Prune && !o.All && o.Selector == "" {
		return fmt.Errorf("all resources selected for prune without explicitly passing --all. To prune all resources, pass the --all flag. If you did not mean to prune all resources, specify a label selector")
	}

	return nil
}

func isIncompatibleServerError(err error) bool {
	// 415: Unsupported media type means we're talking to a server which doesn't
	// support server-side apply.
	if _, ok := err.(*errors.StatusError); !ok {
		// Non-StatusError means the error isn't because the server is incompatible.
		return false
	}
	return err.(*errors.StatusError).Status().Code == http.StatusUnsupportedMediaType
}

// GetObjects returns a (possibly cached) version of all the valid objects to apply
// as a slice of pointer to resource.Info and an error if one or more occurred.
// IMPORTANT: This function can return both valid objects AND an error, since
// "ContinueOnError" is set on the builder. This function should not be called
// until AFTER the "complete" and "validate" methods have been called to ensure that
// the ApplyOptions is filled in and valid.
func (o *ApplyOptions) GetObjects() ([]*resource.Info, error) {
	var err error = nil
	// o.objectsCached为false（说明还未从文件读取对象），则从文件或url中读取对象
	if !o.objectsCached {
		r := o.Builder.
			// 添加o.Builder.objectTyper（解析unstructured的group version kind和是否是认识的）和o.Builder.mapper
			Unstructured().
			// 设置o.Builder.schema字段
			Schema(o.Validator).
			// 设置o.Builder.continueOnError为true
			ContinueOnError().
			// 设置o.Builder.namespace为o.Namespace和o.Builder.defaultNamespace为true
			NamespaceParam(o.Namespace).DefaultNamespace().
			// filenameOptions.Filenames为url，则o.Builder.paths添加URLVisitor
			// filenameOptions.Filenames为"-"，则o.Builder.stream为true，o.Builder.stdinInUse为true，o.Builder.paths添加FileVisitor
			// filenameOptions.Filenames为文件或目录，则o.Builder.paths添加文件或目录下的文件，对应的FileVisitor
			// filenameOptions.Kustomize不为空，则o.Builder.paths添加KustomizeVisitor
			// enforceNamespace为true，则设置o.Builder.requireNamespace为true
			FilenameParam(o.EnforceNamespace, &o.DeleteOptions.FilenameOptions).
			// 当s不为空，且o.Builder.selectAll为false，则设置o.Builder.labelSelector设置为o.Selector
			LabelSelectorParam(o.Selector).
			// 设置o.Builder.flatten = true
			Flatten().
			// 先执行b.visitorResult()，生成Result（其中visitor包装好多层）
			// 然后再进行一系列的包装visitor
			// visitors执行顺序，是从上到下（fn执行顺序是从下往上）
			Do()
		// 执行r.visitor.Visit，层层的visitor执行后，筛选出的info集合，设置为r.info
		// 返回r.info
		o.objects, err = r.Infos()
		o.objectsCached = true
	}
	return o.objects, err
}

// SetObjects stores the set of objects (as resource.Info) to be
// subsequently applied.
func (o *ApplyOptions) SetObjects(infos []*resource.Info) {
	o.objects = infos
	o.objectsCached = true
}

// Run executes the `apply` command.
func (o *ApplyOptions) Run() error {

	if o.PreProcessorFn != nil {
		klog.V(4).Infof("Running apply pre-processor function")
		if err := o.PreProcessorFn(); err != nil {
			return err
		}
	}

	// Enforce CLI specified namespace on server request.
	if o.EnforceNamespace {
		o.VisitedNamespaces.Insert(o.Namespace)
	}

	// Generates the objects using the resource builder if they have not
	// already been stored by calling "SetObjects()" in the pre-processor.
	errs := []error{}
	// 使用visitor机制获取[]*resource.Info
	// 从文件中解析出对象，并转成[]*resource.Info
	infos, err := o.GetObjects()
	if err != nil {
		errs = append(errs, err)
	}
	if len(infos) == 0 && len(errs) == 0 {
		return fmt.Errorf("no objects passed to apply")
	}
	// Iterate through all objects, applying each one.
	for _, info := range infos {
		// 执行真正的apply动作（进行patch或create请求）
		if err := o.applyOneObject(info); err != nil {
			errs = append(errs, err)
		}
	}
	// If any errors occurred during apply, then return error (or
	// aggregate of errors).
	if len(errs) == 1 {
		return errs[0]
	}
	if len(errs) > 1 {
		return utilerrors.NewAggregate(errs)
	}

	if o.PostProcessorFn != nil {
		klog.V(4).Infof("Running apply post-processor function")
		// 如果设置了--output或-o命令行，且参数值不为"name"，则返回nil，不做任何事情
		// 否则，执行apply对象（apiserver返回）的打印
		// 命令行指定了--prune且--dry-run不为client，则执行删除所有apply操作资源命名空间下的、不在文件里（非当前apply操作）的对象、且为kubectl apply创建
		// 打印删除对象
		if err := o.PostProcessorFn(); err != nil {
			return err
		}
	}

	return nil
}

func (o *ApplyOptions) applyOneObject(info *resource.Info) error {
	// 如果info.Mapping.Scope为"namespace"，或info.Namespace有值，则将info.Namespace添加到o.VisitedNamespaces
	o.MarkNamespaceVisited(info)

	// 添加info.Object的annotations["kubernetes.io/change-cause"]为{命令行路径}+{non-flag arguments...}+{flag arguments (--xxx=xxx,-xxx=xxx)}
	if err := o.Recorder.Record(info.Object); err != nil {
		klog.V(4).Infof("error recording current command: %v", err)
	}

	// info.Name为空，且info.Object的generatedName不为空，则返回错误
	if len(info.Name) == 0 {
		metadata, _ := meta.Accessor(info.Object)
		generatedName := metadata.GetGenerateName()
		if len(generatedName) > 0 {
			return fmt.Errorf("from %s: cannot use generate name with apply", generatedName)
		}
	}

	helper := resource.NewHelper(info.Client, info.Mapping).
		DryRun(o.DryRunStrategy == cmdutil.DryRunServer).
		// o.FieldManager
		// 设置了--field-manager那就使用这个值
		// 否则，如果启用了server-side apply，那么field manager值为"kubectl"（为了向前兼容）
		// 其他情况，field manager值为"kubectl-client-side-apply"
		WithFieldManager(o.FieldManager)

	// 如果为server dry run
	if o.DryRunStrategy == cmdutil.DryRunServer {
		// Ensure the APIServer supports server-side dry-run for the resource,
		// otherwise fail early.
		// For APIServers that don't support server-side dry-run will persist
		// changes.
		// apiserver获得OpenAPISchema，并从中中查找是否支持"dryRun"方式对资源进行patch
		// 如果gvk是CRD资源，且支持"dryRun"方式对namespace资源进行patch，则返回nil
		// 否则，直接查找是否支持"dryRun"方式对gvk group-version-kind进行patch，支持返回nil，否则返回错误
		if err := o.DryRunVerifier.HasSupport(info.Mapping.GroupVersionKind); err != nil {
			return err
		}
	}

	if o.ServerSideApply {
		// Send the full object to be applied on the server side.
		data, err := runtime.Encode(unstructured.UnstructuredJSONScheme, info.Object)
		if err != nil {
			return cmdutil.AddSourceToErr("serverside-apply", info.Source, err)
		}

		options := metav1.PatchOptions{
			Force: &o.ForceConflicts,
		}
		obj, err := helper.Patch(
			info.Namespace,
			info.Name,
			types.ApplyPatchType,
			data,
			&options,
		)
		if err != nil {
			if isIncompatibleServerError(err) {
				err = fmt.Errorf("Server-side apply not available on the server: (%v)", err)
			}
			if errors.IsConflict(err) {
				err = fmt.Errorf(`%v
Please review the fields above--they currently have other managers. Here
are the ways you can resolve this warning:
* If you intend to manage all of these fields, please re-run the apply
  command with the `+"`--force-conflicts`"+` flag.
* If you do not intend to manage all of the fields, please edit your
  manifest to remove references to the fields that should keep their
  current managers.
* You may co-own fields by updating your manifest to match the existing
  value; in this case, you'll become the manager if the other manager(s)
  stop managing the field (remove it from their configuration).
See https://kubernetes.io/docs/reference/using-api/server-side-apply/#conflicts`, err)
			}
			return err
		}

		// 忽略所有错误
		// 更新info.Name为obj的name，更新info.Namespace为obj的namespace
		// 更新info.ResourceVersion为obj的ResourceVersion
		// 更新info.Object为obj
		info.Refresh(obj, true)

		// 如果object被删除，则打印出"Warning: Detected changes to resource %[1]s which is currently being deleted.\n"
		WarnIfDeleting(info.Object, o.ErrOut)

		// 将info.Object的uid添加到o.VisitedUids
		if err := o.MarkObjectVisited(info); err != nil {
			return err
		}

		// 设置了--output或-o命令行，且参数值不为"name"，则返回true
		if o.shouldPrintObject() {
			return nil
		}

		printer, err := o.ToPrinter("serverside-applied")
		if err != nil {
			return err
		}

		// 默认打印出
		// {resource}.{group}/{name} serverside-applied
		if err = printer.PrintObj(info.Object, o.Out); err != nil {
			return err
		}
		return nil
	}

	// client side apply

	// Get the modified configuration of the object. Embed the result
	// as an annotation in the modified configuration, so that it will appear
	// in the patch sent to the server.
	// 返回更新annotation["kubectl.kubernetes.io/last-applied-configuration"]的obj的encode byte，annotation["kubectl.kubernetes.io/last-applied-configuration"]为移除了annotation["kubectl.kubernetes.io/last-applied-configuration"]的obj
	// 这里是apply -f xxx.yaml时，xxx.yaml中的obj的annotation["kubectl.kubernetes.io/last-applied-configuration"]为xxx.yaml中的obj的encode byte，（文件中或http的内容）
	modified, err := util.GetModifiedConfiguration(info.Object, true, unstructured.UnstructuredJSONScheme)
	if err != nil {
		return cmdutil.AddSourceToErr(fmt.Sprintf("retrieving modified configuration from:\n%s\nfor:", info.String()), info.Source, err)
	}

	// 调用RESTClient获得apiserver中的object
	// 更新info.Object为apiserver获取的object，info.ResourceVersion为apiserver获取的object的ResourceVersion
	if err := info.Get(); err != nil {
		// 不是notFound错误
		if !errors.IsNotFound(err) {
			return cmdutil.AddSourceToErr(fmt.Sprintf("retrieving current configuration of:\n%s\nfrom server for:", info.String()), info.Source, err)
		}

		// errors.IsNotFound错误，说明info.Object不存在

		// Create the resource if it doesn't exist
		// First, update the annotation used by kubectl apply
		// 更新info.Object的annotation["kubectl.kubernetes.io/last-applied-configuration"]为移除annotation["kubectl.kubernetes.io/last-applied-configuration"]的info.Object的encode byte
		if err := util.CreateApplyAnnotation(info.Object, unstructured.UnstructuredJSONScheme); err != nil {
			return cmdutil.AddSourceToErr("creating", info.Source, err)
		}

		// --dry-run=server或--dry-run=none
		if o.DryRunStrategy != cmdutil.DryRunClient {
			// Then create the resource and skip the three-way merge
			// 如果--dry-run=server，在helper创建时候，已经设置了helper.ServerDryRun为true
			// 先确保obj的ResourceVersion为空
			// 然后进行obj创建
			// 返回创建的obj
			obj, err := helper.Create(info.Namespace, true, info.Object)
			if err != nil {
				return cmdutil.AddSourceToErr("creating", info.Source, err)
			}
			// 忽略所有错误
			// 更新info.Name为obj的name，更新info.Namespace为obj的namespace
			// 更新info.ResourceVersion为obj的ResourceVersion
			// 更新info.Object为obj
			info.Refresh(obj, true)
		}

		// 将info.Object的uid添加到o.VisitedUids
		if err := o.MarkObjectVisited(info); err != nil {
			return err
		}

		// 设置了--output或-o命令行，且参数值不为"name"，则返回true
		if o.shouldPrintObject() {
			return nil
		}

		// flag ToOptions
		// 根据--output or --template，返回最终的实现了ResourcePrinter接口的对象TypeSetterPrinter{delegate: p, Typer: scheme}
		// p可能是JSONPathPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\jsonpath.go
		// p可能是GoTemplatePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\gotemplate.go
		// p可能是NamePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\name.go
		// p可能是JSONPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\json.go
		// p可能是YAMLPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\yaml.go
		// p可能是OmitManagedFieldsPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\omitmanagedfields.go
		// 其中TypeSetterPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\printers.go
		printer, err := o.ToPrinter("created")
		if err != nil {
			return err
		}
		// 默认打印出
		// {resource}.{group}/{name} created
		if err = printer.PrintObj(info.Object, o.Out); err != nil {
			return err
		}
		return nil
	}

	// info.Object已经在apiserver中

	// 将info.Object的uid添加到o.VisitedUids
	if err := o.MarkObjectVisited(info); err != nil {
		return err
	}

	// --dry-run=server或--dry-run=none
	if o.DryRunStrategy != cmdutil.DryRunClient {
		metadata, _ := meta.Accessor(info.Object)
		annotationMap := metadata.GetAnnotations()
		// 如果没有annotation["kubectl.kubernetes.io/last-applied-configuration"]，则打印警告
		if _, ok := annotationMap[corev1.LastAppliedConfigAnnotation]; !ok {
			fmt.Fprintf(o.ErrOut, warningNoLastAppliedConfigAnnotation, info.ObjectName(), corev1.LastAppliedConfigAnnotation, o.cmdBaseName)
		}

		patcher, err := newPatcher(o, info, helper)
		if err != nil {
			return err
		}
		// info.Object是apiserver中的（annotation["kubectl.kubernetes.io/last-applied-configuration"]是老的）。modified是文件中内容，annotation["kubectl.kubernetes.io/last-applied-configuration"]是新的（移除了annotation["kubectl.kubernetes.io/last-applied-configuration"]的文件内容）
		// crd资源和aggregate apiserver资源，使用json merge patch
		// 内置资源使用strategic merge patch
		// 移除的字段（值为null），只看以前文件和当文件内容的patch
		// 增加和更新字段，只看apiserver和当前的文件内容的patch（明确字段值设置为nil，让服务器端移除字段）
		// crd资源和aggregate apiservewr资源，同时会判断是否满足preconditions，即不能对"apiVersion"、"kind"、"metadata.name"进行修改
		// 如果存在openapi接口（"/openapi/v2"），内置资源会使用进行strategic merge patch（使用openapi信息查找merge path策略），否则使用结构体里字段的json tag信息获取字段，并从tag获取"patchStrategy"和"patchMergeKey"值，查找merge path策略，进行strategic merge patch
		// 进行patch，如果成功直接返回。如果遇到冲突错误，则进行重试，重试次数为p.Retries（如果为0，则为5）
		// 重试次数用完后，还是冲突错误或IsInvalid错误，且命令行设置了--force，则进行删除然后创建
		// 先执行删除，然后再执行创建modified，如果执行modified创建失败，则创建original
		// 返回modified和创建完后返回的资源
		patchBytes, patchedObject, err := patcher.Patch(info.Object, modified, info.Source, info.Namespace, info.Name, o.ErrOut)
		if err != nil {
			return cmdutil.AddSourceToErr(fmt.Sprintf("applying patch:\n%s\nto:\n%v\nfor:", patchBytes, info), info.Source, err)
		}

		// ignoreError为true，则忽略所有错误
		// 更新info.Name为obj的name，更新info.Namespace为obj的namespace
		// 更新info.ResourceVersion为obj的ResourceVersion
		// 更新info.Object为obj
		info.Refresh(patchedObject, true)

		// 如果object被删除，则打印出"Warning: Detected changes to resource %[1]s which is currently being deleted.\n"
		WarnIfDeleting(info.Object, o.ErrOut)

		// patch为空，且没有设置--output或-o命令行，或设置了--output或-o命令行且参数值为"name"
		if string(patchBytes) == "{}" && !o.shouldPrintObject() {
			printer, err := o.ToPrinter("unchanged")
			if err != nil {
				return err
			}
			// 默认打印出
			// {resource}.{group}/{name} unchanged
			if err = printer.PrintObj(info.Object, o.Out); err != nil {
				return err
			}
			return nil
		}
	}

	// 设置了--output或-o命令行，且参数值不为"name"，则返回true
	if o.shouldPrintObject() {
		return nil
	}

	printer, err := o.ToPrinter("configured")
	if err != nil {
		return err
	}
	// 默认打印出
	// {resource}.{group}/{name} configured
	if err = printer.PrintObj(info.Object, o.Out); err != nil {
		return err
	}

	return nil
}

// 设置了--output或-o命令行，且参数值不为"name"，则返回true
func (o *ApplyOptions) shouldPrintObject() bool {
	// Print object only if output format other than "name" is specified
	shouldPrint := false
	output := *o.PrintFlags.OutputFormat
	shortOutput := output == "name"
	if len(output) > 0 && !shortOutput {
		shouldPrint = true
	}
	return shouldPrint
}

// 如果设置了--output或-o命令行，且参数值不为"name"，则返回nil，不做任何事情
// 获得apiserver返回的[]*resource.Info（可能create或patch）
// 如果超过一个对象，将所有info.Object组装成corev1.List，进行打印
// 如果只有一个对象，直接打印
func (o *ApplyOptions) printObjects() error {

	// 设置了--output或-o命令行，且参数值不为"name"，则返回true
	if !o.shouldPrintObject() {
		return nil
	}

	// 由于o.objectsCached为true，所以返回的[]*resource.Info是从apiserver返回的（可能create或patch）
	infos, err := o.GetObjects()
	if err != nil {
		return err
	}

	if len(infos) > 0 {
		// 根据--output or --template，返回最终的实现了ResourcePrinter接口的对象TypeSetterPrinter{delegate: p, Typer: scheme}
		// p可能是JSONPathPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\jsonpath.go
		// p可能是GoTemplatePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\gotemplate.go
		// p可能是NamePrinter在staging\src\k8s.io\cli-runtime\pkg\printers\name.go
		// p可能是JSONPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\json.go
		// p可能是YAMLPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\yaml.go
		// p可能是OmitManagedFieldsPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\omitmanagedfields.go
		// 其中TypeSetterPrinter在staging\src\k8s.io\cli-runtime\pkg\printers\printers.go
		printer, err := o.ToPrinter("")
		if err != nil {
			return err
		}

		objToPrint := infos[0].Object
		// 超过一个对象，将所有info.Object组装成corev1.List
		if len(infos) > 1 {
			objs := []runtime.Object{}
			for _, info := range infos {
				objs = append(objs, info.Object)
			}
			list := &corev1.List{
				TypeMeta: metav1.TypeMeta{
					Kind:       "List",
					APIVersion: "v1",
				},
				ListMeta: metav1.ListMeta{},
			}
			if err := meta.SetList(list, objs); err != nil {
				return err
			}

			objToPrint = list
		}
		// 打印对象
		if err := printer.PrintObj(objToPrint, o.Out); err != nil {
			return err
		}
	}

	return nil
}

// MarkNamespaceVisited keeps track of which namespaces the applied
// objects belong to. Used for pruning.
// 如果info.Mapping.Scope为"namespace"，或info.Namespace有值，则将info.Namespace添加到o.VisitedNamespaces
func (o *ApplyOptions) MarkNamespaceVisited(info *resource.Info) {
	// info.Mapping.Scope为"namespace"，或info.Namespace有值，则返回true
	if info.Namespaced() {
		o.VisitedNamespaces.Insert(info.Namespace)
	}
}

// MarkObjectVisited keeps track of UIDs of the applied
// objects. Used for pruning.
// 将info.Object的uid添加到o.VisitedUids
func (o *ApplyOptions) MarkObjectVisited(info *resource.Info) error {
	metadata, err := meta.Accessor(info.Object)
	if err != nil {
		return err
	}
	o.VisitedUids.Insert(string(metadata.GetUID()))
	return nil
}

// PrintAndPrunePostProcessor returns a function which meets the PostProcessorFn
// function signature. This returned function prints all the
// objects as a list (if configured for that), and prunes the
// objects not applied. The returned function is the standard
// apply post processor.
// 如果设置了--output或-o命令行，且参数值不为"name"，则返回nil，不做任何事情
// 否则，执行apply对象（apiserver返回）的打印
// 命令行指定了--prune且--dry-run不为client，则执行删除所有apply操作资源命名空间下的、不在文件里（非当前apply操作）的对象、且为kubectl apply创建
func (o *ApplyOptions) PrintAndPrunePostProcessor() func() error {

	return func() error {
		// 如果设置了--output或-o命令行，且参数值不为"name"，则返回nil，不做任何事情
		// 获得apiserver返回的[]*resource.Info（可能create或patch）
		// 如果超过一个对象，将所有info.Object组装成corev1.List，进行打印
		// 如果只有一个对象，直接打印
		if err := o.printObjects(); err != nil {
			return err
		}

		// 命令行指定了--prune
		if o.Prune {
			p := newPruner(o)
			// 默认的o.PruneResources（--prune-whitelist）为空
			// 如果pruneResources为空，则pruneResources使用默认的资源类型列表（ConfigMap、Endpoints、Namespace、PersistentVolumeClaim、PersistentVolume、Pod、ReplicationController、Secret、Service、Job、CronJob、Ingress、DaemonSet、Deployment、ReplicaSet、StatefulSet）
			// 遍历需要apply的namespaced资源（文件中的命名空间下的对象）的所有命名空间和命名空间下的pruneResources类型的所有资源
			// 资源不存在annotations["kubectl.kubernetes.io/last-applied-configuration"]，说明不是kubectl apply创建的资源，则跳过
			// 如果资源uid是kubectl apply操作的资源的uid，则跳过
			// 如果--dry-run不为client，则执行删除这个资源。否则，不做删除
			// 打印出删除资源（默认输出{resource}.{group}/{name} pruned）
			return p.pruneAll(o)
		}

		return nil
	}
}

const (
	// FieldManagerClientSideApply is the default client-side apply field manager.
	//
	// The default field manager is not `kubectl-apply` to distinguish from
	// server-side apply.
	FieldManagerClientSideApply = "kubectl-client-side-apply"
	// The default server-side apply field manager is `kubectl`
	// instead of a field manager like `kubectl-server-side-apply`
	// for backward compatibility to not conflict with old versions
	// of kubectl server-side apply where `kubectl` has already been the field manager.
	fieldManagerServerSideApply = "kubectl"
)

// GetApplyFieldManagerFlag gets the field manager for kubectl apply
// if it is not set.
//
// The default field manager is not `kubectl-apply` to distinguish between
// client-side and server-side apply.
// 设置了--field-manager那就使用这个值
// 否则，如果启用了server-side apply，那么field manager值为"kubectl"（为了向前兼容）
// 其他情况，field manager值为"kubectl-client-side-apply"
func GetApplyFieldManagerFlag(cmd *cobra.Command, serverSide bool) string {
	// The field manager flag was set
	if cmd.Flag("field-manager").Changed {
		return cmdutil.GetFlagString(cmd, "field-manager")
	}

	if serverSide {
		return fieldManagerServerSideApply
	}

	return FieldManagerClientSideApply
}

// WarnIfDeleting prints a warning if a resource is being deleted
// 如果object被删除，则打印出"Warning: Detected changes to resource %[1]s which is currently being deleted.\n"
func WarnIfDeleting(obj runtime.Object, stderr io.Writer) {
	metadata, _ := meta.Accessor(obj)
	if metadata != nil && metadata.GetDeletionTimestamp() != nil {
		// just warn the user about the conflict
		fmt.Fprintf(stderr, warningChangesOnDeletingResource, metadata.GetName())
	}
}
