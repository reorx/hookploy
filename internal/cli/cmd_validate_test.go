package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reorx/hookploy/internal/api"
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

// Behavior: `validate --json` emits api.ValidateResult on stdout in both the
// success and the failure case; failure keeps exit code 1.
func TestValidateCommandJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"validate", "-f", filepath.Join("..", "..", "testdata", "hookploy.yaml"), "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut.String())
	}
	var res api.ValidateResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("validate --json must decode as api.ValidateResult: %v\n%s", err, out.String())
	}
	if !res.OK || res.Servers == 0 || res.Services == 0 || res.Error != "" {
		t.Fatalf("unexpected result: %+v", res)
	}

	bad := filepath.Join(t.TempDir(), "hookploy.yaml")
	os.WriteFile(bad, []byte("services:\n  a: { server: ghost, dir: /a, deploy: [compose.up] }\n"), 0o644)
	out.Reset()
	errOut.Reset()
	code = Run([]string{"validate", "-f", bad, "--json"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	res = api.ValidateResult{}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("failed validate --json must still emit JSON on stdout: %v\n%s", err, out.String())
	}
	if res.OK || res.Error == "" || !strings.Contains(res.Error, "ghost") {
		t.Fatalf("unexpected failure result: %+v", res)
	}
}

// Behavior: `validate` takes its file via -f only. A stray positional would
// otherwise stop flag parsing and silently validate the default
// ./hookploy.yaml with --json dropped — a false OK. It must fail with exit 2.
func TestValidateRejectsPositionalArgs(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.yaml")
	os.WriteFile(bad, []byte("services:\n  a: { server: ghost, dir: /a, deploy: [compose.up] }\n"), 0o644)

	for _, args := range [][]string{
		{"validate", bad},
		{"validate", bad, "--json"},
	} {
		code, out, errOut := runCLI(t, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2 (stdout %q, stderr %q)", args, code, out, errOut)
		}
		if !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: stderr should show usage, got %q", args, errOut)
		}
	}
}
