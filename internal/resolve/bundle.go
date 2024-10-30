package resolve

import (
	"context"
	"errors"

	bsemver "github.com/blang/semver/v4"

	"github.com/operator-framework/operator-registry/alpha/action"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/pkg/image"

	ocv1alpha1 "github.com/operator-framework/operator-controller/api/v1alpha1"
	"github.com/operator-framework/operator-controller/internal/bundleutil"
)

type BundleResolver struct {
	Registry image.Registry
}

func (r *BundleResolver) Resolve(ctx context.Context, ext *ocv1alpha1.ClusterExtension, _ *ocv1alpha1.BundleMetadata) (*declcfg.Bundle, *bsemver.Version, *declcfg.Deprecation, error) {
	render := action.Render{
		Refs:     []string{ext.Spec.Source.Bundle.Ref},
		Registry: r.Registry,
	}
	fbc, err := render.Run(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(fbc.Bundles) != 1 {
		return nil, nil, nil, errors.New("expected exactly one bundle")
	}
	bundle := fbc.Bundles[0]
	v, err := bundleutil.GetVersion(bundle)
	if err != nil {
		return nil, nil, nil, err
	}
	return &bundle, v, nil, nil
}
