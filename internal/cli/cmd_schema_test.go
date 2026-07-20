package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// Behavior: `hookploy schema` prints the hookploy.yaml JSON Schema on stdout
// and exits 0. The output is JSON itself, so there is no --json flag.
func TestSchemaCommand(t *testing.T) {
	code, out, errOut := runCLI(t, "schema")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("schema output must be valid JSON: %v\n%s", err, out)
	}
	if got := doc["$schema"]; got != "http://json-schema.org/draft-07/schema#" {
		t.Errorf("$schema = %v, want the draft-07 meta-schema", got)
	}
	defs, ok := doc["definitions"].(map[string]any)
	if !ok {
		t.Fatalf("schema must carry a definitions object, got %T", doc["definitions"])
	}
	if _, ok := defs["step"]; !ok {
		t.Errorf("definitions is missing the step definition; got keys %v", keysOf(defs))
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Behavior: `schema` takes no positional arguments. `hookploy schema x.yaml`
// used to print the generic schema and exit 0, reading as "x.yaml is valid".
func TestSchemaRejectsPositionalArgs(t *testing.T) {
	code, out, errOut := runCLI(t, "schema", "hookploy.yaml")
	if code != 2 {
		t.Fatalf("exit %d, want 2 (stdout %q, stderr %q)", code, out, errOut)
	}
	if !strings.Contains(errOut, "usage:") {
		t.Errorf("stderr should show usage, got %q", errOut)
	}
	if !strings.Contains(errOut, "validate") {
		t.Errorf("stderr should point at `hookploy validate`, got %q", errOut)
	}
}
