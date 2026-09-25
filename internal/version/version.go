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
	have, ok := parseCore(v)
	if !ok {
		return false
	}
	want, ok := parseCore(min)
	if !ok {
		return false
	}
	for i := range have {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
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
