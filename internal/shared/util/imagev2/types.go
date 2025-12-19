package imagev2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/google/renameio/v2"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/manifest"
)

// Repository is an image repository client that provides OCI registry access.
type Repository interface {
	// Named returns the reference of the repository. It must implement reference.NamedTagged or reference.Canonical.
	Named() reference.Named

	// Resolve gets the canonical digest for a reference
	Resolve(ctx context.Context) (ocispecv1.Descriptor, error)

	// FetchManifest fetches raw manifest bytes and media type
	FetchManifest(ctx context.Context, desc ocispecv1.Descriptor) ([]byte, string, error)

	// FetchBlob fetches a blob by digest
	FetchBlob(ctx context.Context, desc ocispecv1.Descriptor) (io.ReadCloser, error)

	// Close releases any resources held by the repository.
	Close() error
}

type CachingRepository struct {
	inner Repository

	// Local storage
	cacheDir string

	// Parsed content cache
	resolution *ocispecv1.Descriptor
}

func NewCachingRepository(client Repository) (*CachingRepository, error) {
	cacheDir, err := os.MkdirTemp("", "oci-session-")
	if err != nil {
		return nil, err
	}

	return &CachingRepository{
		inner:    client,
		cacheDir: cacheDir,
	}, nil
}

func (s *CachingRepository) Named() reference.Named {
	return s.inner.Named()
}

func (s *CachingRepository) Close() error {
	return errors.Join(s.inner.Close(), os.RemoveAll(s.cacheDir))
}

func (s *CachingRepository) manifestsDir() string {
	return filepath.Join(s.cacheDir, "manifests")
}

func (s *CachingRepository) blobsDir() string {
	return filepath.Join(s.cacheDir, "blobs")
}

func (s *CachingRepository) Resolve(ctx context.Context) (ocispecv1.Descriptor, error) {
	if s.resolution != nil {
		return *s.resolution, nil
	}

	desc, err := s.inner.Resolve(ctx)
	if err != nil {
		return ocispecv1.Descriptor{}, err
	}
	s.resolution = &desc
	return desc, nil
}

func (s *CachingRepository) FetchManifest(ctx context.Context, desc ocispecv1.Descriptor) ([]byte, string, error) {
	manifestsDir := s.manifestsDir()
	manifestPath := filepath.Join(manifestsDir, desc.Digest.String())
	manifestBytes, err := os.ReadFile(manifestPath)
	if err == nil {
		return manifestBytes, manifest.GuessMIMEType(manifestBytes), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}

	// Fetch raw manifest
	raw, mediaType, err := s.inner.FetchManifest(ctx, desc)
	if err != nil {
		return nil, "", err
	}
	desc.MediaType = mediaType

	f, err := s.cacheFile(manifestPath, bytes.NewReader(raw))
	if err != nil {
		return nil, "", err
	}
	return raw, mediaType, f.Close()
}

func (s *CachingRepository) FetchBlob(ctx context.Context, desc ocispecv1.Descriptor) (io.ReadCloser, error) {
	blobDir := s.blobsDir()
	blobPath := filepath.Join(blobDir, desc.Digest.String())

	// Check cache
	if f, err := os.Open(blobPath); err == nil {
		return f, nil
	}

	// Fetch and cache
	reader, err := s.inner.FetchBlob(ctx, desc)
	if err != nil {
		return nil, err
	}

	f, err := s.cacheFile(blobPath, reader)
	return f, errors.Join(err, reader.Close())
}

func (s *CachingRepository) cacheFile(path string, reader io.Reader) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := renameio.TempFile("", path)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, reader); err != nil {
		return nil, errors.Join(err, f.Cleanup())
	}
	if err := f.CloseAtomicallyReplace(); err != nil {
		return nil, err
	}
	return os.Open(path)
}

func ParseContent(desc ocispecv1.Descriptor, raw []byte) (*Content, error) {
	content := &Content{
		Descriptor: desc,
	}

	switch {
	case isIndex(desc.MediaType):
		var idx ocispecv1.Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return nil, err
		}
		content.Index = &idx

	case isManifest(desc.MediaType):
		var man ocispecv1.Manifest
		if err := json.Unmarshal(raw, &man); err != nil {
			return nil, err
		}
		content.Manifest = &man

	default:
		return nil, fmt.Errorf("unknown media type: %s", desc.MediaType)
	}

	return content, nil
}

type Content struct {
	ocispecv1.Descriptor

	Index    *ocispecv1.Index
	Manifest *ocispecv1.Manifest
}

func isIndex(mediaType string) bool {
	return mediaType == ocispecv1.MediaTypeImageIndex || mediaType == manifest.DockerV2ListMediaType
}

func isManifest(mediaType string) bool {
	return mediaType == ocispecv1.MediaTypeImageManifest || mediaType == manifest.DockerV2Schema2MediaType
}

// Handler knows how to unpack a specific type of OCI content.
type Handler interface {
	// Name returns a human-readable name for logging and debugging.
	Name() string

	// Matches returns true if this handler can handle the content.
	// Handlers may use the session to fetch additional content (e.g., config blob)
	// to make matching decisions. The session caches fetched content, so repeated
	// fetches across handlers are efficient.
	Matches(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte) bool

	// Unpack processes the content and writes the unpacked content to dest.
	Unpack(ctx context.Context, repo Repository, desc ocispecv1.Descriptor, manifestBytes []byte, dest string) error
}

// RepositoryFactory creates a Session for a given image reference.
type RepositoryFactory func(ctx context.Context, ref string) (Repository, error)

// Resolver holds handlers and unpacks OCI content.
type Resolver struct {
	handlers []Handler
}

// NewResolver creates a new resolver.
func NewResolver() *Resolver {
	return &Resolver{}
}

// Register adds a handler to the resolver. Handlers are tried in registration
// order, so register higher-priority handlers first.
func (r *Resolver) Register(h Handler) {
	r.handlers = append(r.handlers, h)
}

// Unpack finds the first matching handler and unpacks content to the destination.
// The caller is responsible for creating and closing the session.
func (r *Resolver) Unpack(ctx context.Context, repo Repository, dest string) error {
	desc, err := repo.Resolve(ctx)
	if err != nil {
		return err
	}

	manifestBytes, mediaType, err := repo.FetchManifest(ctx, desc)
	if err != nil {
		return err
	}
	desc.MediaType = mediaType

	for _, handler := range r.handlers {
		if handler.Matches(ctx, repo, desc, manifestBytes) {
			if err := handler.Unpack(ctx, repo, desc, manifestBytes, dest); err != nil {
				return fmt.Errorf("handler %s: %w", handler.Name(), err)
			}
			return nil
		}
	}

	return fmt.Errorf("no handler matched content (mediaType=%s, digest=%s)", mediaType, desc.Digest)
}
