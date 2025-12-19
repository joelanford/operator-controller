package imagev2

import (
	"context"
	"fmt"
	"io"

	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/pkg/blobinfocache/none"
	"go.podman.io/image/v5/types"
	"oras.land/oras-go/v2/content"
)

var _ Repository = (*ContainersImageClient)(nil)

type ContainersImageClient struct {
	ref         reference.Named
	imageSource types.ImageSource
}

func NewContainersImageClient(ctx context.Context, ref reference.Named, srcCtx *types.SystemContext) (*ContainersImageClient, error) {
	dockerRef, err := docker.NewReference(ref)
	if err != nil {
		return nil, err
	}
	imgSrc, err := dockerRef.NewImageSource(ctx, srcCtx)
	if err != nil {
		return nil, err
	}
	return &ContainersImageClient{
		ref:         ref,
		imageSource: imgSrc,
	}, nil
}

func (c *ContainersImageClient) Named() reference.Named {
	return c.ref
}

func (c *ContainersImageClient) Resolve(ctx context.Context) (ocispecv1.Descriptor, error) {
	manifestBytes, mediaType, err := c.imageSource.GetManifest(ctx, nil)
	if err != nil {
		return ocispecv1.Descriptor{}, err
	}

	imgDigest, err := manifest.Digest(manifestBytes)
	if err != nil {
		return ocispecv1.Descriptor{}, err
	}

	return ocispecv1.Descriptor{
		MediaType: mediaType,
		Digest:    imgDigest,
		Size:      int64(len(manifestBytes)),
	}, nil
}

func (c *ContainersImageClient) Close() error {
	return c.imageSource.Close()
}

func (c *ContainersImageClient) FetchManifest(ctx context.Context, desc ocispecv1.Descriptor) ([]byte, string, error) {
	return c.imageSource.GetManifest(ctx, &desc.Digest)
}

func (c *ContainersImageClient) FetchBlob(ctx context.Context, desc ocispecv1.Descriptor) (io.ReadCloser, error) {
	blobInfo := types.BlobInfo{Digest: desc.Digest, Size: desc.Size}
	reader, _, err := c.imageSource.GetBlob(ctx, blobInfo, none.NoCache)
	if err != nil {
		return nil, err
	}

	return &blob{
		Reader: content.NewVerifyReader(reader, desc),
		Closer: reader,
	}, nil
}

func (c *ContainersImageClient) newImageSource(ctx context.Context, ref reference.Named, srcCtx *types.SystemContext) (types.ImageSource, error) {
	dockerRef, err := docker.NewReference(ref)
	if err != nil {
		return nil, err
	}
	is, err := dockerRef.NewImageSource(ctx, srcCtx)
	if err != nil {
		return nil, fmt.Errorf("error creating image source: %v", err)
	}
	return is, nil
}

type blob struct {
	io.Reader
	io.Closer
}

func (b *blob) Read(p []byte) (n int, err error) {
	return b.Reader.Read(p)
}
func (b *blob) Close() error {
	return b.Closer.Close()
}
