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
	"github.com/go-logr/logr"
	"github.com/opencontainers/go-digest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	catalogdv1 "github.com/operator-framework/operator-controller/catalogd/api/v1"
	fsutil "github.com/operator-framework/operator-controller/internal/util/fs"
	imageutil "github.com/operator-framework/operator-controller/internal/util/image"
)

const ConfigDirLabel = "operators.operatorframework.io.index.configs.v1"

type ContainersImageRegistry struct {
	BaseCachePath string
	Puller        imageutil.Puller
}

type catalogApplier struct {
	baseCachePath string
	catalog       *catalogdv1.ClusterCatalog
}

func (a catalogApplier) Exists(ctx context.Context, canonicalRef reference.Canonical) (bool, time.Time, error) {
	l := logr.FromContextOrDiscard(ctx)
	unpackPath := a.unpackPath(canonicalRef.Digest())
	modTime, err := fsutil.GetDirectoryModTime(unpackPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, time.Time{}, nil
	case errors.Is(err, fsutil.ErrNotDirectory):
		l.Info("unpack path is not a directory; attempting to delete", "path", unpackPath)
		return false, time.Time{}, os.RemoveAll(unpackPath)
	case err != nil:
		return false, time.Time{}, err
	}
	l.Info("image already unpacked")
	return true, modTime, nil
}

func (a catalogApplier) catalogPath() string {
	return filepath.Join(a.baseCachePath, a.catalog.Name)
}

func (a catalogApplier) unpackPath(digest digest.Digest) string {
	return filepath.Join(a.catalogPath(), digest.String())
}

func (a catalogApplier) successResult(ref reference.Canonical, modTime time.Time) (*Result, error) {
	return &Result{
		FS: os.DirFS(a.unpackPath(ref.Digest())),
		ResolvedSource: &catalogdv1.ResolvedCatalogSource{
			Type: catalogdv1.SourceTypeImage,
			Image: &catalogdv1.ResolvedImageSource{
				Ref: ref.String(),
			},
		},
		State:   StateUnpacked,
		Message: fmt.Sprintf("unpacked %q successfully", ref),

		// We truncate both the unpack time and last successful poll attempt
		// to the second because metav1.Time is serialized
		// as RFC 3339 which only has second-level precision. When we
		// use this result in a comparison with what we deserialized
		// from the Kubernetes API server, we need it to match.
		UnpackTime:                modTime.Truncate(time.Second),
		LastSuccessfulPollAttempt: metav1.NewTime(time.Now().Truncate(time.Second)),
	}, nil
}

func (a catalogApplier) Apply(ctx context.Context, srcRef reference.Named, canonicalRef reference.Canonical, img types.Image, imgSrc types.ImageSource) (time.Time, error) {
	cfg, err := img.OCIConfig(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("error parsing image config: %w", err)
	}

	_, specIsCanonical := srcRef.(reference.Canonical)

	dirToUnpack, ok := cfg.Config.Labels[ConfigDirLabel]
	if !ok {
		// If the spec is a tagged ref, retries could end up resolving a new digest, where the label
		// might show up. If the spec is canonical, no amount of retries will make the label appear.
		// Therefore, we treat the error as terminal if the reference from the spec is canonical.
		return time.Time{}, wrapTerminal(fmt.Errorf("catalog image is missing the required label %q", ConfigDirLabel), specIsCanonical)
	}

	applyFilter := imageutil.AllFilters(
		imageutil.OnlyPath(dirToUnpack),
		imageutil.ForceOwnershipRWX(),
	)
	return imageutil.ApplyLayersToDisk(ctx, a.unpackPath(canonicalRef.Digest()), img, imgSrc, applyFilter)
}

func (i *ContainersImageRegistry) Unpack(ctx context.Context, catalog *catalogdv1.ClusterCatalog) (*Result, error) {
	if catalog.Spec.Source.Type != catalogdv1.SourceTypeImage {
		panic(fmt.Sprintf("programmer error: source type %q is unable to handle specified catalog source type %q", catalogdv1.SourceTypeImage, catalog.Spec.Source.Type))
	}

	if catalog.Spec.Source.Image == nil {
		return nil, reconcile.TerminalError(fmt.Errorf("error parsing catalog, catalog %s has a nil image source", catalog.Name))
	}

	applier := &catalogApplier{
		baseCachePath: i.BaseCachePath,
		catalog:       catalog,
	}

	canonicalRef, modTime, err := i.Puller.Pull(ctx, catalog.Spec.Source.Image.Ref, applier)
	if err != nil {
		return nil, err
	}

	//////////////////////////////////////////////////////
	//
	// Delete other images. They are no longer needed.
	//
	//////////////////////////////////////////////////////
	if err := i.deleteOtherImages(catalog, canonicalRef.Digest()); err != nil {
		return nil, fmt.Errorf("error deleting old images: %w", err)
	}

	return applier.successResult(canonicalRef, modTime)
}

func (i *ContainersImageRegistry) Cleanup(_ context.Context, catalog *catalogdv1.ClusterCatalog) error {
	applier := &catalogApplier{
		baseCachePath: i.BaseCachePath,
		catalog:       catalog,
	}
	if err := fsutil.DeleteReadOnlyRecursive(applier.catalogPath()); err != nil {
		return fmt.Errorf("error deleting catalog cache: %w", err)
	}
	return nil
}

func (i *ContainersImageRegistry) deleteOtherImages(catalog *catalogdv1.ClusterCatalog, digestToKeep digest.Digest) error {
	applier := &catalogApplier{
		baseCachePath: i.BaseCachePath,
		catalog:       catalog,
	}
	catalogPath := applier.catalogPath()
	imgDirs, err := os.ReadDir(catalogPath)
	if err != nil {
		return fmt.Errorf("error reading image directories: %w", err)
	}
	for _, imgDir := range imgDirs {
		if imgDir.Name() == digestToKeep.String() {
			continue
		}
		imgDirPath := filepath.Join(catalogPath, imgDir.Name())
		if err := fsutil.DeleteReadOnlyRecursive(imgDirPath); err != nil {
			return fmt.Errorf("error removing image directory: %w", err)
		}
	}
	return nil
}

func wrapTerminal(err error, isTerminal bool) error {
	if !isTerminal {
		return err
	}
	return reconcile.TerminalError(err)
}
