package v2

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"

	"github.com/blang/semver/v4"
	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	v1 "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/go-openapi/loads"
	"github.com/go-openapi/spec"
	"helm.sh/helm/v3/pkg/chart"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"

	"github.com/operator-framework/operator-controller/internal/rukpak/convert"
	"github.com/operator-framework/operator-controller/internal/rukpak/convert/v2/internal/parametrize"
	"github.com/operator-framework/operator-controller/internal/rukpak/util"
)

func RegistryV1ToHelmChart(ctx context.Context, rv1 fs.FS) (*chart.Chart, error) {
	k8sAPIVersion := getKubernetesAPIVersion()
	openapiSchemaURL := fmt.Sprintf("https://raw.githubusercontent.com/kubernetes/kubernetes/refs/%s/api/openapi-spec/v3/apis__apps__v1_openapi.json", k8sAPIVersion)

	return convertRegistryV1ToHelm(ctx, rv1, openapiSchemaURL)
}

func convertRegistryV1ToHelm(ctx context.Context, rv1FS fs.FS, openapiSchemaURL string) (*chart.Chart, error) {
	rv1, err := convert.LoadRegistryV1(ctx, rv1FS)
	if err != nil {
		return nil, err
	}

	if len(rv1.CSV.Spec.APIServiceDefinitions.Owned) > 0 {
		return nil, fmt.Errorf("apiServiceDefintions are not supported")
	}

	// If the CSV includes webhook definitions, should we fail if
	// it supports anything other than AllNamespaces install mode? In
	// OLMv0, it fails in this way, but only for bundles with conversion
	// webhooks.
	//
	// OLMv1 considers APIs to be cluster-scoped, and the upstream
	// Kubernetes api-server maintainers warned us explicitly not
	// to use namespaceSelectors in validating/mutating webhooks because
	// they were not designed to be used that way. They were designed so
	// core control plane components could be exempted during cluster
	// upgrade or bootstrap phases to ensure cluster stability.
	//
	// The alternative is to support all install modes, never set
	// namespaceSelectors in the webhook definitions, and use a
	// constant webhook metadata.name such that it is impossible to
	// install the same webhook multiple times targeting different
	// namespaces.
	//
	// For now, we'll go with the latter approach in order to have
	// _some_ support for as many CSVs as possible.

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

	/////////////////
	// Setup schema
	/////////////////
	valuesSchema, err := newValuesSchemaFile(rv1.CSV, openapiSchemaURL)
	if err != nil {
		return nil, err
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
	saFiles, err := newServiceAccountFiles(rv1.CSV)
	if err != nil {
		return nil, err
	}
	chrt.Templates = append(chrt.Templates, saFiles...)

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

	///////////////////
	// Setup services
	///////////////////
	serviceFiles, err := newServiceFiles(rv1.CSV)
	if err != nil {
		return nil, err
	}
	chrt.Templates = append(chrt.Templates, serviceFiles...)

	///////////////////
	// Setup webhooks
	///////////////////
	webhookFiles, err := newWebhookFiles(rv1)
	if err != nil {
		return nil, err
	}
	chrt.Templates = append(chrt.Templates, webhookFiles...)

	//////////////////////////////////////
	// Add CRDs
	//////////////////////////////////////
	crdFiles, err := newCRDFiles(rv1.CRDs)
	if err != nil {
		return nil, err
	}
	chrt.Templates = append(chrt.Templates, crdFiles...)

	//////////////////////////////////////
	// Add all other static bundle objects
	//////////////////////////////////////
	for _, obj := range rv1.Others {
		f, err := newFile(&obj)
		if err != nil {
			return nil, err
		}
		chrt.Templates = append(chrt.Templates, f)
	}

	return chrt, nil
}

func newFile(obj client.Object, instructions ...parametrize.Instruction) (*chart.File, error) {
	obj.SetNamespace("")
	gvk := obj.GetObjectKind().GroupVersionKind()

	// Execute the parametrize instructions on the object.
	var u unstructured.Unstructured
	uObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("convert %s %q to unstructured: %w", gvk.Kind, obj.GetName(), err)
	}
	u.Object = uObj
	u.SetGroupVersionKind(gvk)

	yamlData, err := parametrize.Execute(u, instructions...)
	if err != nil {
		return nil, fmt.Errorf("parametrize %s %q: %w", gvk.Kind, obj.GetName(), err)
	}

	return &chart.File{
		Name: fileNameForObject(gvk.GroupKind(), obj.GetName()),
		Data: yamlData,
	}, nil
}

func newServiceAccountFiles(csv v1alpha1.ClusterServiceVersion) ([]*chart.File, error) {
	saNames := sets.New[string]()

	for _, perms := range csv.Spec.InstallStrategy.StrategySpec.Permissions {
		saNames.Insert(perms.ServiceAccountName)
	}
	for _, perms := range csv.Spec.InstallStrategy.StrategySpec.ClusterPermissions {
		saNames.Insert(perms.ServiceAccountName)
	}
	var errs []error
	files := make([]*chart.File, 0, len(saNames))
	for _, saName := range sets.List(saNames) {
		sa := corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name: saName,
			},
		}
		sa.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))

		file, err := newFile(&sa, parametrize.Pipeline(".Release.Namespace", "metadata.namespace"))
		if err != nil {
			errs = append(errs, fmt.Errorf("create template file for service account %q: %w", saName, err))
		}

		files = append(files, file)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
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
		var webhooksForDeployment []v1alpha1.WebhookDescription
		for _, webhook := range csv.Spec.WebhookDefinitions {
			if webhook.DeploymentName == depSpec.Name {
				webhooksForDeployment = append(webhooksForDeployment, webhook)
			}
		}

		if len(webhooksForDeployment) > 0 {
			// Need to inject volumes and volume mounts for the cert secret
			depSpec.Spec.Template.Spec.Volumes = slices.DeleteFunc(depSpec.Spec.Template.Spec.Volumes, func(v corev1.Volume) bool {
				return v.Name == "apiservice-cert" || v.Name == "webhook-cert"
			})
			depSpec.Spec.Template.Spec.Volumes = append(depSpec.Spec.Template.Spec.Volumes,
				corev1.Volume{
					Name: "apiservice-cert",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: fmt.Sprintf("%s-%s-cert", csv.Name, webhooksForDeployment[0].DomainName()),
							Items: []corev1.KeyToPath{
								{
									Key:  "tls.crt",
									Path: "apiserver.crt",
								},
								{
									Key:  "tls.key",
									Path: "apiserver.key",
								},
							},
						},
					},
				},
				corev1.Volume{
					Name: "webhook-cert",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: fmt.Sprintf("%s-%s-cert", csv.Name, webhooksForDeployment[0].DomainName()),
							Items: []corev1.KeyToPath{
								{
									Key:  "tls.crt",
									Path: "tls.crt",
								},
								{
									Key:  "tls.key",
									Path: "tls.key",
								},
							},
						},
					},
				},
			)

			volumeMounts := []corev1.VolumeMount{
				{Name: "apiservice-cert", MountPath: "/apiserver.local.config/certificates"},
				{Name: "webhook-cert", MountPath: "/tmp/k8s-webhook-server/serving-certs"},
			}
			for i := range depSpec.Spec.Template.Spec.Containers {
				depSpec.Spec.Template.Spec.Containers[i].VolumeMounts = slices.DeleteFunc(depSpec.Spec.Template.Spec.Containers[i].VolumeMounts, func(vm corev1.VolumeMount) bool {
					return vm.Name == "apiservice-cert" || vm.Name == "webhook-cert"
				})
				depSpec.Spec.Template.Spec.Containers[i].VolumeMounts = append(depSpec.Spec.Template.Spec.Containers[i].VolumeMounts, volumeMounts...)
			}
		}

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
			containerJSON, _ := json.Marshal(container)
			snippets[container.Name] = string(containerJSON)
		}

		for _, fieldName := range sets.List(sets.KeySet(snippets)) {
			sb.WriteString(fmt.Sprintf(`{{- define "deployment.%s.%s" -}}
%s
{{- end -}}

`, depSpec.Name, fieldName, strings.TrimSpace(snippets[fieldName])))

		}
		sb.WriteString(fmt.Sprintf(`{{- define "deployment.%[1]s.spec.overrides" -}}
  {{- $overrides := dict -}}

  {{- $templateMetadataOverrides := dict
    "annotations" (dict
      "olm.targetNamespaces" (include "olm.targetNamespaces" .)
    )
  -}}

  {{- $templateSpecOverrides := dict -}}
  {{- $origAffinity := fromYaml (include "deployment.%[1]s.affinity" .) -}}
  {{- if .Values.affinity -}}
    {{- $_ := set $templateSpecOverrides "affinity" .Values.affinity -}}
  {{- else if $origAffinity -}}
    {{- $_ := set $templateSpecOverrides "affinity" $origAffinity -}}
  {{- end -}}

  {{- $origNodeSelector := fromYaml (include "deployment.%[1]s.nodeSelector" .) -}}
  {{- if .Values.nodeSelector -}}
    {{- $_ := set $templateSpecOverrides "nodeSelector" .Values.nodeSelector -}}
  {{- else if $origNodeSelector -}}
    {{- $_ := set $templateSpecOverrides "nodeSelector" $origNodeSelector -}}
  {{- end -}}

  {{- $origSelector := fromYaml (include "deployment.%[1]s.selector" .) -}}
  {{- if .Values.selector -}}
    {{- $_ := set $overrides "selector" .Values.selector -}}
  {{- else if $origSelector -}}
    {{- $_ := set $overrides "selector" $origSelector -}}
  {{- end -}}

  {{- $origTolerations := fromYamlArray (include "deployment.%[1]s.tolerations" .) -}}
  {{- if and $origTolerations .Values.tolerations -}}
    {{- $_ := set $templateSpecOverrides "tolerations" (concat $origTolerations .Values.tolerations | uniq) -}}
  {{- else if .Values.tolerations -}}
    {{- $_ := set $templateSpecOverrides "tolerations" .Values.tolerations -}}
  {{- else if $origTolerations -}}
    {{- $_ := set $templateSpecOverrides "tolerations" $origTolerations -}}
  {{- end -}}

  {{- $origVolumes := fromYamlArray (include "deployment.%[1]s.volumes" .) -}}
  {{- if and $origVolumes .Values.volumes -}}
    {{- $volumes := .Values.volumes -}}
    {{- $volumeNames := list -}}
    {{- range $volumes -}}{{- $volumeNames = append $volumeNames .name -}}{{- end -}}
    {{- range $origVolumes -}}
      {{- if not (has .name $volumeNames) -}}
        {{- $volumes = append $volumes . -}}
        {{- $volumeNames = append $volumeNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $templateSpecOverrides "volumes" $volumes -}}
  {{- else if .Values.volumes -}}
    {{- $_ := set $templateSpecOverrides "volumes" .Values.volumes -}}
  {{- else if $origVolumes -}}
    {{- $_ := set $templateSpecOverrides "volumes" $origVolumes -}}
  {{- end -}}

  {{- $containers := list -}}
`, depSpec.Name))

		for i, container := range depSpec.Spec.Template.Spec.Containers {
			sb.WriteString(fmt.Sprintf(`

  {{- $origContainer%[1]d := fromYaml (include "deployment.%[2]s.%[3]s" .) -}}

  {{- $origEnv%[1]d := $origContainer%[1]d.env -}}
  {{- if and $origEnv%[1]d .Values.env -}}
    {{- $env := .Values.env -}}
    {{- $envNames := list -}}
    {{- range $env -}}{{- $envNames = append $envNames .name -}}{{- end -}}
    {{- range $origEnv%[1]d -}}
      {{- if not (has .name $envNames) -}}
        {{- $env = append $env . -}}
        {{- $envNames = append $envNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $origContainer%[1]d "env" $env -}}
  {{- else if .Values.env -}}
    {{- $_ := set $origContainer%[1]d "env" .Values.env -}}
  {{- end -}}

  {{- $origEnvFrom%[1]d := $origContainer%[1]d.envFrom -}}
  {{- if and $origEnvFrom%[1]d .Values.envFrom -}}
    {{- $_ := set $origContainer%[1]d "envFrom" (concat $origEnvFrom%[1]d .Values.envFrom | uniq) -}}
  {{- else if .Values.envFrom -}}
    {{- $_ := set $origContainer%[1]d "envFrom" .Values.envFrom -}}
  {{- end -}}

  {{- $origResources%[1]d := $origContainer%[1]d.resources -}}
  {{- if .Values.resources -}}
    {{- $_ := set $origContainer%[1]d "resources" .Values.resources -}}
  {{- end -}}

  {{- $origVolumeMounts%[1]d := $origContainer%[1]d.volumeMounts -}}
  {{- if and $origVolumeMounts%[1]d .Values.volumeMounts -}}
    {{- $volumeMounts := .Values.volumeMounts -}}
    {{- $volumeMountNames := list -}}
    {{- range $volumeMounts -}}{{- $volumeMountNames = append $volumeMountNames .name -}}{{- end -}}
    {{- range $origVolumeMounts%[1]d -}}
      {{- if not (has .name $volumeMountNames) -}}
        {{- $volumeMounts = append $volumeMounts . -}}
        {{- $volumeMountNames = append $volumeMountNames .name -}}
      {{- end -}}
    {{- end -}}
    {{- $_ := set $origContainer%[1]d "volumeMounts" $volumeMounts -}}
  {{- else if .Values.volumeMounts -}}
    {{- $_ := set $origContainer%[1]d "volumeMounts" .Values.volumeMounts -}}
  {{- end -}}

  {{- $containers = append $containers $origContainer%[1]d -}}`, i, depSpec.Name, container.Name))
		}

		sb.WriteString(`
  {{- $templateSpecOverrides := merge $templateSpecOverrides (dict "containers" $containers) -}}

  {{- $overrides = merge $overrides (dict "template" (dict "metadata" $templateMetadataOverrides)) -}}
  {{- $overrides = merge $overrides (dict "template" (dict "spec" $templateSpecOverrides)) -}}
  {{- dict "spec" $overrides | toYaml -}}
{{- end -}}`)
	}
	return &chart.File{
		Name: "templates/_helpers.deployments.tpl",
		Data: sb.Bytes(),
	}
}

func newTargetNamespacesTemplateHelper(csv v1alpha1.ClusterServiceVersion) *chart.File {
	filename := "templates/_helpers.targetNamespaces.tpl"
	watchNsSetup := getWatchNamespacesSchema(csv)
	defineTemplate := `{{- define "olm.targetNamespaces" -}}
%s
{{- end -}}
`

	if !watchNsSetup.IncludeField {
		templateHelperDefaultValue := fmt.Sprintf(`{{- %s -}}`, watchNsSetup.TemplateHelperDefaultValue)
		return &chart.File{
			Name: filename,
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
			Name: filename,
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
			Name: filename,
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
			Name: filename,
			Data: []byte(fmt.Sprintf(defineTemplate, value)),
		}
	}

	return nil
}

func newServiceFiles(csv v1alpha1.ClusterServiceVersion) ([]*chart.File, error) {
	services := map[string]sets.Set[corev1.ServicePort]{}
	deploymentNames := map[string]string{}

	for _, desc := range csv.Spec.WebhookDefinitions {
		serviceName := fmt.Sprintf("%s-service", desc.DomainName())
		deploymentNames[serviceName] = desc.DeploymentName

		containerPort := int32(443)
		if desc.ContainerPort > 0 {
			containerPort = desc.ContainerPort
		}

		targetPort := intstr.FromInt32(containerPort)
		if desc.TargetPort != nil {
			targetPort = *desc.TargetPort
		}
		sp := corev1.ServicePort{
			Name:       strconv.Itoa(int(containerPort)),
			Port:       containerPort,
			TargetPort: targetPort,
		}
		servicePorts, ok := services[serviceName]
		if !ok {
			servicePorts = sets.New[corev1.ServicePort]()
		}
		servicePorts.Insert(sp)
		services[serviceName] = servicePorts
	}

	var (
		files []*chart.File
		errs  []error
	)
	for serviceName, servicePorts := range services {
		ports := servicePorts.UnsortedList()
		slices.SortFunc(ports, func(a, b corev1.ServicePort) int {
			return cmp.Compare(a.Name, b.Name)
		})
		service := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name: serviceName,
			},
			Spec: corev1.ServiceSpec{
				Ports: ports,
			},
		}
		service.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))

		file, err := newFile(&service, parametrize.Pipeline(fmt.Sprintf(`$selector := fromYaml (include "deployment.%s.selector" .) -}}
{{- if .Values.selector -}}
{{- $selector = .Values.selector -}}
{{- end -}}
{{- $selector.matchLabels | toYaml | nindent 4`, deploymentNames[serviceName]), "spec.selector"))

		if err != nil {
			errs = append(errs, fmt.Errorf("create template file for service %q: %w", serviceName, err))
			continue
		}
		files = append(files, file)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
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
		dep.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
		dep.Spec.Template.SetAnnotations(mergeMaps(dep.Spec.Template.Annotations, csvMetadataAnnotations))
		delete(dep.Spec.Template.Annotations, "olm.targetNamespaces")

		// Hardcode the deployment with RevisionHistoryLimit=1 (something OLMv0 does, not sure why)
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

		depFile, err := newFile(&dep, parametrize.MergeBlock(fmt.Sprintf(`fromYaml (include "deployment.%s.spec.overrides" .)`, dep.Name), "spec"))
		if err != nil {
			errs = append(errs, fmt.Errorf("create template file for deployment %q: %w", depSpec.Name, err))
		}
		files = append(files, depFile)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
}

func newCRDFiles(crds []apiextensionsv1.CustomResourceDefinition) ([]*chart.File, error) {
	var (
		files []*chart.File
		errs  []error
	)

	for _, crd := range crds {
		var u unstructured.Unstructured
		uObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&crd)
		if err != nil {
			errs = append(errs, fmt.Errorf("convert crd %q to unstructured: %w", crd.GetName(), err))
			continue
		}
		u.Object = uObject
		gvk := apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")
		u.SetGroupVersionKind(gvk)

		var instructions []parametrize.Instruction
		if crd.Spec.Conversion != nil && crd.Spec.Conversion.Strategy == apiextensionsv1.WebhookConverter {
			insertServiceNamespace := parametrize.Pipeline(".Release.Namespace", "spec.conversion.webhook.clientConfig.service.namespace")
			instructions = append(instructions, insertServiceNamespace)
		}
		yamlData, err := parametrize.Execute(u, instructions...)
		if err != nil {
			errs = append(errs, fmt.Errorf("parametrize crd %q: %w", crd.GetName(), err))
			continue
		}

		files = append(files, &chart.File{
			Name: fileNameForObject(gvk.GroupKind(), crd.GetName()),
			Data: yamlData,
		})
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
}

func newWebhookFiles(rv1 *convert.RegistryV1) ([]*chart.File, error) {
	var (
		files []*chart.File
		errs  []error
	)

	// NOTES:
	// -  [ ] if we use a namespace selector, it needs to be templated based on the targetNamespaces
	// -  [x] the Service namespace needs to be templated as the .Release.Namespace
	// -  [ ] the CA bundle needs to be injected by cert-manager from a Certificate we generate
	//    for the webhook service in the install namespace

	// Q&A:
	// -  Q: should we even setup a namespace selector? If we do, that seems to encourage attempts
	//       for multi-tenancy. APIs are cluster-wide, so webhooks for those APIs should be cluster-wide
	//       as well.
	//    A: We will not setup a namespace selector. The webhook will be cluster-wide.

	// -  Q: along the same lines, should we use metadata.name instead of metadata.generateName? if we
	//       use metadata.name, then we have built-in guarantees that no two bundles can provide the same
	//       webhook.
	//    A: We will use metadata.name.

	// -  Q: Is it really required for a CRD to have spec.preserveUnknownFields=false to let the API Server
	//       call the webhook to do the conversion?
	//    A: Yes, it seems so. This provides better guarantees that the webhook will be able to handle all
	//       of the fields in the object.

	for _, desc := range rv1.CSV.Spec.WebhookDefinitions {
		switch desc.Type {
		case v1alpha1.ValidatingAdmissionWebhook:
			wh := admissionregistrationv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: desc.GenerateName,
				},
				Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{
						Name:                    desc.GenerateName,
						Rules:                   desc.Rules,
						FailurePolicy:           desc.FailurePolicy,
						MatchPolicy:             desc.MatchPolicy,
						ObjectSelector:          desc.ObjectSelector,
						SideEffects:             desc.SideEffects,
						TimeoutSeconds:          desc.TimeoutSeconds,
						AdmissionReviewVersions: desc.AdmissionReviewVersions,
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							Service: &admissionregistrationv1.ServiceReference{
								Name: fmt.Sprintf("%s-service", desc.DomainName()),
								Path: desc.WebhookPath,
								Port: &desc.ContainerPort,
							},
						},
					},
				},
			}

			// Execute the parametrize instructions on the webhook.
			var u unstructured.Unstructured
			uObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&wh)
			if err != nil {
				errs = append(errs, fmt.Errorf("convert validatingadmissionwebhook %q to unstructured: %w", desc.GenerateName, err))
				continue
			}
			u.Object = uObj
			gvk := admissionregistrationv1.SchemeGroupVersion.WithKind("ValidatingWebhookConfiguration")
			u.SetGroupVersionKind(gvk)

			insertServiceNamespace := parametrize.Pipeline(".Release.Namespace", "webhooks.0.clientConfig.service.namespace")
			insertCAInjectorAnnotation := parametrize.Pipeline(fmt.Sprintf(`dict "cert-manager.io/inject-ca-from" (printf "%%s/%s" .Release.Namespace) | toYaml | nindent 4`, desc.DomainName()), `metadata.annotations`)
			yamlData, err := parametrize.Execute(u,
				insertServiceNamespace,
				insertCAInjectorAnnotation,
			)
			if err != nil {
				errs = append(errs, fmt.Errorf("parametrize validatingadmissionwebhook %q: %w", desc.GenerateName, err))
				continue
			}

			files = append(files, &chart.File{
				Name: fileNameForObject(gvk.GroupKind(), desc.GenerateName),
				Data: yamlData,
			})
		case v1alpha1.MutatingAdmissionWebhook:
			wh := admissionregistrationv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{
					Name: desc.GenerateName,
				},
				Webhooks: []admissionregistrationv1.MutatingWebhook{
					{
						Name:                    desc.GenerateName,
						Rules:                   desc.Rules,
						FailurePolicy:           desc.FailurePolicy,
						MatchPolicy:             desc.MatchPolicy,
						ObjectSelector:          desc.ObjectSelector,
						SideEffects:             desc.SideEffects,
						TimeoutSeconds:          desc.TimeoutSeconds,
						AdmissionReviewVersions: desc.AdmissionReviewVersions,
						ClientConfig: admissionregistrationv1.WebhookClientConfig{
							Service: &admissionregistrationv1.ServiceReference{
								Name: fmt.Sprintf("%s-service", desc.DomainName()),
								Path: desc.WebhookPath,
								Port: &desc.ContainerPort,
							},
						},
						ReinvocationPolicy: desc.ReinvocationPolicy,
					},
				},
			}

			// Execute the parametrize instructions on the webhook.
			var u unstructured.Unstructured
			uObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&wh)
			if err != nil {
				errs = append(errs, fmt.Errorf("convert mutatingadmissionwebhook %q to unstructured: %w", desc.GenerateName, err))
				continue
			}
			u.Object = uObj
			gvk := admissionregistrationv1.SchemeGroupVersion.WithKind("MutatingWebhookConfiguration")
			u.SetGroupVersionKind(gvk)

			insertServiceNamespace := parametrize.Pipeline(".Release.Namespace", "webhooks.0.clientConfig.service.namespace")
			insertCAInjectorAnnotation := parametrize.Pipeline(fmt.Sprintf(`dict "cert-manager.io/inject-ca-from" (printf "%%s/%s" .Release.Namespace) | toYaml | nindent 4`, desc.DomainName()), `metadata.annotations`)
			yamlData, err := parametrize.Execute(u,
				insertServiceNamespace,
				insertCAInjectorAnnotation,
			)
			if err != nil {
				errs = append(errs, fmt.Errorf("parametrize mutatingadmissionwebhook %q: %w", desc.GenerateName, err))
				continue
			}

			files = append(files, &chart.File{
				Name: fileNameForObject(gvk.GroupKind(), desc.GenerateName),
				Data: yamlData,
			})
		case v1alpha1.ConversionWebhook:
			for _, conversionCRD := range desc.ConversionCRDs {
				crd, i, err := findCRD(conversionCRD, rv1.CSV.Spec.CustomResourceDefinitions.Owned, rv1.CRDs)
				if err != nil {
					errs = append(errs, err)
					continue
				}

				if crd.Spec.PreserveUnknownFields {
					errs = append(errs, fmt.Errorf("CRD %q sets spec.preserveUnknownFields=true; must be false to let API Server call webhook to do the conversion", crd.Name))
					continue
				}

				conversionWebhookPath := "/"
				if desc.WebhookPath != nil {
					conversionWebhookPath = *desc.WebhookPath
				}

				rv1.CRDs[i].Spec.Conversion = &apiextensionsv1.CustomResourceConversion{
					Strategy: apiextensionsv1.WebhookConverter,
					Webhook: &apiextensionsv1.WebhookConversion{
						ClientConfig: &apiextensionsv1.WebhookClientConfig{
							Service: &apiextensionsv1.ServiceReference{
								Name: fmt.Sprintf("%s-service", desc.DomainName()),
								Path: &conversionWebhookPath,
								Port: &desc.ContainerPort,
							},
						},
						ConversionReviewVersions: desc.AdmissionReviewVersions,
					},
				}
			}
		}

		issuer := &certmanagerv1.Issuer{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("%s-selfsigned-issuer", rv1.CSV.Name),
			},
			Spec: certmanagerv1.IssuerSpec{
				IssuerConfig: certmanagerv1.IssuerConfig{
					SelfSigned: &certmanagerv1.SelfSignedIssuer{},
				},
			},
		}
		issuer.SetGroupVersionKind(certmanagerv1.SchemeGroupVersion.WithKind("Issuer"))
		certificate := &certmanagerv1.Certificate{
			ObjectMeta: metav1.ObjectMeta{
				Name: desc.DomainName(),
			},
			Spec: certmanagerv1.CertificateSpec{
				SecretName: fmt.Sprintf("%s-%s-cert", rv1.CSV.Name, desc.DomainName()),
				IssuerRef: v1.ObjectReference{
					Name: fmt.Sprintf("%s-selfsigned-issuer", rv1.CSV.Name),
				},
			},
		}
		certificate.SetGroupVersionKind(certmanagerv1.SchemeGroupVersion.WithKind("Certificate"))
		issuerFile, err := newFile(issuer)
		if err != nil {
			errs = append(errs, fmt.Errorf("create issuer %q: %w", desc.GenerateName, err))
			continue
		}
		certFile, err := newFile(certificate, parametrize.Pipeline(
			fmt.Sprintf(`list (printf "%s.%%s.svc" .Release.Namespace) | toYaml | nindent 4`, fmt.Sprintf("%s-service", desc.DomainName())),
			"spec.dnsNames"),
		)
		if err != nil {
			errs = append(errs, fmt.Errorf("create certificate %q: %w", desc.GenerateName, err))
			continue
		}

		files = append(files, issuerFile, certFile)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return files, nil
}

func findCRD(crdName string, ownedCRDs []v1alpha1.CRDDescription, crds []apiextensionsv1.CustomResourceDefinition) (*apiextensionsv1.CustomResourceDefinition, int, error) {
	foundOwnedCRD := false
	for _, ownedCRD := range ownedCRDs {
		if ownedCRD.Name == crdName {
			foundOwnedCRD = true
			break
		}
	}
	var errs []error
	if !foundOwnedCRD {
		errs = append(errs, fmt.Errorf("CRD %q is not owned by the CSV", crdName))
	}

	var (
		foundCRD   *apiextensionsv1.CustomResourceDefinition
		foundIndex int
	)
	for i, crd := range crds {
		if crd.Name == crdName {
			foundCRD = &crd
			foundIndex = i
			break
		}
	}
	if foundCRD == nil {
		errs = append(errs, fmt.Errorf("CRD %q is not found in the bundle", crdName))
	}
	if len(errs) > 0 {
		return nil, -1, errors.Join(errs...)
	}
	return foundCRD, foundIndex, nil
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
