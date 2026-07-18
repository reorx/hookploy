package ops

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/model"
	"gopkg.in/yaml.v3"
)

func parseSteps(t *testing.T, src string) []Step {
	t.Helper()
	steps, err := parseStepsErr(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return steps
}

func parseStepsErr(src string) ([]Step, error) {
	var nodes []yaml.Node
	if err := yaml.Unmarshal([]byte(src), &nodes); err != nil {
		return nil, err
	}
	var steps []Step
	for i := range nodes {
		s, err := ParseStep(&nodes[i])
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, nil
}

// Behavior: a bare string is a zero-arg op; a single-key map carries args.
func TestParseStepForms(t *testing.T) {
	steps := parseSteps(t, `
- image.pin
- compose.up: { force_recreate: true, services: [web, worker] }
- compose.run: { service: web, argv: [python, manage.py, migrate] }
`)
	if steps[0].Op != "image.pin" {
		t.Fatalf("op[0] = %s", steps[0].Op)
	}
	up := steps[1].Args.(*ComposeUp)
	if !up.ForceRecreate || len(up.Services) != 2 {
		t.Fatalf("compose.up args wrong: %+v", up)
	}
	run := steps[2].Args.(*ComposeRun)
	if run.Service != "web" || len(run.Argv) != 3 {
		t.Fatalf("compose.run args wrong: %+v", run)
	}
}

// Behavior: unknown ops and unknown params are rejected with the source line.
func TestParseStepErrors(t *testing.T) {
	cases := []struct {
		src     string
		wantSub string
	}{
		{"- frob.nicate", "unknown op"},
		{"- compose.up: { forse_recreate: true }", "unknown field"},
		{"- compose.run: { argv: [ls] }", "service"},        // missing required
		{"- compose.run", "service"},                        // string form still needs required args
		{"- artifact.extract: { url: x, to: y }", "sha256"}, // sha256 mandatory
		{"- healthcheck: {}", "url"},                        // url required
		{"- image.extract: { from: /a }", "to"},             // to required
		{"- run: {}", "argv"},                               // argv required
		{"- compose.up: [a, b]", "mapping"},                 // args must be a mapping
		{"- compose.up: {x: 1}\n  extra: {}", "single-key"}, // two keys in one step
	}
	for _, c := range cases {
		_, err := parseStepsErr(c.src)
		if err == nil {
			t.Errorf("%q: expected error", c.src)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%q: error %q does not mention %q", c.src, err, c.wantSub)
		}
	}
	// line numbers included
	_, err := parseStepsErr("\n\n- frob.nicate")
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error should carry line 3, got: %v", err)
	}
}

// Behavior: healthcheck fills documented defaults (expect 200, retries 5, interval 3s).
func TestHealthcheckDefaults(t *testing.T) {
	steps := parseSteps(t, `- healthcheck: { url: "http://x/healthz" }`)
	hc := steps[0].Args.(*Healthcheck)
	if hc.Expect != 200 || hc.Retries != 5 || time.Duration(hc.Interval) != 3*time.Second {
		t.Fatalf("defaults not applied: %+v", hc)
	}
}

// Behavior: steps survive a JSON round trip with their concrete arg types —
// this is the DB snapshot / M2 wire format.
func TestStepJSONRoundTrip(t *testing.T) {
	steps := parseSteps(t, `
- image.pin
- env.write: { file: .env, set: { A: "1", B: "two" } }
- healthcheck: { url: "http://x", retries: 7 }
`)
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	var back []Step
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back[0].Args.(*ImagePin); !ok {
		t.Fatalf("step 0 args type lost: %T", back[0].Args)
	}
	ew := back[1].Args.(*EnvWrite)
	if ew.Set["B"] != "two" {
		t.Fatalf("env.write set lost: %+v", ew)
	}
	hc := back[2].Args.(*Healthcheck)
	if hc.Retries != 7 || hc.Expect != 200 {
		t.Fatalf("healthcheck lost: %+v", hc)
	}
}

// Behavior: ${payload.x} interpolates into structured arg positions only;
// required keys missing fail fast, optional keys yield empty string.
func TestInterpolate(t *testing.T) {
	steps := parseSteps(t, `
- artifact.extract: { url: "${payload.webdist_url}", sha256: "${payload.webdist_sha256}", to: webdist }
- run: { argv: [echo, "${payload.nested.msg?}", "v=${payload.num}"] }
- env.write: { file: .env, set: { REL: "${payload.release?}" } }
`)
	payload := map[string]any{
		"webdist_url":    "https://x/dist.tar.gz",
		"webdist_sha256": "abc",
		"num":            float64(42),
		"nested":         map[string]any{"msg": "hi"},
	}
	out, err := Interpolate(steps, payload)
	if err != nil {
		t.Fatal(err)
	}
	ae := out[0].Args.(*ArtifactExtract)
	if ae.URL != "https://x/dist.tar.gz" || ae.SHA256 != "abc" {
		t.Fatalf("artifact.extract not interpolated: %+v", ae)
	}
	run := out[1].Args.(*Run)
	if run.Argv[1] != "hi" || run.Argv[2] != "v=42" {
		t.Fatalf("run argv: %+v", run.Argv)
	}
	if out[2].Args.(*EnvWrite).Set["REL"] != "" {
		t.Fatalf("optional missing should be empty")
	}
	// original steps untouched (deep copy)
	if steps[0].Args.(*ArtifactExtract).URL != "${payload.webdist_url}" {
		t.Fatalf("Interpolate mutated its input")
	}

	// required key missing → fail fast, naming the key
	_, err = Interpolate(steps, map[string]any{"webdist_url": "u"})
	if err == nil || !strings.Contains(err.Error(), "webdist_sha256") {
		t.Fatalf("want missing-key error, got: %v", err)
	}
}

// Behavior: a hostile payload value stays a single argv element — command
// injection is structurally impossible.
func TestInterpolateInjectionSafety(t *testing.T) {
	steps := parseSteps(t, `- run: { argv: [echo, "${payload.msg}"] }`)
	out, err := Interpolate(steps, map[string]any{"msg": `"; rm -rf / #`})
	if err != nil {
		t.Fatal(err)
	}
	argv := out[0].Args.(*Run).Argv
	if len(argv) != 2 || argv[1] != `"; rm -rf / #` {
		t.Fatalf("hostile payload split or altered: %#v", argv)
	}
}

// Behavior: non-scalar payload values cannot be interpolated.
func TestInterpolateNonScalar(t *testing.T) {
	steps := parseSteps(t, `- run: { argv: ["${payload.obj}"] }`)
	_, err := Interpolate(steps, map[string]any{"obj": map[string]any{"a": 1}})
	if err == nil || !strings.Contains(err.Error(), "obj") {
		t.Fatalf("want non-scalar error, got %v", err)
	}
}

var _ = model.Duration(0) // keep import for Healthcheck.Interval type assertions
