package storage

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"gonum.org/v1/gonum/graph"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	"github.com/operator-framework/operator-registry/alpha/declcfg"

	"github.com/operator-framework/operator-controller/internal/shared/fbc"
)

// ExtensionStatusResponse represents the status of a ClusterExtension
type ExtensionStatusResponse struct {
	Package                 string         `json:"package"`
	CurrentPlatformVersion  fbc.MajorMinor `json:"currentPlatformVersion"`
	CurrentExtensionVersion *VersionInfo   `json:"currentExtensionVersion,omitempty"`
	AvailableVersions       []VersionInfo  `json:"availableVersions,omitempty"`
}

// VersionInfo contains metadata about a specific version
type VersionInfo struct {
	Version                        string         `json:"version"`
	LifecyclePhase                 string         `json:"lifecyclePhase"`
	LifecyclePhaseEnds             string         `json:"lifecyclePhaseEnds"`
	Retracted                      bool           `json:"retracted"`
	ReleaseDate                    string         `json:"releaseDate,omitempty"`
	Notifications                  []Notification `json:"notifications,omitempty"`
	SupportedPlatformVersions      []string       `json:"supportedPlatformVersions,omitempty"`
	RequiresUpdatePlatformVersions []string       `json:"requiresUpdatePlatformVersions,omitempty"`
}

// Notification represents information about the current version that a user needs to know.
type Notification struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// ClusterUpdatePlanResponse represents the update plan for upgrading a cluster
type ClusterUpdatePlanResponse struct {
	Package              string         `json:"package"`
	FromExtension        VersionInfo    `json:"fromExtension"`
	FromPlatformVersion  fbc.MajorMinor `json:"fromPlatformVersion"`
	ToPlatformVersion    fbc.MajorMinor `json:"toPlatformVersion"`
	BeforePlatformUpdate []VersionInfo  `json:"beforePlatformUpdate"`
	AfterPlatformUpdate  []VersionInfo  `json:"afterPlatformUpdate"`
	Error                string         `json:"error,omitempty"`
}

const (
	WarningTypeRetracted      = "Retracted"
	WarningTypeEOL            = "EndOfLife"
	WarningTypeNotSupported   = "NotSupported"
	WarningTypeRequiresUpdate = "RequiresUpdate"

	SeverityError   = "error"
	SeverityWarning = "warning"
)

func (s *LocalDirV1) handleInfo(w http.ResponseWriter, r *http.Request) {
	s.m.RLock()
	defer s.m.RUnlock()

	// Discover cluster version
	platform, err := s.ClusterVersionGetter.GetVersion(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to detect server version: %v", err), http.StatusInternalServerError)
		return
	}

	catalogName := r.PathValue("catalog")
	packageName := r.PathValue("packageName")
	fromVersionReleaseStr := r.PathValue("fromVersionRelease")
	fromVersionStr, fromReleaseStr, _ := strings.Cut(fromVersionReleaseStr, "@")

	var fromVersionRelease *fbc.VersionRelease
	if fromVersionStr != "" {
		fromVersionRelease, err = fbc.NewVersionRelease(fromVersionStr, fromReleaseStr)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid version release string %q: %v", fromVersionStr, err), http.StatusBadRequest)
			return
		}
	}

	packageData, found, err := s.loadPackageV2(catalogName, packageName)
	if err != nil {
		http.Error(w, fmt.Sprintf("error loading package %q: %v", packageName, err), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, fmt.Sprintf("package %q not found", packageName), http.StatusNotFound)
		return
	}

	// Compute status response
	response, err := computeExtensionStatus(packageData, fromVersionRelease, platform)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to compute extension status for package %q: %v", packageName, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, fmt.Sprintf("error encoding response: %v", err), http.StatusInternalServerError)
		return
	}
}

func computeExtensionStatus(pkg *fbc.PackageV2, fromVersionRelease *fbc.VersionRelease, platform fbc.MajorMinor) (*ExtensionStatusResponse, error) {
	response := &ExtensionStatusResponse{
		Package:                pkg.Package,
		CurrentPlatformVersion: platform,
	}

	// Build graph
	graph, err := fbc.NewGraph(fbc.GraphConfig{
		Packages:     []fbc.PackageV2{*pkg},
		AsOf:         time.Now(),
		IncludePreGA: false,
	})
	if err != nil {
		return nil, fmt.Errorf("error building graph: %w", err)
	}

	var currentNode *fbc.Node
	if fromVersionRelease != nil {
		currentNode = graph.FirstNodeMatching(fbc.ExactVersionRelease(*fromVersionRelease))
		if currentNode == nil {
			return nil, fmt.Errorf("version-release %q not found in catalog", fromVersionRelease.String())
		}
		// Populate current version info
		response.CurrentExtensionVersion = nodeToVersionInfo(currentNode, platform)
	}

	// Find versions
	availableVersions := graph.PreferredNodes(pkg.Package, currentNode, platform)

	// Sort availableVersions (preferred versions first)
	slices.SortFunc(availableVersions, func(a, b *fbc.Node) int {
		return b.Compare(a)
	})
	response.AvailableVersions = mapSlice(availableVersions, func(n *fbc.Node) VersionInfo {
		return *nodeToVersionInfo(n, platform)
	})

	return response, nil
}

func mapSlice[T any, U any](slice []T, fn func(T) U) []U {
	result := make([]U, len(slice))
	for i, v := range slice {
		result[i] = fn(v)
	}
	return result
}

func nodeToVersionInfo(node *fbc.Node, platform fbc.MajorMinor) *VersionInfo {
	versionsToStrings := func(in sets.Set[fbc.MajorMinor]) []string {
		versionSlice := mapSlice(in.UnsortedList(), func(mm fbc.MajorMinor) string { return mm.String() })
		slices.Sort(versionSlice)
		return versionSlice
	}
	releaseDate := ""
	if !node.ReleaseDate.IsZero() {
		releaseDate = node.ReleaseDate.Format(time.RFC3339)
	}

	lifeCyclePhaseEnds := "N/A"
	if node.LifecyclePhaseEnds != nil {
		lifeCyclePhaseEnds = node.LifecyclePhaseEnds.String()
	}

	return &VersionInfo{
		Version:                        node.VR(),
		LifecyclePhase:                 node.LifecyclePhase.String(),
		LifecyclePhaseEnds:             lifeCyclePhaseEnds,
		Retracted:                      node.Retracted,
		ReleaseDate:                    releaseDate,
		Notifications:                  generateNotifications(node, platform),
		SupportedPlatformVersions:      versionsToStrings(node.SupportedPlatformVersions),
		RequiresUpdatePlatformVersions: versionsToStrings(node.RequiresUpdatePlatformVersions),
	}
}

func generateNotifications(node *fbc.Node, platform fbc.MajorMinor) []Notification {
	var notifications []Notification

	// Check if retracted
	if node.Retracted {
		notifications = append(notifications, Notification{
			Type:     WarningTypeRetracted,
			Severity: SeverityError,
			Message:  fmt.Sprintf("Version %s is retracted and should not be used", node.VR()),
		})
	}

	// Check lifecycle phase
	if node.LifecyclePhase == fbc.LifecyclePhaseEndOfLife {
		notifications = append(notifications, Notification{
			Type:     WarningTypeEOL,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("Version %s is end-of-life", node.VR()),
		})
	}

	if len(node.SupportedPlatformVersions) == 0 && len(node.RequiresUpdatePlatformVersions) == 0 {
		notifications = append(notifications, Notification{
			Type:     WarningTypeNotSupported,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("Version %s is not officially supported on any platform", node.VR()),
		})
	} else {
		if node.RequiresUpdatePlatformVersions.Has(platform) {
			notifications = append(notifications, Notification{
				Type:     WarningTypeRequiresUpdate,
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("Version %s is compatible with platform version %s, but an update is required to remain supported", node.VR(), platform.String()),
			})
		} else if !node.SupportedPlatformVersions.Has(platform) {
			notifications = append(notifications, Notification{
				Type:     WarningTypeNotSupported,
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("Version %s is not supported on platform version %s", node.VR(), platform.String()),
			})
		}
	}

	return notifications
}

func (s *LocalDirV1) handleClusterUpdatePlan(w http.ResponseWriter, r *http.Request) {
	s.m.RLock()
	defer s.m.RUnlock()

	catalogName := r.PathValue("catalog")
	packageName := r.PathValue("packageName")
	fromExtensionVersionReleaseStr := r.PathValue("fromExtensionVersionRelease")
	fromExtensionVersionStr, fromExtensionReleaseStr, _ := strings.Cut(fromExtensionVersionReleaseStr, "@")

	toPlatformVersionStr := r.PathValue("toPlatformVersion")

	fromExtensionVersionRelease, err := fbc.NewVersionRelease(fromExtensionVersionStr, fromExtensionReleaseStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid version release string %q: %v", fromExtensionVersionReleaseStr, err), http.StatusBadRequest)
		return
	}

	// Parse to platform version
	toPlatformVersion, err := fbc.NewMajorMinorFromString(toPlatformVersionStr)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid toPlatformVersion %q: %v", toPlatformVersionStr, err), http.StatusBadRequest)
		return
	}

	// Get current platform version
	fromPlatform, err := s.ClusterVersionGetter.GetVersion(r.Context())
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to detect current platform version: %v", err), http.StatusInternalServerError)
		return
	}

	// Load package data
	packageData, found, err := s.loadPackageV2(catalogName, packageName)
	if err != nil {
		http.Error(w, fmt.Sprintf("error loading package %q: %v", packageName, err), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, fmt.Sprintf("package %q not found", packageName), http.StatusNotFound)
		return
	}

	// Compute the update plan
	response, err := computeClusterUpdatePlan(packageData, *fromExtensionVersionRelease, fromPlatform, toPlatformVersion)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to compute cluster update plan for package %q: %v", packageName, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, fmt.Sprintf("error encoding response: %v", err), http.StatusInternalServerError)
		return
	}
}

func computeClusterUpdatePlan(pkg *fbc.PackageV2, fromExtensionVersionRelease fbc.VersionRelease, fromPlatform, toPlatform fbc.MajorMinor) (*ClusterUpdatePlanResponse, error) {
	response := &ClusterUpdatePlanResponse{
		Package: pkg.Package,
	}

	// Validate platform update
	if err := validatePlatformUpdate(fromPlatform, toPlatform); err != nil {
		response.Error = err.Error()
		return response, nil
	}

	// Build graph
	graph, err := fbc.NewGraph(fbc.GraphConfig{
		Packages:     []fbc.PackageV2{*pkg},
		AsOf:         time.Now(),
		IncludePreGA: false,
	})
	if err != nil {
		return nil, fmt.Errorf("error building graph: %w", err)
	}

	// Find the starting node
	fromNode := graph.FirstNodeMatching(fbc.AndNodes(
		fbc.PackageNodes(pkg.Package),
		fbc.ExactVersionRelease(fromExtensionVersionRelease),
	))
	if fromNode == nil {
		response.Error = fmt.Sprintf("version release %q not found in catalog", fromExtensionVersionRelease.String())
		return response, nil
	}

	response.FromExtension = ptr.Deref(nodeToVersionInfo(fromNode, fromPlatform), VersionInfo{})
	response.FromPlatformVersion = fromPlatform
	response.ToPlatformVersion = toPlatform

	// Compute the update path
	updatePlan := planPlatformUpdate(graph, fromNode, fromPlatform, toPlatform)
	if updatePlan.Error != nil {
		response.Error = updatePlan.Error.Error()
		return response, nil
	}

	// Convert nodes to version info
	response.BeforePlatformUpdate = mapSlice(updatePlan.Before, func(n *fbc.Node) VersionInfo {
		return *nodeToVersionInfo(n, fromPlatform)
	})
	response.AfterPlatformUpdate = mapSlice(updatePlan.After, func(n *fbc.Node) VersionInfo {
		return *nodeToVersionInfo(n, toPlatform)
	})

	return response, nil
}

type platformNodeUpdate struct {
	Before []*fbc.Node
	After  []*fbc.Node
	Error  error
}

func validatePlatformUpdate(from, to fbc.MajorMinor) error {
	diff := from.Compare(to)
	if diff == 0 {
		return fmt.Errorf("platform update requires different from version (%s) and to version (%s)", from.String(), to.String())
	}
	if diff > 0 {
		return fmt.Errorf("downgrading from %s to %s is not supported", from.String(), to.String())
	}
	return nil
}

func planPlatformUpdate(g *fbc.Graph, from *fbc.Node, fromPlatform, toPlatform fbc.MajorMinor) platformNodeUpdate {
	// If the from node is not supported on the current platform version, that issue needs to be resolved
	// before planning a platform update.
	if !from.SupportedPlatformVersions.Has(fromPlatform) && !from.RequiresUpdatePlatformVersions.Has(fromPlatform) {
		return platformNodeUpdate{
			Error: fmt.Errorf("version %s is not supported on the current platform version %s", from.VR(), fromPlatform.String()),
		}
	}

	// Build set of traversed platforms
	traversedPlatforms := sets.New[fbc.MajorMinor]()
	for curPlatform := fromPlatform; curPlatform.Compare(toPlatform) <= 0; curPlatform.Minor++ {
		traversedPlatforms.Insert(curPlatform)
	}

	fromPlatformSet := sets.New[fbc.MajorMinor](fromPlatform)
	toPlatformSet := sets.New[fbc.MajorMinor](toPlatform)

	// Find all update paths to nodes supported on the toPlatform
	type updatePath struct {
		nodes  []*fbc.Node
		weight float64
	}
	var updatePaths []updatePath

	for to := range g.NodesMatching(fbc.AndNodes(
		fbc.PackageNodes(from.Name),
		supportedOnPlatforms(toPlatformSet),
	)) {
		path, weight, ok := g.Paths().Between(from.ID(), to.ID())
		if !ok || weight == math.Inf(1) {
			continue
		}
		nodes := mapSlice(path, func(n graph.Node) *fbc.Node { return n.(*fbc.Node) })
		updatePaths = append(updatePaths, updatePath{
			nodes:  nodes,
			weight: weight,
		})
	}

	// Sort update paths by weight (then by number of updates)
	slices.SortFunc(updatePaths, func(a, b updatePath) int {
		if v := cmp.Compare(a.weight, b.weight); v != 0 {
			return v
		}
		return cmp.Compare(len(a.nodes), len(b.nodes))
	})

	// For each update path, try to find a viable plan
	for _, p := range updatePaths {
		pnu := platformNodeUpdate{}

		// 1. Build pre-update path
		for _, n := range p.nodes {
			if !functionalOnPlatforms(fromPlatformSet)(g, n) {
				break
			}
			pnu.Before = append(pnu.Before, n)
		}

		// 2. Identify span node and check for compatibility across all platform versions
		spanNode := pnu.Before[len(pnu.Before)-1]
		if !functionalOnPlatforms(traversedPlatforms)(g, spanNode) {
			// This path won't work, so move on to the next possible path
			continue
		}

		// 3. Build the post-update path
		if len(pnu.Before) == len(p.nodes) {
			// If the entire update can happen prior to the platform update, we're done!
			return pnu
		}
		if len(pnu.Before) != 0 {
			// The span node shows up as the last node in the pre-update path.
			// This ensures that the span node shows up again as the first node
			// of the post-update path.
			pnu.After = append(pnu.After, spanNode)
		}
		for _, n := range p.nodes[len(pnu.Before):] {
			if !functionalOnPlatforms(toPlatformSet)(g, n) {
				break
			}
			pnu.After = append(pnu.After, n)
		}

		// NOTE: we know that the final node in the update path is supported on the "to platform" because that criteria
		// was used originally when constructing the candidate update paths. Therefore, there is no need to check the
		// last post-update node again for "to platform" support.

		if len(pnu.Before)+len(pnu.After)-1 != len(p.nodes) {
			// We know we have an invalid path if not all nodes from the original
			// path show up in the before/after path (with the span node
			// showing up twice). We subtract 1 to make sure the span node is not
			// double-counted.
			continue
		}
		return pnu
	}

	// At this point, not a single candidate update path was viable
	return platformNodeUpdate{
		Error: fmt.Errorf("no update paths from version %s coincide with the support along the platform update path", from.VR()),
	}
}

func supportedOnPlatforms(platforms sets.Set[fbc.MajorMinor]) fbc.NodePredicate {
	return func(_ *fbc.Graph, n *fbc.Node) bool {
		return n.SupportedPlatformVersions.IsSuperset(platforms)
	}
}

func functionalOnPlatforms(platforms sets.Set[fbc.MajorMinor]) fbc.NodePredicate {
	return func(_ *fbc.Graph, n *fbc.Node) bool {
		nodeFunctionalPlatforms := n.SupportedPlatformVersions.Union(n.RequiresUpdatePlatformVersions)
		return nodeFunctionalPlatforms.IsSuperset(platforms)
	}
}

func (s *LocalDirV1) loadPackageV2(catalog, packageName string) (*fbc.PackageV2, bool, error) {
	if !s.EnableMetasHandler {
		return nil, false, fmt.Errorf("index-based lookup requires EnableMetasHandler to be true")
	}

	catalogFile, _, err := s.catalogData(catalog)
	if err != nil {
		return nil, false, err
	}
	defer catalogFile.Close()

	idx, err := s.getIndex(catalog)
	if err != nil {
		return nil, false, err
	}

	// Use the index to get only the olm.package.v2 entries for this package
	indexReader := idx.Get(catalogFile, fbc.SchemaPackageV2, packageName, "")

	var foundPackage *fbc.PackageV2

	err = declcfg.WalkMetasReader(indexReader, func(meta *declcfg.Meta, err error) error {
		if err != nil {
			return err
		}

		var pkg fbc.PackageV2
		if err := json.Unmarshal(meta.Blob, &pkg); err != nil {
			return fmt.Errorf("error unmarshaling package: %w", err)
		}

		foundPackage = &pkg
		return nil
	})

	if err != nil {
		return nil, false, err
	}

	if foundPackage == nil {
		return nil, false, nil
	}

	return foundPackage, true, nil
}
