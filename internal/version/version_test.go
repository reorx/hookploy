package version

import "testing"

// Behavior: AtLeast compares release versions by their numeric core. A dev
// build (built from the source tree) satisfies any minimum; anything that
// does not parse as vX.Y.Z satisfies none. A suffix after the core — a
// pre-release, or `git describe` distance — counts as that core: a build
// past v0.6.0 may or may not carry v0.7.0's features, so it is not trusted to.
func TestAtLeast(t *testing.T) {
	cases := []struct {
		v, min string
		want   bool
	}{
		{"v0.7.0", "v0.7.0", true},
		{"v0.7.1", "v0.7.0", true},
		{"v0.10.0", "v0.7.0", true},
		{"v1.0.0", "v0.7.0", true},
		{"v0.6.9", "v0.7.0", false},
		{"v0.6.0", "v0.7.0", false},
		{"v0.6.0-3-g345200d-dirty", "v0.7.0", false},
		{"v0.7.0-rc.1", "v0.7.0", true},
		{"0.7.0", "v0.7.0", true},
		{"dev", "v0.7.0", true},
		{"", "v0.7.0", false},
		{"345200d", "v0.7.0", false},
		{"v0.7", "v0.7.0", false},
	}
	for _, c := range cases {
		if got := AtLeast(c.v, c.min); got != c.want {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", c.v, c.min, got, c.want)
		}
	}
}
