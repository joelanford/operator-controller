package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	bsemver "github.com/blang/semver/v4"
	"k8s.io/klog/v2"

	"github.com/operator-framework/operator-registry/alpha/declcfg"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/shared/fbc"
)

// ExtensionStatusResponse represents the status of a ClusterExtension
type ExtensionStatusResponse struct {
	Package        string         `json:"package"`
	CurrentVersion *VersionInfo   `json:"currentVersion,omitempty"`
	Notifications  []Notification `json:"notifications,omitempty"`
	Upgrades       *Upgrades      `json:"upgrades,omitempty"`
}

// Upgrades contains categorized upgrade options
type Upgrades struct {
	BestInRange        *VersionInfo  `json:"bestInRange,omitempty"`
	OthersInRange      []VersionInfo `json:"othersInRange,omitempty"`
	BestOutsideRange   *VersionInfo  `json:"bestOutsideRange,omitempty"`
	OthersOutsideRange []VersionInfo `json:"othersOutsideRange,omitempty"`
}

// VersionInfo contains metadata about a specific version
type VersionInfo struct {
	Version                        string   `json:"version"`
	LifecyclePhase                 string   `json:"lifecyclePhase"`
	Retracted                      bool     `json:"retracted"`
	ReleaseDate                    string   `json:"releaseDate,omitempty"`
	SupportedPlatformVersions      []string `json:"supportedPlatformVersions,omitempty"`
	RequiresUpdatePlatformVersions []string `json:"requiresUpdatePlatformVersions,omitempty"`
}

// Notification represents information about the current version that a user needs to know.
type Notification struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

const (
	WarningTypeRetracted      = "Retracted"
	WarningTypeEOL            = "EndOfLife"
	WarningTypeIncompatible   = "Incompatible"
	WarningTypeRequiresUpdate = "RequiresUpdate"

	SeverityError   = "error"
	SeverityWarning = "warning"
)

func (s *LocalDirV1) handleExtensionStatus(w http.ResponseWriter, r *http.Request) {
	s.m.RLock()
	defer s.m.RUnlock()

	ctx := r.Context()
	catalog := r.PathValue("catalog")

	// Discover cluster version (currently returns nil, can be implemented later with discovery client)
	var clusterVersion *fbc.MajorMinor

	// List all ClusterExtensions
	var clusterExtensions ocv1.ClusterExtensionList
	if err := s.Client.List(ctx, &clusterExtensions); err != nil {
		klog.FromContext(ctx).Error(err, "error listing ClusterExtensions")
		http.Error(w, fmt.Sprintf("error listing ClusterExtensions: %v", err), http.StatusInternalServerError)
		return
	}

	// Compute status for each ClusterExtension
	var responses []ExtensionStatusResponse
	for _, ext := range clusterExtensions.Items {
		// Skip if no install status or if not installed from a catalog
		if ext.Status.Install == nil || ext.Spec.Source.Catalog == nil {
			continue
		}

		packageName := ext.Spec.Source.Catalog.PackageName
		currentVersionStr := ext.Status.Install.Bundle.Version

		// Parse current version
		currentVersion, err := bsemver.Parse(currentVersionStr)
		if err != nil {
			klog.FromContext(ctx).Error(err, "invalid version for ClusterExtension", "name", ext.Name, "version", currentVersionStr)
			continue
		}

		// Parse version constraint from spec
		versionConstraint := func(_ bsemver.Version) bool {
			return true
		}
		if ext.Spec.Source.Catalog.Version != "" {
			vc, err := bsemver.ParseRange(ext.Spec.Source.Catalog.Version)
			if err != nil {
				klog.FromContext(ctx).Error(err, "invalid version constraint for ClusterExtension", "name", ext.Name, "constraint", ext.Spec.Source.Catalog.Version)
			} else {
				versionConstraint = vc
			}
		}

		// Load package data from catalog
		packageData, found, err := s.loadPackageV2(catalog, packageName)
		if err != nil {
			klog.FromContext(ctx).Error(err, "error loading package", "package", packageName, "catalog", catalog)
			continue
		}
		if !found {
			klog.FromContext(ctx).Info("package not found in catalog", "package", packageName, "catalog", catalog)
			continue
		}

		// Compute status response
		response, err := computeExtensionStatus(packageData, currentVersion, clusterVersion, versionConstraint)
		if err != nil {
			klog.FromContext(ctx).Error(err, "error computing status", "package", packageName)
			continue
		}

		responses = append(responses, *response)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(responses); err != nil {
		http.Error(w, fmt.Sprintf("error encoding response: %v", err), http.StatusInternalServerError)
		return
	}
}

func computeExtensionStatus(pkg *fbc.PackageV2, currentVersion bsemver.Version, clusterVersion *fbc.MajorMinor, versionConstraint bsemver.Range) (*ExtensionStatusResponse, error) {
	response := &ExtensionStatusResponse{
		Package:       pkg.Package,
		Notifications: []Notification{},
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

	// Find current version node
	vr := fbc.VersionRelease{
		Version: currentVersion,
		Release: nil,
	}
	currentNode := graph.FirstNodeMatching(fbc.ExactVersionRelease(vr))

	if currentNode == nil {
		return nil, fmt.Errorf("current version %s not found in catalog", currentVersion.String())
	}

	// Populate current version info
	response.CurrentVersion = nodeToVersionInfo(currentNode)

	// Generate warnings
	response.Notifications = generateWarnings(currentNode, clusterVersion)

	// Find upgrades
	upgrades := findUpgrades(graph, currentNode, clusterVersion)
	if len(upgrades) > 0 {
		response.Upgrades = &Upgrades{}
	}

	// Categorize upgrades
	var upgradesInRange []*fbc.Node
	var upgradesOutsideRange []*fbc.Node

	for _, node := range upgrades {
		if versionConstraint(node.VersionRelease.Version) {
			upgradesInRange = append(upgradesInRange, node)
		} else {
			upgradesOutsideRange = append(upgradesOutsideRange, node)
		}
	}

	// Sort each category (highest version first)
	slices.SortFunc(upgradesInRange, func(a, b *fbc.Node) int {
		return b.Compare(a)
	})
	slices.SortFunc(upgradesOutsideRange, func(a, b *fbc.Node) int {
		return b.Compare(a)
	})

	// Populate best and others in range
	if len(upgradesInRange) > 0 {
		response.Upgrades.BestInRange = nodeToVersionInfo(upgradesInRange[0])
		response.Upgrades.OthersInRange = mapSlice(upgradesInRange[1:], func(n *fbc.Node) VersionInfo {
			return *nodeToVersionInfo(n)
		})
	}

	// Populate best and others outside range
	if len(upgradesOutsideRange) > 0 {
		response.Upgrades.BestOutsideRange = nodeToVersionInfo(upgradesOutsideRange[0])
		response.Upgrades.OthersOutsideRange = mapSlice(upgradesOutsideRange[1:], func(n *fbc.Node) VersionInfo {
			return *nodeToVersionInfo(n)
		})
	}

	return response, nil
}

func mapSlice[T any, U any](slice []T, fn func(T) U) []U {
	result := make([]U, len(slice))
	for i, v := range slice {
		result[i] = fn(v)
	}
	return result
}

func nodeToVersionInfo(node *fbc.Node) *VersionInfo {
	supportedPlatforms := make([]string, 0, node.SupportedPlatformVersions.Len())
	for _, mm := range node.SupportedPlatformVersions.UnsortedList() {
		supportedPlatforms = append(supportedPlatforms, mm.String())
	}
	slices.Sort(supportedPlatforms)

	requiresUpdatePlatforms := make([]string, 0, node.RequiresUpdatePlatformVersions.Len())
	for _, mm := range node.RequiresUpdatePlatformVersions.UnsortedList() {
		requiresUpdatePlatforms = append(requiresUpdatePlatforms, mm.String())
	}
	slices.Sort(requiresUpdatePlatforms)

	releaseDate := ""
	if !node.ReleaseDate.IsZero() {
		releaseDate = node.ReleaseDate.Format(time.RFC3339)
	}

	return &VersionInfo{
		Version:                        node.VR(),
		LifecyclePhase:                 node.LifecyclePhase.String(),
		Retracted:                      node.Retracted,
		ReleaseDate:                    releaseDate,
		SupportedPlatformVersions:      supportedPlatforms,
		RequiresUpdatePlatformVersions: requiresUpdatePlatforms,
	}
}

func generateWarnings(node *fbc.Node, clusterVersion *fbc.MajorMinor) []Notification {
	warnings := []Notification{}

	// Check if retracted
	if node.Retracted {
		warnings = append(warnings, Notification{
			Type:     WarningTypeRetracted,
			Severity: SeverityError,
			Message:  fmt.Sprintf("Version %s is retracted and should not be used", node.VR()),
		})
	}

	// Check lifecycle phase
	if node.LifecyclePhase == fbc.LifecyclePhaseEndOfLife {
		warnings = append(warnings, Notification{
			Type:     WarningTypeEOL,
			Severity: SeverityWarning,
			Message:  fmt.Sprintf("Version %s is end-of-life", node.VR()),
		})
	}

	// Check platform compatibility if cluster version is provided
	if clusterVersion != nil {
		if !node.SupportedPlatformVersions.Has(*clusterVersion) {
			warnings = append(warnings, Notification{
				Type:     WarningTypeIncompatible,
				Severity: SeverityError,
				Message:  fmt.Sprintf("Version %s is not compatible with cluster version %s", node.VR(), clusterVersion.String()),
			})
		}
		if node.RequiresUpdatePlatformVersions.Has(*clusterVersion) {
			warnings = append(warnings, Notification{
				Type:     WarningTypeRequiresUpdate,
				Severity: SeverityWarning,
				Message:  fmt.Sprintf("Version %s is compatible with cluster version %s, but an update is required to remain supported", node.VR(), clusterVersion.String()),
			})
		}
	}

	return warnings
}

func findUpgrades(graph *fbc.Graph, currentNode *fbc.Node, clusterVersion *fbc.MajorMinor) []*fbc.Node {
	// Get immediate successors (one-hop upgrades)
	var upgrades []*fbc.Node

	successorPredicates := []fbc.NodePredicate{
		fbc.SuccessorOf(currentNode),
		func(_ *fbc.Graph, suc *fbc.Node) bool {
			// LifecyclePhase is at least as good as current
			return suc.LifecyclePhase.Compare(currentNode.LifecyclePhase) >= 0
		},
		func(_ *fbc.Graph, suc *fbc.Node) bool {
			// Never suggest updates to pre-GA versions
			return suc.LifecyclePhase != fbc.LifecyclePhasePreGA
		},
		func(_ *fbc.Graph, suc *fbc.Node) bool {
			// Never suggest updates to retracted versions
			return !suc.Retracted
		},
	}
	if clusterVersion != nil {
		successorPredicates = append(successorPredicates, func(_ *fbc.Graph, suc *fbc.Node) bool {
			// If a cluster version was provided, only suggest versions that support the cluster version.
			return suc.SupportedPlatformVersions.Has(*clusterVersion)
		})
	}

	for successor := range graph.NodesMatching(fbc.AndNodes(successorPredicates...)) {
		upgrades = append(upgrades, successor)
	}

	return upgrades
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
