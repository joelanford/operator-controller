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
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fsutil "github.com/operator-framework/operator-controller/internal/util/fs"
)

type DiskCache struct {
	BasePath   string
	FilterFunc func(context.Context, string, reference.Named, reference.Canonical, types.Image, types.ImageSource) (archive.Filter, error)
}

func (a *DiskCache) Fetch(ctx context.Context, id string, canonicalRef reference.Canonical) (fs.FS, time.Time, error) {
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

func (a *DiskCache) idPath(id string) string {
	return filepath.Join(a.BasePath, id)
}

func (a *DiskCache) unpackPath(id string, digest digest.Digest) string {
	return filepath.Join(a.idPath(id), digest.String())
}

func (a *DiskCache) Store(ctx context.Context, id string, srcRef reference.Named, canonicalRef reference.Canonical, img types.Image, imgSrc types.ImageSource) (fs.FS, time.Time, error) {
	var filter archive.Filter
	if a.FilterFunc != nil {
		var err error
		filter, err = a.FilterFunc(ctx, id, srcRef, canonicalRef, img, imgSrc)
		if err != nil {
			return nil, time.Time{}, err
		}
	}

	modTime, err := ApplyLayersToDisk(ctx, a.unpackPath(id, canonicalRef.Digest()), img, imgSrc, filter)
	if err != nil {
		return nil, time.Time{}, err
	}
	return os.DirFS(a.unpackPath(id, canonicalRef.Digest())), modTime, nil
}

func (a *DiskCache) DeleteID(_ context.Context, id string) error {
	return fsutil.DeleteReadOnlyRecursive(a.idPath(id))
}

func (a *DiskCache) GarbageCollect(_ context.Context, id string, keep reference.Canonical) error {
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
