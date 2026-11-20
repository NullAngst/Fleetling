// Package fleetling holds the version, read from the VERSION file at the
// top of the repository so every build carries it: release binaries and
// images built straight from git alike.
//
// The numbering: each release bumps the middle number, 1.0.0, 1.1.0, ...
// 1.9.0, and after .9 the first number goes up and the middle one starts
// over: 2.0.0. The last number stays 0. Bump VERSION before making the
// release; the release workflow refuses a tag that doesn't match it.
package fleetling

import (
	_ "embed"
	"fmt"
	"strconv"
	"strings"
)

//go:embed VERSION
var raw string

// Version is this build's version, like 1.4.0.
func Version() string { return strings.TrimSpace(raw) }

// Ver is a parsed version.
type Ver struct{ Major, Minor, Patch int }

func (v Ver) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Parse reads 1.4.0, with or without a leading v or V, as release tags
// sometimes have one.
func Parse(s string) (Ver, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Ver{}, fmt.Errorf("%q is not a version like 1.4.0", s)
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 || p != strconv.Itoa(x) {
			return Ver{}, fmt.Errorf("%q is not a version like 1.4.0", s)
		}
		n[i] = x
	}
	return Ver{n[0], n[1], n[2]}, nil
}

// Valid reports whether v follows the numbering: a middle number from 0 to
// 9 and a last number of 0. Versions from before the numbering (0.9.1)
// still parse and compare; they just aren't valid for a new release.
func (v Ver) Valid() error {
	if v.Major < 1 || v.Minor > 9 || v.Patch != 0 {
		return fmt.Errorf("%s doesn't follow the numbering: the first number at least 1, the middle 0 to 9, the last 0", v)
	}
	return nil
}

// Next is the version after v: 1.4.0 to 1.5.0, 1.9.0 to 2.0.0.
func (v Ver) Next() Ver {
	if v.Minor >= 9 {
		return Ver{v.Major + 1, 0, 0}
	}
	return Ver{v.Major, v.Minor + 1, 0}
}

// Less reports whether v is older than o.
func (v Ver) Less(o Ver) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}
