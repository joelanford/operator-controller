package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"iter"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/archive"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/types"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"

	imageutil "github.com/operator-framework/operator-controller/internal/util/image"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := newCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newCmd() *cobra.Command {
	var propertyType string
	cmd := &cobra.Command{
		Use:  "render-csv-metadata",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			img := args[0]
			puller := imageutil.ContainersImagePuller{SourceCtxFunc: func(ctx context.Context) (*types.SystemContext, error) {
				return &types.SystemContext{}, nil
			}}
			cache := &tmpCache{}
			_, _, _, err := puller.Pull(cmd.Context(), "owner", img, cache)
			if err != nil {
				return fmt.Errorf("failed to pull image %q: %v", img, err)
			}
			p := prop{Type: propertyType}
			if propertyType == "olm.csv.metadata" {
				p.Value, err = json.Marshal(cache.csvMetadata)
			} else if propertyType == "olm.bundle.object" {
				p.Value, err = json.Marshal(cache.bundleObject)
			} else {
				return fmt.Errorf("unknown property type: %q", propertyType)
			}
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(p)
		},
	}
	cmd.Flags().StringVarP(&propertyType, "property-type", "p", "olm.csv.metadata", "The type of property to be rendered (`olm.csv.metadata` or `olm.bundle.object`).")
	return cmd
}

type tmpCache struct {
	csvMetadata  *csvMetadata
	bundleObject *bundleObject
}

func (s tmpCache) Fetch(ctx context.Context, _ string, canonical reference.Canonical) (fs.FS, time.Time, error) {
	return nil, time.Time{}, nil
}

func (s *tmpCache) Store(ctx context.Context, _ string, named reference.Named, canonical reference.Canonical, image ocispecv1.Image, layers iter.Seq[imageutil.LayerData]) (fs.FS, time.Time, error) {
	tmpDir, err := os.MkdirTemp("", "render-csv-metadata-*")
	if err != nil {
		return nil, time.Time{}, err
	}
	defer os.RemoveAll(tmpDir)
	for layer := range layers {
		if layer.Err != nil {
			return nil, time.Time{}, layer.Err
		}
		if _, err := archive.Apply(ctx, tmpDir, layer.Reader, archive.WithFilter(func(header *tar.Header) (bool, error) {
			name := strings.TrimPrefix(header.Name, "/")
			if !strings.HasPrefix(name, "manifests/") {
				return false, nil
			}
			header.Uid = os.Getuid()
			header.Gid = os.Getgid()
			if header.FileInfo().IsDir() {
				header.Mode = 0700
			} else {
				header.Mode = 0600
			}
			return true, nil
		})); err != nil {
			return nil, time.Time{}, err
		}
	}
	if err := filepath.WalkDir(tmpDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var u unstructured.Unstructured
		if err := yaml.Unmarshal(data, &u); err != nil {
			return err
		}
		if u.GetKind() != "ClusterServiceVersion" {
			return nil
		}

		var csv v1alpha1.ClusterServiceVersion
		if err := yaml.Unmarshal(data, &csv); err != nil {
			return err
		}
		s.csvMetadata = newCsvMetadata(csv)
		s.bundleObject = &bundleObject{Data: data}

		return nil
	}); err != nil {
		return nil, time.Time{}, err
	}
	return nil, time.Time{}, nil
}

func (s tmpCache) Delete(ctx context.Context, _ string) error {
	panic("not implemented")
}

func (s tmpCache) GarbageCollect(ctx context.Context, _ string, canonical reference.Canonical) error {
	return nil
}

type prop struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type bundleObject struct {
	Data []byte `json:"data"`
}

type csvMetadata struct {
	Annotations               map[string]string                  `json:"annotations,omitempty"`
	APIServiceDefinitions     v1alpha1.APIServiceDefinitions     `json:"apiServiceDefinitions,omitempty"`
	CustomResourceDefinitions v1alpha1.CustomResourceDefinitions `json:"crdDescriptions,omitempty"`
	Description               string                             `json:"description,omitempty"`
	DisplayName               string                             `json:"displayName,omitempty"`
	InstallModes              []v1alpha1.InstallMode             `json:"installModes,omitempty"`
	Keywords                  []string                           `json:"keywords,omitempty"`
	Labels                    map[string]string                  `json:"labels,omitempty"`
	Links                     []v1alpha1.AppLink                 `json:"links,omitempty"`
	Maintainers               []v1alpha1.Maintainer              `json:"maintainers,omitempty"`
	Maturity                  string                             `json:"maturity,omitempty"`
	MinKubeVersion            string                             `json:"minKubeVersion,omitempty"`
	NativeAPIs                []metav1.GroupVersionKind          `json:"nativeAPIs,omitempty"`
	Provider                  v1alpha1.AppLink                   `json:"provider,omitempty"`
}

func newCsvMetadata(csv v1alpha1.ClusterServiceVersion) *csvMetadata {
	return &csvMetadata{
		Annotations:               csv.GetAnnotations(),
		APIServiceDefinitions:     csv.Spec.APIServiceDefinitions,
		CustomResourceDefinitions: csv.Spec.CustomResourceDefinitions,
		Description:               csv.Spec.Description,
		DisplayName:               csv.Spec.DisplayName,
		InstallModes:              csv.Spec.InstallModes,
		Keywords:                  csv.Spec.Keywords,
		Labels:                    csv.GetLabels(),
		Links:                     csv.Spec.Links,
		Maintainers:               csv.Spec.Maintainers,
		Maturity:                  csv.Spec.Maturity,
		MinKubeVersion:            csv.Spec.MinKubeVersion,
		NativeAPIs:                csv.Spec.NativeAPIs,
		Provider:                  csv.Spec.Provider,
	}
}
