package resolve

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	mmsemver "github.com/Masterminds/semver/v3"
	bsemver "github.com/blang/semver/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/bundleutil"
	"github.com/operator-framework/operator-controller/internal/operator-controller/catalogmetadata/compare"
	"github.com/operator-framework/operator-controller/internal/operator-controller/catalogmetadata/filter"
	"github.com/operator-framework/operator-controller/internal/shared/fbc"
	filterutil "github.com/operator-framework/operator-controller/internal/shared/util/filter"
)

type ValidationFunc func(*declcfg.Bundle) error

type CatalogResolver struct {
	WalkCatalogsFunc func(context.Context, string, CatalogWalkFunc, ...client.ListOption) error
	Validations      []ValidationFunc
}

type foundBundle struct {
	bundle   *declcfg.Bundle
	catalog  string
	priority int32
}

// Resolve returns a Bundle from a catalog that needs to get installed on the cluster.
func (r *CatalogResolver) Resolve(ctx context.Context, ext *ocv1.ClusterExtension, installedBundle *ocv1.BundleMetadata) (*declcfg.Bundle, *bsemver.Version, *declcfg.Deprecation, error) {
	l := log.FromContext(ctx)
	packageName := ext.Spec.Source.Catalog.PackageName
	versionRange := ext.Spec.Source.Catalog.Version
	channels := ext.Spec.Source.Catalog.Channels

	// unless overridden, default to selecting all bundles
	var selector = labels.Everything()
	var err error
	if ext.Spec.Source.Catalog != nil {
		selector, err = metav1.LabelSelectorAsSelector(ext.Spec.Source.Catalog.Selector)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("desired catalog selector is invalid: %w", err)
		}
		// A nothing (empty) selector selects everything
		if selector == labels.Nothing() {
			selector = labels.Everything()
		}
	}

	var versionRangeConstraints *mmsemver.Constraints
	if versionRange != "" {
		versionRangeConstraints, err = mmsemver.NewConstraint(versionRange)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("desired version range %q is invalid: %w", versionRange, err)
		}
	}

	type catStat struct {
		CatalogName    string `json:"catalogName"`
		PackageFound   bool   `json:"packageFound"`
		TotalBundles   int    `json:"totalBundles"`
		MatchedBundles int    `json:"matchedBundles"`
	}

	var catStats []*catStat

	var resolvedBundles []foundBundle
	var priorDeprecation *declcfg.Deprecation

	listOptions := []client.ListOption{
		client.MatchingLabelsSelector{Selector: selector},
	}
	if err := r.WalkCatalogsFunc(ctx, packageName, func(ctx context.Context, cat *ocv1.ClusterCatalog, packageFBC []declcfg.Meta, err error) error {
		if err != nil {
			return fmt.Errorf("error getting package %q from catalog %q: %w", packageName, cat.Name, err)
		}

		cs := catStat{CatalogName: cat.Name}
		catStats = append(catStats, &cs)

		if len(packageFBC) == 0 {
			return nil
		}

		packageV2Schema := "olm.package.v2"
		fbcPackagesV2, err := parseMetas[fbc.PackageV2](packageFBC, fbc.SchemaPackageV2)
		if err != nil {
			return fmt.Errorf("error parsing %s data %q from catalog %q: %w", packageV2Schema, packageName, cat.Name, err)
		}
		var (
			thisBundle      declcfg.Bundle
			thisDeprecation *declcfg.Deprecation
		)
		if len(fbcPackagesV2) > 0 {
			cs.PackageFound = true
			cs.TotalBundles = len(fbcPackagesV2[0].Bundles)

			g, err := fbc.NewGraph(fbc.GraphConfig{
				Packages:     fbcPackagesV2,
				AsOf:         time.Now(),
				IncludePreGA: false,
			})
			if err != nil {
				return fmt.Errorf("error building graph for package %q from catalog %q: %w", packageName, cat.Name, err)
			}

			predicates := []fbc.NodePredicate{
				fbc.PackageNodes(packageName),
				fbc.NodeInRange(asRange(versionRangeConstraints)),
			}
			if ext.Spec.Source.Catalog.UpgradeConstraintPolicy != ocv1.UpgradeConstraintPolicySelfCertified && installedBundle != nil {
				vr := fbc.VersionRelease{
					Version: bsemver.MustParse(installedBundle.Version),
					Release: nil,
				}
				from := g.FirstNodeMatching(fbc.ExactVersionRelease(vr))
				predicates = append(predicates, fbc.OrNodes(
					fbc.SuccessorOf(from),
					fbc.IsNode(from),
				))
			}

			// Apply the predicates to get the candidate bundles
			matchingNodes := slices.Collect(g.NodesMatching(fbc.AndNodes(predicates...)))
			cs.MatchedBundles = len(matchingNodes)
			if len(matchingNodes) == 0 {
				return nil
			}

			// Sort the bundles by deprecation and then by version
			slices.SortStableFunc(matchingNodes, func(a, b *fbc.Node) int {
				return b.Compare(a)
			})

			thisNode := matchingNodes[0]
			thisBundle = declcfg.Bundle{
				Schema:  declcfg.SchemaBundle,
				Package: packageName,
				Name:    thisNode.NVR(),
				Image:   thisNode.ImageReference.String(),
				Properties: []property.Property{
					property.MustBuildPackage(thisNode.Name, thisNode.VersionRelease.Version.String()),
				},
			}

			if thisNode.Retracted {
				thisDeprecation = &declcfg.Deprecation{
					Schema:  declcfg.SchemaDeprecation,
					Package: packageName,
					Entries: []declcfg.DeprecationEntry{
						{
							Reference: declcfg.PackageScopedReference{
								Schema: declcfg.SchemaBundle,
								Name:   thisNode.NVR(),
							},
							Message: fmt.Sprintf("Bundle %s has been retracted. Immediate upgrade is recommended.", thisNode.NVR()),
						},
					},
				}
			}
		} else {
			fbcChannels, err := parseMetas[declcfg.Channel](packageFBC, declcfg.SchemaChannel)
			if err != nil {
				return fmt.Errorf("error parsing %s data in package %q from catalog %q: %w", declcfg.SchemaChannel, packageName, cat.Name, err)
			}
			fbcBundles, err := parseMetas[declcfg.Bundle](packageFBC, declcfg.SchemaBundle)
			if err != nil {
				return fmt.Errorf("error parsing %s data in package %q from catalog %q: %w", declcfg.SchemaBundle, packageName, cat.Name, err)
			}
			fbcDeprecations, err := parseMetas[declcfg.Deprecation](packageFBC, declcfg.SchemaDeprecation)
			if err != nil {
				return fmt.Errorf("error parsing %s data in package %q from catalog %q: %w", declcfg.SchemaDeprecation, packageName, cat.Name, err)
			}

			cs.PackageFound = true
			cs.TotalBundles = len(fbcBundles)

			var predicates []filterutil.Predicate[declcfg.Bundle]
			if len(channels) > 0 {
				channelSet := sets.New(channels...)
				filteredChannels := slices.DeleteFunc(fbcChannels, func(c declcfg.Channel) bool {
					return !channelSet.Has(c.Name)
				})
				predicates = append(predicates, filter.InAnyChannel(filteredChannels...))
			}

			if versionRangeConstraints != nil {
				predicates = append(predicates, filter.InMastermindsSemverRange(versionRangeConstraints))
			}

			if ext.Spec.Source.Catalog.UpgradeConstraintPolicy != ocv1.UpgradeConstraintPolicySelfCertified && installedBundle != nil {
				successorPredicate, err := filter.SuccessorsOf(*installedBundle, fbcChannels...)
				if err != nil {
					return fmt.Errorf("error finding upgrade edges: %w", err)
				}
				predicates = append(predicates, successorPredicate)
			}

			// Apply the predicates to get the candidate bundles
			fbcBundles = filterutil.InPlace(fbcBundles, filterutil.And(predicates...))
			cs.MatchedBundles = len(fbcBundles)
			if len(fbcBundles) == 0 {
				return nil
			}

			// If this package has a deprecation, we:
			//   1. Want to sort deprecated bundles to the end of the list
			//   2. Want to keep track of it so that we can return it if we end
			//      up resolving a bundle from this package.
			byDeprecation := func(a, b declcfg.Bundle) int { return 0 }
			if len(fbcDeprecations) > 0 {
				thisDeprecation = &fbcDeprecations[0]
				byDeprecation = compare.ByDeprecationFunc(*thisDeprecation)
			}

			// Sort the bundles by deprecation and then by version
			slices.SortStableFunc(fbcBundles, func(a, b declcfg.Bundle) int {
				if lessDep := byDeprecation(a, b); lessDep != 0 {
					return lessDep
				}
				return compare.ByVersion(a, b)
			})

			thisBundle = fbcBundles[0]
		}

		if len(resolvedBundles) != 0 {
			// We've already found one or more package candidates
			currentIsDeprecated := isDeprecated(thisBundle, thisDeprecation)
			priorIsDeprecated := isDeprecated(*resolvedBundles[len(resolvedBundles)-1].bundle, priorDeprecation)
			if currentIsDeprecated && !priorIsDeprecated {
				// Skip this deprecated package and retain the non-deprecated package(s)
				return nil
			} else if !currentIsDeprecated && priorIsDeprecated {
				// Our package candidates so far were deprecated and this one is not; clear the lists
				resolvedBundles = []foundBundle{}
			}
		}
		// The current bundle shares deprecation status with prior bundles or
		// there are no prior bundles. Add it to the list.
		resolvedBundles = append(resolvedBundles, foundBundle{&thisBundle, cat.GetName(), cat.Spec.Priority})
		priorDeprecation = thisDeprecation
		return nil
	}, listOptions...); err != nil {
		return nil, nil, nil, fmt.Errorf("error walking catalogs: %w", err)
	}

	// Resolve for priority
	if len(resolvedBundles) > 1 {
		// Want highest first (reverse sort)
		sort.Slice(resolvedBundles, func(i, j int) bool { return resolvedBundles[i].priority > resolvedBundles[j].priority })
		// If the top two bundles do not have the same priority, then priority breaks the tie
		// Reduce resolvedBundles to just the first item (highest priority)
		if resolvedBundles[0].priority != resolvedBundles[1].priority {
			resolvedBundles = []foundBundle{resolvedBundles[0]}
		}
	}

	// Check for ambiguity
	if len(resolvedBundles) != 1 {
		l.Info("resolution failed", "stats", catStats)
		return nil, nil, nil, resolutionError{
			PackageName:     packageName,
			Version:         versionRange,
			Channels:        channels,
			InstalledBundle: installedBundle,
			ResolvedBundles: resolvedBundles,
		}
	}
	resolvedBundle := resolvedBundles[0].bundle
	resolvedBundleVersion, err := bundleutil.GetVersion(*resolvedBundle)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("error getting resolved bundle version for bundle %q: %w", resolvedBundle.Name, err)
	}

	// Run validations against the resolved bundle to ensure only valid resolved bundles are being returned
	// Open Question: Should we grab the first valid bundle earlier?
	//        Answer: No, that would be a hidden resolution input, which we should avoid at all costs; the query can be
	//                constrained in order to eliminate the invalid bundle from the resolution.
	for _, validation := range r.Validations {
		if err := validation(resolvedBundle); err != nil {
			return nil, nil, nil, fmt.Errorf("validating bundle %q: %w", resolvedBundle.Name, err)
		}
	}

	l.V(4).Info("resolution succeeded", "stats", catStats)
	return resolvedBundle, resolvedBundleVersion, priorDeprecation, nil
}

type resolutionError struct {
	PackageName     string
	Version         string
	Channels        []string
	InstalledBundle *ocv1.BundleMetadata
	ResolvedBundles []foundBundle
}

func (rei resolutionError) Error() string {
	var sb strings.Builder
	if rei.InstalledBundle != nil {
		sb.WriteString(fmt.Sprintf("error upgrading from currently installed version %q: ", rei.InstalledBundle.Version))
	}

	if len(rei.ResolvedBundles) > 1 {
		sb.WriteString(fmt.Sprintf("found bundles for package %q ", rei.PackageName))
	} else {
		sb.WriteString(fmt.Sprintf("no bundles found for package %q ", rei.PackageName))
	}

	if rei.Version != "" {
		sb.WriteString(fmt.Sprintf("matching version %q ", rei.Version))
	}

	if len(rei.Channels) > 0 {
		sb.WriteString(fmt.Sprintf("in channels %v ", rei.Channels))
	}

	matchedCatalogs := []string{}
	for _, r := range rei.ResolvedBundles {
		matchedCatalogs = append(matchedCatalogs, r.catalog)
	}
	slices.Sort(matchedCatalogs) // sort for consistent error message
	if len(matchedCatalogs) > 1 {
		sb.WriteString(fmt.Sprintf("in multiple catalogs with the same priority %v ", matchedCatalogs))
	}

	return strings.TrimSpace(sb.String())
}

func isDeprecated(bundle declcfg.Bundle, deprecation *declcfg.Deprecation) bool {
	if deprecation == nil {
		return false
	}
	for _, entry := range deprecation.Entries {
		if entry.Reference.Schema == declcfg.SchemaBundle && entry.Reference.Name == bundle.Name {
			return true
		}
	}
	return false
}

type CatalogWalkFunc func(context.Context, *ocv1.ClusterCatalog, []declcfg.Meta, error) error

func CatalogWalker(
	listCatalogs func(context.Context, ...client.ListOption) ([]ocv1.ClusterCatalog, error),
	getPackage func(context.Context, *ocv1.ClusterCatalog, string) ([]declcfg.Meta, error),
) func(ctx context.Context, packageName string, f CatalogWalkFunc, catalogListOpts ...client.ListOption) error {
	return func(ctx context.Context, packageName string, f CatalogWalkFunc, catalogListOpts ...client.ListOption) error {
		l := log.FromContext(ctx)
		catalogs, err := listCatalogs(ctx, catalogListOpts...)
		if err != nil {
			return fmt.Errorf("error listing catalogs: %w", err)
		}

		// Remove disabled catalogs from consideration
		catalogs = slices.DeleteFunc(catalogs, func(c ocv1.ClusterCatalog) bool {
			if c.Spec.AvailabilityMode == ocv1.AvailabilityModeUnavailable {
				l.Info("excluding ClusterCatalog from resolution process since it is disabled", "catalog", c.Name)
				return true
			}
			return false
		})

		availableCatalogNames := mapSlice(catalogs, func(c ocv1.ClusterCatalog) string { return c.Name })
		l.Info("using ClusterCatalogs for resolution", "catalogs", availableCatalogNames)

		for i := range catalogs {
			cat := &catalogs[i]

			// process enabled catalogs
			fbc, fbcErr := getPackage(ctx, cat, packageName)

			if walkErr := f(ctx, cat, fbc, fbcErr); walkErr != nil {
				return walkErr
			}
		}

		return nil
	}
}

func mapSlice[I any, O any](in []I, f func(I) O) []O {
	out := make([]O, len(in))
	for i := range in {
		out[i] = f(in[i])
	}
	return out
}

func parseMetas[T any](metas []declcfg.Meta, schema string) ([]T, error) {
	out := make([]T, 0, len(metas))
	for _, meta := range metas {
		if meta.Schema == schema {
			var t T
			if err := json.Unmarshal(meta.Blob, &t); err != nil {
				return nil, err
			}
			out = append(out, t)
		}
	}
	return out, nil
}

func asRange(constraints *mmsemver.Constraints) bsemver.Range {
	if constraints == nil {
		return func(bsemver.Version) bool { return true }
	}
	return func(b bsemver.Version) bool {
		pre := mapSlice(b.Pre, func(pr bsemver.PRVersion) string { return pr.String() })
		mmv := mmsemver.New(b.Major, b.Minor, b.Patch, strings.Join(pre, "."), strings.Join(b.Build, "."))
		return constraints.Check(mmv)
	}
}
