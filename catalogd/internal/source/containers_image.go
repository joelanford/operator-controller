package source

import (
	"context"
	"fmt"
	"io/fs"
	"time"

	"github.com/containerd/containerd/archive"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	catalogdv1 "github.com/operator-framework/operator-controller/catalogd/api/v1"
	errorutil "github.com/operator-framework/operator-controller/internal/util/error"
	imageutil "github.com/operator-framework/operator-controller/internal/util/image"
)

const ConfigDirLabel = "operators.operatorframework.io.index.configs.v1"

type ContainersImageRegistry struct {
	Cache  imageutil.Cache
	Puller imageutil.Puller
}

func CatalogCache(basePath string) imageutil.Cache {
	return &imageutil.DiskCache{
		BasePath: basePath,
		FilterFunc: func(ctx context.Context, _ string, srcRef reference.Named, _ reference.Canonical, img types.Image, _ types.ImageSource) (archive.Filter, error) {
			cfg, err := img.OCIConfig(ctx)
			if err != nil {
				return nil, fmt.Errorf("error parsing image config: %w", err)
			}

			_, specIsCanonical := srcRef.(reference.Canonical)

			dirToUnpack, ok := cfg.Config.Labels[ConfigDirLabel]
			if !ok {
				// If the spec is a tagged ref, retries could end up resolving a new digest, where the label
				// might show up. If the spec is canonical, no amount of retries will make the label appear.
				// Therefore, we treat the error as terminal if the reference from the spec is canonical.
				return nil, errorutil.WrapTerminal(fmt.Errorf("catalog image is missing the required label %q", ConfigDirLabel), specIsCanonical)
			}

			return imageutil.AllFilters(
				imageutil.OnlyPath(dirToUnpack),
				imageutil.ForceOwnershipRWX(),
			), nil
		},
	}
}

func (i *ContainersImageRegistry) Unpack(ctx context.Context, catalog *catalogdv1.ClusterCatalog) (*Result, error) {
	if catalog.Spec.Source.Type != catalogdv1.SourceTypeImage {
		panic(fmt.Sprintf("programmer error: source type %q is unable to handle specified catalog source type %q", catalogdv1.SourceTypeImage, catalog.Spec.Source.Type))
	}

	if catalog.Spec.Source.Image == nil {
		return nil, reconcile.TerminalError(fmt.Errorf("error parsing catalog, catalog %s has a nil image source", catalog.Name))
	}

	fsys, canonicalRef, modTime, err := i.Puller.Pull(ctx, catalog.Name, catalog.Spec.Source.Image.Ref, i.Cache)
	if err != nil {
		return nil, err
	}

	return successResult(fsys, canonicalRef, modTime)
}

func successResult(fsys fs.FS, ref reference.Canonical, modTime time.Time) (*Result, error) {
	return &Result{
		FS: fsys,
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

func (i *ContainersImageRegistry) Cleanup(ctx context.Context, catalog *catalogdv1.ClusterCatalog) error {
	return i.Cache.DeleteID(ctx, catalog.Name)
}
