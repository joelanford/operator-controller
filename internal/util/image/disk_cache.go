package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/containerd/containerd/archive"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/pkg/blobinfocache/none"
	"github.com/containers/image/v5/pkg/compression"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	errorutil "github.com/operator-framework/operator-controller/internal/util/error"
	fsutil "github.com/operator-framework/operator-controller/internal/util/fs"
)

const ConfigDirLabel = "operators.operatorframework.io.index.configs.v1"

func CatalogCache(basePath string) Cache {
	return &diskCache{
		basePath: basePath,
		filterFunc: func(ctx context.Context, srcRef reference.Named, image ocispecv1.Image) (archive.Filter, error) {
			_, specIsCanonical := srcRef.(reference.Canonical)
			dirToUnpack, ok := image.Config.Labels[ConfigDirLabel]
			if !ok {
				// If the spec is a tagged ref, retries could end up resolving a new digest, where the label
				// might show up. If the spec is canonical, no amount of retries will make the label appear.
				// Therefore, we treat the error as terminal if the reference from the spec is canonical.
				return nil, errorutil.WrapTerminal(fmt.Errorf("catalog image is missing the required label %q", ConfigDirLabel), specIsCanonical)
			}

			return allFilters(
				onlyPath(dirToUnpack),
				forceOwnershipRWX(),
			), nil
		},
	}
}

func BundleCache(basePath string) Cache {
	return &diskCache{
		basePath: basePath,
		filterFunc: func(_ context.Context, _ reference.Named, _ ocispecv1.Image) (archive.Filter, error) {
			return forceOwnershipRWX(), nil
		},
	}
}

type diskCache struct {
	basePath   string
	filterFunc func(context.Context, reference.Named, ocispecv1.Image) (archive.Filter, error)
}

func (a *diskCache) Fetch(ctx context.Context, id string, canonicalRef reference.Canonical) (fs.FS, time.Time, error) {
	l := log.FromContext(ctx)
	unpackPath := a.unpackPath(id, canonicalRef.Digest())
	modTime, err := fsutil.GetDirectoryModTime(unpackPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, time.Time{}, nil
	case errors.Is(err, fsutil.ErrNotDirectory):
		l.Info("unpack path is not a directory; attempting to delete", "path", unpackPath)
		return nil, time.Time{}, fsutil.DeleteReadOnlyRecursive(unpackPath)
	case err != nil:
		return nil, time.Time{}, fmt.Errorf("error checking image content already unpacked: %w", err)
	}
	l.Info("image already unpacked")
	return os.DirFS(a.unpackPath(id, canonicalRef.Digest())), modTime, nil
}

func (a *diskCache) idPath(id string) string {
	return filepath.Join(a.basePath, id)
}

func (a *diskCache) unpackPath(id string, digest digest.Digest) string {
	return filepath.Join(a.idPath(id), digest.String())
}

func (a *diskCache) Store(ctx context.Context, id string, srcRef reference.Named, canonicalRef reference.Canonical, img types.Image, imgSrc types.ImageSource) (fs.FS, time.Time, error) {
	var filter archive.Filter
	if a.filterFunc != nil {
		var err error

		imgCfg, err := img.OCIConfig(ctx)
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("error parsing image config: %w", err)
		}

		filter, err = a.filterFunc(ctx, srcRef, *imgCfg)
		if err != nil {
			return nil, time.Time{}, err
		}
	}

	modTime, err := a.applyLayersToDisk(ctx, id, canonicalRef, img, imgSrc, filter)
	if err != nil {
		return nil, time.Time{}, err
	}
	return os.DirFS(a.unpackPath(id, canonicalRef.Digest())), modTime, nil
}

func (a *diskCache) DeleteID(_ context.Context, id string) error {
	return fsutil.DeleteReadOnlyRecursive(a.idPath(id))
}

func (a *diskCache) GarbageCollect(_ context.Context, id string, keep reference.Canonical) error {
	idPath := a.idPath(id)
	dirEntries, err := os.ReadDir(idPath)
	if err != nil {
		return fmt.Errorf("error reading image directories: %w", err)
	}

	dirEntries = slices.DeleteFunc(dirEntries, func(entry os.DirEntry) bool {
		return entry.Name() == keep.Digest().String()
	})

	for _, dirEntry := range dirEntries {
		if err := fsutil.DeleteReadOnlyRecursive(filepath.Join(idPath, dirEntry.Name())); err != nil {
			return fmt.Errorf("error removing entry %s: %w", dirEntry.Name(), err)
		}
	}
	return nil
}

// applyLayersToDisk writes the layers from img and imgSrc to disk using the provided filter.
// The destination directory will be created, if necessary. If dest is already present, its
// contents will be deleted. If img and imgSrc do not represent the same image, an error will
// be returned due to a mismatch in the expected layers. Once complete, the dest and its contents
// are marked as read-only to provide a safeguard against unintended changes.
func (a *diskCache) applyLayersToDisk(ctx context.Context, id string, canonicalRef reference.Canonical, img types.Image, imgSrc types.ImageSource, filter archive.Filter) (time.Time, error) {
	var applyOpts []archive.ApplyOpt
	if filter != nil {
		applyOpts = append(applyOpts, archive.WithFilter(filter))
	}

	dest := a.unpackPath(id, canonicalRef.Digest())
	if err := fsutil.EnsureEmptyDirectory(dest, 0700); err != nil {
		return time.Time{}, fmt.Errorf("error ensuring empty unpack directory: %w", err)
	}
	l := log.FromContext(ctx)
	l.Info("unpacking image", "path", dest)
	for i, layerInfo := range img.LayerInfos() {
		if err := func() error {
			layerReader, _, err := imgSrc.GetBlob(ctx, layerInfo, none.NoCache)
			if err != nil {
				return fmt.Errorf("error getting blob for layer[%d]: %w", i, err)
			}
			defer layerReader.Close()

			decompressed, _, err := compression.AutoDecompress(layerReader)
			if err != nil {
				return fmt.Errorf("auto-decompress failed: %w", err)
			}
			defer decompressed.Close()

			if _, err := archive.Apply(ctx, dest, decompressed, applyOpts...); err != nil {
				return fmt.Errorf("error applying layer[%d]: %w", i, err)
			}
			l.Info("applied layer", "layer", i)
			return nil
		}(); err != nil {
			return time.Time{}, errors.Join(err, fsutil.DeleteReadOnlyRecursive(dest))
		}
	}
	if err := fsutil.SetReadOnlyRecursive(dest); err != nil {
		return time.Time{}, fmt.Errorf("error making unpack directory read-only: %w", err)
	}
	return fsutil.GetDirectoryModTime(dest)
}
