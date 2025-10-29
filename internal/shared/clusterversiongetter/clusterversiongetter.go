package clusterversiongetter

import (
	"context"
	"errors"
	"fmt"
	"strings"

	bsemver "github.com/blang/semver/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/operator-framework/operator-controller/internal/shared/fbc"
)

// ClusterVersionGetter provides methods to retrieve cluster version information
type ClusterVersionGetter interface {
	GetVersion(ctx context.Context) (fbc.MajorMinor, error)
}

type KubernetesVersionGetter struct {
	discoveryClient discovery.DiscoveryInterface
}

type OpenshiftVersionGetter struct {
	dynamicClient dynamic.Interface
}

func NewKubernetesVersionGetter(config *rest.Config) (ClusterVersionGetter, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create discovery client: %w", err)
	}
	return &KubernetesVersionGetter{
		discoveryClient: discoveryClient,
	}, nil
}

func (g *KubernetesVersionGetter) GetVersion(_ context.Context) (fbc.MajorMinor, error) {
	serverVersionInfo, err := g.discoveryClient.ServerVersion()
	if err != nil {
		return fbc.MajorMinor{}, fmt.Errorf("failed to get Kubernetes server version: %w", err)
	}

	serverVersion, err := bsemver.Parse(strings.TrimPrefix(serverVersionInfo.GitVersion, "v"))
	if err != nil {
		return fbc.MajorMinor{}, fmt.Errorf("failed to parse Kubernetes server version: %w", err)
	}

	// Parse the version string (e.g., "v1.28.0") to extract major.minor
	return fbc.NewMajorMinorFromVersion(serverVersion), nil
}

// NewOpenshiftVersionGetter creates a new cluster version getter from a rest config
func NewOpenshiftVersionGetter(config *rest.Config) (ClusterVersionGetter, error) {
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("unable to create dynamic client: %w", err)
	}

	return &OpenshiftVersionGetter{
		dynamicClient: dynamicClient,
	}, nil
}

func (g *OpenshiftVersionGetter) GetVersion(ctx context.Context) (fbc.MajorMinor, error) {
	// Define the ClusterVersion GVR
	clusterVersionGVR := schema.GroupVersionResource{
		Group:    "config.openshift.io",
		Version:  "v1",
		Resource: "clusterversions",
	}

	// Get the ClusterVersion object named "version" (the singleton instance in OpenShift)
	clusterVersion, err := g.dynamicClient.Resource(clusterVersionGVR).Get(ctx, "version", metav1.GetOptions{})
	if err != nil {
		return fbc.MajorMinor{}, fmt.Errorf("failed to get ClusterVersion: %w", err)
	}

	// Parse the current version from the unstructured object
	versionStr, err := parseCurrentVersion(clusterVersion)
	if err != nil {
		return fbc.MajorMinor{}, fmt.Errorf("failed to parse OpenShift version: %w", err)
	}

	version, err := bsemver.Parse(versionStr)
	if err != nil {
		return fbc.MajorMinor{}, fmt.Errorf("failed to parse OpenShift version: %w", err)
	}

	// Convert the version string to MajorMinor
	return fbc.NewMajorMinorFromVersion(version), nil
}

// parseCurrentVersion extracts the current installed version from the ClusterVersion unstructured object
// The current version is stored in status.desired.version (the most recent history entry)
func parseCurrentVersion(obj *unstructured.Unstructured) (string, error) {
	desired, found, err := unstructured.NestedMap(obj.Object, "status", "desired")
	if err != nil {
		return "", fmt.Errorf("error accessing status.history field: %w", err)
	}
	if !found {
		return "", fmt.Errorf("status.desired field not found or empty")
	}

	version, found, err := unstructured.NestedString(desired, "version")
	if err != nil {
		return "", fmt.Errorf("error accessing version field in history: %w", err)
	}
	if !found {
		return "", fmt.Errorf("version field not found in status.desired")
	}

	return version, nil
}

type StaticClusterVersionGetter struct {
	version *fbc.MajorMinor
}

func NewStaticClusterVersionGetter(version *fbc.MajorMinor) ClusterVersionGetter {
	return &StaticClusterVersionGetter{
		version: version,
	}
}

func (g *StaticClusterVersionGetter) GetVersion(_ context.Context) (fbc.MajorMinor, error) {
	if g.version == nil {
		return fbc.MajorMinor{}, errors.New("version not set")
	}
	return *g.version, nil
}
