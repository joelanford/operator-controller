package image

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/docker"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/pkg/sysregistriesv2"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/types"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type Puller interface {
	Pull(context.Context, string, string, Cache) (fs.FS, reference.Canonical, time.Time, error)
}

type Cache interface {
	Fetch(context.Context, string, reference.Canonical) (fs.FS, time.Time, error)
	Store(context.Context, string, reference.Named, reference.Canonical, types.Image, types.ImageSource) (fs.FS, time.Time, error)
	DeleteID(context.Context, string) error
	GarbageCollect(context.Context, string, reference.Canonical) error
}

var insecurePolicy = []byte(`{"default":[{"type":"insecureAcceptAnything"}]}`)

type ContainersImagePuller struct {
	SourceCtxFunc func(context.Context) (*types.SystemContext, error)
}

func (p *ContainersImagePuller) Pull(ctx context.Context, id string, ref string, cache Cache) (fs.FS, reference.Canonical, time.Time, error) {
	// Reload registries cache in case of configuration update
	sysregistriesv2.InvalidateCache()

	dockerRef, err := reference.ParseNamed(ref)
	if err != nil {
		return nil, nil, time.Time{}, reconcile.TerminalError(fmt.Errorf("error parsing image reference %q: %w", ref, err))
	}
	dockerImgRef, err := docker.NewReference(dockerRef)
	if err != nil {
		return nil, nil, time.Time{}, reconcile.TerminalError(fmt.Errorf("error creating reference: %w", err))
	}

	l := log.FromContext(ctx, "ref", dockerRef.String())

	srcCtx, err := p.SourceCtxFunc(ctx)
	if err != nil {
		return nil, nil, time.Time{}, err
	}

	//////////////////////////////////////////////////////
	//
	// Resolve a canonical reference for the image.
	//
	//////////////////////////////////////////////////////
	canonicalRef, err := resolveCanonicalRef(ctx, dockerImgRef, srcCtx)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	l = l.WithValues("digest", canonicalRef.Digest().String())

	///////////////////////////////////////////////////////
	//
	// Check if the cache has already applied the
	// canonical ref. If so, we're done.
	//
	///////////////////////////////////////////////////////
	fsys, modTime, err := cache.Fetch(ctx, id, canonicalRef)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("error checking if ref has already been applied: %w", err)
	}
	if fsys != nil {
		return fsys, canonicalRef, modTime, nil
	}

	//////////////////////////////////////////////////////
	//
	// Create an OCI layout reference for the destination,
	// where we will temporarily store the image in order
	// to unpack it.
	//
	// We use the OCI layout as a temporary storage because
	// copy.Image can concurrently pull all the layers.
	//
	//////////////////////////////////////////////////////
	layoutDir, err := os.MkdirTemp("", "oci-layout-*")
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("error creating temporary directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(layoutDir); err != nil {
			l.Error(err, "error removing temporary OCI layout directory")
		}
	}()

	layoutImgRef, err := layout.NewReference(layoutDir, canonicalRef.String())
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("error creating reference: %w", err)
	}

	//////////////////////////////////////////////////////
	//
	// Load an image signature policy and build
	// a policy context for the image pull.
	//
	//////////////////////////////////////////////////////
	policyContext, err := loadPolicyContext(srcCtx, l)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("error loading policy context: %w", err)
	}
	defer func() {
		if err := policyContext.Destroy(); err != nil {
			l.Error(err, "error destroying policy context")
		}
	}()

	//////////////////////////////////////////////////////
	//
	// Pull the image from the source to the destination
	//
	//////////////////////////////////////////////////////
	if _, err := copy.Image(ctx, policyContext, layoutImgRef, dockerImgRef, &copy.Options{
		SourceCtx: srcCtx,
		// We use the OCI layout as a temporary storage and
		// pushing signatures for OCI images is not supported
		// so we remove the source signatures when copying.
		// Signature validation will still be performed
		// accordingly to a provided policy context.
		RemoveSignatures: true,
	}); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("error copying image: %w", err)
	}
	l.Info("pulled image")

	fsys, modTime, err = p.applyImage(ctx, id, dockerRef, canonicalRef, layoutImgRef, cache, srcCtx)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("error applying image: %w", err)
	}

	/////////////////////////////////////////////////////////////
	//
	// Clean up any images from the cache that we no longer need.
	//
	/////////////////////////////////////////////////////////////
	if err := cache.GarbageCollect(ctx, id, canonicalRef); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("error deleting old images: %w", err)
	}
	return fsys, canonicalRef, modTime, nil
}

func resolveCanonicalRef(ctx context.Context, imgRef types.ImageReference, srcCtx *types.SystemContext) (reference.Canonical, error) {
	if canonicalRef, ok := imgRef.DockerReference().(reference.Canonical); ok {
		return canonicalRef, nil
	}

	imgSrc, err := imgRef.NewImageSource(ctx, srcCtx)
	if err != nil {
		return nil, fmt.Errorf("error creating image source: %w", err)
	}
	defer imgSrc.Close()

	manifestBlob, _, err := imgSrc.GetManifest(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("error getting manifest: %w", err)
	}
	imgDigest, err := manifest.Digest(manifestBlob)
	if err != nil {
		return nil, fmt.Errorf("error getting digest of manifest: %w", err)
	}
	canonicalRef, err := reference.WithDigest(reference.TrimNamed(imgRef.DockerReference()), imgDigest)
	if err != nil {
		return nil, fmt.Errorf("error creating canonical reference: %w", err)
	}
	return canonicalRef, nil
}

func (p *ContainersImagePuller) applyImage(ctx context.Context, id string, srcRef reference.Named, canonicalRef reference.Canonical, srcImgRef types.ImageReference, applier Cache, sourceContext *types.SystemContext) (fs.FS, time.Time, error) {
	imgSrc, err := srcImgRef.NewImageSource(ctx, sourceContext)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("error creating image source: %w", err)
	}
	img, err := image.FromSource(ctx, sourceContext, imgSrc)
	if err != nil {
		return nil, time.Time{}, errors.Join(
			imgSrc.Close(),
			fmt.Errorf("error reading image: %w", err),
		)
	}
	defer func() {
		if err := img.Close(); err != nil {
			panic(err)
		}
	}()
	return applier.Store(ctx, id, srcRef, canonicalRef, img, imgSrc)
}

func loadPolicyContext(sourceContext *types.SystemContext, l logr.Logger) (*signature.PolicyContext, error) {
	policy, err := signature.DefaultPolicy(sourceContext)
	// TODO: there are security implications to silently moving to an insecure policy
	// tracking issue: https://github.com/operator-framework/operator-controller/issues/1622
	if err != nil {
		l.Info("no default policy found, using insecure policy")
		policy, err = signature.NewPolicyFromBytes(insecurePolicy)
	}
	if err != nil {
		return nil, fmt.Errorf("error loading signature policy: %w", err)
	}
	return signature.NewPolicyContext(policy)
}
