package bundleutil_test

import (
	"encoding/json"
	"testing"

	bsemver "github.com/blang/semver/v4"
	"github.com/stretchr/testify/require"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
	"github.com/operator-framework/operator-registry/alpha/property"

	"github.com/operator-framework/operator-controller/internal/operator-controller/bundle"
	"github.com/operator-framework/operator-controller/internal/operator-controller/bundleutil"
)

func TestGetVersionAndRelease(t *testing.T) {
	tests := []struct {
		name               string
		pkgProperty        *property.Property
		wantVersionRelease *bundle.VersionRelease
		wantErr            bool
	}{
		{
			name: "valid version with build metadata as legacy release",
			pkgProperty: &property.Property{
				Type:  property.TypePackage,
				Value: json.RawMessage(`{"version": "1.0.0-pre+1.alpha.2"}`),
			},
			wantVersionRelease: &bundle.VersionRelease{
				Version: bsemver.MustParse("1.0.0-pre"),
				Release: bundle.Release([]bsemver.PRVersion{
					{VersionNum: 1, IsNum: true},
					{VersionStr: "alpha"},
					{VersionNum: 2, IsNum: true},
				}),
			},
			wantErr: false,
		},
		{
			name: "explicit release field is preferred over build metadata",
			pkgProperty: &property.Property{
				Type:  property.TypePackage,
				Value: json.RawMessage(`{"version": "1.0.0+99", "release": "3"}`),
			},
			wantVersionRelease: &bundle.VersionRelease{
				Version: bsemver.MustParse("1.0.0+99"),
				Release: bundle.Release([]bsemver.PRVersion{
					{VersionNum: 3, IsNum: true},
				}),
			},
			wantErr: false,
		},
		{
			name: "explicit release field with no build metadata",
			pkgProperty: &property.Property{
				Type:  property.TypePackage,
				Value: json.RawMessage(`{"version": "2.0.0", "release": "1.2"}`),
			},
			wantVersionRelease: &bundle.VersionRelease{
				Version: bsemver.MustParse("2.0.0"),
				Release: bundle.Release([]bsemver.PRVersion{
					{VersionNum: 1, IsNum: true},
					{VersionNum: 2, IsNum: true},
				}),
			},
			wantErr: false,
		},
		{
			name: "invalid version",
			pkgProperty: &property.Property{
				Type:  property.TypePackage,
				Value: json.RawMessage(`{"version": "abcd"}`),
			},
			wantErr: true,
		},
		{
			name: "invalid release - build metadata with leading zeros",
			pkgProperty: &property.Property{
				Type:  property.TypePackage,
				Value: json.RawMessage(`{"version": "1.0.0+001"}`),
			},
			wantVersionRelease: &bundle.VersionRelease{
				Version: bsemver.MustParse("1.0.0+001"),
			},
			wantErr: false,
		},
		{
			name: "invalid explicit release field",
			pkgProperty: &property.Property{
				Type:  property.TypePackage,
				Value: json.RawMessage(`{"version": "1.0.0", "release": "01"}`),
			},
			wantErr: true,
		},
		{
			name: "invalid json",
			pkgProperty: &property.Property{
				Type:  property.TypePackage,
				Value: json.RawMessage(`abcd`),
			},
			wantErr: true,
		},
		{
			name:        "no version property",
			pkgProperty: nil,
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			properties := make([]property.Property, 0)
			if tc.pkgProperty != nil {
				properties = append(properties, *tc.pkgProperty)
			}

			bundle := declcfg.Bundle{
				Name:       "test-bundle",
				Properties: properties,
			}

			vr, err := bundleutil.GetVersionAndRelease(bundle)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				if tc.wantVersionRelease != nil {
					require.Equal(t, tc.wantVersionRelease, vr)
				}
			}
		})
	}
}
