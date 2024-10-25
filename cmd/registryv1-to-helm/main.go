package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"

	"github.com/go-openapi/spec"
	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
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
		Use:  "registryv1-to-helm <registry+v1-directory-path> <helm-directory-path>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			registryv1Path := args[0]
			helmPath := args[1]

			// Do the conversion
			chrt, err := convertRegistryV1ToHelm(cmd.Context(), os.DirFS(registryv1Path))
			if err != nil {
				cmd.PrintErr(err)
				os.Exit(1)
			}

			if err := chartutil.SaveDir(chrt, helmPath); err != nil {
				cmd.PrintErr(err)
				os.Exit(1)
			}
		},
	}
	return cmd
}

func convertRegistryV1ToHelm(ctx context.Context, rv1FS fs.FS) (*chart.Chart, error) {
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
	valuesSchema, err := newValuesSchemaFile(rv1.CSV)
	if err != nil {
		errs = append(errs, err)
	}
	chrt.Schema = valuesSchema

	/////////////////
	// Setup helpers
	/////////////////
	chrt.Templates = append(chrt.Templates, newTemplateHelpers(rv1.CSV))

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
{{ end -}}

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
{{- end -}}
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
{{- end -}}
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

func newTemplateHelpers(csv v1alpha1.ClusterServiceVersion) *chart.File {
	defaultWatchNamespace := ""
	for _, installMode := range csv.Spec.InstallModes {
		if installMode.Supported {
			switch installMode.Type {
			case v1alpha1.InstallModeTypeAllNamespaces:
				defaultWatchNamespace = `""`
				// If AllNamespaces is supported, we prefer watching all namespaces
				// over the install namespace, so no need to continue looking.
				break
			case v1alpha1.InstallModeTypeOwnNamespace:
				defaultWatchNamespace = ".Release.Namespace"
			}
		}
	}
	templateOperatorTargetNamespaces := fmt.Sprintf(`{{- define "olm.targetNamespaces" }}
    {{- $targetNamespaces := %s -}}
	{{- with .Values.watchNamespace -}}
	{{- $targetNamespaces = . -}}
	{{- end -}}
	{{- with .Values.watchNamespaces -}}
	{{- $targetNamespaces = join "," . -}}
	{{- end -}}
	{{- $targetNamespaces }}
{{- end -}}`, defaultWatchNamespace)

	return &chart.File{
		Name: "templates/_helpers.tpl",
		Data: []byte(templateOperatorTargetNamespaces),
	}
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

		var parametrizeInstructions []parametrize.Instruction
		parametrizeInstructions = append(parametrizeInstructions,
			parametrize.MergeBlock(`dict "annotations" (dict "olm.targetNamespaces" (include "olm.targetNamespaces" .))`, "spec.template.metadata.annotations"),
		)

		// TODO: Parameterize the deployment for subscription.spec.config-style config.

		// Execute the parametrize instructions on the deployment.
		var u unstructured.Unstructured
		uObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&dep)
		if err != nil {
			errs = append(errs, fmt.Errorf("convert deployment %q to unstructured: %w", dep.GetName(), err))
			continue
		}
		u.Object = uObj
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

func newValuesSchemaFile(csv v1alpha1.ClusterServiceVersion) ([]byte, error) {
	valuesSchema := spec.Schema{
		SchemaProps: spec.SchemaProps{
			Schema:               "http://json-schema.org/schema#",
			Type:                 spec.StringOrArray{"object"},
			Description:          fmt.Sprintf("Values for the %s Helm chart.", csv.GetName()),
			Properties:           map[string]spec.Schema{},
			AdditionalProperties: &spec.SchemaOrBool{Allows: false},
		},
	}

	watchNamespacesSchema, watchNamespacesPropName, watchNamespacesRequired, ok := getWatchNamespacesSchema(csv)
	if ok {
		valuesSchema.Properties[watchNamespacesPropName] = *watchNamespacesSchema
		if watchNamespacesRequired {
			valuesSchema.Required = append(valuesSchema.Required, watchNamespacesPropName)
		}
	}
	var jsonDataBuf bytes.Buffer
	enc := json.NewEncoder(&jsonDataBuf)
	enc.SetIndent("", "    ")
	if err := enc.Encode(valuesSchema); err != nil {
		return nil, err
	}
	return jsonDataBuf.Bytes(), nil
}

// watchNamespacesSchemaProperties return the OpenAPI v3 schema for the field that controls the namespace or namespaces to watch.
// It returns the schema as a byte slice, a boolean indicating if the field is required. If the returned byte slice is nil,
// the field should not be included in the schema.
func getWatchNamespacesSchema(csv v1alpha1.ClusterServiceVersion) (*spec.Schema, string, bool, bool) {
	minLength := math.MaxInt64
	maxLength := 0
	for _, installMode := range csv.Spec.InstallModes {
		if installMode.Supported {
			switch installMode.Type {
			case v1alpha1.InstallModeTypeAllNamespaces:
				minLength = min(minLength, 0)
				maxLength = max(maxLength, 0)
			case v1alpha1.InstallModeTypeOwnNamespace:
				minLength = min(minLength, 0)
				maxLength = max(maxLength, 1)
			case v1alpha1.InstallModeTypeSingleNamespace:
				minLength = min(minLength, 1)
				maxLength = max(maxLength, 1)
			case v1alpha1.InstallModeTypeMultiNamespace:
				minLength = min(minLength, 2)
				maxLength = max(maxLength, 10)
			}
		}
	}
	if maxLength == 0 {
		return nil, "", false, false
	}
	isRequired := minLength > 0

	itemSchema := spec.StringProperty()
	itemSchema.Pattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	itemSchema.Description = "A namespace that the operator should watch."
	itemSchema.MinLength = ptr.To(int64(1))
	itemSchema.MaxLength = ptr.To(int64(63))

	if maxLength == 1 {
		return itemSchema, "watchNamespace", isRequired, true
	}

	arraySchema := spec.ArrayProperty(itemSchema)
	arraySchema.Description = "A list of namespaces that the operator should watch."
	arraySchema.MinItems = ptr.To(int64(minLength))
	arraySchema.MaxItems = ptr.To(int64(maxLength))
	return arraySchema, "watchNamespaces", isRequired, true
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
