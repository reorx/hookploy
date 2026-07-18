package token

import (
	"regexp"
	"testing"
)

// Behavior: tokens carry a kind prefix + 43 chars of base64url randomness;
// only the SHA-256 hash is ever stored; comparison is constant time.
func TestNewAndHash(t *testing.T) {
	cases := []struct {
		kind   Kind
		prefix string
	}{
		{KindService, "hpt_"},
		{KindServer, "hps_"},
		{KindAdmin, "hpa_"},
	}
	for _, c := range cases {
		plain := New(c.kind)
		if !regexp.MustCompile(`^` + c.prefix + `[A-Za-z0-9_-]{43}$`).MatchString(plain) {
			t.Fatalf("%s token %q malformed", c.kind, plain)
		}
		h := Hash(plain)
		if len(h) != 64 { // sha256 hex
			t.Fatalf("hash %q not sha256 hex", h)
		}
		if !Equal(h, Hash(plain)) {
			t.Fatal("Equal(hash, hash) must be true")
		}
		if Equal(h, Hash(New(c.kind))) {
			t.Fatal("different tokens must not compare equal")
		}
	}
	if New(KindService) == New(KindService) {
		t.Fatal("tokens must be unique")
	}
}
