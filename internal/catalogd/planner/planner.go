package planner

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
)

type Planner struct {
	KubernetesClient client.Client
	CatalogURL       string
	HTTPClient       *http.Client
}

type ExtensionInfo struct {
	Package                 string        `json:"package"`
	CurrentPlatformVersion  string        `json:"currentPlatformVersion"`
	CurrentExtensionVersion *VersionInfo  `json:"currentExtensionVersion,omitempty"`
	AvailableVersions       []VersionInfo `json:"availableVersions,omitempty"`
}

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

type Notification struct {
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type PlatformUpdatePlan struct {
	Package              string        `json:"package"`
	FromExtension        VersionInfo   `json:"fromExtension"`
	FromPlatformVersion  string        `json:"fromPlatformVersion"`
	ToPlatformVersion    string        `json:"toPlatformVersion"`
	BeforePlatformUpdate []VersionInfo `json:"beforePlatformUpdate"`
	AfterPlatformUpdate  []VersionInfo `json:"afterPlatformUpdate"`
	Error                string        `json:"error,omitempty"`
}

func (p *Planner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ui/planExtensionUpdates", p.handleExtensionUpdates)
	mux.HandleFunc("/ui/planPlatformUpdate", p.handlePlatformUpdate)
	return mux
}

func (p *Planner) handleExtensionUpdates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	extensions, err := p.getClusterExtensions(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get ClusterExtensions: %v", err), http.StatusInternalServerError)
		return
	}

	type ExtensionRow struct {
		Name               string
		Package            string
		Current            string
		LifecyclePhase     string
		LifecyclePhaseEnds string
		Status             string
		Updates            template.HTML
	}

	var rows []ExtensionRow
	for _, ext := range extensions {
		row := ExtensionRow{
			Name: ext.Name,
		}

		if ext.Status.Install != nil {
			row.Current = ext.Status.Install.Bundle.Version
		}

		if ext.Spec.Source.SourceType != ocv1.SourceTypeCatalog {
			row.Status = "Not installed from a catalog: no update information available"
		} else {
			row.Package = ext.Spec.Source.Catalog.PackageName
		}

		// Check if extension is at steady state
		installed := meta.FindStatusCondition(ext.Status.Conditions, ocv1.TypeInstalled)
		progressing := meta.FindStatusCondition(ext.Status.Conditions, ocv1.TypeProgressing)

		if installed == nil || installed.Status != metav1.ConditionTrue || progressing == nil || !(progressing.Status == metav1.ConditionTrue && progressing.Reason == ocv1.ReasonSucceeded) {
			// Not at steady state
			row.Status = "Not Ready"
			if progressing != nil && progressing.Reason != ocv1.ReasonSucceeded {
				row.Updates = template.HTML(fmt.Sprintf("%s: %s", progressing.Reason, progressing.Message))
			} else if installed != nil && installed.Status == metav1.ConditionFalse {
				row.Updates = template.HTML(fmt.Sprintf("Installation failed: %s", installed.Message))
			} else {
				row.Updates = "Status unknown"
			}
		} else {
			row.Status = "Ready"
			// Fetch info from catalog
			catalogs, err := p.getClusterCatalogsForClusterExtension(ctx, ext)
			if err != nil || len(catalogs) == 0 {
				row.Updates = template.HTML("No catalog information available")
			} else {
				info, err := p.fetchExtensionInfo(catalogs[0].Name, ext.Spec.Source.Catalog.PackageName, row.Current)
				if err != nil {
					row.Updates = template.HTML(fmt.Sprintf("Error fetching updates: %v", err))
				} else if len(info.AvailableVersions) == 0 || info.AvailableVersions[0].Version == row.Current {
					row.Updates = template.HTML("No updates available")
				} else {
					var versions []string
					for _, v := range info.AvailableVersions {
						versions = append(versions, v.Version)
					}
					row.Updates = template.HTML(strings.Join(versions, ", "))
				}
				if info != nil {
					row.LifecyclePhase = info.CurrentExtensionVersion.LifecyclePhase
					row.LifecyclePhaseEnds = info.CurrentExtensionVersion.LifecyclePhaseEnds
				}
			}
		}
		rows = append(rows, row)
	}

	// Render HTML table
	tmpl := template.Must(template.New("extensions").Parse(`
<!DOCTYPE html>
<html>
<head>
	<meta charset="UTF-8">
	<title>Extension Updates</title>
	<link rel="preconnect" href="https://fonts.googleapis.com">
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
	<link href="https://fonts.googleapis.com/css2?family=Noto+Sans:wght@400;600&display=swap" rel="stylesheet">
	<style>
		body {
			font-family: 'Noto Sans', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Noto Color Emoji';
			margin: 20px;
		}
		h1 {
			font-weight: 600;
		}
		table {
			border-collapse: collapse;
			width: 100%;
			font-size: 14px;
		}
		th, td {
			border: 1px solid #ddd;
			padding: 12px;
			text-align: left;
		}
		th {
			background-color: #f8f9fa;
			font-weight: 600;
		}
		tr:hover {
			background-color: #f5f5f5;
		}
	</style>
</head>
<body>
	<h1>Extension Updates</h1>
	<table>
		<tr>
			<th>ClusterExtension Name</th>
			<th>Package</th>
			<th>Current Version</th>
            <th>Lifecycle Phase</th>
			<th>Status</th>
			<th>Available Updates</th>
		</tr>
		{{range .}}
		<tr>
			<td>{{.Name}}</td>
			<td>{{.Package}}</td>
			<td>{{.Current}}</td>
			<td>{{.LifecyclePhase}} (until {{.LifecyclePhaseEnds}})</td>
			<td>{{.Status}}</td>
			<td>{{.Updates}}</td>
		</tr>
		{{end}}
	</table>
</body>
</html>
`))

	w.Header().Set("Content-Type", "text/html")
	if err := tmpl.Execute(w, rows); err != nil {
		http.Error(w, fmt.Sprintf("Template error: %v", err), http.StatusInternalServerError)
	}
}

func (p *Planner) handlePlatformUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	toPlatformVersion := r.URL.Query().Get("toPlatformVersion")
	if toPlatformVersion == "" {
		http.Error(w, "Query parameter 'toPlatformVersion' is required", http.StatusBadRequest)
		return
	}

	extensions, err := p.getClusterExtensions(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get ClusterExtensions: %v", err), http.StatusInternalServerError)
		return
	}

	type PlatformUpdateRow struct {
		Name         string
		Package      string
		Current      string
		Status       string
		BeforeUpdate template.HTML
		AfterUpdate  template.HTML
		Error        string
	}

	var rows []PlatformUpdateRow
	for _, ext := range extensions {
		row := PlatformUpdateRow{
			Name:    ext.Name,
			Package: ext.Spec.Source.Catalog.PackageName,
		}

		// Check if extension is at steady state
		installed := meta.FindStatusCondition(ext.Status.Conditions, ocv1.TypeInstalled)
		progressing := meta.FindStatusCondition(ext.Status.Conditions, ocv1.TypeProgressing)

		if ext.Status.Install != nil && ext.Status.Install.Bundle.Version != "" {
			row.Current = ext.Status.Install.Bundle.Version
		}

		if installed == nil || installed.Status != metav1.ConditionTrue || progressing == nil || !(progressing.Status == metav1.ConditionTrue && progressing.Reason == ocv1.ReasonSucceeded) {
			// Not at steady state
			row.Status = "Not Ready"
			if progressing != nil && progressing.Reason != ocv1.ReasonSucceeded {
				row.Error = fmt.Sprintf("%s: %s", progressing.Reason, progressing.Message)
			} else if installed != nil && installed.Status == metav1.ConditionFalse {
				row.Error = fmt.Sprintf("Installation failed: %s", installed.Message)
			} else {
				row.Error = "Status unknown"
			}
		} else {
			row.Status = "Ready"
			// Fetch platform update plan from catalog
			catalogs, err := p.getClusterCatalogsForClusterExtension(ctx, ext)
			if err != nil || len(catalogs) == 0 {
				row.Error = "No catalog information available"
			} else {
				plan, err := p.fetchPlatformUpdatePlan(catalogs[0].Name, ext.Spec.Source.Catalog.PackageName, row.Current, toPlatformVersion)
				if err != nil {
					row.Error = fmt.Sprintf("Error fetching plan: %v", err)
				} else if plan.Error != "" {
					row.Error = plan.Error
				} else {
					var beforeVersions []string
					for _, v := range plan.BeforePlatformUpdate {
						beforeVersions = append(beforeVersions, v.Version)
					}
					if len(beforeVersions) > 1 {
						row.BeforeUpdate = template.HTML(strings.Join(beforeVersions, " → "))
					} else {
						row.BeforeUpdate = template.HTML("None")
					}

					var afterVersions []string
					for _, v := range plan.AfterPlatformUpdate {
						afterVersions = append(afterVersions, v.Version)
					}
					if len(afterVersions) > 1 {
						row.AfterUpdate = template.HTML(strings.Join(afterVersions, " → "))
					} else {
						row.AfterUpdate = template.HTML("None")
					}
				}
			}
		}
		rows = append(rows, row)
	}

	// Render HTML table
	tmpl := template.Must(template.New("platform").Parse(`
<!DOCTYPE html>
<html>
<head>
	<meta charset="UTF-8">
	<title>Platform Update Plan</title>
	<link rel="preconnect" href="https://fonts.googleapis.com">
	<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
	<link href="https://fonts.googleapis.com/css2?family=Noto+Sans:wght@400;600&display=swap" rel="stylesheet">
	<style>
		body {
			font-family: 'Noto Sans', -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif, 'Apple Color Emoji', 'Segoe UI Emoji', 'Noto Color Emoji';
			margin: 20px;
		}
		h1 {
			font-weight: 600;
		}
		table {
			border-collapse: collapse;
			width: 100%;
			font-size: 14px;
		}
		th, td {
			border: 1px solid #ddd;
			padding: 12px;
			text-align: left;
		}
		th {
			background-color: #f8f9fa;
			font-weight: 600;
		}
		tr:hover {
			background-color: #f5f5f5;
		}
	</style>
</head>
<body>
	<h1>Platform Update Plan to {{.ToPlatformVersion}}</h1>
	<table>
		<tr>
			<th>Name</th>
			<th>Package</th>
			<th>Current Version</th>
			<th>Status</th>
			<th>Before Platform Update</th>
			<th>After Platform Update</th>
			<th>Error</th>
		</tr>
		{{range .Rows}}
		<tr>
			<td>{{.Name}}</td>
			<td>{{.Package}}</td>
			<td>{{.Current}}</td>
			<td>{{.Status}}</td>
			<td>{{.BeforeUpdate}}</td>
			<td>{{.AfterUpdate}}</td>
			<td>{{.Error}}</td>
		</tr>
		{{end}}
	</table>
</body>
</html>
`))

	w.Header().Set("Content-Type", "text/html")
	if err := tmpl.Execute(w, map[string]interface{}{
		"ToPlatformVersion": toPlatformVersion,
		"Rows":              rows,
	}); err != nil {
		http.Error(w, fmt.Sprintf("Template error: %v", err), http.StatusInternalServerError)
	}
}

func (p *Planner) getClusterExtensions(ctx context.Context) ([]ocv1.ClusterExtension, error) {
	var ceList ocv1.ClusterExtensionList
	err := p.KubernetesClient.List(ctx, &ceList)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(ceList.Items, func(a, b ocv1.ClusterExtension) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return ceList.Items, nil
}

func (p *Planner) getClusterCatalogsForClusterExtension(ctx context.Context, ext ocv1.ClusterExtension) ([]ocv1.ClusterCatalog, error) {
	if ext.Spec.Source.SourceType != ocv1.SourceTypeCatalog {
		return nil, fmt.Errorf("ClusterExtension %q is not sourced from a catalog", ext.Name)
	}
	selector := labels.Everything()
	if sel := ext.Spec.Source.Catalog.Selector; sel != nil {
		var err error
		selector, err = metav1.LabelSelectorAsSelector(sel)
		if err != nil {
			return nil, err
		}
	}
	var ccList ocv1.ClusterCatalogList
	if err := p.KubernetesClient.List(ctx, &ccList, client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	slices.SortFunc(ccList.Items, func(a, b ocv1.ClusterCatalog) int {
		return cmp.Compare(b.Spec.Priority, a.Spec.Priority)
	})
	for _, cc := range ccList.Items {
		fmt.Println(cc.Name, cc.Spec.Priority)
	}
	return ccList.Items, nil
}

func (p *Planner) fetchExtensionInfo(catalogName, packageName, currentVersion string) (*ExtensionInfo, error) {
	// Build URL: /catalogs/<catalogName>/api/v1alpha1/info/<packageName>/<currentVersion>
	u, err := url.Parse(p.CatalogURL)
	if err != nil {
		return nil, fmt.Errorf("invalid catalog URL: %w", err)
	}
	u.Path = fmt.Sprintf("/catalogs/%s/api/v1alpha1/info/%s/%s", catalogName, packageName, currentVersion)

	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("failed to fetch extension info from catalog %q: %w", catalogName, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var info ExtensionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &info, nil
}

func (p *Planner) fetchPlatformUpdatePlan(catalogName, packageName, currentVersion, toPlatformVersion string) (*PlatformUpdatePlan, error) {
	// Build URL: /catalogs/<catalogName>/api/v1alpha1/platformUpdatePlan/<packageName>/<currentVersion>/<toPlatformVersion>
	u, err := url.Parse(p.CatalogURL)
	if err != nil {
		return nil, fmt.Errorf("invalid catalog URL: %w", err)
	}
	u.Path = fmt.Sprintf("/catalogs/%s/api/v1alpha1/platformUpdatePlan/%s/%s/%s", catalogName, packageName, currentVersion, toPlatformVersion)

	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	resp, err := httpClient.Get(u.String())
	if err != nil {
		return nil, fmt.Errorf("failed to fetch platform update plan: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var plan PlatformUpdatePlan
	if err := json.NewDecoder(resp.Body).Decode(&plan); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &plan, nil
}
