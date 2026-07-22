package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hookploy.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const minimalServers = `
servers:
  s1: { local: true }
  s2: {}
`

// Behavior: the full PRD §4 example loads, normalizes and validates.
func TestLoadPRDExample(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "testdata", "hookploy.yaml"))
	if err != nil {
		t.Fatalf("PRD example must validate: %v", err)
	}

	// `server: x` sugar normalizes to one instance + one wave
	lm := cfg.Services["linkmind"]
	if len(lm.Instances) != 1 || lm.Instances[0].Server != "ali-hk-01" || lm.Instances[0].Dir != "/opt/apps/linkmind" {
		t.Fatalf("linkmind instances: %+v", lm.Instances)
	}
	if len(lm.Rollout) != 1 || len(lm.Rollout[0]) != 1 {
		t.Fatalf("linkmind rollout: %+v", lm.Rollout)
	}
	if !lm.Webhook {
		t.Fatal("webhook should default to true")
	}
	if lm.Timeout != 10*time.Minute {
		t.Fatalf("timeout should inherit defaults: %v", lm.Timeout)
	}

	// multi-instance: declaration order kept, rollout waves parsed
	vf := cfg.Services["vocalflow"]
	names := []string{vf.Instances[0].Name, vf.Instances[1].Name, vf.Instances[2].Name}
	if names[0] != "main" || names[1] != "api-sg0" || names[2] != "api-hk0" {
		t.Fatalf("instance order lost: %v", names)
	}
	if vf.Instances[1].Dir != "/opt/apps/vocalflow-rt" {
		t.Fatalf("instance dir should default to service dir: %+v", vf.Instances[1])
	}
	if len(vf.Rollout) != 2 || len(vf.Rollout[0]) != 1 || len(vf.Rollout[1]) != 2 {
		t.Fatalf("vocalflow rollout: %+v", vf.Rollout)
	}

	if cfg.Services["condenser"].Webhook {
		t.Fatal("condenser webhook must be false")
	}
	if len(cfg.Services["simul"].Tasks["db-push"]) != 1 {
		t.Fatal("simul task db-push missing")
	}

	// db path defaults next to the yaml
	if filepath.Base(cfg.DB) != "hookploy.db" || filepath.Dir(cfg.DB) != filepath.Dir(cfg.Path) {
		t.Fatalf("db default wrong: %s", cfg.DB)
	}
	if cfg.Servers["ali-hk-01"] == nil || !cfg.Servers["ali-hk-01"].Local {
		t.Fatal("ali-hk-01 must be local")
	}
}

// Behavior: rollout omitted → one wave per instance, declaration order.
func TestRolloutDefault(t *testing.T) {
	cfg, err := load(t, minimalServers+`
services:
  app:
    dir: /opt/a
    deploy: [compose.up]
    instances:
      b: { server: s2 }
      a: { server: s1 }
`)
	if err != nil {
		t.Fatal(err)
	}
	ro := cfg.Services["app"].Rollout
	if len(ro) != 2 || ro[0][0] != "b" || ro[1][0] != "a" {
		t.Fatalf("default rollout wrong: %+v", ro)
	}
}

// Behavior: top-level `webui` toggles the built-in web UI; omitted means on.
func TestWebUIToggle(t *testing.T) {
	base := minimalServers + `
services:
  app: { server: s1, dir: /opt/a, deploy: [compose.up] }
`
	for _, tc := range []struct {
		prefix string
		want   bool
	}{
		{"", true},
		{"webui: true\n", true},
		{"webui: false\n", false},
	} {
		cfg, err := load(t, tc.prefix+base)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WebUI != tc.want {
			t.Fatalf("prefix %q: WebUI = %v, want %v", tc.prefix, cfg.WebUI, tc.want)
		}
	}
}

// Behavior: top-level `github.webhook_secret` and service `github_repo`
// load into the normalized config; both are empty when omitted.
func TestGithubConfig(t *testing.T) {
	cfg, err := load(t, `
github:
  webhook_secret: s3cret
`+minimalServers+`
services:
  app: { server: s1, dir: /opt/a, deploy: [compose.up], github_repo: reorx/hookploy }
  bare: { server: s1, dir: /opt/b, deploy: [compose.up] }
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Github.WebhookSecret != "s3cret" {
		t.Fatalf("webhook secret: %q", cfg.Github.WebhookSecret)
	}
	if cfg.Services["app"].GithubRepo != "reorx/hookploy" {
		t.Fatalf("github_repo: %q", cfg.Services["app"].GithubRepo)
	}
	if cfg.Services["bare"].GithubRepo != "" {
		t.Fatalf("omitted github_repo should be empty: %q", cfg.Services["bare"].GithubRepo)
	}

	cfg, err = load(t, minimalServers+`
services:
  app: { server: s1, dir: /opt/a, deploy: [compose.up] }
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Github.WebhookSecret != "" {
		t.Fatalf("omitted github section should leave secret empty: %q", cfg.Github.WebhookSecret)
	}
}

// Behavior: a github_repo that is not owner/repo is rejected with the
// service name in the message.
func TestGithubRepoFormat(t *testing.T) {
	for _, bad := range []string{"foo", "a/b/c", "/b", "a/", "a b/c"} {
		_, err := load(t, minimalServers+`
services:
  app: { server: s1, dir: /opt/a, deploy: [compose.up], github_repo: "`+bad+`" }
`)
		if err == nil || !strings.Contains(err.Error(), `service "app"`) || !strings.Contains(err.Error(), "github_repo") {
			t.Fatalf("github_repo %q: want service-scoped error, got %v", bad, err)
		}
	}
}

// Behavior: static validation rejects every misconfiguration class,
// with the file (and line where applicable) in the message.
func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name, yaml, wantSub string
	}{
		{"unknown top-level field", "listne: {}\n" + minimalServers, "listne"},
		{"unknown service field", minimalServers + `
services:
  a: { server: s1, dir: /a, deplooy: [compose.up] }
`, "deplooy"},
		{"server ref missing", minimalServers + `
services:
  a: { server: nope, dir: /a, deploy: [compose.up] }
`, "nope"},
		{"server and instances mutually exclusive", minimalServers + `
services:
  a:
    server: s1
    dir: /a
    deploy: [compose.up]
    instances:
      x: { server: s1 }
`, "mutually exclusive"},
		{"neither server nor instances", minimalServers + `
services:
  a: { dir: /a, deploy: [compose.up] }
`, "server"},
		{"dir missing", minimalServers + `
services:
  a: { server: s1, deploy: [compose.up] }
`, "dir"},
		{"pin without image", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [image.pin, compose.up] }
`, "image"},
		{"pin without later compose.up", minimalServers + `
services:
  a:
    server: s1
    dir: /a
    image: ghcr.io/x/a
    deploy: [compose.up, image.pin]
`, "compose.up"},
		{"unknown op", minimalServers + `
services:
  a: { server: s1, dir: /a, deploy: [compose.blow] }
`, "compose.blow"},
		{"rollout references unknown instance", minimalServers + `
services:
  a:
    dir: /a
    deploy: [compose.up]
    instances:
      x: { server: s1 }
    rollout: [x, y]
`, "y"},
		{"rollout misses an instance", minimalServers + `
services:
  a:
    dir: /a
    deploy: [compose.up]
    instances:
      x: { server: s1 }
      y: { server: s2 }
    rollout: [x]
`, "y"},
		{"rollout duplicates an instance", minimalServers + `
services:
  a:
    dir: /a
    deploy: [compose.up]
    instances:
      x: { server: s1 }
    rollout: [x, x]
`, "once"},
		{"rollout without instances", minimalServers + `
services:
  a:
    server: s1
    dir: /a
    deploy: [compose.up]
    rollout: [a]
`, "rollout"},
		{"instance server missing", minimalServers + `
services:
  a:
    dir: /a
    deploy: [compose.up]
    instances:
      x: {}
`, "server"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := load(t, c.yaml)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not mention %q", err, c.wantSub)
			}
			if !strings.Contains(err.Error(), "hookploy.yaml") {
				t.Fatalf("error %q does not carry the file path", err)
			}
		})
	}
}

// Behavior: op errors point at the yaml line.
func TestOpErrorLine(t *testing.T) {
	_, err := load(t, minimalServers+`
services:
  a:
    server: s1
    dir: /a
    deploy:
      - compose.up
      - frob.nicate
`)
	if err == nil || !strings.Contains(err.Error(), "line 12") {
		t.Fatalf("want line 12 in error, got: %v", err)
	}
}

// Behavior: image.pin inside a task also requires a later compose.up.
func TestPinInTaskValidated(t *testing.T) {
	_, err := load(t, minimalServers+`
services:
  a:
    server: s1
    dir: /a
    image: ghcr.io/x/a
    deploy: [image.pin, compose.up]
    tasks:
      repin: [image.pin]
`)
	if err == nil || !strings.Contains(err.Error(), "compose.up") {
		t.Fatalf("want white-pin error for task, got: %v", err)
	}
}
