package imagev2

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/docker/reference"

	fsutil "github.com/operator-framework/operator-controller/internal/shared/util/fs"
)

// CachingResolver provides cached unpacking with owner isolation.
// Unlike Resolver which writes to a caller-specified destination,
// CachingResolver manages its own storage location and returns an fs.FS
// to the cached content.
type CachingResolver struct {
	basePath string
	resolver *Resolver
}

// NewCachingResolver creates a caching resolver.
// Each owner's content is stored at basePath/ownerID/digest.
func NewCachingResolver(basePath string, resolver *Resolver) *CachingResolver {
	return &CachingResolver{
		basePath: basePath,
		resolver: resolver,
	}
}

// Unpack resolves and unpacks the image content for the given owner.
// If content for the same digest is already cached, it returns the cached content.
// Otherwise, it unpacks the image and caches the result.
func (c *CachingResolver) Unpack(ctx context.Context, ownerID string, repo Repository) (fs.FS, reference.Canonical, time.Time, error) {
	desc, err := repo.Resolve(ctx)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("resolving reference: %w", err)
	}

	canonicalRef, err := reference.WithDigest(reference.TrimNamed(repo.Named()), desc.Digest)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("creating canonical reference: %w", err)
	}

	unpackPath := c.unpackPath(ownerID, desc.Digest)

	// Check cache first
	if contentFS, modTime, err := c.fetchFromCache(unpackPath); err != nil {
		return nil, nil, time.Time{}, err
	} else if contentFS != nil {
		return contentFS, canonicalRef, modTime, nil
	}

	// Unpack to cache
	if err := c.unpackToCache(ctx, repo, unpackPath); err != nil {
		return nil, nil, time.Time{}, err
	}

	modTime, err := fsutil.GetDirectoryModTime(unpackPath)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("getting mod time: %w", err)
	}

	return os.DirFS(unpackPath), canonicalRef, modTime, nil
}

func (c *CachingResolver) fetchFromCache(unpackPath string) (fs.FS, time.Time, error) {
	modTime, err := fsutil.GetDirectoryModTime(unpackPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, nil
	}
	if errors.Is(err, fsutil.ErrNotDirectory) {
		if err := fsutil.DeleteReadOnlyRecursive(unpackPath); err != nil {
			return nil, time.Time{}, fmt.Errorf("cleaning invalid cache entry: %w", err)
		}
		return nil, time.Time{}, nil
	}
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("checking cache: %w", err)
	}

	return os.DirFS(unpackPath), modTime, nil
}

func (c *CachingResolver) unpackToCache(ctx context.Context, repo Repository, unpackPath string) error {
	if err := fsutil.EnsureEmptyDirectory(unpackPath, 0700); err != nil {
		return fmt.Errorf("preparing cache directory: %w", err)
	}

	if err := c.resolver.Unpack(ctx, repo, unpackPath); err != nil {
		_ = fsutil.DeleteReadOnlyRecursive(unpackPath)
		return err
	}

	if err := fsutil.SetReadOnlyRecursive(unpackPath); err != nil {
		return fmt.Errorf("making cache read-only: %w", err)
	}

	return nil
}

// Delete removes all cached content for the given owner.
func (c *CachingResolver) Delete(_ context.Context, ownerID string) error {
	return fsutil.DeleteReadOnlyRecursive(c.ownerPath(ownerID))
}

// GarbageCollect removes cached content for the owner, keeping only the specified digest.
// If keep is empty, all content is removed.
func (c *CachingResolver) GarbageCollect(_ context.Context, ownerID string, keep digest.Digest) error {
	ownerPath := c.ownerPath(ownerID)
	entries, err := os.ReadDir(ownerPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading owner directory: %w", err)
	}

	foundKeep := false
	entries = slices.DeleteFunc(entries, func(entry os.DirEntry) bool {
		if entry.Name() == keep.String() {
			foundKeep = true
			return true
		}
		return false
	})

	for _, entry := range entries {
		if err := fsutil.DeleteReadOnlyRecursive(filepath.Join(ownerPath, entry.Name())); err != nil {
			return fmt.Errorf("removing %s: %w", entry.Name(), err)
		}
	}

	if !foundKeep {
		return fsutil.DeleteReadOnlyRecursive(ownerPath)
	}
	return nil
}

func (c *CachingResolver) ownerPath(ownerID string) string {
	return filepath.Join(c.basePath, ownerID)
}

func (c *CachingResolver) unpackPath(ownerID string, d digest.Digest) string {
	return filepath.Join(c.ownerPath(ownerID), d.String())
}
