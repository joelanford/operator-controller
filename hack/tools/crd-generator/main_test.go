package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/require"
)

func TestRunGenerator(t *testing.T) {
	here, err := os.Getwd()
	require.NoError(t, err)
	// Get to repo root
	err = os.Chdir("../../..")
	require.NoError(t, err)
	defer func() {
		_ = os.Chdir(here)
	}()
	dir, err := os.MkdirTemp("", "crd-generate-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "standard"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "experimental"), 0o700))
	runGenerator(dir, "v0.18.0")

	f1 := filepath.Join(dir, "standard/olm.operatorframework.io_clusterextensions.yaml")
	f2 := "config/base/operator-controller/crd/standard/olm.operatorframework.io_clusterextensions.yaml"
	fmt.Printf("comparing: %s to %s\n", f1, f2)
	compareFiles(t, f1, f2)

	f1 = filepath.Join(dir, "standard/olm.operatorframework.io_clustercatalogs.yaml")
	f2 = "config/base/catalogd/crd/standard/olm.operatorframework.io_clustercatalogs.yaml"
	fmt.Printf("comparing: %s to %s\n", f1, f2)
	compareFiles(t, f1, f2)

	f1 = filepath.Join(dir, "experimental/olm.operatorframework.io_clusterextensions.yaml")
	f2 = "config/base/operator-controller/crd/experimental/olm.operatorframework.io_clusterextensions.yaml"
	fmt.Printf("comparing: %s to %s\n", f1, f2)
	compareFiles(t, f1, f2)

	f1 = filepath.Join(dir, "experimental/olm.operatorframework.io_clustercatalogs.yaml")
	f2 = "config/base/catalogd/crd/experimental/olm.operatorframework.io_clustercatalogs.yaml"
	fmt.Printf("comparing: %s to %s\n", f1, f2)
	compareFiles(t, f1, f2)
}

func TestTags(t *testing.T) {
	here, err := os.Getwd()
	require.NoError(t, err)
	err = os.Chdir("testdata")
	defer func() {
		_ = os.Chdir(here)
	}()
	require.NoError(t, err)
	dir, err := os.MkdirTemp("", "crd-generate-*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "standard"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "experimental"), 0o700))
	runGenerator(dir, "v0.18.0", "github.com/operator-framework/operator-controller/hack/tools/crd-generator/testdata/api/v1")

	f1 := filepath.Join(dir, "standard/olm.operatorframework.io_clusterextensions.yaml")
	f2 := "output/standard/olm.operatorframework.io_clusterextensions.yaml"
	fmt.Printf("comparing: %s to %s\n", f1, f2)
	compareFiles(t, f1, f2)

	f1 = filepath.Join(dir, "experimental/olm.operatorframework.io_clusterextensions.yaml")
	f2 = "output/experimental/olm.operatorframework.io_clusterextensions.yaml"
	fmt.Printf("comparing: %s to %s\n", f1, f2)
	compareFiles(t, f1, f2)
}

func compareFiles(t *testing.T, file1, file2 string) {
	f1, err := os.ReadFile(file1)
	require.NoError(t, err)

	f2, err := os.ReadFile(file2)
	require.NoError(t, err)

	// Make the multi-line string diff output pretty!
	lineTransformer := cmpopts.AcyclicTransformer("SplitLines", func(s string) []string {
		return strings.Split(s, "\n")
	})

	require.Empty(t, cmp.Diff(string(f1), string(f2), lineTransformer))
}
