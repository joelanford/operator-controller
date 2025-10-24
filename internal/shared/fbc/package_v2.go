package fbc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	bsemver "github.com/blang/semver/v4"
)

const (
	SchemaPackageV2 = "olm.package.v2"
)

type PackageV2 struct {
	Schema         string           `json:"schema"`
	Package        string           `json:"package"`
	VersionStreams []VersionStream  `json:"versionStreams"`
	Bundles        []Bundle         `json:"bundles"`
	Retractions    []VersionRelease `json:"retractions"`
}

type Bundle struct {
	Image          string   `json:"image"`
	RelatedImages  []string `json:"relatedImages"`
	VersionRelease `json:",inline"`
	ReleasedAt     time.Time `json:"releasedAt"`
}

type VersionRelease struct {
	Version bsemver.Version `json:"version"`
	Release Release         `json:"release"`
}

func (vr VersionRelease) String() string {
	var sb strings.Builder
	sb.WriteString(vr.Version.String())
	if relStr := vr.Release.String(); relStr != "" {
		sb.WriteString("-" + relStr)
	}
	return sb.String()
}

func (vr VersionRelease) Compare(other VersionRelease) int {
	if v := vr.Version.Compare(other.Version); v != 0 {
		return v
	}
	return vr.Release.Compare(other.Release)
}

type Release []bsemver.PRVersion

func (r Release) String() string {
	if len(r) == 0 {
		return ""
	}
	var buf bytes.Buffer
	_, _ = fmt.Fprintf(&buf, r[0].String())
	for _, seg := range r[1:] {
		_, _ = fmt.Fprintf(&buf, ".%s", seg.String())
	}
	return buf.String()
}

func (r Release) Compare(other Release) int {
	switch {
	case r == nil && other != nil:
		return -1
	case r != nil && other == nil:
		return 1
	}
	rV := bsemver.Version{Pre: r}
	otherV := bsemver.Version{Pre: other}
	return rV.Compare(otherV)
}

func (r *Release) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	tmp, err := NewRelease(str)
	if err != nil {
		return err
	}
	*r = tmp
	return nil
}

func (r Release) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.String())
}

func NewRelease(str string) (Release, error) {
	if str == "" {
		return nil, nil
	}
	var (
		segments = strings.Split(str, ".")
		rel      = make([]bsemver.PRVersion, 0, len(segments))
		errs     []error
	)
	for i, segment := range segments {
		prVer, err := bsemver.NewPRVersion(segment)
		if err != nil {
			errs = append(errs, fmt.Errorf("segment[%d] %q: %v", i, segment, err))
		}
		rel = append(rel, prVer)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid release: %v", err)
	}
	return rel, nil
}
