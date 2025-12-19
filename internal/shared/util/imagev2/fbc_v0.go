package imagev2

import (
	"context"
	"fmt"

	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/manifest"
)

// ConfigDirLabel is the label on catalog images that specifies the directory
// containing the catalog configuration.
const ConfigDirLabel = "operators.operatorframework.io.index.configs.v1"

// FBCv0Handler unpacks FBC catalog images by extracting only the configs directory.
// It handles both single-platform manifests and multi-platform manifest lists/indexes.
type FBCv0Handler struct{}

func (h *FBCv0Handler) Name() string { return "olm.operatorframework.io/fbc+v0" }

func (h *FBCv0Handler) Matches(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte) bool {
	// If this is a manifest list/index, resolve to the platform-specific manifest first
	if isIndex(desc.MediaType) {
		platformDesc, platformManifestBytes, err := resolvePlatformManifest(ctx, repo, manifestBytes, desc.MediaType)
		if err != nil {
			return false
		}

		desc = platformDesc
		manifestBytes = platformManifestBytes
	}

	if !isManifest(desc.MediaType) {
		return false
	}

	cfg, err := FetchImageConfig(ctx, repo, manifestBytes)
	if err != nil {
		return false
	}

	_, ok := cfg.Config.Labels[ConfigDirLabel]
	return ok
}

func (h *FBCv0Handler) Unpack(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte, dest string) error {
	// If this is a manifest list/index, resolve to the platform-specific manifest first
	if isIndex(desc.MediaType) {
		platformDesc, platformManifestBytes, err := resolvePlatformManifest(ctx, repo, manifestBytes, desc.MediaType)
		if err != nil {
			return fmt.Errorf("resolving platform manifest: %w", err)
		}
		desc = platformDesc
		manifestBytes = platformManifestBytes
	}

	cfg, err := FetchImageConfig(ctx, repo, manifestBytes)
	if err != nil {
		return err
	}

	configDir := cfg.Config.Labels[ConfigDirLabel]

	unpacker := &ImageManifestUnpacker{
		Filter: CombineFilters(
			OnlyPaths(configDir),
			ForceOwnershipRWX(),
		),
	}
	return unpacker.Unpack(ctx, repo, manifestBytes, dest)
}

// resolvePlatformManifest selects the appropriate platform manifest from a manifest list/index.
func resolvePlatformManifest(ctx context.Context, repo Repository, indexBytes []byte, indexMediaType string) (ocispecv1.Descriptor, []byte, error) {
	list, err := manifest.ListFromBlob(indexBytes, indexMediaType)
	if err != nil {
		return ocispecv1.Descriptor{}, nil, fmt.Errorf("parsing manifest list: %w", err)
	}

	// ChooseInstance with nil uses the current runtime platform
	chosenDigest, err := list.ChooseInstance(nil)
	if err != nil {
		return ocispecv1.Descriptor{}, nil, fmt.Errorf("choosing platform instance: %w", err)
	}

	instanceInfo, err := list.Instance(chosenDigest)
	if err != nil {
		return ocispecv1.Descriptor{}, nil, fmt.Errorf("getting instance info: %w", err)
	}

	desc := ocispecv1.Descriptor{
		MediaType: instanceInfo.MediaType,
		Digest:    chosenDigest,
		Size:      instanceInfo.Size,
	}

	manifestBytes, mediaType, err := repo.FetchManifest(ctx, desc)
	if err != nil {
		return ocispecv1.Descriptor{}, nil, fmt.Errorf("fetching platform manifest: %w", err)
	}
	desc.MediaType = mediaType

	return desc, manifestBytes, nil
}
