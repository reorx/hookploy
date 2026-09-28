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

// Behavior: Compare orders two versions by their numeric core and reports
// whether an order exists at all. Suffixes are ignored, so versions sharing a
// core compare equal even when the strings differ. "dev" and anything else
// that does not parse as vX.Y.Z has no place in the order — unlike AtLeast,
// Compare does not treat a dev build as newest.
func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v0.7.0", "v0.7.0", 0, true},
		{"v0.6.0", "v0.7.0", -1, true},
		{"v0.8.0", "v0.7.0", 1, true},
		{"v0.7.1", "v0.7.0", 1, true},
		{"v0.10.0", "v0.9.0", 1, true},
		{"v1.0.0", "v0.99.99", 1, true},
		{"v0.7.0-rc.1", "v0.7.0", 0, true},
		{"v0.7.0-3-gabc1234-dirty", "v0.7.0", 0, true},
		{"0.7.0", "v0.7.0", 0, true},
		{"dev", "v0.7.0", 0, false},
		{"v0.7.0", "dev", 0, false},
		{"", "v0.7.0", 0, false},
		{"345200d", "v0.7.0", 0, false},
		{"v0.7", "v0.7.0", 0, false},
	}
	for _, c := range cases {
		got, ok := Compare(c.a, c.b)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("Compare(%q, %q) = %d, %v; want %d, %v", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
