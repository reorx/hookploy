package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Golden snapshots freeze the --json contract of every command: the key set
// plus the static values. Volatile values (ids, timestamps, secrets, the
// build version) are masked, so what the golden locks is the shape — exactly
// what internal/api promises not to break. Refresh with:
//
//	go test ./internal/cli -update
var updateGolden = flag.Bool("update", false, "rewrite the JSON golden files instead of comparing")

const goldenDir = "testdata/golden"

// maskKey replaces the volatile values by key. Empty and null values are left
// alone, so "absent" and "present but masked" stay distinguishable.
func maskKey(key string, v any) (any, bool) {
	if v == nil || v == "" {
		return nil, false
	}
	switch {
	case key == "id" || key == "deploy_id" || key == "execution_id":
		return "<id>", true
	case key == "at" || strings.HasSuffix(key, "_at"):
		return "<time>", true
	case key == "status_url":
		return "/deploys/<id>", true
	case key == "token":
		return "<token>", true
	case key == "version":
		return "<version>", true
	case key == "error":
		// The wording belongs to the package that raised it, not to this
		// contract; only its presence is frozen here.
		return "<error>", true
	}
	return nil, false
}

func mask(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if rep, ok := maskKey(k, val); ok {
				out[k] = rep
				continue
			}
			out[k] = mask(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = mask(e)
		}
		return out
	}
	return v
}

// canonical decodes one JSON document, masks it and re-marshals it with
// sorted keys — so the golden locks the key set, not the field order.
func canonical(t *testing.T, doc string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, doc)
	}
	b, err := json.MarshalIndent(mask(v), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// canonicalNDJSON canonicalizes one JSON document per line.
func canonicalNDJSON(t *testing.T, doc string) string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(doc, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, canonical(t, line))
	}
	return strings.Join(lines, "\n")
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	if *updateGolden {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run: go test ./internal/cli -update): %v", path, err)
	}
	if strings.TrimRight(string(want), "\n") != got {
		t.Errorf("golden %s mismatch\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// Behavior: the --json output of the remote query commands is frozen.
func TestJSONGoldenRemote(t *testing.T) {
	_, fake := startMain(t)
	fake.On("deploy-linkmind").Returning("release deployed!\n", 0)
	fake.On("say-hello").Returning("hi\n", 0)

	code, out, errOut := runCLI(t, "deploy", "linkmind", "--json")
	if code != 0 {
		t.Fatalf("deploy: %d %s", code, errOut)
	}
	var acc struct {
		DeployID string `json:"deploy_id"`
	}
	if err := json.Unmarshal([]byte(out), &acc); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "deploy.json", canonical(t, out))

	waitCLI(t, func() bool {
		_, out, _ := runCLI(t, "deploys", "linkmind", "--json")
		var list []struct {
			Status string `json:"status"`
		}
		if json.Unmarshal([]byte(out), &list) != nil {
			return false
		}
		return len(list) == 1 && list[0].Status == "succeeded"
	})

	for _, tc := range []struct {
		golden string
		args   []string
		ndjson bool
	}{
		{"status.json", []string{"status", "--json"}, false},
		{"deploys.json", []string{"deploys", "linkmind", "--json"}, false},
		{"logs.ndjson", []string{"logs", acc.DeployID, "--json"}, true},
		{"logs_follow.ndjson", []string{"logs", acc.DeployID, "-f", "--json"}, true},
		{"task.json", []string{"task", "linkmind", "hello", "--json"}, false},
	} {
		code, out, errOut := runCLI(t, tc.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, errOut)
		}
		if tc.ndjson {
			checkGolden(t, tc.golden, canonicalNDJSON(t, out))
		} else {
			checkGolden(t, tc.golden, canonical(t, out))
		}
	}
}

// Behavior: the --json output of the local (config/store-only) commands is
// frozen too.
func TestJSONGoldenLocal(t *testing.T) {
	_, cfg := writeConfig(t)
	validCfg := filepath.Join("..", "..", "testdata", "hookploy.yaml")
	invalidCfg := filepath.Join("testdata", "invalid.yaml")

	for _, tc := range []struct {
		golden   string
		args     []string
		wantCode int
	}{
		{"version.json", []string{"version", "--json"}, 0},
		{"validate_ok.json", []string{"validate", "-f", validCfg, "--json"}, 0},
		{"validate_fail.json", []string{"validate", "-f", invalidCfg, "--json"}, 1},
		{"token_create.json", []string{"token", "create", "linkmind", "-f", cfg, "--json"}, 0},
		{"token_rotate.json", []string{"token", "rotate", "linkmind", "-f", cfg, "--json"}, 0},
		{"token_revoke.json", []string{"token", "revoke", "linkmind", "-f", cfg, "--json"}, 0},
		{"server_token_create.json", []string{"server", "token", "create", "s1", "-f", cfg, "--json"}, 0},
		{"admin_token_create.json", []string{"admin-token", "create", "-f", cfg, "--json"}, 0},
	} {
		code, out, errOut := runCLI(t, tc.args...)
		if code != tc.wantCode {
			t.Fatalf("%v: exit %d, want %d: %s", tc.args, code, tc.wantCode, errOut)
		}
		checkGolden(t, tc.golden, canonical(t, out))
	}
}
