package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"

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
			Name:        rv1.PackageName,
			Version:     rv1.CSV.Spec.Version.String(),
			Description: rv1.CSV.Spec.Description,
			Keywords:    rv1.CSV.Spec.Keywords,
			Maintainers: convertMaintainers(rv1.CSV.Spec.Maintainers),
			Annotations: rv1.CSV.Annotations,
		},
	}

	var errs []error

	for _, saFile := range newServiceAccountFiles(rv1.CSV) {
		chrt.Templates = append(chrt.Templates, saFile)
	}

	permissionFiles, err := newPermissionsFiles(rv1.CSV)
	if err != nil {
		return nil, err
	}
	for _, rbacFile := range permissionFiles {
		chrt.Templates = append(chrt.Templates, rbacFile)
	}

	clusterPermissionFiles, err := newClusterPermissionsFiles(rv1.CSV)
	for _, rbacFile := range clusterPermissionFiles {
		chrt.Templates = append(chrt.Templates, rbacFile)
	}

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
		yamlRoleBindingData := []byte(fmt.Sprintf(`{{ $installNamespace := .Release.Namespace }}
{{- range .Values.watchNamespaces -}}
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: %s
  namespace: {{ . }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: %s
subjects:
- kind: ServiceAccount
  name: %s
  namespace: {{ $installNamespace }}
{{- end -}}
`, name, name, perms.ServiceAccountName))
		files = append(files, &chart.File{
			Name: fileNameForObject(schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding"}, name),
			Data: yamlRoleBindingData,
		})

		yamlRoleData := []byte(fmt.Sprintf(`{{ $installNamespace := .Release.Namespace }}
{{- range .Values.watchNamespaces -}}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: %s
  namespace: {{ . }}
%s
{{- end -}}
`, name, yamlRules))
		files = append(files, &chart.File{
			Name: fileNameForObject(schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "Role"}, name),
			Data: yamlRoleData,
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
		yamlClusterRoleBindingData := []byte(fmt.Sprintf(`{{ $installNamespace := .Release.Namespace }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: %s
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: %s
subjects:
- kind: ServiceAccount
  name: %s
  namespace: {{ $installNamespace }}
`, name, name, perms.ServiceAccountName))
		files = append(files, &chart.File{
			Name: fileNameForObject(schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}, name),
			Data: yamlClusterRoleBindingData,
		})

		yamlRoleData := []byte(fmt.Sprintf(`{{ $installNamespace := .Release.Namespace }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %s
%s
`, name, yamlRules))
		files = append(files, &chart.File{
			Name: fileNameForObject(schema.GroupKind{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}, name),
			Data: yamlRoleData,
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
