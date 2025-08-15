package applier_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	featuregatetesting "k8s.io/component-base/featuregate/testing"

	v1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/applier"
	"github.com/operator-framework/operator-controller/internal/operator-controller/features"
)

func TestGetWatchNamespacesWhenFeatureGateIsDisabled(t *testing.T) {
	watchNamespace, err := applier.GetWatchNamespace(t.Context(), &v1.ClusterExtension{
		ObjectMeta: metav1.ObjectMeta{
			Name: "extension",
		},
		Spec: v1.ClusterExtensionSpec{
			Config: &v1.ClusterExtensionConfig{
				Type: v1.ConfigSourceTypeInline,
				Inline: &apiextensionsv1.JSON{
					Raw: []byte(fmt.Sprintf(`{%q:%q}`, applier.ConfigFieldNameWatchNamespace, "watch-namespace")),
				},
			},
		},
	})
	require.NoError(t, err)
	t.Log("Check watchNamespace is '' even if the config is set")
	require.Equal(t, corev1.NamespaceAll, watchNamespace)
}

func TestGetWatchNamespace(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, features.OperatorControllerFeatureGate, features.SingleOwnNamespaceInstallSupport, true)

	for _, tt := range []struct {
		name        string
		want        string
		ce          *v1.ClusterExtension
		expectError bool
	}{
		{
			name: "cluster extension does not have watch namespace config",
			want: corev1.NamespaceAll,
			ce: &v1.ClusterExtension{
				ObjectMeta: metav1.ObjectMeta{
					Name: "extension",
				},
				Spec: v1.ClusterExtensionSpec{},
			},
			expectError: false,
		}, {
			name: "cluster extension has valid namespace config",
			want: "watch-namespace",
			ce: &v1.ClusterExtension{
				ObjectMeta: metav1.ObjectMeta{
					Name: "extension",
				},
				Spec: v1.ClusterExtensionSpec{
					Config: &v1.ClusterExtensionConfig{
						Type: v1.ConfigSourceTypeInline,
						Inline: &apiextensionsv1.JSON{
							Raw: []byte(fmt.Sprintf(`{%q:%q}`, applier.ConfigFieldNameWatchNamespace, "watch-namespace")),
						},
					},
				},
			},
			expectError: false,
		}, {
			name: "cluster extension has invalid namespace config: multiple watch namespaces",
			want: "",
			ce: &v1.ClusterExtension{
				ObjectMeta: metav1.ObjectMeta{
					Name: "extension",
				},
				Spec: v1.ClusterExtensionSpec{
					Config: &v1.ClusterExtensionConfig{
						Type: v1.ConfigSourceTypeInline,
						Inline: &apiextensionsv1.JSON{
							Raw: []byte(fmt.Sprintf(`{%q:%q}`, applier.ConfigFieldNameWatchNamespace, "watch-namespace,watch-namespace2,watch-namespace3")),
						},
					},
				},
			},
			expectError: true,
		}, {
			name: "cluster extension has invalid namespace config: invalid name",
			want: "",
			ce: &v1.ClusterExtension{
				ObjectMeta: metav1.ObjectMeta{
					Name: "extension",
				},
				Spec: v1.ClusterExtensionSpec{
					Config: &v1.ClusterExtensionConfig{
						Type: v1.ConfigSourceTypeInline,
						Inline: &apiextensionsv1.JSON{
							Raw: []byte(fmt.Sprintf(`{%q:%q}`, applier.ConfigFieldNameWatchNamespace, "watch-namespace-")),
						},
					},
				},
			},
			expectError: true,
		}, {
			name: "cluster extension has invalid namespace config: invalid type",
			want: "",
			ce: &v1.ClusterExtension{
				ObjectMeta: metav1.ObjectMeta{
					Name: "extension",
				},
				Spec: v1.ClusterExtensionSpec{
					Config: &v1.ClusterExtensionConfig{
						Type: v1.ConfigSourceTypeInline,
						Inline: &apiextensionsv1.JSON{
							Raw: []byte(fmt.Sprintf(`{%q:[%q]}`, applier.ConfigFieldNameWatchNamespace, "watch-namespace")),
						},
					},
				},
			},
			expectError: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := applier.GetWatchNamespace(t.Context(), tt.ce)
			require.Equal(t, tt.want, got)
			require.Equal(t, tt.expectError, err != nil)
		})
	}
}
