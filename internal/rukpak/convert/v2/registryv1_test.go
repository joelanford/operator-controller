package v2

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-openapi/jsonreference"
	"github.com/go-openapi/spec"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"
)

func Test_getWatchNamespacesSchema(t *testing.T) {
	tests := []struct {
		supportedInstallModeSets []sets.Set[v1alpha1.InstallModeType]
		shouldPanic              bool
		want                     watchNamespaceSchemaConfig
	}{
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces)},
			want: watchNamespaceSchemaConfig{
				TemplateHelperDefaultValue: `""`,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "installMode",
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type: spec.StringOrArray{"string"},
						Enum: []interface{}{"AllNamespaces", "OwnNamespace"},
					},
				},
				TemplateHelperDefaultValue: `"AllNamespaces"`,
				AllowReleaseNamespace:      true,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespace",
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:        spec.StringOrArray{"string"},
						Description: "A namespace that the extension should watch.",
						Pattern:     watchNamespacePattern,
						MinLength:   ptr.To(int64(1)),
						MaxLength:   ptr.To(int64(63)),
					},
				},
				TemplateHelperDefaultValue: `""`,
				AllowReleaseNamespace:      true,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeMultiNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespaces",
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:     spec.StringOrArray{"array"},
						MinItems: ptr.To(int64(1)),
						MaxItems: ptr.To(int64(10)),
						Items: &spec.SchemaOrArray{
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type:        spec.StringOrArray{"string"},
									Description: "A namespace that the extension should watch.",
									Pattern:     watchNamespacePattern,
									MinLength:   ptr.To(int64(1)),
									MaxLength:   ptr.To(int64(63)),
								},
							},
						},
					},
				},
				TemplateHelperDefaultValue: `(list "")`,
				AllowReleaseNamespace:      true,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeMultiNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespaces",
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:     spec.StringOrArray{"array"},
						MinItems: ptr.To(int64(1)),
						MaxItems: ptr.To(int64(10)),
						Items: &spec.SchemaOrArray{
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type:        spec.StringOrArray{"string"},
									Description: "A namespace that the extension should watch.",
									Pattern:     watchNamespacePattern,
									MinLength:   ptr.To(int64(1)),
									MaxLength:   ptr.To(int64(63)),
								},
							},
						},
					},
				},
				TemplateHelperDefaultValue: `(list "")`,
				AllowReleaseNamespace:      false,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeAllNamespaces, v1alpha1.InstallModeTypeSingleNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespace",
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:        spec.StringOrArray{"string"},
						Description: "A namespace that the extension should watch.",
						Pattern:     watchNamespacePattern,
						MinLength:   ptr.To(int64(1)),
						MaxLength:   ptr.To(int64(63)),
					},
				},
				TemplateHelperDefaultValue: `""`,
				AllowReleaseNamespace:      false,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace),
			},
			want: watchNamespaceSchemaConfig{
				TemplateHelperDefaultValue: `.Release.Namespace`,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespace",
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:        spec.StringOrArray{"string"},
						Description: "A namespace that the extension should watch.",
						Pattern:     watchNamespacePattern,
						MinLength:   ptr.To(int64(1)),
						MaxLength:   ptr.To(int64(63)),
					},
				},
				TemplateHelperDefaultValue: `.Release.Namespace`,
				AllowReleaseNamespace:      true,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeOwnNamespace, v1alpha1.InstallModeTypeMultiNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespaces",
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:     spec.StringOrArray{"array"},
						MinItems: ptr.To(int64(1)),
						MaxItems: ptr.To(int64(10)),
						Items: &spec.SchemaOrArray{
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type:        spec.StringOrArray{"string"},
									Description: "A namespace that the extension should watch.",
									Pattern:     watchNamespacePattern,
									MinLength:   ptr.To(int64(1)),
									MaxLength:   ptr.To(int64(63)),
								},
							},
						},
					},
				},
				TemplateHelperDefaultValue: `(list .Release.Namespace)`,
				AllowReleaseNamespace:      true,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeSingleNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespace",
				Required:     true,
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:        spec.StringOrArray{"string"},
						Description: "A namespace that the extension should watch.",
						Pattern:     watchNamespacePattern,
						MinLength:   ptr.To(int64(1)),
						MaxLength:   ptr.To(int64(63)),
					},
				},
				AllowReleaseNamespace: false,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeSingleNamespace, v1alpha1.InstallModeTypeMultiNamespace),
				sets.New[v1alpha1.InstallModeType](v1alpha1.InstallModeTypeMultiNamespace),
			},
			want: watchNamespaceSchemaConfig{
				IncludeField: true,
				FieldName:    "watchNamespaces",
				Required:     true,
				Schema: &spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type:     spec.StringOrArray{"array"},
						MinItems: ptr.To(int64(1)),
						MaxItems: ptr.To(int64(10)),
						Items: &spec.SchemaOrArray{
							Schema: &spec.Schema{
								SchemaProps: spec.SchemaProps{
									Type:        spec.StringOrArray{"string"},
									Description: "A namespace that the extension should watch.",
									Pattern:     watchNamespacePattern,
									MinLength:   ptr.To(int64(1)),
									MaxLength:   ptr.To(int64(63)),
								},
							},
						},
					},
				},
				AllowReleaseNamespace: false,
			},
		},
		{
			supportedInstallModeSets: []sets.Set[v1alpha1.InstallModeType]{{}},
			shouldPanic:              true,
		},
	}
	for _, tt := range tests {
		for _, supportedInstallModes := range tt.supportedInstallModeSets {
			modes := []string{}
			installModes := []v1alpha1.InstallMode{}

			for _, mode := range []v1alpha1.InstallModeType{
				v1alpha1.InstallModeTypeAllNamespaces,
				v1alpha1.InstallModeTypeOwnNamespace,
				v1alpha1.InstallModeTypeSingleNamespace,
				v1alpha1.InstallModeTypeMultiNamespace,
			} {
				if supportedInstallModes.Has(mode) {
					modes = append(modes, string(mode))
					installModes = append(installModes, v1alpha1.InstallMode{Type: mode, Supported: supportedInstallModes.Has(mode)})
				}
			}
			name := strings.Join(modes, "|")
			if name == "" {
				name = "none"
			}
			t.Run(name, func(t *testing.T) {
				if tt.shouldPanic {
					require.Panics(t, func() {
						getWatchNamespacesSchema(installModes)
					})
					return
				}
				got := getWatchNamespacesSchema(installModes)
				if diff := cmp.Diff(got, tt.want, cmpopts.IgnoreUnexported(jsonreference.Ref{})); diff != "" {
					t.Errorf("getWatchNamespacesSchema() mismatch (-got +want):\n%s", diff)
				}
			})
		}
	}
}

func Test_newValuesSchemaFile(t *testing.T) {
	tests := []struct {
		name               string
		watchNsSchemaSetup watchNamespaceSchemaConfig
		want               string
	}{
		{
			name:               "No target-namespace-related field",
			watchNsSchemaSetup: watchNamespaceSchemaConfig{IncludeField: false},
			want:               fmt.Sprintf(schemaTemplate, "", ""),
		},
		{
			name:               "Target-namespace-related field, optional",
			watchNsSchemaSetup: watchNamespaceSchemaConfig{IncludeField: true, FieldName: "zz_field", Schema: spec.StringProperty()},
			want:               fmt.Sprintf(schemaTemplate, "", ",\n        \"zz_field\": {\n            \"type\": \"string\"\n        }"),
		},
		{
			name:               "Target-namespace-related field, required",
			watchNsSchemaSetup: watchNamespaceSchemaConfig{IncludeField: true, FieldName: "zz_field", Schema: spec.StringProperty(), Required: true},
			want:               fmt.Sprintf(schemaTemplate, "\n    \"required\": [\n        \"zz_field\"\n    ],", ",\n        \"zz_field\": {\n            \"type\": \"string\"\n        }"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schBytes, err := newValuesSchemaFile(tt.watchNsSchemaSetup)
			require.NoError(t, err)

			var sch spec.Schema
			require.NoError(t, json.Unmarshal(schBytes, &sch))
		})
	}
}

func Test_getSchema(t *testing.T) {
	assertSchema := func(t *testing.T, gotSchema spec.Schema, definitionName, propertyName string) {
		t.Helper()

		defSchema, ok := gotSchema.Definitions[definitionName]
		require.True(t, ok)
		require.NotNil(t, defSchema)
		prop, ok := defSchema.Properties[propertyName]
		assert.True(t, ok)
		assert.NotNil(t, prop)

		propSchema, ok := gotSchema.Properties[propertyName]
		require.True(t, ok)
		require.NotNil(t, propSchema)
		assert.Equal(t, *spec.RefProperty(fmt.Sprintf("#/definitions/%s/properties/%s", definitionName, propertyName)), propSchema)
	}
	tests := []struct {
		name            string
		extraProperties spec.SchemaProperties
		overrideEmbed   []byte
		assert          func(*testing.T, *spec.Schema, error)
	}{
		{
			name: "Default",
			extraProperties: spec.SchemaProperties{
				"extra": *spec.StringProperty(),
			},
			assert: func(t *testing.T, gotSchema *spec.Schema, err error) {
				require.NoError(t, err)
				require.NotNil(t, gotSchema)
				assertSchema(t, *gotSchema, "io.k8s.api.apps.v1.DeploymentSpec", "selector")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "nodeSelector")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "tolerations")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "resources")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "envFrom")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "env")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "volumes")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.Container", "volumeMounts")
				assertSchema(t, *gotSchema, "io.k8s.api.core.v1.PodSpec", "affinity")
				assertSchema(t, *gotSchema, "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta", "annotations")

				extraProp, ok := gotSchema.Properties["extra"]
				assert.True(t, ok)
				assert.Equal(t, *spec.StringProperty(), extraProp)
			},
		},
		{
			name: "Duplicate property",
			extraProperties: spec.SchemaProperties{
				"selector": *spec.StringProperty(),
			},
			assert: func(t *testing.T, gotSchema *spec.Schema, err error) {
				require.Error(t, err)
				require.Nil(t, gotSchema)
			},
		},
		{
			name:          "Invalid",
			overrideEmbed: []byte(`{`),
			assert: func(t *testing.T, gotSchema *spec.Schema, err error) {
				require.Error(t, err)
				require.Nil(t, gotSchema)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			openapiSpecBytes := make([]byte, len(appsV1OpenAPI))
			copy(openapiSpecBytes, appsV1OpenAPI)
			if tt.overrideEmbed != nil {
				openapiSpecBytes = tt.overrideEmbed
			}
			gotSch, err := getSchema(openapiSpecBytes, tt.extraProperties)
			tt.assert(t, gotSch, err)
		})
	}
}

func Test_newTargetNamespacesTemplateHelper(t *testing.T) {
	tests := []struct {
		name  string
		setup watchNamespaceSchemaConfig
		want  string
	}{
		{
			name:  "no field",
			setup: watchNamespaceSchemaConfig{IncludeField: false, TemplateHelperDefaultValue: `"test"`},
			want:  "{{- define \"olm.targetNamespaces\" -}}\n{{- \"test\" -}}\n{{- end -}}\n",
		},
		{
			name:  "install mode, optional",
			setup: watchNamespaceSchemaConfig{IncludeField: true, FieldName: "installMode", TemplateHelperDefaultValue: `"AllNamespaces"`},
			want: `{{- define "olm.targetNamespaces" -}}
{{- $installMode := .Values.installMode -}}
{{- if not $installMode -}}
  {{- $installMode = "AllNamespaces" -}}
{{- end -}}
{{- if eq $installMode "AllNamespaces" -}}
  {{- "" -}}
{{- else if eq $installMode "OwnNamespace" -}}
  {{- .Release.Namespace -}}
{{- else -}}
  {{- fail (printf "Unsupported install mode: %s" $installMode) -}}
{{- end -}}
{{- end -}}
`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newTargetNamespacesTemplateHelper(tt.setup)
			assert.Equal(t, "templates/_helpers.targetNamespaces.tpl", got.Name)
			assert.Equal(t, tt.want, string(got.Data))
		})
	}
}

const schemaTemplate = `{
    "type": "object",%s
    "properties": {
        "affinity": {
            "$ref": "#/definitions/io.k8s.api.core.v1.PodSpec/properties/affinity"
        },
        "annotations": {
            "$ref": "#/definitions/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta/properties/annotations"
        },
        "env": {
            "$ref": "#/definitions/io.k8s.api.core.v1.Container/properties/env"
        },
        "envFrom": {
            "$ref": "#/definitions/io.k8s.api.core.v1.Container/properties/envFrom"
        },
        "nodeSelector": {
            "$ref": "#/definitions/io.k8s.api.core.v1.PodSpec/properties/nodeSelector"
        },
        "resources": {
            "$ref": "#/definitions/io.k8s.api.core.v1.Container/properties/resources"
        },
        "selector": {
            "$ref": "#/definitions/io.k8s.api.apps.v1.DeploymentSpec/properties/selector"
        },
        "tolerations": {
            "$ref": "#/definitions/io.k8s.api.core.v1.PodSpec/properties/tolerations"
        },
        "volumeMounts": {
            "$ref": "#/definitions/io.k8s.api.core.v1.Container/properties/volumeMounts"
        },
        "volumes": {
            "$ref": "#/definitions/io.k8s.api.core.v1.PodSpec/properties/volumes"
        }%s
    },
    "additionalProperties": false,
    "definitions": {
        "io.k8s.api.apps.v1.DeploymentSpec": {
            "description": "DeploymentSpec is the specification of the desired behavior of the Deployment.",
            "type": "object",
            "required": [
                "selector",
                "template"
            ],
            "properties": {
                "selector": {
                    "description": "Label selector for pods. Existing ReplicaSets whose pods are selected by this will be the ones affected by this deployment. It must match the pod template's labels.",
                    "allOf": [
                        {
                            "$ref": "#/definitions/io.k8s.apimachinery.pkg.apis.meta.v1.LabelSelector"
                        }
                    ]
                }
            }
        },
        "io.k8s.api.core.v1.Container": {
            "description": "A single application container that you want to run within a pod.",
            "type": "object",
            "required": [
                "name"
            ],
            "properties": {
                "env": {
                    "description": "List of environment variables to set in the container. Cannot be updated.",
                    "type": "array",
                    "items": {
                        "default": {},
                        "allOf": [
                            {
                                "$ref": "#/definitions/io.k8s.api.core.v1.EnvVar"
                            }
                        ]
                    },
                    "x-kubernetes-list-map-keys": [
                        "name"
                    ],
                    "x-kubernetes-list-type": "map",
                    "x-kubernetes-patch-merge-key": "name",
                    "x-kubernetes-patch-strategy": "merge"
                },
                "envFrom": {
                    "description": "List of sources to populate environment variables in the container. The keys defined within a source must be a C_IDENTIFIER. All invalid keys will be reported as an event when the container is starting. When a key exists in multiple sources, the value associated with the last source will take precedence. Values defined by an Env with a duplicate key will take precedence. Cannot be updated.",
                    "type": "array",
                    "items": {
                        "default": {},
                        "allOf": [
                            {
                                "$ref": "#/definitions/io.k8s.api.core.v1.EnvFromSource"
                            }
                        ]
                    },
                    "x-kubernetes-list-type": "atomic"
                },
                "resources": {
                    "description": "Compute Resources required by this container. Cannot be updated. More info: https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/",
                    "default": {},
                    "allOf": [
                        {
                            "$ref": "#/definitions/io.k8s.api.core.v1.ResourceRequirements"
                        }
                    ]
                },
                "volumeMounts": {
                    "description": "Pod volumes to mount into the container's filesystem. Cannot be updated.",
                    "type": "array",
                    "items": {
                        "default": {},
                        "allOf": [
                            {
                                "$ref": "#/definitions/io.k8s.api.core.v1.VolumeMount"
                            }
                        ]
                    },
                    "x-kubernetes-list-map-keys": [
                        "mountPath"
                    ],
                    "x-kubernetes-list-type": "map",
                    "x-kubernetes-patch-merge-key": "mountPath",
                    "x-kubernetes-patch-strategy": "merge"
                }
            }
        },
        "io.k8s.api.core.v1.PodSpec": {
            "description": "PodSpec is a description of a pod.",
            "type": "object",
            "required": [
                "containers"
            ],
            "properties": {
                "affinity": {
                    "description": "If specified, the pod's scheduling constraints",
                    "allOf": [
                        {
                            "$ref": "#/definitions/io.k8s.api.core.v1.Affinity"
                        }
                    ]
                },
                "nodeSelector": {
                    "description": "NodeSelector is a selector which must be true for the pod to fit on a node. Selector which must match a node's labels for the pod to be scheduled on that node. More info: https://kubernetes.io/docs/concepts/configuration/assign-pod-node/",
                    "type": "object",
                    "additionalProperties": {
                        "type": "string",
                        "default": ""
                    },
                    "x-kubernetes-map-type": "atomic"
                },
                "tolerations": {
                    "description": "If specified, the pod's tolerations.",
                    "type": "array",
                    "items": {
                        "default": {},
                        "allOf": [
                            {
                                "$ref": "#/definitions/io.k8s.api.core.v1.Toleration"
                            }
                        ]
                    },
                    "x-kubernetes-list-type": "atomic"
                },
                "volumes": {
                    "description": "List of volumes that can be mounted by containers belonging to the pod. More info: https://kubernetes.io/docs/concepts/storage/volumes",
                    "type": "array",
                    "items": {
                        "default": {},
                        "allOf": [
                            {
                                "$ref": "#/definitions/io.k8s.api.core.v1.Volume"
                            }
                        ]
                    },
                    "x-kubernetes-list-map-keys": [
                        "name"
                    ],
                    "x-kubernetes-list-type": "map",
                    "x-kubernetes-patch-merge-key": "name",
                    "x-kubernetes-patch-strategy": "merge,retainKeys"
                }
            }
        },
        "io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta": {
            "description": "ObjectMeta is metadata that all persisted resources must have, which includes all objects users must create.",
            "type": "object",
            "properties": {
                "annotations": {
                    "description": "Annotations is an unstructured key value map stored with a resource that may be set by external tools to store and retrieve arbitrary metadata. They are not queryable and should be preserved when modifying objects. More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/annotations",
                    "type": "object",
                    "additionalProperties": {
                        "type": "string",
                        "default": ""
                    }
                }
            }
        }
    },
    "$schema": "http://json-schema.org/schema#"
}
`
