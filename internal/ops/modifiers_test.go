package ops

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Behavior: `timeout:` and `retries:` sit next to the op name like `on:` —
// in any key order, combinable with each other and with `on:`, and with the
// null-body form for zero-arg ops.
func TestParseStepModifiers(t *testing.T) {
	steps := parseSteps(t, `
- image.pin:
  timeout: 3m
  retries: 2
- compose.pull: { services: [web] }
  timeout: 90s
- compose.up
- run: { argv: [echo] }
  timeout: 10s
- retries: 0
  on: [main]
  artifact.extract: { url: u, sha256: s, to: d }
`)
	pin := steps[0]
	if pin.Timeout != 3*time.Minute || pin.Retries == nil || *pin.Retries != 2 {
		t.Fatalf("image.pin modifiers: timeout=%v retries=%v", pin.Timeout, pin.Retries)
	}
	if _, ok := pin.Args.(*ImagePin); !ok {
		t.Fatalf("a null body means zero args, got %T", pin.Args)
	}
	pull := steps[1]
	if pull.Timeout != 90*time.Second || pull.Retries != nil {
		t.Fatalf("compose.pull modifiers: timeout=%v retries=%v", pull.Timeout, pull.Retries)
	}
	if got := pull.Args.(*ComposePull).Services; len(got) != 1 || got[0] != "web" {
		t.Fatalf("args lost next to modifiers: %v", got)
	}
	if up := steps[2]; up.Timeout != 0 || up.Retries != nil {
		t.Fatalf("a plain step carries no modifiers: %+v", up)
	}
	if steps[3].Timeout != 10*time.Second {
		t.Fatalf("timeout applies to any op, got %v", steps[3].Timeout)
	}
	ae := steps[4]
	if ae.Retries == nil || *ae.Retries != 0 || len(ae.On) != 1 || ae.On[0] != "main" {
		t.Fatalf("modifiers before the op name: retries=%v on=%v", ae.Retries, ae.On)
	}
}

// Behavior: Attempts is the total number of runs a step gets. An explicit
// `retries:` wins; otherwise image.pin and artifact.extract keep the three
// tries their built-in loops used to give them, and every other op runs once.
func TestStepAttempts(t *testing.T) {
	steps := parseSteps(t, `
- image.pin
- artifact.extract: { url: u, sha256: s, to: d }
- compose.pull
- compose.up
- image.pin:
  retries: 0
- compose.pull:
  retries: 4
`)
	want := []int{3, 3, 1, 1, 1, 5}
	for i, w := range want {
		if got := steps[i].Attempts(); got != w {
			t.Errorf("step %d (%s): attempts = %d, want %d", i, steps[i].Op, got, w)
		}
	}
}

// Behavior: modifiers are parsed strictly. `retries` is only for ops that are
// safe to run again from scratch; values must be well-formed; a modifier
// nested inside the args, or healthcheck's old `retries` arg, gets a message
// that says how to fix it.
func TestParseStepModifierErrors(t *testing.T) {
	cases := []struct {
		name, src string
		wantSubs  []string
	}{
		{"retries on a side-effecting op",
			"- compose.run: { service: web, argv: [migrate] }\n  retries: 1",
			[]string{"line 2", "compose.run", "image.pin"}},
		{"retries on run", "- run: { argv: [x] }\n  retries: 1", []string{"run cannot be retried"}},
		{"retries on healthcheck points at attempts",
			"- healthcheck: { url: x }\n  retries: 2", []string{"healthcheck cannot be retried", "attempts"}},
		{"negative retries", "- compose.pull:\n  retries: -1", []string{"retries", "non-negative"}},
		{"retries not a number", "- compose.pull:\n  retries: two", []string{"retries", "non-negative"}},
		{"zero timeout", "- compose.pull:\n  timeout: 0s", []string{"timeout", "positive"}},
		{"timeout without unit", "- compose.pull:\n  timeout: 3", []string{"timeout", "duration"}},
		{"duplicate modifier", "- compose.pull:\n  timeout: 1m\n  timeout: 2m", []string{"duplicate", "timeout"}},
		{"misspelt modifier", "- compose.up: {}\n  timeot: 3m", []string{"timeot", `"timeout"`}},
		{"modifier nested in args", "- compose.pull:\n    timeout: 3m",
			[]string{"line 2", "step modifier", "same indentation"}},
		{"healthcheck retries arg was renamed",
			"- healthcheck: { url: x, retries: 5 }", []string{"line 1", `"retries" was renamed to "attempts"`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseStepsErr(c.src)
			if err == nil {
				t.Fatal("expected error")
			}
			for _, sub := range c.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not mention %q", err, sub)
				}
			}
		})
	}
}

// Behavior: unlike `on:`, timeout and retries travel in the op snapshot — the
// engine that runs the step (possibly on an edge) enforces them. A step
// without modifiers keeps its exact pre-modifier bytes.
func TestStepModifiersOnTheWire(t *testing.T) {
	steps := parseSteps(t, `
- compose.up
- image.pin:
  timeout: 3m
  retries: 2
- compose.pull:
  retries: 0
`)
	b, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	const want = `[{"op":"compose.up"},{"op":"image.pin","timeout":"3m0s","retries":2},{"op":"compose.pull","retries":0}]`
	if string(b) != want {
		t.Fatalf("wire format:\n got %s\nwant %s", b, want)
	}
	var back []Step
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back[0].Timeout != 0 || back[0].Retries != nil {
		t.Fatalf("plain step gained modifiers: %+v", back[0])
	}
	if back[1].Timeout != 3*time.Minute || back[1].Retries == nil || *back[1].Retries != 2 {
		t.Fatalf("modifiers lost: timeout=%v retries=%v", back[1].Timeout, back[1].Retries)
	}
	// an explicit zero is not the same as unset: it switches the default off
	if back[2].Retries == nil || back[2].Attempts() != 1 {
		t.Fatalf("explicit retries: 0 lost: %v", back[2].Retries)
	}
}

// Behavior: interpolation keeps the modifiers.
func TestInterpolateKeepsModifiers(t *testing.T) {
	steps := parseSteps(t, "- compose.pull: { services: [\"${payload.svc}\"] }\n  timeout: 2m\n  retries: 1\n")
	out, err := Interpolate(steps, map[string]any{"svc": "web"})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Timeout != 2*time.Minute || out[0].Retries == nil || *out[0].Retries != 1 {
		t.Fatalf("interpolation dropped modifiers: %+v", out[0])
	}
}

// Behavior: healthcheck's poll count is `attempts` (it always counted every
// poll, the first one included), default 5, and that is its wire name too.
// Snapshots stored before the rename still decode, so old deploy history
// keeps rendering.
func TestHealthcheckAttempts(t *testing.T) {
	steps := parseSteps(t, `
- healthcheck: { url: "http://x" }
- healthcheck: { url: "http://x", attempts: 9 }
`)
	if got := steps[0].Args.(*Healthcheck).Attempts; got != 5 {
		t.Fatalf("default attempts = %d, want 5", got)
	}
	b, err := json.Marshal(steps[1])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"attempts":9`) || strings.Contains(string(b), "retries") {
		t.Fatalf("wire form should use attempts: %s", b)
	}

	var legacy Step
	if err := json.Unmarshal([]byte(`{"op":"healthcheck","args":{"url":"http://x","expect":200,"retries":7,"interval":"3s"}}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if got := legacy.Args.(*Healthcheck).Attempts; got != 7 {
		t.Fatalf("legacy snapshot retries should decode as attempts, got %d", got)
	}
}

// Behavior: NeedsModifierSupport finds the first step an engine older than
// ModifiersSince would silently run differently: one with a timeout or
// retries, or a healthcheck whose attempts differ from the 5 an old engine
// falls back to (it looks for the old "retries" key and finds nothing).
func TestNeedsModifierSupport(t *testing.T) {
	cases := []struct {
		src  string
		want int
	}{
		{"- compose.pull\n- compose.up\n- healthcheck: { url: x }", -1},
		{"- healthcheck: { url: x, attempts: 5 }", -1},
		{"- compose.up\n- compose.pull:\n  timeout: 1m", 1},
		{"- image.pin:\n  retries: 2\n- compose.up", 0},
		{"- compose.up\n- healthcheck: { url: x, attempts: 8 }", 1},
	}
	for _, c := range cases {
		idx, ok := NeedsModifierSupport(parseSteps(t, c.src))
		if idx != c.want || ok != (c.want >= 0) {
			t.Errorf("%q: got (%d, %v), want %d", c.src, idx, ok, c.want)
		}
	}
}
