package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

func writeConfig(t *testing.T) (dir, cfgPath string) {
	t.Helper()
	dir = t.TempDir()
	cfgPath = filepath.Join(dir, "hookploy.yaml")
	yaml := `
servers:
  s1: { local: true }
services:
  linkmind:
    server: s1
    dir: /opt/apps/linkmind
    deploy: [compose.up]
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, cfgPath
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// Behavior: `token create <service>` prints the plaintext once (hpt_ prefix)
// and stores only the hash; the token then resolves via the store.
func TestTokenCreateRotateRevoke(t *testing.T) {
	dir, cfg := writeConfig(t)

	code, out, errOut := runCLI(t, "token", "create", "linkmind", "-f", cfg)
	if code != 0 {
		t.Fatalf("create exit %d: %s", code, errOut)
	}
	plain := strings.TrimSpace(out)
	if !strings.HasPrefix(plain, "hpt_") {
		t.Fatalf("stdout should be the hpt_ token, got %q", out)
	}

	s, err := store.Open(filepath.Join(dir, "hookploy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rec, _ := s.LookupToken(token.Hash(plain))
	if rec == nil || rec.Subject != "linkmind" {
		t.Fatalf("token not stored: %+v", rec)
	}

	// rotate: prints a new token, old one dies
	code, out2, _ := runCLI(t, "token", "rotate", "linkmind", "-f", cfg)
	if code != 0 {
		t.Fatal("rotate failed")
	}
	plain2 := strings.TrimSpace(out2)
	if rec, _ := s.LookupToken(token.Hash(plain)); rec != nil {
		t.Fatal("old token should be revoked after rotate")
	}
	if rec, _ := s.LookupToken(token.Hash(plain2)); rec == nil {
		t.Fatal("rotated token should be valid")
	}

	// revoke: no valid token left
	code, _, _ = runCLI(t, "token", "revoke", "linkmind", "-f", cfg)
	if code != 0 {
		t.Fatal("revoke failed")
	}
	if rec, _ := s.LookupToken(token.Hash(plain2)); rec != nil {
		t.Fatal("token should be revoked")
	}
}

// Behavior: tokens are only issued for services the config knows about.
func TestTokenCreateUnknownService(t *testing.T) {
	_, cfg := writeConfig(t)
	code, _, errOut := runCLI(t, "token", "create", "ghost", "-f", cfg)
	if code == 0 || !strings.Contains(errOut, "ghost") {
		t.Fatalf("expected unknown-service failure, got exit %d, stderr %s", code, errOut)
	}
}

// Behavior: server tokens use hps_, admin tokens hpa_.
func TestServerAndAdminTokens(t *testing.T) {
	_, cfg := writeConfig(t)
	code, out, errOut := runCLI(t, "server", "token", "create", "s1", "-f", cfg)
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "hps_") {
		t.Fatalf("server token: exit %d out %q err %q", code, out, errOut)
	}
	code, out, _ = runCLI(t, "admin-token", "create", "-f", cfg)
	if code != 0 || !strings.HasPrefix(strings.TrimSpace(out), "hpa_") {
		t.Fatalf("admin token: exit %d out %q", code, out)
	}
}
