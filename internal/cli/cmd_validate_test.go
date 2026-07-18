package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Behavior: `hookploy validate -f <valid>` exits 0 and reports counts;
// an invalid file exits 1 with the reason on stderr.
func TestValidateCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"validate", "-f", filepath.Join("..", "..", "testdata", "hookploy.yaml")}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "OK") {
		t.Fatalf("stdout: %s", out.String())
	}

	bad := filepath.Join(t.TempDir(), "hookploy.yaml")
	os.WriteFile(bad, []byte("services:\n  a: { server: ghost, dir: /a, deploy: [compose.up] }\n"), 0o644)
	out.Reset()
	errOut.Reset()
	code = Run([]string{"validate", "-f", bad}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "ghost") {
		t.Fatalf("stderr should name the bad server: %s", errOut.String())
	}
}
