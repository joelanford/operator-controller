/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/oci/archive"
	"go.podman.io/image/v5/oci/layout"
	"go.podman.io/image/v5/pkg/compression"
	"go.podman.io/image/v5/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/bundle/handler"
	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/bundle/source"
	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/render"
	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/render/certproviders"
	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/render/registryv1"
	"github.com/operator-framework/operator-controller/internal/shared/util/imagev2"
	"github.com/operator-framework/operator-controller/internal/shared/version"
)

type renderConfig struct {
	namespace    string
	configFile   string
	certProvider string
	pullCasDir   string
}

// bundleConfig represents the configuration file format for bundle rendering
type bundleConfig struct {
	WatchNamespace string `json:"watchNamespace,omitempty"`
}

var cfg = &renderConfig{}

var rootCmd = &cobra.Command{
	Use:   "olm-render <transport>:<location>",
	Short: "Render a bundle to plain Kubernetes manifests",
	Long: `Render a registry+v1 bundle to plain Kubernetes manifests.

Supported transports (aligned with skopeo):
  dir:<path>           Directory containing unpacked bundle
  tar:<path>           Tar archive of bundle directory (auto-decompresses)
  oci:<path>           OCI layout directory
  oci-archive:<path>   Tar of OCI layout (auto-decompresses)
  docker://<ref>       Remote docker/OCI registry image (single manifest only)

Certificate providers (--cert-provider):
  cert-manager        Use cert-manager for webhook certificates
  service-ca          Use OpenShift Service CA Operator

Manifests are output to stdout in YAML format, separated by "---".`,
	Example: `  # Render from directory
  olm-render dir:./my-bundle-dir --namespace operators

  # Render from tarball of bundle directory
  olm-render tar:bundle.tar.gz --namespace operators

  # Render from OCI layout directory
  olm-render oci:./oci-bundle --namespace operators

  # Render from OCI archive (tarball of OCI layout)
  olm-render oci-archive:bundle.tar --namespace operators

  # Render from remote registry
  olm-render docker://quay.io/my-org/my-operator-bundle:v1.0.0 --namespace operators

  # With config file (specifies watchNamespace)
  olm-render docker://quay.io/my-org/my-operator:v1.0.0 -n operators --config config.yaml

  # With cert-manager webhook support
  olm-render docker://quay.io/my-org/my-operator:v1.0.0 -n operators --cert-provider cert-manager`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runRender,
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version.String())
	},
}

func init() {
	flags := rootCmd.Flags()
	flags.StringVarP(&cfg.namespace, "namespace", "n", "", "Install namespace (required)")
	flags.StringVarP(&cfg.configFile, "config", "c", "", "Bundle configuration file")
	flags.StringVar(&cfg.certProvider, "cert-provider", "", "Certificate provider: cert-manager, service-ca")
	flags.StringVar(&cfg.pullCasDir, "ca-dir", "", "CA certificates directory for registry TLS")

	_ = rootCmd.MarkFlagRequired("namespace")
	rootCmd.AddCommand(versionCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

type transportType int

const (
	transportDir transportType = iota
	transportTar
	transportOCI
	transportOCIArchive
	transportDocker
)

func parseTransport(input string) (transportType, string, error) {
	switch {
	case strings.HasPrefix(input, "dir:"):
		return transportDir, strings.TrimPrefix(input, "dir:"), nil
	case strings.HasPrefix(input, "tar:"):
		return transportTar, strings.TrimPrefix(input, "tar:"), nil
	case strings.HasPrefix(input, "oci:"):
		return transportOCI, strings.TrimPrefix(input, "oci:"), nil
	case strings.HasPrefix(input, "oci-archive:"):
		return transportOCIArchive, strings.TrimPrefix(input, "oci-archive:"), nil
	case strings.HasPrefix(input, "docker://"):
		return transportDocker, strings.TrimPrefix(input, "docker://"), nil
	default:
		return 0, "", fmt.Errorf("unknown transport: %q (expected dir:, tar:, oci:, oci-archive:, or docker://)", input)
	}
}

func runRender(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	input := args[0]

	transport, location, err := parseTransport(input)
	if err != nil {
		return err
	}

	// Validate cert-provider flag
	if cfg.certProvider != "" && cfg.certProvider != "cert-manager" && cfg.certProvider != "service-ca" {
		return fmt.Errorf("invalid --cert-provider value %q: must be 'cert-manager' or 'service-ca'", cfg.certProvider)
	}

	// Load bundle filesystem based on transport
	bundleFS, cleanup, err := loadBundle(ctx, transport, location)
	if err != nil {
		return fmt.Errorf("loading bundle: %w", err)
	}
	defer cleanup()

	// Load bundle from filesystem
	rv1Bundle, err := source.FromFS(bundleFS).GetBundle()
	if err != nil {
		return fmt.Errorf("parsing bundle: %w", err)
	}

	// Process config file if provided
	var renderOpts []render.Option
	if cfg.configFile != "" {
		bundleCfg, err := loadConfig(cfg.configFile)
		if err != nil {
			return fmt.Errorf("loading config file: %w", err)
		}
		if bundleCfg.WatchNamespace != "" {
			renderOpts = append(renderOpts, render.WithTargetNamespaces(bundleCfg.WatchNamespace))
		}
	}

	// Add certificate provider if specified
	if cfg.certProvider != "" {
		certProvider, err := getCertificateProvider(cfg.certProvider)
		if err != nil {
			return err
		}
		renderOpts = append(renderOpts, render.WithCertificateProvider(certProvider))
	}

	// Render manifests
	objs, err := registryv1.Renderer.Render(rv1Bundle, cfg.namespace, renderOpts...)
	if err != nil {
		return fmt.Errorf("rendering manifests: %w", err)
	}

	// Output YAML
	return outputYAML(os.Stdout, objs)
}

func getCertificateProvider(provider string) (render.CertificateProvider, error) {
	switch provider {
	case "cert-manager":
		return certproviders.CertManagerCertificateProvider{}, nil
	case "service-ca":
		return certproviders.OpenshiftServiceCaCertificateProvider{}, nil
	default:
		return nil, fmt.Errorf("unknown certificate provider: %q", provider)
	}
}

func loadConfig(path string) (*bundleConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg bundleConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func loadBundle(ctx context.Context, transport transportType, location string) (fs.FS, func(), error) {
	switch transport {
	case transportDir:
		return loadFromDir(location)
	case transportTar:
		return loadFromTar(location)
	case transportOCI:
		return loadFromOCI(ctx, location)
	case transportOCIArchive:
		return loadFromOCIArchive(ctx, location)
	case transportDocker:
		return loadFromDocker(ctx, location, cfg.pullCasDir)
	default:
		return nil, nil, fmt.Errorf("unsupported transport: %d", transport)
	}
}

func loadFromDir(path string) (fs.FS, func(), error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	return os.DirFS(absPath), func() {}, nil
}

func loadFromTar(path string) (fs.FS, func(), error) {
	tmpDir, err := os.MkdirTemp("", "olm-render-tar-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	f, err := os.Open(path)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	defer f.Close()

	// Auto-decompress
	decompressed, _, err := compression.AutoDecompress(f)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("decompressing archive: %w", err)
	}
	defer decompressed.Close()

	// Extract tar
	if err := untar(decompressed, tmpDir); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("extracting tar: %w", err)
	}

	return os.DirFS(tmpDir), cleanup, nil
}

func untar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}

		// Clean the path and skip entries that resolve to the root
		cleanName := filepath.Clean(header.Name)
		if cleanName == "." || cleanName == "/" {
			continue
		}

		// Sanitize path to prevent directory traversal
		target := filepath.Join(dest, cleanName)
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("invalid tar path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			// Limit copy to prevent decompression bombs
			if _, err := io.Copy(outFile, io.LimitReader(tr, 100*1024*1024)); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadFromOCI(ctx context.Context, path string) (fs.FS, func(), error) {
	tmpDir, err := os.MkdirTemp("", "olm-render-oci-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	// Create reference to OCI layout
	layoutRef, err := layout.NewReference(path, "")
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating OCI layout reference: %w", err)
	}

	repo, err := imagev2.NewContainersImageClient(ctx, layoutRef, nil)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating image client from OCI layout: %w", err)
	}
	defer repo.Close()

	resolver := imagev2.NewResolver()
	resolver.Register(&handler.RegistryV1Handler{})
	if err := resolver.Unpack(ctx, repo, tmpDir); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("unpacking OCI layout: %w", err)
	}

	return os.DirFS(tmpDir), cleanup, nil
}

func loadFromOCIArchive(ctx context.Context, path string) (fs.FS, func(), error) {
	tmpDir, err := os.MkdirTemp("", "olm-render-oci-archive-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	// Create reference to OCI archive
	archiveRef, err := archive.NewReference(path, "")
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating OCI archive reference: %w", err)
	}

	repo, err := imagev2.NewContainersImageClient(ctx, archiveRef, nil)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating image client from OCI archive: %w", err)
	}
	defer repo.Close()

	resolver := imagev2.NewResolver()
	resolver.Register(&handler.RegistryV1Handler{})
	if err := resolver.Unpack(ctx, repo, tmpDir); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("unpacking OCI archive: %w", err)
	}

	return os.DirFS(tmpDir), cleanup, nil
}

func loadFromDocker(ctx context.Context, ref string, casDir string) (fs.FS, func(), error) {
	tmpDir, err := os.MkdirTemp("", "olm-render-docker-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(tmpDir) }

	named, err := reference.ParseNamed(ref)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("parsing reference %q: %w", ref, err)
	}

	dockerRef, err := docker.NewReference(named)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating docker reference for %q: %w", ref, err)
	}

	srcCtx := &types.SystemContext{}
	if casDir != "" {
		srcCtx.DockerCertPath = casDir
		srcCtx.OCICertPath = casDir
	}

	repo, err := imagev2.NewContainersImageClient(ctx, dockerRef, srcCtx)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("creating image client: %w", err)
	}
	defer repo.Close()

	resolver := imagev2.NewResolver()
	resolver.Register(&handler.RegistryV1Handler{})
	if err := resolver.Unpack(ctx, repo, tmpDir); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("unpacking docker image: %w", err)
	}

	return os.DirFS(tmpDir), cleanup, nil
}

func outputYAML(w io.Writer, objs []client.Object) error {
	for i, obj := range objs {
		if i > 0 {
			if _, err := fmt.Fprintln(w, "---"); err != nil {
				return err
			}
		}

		data, err := yaml.Marshal(obj)
		if err != nil {
			return fmt.Errorf("marshaling object %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
		}

		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}
