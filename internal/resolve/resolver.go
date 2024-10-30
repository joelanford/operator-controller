package resolve

import (
	"context"
	"fmt"

	bsemver "github.com/blang/semver/v4"

	"github.com/operator-framework/operator-registry/alpha/declcfg"

	ocv1alpha1 "github.com/operator-framework/operator-controller/api/v1alpha1"
)

type Resolver interface {
	Resolve(ctx context.Context, ext *ocv1alpha1.ClusterExtension, installedBundle *ocv1alpha1.BundleMetadata) (*declcfg.Bundle, *bsemver.Version, *declcfg.Deprecation, error)
}

type Func func(ctx context.Context, ext *ocv1alpha1.ClusterExtension, installedBundle *ocv1alpha1.BundleMetadata) (*declcfg.Bundle, *bsemver.Version, *declcfg.Deprecation, error)

func (f Func) Resolve(ctx context.Context, ext *ocv1alpha1.ClusterExtension, installedBundle *ocv1alpha1.BundleMetadata) (*declcfg.Bundle, *bsemver.Version, *declcfg.Deprecation, error) {
	return f(ctx, ext, installedBundle)
}

type MultiResolver map[string]Resolver

func (m MultiResolver) RegisterType(sourceType string, r Resolver) {
	m[sourceType] = r
}

func (m MultiResolver) Resolve(ctx context.Context, ext *ocv1alpha1.ClusterExtension, installedBundle *ocv1alpha1.BundleMetadata) (*declcfg.Bundle, *bsemver.Version, *declcfg.Deprecation, error) {
	t := ext.Spec.Source.SourceType
	r, ok := m[t]
	if !ok {
		return nil, nil, nil, fmt.Errorf("no resolver for source type %q", t)
	}
	return r.Resolve(ctx, ext, installedBundle)
}
