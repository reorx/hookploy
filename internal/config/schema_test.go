package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"testing"

	"github.com/reorx/hookploy/internal/ops"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

// compileSchema compiles the generated JSON Schema once per test.
func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := JSONSchema()
	if err != nil {
		t.Fatalf("JSONSchema(): %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("generated schema is not valid JSON: %v\n%s", err, raw)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("hookploy.schema.json", doc); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	sch, err := c.Compile("hookploy.schema.json")
	if err != nil {
		t.Fatalf("schema does not compile: %v", err)
	}
	return sch
}

// validateYAML runs a hookploy.yaml document through the schema, using the
// same yaml → JSON pipeline an editor / `hookploy validate` would.
func validateYAML(t *testing.T, sch *jsonschema.Schema, src string) error {
	t.Helper()
	var tree any
	if err := yaml.Unmarshal([]byte(src), &tree); err != nil {
		t.Fatalf("yaml does not parse: %v", err)
	}
	b, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("yaml tree is not JSON-encodable: %v", err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	return sch.Validate(inst)
}

func prdExample(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "hookploy.yaml"))
	if err != nil {
		t.Fatalf("read PRD example: %v", err)
	}
	return string(b)
}

// validCorpus is the set of documents the loader accepts; the schema must
// accept every one of them (see TestSchemaNeverRejectsLoadable).
func validCorpus(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"prd example": prdExample(t),
		"minimal single server": minimalServers + `
services:
  app:
    server: s1
    dir: /opt/a
    deploy: [compose.up]
`,
		"instances without rollout": minimalServers + `
services:
  app:
    dir: /opt/a
    deploy: [compose.up]
    instances:
      b: { server: s2 }
      a: { server: s1 }
`,
		"rollout scalar and sequence waves": minimalServers + `
services:
  app:
    dir: /opt/a
    deploy: [compose.up]
    instances:
      x: { server: s1 }
      y: { server: s2 }
      z: { server: s2 }
    rollout:
      - x
      - [y, z]
`,
		"all op forms": minimalServers + `
listen:
  http: "127.0.0.1:9100"
  grpc: "127.0.0.1:9101"
db: /var/lib/hookploy/hookploy.db
defaults:
  timeout: 10m
services:
  app:
    server: s1
    dir: /opt/a
    image: ghcr.io/x/a
    webhook: false
    timeout: 90s
    deploy:
      - env.require: { file: .env, keys: [A, B] }
      - env.write: { file: .env, set: { A: "1", B: two } }
      - image.pin
      - image.extract: { from: /app/static, to: static, image: ghcr.io/x/b, pull: true }
      - artifact.extract: { url: "${payload.u}", sha256: "${payload.s}", to: dist }
      - compose.pull
      - compose.pull: { services: [web] }
      - compose.restart
      - compose.run: { service: web, argv: [python, manage.py, migrate] }
      - compose.up: { force_recreate: true, services: [web, worker] }
      - compose.exec: { service: web, argv: [ls] }
      - run: { argv: [echo, hi] }
      - healthcheck: { url: "http://127.0.0.1:1/healthz", expect: 204, retries: 3, interval: 1s }
    tasks:
      db-push:
        - compose.exec: { service: web, argv: [pnpm, "db:push"] }
`,
		"servers only, no services": minimalServers,
	}
}

// Behavior: every configuration the loader accepts also passes the schema.
func TestSchemaAcceptsValidConfigs(t *testing.T) {
	sch := compileSchema(t)
	for name, src := range validCorpus(t) {
		t.Run(name, func(t *testing.T) {
			if err := validateYAML(t, sch, src); err != nil {
				t.Fatalf("schema rejected a valid config:\n%v", err)
			}
		})
	}
}

// Behavior: the schema catches the misconfiguration classes an editor can
// see statically, before `hookploy validate` ever runs.
func TestSchemaRejectsInvalidConfigs(t *testing.T) {
	sch := compileSchema(t)
	// wantSub pins the keyword that must do the rejecting, so a case cannot
	// start passing for an unrelated reason.
	cases := []struct{ name, yaml, wantSub string }{
		{"unknown top-level field", "listne: {}\n" + minimalServers,
			"additional properties 'listne' not allowed"},
		{"unknown listen field", "listen: { htp: \"x\" }\n" + minimalServers,
			"additional properties 'htp' not allowed"},
		{"unknown service field", minimalServers + `
services:
  a: { server: s1, dir: /a, deplooy: [compose.up] }
`, "additional properties 'deplooy' not allowed"},
		{"unknown server field", `
servers:
  s1: { loca: true }
`, "additional properties 'loca' not allowed"},
		{"unknown op", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [compose.blow] }
`, "definitions/step/oneOf"},
		{"unknown op arg", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [{ compose.up: { forse_recreate: true } }] }
`, "additional properties 'forse_recreate' not allowed"},
		{"step map with two keys", minimalServers + `
services:
  a:
    server: s1
    dir: /a
    deploy:
      - compose.up: {}
        compose.pull: {}
`, "maxProperties: got 2, want 1"},
		{"step is a sequence", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [[compose.up]] }
`, "got array, want object"},
		{"bare string form of an op that needs args", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [compose.run] }
`, "step/oneOf/0/enum"},
		{"missing required op arg", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [{ compose.run: { argv: [ls] } }] }
`, "properties/compose.run/required]: missing property 'service'"},
		{"wrong op arg type", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [{ compose.up: { force_recreate: 3 } }] }
`, "force_recreate/type]: got number, want boolean"},
		{"op arg array of wrong element type", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [{ compose.pull: { services: [1, 2] } }] }
`, "services/items/type]: got number, want string"},
		{"rollout wave is a map", minimalServers + `
services:
  a:
    dir: /a
    deploy: [compose.up]
    instances:
      x: { server: s1 }
    rollout:
      - { x: 1 }
`, "properties/rollout/items/oneOf"},
		{"server and instances together", minimalServers + `
services:
  a:
    server: s1
    dir: /a
    deploy: [compose.up]
    instances:
      x: { server: s1 }
`, "service/oneOf/0]: 'not' failed"},
		{"neither server nor instances", minimalServers + `
services:
  a: { dir: /a, deploy: [compose.up] }
`, "oneOf/0/required]: missing property 'server'"},
		{"server without dir", minimalServers + `
services:
  a: { server: s1, deploy: [compose.up] }
`, "oneOf/0/required]: missing property 'dir'"},
		{"rollout without instances", minimalServers + `
services:
  a:
    server: s1
    dir: /a
    deploy: [compose.up]
    rollout: [a]
`, "dependency/rollout]: properties 'instances' required"},
		{"deploy missing", minimalServers + `
services:
  a: { server: s1, dir: /a }
`, "service/required]: missing property 'deploy'"},
		{"deploy is not a list", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: compose.up }
`, "deploy/type]: got string, want array"},
		{"unknown instance field", minimalServers + `
services:
  a:
    dir: /a
    deploy: [compose.up]
    instances:
      x: { server: s1, dirr: /b }
`, "additional properties 'dirr' not allowed"},
		{"timeout is not a duration", minimalServers + `
services:
  a: { server: s1, dir: /a, timeout: 30, deploy: [compose.up] }
`, "duration/type]: got number, want string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateYAML(t, sch, c.yaml)
			if err == nil {
				t.Fatalf("schema accepted an invalid config")
			}
			detail := fmt.Sprintf("%#v", err)
			if !strings.Contains(detail, c.wantSub) {
				t.Fatalf("rejected, but not for %q:\n%s", c.wantSub, detail)
			}
		})
	}
}

// Behavior (sync guard): the schema is a permissive upper bound — anything
// the loader accepts, the schema must accept. The converse is deliberately
// allowed: `hookploy validate` stays the authority.
func TestSchemaNeverRejectsLoadable(t *testing.T) {
	sch := compileSchema(t)
	corpus := validCorpus(t)
	// invalid-by-loader documents too: whenever parse happens to accept one,
	// the invariant must still hold.
	for name, src := range map[string]string{
		"dir omitted with per-instance dirs": minimalServers + `
services:
  a:
    deploy: [compose.up]
    instances:
      x: { server: s1, dir: /a }
`,
		"webhook true explicit": minimalServers + `
services:
  a: { server: s1, dir: /a, webhook: true, deploy: [compose.up] }
`,
	} {
		corpus[name] = src
	}
	for name, src := range corpus {
		t.Run(name, func(t *testing.T) {
			if _, err := parse([]byte(src)); err != nil {
				t.Skipf("not loadable, invariant does not apply: %v", err)
			}
			if err := validateYAML(t, sch, src); err != nil {
				t.Fatalf("loader accepts but schema rejects:\n%v", err)
			}
		})
	}
}

// Behavior: the schema knows the whole op vocabulary — every registered op
// is a key of the map form, and the bare-string form lists exactly the ops
// whose zero-value Args validate.
func TestSchemaOpCoverage(t *testing.T) {
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Definitions struct {
			Step struct {
				OneOf []struct {
					Type       string         `json:"type"`
					Enum       []string       `json:"enum"`
					Properties map[string]any `json:"properties"`
				} `json:"oneOf"`
			} `json:"step"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal generated schema: %v", err)
	}
	var enum []string
	var props map[string]any
	for _, branch := range doc.Definitions.Step.OneOf {
		if len(branch.Enum) > 0 {
			enum = branch.Enum
		}
		if len(branch.Properties) > 0 {
			props = branch.Properties
		}
	}
	if props == nil || enum == nil {
		t.Fatalf("step definition missing its string/map branches:\n%s", raw)
	}

	for _, info := range ops.Catalog() {
		if _, ok := props[info.Name]; !ok {
			t.Errorf("op %q missing from the step map branch", info.Name)
		}
	}
	if len(props) != len(ops.Catalog()) {
		t.Errorf("step map branch has %d ops, catalog has %d", len(props), len(ops.Catalog()))
	}

	var wantEnum []string
	for _, info := range ops.Catalog() {
		if info.Defaults.Validate() == nil {
			wantEnum = append(wantEnum, info.Name)
		}
	}
	if strings.Join(enum, ",") != strings.Join(wantEnum, ",") {
		t.Errorf("bare-string enum = %v, want %v", enum, wantEnum)
	}
}

// Behavior: schema generation is deterministic — the published artifact does
// not churn between runs.
func TestSchemaDeterministic(t *testing.T) {
	a, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b, err := JSONSchema()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("JSONSchema() output is not stable across calls")
		}
	}
}

// Behavior: the duration pattern published in the schema must accept exactly
// what time.ParseDuration (the decoder behind model.Duration) accepts — a
// stricter pattern would flag configs that load fine.
func TestDurationPatternMatchesParseDuration(t *testing.T) {
	sch := compileSchema(t)
	accepted := []string{
		"30s", "10m", "1h30m", "0", "+0", "-0", "-1h", "+1h",
		".5s", "1.s", "1.5s", "1h.5m", "3μs", "3µs",
		"100ns", "5us", "250ms", "1h0m0s", "0s",
	}
	for _, s := range accepted {
		if _, err := time.ParseDuration(s); err != nil {
			t.Fatalf("test bug: %q is not a Go duration: %v", s, err)
		}
		src := fmt.Sprintf("defaults:\n  timeout: %q\nservices: {}\n", s)
		if err := validateYAML(t, sch, src); err != nil {
			t.Errorf("schema rejects duration %q which ParseDuration accepts: %v", s, err)
		}
	}
	rejected := []string{"", "10", "abc", "1x", "1.2.3s", "s", "1h -5m", "00"}
	for _, s := range rejected {
		if _, err := time.ParseDuration(s); err == nil {
			t.Fatalf("test bug: %q parses as a Go duration", s)
		}
		src := fmt.Sprintf("defaults:\n  timeout: %q\nservices: {}\n", s)
		if err := validateYAML(t, sch, src); err == nil {
			t.Errorf("schema accepts duration %q which ParseDuration rejects", s)
		}
	}
}

// Behavior: draft-07 implementations ignore keywords sitting next to $ref, so
// no node in the published schema may carry both.
func TestNoSiblingsNextToRef(t *testing.T) {
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch t2 := v.(type) {
		case map[string]any:
			if _, hasRef := t2["$ref"]; hasRef && len(t2) > 1 {
				var siblings []string
				for k := range t2 {
					if k != "$ref" {
						siblings = append(siblings, k)
					}
				}
				sort.Strings(siblings)
				t.Errorf("%s: $ref has sibling keywords %v, which draft-07 ignores", path, siblings)
			}
			for k, val := range t2 {
				walk(path+"/"+k, val)
			}
		case []any:
			for i, e := range t2 {
				walk(fmt.Sprintf("%s/%d", path, i), e)
			}
		}
	}
	walk("#", doc)
}

// schemaSyncCases maps each on-disk struct to the schema node that describes
// it. The op part of the schema is reflection-derived and cannot drift, but
// these nodes are hand-written next to additionalProperties:false — a field
// added to raw.go without a matching schema.go edit silently turns a legal
// config into one the published schema rejects.
//
// The table is deliberately explicit: polymorphic fields (deploy/tasks carry
// steps, rollout carries scalar-or-list waves) are just ordinary keys here.
// This guard compares key sets only; their value shapes are covered by
// TestSchemaAcceptsPRDExample and the step/catalog tests.
var schemaSyncCases = []struct {
	name string
	ptr  string // JSON pointer to the object node in the generated schema
	raw  any    // pointer to the struct decoded from that node
	// keyFields hold the mapping key rather than a yaml field (an instance's
	// name is the map key in `instances:`, not a property of its body).
	keyFields []string
}{
	{name: "rawFile", ptr: "#", raw: &rawFile{}},
	{name: "rawGithub", ptr: "#/properties/github", raw: &rawGithub{}},
	{name: "Listen", ptr: "#/properties/listen", raw: &Listen{}},
	{name: "rawDefaults", ptr: "#/properties/defaults", raw: &rawDefaults{}},
	{name: "rawServer", ptr: "#/definitions/server", raw: &rawServer{}},
	{name: "rawService", ptr: "#/definitions/service", raw: &rawService{}},
	{name: "rawInstance", ptr: "#/definitions/instance", raw: &rawInstance{}, keyFields: []string{"name"}},
}

// resolvePointer walks a "#/a/b" JSON pointer through a decoded schema.
func resolvePointer(t *testing.T, doc any, ptr string) map[string]any {
	t.Helper()
	cur := doc
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "#"), "/") {
		if seg == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s: segment %q does not resolve to an object", ptr, seg)
		}
		if cur, ok = m[seg]; !ok {
			t.Fatalf("%s: segment %q is missing from the schema", ptr, seg)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("%s does not resolve to an object", ptr)
	}
	return m
}

// Behavior: every hand-written object node in the schema lists exactly the
// yaml keys its raw.go struct accepts — in both directions.
func TestSchemaMatchesRawStructs(t *testing.T) {
	raw, err := JSONSchema()
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	for _, tc := range schemaSyncCases {
		t.Run(tc.name, func(t *testing.T) {
			// ops.YAMLFieldSet is what the strict decoder itself consults, so
			// this compares the schema against the real accepted key set.
			want := ops.YAMLFieldSet(tc.raw)
			for _, k := range tc.keyFields {
				if !want[k] {
					t.Errorf("keyFields lists %q but %s has no such yaml field; drop it from the table", k, tc.name)
				}
				delete(want, k)
			}

			node := resolvePointer(t, doc, tc.ptr)
			props, ok := node["properties"].(map[string]any)
			if !ok {
				t.Fatalf("%s has no properties object (got %T)", tc.ptr, node["properties"])
			}

			var missing, extra []string
			for k := range want {
				if _, ok := props[k]; !ok {
					missing = append(missing, k)
				}
			}
			for k := range props {
				if !want[k] {
					extra = append(extra, k)
				}
			}
			sort.Strings(missing)
			sort.Strings(extra)
			if len(missing) > 0 {
				t.Errorf("%s accepts yaml keys %v that %s does not declare — "+
					"raw.go gained a field, sync internal/config/schema.go",
					tc.name, missing, tc.ptr)
			}
			if len(extra) > 0 {
				t.Errorf("%s declares properties %v that %s does not accept — "+
					"raw.go dropped or renamed a field, sync internal/config/schema.go",
					tc.ptr, extra, tc.name)
			}
		})
	}
}
