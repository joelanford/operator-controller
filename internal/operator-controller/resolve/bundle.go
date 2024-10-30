package resolve

import (
	"context"
	"errors"
	"path/filepath"

	bsemver "github.com/blang/semver/v4"

	"github.com/operator-framework/operator-registry/alpha/action"
	"github.com/operator-framework/operator-registry/alpha/declcfg"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/bundleutil"
	imageutil "github.com/operator-framework/operator-controller/internal/shared/util/image"
)

type BundleResolver struct {
	Puller              imageutil.Puller
	Cache               imageutil.Cache
	BrittleCacheBaseDir string
}

func (r *BundleResolver) Resolve(ctx context.Context, ext *ocv1.ClusterExtension, _ *ocv1.BundleMetadata) (*declcfg.Bundle, *bsemver.Version, *declcfg.Deprecation, error) {
	_, canonicalRef, _, err := r.Puller.Pull(ctx, ext.Name, ext.Spec.Source.Bundle.Ref, r.Cache)
	if err != nil {
		return nil, nil, nil, err
	}

	// TODO: This is a temporary workaround to get the bundle from the filesystem
	//    until the operator-registry library is updated to support reading from a
	//    filesystem. This will be removed once the library is updated.
	bundlePath := filepath.Join(r.BrittleCacheBaseDir, ext.Name, canonicalRef.Digest().String())

	render := action.Render{
		Refs:           []string{bundlePath},
		AllowedRefMask: action.RefBundleDir,
	}
	fbc, err := render.Run(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(fbc.Bundles) != 1 {
		return nil, nil, nil, errors.New("expected exactly one bundle")
	}
	bundle := fbc.Bundles[0]
	bundle.Image = canonicalRef.String()
	v, err := bundleutil.GetVersion(bundle)
	if err != nil {
		return nil, nil, nil, err
	}
	return &bundle, v, nil, nil
}
