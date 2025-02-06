package source_test

import (
	"context"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/pkg/sysregistriesv2"
	"github.com/containers/image/v5/types"
	"github.com/go-logr/logr"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/opencontainers/go-digest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/operator-framework/operator-controller/internal/rukpak/source"
	fsutil "github.com/operator-framework/operator-controller/internal/util/fs"
	imageutil "github.com/operator-framework/operator-controller/internal/util/image"
)

const (
	testFileName     string = "test-file"
	testFileContents string = "test-content"
)

func TestUnpackValidInsecure(t *testing.T) {
	imageTagRef, _, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageTagRef),
		},
	}

	bundleID := "test-bundle"
	bundleRef := imageTagRef.String()

	oldBundlePath := filepath.Join(basePath, bundleID, "old")
	err := os.MkdirAll(oldBundlePath, 0755)
	require.NoError(t, err)

	// Attempt to pull and unpack the image
	result, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, source.StateUnpacked, result.State)

	require.NoDirExists(t, oldBundlePath)

	unpackedFile, err := fs.ReadFile(result.Bundle, testFileName)
	require.NoError(t, err)
	// Ensure the unpacked file matches the source content
	assert.Equal(t, []byte(testFileContents), unpackedFile)
	assert.NoError(t, unpacker.Cleanup(context.Background(), bundleID))
}

func TestUnpackValidUsesCache(t *testing.T) {
	_, imageDigestRef, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageDigestRef),
		},
	}

	bundleID := "test-bundle"
	bundleRef := imageDigestRef.String()

	// Populate the bundle cache with a folder that is not actually part of the image
	testCacheFilePath := filepath.Join(basePath, bundleID, imageDigestRef.Digest().String(), "test-folder")
	require.NoError(t, os.MkdirAll(testCacheFilePath, 0700))

	// Attempt to pull and unpack the image
	result, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, source.StateUnpacked, result.State)

	// Make sure the original contents of the cache are still present. If the cached contents
	// were not used, we would expect the original contents to be removed.
	assert.DirExists(t, testCacheFilePath)
	assert.NoError(t, unpacker.Cleanup(context.Background(), bundleID))
}

func TestUnpackCacheCheckError(t *testing.T) {
	imageTagRef, imageDigestRef, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageTagRef),
		},
	}

	bundleID := "test-bundle"
	bundleRef := imageTagRef.String()

	// Create the unpack path and restrict its permissions
	unpackPath := filepath.Join(basePath, bundleID, imageDigestRef.Digest().String())
	require.NoError(t, os.MkdirAll(unpackPath, os.ModePerm))
	require.NoError(t, os.Chmod(basePath, 0000))
	defer func() {
		require.NoError(t, os.Chmod(basePath, 0755))
	}()

	// Attempt to pull and unpack the image
	_, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	assert.ErrorContains(t, err, "permission denied")
}

func TestUnpackNameOnlyImageReference(t *testing.T) {
	imageTagRef, _, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageTagRef),
		},
	}

	bundleID := "test-bundle"
	bundleRef := reference.TrimNamed(imageTagRef).String()

	// Attempt to pull and unpack the image
	_, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	require.ErrorContains(t, err, "tag or digest is needed")
	assert.ErrorIs(t, err, reconcile.TerminalError(nil))
}

func TestUnpackUnservedTaggedImageReference(t *testing.T) {
	imageTagRef, _, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageTagRef),
		},
	}

	bundleID := "test-bundle"
	bundleRef := fmt.Sprintf("%s:unserved-tag", reference.TrimNamed(imageTagRef))

	// Attempt to pull and unpack the image
	_, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	assert.ErrorContains(t, err, "manifest unknown")
}

func TestUnpackUnservedCanonicalImageReference(t *testing.T) {
	_, imageDigestRef, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageDigestRef),
		},
	}

	bundleID := "test-bundle"
	origRef := imageDigestRef.String()
	bundleRef := origRef[:len(origRef)-1] + "1"

	// Attempt to pull and unpack the image
	_, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	assert.ErrorContains(t, err, "manifest unknown")
}

func TestUnpackInvalidImageRef(t *testing.T) {
	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: func(context.Context) (*types.SystemContext, error) {
				return &types.SystemContext{}, nil
			},
		},
	}

	bundleID := "test-bundle"
	bundleRef := "invalid image ref"

	// Attempt to unpack
	result, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	assert.Nil(t, result)
	require.ErrorContains(t, err, "error parsing image reference")
	require.ErrorIs(t, err, reconcile.TerminalError(nil))
	assert.NoDirExists(t, filepath.Join(basePath, bundleID))
}

func TestUnpackUnexpectedFile(t *testing.T) {
	imageTagRef, imageDigestRef, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageTagRef),
		},
	}

	bundleID := "test-bundle"
	bundleRef := imageTagRef.String()

	// Create an unpack path that is a file
	unpackPath := filepath.Join(basePath, bundleID, imageDigestRef.Digest().String())
	require.NoError(t, os.MkdirAll(filepath.Dir(unpackPath), 0700))
	require.NoError(t, os.WriteFile(unpackPath, []byte{}, 0600))

	log.SetLogger(logr.Discard())
	// Attempt to pull and unpack the image
	_, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	require.NoError(t, err)

	// Ensure unpack path is now a directory
	stat, err := os.Stat(unpackPath)
	require.NoError(t, err)
	require.True(t, stat.IsDir())

	// Unset read-only to allow cleanup
	require.NoError(t, fsutil.SetWritableRecursive(unpackPath))
}

func TestUnpackCopySucceedsMountFails(t *testing.T) {
	imageTagRef, _, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageTagRef),
		},
	}

	bundleID := "test-bundle"
	bundleRef := imageTagRef.String()

	// Create an unpack path that is a non-writable directory
	bundleDir := filepath.Join(basePath, bundleID)
	require.NoError(t, os.MkdirAll(bundleDir, 0000))

	// Attempt to pull and unpack the image
	_, err := unpacker.Unpack(context.Background(), bundleID, bundleRef)
	assert.ErrorContains(t, err, "permission denied")
}

func TestCleanup(t *testing.T) {
	imageTagRef, _, cleanup := setupRegistry(t)
	defer cleanup()

	basePath := t.TempDir()
	unpacker := &source.ContainersImageRegistry{
		Cache: source.BundleCache(basePath),
		Puller: &imageutil.ContainersImagePuller{
			SourceCtxFunc: buildPullContextfunc(t, imageTagRef),
		},
	}

	bundleID := "test-bundle"

	// Create an unpack path for the bundle
	bundleDir := filepath.Join(basePath, bundleID)
	require.NoError(t, os.MkdirAll(bundleDir, 0500))

	// Clean up the bundle
	err := unpacker.Cleanup(context.Background(), bundleID)
	require.NoError(t, err)
	assert.NoDirExists(t, bundleDir)
}

func setupRegistry(t *testing.T) (reference.NamedTagged, reference.Canonical, func()) {
	server := httptest.NewServer(registry.New())
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	// Generate an image with file contents
	img, err := crane.Image(map[string][]byte{testFileName: []byte(testFileContents)})
	require.NoError(t, err)

	imageTagRef, err := newReference(serverURL.Host, "test-repo/test-image", "test-tag")
	require.NoError(t, err)

	imgDigest, err := img.Digest()
	require.NoError(t, err)

	imageDigestRef, err := reference.WithDigest(reference.TrimNamed(imageTagRef), digest.Digest(imgDigest.String()))
	require.NoError(t, err)

	require.NoError(t, crane.Push(img, imageTagRef.String()))

	cleanup := func() {
		server.Close()
	}
	return imageTagRef, imageDigestRef, cleanup
}

func newReference(host, repo, tag string) (reference.NamedTagged, error) {
	ref, err := reference.ParseNamed(fmt.Sprintf("%s/%s", host, repo))
	if err != nil {
		return nil, err
	}
	return reference.WithTag(ref, tag)
}

func buildPullContextfunc(t *testing.T, ref reference.Named) func(context.Context) (*types.SystemContext, error) {
	return func(ctx context.Context) (*types.SystemContext, error) {
		// Build a containers/image context that allows pulling from the test registry insecurely
		registriesConf := sysregistriesv2.V2RegistriesConf{Registries: []sysregistriesv2.Registry{
			{
				Prefix: reference.Domain(ref),
				Endpoint: sysregistriesv2.Endpoint{
					Location: reference.Domain(ref),
					Insecure: true,
				},
			},
		}}
		configDir := t.TempDir()
		registriesConfPath := filepath.Join(configDir, "registries.conf")
		f, err := os.Create(registriesConfPath)
		require.NoError(t, err)

		enc := toml.NewEncoder(f)
		require.NoError(t, enc.Encode(registriesConf))
		require.NoError(t, f.Close())

		return &types.SystemContext{
			SystemRegistriesConfPath: registriesConfPath,
		}, nil
	}
}
