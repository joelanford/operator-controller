package source

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/archive"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/types"

	imageutil "github.com/operator-framework/operator-controller/internal/util/image"
)

type ContainersImageRegistry struct {
	Cache  imageutil.Cache
	Puller imageutil.Puller
}

func BundleCache(basePath string) imageutil.Cache {
	return &imageutil.DiskCache{
		BasePath: basePath,
		FilterFunc: func(_ context.Context, _ string, _ reference.Named, _ reference.Canonical, _ types.Image, _ types.ImageSource) (archive.Filter, error) {
			return imageutil.ForceOwnershipRWX(), nil
		},
	}
}

func (i *ContainersImageRegistry) Unpack(ctx context.Context, id string, ref string) (*Result, error) {
	fsys, canonicalRef, _, err := i.Puller.Pull(ctx, id, ref, i.Cache)
	if err != nil {
		return nil, err
	}

	return &Result{
		Bundle:         fsys,
		ResolvedSource: &BundleSource{Type: SourceTypeImage, Name: id, Image: &ImageSource{Ref: canonicalRef.String()}},
		State:          StateUnpacked,
		Message:        fmt.Sprintf("unpacked %q successfully", canonicalRef),
	}, nil
}

func (i *ContainersImageRegistry) Cleanup(ctx context.Context, id string) error {
	return i.Cache.DeleteID(ctx, id)
}
