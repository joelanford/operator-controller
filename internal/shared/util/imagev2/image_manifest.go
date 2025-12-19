package imagev2

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/archive"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/pkg/compression"
	"k8s.io/klog/v2"
)

// FetchImageConfig fetches and decodes the OCI image config from manifest bytes.
// This is commonly used by handlers to inspect image labels for matching decisions.
// The manifestBytes must be an OCI image manifest.
func FetchImageConfig(ctx context.Context, repo Repository, manifestBytes []byte) (*ocispecv1.Image, error) {
	var manifest ocispecv1.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}

	reader, err := repo.FetchBlob(ctx, manifest.Config)
	if err != nil {
		return nil, fmt.Errorf("fetching config blob: %w", err)
	}

	cfg, err := decodeImageConfig(reader)
	return cfg, errors.Join(err, reader.Close())
}

func decodeImageConfig(reader io.Reader) (*ocispecv1.Image, error) {
	var cfg ocispecv1.Image
	if err := json.NewDecoder(reader).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	return &cfg, nil
}

// LayerFilter filters or modifies tar entries during layer extraction.
// Return true to keep the entry, false to skip it.
type LayerFilter = archive.Filter

// ImageManifestUnpacker unpacks OCI image manifests by applying layers in order.
type ImageManifestUnpacker struct {
	Filter LayerFilter
}

// Unpack extracts layers from the manifest in order, applying the configured filter.
// The manifestBytes must be an OCI image manifest.
func (u *ImageManifestUnpacker) Unpack(ctx context.Context, repo Repository, manifestBytes []byte, dest string) error {
	l := klog.FromContext(ctx)

	var manifest ocispecv1.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	l.Info("unpacking image", "path", dest)
	for i, layer := range manifest.Layers {
		if err := u.applyLayer(ctx, repo, dest, layer); err != nil {
			return fmt.Errorf("applying layer %d: %w", i, err)
		}
		l.Info("applied layer", "layer", i)

	}
	return nil
}

func (u *ImageManifestUnpacker) applyLayer(ctx context.Context, repo Repository, dest string, layer ocispecv1.Descriptor) error {
	reader, err := repo.FetchBlob(ctx, layer)
	if err != nil {
		return err
	}

	err = u.decompressAndApply(ctx, dest, reader)
	return errors.Join(err, reader.Close())
}

func (u *ImageManifestUnpacker) decompressAndApply(ctx context.Context, dest string, reader io.Reader) error {
	decompressed, _, err := compression.AutoDecompress(reader)
	if err != nil {
		return fmt.Errorf("decompressing layer: %w", err)
	}

	err = u.applyArchive(ctx, dest, decompressed)
	return errors.Join(err, decompressed.Close())
}

func (u *ImageManifestUnpacker) applyArchive(ctx context.Context, dest string, reader io.Reader) error {
	var opts []archive.ApplyOpt
	if u.Filter != nil {
		opts = append(opts, archive.WithFilter(u.Filter))
	}
	_, err := archive.Apply(ctx, dest, reader, opts...)
	return err
}

// CombineFilters executes each filter in order. If any filter returns false or an error,
// the combined filter immediately returns that result.
func CombineFilters(filters ...LayerFilter) LayerFilter {
	return func(h *tar.Header) (bool, error) {
		for _, filter := range filters {
			keep, err := filter(h)
			if err != nil {
				return false, err
			}
			if !keep {
				return false, nil
			}
		}
		return true, nil
	}
}

// OnlyPaths keeps only files and directories that match any of the given paths
// or are present under any of those paths.
func OnlyPaths(paths ...string) LayerFilter {
	wantPaths := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != "" {
			wantPaths = append(wantPaths, path.Clean(strings.TrimPrefix(p, "/")))
		}
	}

	return func(h *tar.Header) (bool, error) {
		headerPath := path.Clean(strings.TrimPrefix(h.Name, "/"))
		for _, wantPath := range wantPaths {
			relPath, err := filepath.Rel(wantPath, headerPath)
			if err != nil {
				return false, fmt.Errorf("getting relative path: %w", err)
			}
			if relPath != ".." && !strings.HasPrefix(relPath, "../") {
				return true, nil
			}
		}
		return false, nil
	}
}

// ForceOwnershipRWX sets a tar header's Uid and Gid to the current process's
// Uid and Gid and ensures its permissions give the owner full read/write/execute permission.
func ForceOwnershipRWX() LayerFilter {
	uid := os.Getuid()
	gid := os.Getgid()
	return func(h *tar.Header) (bool, error) {
		h.Uid = uid
		h.Gid = gid
		h.Mode |= 0700
		h.PAXRecords = nil
		h.Xattrs = nil //nolint:staticcheck
		return true, nil
	}
}
