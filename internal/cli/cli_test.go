package cli

import (
	"bytes"
	"strings"
	"testing"
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
