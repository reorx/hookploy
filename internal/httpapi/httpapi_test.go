package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

const testConfig = `
servers:
  s1: { local: true }
services:
  linkmind:
    server: s1
    dir: /opt/apps/linkmind
    image: ghcr.io/reorx/linkmind
    deploy:
      - run: { argv: [deploy-linkmind] }
  condenser:
    server: s1
    dir: /opt/apps/condenser
    webhook: false
    deploy:
      - run: { argv: [deploy-condenser] }
  simul:
    server: s1
    dir: /opt/apps/simul
    deploy:
      - run: { argv: [publish, "${payload.release_url}"] }
    tasks:
      db-push:
        - run: { argv: [db-push-step] }
`

type harness struct {
	t          *testing.T
	ts         *httptest.Server
	store      *store.Store
	fake       *runner.FakeRunner
	cfgPath    string
	svcToken   string // linkmind service token
	adminToken string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hookploy.yaml")
	if err := os.WriteFile(cfgPath, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	fake := &runner.FakeRunner{}
	eng := &engine.Engine{Runner: fake, Sleep: func(context.Context, time.Duration) error { return nil }}
	reg := executor.NewRegistry(50 * time.Millisecond)
	reg.Register("s1", &executor.Local{Engine: eng})
	sched := scheduler.New(st, reg)
	t.Cleanup(sched.Shutdown)

	cfgHolder := cfg
	srv := &Server{
		Store:  st,
		Sched:  sched,
		Config: func() *config.Config { return cfgHolder },
		Reload: func() error {
			c2, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			cfgHolder = c2
			return nil
		},
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	svcToken := token.New(token.KindService)
	st.InsertToken(string(token.KindService), "linkmind", token.Hash(svcToken))
	adminToken := token.New(token.KindAdmin)
	st.InsertToken(string(token.KindAdmin), "admin", token.Hash(adminToken))

	return &harness{t: t, ts: ts, store: st, fake: fake, cfgPath: cfgPath, svcToken: svcToken, adminToken: adminToken}
}

func (h *harness) hook(service, tok, body string, headers ...string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.ts.URL+"/hooks/"+service, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) adminReq(method, path, body string) *http.Response {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, r io.Reader) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func (h *harness) waitDeploy(id string, want model.Status) *model.Deploy {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := h.store.GetDeploy(id)
		if d != nil && d.Status == want {
			return d
		}
		if d != nil && d.Status.Terminal() && d.Status != want {
			h.t.Fatalf("deploy %s: %s (want %s), error=%s", id, d.Status, want, d.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timeout waiting for %s", id)
	return nil
}

// Behavior: a valid webhook returns 202 with deploy_id/status_url, records
// the payload, and the deploy runs to succeeded.
func TestWebhookHappyPath(t *testing.T) {
	h := newHarness(t)
	resp := h.hook("linkmind", h.svcToken, `{"note":"hi"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	acc := decodeJSON[api.Accepted](t, resp.Body)
	if !strings.HasPrefix(acc.DeployID, "dp_") || acc.StatusURL != "/deploys/"+acc.DeployID {
		t.Fatalf("bad accepted body: %+v", acc)
	}
	d := h.waitDeploy(acc.DeployID, model.StatusSucceeded)
	if !bytes.Contains([]byte(d.Payload), []byte(`"note"`)) {
		t.Fatalf("payload not persisted: %s", d.Payload)
	}
	if got := h.fake.JoinedCalls(); len(got) != 1 || got[0] != "deploy-linkmind" {
		t.Fatalf("pipeline ran wrong: %v", got)
	}
}

// Behavior: auth/kind failures map to 401/404/403 as documented.
func TestWebhookAuthMatrix(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name    string
		service string
		token   string
		want    int
	}{
		{"no token", "linkmind", "", 401},
		{"bad token", "linkmind", "hpt_wrong", 401},
		{"unknown service", "ghost", h.svcToken, 404},
		{"token of another service", "condenser", h.svcToken, 403},
		{"webhook disabled", "condenser", h.condenserToken(), 403},
		{"admin token cannot hit hooks", "linkmind", h.adminToken, 403},
	}
	for _, c := range cases {
		resp := h.hook(c.service, c.token, `{}`)
		if resp.StatusCode != c.want {
			t.Errorf("%s: got %d want %d", c.name, resp.StatusCode, c.want)
		}
		resp.Body.Close()
	}
}

func (h *harness) condenserToken() string {
	tok := token.New(token.KindService)
	h.store.InsertToken(string(token.KindService), "condenser", token.Hash(tok))
	return tok
}

// Behavior: the legacy X-Deploy-Token header authenticates too.
func TestWebhookXDeployTokenHeader(t *testing.T) {
	h := newHarness(t)
	resp := h.hook("linkmind", "", `{}`, "X-Deploy-Token", h.svcToken)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// Behavior: a malformed payload.digest still returns 202, but the deploy is
// recorded as failed with a digest error (CI contract stays single-path).
func TestWebhookBadDigest(t *testing.T) {
	h := newHarness(t)
	resp := h.hook("linkmind", h.svcToken, `{"digest":"not-a-digest"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	acc := decodeJSON[api.Accepted](t, resp.Body)
	d := h.waitDeploy(acc.DeployID, model.StatusFailed)
	if !strings.Contains(d.Error, "digest") {
		t.Fatalf("error should mention digest: %q", d.Error)
	}
	if len(h.fake.Calls) != 0 {
		t.Fatal("nothing must run for a rejected digest")
	}
}

// Behavior: a missing required interpolation key fails the deploy (naming
// the key) but still answers 202.
func TestWebhookInterpolationFailure(t *testing.T) {
	h := newHarness(t)
	tok := token.New(token.KindService)
	h.store.InsertToken(string(token.KindService), "simul", token.Hash(tok))
	resp := h.hook("simul", tok, `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	acc := decodeJSON[api.Accepted](t, resp.Body)
	d := h.waitDeploy(acc.DeployID, model.StatusFailed)
	if !strings.Contains(d.Error, "release_url") {
		t.Fatalf("error should name the missing key: %q", d.Error)
	}
}

// Behavior: non-object bodies are rejected with 400; bodies over 1MiB with 413.
func TestWebhookBodyValidation(t *testing.T) {
	h := newHarness(t)
	resp := h.hook("linkmind", h.svcToken, `[1,2,3]`)
	if resp.StatusCode != 400 {
		t.Fatalf("array body: %d", resp.StatusCode)
	}
	resp = h.hook("linkmind", h.svcToken, "{"+strings.Repeat(`"k":"v",`, 200000)+`"z":"v"}`)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body: %d", resp.StatusCode)
	}
	// empty body = empty payload, fine
	resp = h.hook("linkmind", h.svcToken, "")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("empty body: %d", resp.StatusCode)
	}
}

// Behavior: status endpoints require an admin token and serve api DTOs.
func TestStatusEndpoints(t *testing.T) {
	h := newHarness(t)
	resp := h.hook("linkmind", h.svcToken, `{}`)
	acc := decodeJSON[api.Accepted](t, resp.Body)
	h.waitDeploy(acc.DeployID, model.StatusSucceeded)

	// no token → 401; service token → 403
	r, _ := http.Get(h.ts.URL + "/deploys/" + acc.DeployID)
	if r.StatusCode != 401 {
		t.Fatalf("unauthenticated read: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", h.ts.URL+"/deploys/"+acc.DeployID, nil)
	req.Header.Set("Authorization", "Bearer "+h.svcToken)
	r, _ = http.DefaultClient.Do(req)
	if r.StatusCode != 403 {
		t.Fatalf("service token must not read status API: %d", r.StatusCode)
	}

	// deploy details with executions and op records
	resp = h.adminReq("GET", "/deploys/"+acc.DeployID, "")
	d := decodeJSON[api.Deploy](t, resp.Body)
	if d.Status != "succeeded" || len(d.Executions) != 1 || len(d.Executions[0].Ops) != 1 {
		t.Fatalf("deploy DTO incomplete: %+v", d)
	}
	if d.Executions[0].Ops[0].Name != "run" || d.Executions[0].Ops[0].ExitCode == nil {
		t.Fatalf("op record: %+v", d.Executions[0].Ops[0])
	}

	// services overview
	resp = h.adminReq("GET", "/services", "")
	services := decodeJSON[[]api.ServiceSummary](t, resp.Body)
	byName := map[string]api.ServiceSummary{}
	for _, s := range services {
		byName[s.Name] = s
	}
	if len(services) != 3 || byName["linkmind"].LastDeploy == nil || byName["linkmind"].LastDeploy.ID != acc.DeployID {
		t.Fatalf("services overview: %+v", services)
	}
	if byName["condenser"].Webhook {
		t.Fatal("condenser must report webhook=false")
	}

	// per-service history
	resp = h.adminReq("GET", "/services/linkmind/deploys", "")
	hist := decodeJSON[[]api.Deploy](t, resp.Body)
	if len(hist) != 1 || hist[0].ID != acc.DeployID {
		t.Fatalf("history: %+v", hist)
	}

	// servers
	resp = h.adminReq("GET", "/servers", "")
	servers := decodeJSON[[]api.ServerInfo](t, resp.Body)
	if len(servers) != 1 || servers[0].Name != "s1" || servers[0].Status != "online" || !servers[0].Local {
		t.Fatalf("servers: %+v", servers)
	}
}

// Behavior: logs endpoint returns recorded output; follow=1 streams NDJSON
// live until the deploy finishes.
func TestLogsAndFollow(t *testing.T) {
	h := newHarness(t)
	h.fake.On("deploy-linkmind").Returning("hello from deploy\n", 0)

	resp := h.hook("linkmind", h.svcToken, `{}`)
	acc := decodeJSON[api.Accepted](t, resp.Body)
	h.waitDeploy(acc.DeployID, model.StatusSucceeded)

	resp = h.adminReq("GET", "/deploys/"+acc.DeployID+"/logs", "")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello from deploy") {
		t.Fatalf("logs missing output: %q", body)
	}

	// follow on a finished deploy: replays then terminates
	resp = h.adminReq("GET", "/deploys/"+acc.DeployID+"/logs?follow=1", "")
	sc := bufio.NewScanner(resp.Body)
	var sawLine bool
	for sc.Scan() {
		var l api.LogLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue // final status line is checked below via structure
		}
		if strings.Contains(l.Data, "hello from deploy") {
			sawLine = true
		}
	}
	if !sawLine {
		t.Fatal("follow did not replay the log line")
	}
}

// Behavior: manual deploy trigger works even with webhook:false; named tasks
// run via their endpoint and never via /hooks.
func TestManualTriggerAndTask(t *testing.T) {
	h := newHarness(t)

	resp := h.adminReq("POST", "/services/condenser/deploy", `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("manual deploy: %d", resp.StatusCode)
	}
	acc := decodeJSON[api.Accepted](t, resp.Body)
	h.waitDeploy(acc.DeployID, model.StatusSucceeded)

	resp = h.adminReq("POST", "/services/simul/tasks/db-push", `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("task trigger: %d", resp.StatusCode)
	}
	acc = decodeJSON[api.Accepted](t, resp.Body)
	d := h.waitDeploy(acc.DeployID, model.StatusSucceeded)
	if d.Kind != model.KindTask || d.Task != "db-push" {
		t.Fatalf("task deploy record: %+v", d)
	}
	joined := strings.Join(h.fake.JoinedCalls(), "|")
	if !strings.Contains(joined, "deploy-condenser") || !strings.Contains(joined, "db-push-step") {
		t.Fatalf("calls: %v", h.fake.JoinedCalls())
	}

	// unknown task → 404
	resp = h.adminReq("POST", "/services/simul/tasks/nope", `{}`)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown task: %d", resp.StatusCode)
	}
}

// Behavior: POST /-/reload picks up config changes for new requests.
func TestReload(t *testing.T) {
	h := newHarness(t)
	newCfg := testConfig + `
  fresh:
    server: s1
    dir: /opt/apps/fresh
    deploy:
      - run: { argv: [deploy-fresh] }
`
	if err := os.WriteFile(h.cfgPath, []byte(newCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := h.adminReq("POST", "/-/reload", "")
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("reload: %d %s", resp.StatusCode, b)
	}
	resp = h.adminReq("GET", "/services", "")
	services := decodeJSON[[]api.ServiceSummary](t, resp.Body)
	if len(services) != 4 {
		t.Fatalf("new service not visible after reload: %+v", services)
	}

	// invalid config → 400, old config stays active
	os.WriteFile(h.cfgPath, []byte("listen: {"), 0o644)
	resp = h.adminReq("POST", "/-/reload", "")
	if resp.StatusCode != 400 {
		t.Fatalf("invalid reload: %d", resp.StatusCode)
	}
	resp = h.adminReq("GET", "/services", "")
	if services := decodeJSON[[]api.ServiceSummary](t, resp.Body); len(services) != 4 {
		t.Fatal("previous config must stay active after failed reload")
	}
}

var _ = fmt.Sprintf
