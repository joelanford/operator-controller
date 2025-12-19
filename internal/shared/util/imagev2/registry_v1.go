package imagev2

import (
	"context"

	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// Bundle label keys used to identify and configure registry+v1 bundles.
const (
	// BundleMediaTypeLabel is the label on bundle images that specifies the bundle format.
	BundleMediaTypeLabel = "operators.operatorframework.io.bundle.mediatype.v1"

	// BundleManifestsLabel is the label on bundle images that specifies the directory
	// containing the bundle manifests (CSV, CRDs, etc.).
	BundleManifestsLabel = "operators.operatorframework.io.bundle.manifests.v1"

	// BundleMetadataLabel is the label on bundle images that specifies the directory
	// containing the bundle metadata (annotations.yaml, properties.yaml).
	BundleMetadataLabel = "operators.operatorframework.io.bundle.metadata.v1"

	// BundleMediaTypeRegistryV1 is the media type value for registry+v1 bundles.
	BundleMediaTypeRegistryV1 = "registry+v1"
)

// RegistryV1Handler unpacks registry+v1 bundle images by extracting manifests and metadata directories.
type RegistryV1Handler struct{}

func (h *RegistryV1Handler) Name() string { return "olm.operatorframework.io/registry+v1" }

func (h *RegistryV1Handler) Matches(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte) bool {
	if !isManifest(desc.MediaType) {
		return false
	}

	cfg, err := FetchImageConfig(ctx, repo, manifestBytes)
	if err != nil {
		return false
	}

	mediaType, ok := cfg.Config.Labels[BundleMediaTypeLabel]
	return ok && mediaType == BundleMediaTypeRegistryV1
}

func (h *RegistryV1Handler) Unpack(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte, dest string) error {
	cfg, err := FetchImageConfig(ctx, repo, manifestBytes)
	if err != nil {
		return err
	}

	// Only filter to specific paths if both labels are present and non-empty
	manifestsDir := cfg.Config.Labels[BundleManifestsLabel]
	metadataDir := cfg.Config.Labels[BundleMetadataLabel]

	var filters []LayerFilter
	if manifestsDir != "" && metadataDir != "" {
		filters = append(filters, OnlyPaths(manifestsDir, metadataDir))
	}
	filters = append(filters, ForceOwnershipRWX())

	unpacker := &ImageManifestUnpacker{
		Filter: CombineFilters(filters...),
	}
	return unpacker.Unpack(ctx, repo, manifestBytes, dest)
}