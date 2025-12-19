package imagev2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/google/renameio/v2"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/provenance"
	"helm.sh/helm/v3/pkg/registry"
	"k8s.io/klog/v2"
)

// ProvenancePolicy controls how provenance data is handled during chart unpacking.
type ProvenancePolicy int

const (
	// ProvenanceRequired requires provenance data to be present and valid.
	// This is the default (zero value) and will fail if provenance is missing or invalid.
	ProvenanceRequired ProvenancePolicy = iota

	// ProvenanceVerifyIfPresent verifies provenance if present, but does not require it.
	ProvenanceVerifyIfPresent

	// ProvenanceIgnore skips provenance verification entirely.
	ProvenanceIgnore
)

// HelmChartHandler unpacks OCI Helm chart artifacts by extracting the chart layer.
type HelmChartHandler struct {
	// Provenance controls how provenance data is handled.
	// Default (zero value) is ProvenanceRequired, which fails if provenance is missing or invalid.
	Provenance ProvenancePolicy

	// Signatory is used for provenance verification.
	// Required when Provenance is ProvenanceRequired or ProvenanceVerifyIfPresent.
	Signatory *provenance.Signatory
}

func (h *HelmChartHandler) Name() string { return "helm.sh/chart" }

func (h *HelmChartHandler) Matches(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte) bool {
	if !isManifest(desc.MediaType) {
		return false
	}

	var manifest ocispecv1.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return false
	}

	// Helm charts use a specific config media type
	return manifest.Config.MediaType == registry.ConfigMediaType
}

func (h *HelmChartHandler) Unpack(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte, dest string) error {
	var manifest ocispecv1.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}

	meta, err := h.fetchChartMetadata(ctx, repo, manifest.Config)
	if err != nil {
		return fmt.Errorf("fetching chart metadata: %w", err)
	}

	chartFilename := fmt.Sprintf("%s-%s.tgz", meta.Name, meta.Version)
	provFilename := chartFilename + ".prov"

	var chartLayer, provLayer *ocispecv1.Descriptor
	for i := range manifest.Layers {
		layer := &manifest.Layers[i]
		switch layer.MediaType {
		case registry.ProvLayerMediaType:
			provLayer = layer
		case registry.ChartLayerMediaType:
			chartLayer = layer
		}
	}

	if chartLayer == nil {
		return errors.New("no chart layer found")
	}

	if err := h.checkProvenancePolicy(provLayer); err != nil {
		return err
	}

	if err := h.extractLayer(ctx, repo, dest, chartFilename, *chartLayer); err != nil {
		return fmt.Errorf("extracting chart layer: %w", err)
	}

	if provLayer != nil && h.Provenance != ProvenanceIgnore {
		if err := h.extractLayer(ctx, repo, dest, provFilename, *provLayer); err != nil {
			return fmt.Errorf("extracting provenance layer: %w", err)
		}
		chartPath := filepath.Join(dest, chartFilename)
		provPath := filepath.Join(dest, provFilename)
		if err := h.verifyProvenance(chartPath, provPath); err != nil {
			return err
		}
	}
	return nil
}

func (h *HelmChartHandler) checkProvenancePolicy(provLayer *ocispecv1.Descriptor) error {
	if h.Provenance == ProvenanceRequired && provLayer == nil {
		return errors.New("provenance data is required but not present")
	}
	return nil
}

func (h *HelmChartHandler) verifyProvenance(chartPath string, provenancePath string) error {
	if h.Signatory == nil {
		return errors.New("signatory is required for provenance verification")
	}

	if _, err := h.Signatory.Verify(chartPath, provenancePath); err != nil {
		return fmt.Errorf("provenance verification failed: %w", err)
	}
	return nil
}

func (h *HelmChartHandler) fetchChartMetadata(ctx context.Context, repo Repository, configDesc ocispecv1.Descriptor) (*chart.Metadata, error) {
	reader, err := repo.FetchBlob(ctx, configDesc)
	if err != nil {
		return nil, err
	}

	var meta chart.Metadata
	decodeErr := json.NewDecoder(reader).Decode(&meta)
	return &meta, errors.Join(decodeErr, reader.Close())
}

func (h *HelmChartHandler) extractLayer(ctx context.Context, repo Repository, dest, filename string, layer ocispecv1.Descriptor) error {
	l := klog.FromContext(ctx)

	reader, err := repo.FetchBlob(ctx, layer)
	if err != nil {
		return err
	}

	if err := h.writeFile(dest, filename, reader); err != nil {
		return errors.Join(err, reader.Close())
	}
	l.Info("applied layer", "mediaType", layer.MediaType, "path", dest)
	return reader.Close()
}

func (h *HelmChartHandler) writeFile(dest, filename string, reader io.Reader) error {
	path := filepath.Join(dest, filename)
	f, err := renameio.TempFile("", path)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}

	if _, err := io.Copy(f, reader); err != nil {
		return errors.Join(fmt.Errorf("writing file: %w", err), f.Cleanup())
	}
	return f.CloseAtomicallyReplace()
}
