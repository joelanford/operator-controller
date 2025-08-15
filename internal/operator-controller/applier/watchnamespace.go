package applier

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	config2 "github.com/operator-framework/operator-controller/internal/operator-controller/config"
	"github.com/operator-framework/operator-controller/internal/operator-controller/features"
)

const (
	ConfigFieldNameWatchNamespace = "watchNamespace"
)

// GetWatchNamespace determines the watch namespace the ClusterExtension should use
// Note: this is a temporary artifice to enable gated use of single/own namespace install modes
// for registry+v1 bundles. This will go away once the ClusterExtension API is updated to include
// (opaque) runtime configuration.
func GetWatchNamespace(ctx context.Context, ext *ocv1.ClusterExtension) (string, error) {
	if ext == nil || !features.OperatorControllerFeatureGate.Enabled(features.SingleOwnNamespaceInstallSupport) {
		return corev1.NamespaceAll, nil
	}

	configSource := config2.SourceFromClusterExtension(ext)
	values, err := configSource.Values(ctx)
	if err != nil {
		return "", fmt.Errorf("error parsing configuration from cluster extension: %v", err)
	}

	watchNamespaceVal, ok := values[ConfigFieldNameWatchNamespace]
	if !ok {
		return corev1.NamespaceAll, nil
	}

	watchNamespace, ok := watchNamespaceVal.(string)
	if !ok {
		return "", fmt.Errorf("error parsing configuration from cluster extension: %q is not a string", ConfigFieldNameWatchNamespace)
	}
	if errs := validation.IsDNS1123Subdomain(watchNamespace); len(errs) > 0 {
		return "", fmt.Errorf("invalid watch namespace '%s': namespace must consist of lower case alphanumeric characters, '-' or '.', and must start and end with an alphanumeric character", watchNamespace)
	}
	return watchNamespace, nil
}
