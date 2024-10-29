package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/go-openapi/loads"
	"github.com/go-openapi/spec"
	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"

	"github.com/operator-framework/operator-controller/cmd/registryv1-to-helm/internal/parametrize"
	"github.com/operator-framework/operator-controller/internal/rukpak/convert"
	"github.com/operator-framework/operator-controller/internal/rukpak/util"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:  "registryv1-to-helm <registry+v1-directory-path> [output-path]",
		Args: cobra.RangeArgs(1, 2),
		Run: func(cmd *cobra.Command, args []string) {
			registryv1Path := args[0]

			saveDir := "."
			if len(args) == 2 {
				saveDir = args[1]
			}

			k8sAPIVersion := getKubernetesAPIVersion()
			openapiSchemaURL := fmt.Sprintf("https://raw.githubusercontent.com/kubernetes/kubernetes/refs/%s/api/openapi-spec/v3/apis__apps__v1_openapi.json", k8sAPIVersion)

			// Do the conversion
			chrt, err := convertRegistryV1ToHelm(cmd.Context(), os.DirFS(registryv1Path), openapiSchemaURL)
			if err != nil {
				cmd.PrintErr(err)
				os.Exit(1)
			}

			if err := chartutil.SaveDir(chrt, saveDir); err != nil {
				cmd.PrintErr(err)
				os.Exit(1)
			}
			cmd.Printf("Chart saved to %s\n", filepath.Join(saveDir, chrt.Metadata.Name))
		},
	}
	return cmd
}

func convertRegistryV1ToHelm(ctx context.Context, rv1FS fs.FS, openapiSchemaURL string) (*chart.Chart, error) {
	rv1, err := convert.LoadRegistryV1(ctx, rv1FS)
	if err != nil {
		return nil, err
	}

	convertMaintainers := func(maintainers []v1alpha1.Maintainer) []*chart.Maintainer {
		var chrtMaintainers []*chart.Maintainer
		for _, maintainer := range maintainers {
			chrtMaintainers = append(chrtMaintainers, &chart.Maintainer{
				Name:  maintainer.Name,
				Email: maintainer.Email,
			})
		}
		return chrtMaintainers
	}

	chrt := &chart.Chart{
		Metadata: &chart.Metadata{
			APIVersion:  "v2",
			Name:        rv1.PackageName,
			Version:     rv1.CSV.Spec.Version.String(),
			Description: rv1.CSV.Spec.Description,
			Keywords:    rv1.CSV.Spec.Keywords,
			Maintainers: convertMaintainers(rv1.CSV.Spec.Maintainers),
			Annotations: rv1.CSV.Annotations,
		},
	}
	if rv1.CSV.Spec.MinKubeVersion != "" {
		chrt.Metadata.KubeVersion = fmt.Sprintf(">= %s", rv1.CSV.Spec.MinKubeVersion)
	}

	var errs []error

	/////////////////
	// Setup schema
	/////////////////
	valuesSchema, err := newValuesSchemaFile(rv1.CSV, openapiSchemaURL)
	if err != nil {
		errs = append(errs, err)
	}
	chrt.Schema = valuesSchema

	/////////////////
	// Setup helpers
	/////////////////
	chrt.Templates = append(chrt.Templates, newTargetNamespacesTemplateHelper(rv1.CSV))
	chrt.Templates = append(chrt.Templates, newDeploymentsTemplateHelper(rv1.CSV))

	/////////////////
	// Setup RBAC
	/////////////////
	chrt.Templates = append(chrt.Templates, newServiceAccountFiles(rv1.CSV)...)

	permissionFiles, err := newPermissionsFiles(rv1.CSV)
	if err != nil {
		return nil, err
	}
	chrt.Templates = append(chrt.Templates, permissionFiles...)

	clusterPermissionFiles, err := newClusterPermissionsFiles(rv1.CSV)
	for _, rbacFile := range clusterPermissionFiles {
		chrt.Templates = append(chrt.Templates, rbacFile)
	}

	///////////////////
	// Setup Deployment
	///////////////////
	deploymentFiles, err := newDeploymentFiles(rv1.CSV)
	if err != nil {
		return nil, err
	}
	chrt.Templates = append(chrt.Templates, deploymentFiles...)

	//////////////////////////////////////
	// Add all other static bundle objects
	//////////////////////////////////////
	for _, obj := range rv1.Others {
		f, err := newFile(obj)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		chrt.Templates = append(chrt.Templates, f)
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return chrt, nil
}

func newFile(u unstructured.Unstructured) (*chart.File, error) {
	u.SetNamespace("")
	fileName := fileNameForObject(u.GroupVersionKind().GroupKind(), u.GetName())
	yamlData, err := yaml.Marshal(u.Object)
	if err != nil {
		return nil, fmt.Errorf("marshal object for file %q: %w", fileName, err)
	}
	return &chart.File{
		Name: fileName,
		Data: yamlData,
	}, nil
}

func newServiceAccountFiles(csv v1alpha1.ClusterServiceVersion) []*chart.File {
	saNames := sets.New[string]()

	for _, perms := range csv.Spec.InstallStrategy.StrategySpec.Permissions {
		saNames.Insert(perms.ServiceAccountName)
	}
	for _, perms := range csv.Spec.InstallStrategy.StrategySpec.ClusterPermissions {
		saNames.Insert(perms.ServiceAccountName)
	}
	files := make([]*chart.File, 0, len(saNames))
	for _, saName := range sets.List(saNames) {
		yamlData := []byte(fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: %s
  namespace: {{ .Release.Namespace }}
`, saName))
		files = append(files, &chart.File{
			Name: fileNameForObject(schema.GroupKind{Group: "", Kind: "ServiceAccount"}, saName),
			Data: yamlData,
		})
	}
	return files
}

// newPermissionsFiles returns a list of template files for the necessary RBAC permissions from the CSV's permissions.
// A few implementation notes about how the helm templating should be done:
//   - The serviceAccountName and the rules are provided by the CSV.
//   - if .Values.watchNamespaces corresponds to watching all namespaces, then ClusterRole and ClusterRoleBindings should be used
//   - if .Values.watchNamespaces corresponds to watching specific namespaces, then Role and RoleBindings should be used. In this case,
//     the Role and RoleBinding should be created in each namespace in .Values.watchNamespaces, and the RoleBinding subject to be the
//     ServiceAccount in the install namespace.
func newPermissionsFiles(csv v1alpha1.ClusterServiceVersion) ([]*chart.File, error) {
	var (
		files []*chart.File
		errs  []error
	)
	for _, perms := range csv.Spec.InstallStrategy.StrategySpec.Permissions {
		rulesMap := map[string][]rbacv1.PolicyRule{
			"rules": perms.Rules,
		}
		yamlRules, err := yaml.Marshal(rulesMap)
		if err != nil {
			errs = append(errs, fmt.Errorf("marshal rules for service account %q: %w", perms.ServiceAccountName, err))
			continue
		}
		name := generateName(csv.Name, perms)
		yamlData := []byte(fmt.Sprintf(`{{- $installNamespace := .Release.Namespace -}}
{{- $targetNamespaces := include "olm.targetNamespaces" . -}}
{{- $name := %[1]q -}}
{{- $serviceAccountName := %[2]q -}}

{{- $promoteToClusterRole := (eq $targetNamespaces "") -}}

{{- define "rules-%[1]s" -}}
%[3]s
{{- end -}}

{{- if $promoteToClusterRole -}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ $name }}
{{ template "rules-%[1]s" }}
{{- else -}}
{{- range (split "," $targetNamespaces) -}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ $name }}
  namespace: {{ . }}
{{ template "rules-%[1]s" }}
{{ end -}}
{{- end -}}

{{- if $promoteToClusterRole -}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ $name }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ $name }}
subjects:
- kind: ServiceAccount
  name: {{ $serviceAccountName }}
  namespace: {{ $installNamespace }}
{{- else -}}
{{- range (split "," $targetNamespaces) -}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ $name }}
  namespace: {{ . }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ $name }}
subjects:
- kind: ServiceAccount
  name: {{ $serviceAccountName }}
  namespace: {{ $installNamespace }}
{{ end -}}
{{- end -}}
`, name, perms.ServiceAccountName, yamlRules))
		files = append(files, &chart.File{
			Name: fmt.Sprintf("templates/rbac.authorization.k8s.io-csv.permissions-%s.yaml", perms.ServiceAccountName),
			Data: yamlData,
		})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
}

// newClusterPermissionsFiles returns a list of template files for the necessary RBAC permissions from the CSV's clusterPermissions.
// A few implementation notes about how the helm templating should be done:
//   - The serviceAccountName and the rules are provided by the CSV.
//   - clusterPermissions from the CSV are always translated to ClusterRole and ClusterRoleBindings.

func newClusterPermissionsFiles(csv v1alpha1.ClusterServiceVersion) ([]*chart.File, error) {
	var (
		files []*chart.File
		errs  []error
	)
	for _, perms := range csv.Spec.InstallStrategy.StrategySpec.ClusterPermissions {
		rulesMap := map[string][]rbacv1.PolicyRule{
			"rules": perms.Rules,
		}
		yamlRules, err := yaml.Marshal(rulesMap)
		if err != nil {
			errs = append(errs, fmt.Errorf("marshal rules for service account %q: %w", perms.ServiceAccountName, err))
			continue
		}

		name := generateName(csv.Name, perms)
		yamlData := []byte(fmt.Sprintf(`{{- $name := %q -}}
{{- $serviceAccountName := %q -}}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ $name }}
%s
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ $name }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ $name }}
subjects:
- kind: ServiceAccount
  name: {{ $serviceAccountName }}
  namespace: {{ .Release.Namespace }}
`, name, perms.ServiceAccountName, yamlRules))
		files = append(files, &chart.File{
			Name: fmt.Sprintf("templates/rbac.authorization.k8s.io-csv.clusterPermissions-%s.yaml", perms.ServiceAccountName),
			Data: yamlData,
		})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
}

func newDeploymentsTemplateHelper(csv v1alpha1.ClusterServiceVersion) *chart.File {
	var sb bytes.Buffer
	for _, depSpec := range csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs {
		snippets := map[string]string{}

		affinityJSON, _ := json.Marshal(depSpec.Spec.Template.Spec.Affinity)
		snippets["affinity"] = string(affinityJSON)

		nodeSelectorJSON, _ := json.Marshal(depSpec.Spec.Template.Spec.NodeSelector)
		snippets["nodeSelector"] = string(nodeSelectorJSON)

		selectorJSON, _ := json.Marshal(depSpec.Spec.Selector)
		snippets["selector"] = string(selectorJSON)

		tolerationsJSON, _ := json.Marshal(depSpec.Spec.Template.Spec.Tolerations)
		snippets["tolerations"] = string(tolerationsJSON)

		volumesJSON, _ := json.Marshal(depSpec.Spec.Template.Spec.Volumes)
		snippets["volumes"] = string(volumesJSON)

		for _, container := range depSpec.Spec.Template.Spec.Containers {
			envJSON, _ := json.Marshal(container.Env)
			snippets[fmt.Sprintf("%s.env", container.Name)] = string(envJSON)

			envFromJSON, _ := json.Marshal(container.EnvFrom)
			snippets[fmt.Sprintf("%s.envFrom", container.Name)] = string(envFromJSON)

			resourcesJSON, _ := json.Marshal(container.Resources)
			snippets[fmt.Sprintf("%s.resources", container.Name)] = string(resourcesJSON)

			volumeMountsJSON, _ := json.Marshal(container.VolumeMounts)
			snippets[fmt.Sprintf("%s.volumeMounts", container.Name)] = string(volumeMountsJSON)
		}

		for _, fieldName := range sets.List(sets.KeySet(snippets)) {
			sb.WriteString(fmt.Sprintf(`{{- define "deployment.%s.%s" -}}
%s
{{- end -}}

`, depSpec.Name, fieldName, strings.TrimSpace(snippets[fieldName])))
		}
	}
	return &chart.File{
		Name: "templates/_helpers.deployments.tpl",
		Data: sb.Bytes(),
	}
}

func newTargetNamespacesTemplateHelper(csv v1alpha1.ClusterServiceVersion) *chart.File {
	watchNsSetup := getWatchNamespacesSchema(csv)
	defineTemplate := `{{- define "olm.targetNamespaces" -}}
%s
{{- end -}}
`

	if !watchNsSetup.IncludeField {
		templateHelperDefaultValue := fmt.Sprintf(`{{- %s -}}`, watchNsSetup.TemplateHelperDefaultValue)
		return &chart.File{
			Name: "templates/_helpers.tpl",
			Data: []byte(fmt.Sprintf(defineTemplate, templateHelperDefaultValue)),
		}
	}

	switch watchNsSetup.FieldName {
	case "installMode":
		value := `{{- $installMode := .Values.installMode -}}`
		if !watchNsSetup.Required {
			value += fmt.Sprintf(`
{{- if not $installMode -}}
  {{- $installMode = %s -}}
{{- end -}}`, watchNsSetup.TemplateHelperDefaultValue)
		}
		value += fmt.Sprintf(`
{{- if eq $installMode "AllNamespaces" -}}
  {{- "" -}}
{{- else if eq $installMode "OwnNamespace" -}}
  {{- .Release.Namespace -}}
{{- else -}}
  {{- fail (printf "Unsupported install mode: %%s" $installMode) -}}
{{- end -}}`)

		return &chart.File{
			Name: "templates/_helpers.tpl",
			Data: []byte(fmt.Sprintf(defineTemplate, value)),
		}
	case "watchNamespace":
		value := `{{- $targetNamespaces := .Values.watchNamespace -}}`
		if !watchNsSetup.Required {
			value += fmt.Sprintf(`
{{- if not $targetNamespaces -}}
  {{- $targetNamespaces = %s -}}
{{- end -}}`, watchNsSetup.TemplateHelperDefaultValue)
		}
		if !watchNsSetup.AllowReleaseNamespace {
			value += `
{{- if eq $targetNamespaces .Release.Namespace -}}
  {{- fail "OwnNamespace mode is not supported. watchNamespace cannot be set to the install namespace" -}}
{{- end -}}`
		}
		value += `
{{- $targetNamespaces -}}`
		return &chart.File{
			Name: "templates/_helpers.tpl",
			Data: []byte(fmt.Sprintf(defineTemplate, value)),
		}
	case "watchNamespaces":
		value := `{{- $targetNamespaces := .Values.watchNamespaces -}}`
		if !watchNsSetup.Required {
			value += fmt.Sprintf(`
{{- if not $targetNamespaces -}}
  {{- $targetNamespaces = %s -}}
{{- end -}}`, watchNsSetup.TemplateHelperDefaultValue)
		}
		if !watchNsSetup.AllowReleaseNamespace {
			value += `
{{- if has .Release.Namespace $targetNamespaces -}}
  {{- fail "OwnNamespace mode is not supported. watchNamespaces cannot include the install namespace" -}}
{{- end -}}`
		}
		value += `
{{- join "," $targetNamespaces -}}`
		return &chart.File{
			Name: "templates/_helpers.targetNamespaces.tpl",
			Data: []byte(fmt.Sprintf(defineTemplate, value)),
		}
	}

	return nil
}

func newDeploymentFiles(csv v1alpha1.ClusterServiceVersion) ([]*chart.File, error) {
	var (
		files []*chart.File
		errs  []error
	)

	csvMetadataAnnotations := csv.GetAnnotations()
	for _, depSpec := range csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs {
		dep := appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:   depSpec.Name,
				Labels: depSpec.Label,
			},
			Spec: depSpec.Spec,
		}
		dep.Spec.Template.SetAnnotations(mergeMaps(dep.Spec.Template.Annotations, csvMetadataAnnotations))
		delete(dep.Spec.Template.Annotations, "olm.targetNamespaces")

		// Hardcode the deployment with RevisionHistoryLimit=1; this mimics
		// What OLMv0 does, and is thus a reasonable default.
		dep.Spec.RevisionHistoryLimit = ptr.To(int32(1))

		dep.Spec.Template.Spec.Affinity = nil
		dep.Spec.Template.Spec.NodeSelector = nil
		dep.Spec.Selector = nil
		dep.Spec.Template.Spec.Tolerations = nil
		dep.Spec.Template.Spec.Volumes = nil

		for i := range dep.Spec.Template.Spec.Containers {
			dep.Spec.Template.Spec.Containers[i].Env = nil
			dep.Spec.Template.Spec.Containers[i].EnvFrom = nil
			dep.Spec.Template.Spec.Containers[i].Resources = corev1.ResourceRequirements{}
			dep.Spec.Template.Spec.Containers[i].VolumeMounts = nil
		}

		var parametrizeInstructions []parametrize.Instruction
		for _, f := range []func(dep appsv1.Deployment) []parametrize.Instruction{
			// set annotations["olm.targetNamespaces"] to the value of the helper
			mergeTargetNamespaces,

			// inject deployment-level and pod-level configuration derived from values
			overrideAffinity,
			overrideNodeSelector,
			overrideSelector,
			mergeAndDeduplicateTolerationsByEquality,
			mergeAndOverrideVolumesByName,

			// inject container-level configuration derived from values
			//mergeButNotOverrideAnnotationsByName,
			mergeAndOverrideEnvByName,
			//mergeAndDeduplicateEnvFromByEquality,
			//overrideResources,
			//mergeAndOverrideVolumesMountsByName,
		} {
			parametrizeInstructions = append(parametrizeInstructions, f(dep)...)
		}

		// Execute the parametrize instructions on the deployment.
		var u unstructured.Unstructured
		uObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&dep)
		if err != nil {
			errs = append(errs, fmt.Errorf("convert deployment %q to unstructured: %w", dep.GetName(), err))
			continue
		}
		u.Object = uObj
		u.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))

		yamlData, err := parametrize.Execute(u, parametrizeInstructions...)
		if err != nil {
			errs = append(errs, fmt.Errorf("parametrize deployment %q: %w", dep.GetName(), err))
			continue
		}

		files = append(files, &chart.File{
			Name: fileNameForObject(schema.GroupKind{Group: "apps", Kind: "Deployment"}, dep.GetName()),
			Data: yamlData,
		})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
}

func mergeTargetNamespaces(dep appsv1.Deployment) []parametrize.Instruction {
	dict := `(dict "olm.targetNamespaces" (include "olm.targetNamespaces" .))`
	if len(dep.Spec.Template.Annotations) == 0 {
		return []parametrize.Instruction{parametrize.Pipeline(fmt.Sprintf(`%s | toJson`, dict), "spec.template.metadata.annotations")}
	}
	return []parametrize.Instruction{parametrize.MergeBlock(fmt.Sprintf(`dict "annotations" %s`, dict), "spec.template.metadata.annotations")}
}

func overrideAffinity(dep appsv1.Deployment) []parametrize.Instruction {
	return []parametrize.Instruction{parametrize.Pipeline(fmt.Sprintf(`if .Values.affinity }}{{ .Values.affinity | toJson}}{{ else }}{{ include "deployment.%s.affinity" . }}{{ end`, dep.Name), "spec.template.spec.affinity")}
}

func overrideNodeSelector(dep appsv1.Deployment) []parametrize.Instruction {
	return []parametrize.Instruction{parametrize.Pipeline(fmt.Sprintf(`if .Values.nodeSelector }}{{ .Values.nodeSelector | toJson}}{{ else }}{{ include "deployment.%s.nodeSelector" . }}{{ end`, dep.Name), "spec.template.spec.nodeSelector")}
}

func overrideSelector(dep appsv1.Deployment) []parametrize.Instruction {
	return []parametrize.Instruction{parametrize.Pipeline(fmt.Sprintf(`if .Values.selector }}{{ .Values.selector | toJson}}{{ else }}{{ include "deployment.%s.selector" . }}{{ end`, dep.Name), "spec.selector")}
}

func mergeAndDeduplicateTolerationsByEquality(dep appsv1.Deployment) []parametrize.Instruction {
	return []parametrize.Instruction{parametrize.Pipeline(fmt.Sprintf(`concat (default (list) .Values.tolerations) (include "deployment.%s.tolerations" . | fromJsonArray) | uniq | toJson`, dep.Name), "spec.template.spec.tolerations")}
}

func mergeAndOverrideVolumesByName(dep appsv1.Deployment) []parametrize.Instruction {
	return []parametrize.Instruction{parametrize.Pipeline(fmt.Sprintf(`$volumes := default (list) .Values.volumes -}}
{{- $volumeNames := list -}}
{{- range $volumes -}}{{- $volumeNames = append $volumeNames .name -}}{{- end -}}
{{- range (include "deployment.%[1]s.volumes" . | fromJsonArray) -}}
  {{- if not (has .name $volumeNames) -}}
    {{- $volumes = append $volumes . -}}
    {{- $volumeNames = append $volumeNames .name -}}
  {{- end -}}
{{- end -}}
{{- $volumes | toJson`, dep.Name), "spec.template.spec.volumes")}
}

func mergeAndOverrideEnvByName(dep appsv1.Deployment) []parametrize.Instruction {
	var instructions []parametrize.Instruction
	for i, container := range dep.Spec.Template.Spec.Containers {
		instructions = append(instructions, parametrize.Pipeline(fmt.Sprintf(`$envs := default (list) .Values.env -}}
{{- $envNames := list -}}
{{- range $envs -}}{{- $envNames = append $envNames .name -}}{{- end -}}
{{- range (include "deployment.%[1]s.%[2]s.env" . | fromJsonArray) -}}
  {{- if not (has .name $envNames) -}}
    {{- $envs = append $envs . -}}
    {{- $envNames = append $envNames .name -}}
  {{- end -}}
{{- end -}}
{{- $envs | toJson`, dep.Name, container.Name), fmt.Sprintf("spec.template.spec.containers.%d.env", i)))
	}
	return instructions
}

func fileNameForObject(gk schema.GroupKind, name string) string {
	if gk.Group == "" {
		gk.Group = "core"
	}
	return fmt.Sprintf("templates/%s.%s-%s.yaml", gk.Group, gk.Kind, name)
}

const maxNameLength = 63

func generateName(base string, o interface{}) string {
	hashStr, err := util.DeepHashObject(o)
	if err != nil {
		panic(err)
	}
	if len(base)+len(hashStr) > maxNameLength {
		base = base[:maxNameLength-len(hashStr)-1]
	}
	if base == "" {
		return hashStr
	}
	return fmt.Sprintf("%s-%s", base, hashStr)
}

func newValuesSchemaFile(csv v1alpha1.ClusterServiceVersion, openapiSchemaURL string) ([]byte, error) {
	valuesSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Schema:               "http://json-schema.org/schema#",
			Type:                 spec.StringOrArray{"object"},
			Description:          fmt.Sprintf("Values for the %s Helm chart.", csv.GetName()),
			Properties:           map[string]spec.Schema{},
			AdditionalProperties: &spec.SchemaOrBool{Allows: false},
		},
	}

	appsV1Definitions, err := getAppsV1Definitions(openapiSchemaURL)
	if err != nil {
		return nil, err
	}
	valuesSchema.Definitions = appsV1Definitions

	watchNsConfig := getWatchNamespacesSchema(csv)
	if watchNsConfig.IncludeField {
		valuesSchema.Properties[watchNsConfig.FieldName] = *watchNsConfig.Schema
		if watchNsConfig.Required {
			valuesSchema.Required = append(valuesSchema.Required, watchNsConfig.FieldName)
		}
	}

	valuesSchema.Properties["selector"] = *spec.RefProperty("#/definitions/io.k8s.api.apps.v1.DeploymentSpec/properties/selector")
	valuesSchema.Properties["nodeSelector"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.PodSpec/properties/nodeSelector")
	valuesSchema.Properties["tolerations"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.PodSpec/properties/tolerations")
	valuesSchema.Properties["resources"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.Container/properties/resources")
	valuesSchema.Properties["envFrom"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.Container/properties/envFrom")
	valuesSchema.Properties["env"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.Container/properties/env")
	valuesSchema.Properties["volumes"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.PodSpec/properties/volumes")
	valuesSchema.Properties["volumeMounts"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.Container/properties/volumeMounts")
	valuesSchema.Properties["affinity"] = *spec.RefProperty("#/definitions/io.k8s.api.core.v1.PodSpec/properties/affinity")
	valuesSchema.Properties["annotations"] = *spec.RefProperty("#/definitions/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta/properties/annotations")

	var jsonDataBuf bytes.Buffer
	enc := json.NewEncoder(&jsonDataBuf)
	enc.SetIndent("", "    ")
	if err := enc.Encode(valuesSchema); err != nil {
		return nil, err
	}
	return jsonDataBuf.Bytes(), nil
}

type watchNamespaceSchemaSetup struct {
	IncludeField               bool
	FieldName                  string
	Required                   bool
	Schema                     *spec.Schema
	TemplateHelperDefaultValue string
	AllowReleaseNamespace      bool
}

// watchNamespacesSchemaProperties return the OpenAPI v3 schema for the field that controls the namespace or namespaces to watch.
// It returns the schema as a byte slice, a boolean indicating if the field is required. If the returned byte slice is nil,
// the field should not be included in the schema.
func getWatchNamespacesSchema(csv v1alpha1.ClusterServiceVersion) watchNamespaceSchemaSetup {
	const (
		AllNamespaces   = 1 << iota // 1 (0001)
		OwnNamespace                // 2 (0010)
		SingleNamespace             // 4 (0100)
		MultiNamespace              // 8 (1000)
	)
	supportedInstallModes := 0
	for _, installMode := range csv.Spec.InstallModes {
		if installMode.Supported {
			switch installMode.Type {
			case v1alpha1.InstallModeTypeAllNamespaces:
				supportedInstallModes |= AllNamespaces
			case v1alpha1.InstallModeTypeOwnNamespace:
				supportedInstallModes |= OwnNamespace
			case v1alpha1.InstallModeTypeSingleNamespace:
				supportedInstallModes |= SingleNamespace
			case v1alpha1.InstallModeTypeMultiNamespace:
				supportedInstallModes |= MultiNamespace
			}
		}
	}

	watchNamespaceSchema := func() *spec.Schema {
		itemSchema := spec.StringProperty()
		itemSchema.Pattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
		itemSchema.Description = "A namespace that the operator should watch."
		itemSchema.MinLength = ptr.To(int64(1))
		itemSchema.MaxLength = ptr.To(int64(63))
		return itemSchema
	}

	watchNamespacesSchema := func(item *spec.Schema, maxLength int64) *spec.Schema {
		arraySchema := spec.ArrayProperty(item)
		arraySchema.MinItems = ptr.To(int64(1))
		arraySchema.MaxItems = ptr.To(maxLength)
		return arraySchema
	}

	// 16 cases, for each combination of supported install modes
	switch supportedInstallModes {
	case AllNamespaces:
		return watchNamespaceSchemaSetup{
			IncludeField:               false,
			TemplateHelperDefaultValue: `""`,
		}
	case AllNamespaces | OwnNamespace:
		// "installMode" enum
		schema := spec.StringProperty()
		schema.Enum = []interface{}{"AllNamespaces", "OwnNamespace"}
		schema.Default = "AllNamespaces"

		return watchNamespaceSchemaSetup{
			IncludeField:               true,
			FieldName:                  "installMode",
			Required:                   false,
			Schema:                     schema,
			AllowReleaseNamespace:      true,
			TemplateHelperDefaultValue: `"AllNamespaces"`,
		}
	case AllNamespaces | OwnNamespace | SingleNamespace:
		// "watchNamespace" string, optional, .Release.Namespace allowed, unset means all namespaces
		schema := watchNamespaceSchema()
		schema.Default = ""

		return watchNamespaceSchemaSetup{
			IncludeField:               true,
			FieldName:                  "watchNamespace",
			Required:                   false,
			Schema:                     schema,
			AllowReleaseNamespace:      true,
			TemplateHelperDefaultValue: `""`,
		}
	case AllNamespaces | OwnNamespace | MultiNamespace, AllNamespaces | OwnNamespace | SingleNamespace | MultiNamespace:
		// "watchNamespaces" array of strings, optional, len(1..10), .Release.Namespace allowed, unset means all namespaces
		schema := watchNamespacesSchema(watchNamespaceSchema(), 10)
		schema.Default = []interface{}{corev1.NamespaceAll}

		return watchNamespaceSchemaSetup{
			IncludeField:               true,
			FieldName:                  "watchNamespaces",
			Required:                   false,
			Schema:                     schema,
			AllowReleaseNamespace:      true,
			TemplateHelperDefaultValue: `(list "")`,
		}
	case AllNamespaces | SingleNamespace:
		// "watchNamespace" string, optional, .Release.Namespace not allowed, unset means all namespaces
		schema := watchNamespaceSchema()
		schema.Default = ""

		return watchNamespaceSchemaSetup{
			IncludeField:               true,
			FieldName:                  "watchNamespace",
			Required:                   false,
			Schema:                     schema,
			AllowReleaseNamespace:      false,
			TemplateHelperDefaultValue: `""`,
		}
	case AllNamespaces | SingleNamespace | MultiNamespace, AllNamespaces | MultiNamespace:
		// "watchNamespaces" array of strings, optional, len(1..10), .Release.Namespace not allowed, unset means all namespaces
		singleOrMultiNamespacesListSchema := watchNamespacesSchema(watchNamespaceSchema(), 10)
		allNamespacesItemSchema := spec.StringProperty()
		allNamespacesItemSchema.Description = "A namespace string that represents all namespaces."
		allNamespacesItemSchema.Enum = []interface{}{corev1.NamespaceAll}
		allNamespacesListSchema := watchNamespacesSchema(allNamespacesItemSchema, 1)

		schema := &spec.Schema{}
		schema.AnyOf = []spec.Schema{*singleOrMultiNamespacesListSchema, *allNamespacesListSchema}
		schema.Default = []interface{}{corev1.NamespaceAll}

		return watchNamespaceSchemaSetup{
			IncludeField:               true,
			FieldName:                  "watchNamespaces",
			Required:                   false,
			Schema:                     schema,
			AllowReleaseNamespace:      false,
			TemplateHelperDefaultValue: `(list "")`,
		}
	case OwnNamespace:
		// no field
		return watchNamespaceSchemaSetup{
			IncludeField:               false,
			TemplateHelperDefaultValue: `.Release.Namespace`,
			AllowReleaseNamespace:      true,
		}
	case OwnNamespace | SingleNamespace:
		// "watchNamespace" string, optional, .Release.Namespace allowed, unset means .Release.Namespace
		schema := watchNamespaceSchema()

		return watchNamespaceSchemaSetup{
			IncludeField:               true,
			FieldName:                  "watchNamespace",
			Required:                   false,
			Schema:                     schema,
			AllowReleaseNamespace:      true,
			TemplateHelperDefaultValue: `.Release.Namespace`,
		}
	case OwnNamespace | SingleNamespace | MultiNamespace, OwnNamespace | MultiNamespace:
		// "watchNamespaces" array of strings, optional, len(1..10), .Release.Namespace allowed, unset means .Release.Namespace
		schema := watchNamespacesSchema(watchNamespaceSchema(), 10)

		return watchNamespaceSchemaSetup{
			IncludeField:               true,
			FieldName:                  "watchNamespaces",
			Required:                   false,
			Schema:                     schema,
			AllowReleaseNamespace:      true,
			TemplateHelperDefaultValue: `(list .Release.Namespace)`,
		}
	case SingleNamespace:
		// "watchNamespace" string, required, .Release.Namespace not allowed
		schema := watchNamespaceSchema()
		return watchNamespaceSchemaSetup{
			IncludeField:          true,
			FieldName:             "watchNamespace",
			Required:              true,
			Schema:                schema,
			AllowReleaseNamespace: false,
		}
	case SingleNamespace | MultiNamespace, MultiNamespace:
		// "watchNamespaces" array of strings, required, len(1..10), .Release.Namespace not allowed
		schema := watchNamespacesSchema(watchNamespaceSchema(), 10)
		return watchNamespaceSchemaSetup{
			IncludeField:          true,
			FieldName:             "watchNamespaces",
			Required:              true,
			Schema:                schema,
			AllowReleaseNamespace: false,
		}
	default:
		panic("no supported install modes")
	}
}

// mergeMaps takes any number of maps and merges them into a new map.
// Later maps override keys from earlier maps if there are duplicates.
func mergeMaps[K comparable, V any](maps ...map[K]V) map[K]V {
	result := make(map[K]V)

	for _, m := range maps {
		for k, v := range m {
			result[k] = v
		}
	}

	return result
}

func getAppsV1Definitions(openapiSchemaURL string) (spec.Definitions, error) {
	doc, err := loads.JSONSpec(openapiSchemaURL)
	if err != nil {
		return nil, err
	}
	var docMap map[string]interface{}
	if err := json.Unmarshal(doc.Raw(), &docMap); err != nil {
		return nil, err
	}

	components, ok := docMap["components"].(map[string]interface{})
	if !ok {
		return nil, errors.New("missing components in the Kubernetes API spec")
	}
	schemas, ok := components["schemas"].(map[string]interface{})
	if !ok {
		return nil, errors.New("missing schemas in the Kubernetes API spec")
	}

	jsonSchemas, err := json.Marshal(schemas)
	if err != nil {
		return nil, err
	}

	jsonSchemas = bytes.ReplaceAll(jsonSchemas, []byte("#/components/schemas/"), []byte("#/definitions/"))

	var definitions spec.Definitions
	if err := json.Unmarshal(jsonSchemas, &definitions); err != nil {
		return nil, err
	}

	return definitions, nil
}

func getKubernetesAPIVersion() string {
	dependency := "k8s.io/api"
	info, ok := debug.ReadBuildInfo()
	if ok {
		for _, dep := range info.Deps {
			if dep.Path == dependency {
				vers := strings.TrimPrefix(dep.Version, "v")
				v := semver.MustParse(vers)
				if v.Major == 0 {
					v.Major = 1
				}
				if len(v.Pre) > 0 {
					v.Patch -= 1
				}
				v.Pre = nil
				v.Build = nil
				return fmt.Sprintf("tags/v%s", v)
			}
		}
	}
	return "heads/master"
}

/*
func newDeploymentsTemplateFromCSV(csv v1alpha1.ClusterServiceVersion) (*chart.File, error) {
	for _, depSpec := range csv.Spec.InstallStrategy.StrategySpec.DeploymentSpecs {
		dep := appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: depSpec.Name,
			},
			Spec: depSpec.Spec,
		}

		depAffinity := dep.Spec.Template.Spec.Affinity
		depAnnotations := dep.Annotations
		podAnnotations := dep.Spec.Template.Annotations
		depEnvs := getDeploymentEnvironments(dep)
		depEnvFroms := getDeploymentEnvFroms(dep)
		nodeSelector := dep.Spec.Template.Spec.NodeSelector
		resources := getDeploymentResources(dep)
		depSelector := dep.Spec.Selector
		tolerations := dep.Spec.Template.Spec.Tolerations
		volumeMounts := getDeploymentVolumeMounts(dep)
		volumes := dep.Spec.Template.Spec.Volumes

		yamlData, err := yaml.Marshal(dep)
		if err != nil {
			return nil, fmt.Errorf("marshal deployment %q: %w", dep.Name, err)
		}
		return &chart.File{
			Name: fmt.Sprintf("%s.deployment-%s.yaml", csv.GetName(), dep.Name),
			Data: yamlData,
		}, nil
	}
	return nil, nil
}

func getDeploymentEnvironments(dep appsv1.Deployment) [][]corev1.EnvVar {
	var containerEnvs [][]corev1.EnvVar
	for _, container := range dep.Spec.Template.Spec.Containers {
		containerEnvs = append(containerEnvs, container.Env)
	}
	return containerEnvs
}

func getDeploymentEnvFroms(dep appsv1.Deployment) [][]corev1.EnvFromSource {
	var containerEnvFroms [][]corev1.EnvFromSource
	for _, container := range dep.Spec.Template.Spec.Containers {
		containerEnvFroms = append(containerEnvFroms, container.EnvFrom)
	}
	return containerEnvFroms
}

func getDeploymentResources(dep appsv1.Deployment) []corev1.ResourceRequirements {
	var containerResources []corev1.ResourceRequirements
	for _, container := range dep.Spec.Template.Spec.Containers {
		containerResources = append(containerResources, container.Resources)
	}
	return containerResources
}

func getDeploymentVolumeMounts(dep appsv1.Deployment) [][]corev1.VolumeMount {
	var containerVolumeMounts [][]corev1.VolumeMount
	for _, container := range dep.Spec.Template.Spec.Containers {
		containerVolumeMounts = append(containerVolumeMounts, container.VolumeMounts)
	}
	return containerVolumeMounts
}

var (
	affinityTemplate     = ``
	annotationsTemplate  = ``
	envTemplate          = ``
	envFromTemplate      = ``
	nodeSelectorTemplate = ``
	resourcesTemplate    = ``
	selectorTemplate     = ``
	tolerationsTemplate  = ``
	volumeMountsTemplate = ``
	volumesTemplate      = ``
)
*/
