package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	bsemver "github.com/blang/semver/v4"
	"k8s.io/apimachinery/pkg/util/sets"

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

const (
	WarningTypeRetracted      = "Retracted"
	WarningTypeEOL            = "EndOfLife"
	WarningTypeNotSupported   = "NotSupported"
	WarningTypeRequiresUpdate = "RequiresUpdate"

	SeverityError   = "error"
	SeverityWarning = "warning"
)

func (s *LocalDirV1) handleResolutions(w http.ResponseWriter, r *http.Request) {
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
	fromVersionStr := r.PathValue("fromVersion")
	fromReleaseStr := r.PathValue("fromRelease")

	var fromVersionRelease *fbc.VersionRelease
	if fromVersionStr != "" {
		// Parse current version
		fromVersion, err := bsemver.Parse(fromVersionStr)
		if err != nil {
			http.Error(w, fmt.Sprintf("invalid version %q: %v", fromVersionStr, err), http.StatusBadRequest)
			return
		}
		var fromRelease fbc.Release
		if fromReleaseStr != "" {
			fromRelease, err = fbc.NewRelease(fromReleaseStr)
			if err != nil {
				http.Error(w, fmt.Sprintf("invalid release %q: %v", fromReleaseStr, err), http.StatusBadRequest)
				return
			}
		}
		fromVersionRelease = &fbc.VersionRelease{
			Version: fromVersion,
			Release: fromRelease,
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

	return &VersionInfo{
		Version:                        node.VR(),
		LifecyclePhase:                 node.LifecyclePhase.String(),
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
