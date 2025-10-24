package fbc

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"github.com/blang/semver/v4"
)

type MajorMinor struct {
	Major uint64 `json:"major"`
	Minor uint64 `json:"minor"`
}

func NewMajorMinorFromVersion(v semver.Version) MajorMinor {
	return MajorMinor{Major: v.Major, Minor: v.Minor}
}

func NewMajorMinorFromString(s string) (MajorMinor, error) {
	matches := majorMinorRegexp.FindStringSubmatch(s)
	if len(matches) == 0 {
		return MajorMinor{}, fmt.Errorf("invalid version %q; expected <major>.<minor>", s)
	}
	if len(matches) != 3 {
		panic("programmer error: expected 2 submatches")
	}

	major, _ := strconv.ParseUint(matches[1], 10, 64)
	minor, _ := strconv.ParseUint(matches[2], 10, 64)
	return MajorMinor{
		Major: major,
		Minor: minor,
	}, nil
}

func (v MajorMinor) String() string {
	return fmt.Sprintf("%d.%d", v.Major, v.Minor)
}

func (v MajorMinor) MarshalJSON() ([]byte, error) {
	return json.Marshal(v.String())
}

func (v MajorMinor) Compare(other MajorMinor) int {
	if v := cmp.Compare(v.Major, other.Major); v != 0 {
		return v
	}
	return cmp.Compare(v.Minor, other.Minor)
}

const majorMinorPattern = `^(0|[1-9]\d*)\.(0|[1-9]\d*)$`

var majorMinorRegexp = regexp.MustCompile(majorMinorPattern)

func (v *MajorMinor) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	mm, err := NewMajorMinorFromString(s)
	if err != nil {
		return err
	}
	*v = mm
	return nil
}
