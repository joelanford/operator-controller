package fbc

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/blang/semver/v4"
	"go.podman.io/image/v5/docker/reference"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/path"
	"gonum.org/v1/gonum/graph/simple"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/operator-framework/operator-controller/internal/shared/fbc/internal/util"
)

type Graph struct {
	wg simple.WeightedDirectedGraph

	paths path.AllShortest
	heads sets.Set[*Node]
}

type Package struct {
	Name    string
	Streams []VersionStream
	Nodes   []*Node
}

type GraphConfig struct {
	Packages     []PackageV2
	AsOf         time.Time
	IncludePreGA bool
}

func nodesFromPackage(pkg PackageV2, asOf time.Time) ([]*Node, error) {
	var (
		out  = make([]*Node, 0, len(pkg.Bundles))
		errs []error
	)

	for _, b := range pkg.Bundles {
		bundleName := fmt.Sprintf("%s.v%s", pkg.Package, b.VersionRelease.String())
		imageRef, err := reference.Parse(b.Image)
		if err != nil {
			errs = append(errs, fmt.Errorf("error parsing image %q for bundle %q: %v", b.Image, bundleName, err))
			continue
		}
		canonicalRef, ok := imageRef.(reference.Canonical)
		if !ok {
			errs = append(errs, fmt.Errorf("image %q for bundle %q is not canonical, digest-based references are required", imageRef, bundleName))
			continue
		}
		mm := NewMajorMinorFromVersion(b.VersionRelease.Version)
		vs := getVersionStream(pkg.VersionStreams, mm)
		if vs == nil {
			errs = append(errs, fmt.Errorf("bundle %q missing version stream metadata: version stream %q not found", bundleName, mm))
			continue
		}
		isRetracted := false
		for _, r := range pkg.Retractions {
			if b.VersionRelease.Compare(r) == 0 {
				isRetracted = true
				break
			}
		}

		node := &Node{
			Name:                           pkg.Package,
			VersionRelease:                 b.VersionRelease,
			ReleaseDate:                    b.ReleasedAt,
			ImageReference:                 canonicalRef,
			LifecyclePhase:                 vs.LifecycleDates.Phase(asOf),
			SupportedPlatformVersions:      sets.New(vs.SupportedPlatformVersions...),
			RequiresUpdatePlatformVersions: sets.New(vs.RequiresUpdatePlatformVersions...),
			Retracted:                      isRetracted,
		}
		out = append(out, node)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func getVersionStream(versionStreams []VersionStream, mm MajorMinor) *VersionStream {
	for _, vs := range versionStreams {
		if mm.Compare(vs.Version) == 0 {
			return &vs
		}
	}
	return nil
}

func NewGraph(cfg GraphConfig) (*Graph, error) {
	wg := simple.NewWeightedDirectedGraph(0, math.Inf(1))

	var errs []error
	for _, pkg := range cfg.Packages {
		nodes, err := nodesFromPackage(pkg, cfg.AsOf)
		if err != nil {
			errs = append(errs, fmt.Errorf("error loading package %q: %v", pkg.Package, err))
			continue
		}
		for _, node := range nodes {
			wg.AddNode(node)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	g := &Graph{wg: *wg}
	if err := g.buildEdges(cfg); err != nil {
		return nil, err
	}
	g.paths = path.DijkstraAllPaths(wg)

	heads := sets.New[*Node]()
	for n := range g.NodesMatching(isHead) {
		heads.Insert(n)
	}
	g.heads = heads
	return g, nil
}

func (g *Graph) Paths() path.AllShortest {
	return g.paths
}

func (g *Graph) To(to *Node) iter.Seq[*Node] {
	return NodeIterator(g.wg.To(to.ID()))
}

func (g *Graph) From(from *Node) iter.Seq[*Node] {
	return NodeIterator(g.wg.From(from.ID()))
}

type WeightedEdge struct {
	From, To *Node
	Weight   float64
}

func (g *Graph) EdgeWeight(from, to *Node) float64 {
	w := g.wg.WeightedEdge(from.ID(), to.ID())
	if w == nil {
		return math.Inf(1)
	}
	return w.Weight()
}

func (g *Graph) FirstNodeMatching(match NodePredicate) *Node {
	for n := range NodeIterator(g.wg.Nodes()) {
		if match(g, n) {
			return n
		}
	}
	return nil
}

func (g *Graph) NodesMatching(match NodePredicate) iter.Seq[*Node] {
	it := NodeIterator(g.wg.Nodes())
	return func(yield func(*Node) bool) {
		for n := range it {
			if match(g, n) {
				if !yield(n) {
					return
				}
			}
		}
	}
}

func (g *Graph) Heads() sets.Set[*Node] {
	return g.heads
}

func isHead(g *Graph, n *Node) bool {
	for range NodeIterator(g.wg.From(n.ID())) {
		return false
	}
	return true
}

func (g *Graph) buildEdges(cfg GraphConfig) error {
	var errs []error
	for _, pkg := range cfg.Packages {
		var (
			streamsByMajorMinor = util.KeySlice(pkg.VersionStreams, func(s VersionStream) (MajorMinor, VersionStream) { return s.Version, s })
			nodesByReleaseDate  = slices.SortedFunc(
				g.NodesMatching(PackageNodes(pkg.Package)),
				func(a, b *Node) int {
					return a.ReleaseDate.Compare(b.ReleaseDate)
				},
			)
			froms = make([]*Node, 0, len(nodesByReleaseDate))
		)
		for _, to := range nodesByReleaseDate {
			toMM := NewMajorMinorFromVersion(to.VersionRelease.Version)
			stream, ok := streamsByMajorMinor[toMM]
			if !ok {
				errs = append(errs, fmt.Errorf("node with reference %s has major.minor version %s, but that version is not in an available stream", to.ImageReference.String(), toMM))
				continue
			}

			if !cfg.IncludePreGA && to.LifecyclePhase == LifecyclePhasePreGA {
				continue
			}

			g.initializeEdgesTo(froms, to, stream.MinimumUpdateVersion)
			froms = append(froms, to)
		}
		// TODO: instead of assigning static weights, perhaps pathfinding
		//   should accept and use a ranking function that defines the relative
		//   priority of a set of paths.
		g.assignEdgeWeights(pkg)
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func NodeIterator(it graph.Nodes) iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		for it.Next() {
			n := it.Node().(*Node)
			if !yield(n) {
				return
			}
		}
	}
}

func (cfg *GraphConfig) Validate() error {
	if len(cfg.Packages) == 0 {
		return errors.New("no packages specified")
	}

	var errs []error
	for _, pkg := range cfg.Packages {
		var pkgErrs []error

		// Validate version streams
		var invalidOrderErrs []error
		if len(pkg.VersionStreams) == 0 {
			pkgErrs = append(pkgErrs, fmt.Errorf("no version streams"))
		}
		for _, stream := range pkg.VersionStreams {
			if err := stream.LifecycleDates.ValidateOrder(); err != nil {
				invalidOrderErrs = append(invalidOrderErrs, err)
			}
		}
		if len(invalidOrderErrs) > 0 {
			pkgErrs = append(pkgErrs, fmt.Errorf("invalid version streams: %v", invalidOrderErrs))
		}

		// Validate Bundles
		missingVersionStreams := sets.New[MajorMinor]()
		if len(pkg.Bundles) == 0 {
			pkgErrs = append(pkgErrs, fmt.Errorf("no bundles", pkg))
		}
		for _, b := range pkg.Bundles {
			mm := NewMajorMinorFromVersion(b.VersionRelease.Version)
			vs := getVersionStream(pkg.VersionStreams, mm)
			if vs == nil {
				missingVersionStreams.Insert(mm)
			}
		}
		if len(missingVersionStreams) > 0 {
			pkgErrs = append(pkgErrs, fmt.Errorf("missing version streams: %v", slices.SortedFunc(maps.Keys(missingVersionStreams), util.Compare)))
		}
		if len(pkgErrs) > 0 {
			errs = append(errs, fmt.Errorf("invalid package %q: %v", pkg, errors.Join(pkgErrs...)))
		}
	}
	if cfg.AsOf.IsZero() {
		errs = append(errs, fmt.Errorf("no as-of timestamp specified"))
	}
	return errors.Join(errs...)
}

func (g *Graph) initializeEdgesTo(froms []*Node, to *Node, minimumUpdateVersion semver.Version) {
	for _, from := range froms {
		// Don't update to a lower version
		if from.VersionRelease.Compare(to.VersionRelease) > 0 {
			continue
		}

		// Don't update from a version below the minimum update version
		if from.VersionRelease.Version.LT(minimumUpdateVersion) {
			continue
		}

		// Don't update to a different major version
		if from.VersionRelease.Version.Major != to.VersionRelease.Version.Major {
			continue
		}

		// We don't know from's full set of successors yet. For now, set weight to 1.
		// Once all edges have been set, we can make a second pass to set better weights.
		edge := simple.WeightedEdge{F: from, T: to, W: 1}
		g.wg.SetWeightedEdge(edge)
	}
}

// assignEdgeWeights assigns edge weights to prioritize updating through supported nodes and to higher versions
// (in that order). It assigns a rank to each node (higher nodes have better support phase and higher versions), and
// then assigns all incoming edge weights as that node's rank.
//
// In order to guarantee that all paths with worse support are worse than all paths with better support,
// assignEdgeWeights create gaps between ranks when support tiers are crossed. For example, if there are 3 nodes with
// "full" support with ranks 1, 2, and 3, then traversing upgrades 3 -> 2 -> 1 would have a total sum of 6. Therefore,
// the best "maintenance" support node needs rank 7 to ensure that all paths through a single "maintenance" support
// node are worse than the worst path through all "full" supports nodes.
func (g *Graph) assignEdgeWeights(pkg PackageV2) {
	bestNodes := slices.SortedFunc(g.NodesMatching(PackageNodes(pkg.Package)), func(a *Node, b *Node) int {
		if v := b.LifecyclePhase.Compare(a.LifecyclePhase); v != 0 {
			return v
		}
		return b.Compare(a)
	})

	const delta = 0.01
	var (
		rank                   = float64(0)
		nextLifecyclePhaseRank = float64(0)
		curLifecyclePhase      = LifeCyclePhaseUnknown
	)
	for _, to := range bestNodes {
		if curLifecyclePhase != to.LifecyclePhase {
			curLifecyclePhase = to.LifecyclePhase

			rank = nextLifecyclePhaseRank
			nextLifecyclePhaseRank = 0
		}
		rank += delta
		nextLifecyclePhaseRank += rank
		for from := range NodeIterator(g.wg.To(to.ID())) {
			g.wg.RemoveEdge(from.ID(), to.ID())
			g.wg.SetWeightedEdge(simple.WeightedEdge{F: from, T: to, W: rank})
		}
	}
}

func (g *Graph) PreferredNodes(packageName string, from *Node, platform MajorMinor, extraPredicates ...NodePredicate) []*Node {
	availablePredicates := preferredVersionPredicates(packageName, from, platform)
	availablePredicates = append(availablePredicates, extraPredicates...)

	if from != nil && (from.SupportedPlatformVersions.Has(platform) || from.RequiresUpdatePlatformVersions.Has(platform)) {
		availablePredicates = append(availablePredicates, func(_ *Graph, suc *Node) bool {
			return suc.SupportedPlatformVersions.Has(platform)
		})
	}

	available := slices.Collect(g.NodesMatching(AndNodes(availablePredicates...)))

	// If any available node is supported, remove nodes that are not supported
	anySupportPlatform := false
	for _, node := range available {
		if node.SupportedPlatformVersions.Has(platform) {
			anySupportPlatform = true
			break
		}
	}
	if anySupportPlatform {
		available = slices.DeleteFunc(available, func(node *Node) bool {
			return !node.SupportedPlatformVersions.Has(platform)
		})
	}

	return available
}

func (g *Graph) AllNodes(packageName string, from *Node, extraPredicates ...NodePredicate) []*Node {
	availablePredicates := allVersionsPredicates(packageName, from)
	availablePredicates = append(availablePredicates, extraPredicates...)

	return slices.Collect(g.NodesMatching(AndNodes(availablePredicates...)))
}

func allVersionsPredicates(packageName string, from *Node) []NodePredicate {
	predicates := []NodePredicate{PackageNodes(packageName)}
	if from != nil {
		predicates = append(predicates, OrNodes(
			IsNode(from),
			SuccessorOf(from),
		))
	}
	return predicates
}

func preferredVersionPredicates(packageName string, from *Node, platform MajorMinor) []NodePredicate {
	predicates := allVersionsPredicates(packageName, from)
	predicates = append(predicates,
		func(_ *Graph, n *Node) bool { return n.LifecyclePhase != LifecyclePhasePreGA }, // Never suggest pre-GA versions
		func(_ *Graph, n *Node) bool { return !n.Retracted },                            // Never suggest retracted versions
		func(_ *Graph, to *Node) bool {
			return from == nil || to.LifecyclePhase.Compare(from.LifecyclePhase) >= 0
		}, // LifecyclePhase is at least as good as from node
		func(_ *Graph, to *Node) bool {
			switch {
			case from == nil:
				return true
			case from.RequiresUpdatePlatformVersions.Has(platform):
				return to.SupportedPlatformVersions.Has(platform) || to.RequiresUpdatePlatformVersions.Has(platform)
			case from.SupportedPlatformVersions.Has(platform):
				return to.SupportedPlatformVersions.Has(platform)
			default:
				return true
			}
		}, // Platform compatibility is as least as good as from node
	)
	return predicates
}
