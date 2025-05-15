package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/google/renameio/v2"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"
)

func (s *LocalDirV1) handleV1ConsoleCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := r.PathValue("catalog")
	consoleFile, consoleStat, err := s.consoleData(catalog)
	if err != nil {
		httpError(w, err)
		return
	}
	http.ServeContent(w, r, "catalog.json", consoleStat.ModTime(), consoleFile)
}

func (s *LocalDirV1) handleV1ConsoleIcon(w http.ResponseWriter, r *http.Request) {
	catalog := r.PathValue("catalog")
	fileName := r.PathValue("iconFile")
	packageName, ext, ok := strings.Cut(fileName, ".")
	if !ok {
		httpError(w, fmt.Errorf("invalid file name %s", fileName))
	}

	iconFile, iconStat, err := s.iconData(catalog, packageName, ext)
	if err != nil {
		httpError(w, err)
		return
	}
	http.ServeContent(w, r, fmt.Sprintf("%s.%s", packageName, ext), iconStat.ModTime(), iconFile)
}

func (s *LocalDirV1) consoleData(catalog string) (*os.File, os.FileInfo, error) {
	f, err := os.Open(catalogConsoleFilePath(s.catalogDir(catalog)))
	if err != nil {
		return nil, nil, err
	}
	fStat, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	return f, fStat, nil
}

func (s *LocalDirV1) iconData(catalog, packageName, ext string) (*os.File, os.FileInfo, error) {
	f, err := os.Open(catalogConsolePackageIconPath(s.catalogDir(catalog), packageName, ext))
	if err != nil {
		return nil, nil, err
	}
	fStat, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	return f, fStat, nil
}

func catalogConsoleFilePath(catalogDir string) string {
	return filepath.Join(catalogDir, "console-catalog.json")
}

func catalogConsolePackageIconPath(catalogDir, packageName, ext string) string {
	return filepath.Join(catalogDir, fmt.Sprintf("icon-%s.%s", packageName, ext))
}

func storeConsoleData(catalogDir string, metas <-chan *declcfg.Meta) error {
	type packageCard struct {
		DisplayName      string `json:"displayName"`
		Provider         string `json:"provider"`
		ShortDescription string `json:"shortDescription"`
		IconURL          string `json:"iconUrl"`
	}
	type searchFacets struct {
		Categories         map[string][]string `json:"categories"`
		Capabilities       map[string][]string `json:"capabilities"`
		Features           map[string][]string `json:"features"`
		ValidSubscriptions map[string][]string `json:"validSubscriptions"`
	}
	type catalogView struct {
		Packages     map[string]packageCard `json:"packages"`
		SearchFacets searchFacets           `json:"searchFacets"`
	}

	packageCards := make(map[string]packageCard)
	highestVersionSeenSoFarWithMetadata := make(map[string]semver.Version)
	bestMetadataSoFar := make(map[string]property.CSVMetadata)
	facets := searchFacets{
		Categories:         make(map[string][]string),
		Capabilities:       make(map[string][]string),
		Features:           make(map[string][]string),
		ValidSubscriptions: make(map[string][]string),
	}
	for m := range metas {
		switch m.Schema {
		case declcfg.SchemaPackage:
			var pkg declcfg.Package
			if err := json.Unmarshal(m.Blob, &pkg); err != nil {
				return err
			}
			pkgCard := packageCards[pkg.Name]
			if pkg.Icon != nil {
				ext := ""
				switch pkg.Icon.MediaType {
				case "image/png":
					ext = "png"
				case "image/jpeg":
					ext = "jpeg"
				case "image/svg+xml":
					ext = "svg"
				case "image/gif":
					ext = "gif"
				default:
					return fmt.Errorf("unknown icon media type: %s", pkg.Icon.MediaType)
				}
				if err := storeIcon(catalogDir, pkg.Icon.Data, pkg.Name, ext); err != nil {
					return err
				}
				pkgCard.IconURL = fmt.Sprintf("/api/v1/console/icons/%s.%s", pkg.Name, ext)
			}
			packageCards[pkg.Name] = pkgCard
		case declcfg.SchemaBundle:
			var bundle declcfg.Bundle
			if err := json.Unmarshal(m.Blob, &bundle); err != nil {
				return err
			}
			ver, metadata, err := getBundleVersionAndMetadata(bundle)
			if err != nil {
				return err
			}
			if ver == nil || metadata == nil {
				continue
			}
			verSoFar := highestVersionSeenSoFarWithMetadata[bundle.Package]

			compare, err := compareVersions(*ver, verSoFar)
			if err != nil {
				return err
			}
			if compare >= 0 {
				highestVersionSeenSoFarWithMetadata[bundle.Package] = *ver
				bestMetadataSoFar[bundle.Package] = *metadata
			}
		}
	}

	for pkgName, metadata := range bestMetadataSoFar {
		pkgCard, ok := packageCards[pkgName]
		if !ok {
			return fmt.Errorf("unknown package %s", pkgName)
		}
		pkgCard.DisplayName = metadata.DisplayName
		pkgCard.Provider = metadata.Provider.Name
		pkgCard.ShortDescription = metadata.Annotations["description"]
		if pkgCard.ShortDescription == "" {
			lines := strings.Split(strings.TrimSpace(metadata.Description), "\n")
			pkgCard.ShortDescription = lines[0]
		}
		packageCards[pkgName] = pkgCard

		for k, v := range metadata.Annotations {
			switch k {
			case "capabilities":
				facets.Capabilities[v] = append(facets.Capabilities[v], pkgName)
			case "categories":
				categories := strings.Split(v, ",")
				for _, c := range categories {
					c = strings.TrimSpace(c)
					facets.Categories[c] = append(facets.Categories[c], pkgName)
				}
			case "operators.openshift.io/valid-subscription":
				var subs []string
				if err := json.Unmarshal([]byte(v), &subs); err != nil {
					facets.ValidSubscriptions[v] = append(facets.ValidSubscriptions[v], pkgName)
				} else {
					for _, sub := range subs {
						facets.ValidSubscriptions[sub] = append(facets.ValidSubscriptions[sub], pkgName)
					}
				}
			case "features.operators.openshift.io/token-auth-gcp", "features.operators.openshift.io/token-auth-azure",
				"features.operators.openshift.io/token-auth-aws", "features.operators.openshift.io/tls-profiles",
				"features.operators.openshift.io/proxy-aware", "features.operators.openshift.io/fips-compliant",
				"features.operators.openshift.io/disconnected", "features.operators.openshift.io/csi",
				"features.operators.openshift.io/cni", "features.operators.openshift.io/cnf":
				if v == "true" {
					facets.Features[k] = append(facets.Features[k], pkgName)
				}
			}
		}
	}
	for k, pkgNames := range facets.Categories {
		slices.Sort(pkgNames)
		facets.Categories[k] = pkgNames
	}
	for k, pkgNames := range facets.Capabilities {
		slices.Sort(pkgNames)
		facets.Capabilities[k] = pkgNames
	}
	for k, pkgNames := range facets.Features {
		slices.Sort(pkgNames)
		facets.Features[k] = pkgNames
	}
	for k, pkgNames := range facets.ValidSubscriptions {
		slices.Sort(pkgNames)
		facets.ValidSubscriptions[k] = pkgNames
	}

	cv := catalogView{
		Packages:     packageCards,
		SearchFacets: facets,
	}

	f, err := renameio.NewPendingFile(catalogConsoleFilePath(catalogDir))
	if err != nil {
		return err
	}
	defer f.Cleanup()

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cv); err != nil {
		return err
	}
	return f.CloseAtomicallyReplace()
}

func storeIcon(catalogDir string, data []byte, packageName, ext string) error {
	return renameio.WriteFile(catalogConsolePackageIconPath(catalogDir, packageName, ext), data, 0600)
}

func getBundleVersionAndMetadata(bundle declcfg.Bundle) (*semver.Version, *property.CSVMetadata, error) {
	var (
		version  *semver.Version
		metadata *property.CSVMetadata
	)
	for _, p := range bundle.Properties {
		switch p.Type {
		case property.TypeCSVMetadata:
			var csvMetadata property.CSVMetadata
			if err := json.Unmarshal(p.Value, &csvMetadata); err != nil {
				return nil, nil, err
			}
			metadata = &csvMetadata
		case property.TypePackage:
			var pkg property.Package
			if err := json.Unmarshal(p.Value, &pkg); err != nil {
				return nil, nil, err
			}
			v, err := semver.Parse(pkg.Version)
			if err != nil {
				return nil, nil, err
			}
			version = &v
		}
		if version != nil && metadata != nil {
			break
		}
	}
	return version, metadata, nil
}

func compareVersions(a, b semver.Version) (int, error) {
	if v := a.Compare(b); v != 0 {
		return v, nil
	}
	if len(a.Build) == 0 {
		return -1, nil
	}
	if len(b.Build) == 0 {
		return 1, nil
	}

	aPre, err := semver.NewPRVersion(strings.Join(a.Build, "-"))
	if err != nil {
		return 0, err
	}
	bPre, err := semver.NewPRVersion(strings.Join(b.Build, "-"))
	if err != nil {
		return 0, err
	}
	return aPre.Compare(bPre), nil
}
