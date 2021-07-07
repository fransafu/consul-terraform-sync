package tftmpl

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/consul-terraform-sync/internal/hcl2shim"
	"github.com/hashicorp/consul-terraform-sync/templates/hcltmpl"
	"github.com/hashicorp/consul-terraform-sync/version"
	goVersion "github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

const (
	// TerraformRequiredVersion is the version constraint pinned to the generated
	// root module to ensure compatibility across Sync, Terraform, and
	// modules.
	TerraformRequiredVersion = version.CompatibleTerraformVersionConstraint

	// RootFilename is the file name for the root module.
	RootFilename = "main.tf"

	// VarsFilename is the file name for the variable definitions in the root
	// module. This includes the required services variable and generated
	// provider variables based on CTS user configuration for the task.
	VarsFilename = "variables.tf"

	// ModuleVarsFilename is the file name for the variable definitions corresponding
	// to the input variables from a user that is specific to the task's module.
	ModuleVarsFilename = "variables.module.tf"

	// TFVarsFilename is the file name where the required Consul services input
	// variable is written to.
	TFVarsFilename = "terraform.tfvars"

	// TFVarsTmplFilename is the template file for TFVarsFilename. This is used
	// by hcat for monitoring service changes from Consul.
	TFVarsTmplFilename = "terraform.tfvars.tmpl"

	// ProviderTFVarsFilename is the file name for input variables for
	// configured Terraform providers. Generated provider input variables are
	// written in a separate file from terraform.tfvars because it may contain
	// sensitive or secret values.
	ProvidersTFVarsFilename = "providers.tfvars"
)

var (
	// RootPreamble is a warning message included to the beginning of the
	// generated root module files.
	RootPreamble = []byte(
		`# This file is generated by Consul Terraform Sync.
#
# The HCL blocks, arguments, variables, and values are derived from the
# operator configuration for Sync. Any manual changes to this file
# may not be preserved and could be overwritten by a subsequent update.
#
`)

	// TaskPreamble is the base format for task information included at the
	// beginning of generated module files.
	TaskPreamble = `# Task: %s
# Description: %s
`

	rootFileFuncs = map[string]tfFileFunc{
		RootFilename:       newMainTF,
		VarsFilename:       newVariablesTF,
		ModuleVarsFilename: newModuleVariablesTF,
	}

	tfvarsFileFuncs = map[string]tfFileFunc{
		TFVarsTmplFilename:      newTFVarsTmpl,
		ProvidersTFVarsFilename: newProvidersTFVars,
	}
)

// Task contains information for a Sync task. The Terraform driver
// interprets task values for determining the Terraform module.
type Task struct {
	Description string
	Name        string
	Source      string
	Version     string
}

type Service struct {
	// Consul service information
	Datacenter  string
	Description string
	Name        string
	Namespace   string
	Tag         string
	Filter      string

	// CTSUserDefinedMeta is user defined metadata that is configured by
	// operators for CTS to append to Consul service information to be used for
	// network infrastructure automation.
	CTSUserDefinedMeta map[string]string
}

type tfFileFunc func(io.Writer, string, *RootModuleInputData) error

// hcatQuery prepares formatted parameters that satisfies hcat
// query syntax to make Consul requests to /v1/health/service/:service
func (s Service) hcatQuery() string {
	var opts []string

	if s.Datacenter != "" {
		opts = append(opts, fmt.Sprintf("dc=%s", s.Datacenter))
	}

	if s.Namespace != "" {
		opts = append(opts, fmt.Sprintf("ns=%s", s.Namespace))
	}

	if s.Tag != "" {
		opts = append(opts, fmt.Sprintf(`\"%s\" in Service.Tags`, s.Tag))
	}

	if s.Filter != "" {
		filter := strings.ReplaceAll(s.Filter, `"`, `\"`)
		filter = strings.Trim(filter, "\n")
		opts = append(opts, fmt.Sprintf("%s", filter))
	}

	query := fmt.Sprintf("%q", s.Name)
	if len(opts) > 0 {
		query = query + ` "` + strings.Join(opts, `" "`) + `"`
	}
	return query
}

// RootModuleInputData is the input data used to generate the root module
type RootModuleInputData struct {
	TerraformVersion *goVersion.Version
	Backend          map[string]interface{}
	Providers        []hcltmpl.NamedBlock
	ProviderInfo     map[string]interface{}
	Services         []Service
	Task             Task
	Variables        hcltmpl.Variables
	Condition        Condition

	Path      string
	FilePerms os.FileMode

	// used for testing purposes whether or not to override existing files
	skipOverride bool

	backend *hcltmpl.NamedBlock
}

// init processes input data used to generate a Terraform root module. It
// converts the RootModuleInputData values into HCL objects compatible for
// Terraform configuration syntax.
func (d *RootModuleInputData) init() {
	if d.Backend != nil {
		block := hcltmpl.NewNamedBlock(d.Backend)
		d.backend = &block
	} else {
		d.Backend = make(map[string]interface{})
	}

	sort.Slice(d.Providers, func(i, j int) bool {
		return d.Providers[i].Name < d.Providers[j].Name
	})

	sort.Slice(d.Services, func(i, j int) bool {
		return d.Services[i].Name < d.Services[j].Name
	})
}

// InitRootModule generates the root module and writes the following files to
// disk.
//   always: main.tf, variables.tf, terraform.tfvars.tmpl
// conditionally: variables.module.tf, providers.tfvars
func InitRootModule(input *RootModuleInputData) error {
	input.init()

	fileFuncs := make(map[string]tfFileFunc)
	for k, v := range rootFileFuncs {
		fileFuncs[k] = v
	}
	for k, v := range tfvarsFileFuncs {
		fileFuncs[k] = v
	}
	return initModule(input, fileFuncs)
}

func initModule(input *RootModuleInputData, fileFuncs map[string]tfFileFunc) error {
	for filename, newFileFunc := range fileFuncs {
		if filename == ModuleVarsFilename && len(input.Variables) == 0 {
			// Skip variables.module.tf if there are no user input variables
			continue
		}

		filePath := filepath.Join(input.Path, filename)
		if fileExists(filePath) {
			if input.skipOverride {
				log.Printf("[DEBUG] (templates.tftmpl) %s in root module for task %q "+
					"already exists, skipping file creation", filename, input.Task.Name)
				continue
			}
			log.Printf("[INFO] (templates.tftmpl) overwriting %s in root module "+
				"for task %q", filename, input.Task.Name)
		}

		log.Printf("[DEBUG] (templates.tftmpl) creating %s in root module for "+
			"task %q: %s", filename, input.Task.Name, filePath)

		f, err := os.Create(filePath)
		if err != nil {
			log.Printf("[ERR] (templates.tftmpl) unable to create %s in root "+
				"module for %q: %s", filename, input.Task.Name, err)
			return err
		}
		defer f.Close()

		if err := f.Chmod(input.FilePerms); err != nil {
			log.Printf("[ERR] (templates.tftmpl) unable to change permissions "+
				"for %s in root module for %q: %s", filename, input.Task.Name, err)
			return err
		}

		if err := newFileFunc(f, filename, input); err != nil {
			log.Printf("[ERR] (templates.tftmpl) error writing content for %s in "+
				"root module for %q: %s", filename, input.Task.Name, err)
			return err
		}

		f.Sync()
	}

	return nil
}

// newMainTF writes content used for main.tf of a Terraform root module.
func newMainTF(w io.Writer, filename string, input *RootModuleInputData) error {
	err := writePreamble(w, input.Task, filename)
	if err != nil {
		return err
	}

	hclFile := hclwrite.NewEmptyFile()
	rootBody := hclFile.Body()
	rootBody.AppendNewline()
	appendRootTerraformBlock(rootBody, input.backend, input.ProviderInfo)
	rootBody.AppendNewline()
	appendRootProviderBlocks(rootBody, input.Providers)
	rootBody.AppendNewline()
	appendRootModuleBlock(rootBody, input.Task, input.Condition,
		input.Variables.Keys())

	// Format the file before writing
	content := hclFile.Bytes()
	content = hclwrite.Format(content)
	_, err = w.Write(content)
	return err
}

// appendRootTerraformBlock appends the Terraform block with version constraint
// and backend.
func appendRootTerraformBlock(body *hclwrite.Body, backend *hcltmpl.NamedBlock,
	providerInfo map[string]interface{}) {

	tfBlock := body.AppendNewBlock("terraform", nil)
	tfBody := tfBlock.Body()
	tfBody.SetAttributeValue("required_version", cty.StringVal(TerraformRequiredVersion))

	if len(providerInfo) != 0 {
		requiredProvidersBody := tfBody.AppendNewBlock("required_providers", nil).Body()
		for _, pName := range sortedKeys(providerInfo) {
			info, ok := providerInfo[pName]
			if ok {
				requiredProvidersBody.SetAttributeValue(pName, hcl2shim.HCL2ValueFromConfigValue(info))
			}
		}
	}

	// Configure the Terraform backend within the Terraform block
	if backend == nil || backend.Name == "" {
		return
	}
	backendBody := tfBody.AppendNewBlock("backend", []string{backend.Name}).Body()
	backendAttrs := backend.SortedAttributes()
	for _, attr := range backendAttrs {
		backendBody.SetAttributeValue(attr, backend.Variables[attr])
	}
}

// appendRootProviderBlocks appends Terraform provider blocks for the providers
// the task requires.
func appendRootProviderBlocks(body *hclwrite.Body, providers []hcltmpl.NamedBlock) {
	lastIdx := len(providers) - 1
	for i, p := range providers {
		providerBody := body.AppendNewBlock("provider", []string{p.Name}).Body()

		// Convert user provider config to provider block arguments from variables
		// and sort the attributes / sub-attributes for consistency. Format
		// depends on if attribute is type object or not:
		// attr = var.<providerName>.<attr>
		// objAttr {
		//    subAttr = var.<providerName>.<objAttr>.<subAttr>
		// }
		providerAttrs := p.SortedAttributes()
		for _, attr := range providerAttrs {
			// Drop the alias meta attribute. Each provider instance will be ran as
			// a separate task
			if attr == "alias" {
				continue
			}
			// auto_commit is an internal setting
			if attr == "auto_commit" {
				continue
			}

			val := p.Variables[attr]
			if val.Type().IsObjectType() {
				subAttrs := make(map[string]interface{})
				for k := range val.AsValueMap() {
					subAttrs[k] = true
				}

				objProviderBody := providerBody.AppendNewBlock(attr, nil).Body()
				for _, subAttr := range sortedKeys(subAttrs) {
					objProviderBody.SetAttributeTraversal(subAttr, hcl.Traversal{
						hcl.TraverseRoot{Name: "var"},
						hcl.TraverseAttr{Name: p.Name},
						hcl.TraverseAttr{Name: attr},
						hcl.TraverseAttr{Name: subAttr},
					})
				}
				continue
			}

			providerBody.SetAttributeTraversal(attr, hcl.Traversal{
				hcl.TraverseRoot{Name: "var"},
				hcl.TraverseAttr{Name: p.Name},
				hcl.TraverseAttr{Name: attr},
			})
		}
		if i != lastIdx {
			body.AppendNewline()
		}
	}
}

// appendRootModuleBlock appends a Terraform module block for the task
func appendRootModuleBlock(body *hclwrite.Body, task Task, cond Condition,
	varNames []string) {

	// Add user description for task above the module block
	if task.Description != "" {
		appendComment(body, task.Description)
	}

	moduleBlock := body.AppendNewBlock("module", []string{task.Name})
	moduleBody := moduleBlock.Body()

	moduleBody.SetAttributeValue("source", cty.StringVal(task.Source))

	if len(task.Version) > 0 {
		moduleBody.SetAttributeValue("version", cty.StringVal(task.Version))
	}

	moduleBody.SetAttributeTraversal("services", hcl.Traversal{
		hcl.TraverseRoot{Name: "var"},
		hcl.TraverseAttr{Name: "services"},
	})

	if cond != nil && cond.SourceIncludesVariable() {
		cond.appendModuleAttribute(moduleBody)
	}

	if len(varNames) != 0 {
		moduleBody.AppendNewline()
	}
	for _, name := range varNames {
		moduleBody.SetAttributeTraversal(name, hcl.Traversal{
			hcl.TraverseRoot{Name: "var"},
			hcl.TraverseAttr{Name: name},
		})
	}
}

// appendComment appends a single HCL comment line
func appendComment(b *hclwrite.Body, comment string) {
	b.AppendUnstructuredTokens(hclwrite.Tokens{{
		Type:  hclsyntax.TokenComment,
		Bytes: []byte(fmt.Sprintf("# %s", comment)),
	}})
	b.AppendNewline()
}

func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]interface{}) []string {
	sorted := make([]string, 0, len(m))
	for key := range m {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	return sorted
}

// writePreamble writes a preamble to the writer for generated root module
// files. Each preamble includes task information.
func writePreamble(w io.Writer, task Task, filename string) error {
	_, err := w.Write(RootPreamble)
	if err != nil {
		// The preamble is not required for TF config files to be usable. So any
		// errors here we'll just log and continue.
		log.Printf("[WARN] (templates.tftmpl) unable to write preamble warning to %q",
			filename)
	}

	// Adding the task name to generated files guarantees the file content is
	// distinct across tasks. hcat manages unique templates based on IDs which
	// are generated by the hash of the content. Unique content for templates
	// across all tasks is necessary for the terraform.tfvars.tmpl template file
	// to be considered as different templates for the edge case where tasks have
	// similar services and providers used. Otherwise, only one of the identical
	// template files will render by hcat causing CTS to indefinitely wait.
	_, err = fmt.Fprintf(w, TaskPreamble, task.Name, task.Description)
	if err != nil {
		log.Printf("[WARN] (templates.tftmpl) unable to write task preamble warning to %q",
			filename)
	}
	return err
}
