package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/version"
)

// Behavior: `hookploy --help` (and bare `hookploy`) lists all subcommands.
func TestHelpListsSubcommands(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"--help"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, errOut.String())
	}
	help := out.String()
	for _, cmd := range []string{"main", "validate", "status", "deploys", "logs", "deploy", "task", "token", "server", "admin-token"} {
		if !strings.Contains(help, cmd) {
			t.Errorf("help output missing command %q", cmd)
		}
	}
}

// Behavior: `hookploy version` (and the --version/-v aliases) prints the
// stamped version to stdout and exits 0.
func TestVersionCommand(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		var out, errOut bytes.Buffer
		code := Run(args, &out, &errOut)
		if code != 0 {
			t.Fatalf("%v: exit code = %d, want 0; stderr: %s", args, code, errOut.String())
		}
		if !strings.Contains(out.String(), version.Version) {
			t.Errorf("%v: output %q should contain version %q", args, out.String(), version.Version)
		}
	}
}

// Behavior: `version --json` emits exactly api.VersionInfo.
func TestVersionCommandJSON(t *testing.T) {
	code, out, errOut := runCLI(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	var v api.VersionInfo
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("version --json must decode as api.VersionInfo: %v\n%s", err, out)
	}
	if v.Version != version.Version {
		t.Errorf("version = %q, want %q", v.Version, version.Version)
	}
}

// Behavior: unknown commands fail with a usage error on stderr, exit 2.
func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"frobnicate"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "frobnicate") {
		t.Errorf("stderr should mention the unknown command, got: %s", errOut.String())
	}
}

// Behavior: two-level commands route to their subcommand table.
func TestTokenUnknownSubcommand(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"token", "explode"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr: %s", code, errOut.String())
	}
}

// Behavior: `version` rejects positional arguments the same way it rejects
// unknown flags (exit 2); the --version/-v aliases keep working.
func TestVersionRejectsPositionalArgs(t *testing.T) {
	code, out, errOut := runCLI(t, "version", "garbage")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (stdout %q, stderr %q)", code, out, errOut)
	}
	if !strings.Contains(errOut, "usage:") {
		t.Errorf("stderr should show usage, got %q", errOut)
	}
}

// failWriter fails every write, standing in for a closed pipe or a full disk.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// Behavior: a failed JSON write is reported and exits non-zero. `token create`
// prints its plaintext exactly once, so a silently swallowed write loses a
// secret that cannot be recovered.
func TestJSONWriteFailureIsReported(t *testing.T) {
	var errOut bytes.Buffer
	code := Run([]string{"version", "--json"}, failWriter{}, &errOut)
	if code == 0 {
		t.Fatalf("exit 0 despite a failed stdout write; stderr %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "broken pipe") {
		t.Errorf("stderr should report the write failure, got %q", errOut.String())
	}
}
