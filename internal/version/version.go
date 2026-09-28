// Package version holds the binary version, stamped at build time via
// -ldflags "-X github.com/reorx/hookploy/internal/version.Version=v0.x.y".
package version

import (
	"strconv"
	"strings"
)

var Version = "dev"

// AtLeast reports whether version v is at or past release min, comparing the
// numeric vX.Y.Z core. A "dev" build is built from the source tree and
// satisfies any minimum; a version that does not parse satisfies none. A
// suffix after the core (a pre-release, or `git describe` distance) counts as
// that core — a build past v0.6.0 is not trusted to carry v0.7.0's features.
func AtLeast(v, min string) bool {
	if v == "dev" {
		return true
	}
	c, ok := Compare(v, min)
	return ok && c >= 0
}

// Compare orders a against b by their numeric vX.Y.Z core: -1, 0 or +1.
// Suffixes are ignored, so two builds of the same core compare equal. ok is
// false when either side does not parse — including "dev", which Compare
// leaves unordered, unlike AtLeast.
func Compare(a, b string) (c int, ok bool) {
	x, ok := parseCore(a)
	if !ok {
		return 0, false
	}
	y, ok := parseCore(b)
	if !ok {
		return 0, false
	}
	for i := range x {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseCore extracts [major, minor, patch] from "v1.2.3" or "1.2.3-suffix".
func parseCore(v string) ([3]int, bool) {
	var out [3]int
	core, _, _ := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
