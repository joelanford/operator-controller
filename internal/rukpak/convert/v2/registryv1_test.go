package v2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/blang/semver/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	k8sresource "k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/util/jsonpath"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/operator-framework/api/pkg/lib/version"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"
)

func TestRegistryV1ToHelmChart(t *testing.T) {
	type testCase struct {
		name             string
		cfg              genBundleConfig
		installNamespace string
		values           chartutil.Values
		assert           func(*testing.T, string, *chart.Chart, error, string, error)
	}

	type configOverrideFields struct {
		selector     *metav1.LabelSelector
		nodeSelector map[string]string
		tolerations  []corev1.Toleration
		volumes      []corev1.Volume
		affinity     *corev1.Affinity
		resources    *corev1.ResourceRequirements
		env          []corev1.EnvVar
		envFrom      []corev1.EnvFromSource
		volumeMounts []corev1.VolumeMount
		annotations  map[string]string
	}

	standardAssertions := func(t *testing.T, installNamespace string, chrt *chart.Chart, convertError error, manifest string, templateError error, overrides configOverrideFields) {
		assert.NoError(t, convertError)
		assert.NotNil(t, chrt)
		assert.NoError(t, templateError)
		assert.NotEmpty(t, manifest)

		assert.Equal(t, "v2", chrt.Metadata.APIVersion)
		assert.Equal(t, "example-operator", chrt.Metadata.Name)
		assert.Equal(t, csvSpecVersion().String(), chrt.Metadata.Version)
		assert.Equal(t, csvSpecDescription(), chrt.Metadata.Description)
		assert.Equal(t, csvSpecKeywords(), chrt.Metadata.Keywords)
		assert.Equal(t, convertMaintainers(csvSpecMaintainers()), chrt.Metadata.Maintainers)
		assert.Equal(t, csvAnnotations(), chrt.Metadata.Annotations)
		assert.Equal(t, convertSpecLinks(csvSpecLinks()), chrt.Metadata.Sources)
		assert.Equal(t, csvSpecProvider().URL, chrt.Metadata.Home)
		assert.Equal(t, ">= "+csvSpecMinKubeVersion(), chrt.Metadata.KubeVersion)

		assertInManifest(t, manifest,
			func(obj client.Object) bool {
				return obj.GetObjectKind().GroupVersionKind().Kind == "Deployment"
			},
			func(t *testing.T, obj client.Object) {
				// pod template metadata should have combo of csv.metadata.annotations and olm.targetNamespaces="" annotation
				expectedAnnotations := mergeMaps(csvAnnotations(), map[string]string{"olm.targetNamespaces": ""})
				assertFieldEqual(t, obj, expectedAnnotations, `{.spec.template.metadata.annotations}`)

				if overrides.selector != nil {
					assertFieldEqual(t, obj, overrides.selector, `{.spec.selector}`)
				} else {
					assertFieldEqual(t, obj, csvDeploymentSelector(), `{.spec.selector}`)
				}

				if overrides.nodeSelector != nil {
					assertFieldEqual(t, obj, overrides.nodeSelector, `{.spec.template.spec.nodeSelector}`)
				} else {
					assertFieldEqual(t, obj, csvPodNodeSelector(), `{.spec.template.spec.nodeSelector}`)
				}

				if overrides.tolerations != nil {
					assertFieldEqual(t, obj, overrides.tolerations, `{.spec.template.spec.tolerations}`)
				} else {
					assertFieldEqual(t, obj, csvPodTolerations(), `{.spec.template.spec.tolerations}`)
				}

				if overrides.affinity != nil {
					assertFieldEqual(t, obj, overrides.affinity, `{.spec.template.spec.affinity}`)
				} else {
					assertFieldEqual(t, obj, csvPodAffinity(), `{.spec.template.spec.affinity}`)
				}

				if overrides.volumes != nil {
					assertFieldEqual(t, obj, overrides.volumes, `{.spec.template.spec.volumes}`)
				} else {
					assertFieldEqual(t, obj, csvPodVolumes(), `{.spec.template.spec.volumes}`)
				}

				if overrides.resources != nil {
					assertFieldEqual(t, obj, overrides.resources, `{.spec.template.spec.containers[0].resources}`)
				} else {
					assertFieldEqual(t, obj, csvContainerResources(), `{.spec.template.spec.containers[0].resources}`)
				}

				if overrides.env != nil {
					assertFieldEqual(t, obj, overrides.env, `{.spec.template.spec.containers[0].env}`)
				} else {
					assertFieldEqual(t, obj, csvContainerEnv(), `{.spec.template.spec.containers[0].env}`)
				}

				if overrides.envFrom != nil {
					assertFieldEqual(t, obj, overrides.envFrom, `{.spec.template.spec.containers[0].envFrom}`)
				} else {
					assertFieldEqual(t, obj, csvContainerEnvFrom(), `{.spec.template.spec.containers[0].envFrom}`)
				}

				if overrides.volumeMounts != nil {
					assertFieldEqual(t, obj, overrides.volumeMounts, `{.spec.template.spec.containers[0].volumeMounts}`)
				} else {
					assertFieldEqual(t, obj, csvContainerVolumeMounts(), `{.spec.template.spec.containers[0].volumeMounts}`)
				}

				// pod template spec should have the original values from the CSV that are not overridable
				assertFieldEqual(t, obj, csvPodLabels(), `{.spec.template.metadata.labels}`)
				assertFieldEqual(t, obj, csvContainerName(), `{.spec.template.spec.containers[0].name}`)
				assertFieldEqual(t, obj, csvContainerImage(), `{.spec.template.spec.containers[0].image}`)
			},
		)

		assertPresent(t, manifest,
			func(obj client.Object) bool {
				if obj.GetObjectKind().GroupVersionKind().Kind != "ClusterRole" {
					return false
				}
				var cr rbacv1.ClusterRole
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.(*unstructured.Unstructured).Object, &cr); err != nil {
					return false
				}
				return equality.Semantic.DeepEqualWithNilDifferentFromEmpty(csvClusterPermissionRules(), cr.Rules)
			},
		)

		assertPresent(t, manifest,
			func(obj client.Object) bool {
				return obj.GetObjectKind().GroupVersionKind().Kind == "ServiceAccount" &&
					obj.GetName() == csvServiceAccountName() &&
					(obj.GetNamespace() == installNamespace || obj.GetNamespace() == "")
			},
		)

		assertInManifest(t, manifest,
			func(obj client.Object) bool {
				return obj.GetObjectKind().GroupVersionKind().Kind == "ConfigMap" && obj.GetName() == "example-config"
			},
			func(t *testing.T, obj client.Object) {
				assertFieldEqual(t, obj, configMapData(), `{.data}`)
			},
		)
	}

	tests := []testCase{
		{
			name: "No overrides, AllNamespaces",
			cfg: genBundleConfig{
				supportedInstallModes: []v1alpha1.InstallModeType{v1alpha1.InstallModeTypeAllNamespaces},
			},
			installNamespace: "test-namespace",
			values:           nil,
			assert: func(t *testing.T, installNamespace string, chrt *chart.Chart, convertError error, manifest string, templateError error) {
				standardAssertions(t, installNamespace, chrt, convertError, manifest, templateError, configOverrideFields{})
				assertPresent(t, manifest,
					func(obj client.Object) bool {
						if obj.GetObjectKind().GroupVersionKind().Kind != "ClusterRole" {
							return false
						}
						var cr rbacv1.ClusterRole
						if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.(*unstructured.Unstructured).Object, &cr); err != nil {
							return false
						}
						return equality.Semantic.DeepEqualWithNilDifferentFromEmpty(csvPermissionRules(), cr.Rules)
					},
				)
				assertNotPresent(t, manifest,
					func(obj client.Object) bool {
						// There should be no Roles in the manifest
						return obj.GetObjectKind().GroupVersionKind().Kind == "Role"
					},
				)
			},
		},
		{
			name: "All config overrides, AllNamespaces",
			cfg: genBundleConfig{
				supportedInstallModes: []v1alpha1.InstallModeType{v1alpha1.InstallModeTypeAllNamespaces},
			},
			installNamespace: "test-namespace",
			values: chartutil.Values{
				"selector":     &metav1.LabelSelector{MatchLabels: map[string]string{"overrideKey": "overrideValue"}},
				"nodeSelector": map[string]string{"nodeSelectorOverrideKey": "nodeSelectorOverrideValue"},
				"tolerations":  []corev1.Toleration{{Key: "tolerationOverrideKey", Value: "tolerationOverrideValue"}},
				"affinity": &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
						{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"affinityOverrideKey": "affinityOverrideValue"}}},
					}},
				},
				"volumes":      []map[string]interface{}{{"name": "volumeOverrideName", "emptyDir": map[string]interface{}{}}},
				"resources":    &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
				"env":          []map[string]interface{}{{"name": "envOverrideName", "value": "envOverrideValue"}},
				"envFrom":      []corev1.EnvFromSource{{Prefix: "envFromOverridePrefix"}},
				"volumeMounts": []map[string]interface{}{{"name": "volumeMountOverrideName", "mountPath": "volumeMountOverridePath"}},
				// TODO: what about annotations?
			},
			assert: func(t *testing.T, installNamespace string, chrt *chart.Chart, convertError error, manifest string, templateError error) {
				chartutil.SaveDir(chrt, ".")
				standardAssertions(t, installNamespace, chrt, convertError, manifest, templateError, configOverrideFields{
					selector:     &metav1.LabelSelector{MatchLabels: map[string]string{"overrideKey": "overrideValue"}},
					nodeSelector: map[string]string{"nodeSelectorOverrideKey": "nodeSelectorOverrideValue"},
					tolerations:  []corev1.Toleration{{Key: "tolerationOverrideKey", Value: "tolerationOverrideValue"}},
					affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
							{LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"affinityOverrideKey": "affinityOverrideValue"}}},
						}},
					},
					volumes:      []corev1.Volume{{Name: "volumeOverrideName", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
					resources:    &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
					env:          []corev1.EnvVar{{Name: "envOverrideName", Value: "envOverrideValue"}},
					envFrom:      []corev1.EnvFromSource{{Prefix: "envFromOverridePrefix"}},
					volumeMounts: []corev1.VolumeMount{{Name: "volumeMountOverrideName", MountPath: "volumeMountOverridePath"}},
				})
				assertPresent(t, manifest,
					func(obj client.Object) bool {
						if obj.GetObjectKind().GroupVersionKind().Kind != "ClusterRole" {
							return false
						}
						var cr rbacv1.ClusterRole
						if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.(*unstructured.Unstructured).Object, &cr); err != nil {
							return false
						}
						return equality.Semantic.DeepEqualWithNilDifferentFromEmpty(csvPermissionRules(), cr.Rules)
					},
				)
				assertNotPresent(t, manifest,
					func(obj client.Object) bool {
						// There should be no Roles in the manifest
						return obj.GetObjectKind().GroupVersionKind().Kind == "Role"
					},
				)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotChart, convertError := RegistryV1ToHelmChart(context.Background(), genBundleFS(tc.cfg))
			if convertError != nil {
				tc.assert(t, tc.installNamespace, gotChart, convertError, "", nil)
				return
			}
			manifest, templateError := templateChart(gotChart, tc.installNamespace, tc.values)
			tc.assert(t, tc.installNamespace, gotChart, nil, manifest, templateError)
		})
	}
}

//
//func Test_getWatchNamespacesSchema(t *testing.T) {
//	tests := []struct {
//		supportedInstallModeSets []sets.Set[v1alpha1.InstallModeType]
//		shouldPanic              bool
//		want                     watchNamespaceSchemaConfig
//	}{
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces)},
//			want: watchNamespaceSchemaConfig{
//				TemplateHelperDefaultValue: `""`,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "installMode",
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type: spec.StringOrArray{"string"},
//						Enum: []interface{}{"AllNamespaces", "OwnNamespace"},
//					},
//				},
//				TemplateHelperDefaultValue: `"AllNamespaces"`,
//				AllowReleaseNamespace:      true,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespace",
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:        spec.StringOrArray{"string"},
//						Description: "A namespace that the extension should watch.",
//						Pattern:     watchNamespacePattern,
//						MinLength:   ptr.To(int64(1)),
//						MaxLength:   ptr.To(int64(63)),
//					},
//				},
//				TemplateHelperDefaultValue: `""`,
//				AllowReleaseNamespace:      true,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeMultiNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespaces",
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:     spec.StringOrArray{"array"},
//						MinItems: ptr.To(int64(1)),
//						MaxItems: ptr.To(int64(10)),
//						Items: &spec.SchemaOrArray{
//							Schema: &spec.Schema{
//								SchemaProps: spec.SchemaProps{
//									Type:        spec.StringOrArray{"string"},
//									Description: "A namespace that the extension should watch.",
//									Pattern:     watchNamespacePattern,
//									MinLength:   ptr.To(int64(1)),
//									MaxLength:   ptr.To(int64(63)),
//								},
//							},
//						},
//					},
//				},
//				TemplateHelperDefaultValue: `(list "")`,
//				AllowReleaseNamespace:      true,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeMultiNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespaces",
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:     spec.StringOrArray{"array"},
//						MinItems: ptr.To(int64(1)),
//						MaxItems: ptr.To(int64(10)),
//						Items: &spec.SchemaOrArray{
//							Schema: &spec.Schema{
//								SchemaProps: spec.SchemaProps{
//									Type:        spec.StringOrArray{"string"},
//									Description: "A namespace that the extension should watch.",
//									Pattern:     watchNamespacePattern,
//									MinLength:   ptr.To(int64(1)),
//									MaxLength:   ptr.To(int64(63)),
//								},
//							},
//						},
//					},
//				},
//				TemplateHelperDefaultValue: `(list "")`,
//				AllowReleaseNamespace:      false,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeSingleNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespace",
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:        spec.StringOrArray{"string"},
//						Description: "A namespace that the extension should watch.",
//						Pattern:     watchNamespacePattern,
//						MinLength:   ptr.To(int64(1)),
//						MaxLength:   ptr.To(int64(63)),
//					},
//				},
//				TemplateHelperDefaultValue: `""`,
//				AllowReleaseNamespace:      false,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				TemplateHelperDefaultValue: `.Release.Namespace`,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespace",
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:        spec.StringOrArray{"string"},
//						Description: "A namespace that the extension should watch.",
//						Pattern:     watchNamespacePattern,
//						MinLength:   ptr.To(int64(1)),
//						MaxLength:   ptr.To(int64(63)),
//					},
//				},
//				TemplateHelperDefaultValue: `.Release.Namespace`,
//				AllowReleaseNamespace:      true,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeMultiNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespaces",
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:     spec.StringOrArray{"array"},
//						MinItems: ptr.To(int64(1)),
//						MaxItems: ptr.To(int64(10)),
//						Items: &spec.SchemaOrArray{
//							Schema: &spec.Schema{
//								SchemaProps: spec.SchemaProps{
//									Type:        spec.StringOrArray{"string"},
//									Description: "A namespace that the extension should watch.",
//									Pattern:     watchNamespacePattern,
//									MinLength:   ptr.To(int64(1)),
//									MaxLength:   ptr.To(int64(63)),
//								},
//							},
//						},
//					},
//				},
//				TemplateHelperDefaultValue: `(list .Release.Namespace)`,
//				AllowReleaseNamespace:      true,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeSingleNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespace",
//				Required:     true,
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:        spec.StringOrArray{"string"},
//						Description: "A namespace that the extension should watch.",
//						Pattern:     watchNamespacePattern,
//						MinLength:   ptr.To(int64(1)),
//						MaxLength:   ptr.To(int64(63)),
//					},
//				},
//				AllowReleaseNamespace: false,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
//				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeMultiNamespace),
//			},
//			want: watchNamespaceSchemaConfig{
//				IncludeField: true,
//				FieldName:    "watchNamespaces",
//				Required:     true,
//				Schema: &spec.Schema{
//					SchemaProps: spec.SchemaProps{
//						Type:     spec.StringOrArray{"array"},
//						MinItems: ptr.To(int64(1)),
//						MaxItems: ptr.To(int64(10)),
//						Items: &spec.SchemaOrArray{
//							Schema: &spec.Schema{
//								SchemaProps: spec.SchemaProps{
//									Type:        spec.StringOrArray{"string"},
//									Description: "A namespace that the extension should watch.",
//									Pattern:     watchNamespacePattern,
//									MinLength:   ptr.To(int64(1)),
//									MaxLength:   ptr.To(int64(63)),
//								},
//							},
//						},
//					},
//				},
//				AllowReleaseNamespace: false,
//			},
//		},
//		{
//			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{{}},
//			shouldPanic:              true,
//		},
//	}
//	for _, tt := range tests {
//		for _, supportedInstallModes := range tt.supportedInstallModeSets {
//			modes := []string{}
//			installModes := []v1alpha1.InstallMode{}
//
//			for _, mode := range []v1alpha1.InstallModeType{
//				v1alpha1.InstallModeTypeAllNamespaces,
//				v1alpha1.InstallModeTypeOwnNamespace,
//				v1alpha1.InstallModeTypeSingleNamespace,
//				v1alpha1.InstallModeTypeMultiNamespace,
//			} {
//				if supportedInstallModes.Has(mode) {
//					modes = append(modes, string(mode))
//					installModes = append(installModes, v1alpha1.InstallMode{Type: mode, Supported: supportedInstallModes.Has(mode)})
//				}
//			}
//			name := strings.Join(modes, "|")
//			if name == "" {
//				name = "none"
//			}
//			t.Run(name, func(t *testing.T) {
//				if tt.shouldPanic {
//					require.Panics(t, func() {
//						getWatchNamespacesSchema(installModes)
//					})
//					return
//				}
//				got := getWatchNamespacesSchema(installModes)
//				if diff := cmp.Diff(got, tt.want, cmpopts.IgnoreUnexported(jsonreference.Ref{})); diff != "" {
//					t.Errorf("getWatchNamespacesSchema() mismatch (-got +want):\n%s", diff)
//				}
//			})
//		}
//	}
//}
//
//func Test_newValuesSchemaFile(t *testing.T) {
//	tests := []struct {
//		name               string
//		watchNsSchemaSetup watchNamespaceSchemaConfig
//	}{
//		{
//			name:               "No target-namespace-related field",
//			watchNsSchemaSetup: watchNamespaceSchemaConfig{IncludeField: false},
//		},
//		{
//			name:               "Target-namespace-related field, optional",
//			watchNsSchemaSetup: watchNamespaceSchemaConfig{IncludeField: true, FieldName: "zz_field", Schema: spec.StringProperty()},
//		},
//		{
//			name:               "Target-namespace-related field, required",
//			watchNsSchemaSetup: watchNamespaceSchemaConfig{IncludeField: true, FieldName: "zz_field", Schema: spec.StringProperty(), Required: true},
//		},
//	}
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			schBytes, err := newValuesSchemaFile(tt.watchNsSchemaSetup)
//			require.NoError(t, err)
//
//			var sch spec.Schema
//			require.NoError(t, json.Unmarshal(schBytes, &sch))
//
//		})
//	}
//}
//
//func Test_getSchema(t *testing.T) {
//	assertSchema := func(t *testing.T, gotSchema spec.Schema, definitionName, propertyName string) {
//		t.Helper()
//
//		defSchema, ok := gotSchema.Definitions[definitionName]
//		require.True(t, ok)
//		require.NotNil(t, defSchema)
//		prop, ok := defSchema.Properties[propertyName]
//		assert.True(t, ok)
//		assert.NotNil(t, prop)
//
//		propSchema, ok := gotSchema.Properties[propertyName]
//		require.True(t, ok)
//		require.NotNil(t, propSchema)
//		assert.Equal(t, *spec.RefProperty(fmt.Sprintf("#/definitions/%s/properties/%s", definitionName, propertyName)), propSchema)
//	}
//	tests := []struct {
//		name            string
//		extraProperties spec.SchemaProperties
//		assert          func(*testing.T, *spec.Schema, error)
//	}{
//		{
//			name: "Default",
//			extraProperties: spec.SchemaProperties{
//				"extra": *spec.StringProperty(),
//			},
//			assert: func(t *testing.T, gotSchema *spec.Schema, err error) {
//				require.NoError(t, err)
//				require.NotNil(t, gotSchema)
//				assertSchema(t, *gotSchema, "io.k8s.api.apps.v1.DeploymentSpec", "selector")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "nodeSelector")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "tolerations")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "resources")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "envFrom")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "env")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "volumes")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "volumeMounts")
//				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "affinity")
//				assertSchema(t, *gotSchema, "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta", "annotations")
//
//				extraProp, ok := gotSchema.Properties["extra"]
//				assert.True(t, ok)
//				assert.Equal(t, *spec.StringProperty(), extraProp)
//			},
//		},
//		{
//			name: "Duplicate property",
//			extraProperties: spec.SchemaProperties{
//				"selector": *spec.StringProperty(),
//			},
//			assert: func(t *testing.T, gotSchema *spec.Schema, err error) {
//				require.Error(t, err)
//				require.Nil(t, gotSchema)
//			},
//		},
//	}
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			gotSch, err := getSchema(tt.extraProperties)
//			tt.assert(t, gotSch, err)
//		})
//	}
//}
//
//func Test_newTargetNamespacesTemplateHelper(t *testing.T) {
//	tests := []struct {
//		name  string
//		setup watchNamespaceSchemaConfig
//		want  string
//	}{
//		{
//			name:  "no field",
//			setup: watchNamespaceSchemaConfig{IncludeField: false, TemplateHelperDefaultValue: `"test"`},
//			want:  "{{- define \"olm.targetNamespaces\" -}}\n{{- \"test\" -}}\n{{- end -}}\n",
//		},
//		{
//			name:  "install mode, optional",
//			setup: watchNamespaceSchemaConfig{IncludeField: true, FieldName: "installMode", TemplateHelperDefaultValue: `"AllNamespaces"`},
//			want: `{{- define "olm.targetNamespaces" -}}
//{{- $installMode := .Values.installMode -}}
//{{- if not $installMode -}}
//  {{- $installMode = "AllNamespaces" -}}
//{{- end -}}
//{{- if eq $installMode "AllNamespaces" -}}
//  {{- "" -}}
//{{- else if eq $installMode "OwnNamespace" -}}
//  {{- .Release.Namespace -}}
//{{- else -}}
//  {{- fail (printf "Unsupported install mode: %s" $installMode) -}}
//{{- end -}}
//{{- end -}}
//`,
//		},
//	}
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			got := newTargetNamespacesTemplateHelper(tt.setup)
//			assert.Equal(t, "templates/_helpers.targetNamespaces.tpl", got.Name)
//			assert.Equal(t, tt.want, string(got.Data))
//		})
//	}
//}

func assertInManifest(t *testing.T, manifest string, match func(client.Object) bool, assert func(*testing.T, client.Object)) {
	t.Helper()

	foundMatch := false
	res := k8sresource.NewLocalBuilder().Unstructured().Stream(strings.NewReader(manifest), "manifest").Do()
	_ = res.Visit(func(info *k8sresource.Info, err error) error {
		require.NoError(t, err)

		obj := info.Object.(client.Object)
		if match(obj) {
			foundMatch = true
			assert(t, obj)
		}
		return nil
	})
	if !foundMatch {
		t.Errorf("no object matched the given criteria")
	}
}

func assertPresent(t *testing.T, manifest string, match func(client.Object) bool) {
	t.Helper()

	foundMatch := false
	res := k8sresource.NewLocalBuilder().Unstructured().Stream(strings.NewReader(manifest), "manifest").Do()
	_ = res.Visit(func(info *k8sresource.Info, err error) error {
		require.NoError(t, err)
		obj := info.Object.(client.Object)
		if match(obj) {
			foundMatch = true
		}
		return nil
	})
	if !foundMatch {
		t.Errorf("no object matched the given criteria")
	}
}

func assertNotPresent(t *testing.T, manifest string, match func(client.Object) bool) {
	t.Helper()

	res := k8sresource.NewLocalBuilder().Unstructured().Stream(strings.NewReader(manifest), "manifest").Do()
	_ = res.Visit(func(info *k8sresource.Info, err error) error {
		require.NoError(t, err)
		obj := info.Object.(client.Object)
		if match(obj) {
			t.Errorf("manifest contains unexpected object: %#v, %#v", obj.GetObjectKind().GroupVersionKind(), types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()})
		}
		return nil
	})
}

func assertFieldEqual(t *testing.T, obj client.Object, value interface{}, path string) bool {
	t.Helper()
	uObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	require.NoError(t, err)

	jp := jsonpath.New("assert").AllowMissingKeys(false)
	jp.EnableJSONOutput(true)
	require.NoError(t, jp.Parse(path))

	var resultBuf bytes.Buffer
	require.NoError(t, jp.Execute(&resultBuf, uObject))

	var result []json.RawMessage
	require.NoError(t, json.Unmarshal(resultBuf.Bytes(), &result))

	expected, err := json.Marshal(value)
	require.NoError(t, err)

	return assert.JSONEq(t, string(expected), string(result[0]))
}

type genBundleConfig struct {
	supportedInstallModes        []v1alpha1.InstallModeType
	includeAPIServiceDefinitions bool
	includeWebhookDefinitions    bool
	minKubeVersion               string
}

func genBundleFS(cfg genBundleConfig) fs.FS {
	mustMarshal := func(v interface{}) []byte {
		b, err := yaml.Marshal(v)
		if err != nil {
			panic(err)
		}
		return b
	}
	newFSFile := func(data []byte) *fstest.MapFile {
		return &fstest.MapFile{
			Data: data,
			Mode: 0644,
		}
	}

	configMap := corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "ConfigMap",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "example-config",
		},
		Data: configMapData(),
	}
	crd := apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "examples.example.com",
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "examples",
				Singular: "example",
				Kind:     "Example",
				ListKind: "ExampleList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema:  &apiextensionsv1.CustomResourceValidation{},
				},
			},
		},
	}
	csv := v1alpha1.ClusterServiceVersion{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "operators.coreos.com/v1alpha1",
			Kind:       "ClusterServiceVersion",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        fmt.Sprintf("example-operator.v%s", csvSpecVersion().String()),
			Labels:      csvLabels(),
			Annotations: csvAnnotations(),
		},
		Spec: v1alpha1.ClusterServiceVersionSpec{
			CustomResourceDefinitions: csvSpecCRDDefinitions(crd),
			APIServiceDefinitions:     csvSpecAPIServiceDefinitions(cfg.includeAPIServiceDefinitions),
			WebhookDefinitions:        csvSpecWebhookDefinitions(cfg.includeWebhookDefinitions),
			Maintainers:               csvSpecMaintainers(),
			InstallModes:              csvSpecInstallModes(cfg.supportedInstallModes),
			Version:                   csvSpecVersion(),
			MinKubeVersion:            csvSpecMinKubeVersion(),
			Description:               csvSpecDescription(),
			Keywords:                  csvSpecKeywords(),
			Annotations:               csvSpecAnnotations(),
			Provider:                  csvSpecProvider(),
			Links:                     csvSpecLinks(),
			InstallStrategy: v1alpha1.NamedInstallStrategy{
				StrategyName: v1alpha1.InstallStrategyNameDeployment,
				StrategySpec: v1alpha1.StrategyDetailsDeployment{
					Permissions: []v1alpha1.StrategyDeploymentPermissions{
						{
							ServiceAccountName: csvServiceAccountName(),
							Rules:              csvPermissionRules(),
						},
					},
					ClusterPermissions: []v1alpha1.StrategyDeploymentPermissions{
						{
							ServiceAccountName: csvServiceAccountName(),
							Rules:              csvClusterPermissionRules(),
						},
					},
					DeploymentSpecs: []v1alpha1.StrategyDeploymentSpec{
						{
							Name:  "example",
							Label: csvDeploymentLabels(),
							Spec: appsv1.DeploymentSpec{
								Replicas: csvDeploymentReplicas(),
								Selector: csvDeploymentSelector(),
								Template: corev1.PodTemplateSpec{
									ObjectMeta: metav1.ObjectMeta{
										Labels: csvPodLabels(),
									},
									Spec: corev1.PodSpec{
										ServiceAccountName: csvServiceAccountName(),
										Containers: []corev1.Container{{
											Name:         csvContainerName(),
											Image:        csvContainerImage(),
											Resources:    csvContainerResources(),
											Env:          csvContainerEnv(),
											EnvFrom:      csvContainerEnvFrom(),
											VolumeMounts: csvContainerVolumeMounts(),
										}},
										Volumes:      csvPodVolumes(),
										NodeSelector: csvPodNodeSelector(),
										Tolerations:  csvPodTolerations(),
										Affinity:     csvPodAffinity(),
									},
								},
							},
						},
					},
				},
			},

			// TODO: Do Labels and Selector need to be accounted for in the conversion logic?
			Labels:   nil,
			Selector: nil,
		},
	}
	bundleAnnotations := map[string]map[string]string{
		"annotations": {
			"operators.operatorframework.io.bundle.manifests.v1": "manifests/",
			"operators.operatorframework.io.bundle.mediatype.v1": "registry+v1",
			"operators.operatorframework.io.bundle.package.v1":   "example-operator",
		},
	}
	return fstest.MapFS{
		"manifests/configmap.yaml":  newFSFile(mustMarshal(configMap)),
		"manifests/csv.yaml":        newFSFile(mustMarshal(csv)),
		"manifests/crd.yaml":        newFSFile(mustMarshal(crd)),
		"metadata/annotations.yaml": newFSFile(mustMarshal(bundleAnnotations)),
	}
}

func configMapData() map[string]string {
	return map[string]string{"config": "example-config"}
}

func csvLabels() map[string]string {
	return map[string]string{"csvLabel": "csvLabelValue"}
}

func csvAnnotations() map[string]string {
	return map[string]string{"csvAnnotation": "csvAnnotationValue"}
}

func csvSpecMinKubeVersion() string {
	return "1.31.0"
}

func csvSpecDescription() string {
	return "csvSpecDescription"
}

func csvSpecKeywords() []string {
	return []string{"csvSpecKeyword1", "csvSpecKeyword2"}
}

func csvSpecAnnotations() map[string]string {
	return map[string]string{"csvSpecAnnotation": "csvSpecAnnotationValue"}
}

func csvSpecInstallModes(supportedInstallModes []v1alpha1.InstallModeType) []v1alpha1.InstallMode {
	allInstallModes := []v1alpha1.InstallModeType{
		v1alpha1.InstallModeTypeAllNamespaces,
		v1alpha1.InstallModeTypeOwnNamespace,
		v1alpha1.InstallModeTypeSingleNamespace,
		v1alpha1.InstallModeTypeMultiNamespace,
	}
	supported := sets.New[v1alpha1.InstallModeType](supportedInstallModes...)
	modes := make([]v1alpha1.InstallMode, 0, len(allInstallModes))
	for _, mode := range allInstallModes {
		modes = append(modes, v1alpha1.InstallMode{Type: mode, Supported: supported.Has(mode)})
	}
	return modes
}

func csvSpecProvider() v1alpha1.AppLink {
	return v1alpha1.AppLink{
		URL: "https://example.com",
	}
}

func csvSpecLinks() []v1alpha1.AppLink {
	return []v1alpha1.AppLink{
		{
			Name: "Operator source",
			URL:  "https://example.com/operator",
		},
		{
			Name: "Operand source",
			URL:  "https://example.com/operand",
		},
	}
}

func csvSpecVersion() version.OperatorVersion {
	return version.OperatorVersion{Version: semver.MustParse("0.1.0")}
}

func csvSpecMaintainers() []v1alpha1.Maintainer {
	return []v1alpha1.Maintainer{
		{
			Name:  "maintainer1",
			Email: "maintainer1@example.com",
		},
		{
			Name:  "maintainer2",
			Email: "maintainer2@example.com",
		},
	}
}

func csvSpecCRDDefinitions(crd apiextensionsv1.CustomResourceDefinition) v1alpha1.CustomResourceDefinitions {
	descs := make([]v1alpha1.CRDDescription, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		descs = append(descs, v1alpha1.CRDDescription{
			Name:    crd.Name,
			Version: v.Name,
			Kind:    crd.Spec.Names.Kind,
		})
	}
	return v1alpha1.CustomResourceDefinitions{Owned: descs}
}

func csvSpecAPIServiceDefinitions(included bool) v1alpha1.APIServiceDefinitions {
	if !included {
		return v1alpha1.APIServiceDefinitions{}
	}
	return v1alpha1.APIServiceDefinitions{
		Owned: []v1alpha1.APIServiceDescription{
			{
				Name:    "v1alpha1.example.com",
				Group:   "example.com",
				Version: "v1alpha1",
				Kind:    "Example",
			},
		},
	}
}

func csvSpecWebhookDefinitions(included bool) []v1alpha1.WebhookDescription {
	if !included {
		return nil
	}
	return []v1alpha1.WebhookDescription{
		{Type: v1alpha1.ValidatingAdmissionWebhook},
		{Type: v1alpha1.MutatingAdmissionWebhook},
		{Type: v1alpha1.ConversionWebhook},
	}
}

func csvDeploymentLabels() map[string]string {
	return map[string]string{"csvDeploymentLabel": "csvDeploymentLabelValue"}
}

func csvDeploymentReplicas() *int32 {
	return ptr.To(int32(1))
}

func csvDeploymentSelector() *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: csvPodLabels()}
}

func csvPermissionRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups: []string{"example.com"},
		Resources: []string{"tests"},
		Verbs:     []string{"*"},
	}}
}

func csvClusterPermissionRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{{
		APIGroups: []string{"example.com"},
		Resources: []string{"clustertests"},
		Verbs:     []string{"*"},
	}}
}

func csvServiceAccountName() string {
	return "csvServiceAccountName"
}

func csvPodLabels() map[string]string {
	return map[string]string{"csvPodLabel": "csvPodLabelValue"}
}

func csvPodVolumes() []corev1.Volume {
	return []corev1.Volume{{Name: "csvVolume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
}

func csvPodNodeSelector() map[string]string {
	return map[string]string{"csvPodNodeSelector": "csvPodNodeSelectorValue"}
}

func csvPodTolerations() []corev1.Toleration {
	return []corev1.Toleration{{Key: "csvPodTolerationKey", Operator: corev1.TolerationOpExists}}
}

func csvPodAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      "csvPodAffinityKey",
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{"csvPodAffinityValue"},
					}},
				}},
			},
		},
	}
}

func csvContainerName() string {
	return "csvContainerName"
}

func csvContainerImage() string {
	return "csvContainerImage:latest"
}

func csvContainerResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("100Mi"),
		},
	}
}

func csvContainerEnv() []corev1.EnvVar {
	return []corev1.EnvVar{{Name: "csvContainerEnvName", Value: "csvContainerEnvValue"}}
}

func csvContainerEnvFrom() []corev1.EnvFromSource {
	return []corev1.EnvFromSource{{
		ConfigMapRef: &corev1.ConfigMapEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "csvContainerEnvFromName"},
		},
	}}
}

func csvContainerVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "csvContainerVolumeMountName", MountPath: "csvContainerVolumeMountPath"},
	}
}

func templateChart(chrt *chart.Chart, namespace string, values chartutil.Values) (string, error) {
	i := action.NewInstall(&action.Configuration{})
	i.Namespace = namespace
	i.DryRun = true
	i.DryRunOption = "true"
	i.ReleaseName = "release-name"
	i.ClientOnly = true
	i.IncludeCRDs = true
	i.KubeVersion = &chartutil.KubeVersion{
		Version: "v1.31.0",
		Major:   "1",
		Minor:   "31",
	}
	rel, err := i.Run(chrt, values)
	if err != nil {
		return "", err
	}
	return rel.Manifest, nil
}
