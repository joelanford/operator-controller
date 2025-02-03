package source

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	fsutil "github.com/operator-framework/operator-controller/internal/util/fs"
	imageutil "github.com/operator-framework/operator-controller/internal/util/image"
)

type ContainersImageRegistry struct {
	BaseCachePath string
	Puller        imageutil.Puller
}

type bundleApplier struct {
	baseCachePath string
	bundle        *BundleSource
}

func (a bundleApplier) Exists(ctx context.Context, canonicalRef reference.Canonical) (bool, time.Time, error) {
	l := log.FromContext(ctx)
	unpackPath := a.unpackPath(canonicalRef.Digest())
	modTime, err := fsutil.GetDirectoryModTime(unpackPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, time.Time{}, nil
	case errors.Is(err, fsutil.ErrNotDirectory):
		l.Info("unpack path is not a directory; attempting to delete", "path", unpackPath)
		return false, time.Time{}, os.RemoveAll(unpackPath)
	case err != nil:
		return false, time.Time{}, fmt.Errorf("error checking bundle already unpacked: %w", err)
	}
	l.Info("image already unpacked")
	return true, modTime, nil
}

func (a bundleApplier) bundlePath() string {
	return filepath.Join(a.baseCachePath, a.bundle.Name)
}

func (a bundleApplier) unpackPath(digest digest.Digest) string {
	return filepath.Join(a.bundlePath(), digest.String())
}

func (a bundleApplier) successResult(ref reference.Canonical) *Result {
	return &Result{
		Bundle:         os.DirFS(a.unpackPath(ref.Digest())),
		ResolvedSource: &BundleSource{Type: SourceTypeImage, Name: a.bundle.Name, Image: &ImageSource{Ref: ref.String()}},
		State:          StateUnpacked,
		Message:        fmt.Sprintf("unpacked %q successfully", ref),
	}
}

func (a bundleApplier) Apply(ctx context.Context, _ reference.Named, canonicalRef reference.Canonical, img types.Image, imgSrc types.ImageSource) (time.Time, error) {
	return imageutil.ApplyLayersToDisk(ctx, a.unpackPath(canonicalRef.Digest()), img, imgSrc, imageutil.ForceOwnershipRWX())
}

func (i *ContainersImageRegistry) Unpack(ctx context.Context, bundle *BundleSource) (*Result, error) {
	if bundle.Type != SourceTypeImage {
		panic(fmt.Sprintf("programmer error: source type %q is unable to handle specified bundle source type %q", SourceTypeImage, bundle.Type))
	}

	if bundle.Image == nil {
		return nil, reconcile.TerminalError(fmt.Errorf("error parsing bundle, bundle %s has a nil image source", bundle.Name))
	}

	applier := &bundleApplier{
		baseCachePath: i.BaseCachePath,
		bundle:        bundle,
	}

	canonicalRef, _, err := i.Puller.Pull(ctx, bundle.Image.Ref, applier)
	if err != nil {
		return nil, err
	}

	//////////////////////////////////////////////////////
	//
	// Delete other images. They are no longer needed.
	//
	//////////////////////////////////////////////////////
	if err := i.deleteOtherImages(bundle, canonicalRef.Digest()); err != nil {
		return nil, fmt.Errorf("error deleting old images: %w", err)
	}

	return applier.successResult(canonicalRef), nil
}

func (i *ContainersImageRegistry) Cleanup(_ context.Context, bundle *BundleSource) error {
	applier := &bundleApplier{
		baseCachePath: i.BaseCachePath,
		bundle:        bundle,
	}
	return fsutil.DeleteReadOnlyRecursive(applier.bundlePath())
}

func (i *ContainersImageRegistry) deleteOtherImages(bundle *BundleSource, digestToKeep digest.Digest) error {
	applier := &bundleApplier{
		baseCachePath: i.BaseCachePath,
		bundle:        bundle,
	}
	bundlePath := applier.bundlePath()
	imgDirs, err := os.ReadDir(bundlePath)
	if err != nil {
		return fmt.Errorf("error reading image directories: %w", err)
	}
	for _, imgDir := range imgDirs {
		if imgDir.Name() == digestToKeep.String() {
			continue
		}
		imgDirPath := filepath.Join(bundlePath, imgDir.Name())
		if err := fsutil.DeleteReadOnlyRecursive(imgDirPath); err != nil {
			return fmt.Errorf("error removing image directory: %w", err)
		}
	}
	return nil
}
