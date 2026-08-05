package edge_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/edge"
	"github.com/reorx/hookploy/internal/edgehub"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/httpapi"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/scheduler"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

const sseTestConfig = `
servers:
  e1: {}
services:
  app:
    server: e1
    dir: /opt/apps/app
    timeout: 30s
    deploy:
      - run: { argv: [deploy-app] }
`

// ── a main you can cut the cable to ────────────────────────────────────────

// cutProxy forwards TCP to main and can sever every live connection while
// still accepting new ones — what a proxy that drops idle streams looks like
// from the edge.
type cutProxy struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	conns []net.Conn
}

func newCutProxy(t *testing.T, targetURL string) *cutProxy {
	t.Helper()
	u, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: u.Host}
	go p.serve()
	t.Cleanup(func() { ln.Close(); p.cut() })
	return p
}

func (p *cutProxy) url() string { return "http://" + p.ln.Addr().String() }

func (p *cutProxy) serve() {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.pipe(client)
	}
}

func (p *cutProxy) pipe(client net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	p.conns = append(p.conns, client, upstream)
	p.mu.Unlock()

	go func() { _, _ = io.Copy(upstream, client); upstream.Close() }()
	_, _ = io.Copy(client, upstream)
	client.Close()
}

// cut severs every connection currently open through the proxy.
func (p *cutProxy) cut() {
	p.mu.Lock()
	conns := p.conns
	p.conns = nil
	p.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// ── harness ────────────────────────────────────────────────────────────────

type sseMain struct {
	t     *testing.T
	ts    *httptest.Server
	proxy *cutProxy
	store *store.Store
	hub   *edgehub.Hub
	token string // e1's server token
	hook  string // app's service token
}

func newSSEMain(t *testing.T, grace time.Duration) *sseMain {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hookploy.yaml")
	if err := os.WriteFile(cfgPath, []byte(sseTestConfig), 0o644); err != nil {
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

	reg := executor.NewRegistry(5 * time.Second)
	hub := edgehub.New(reg, nil)
	hub.GraceWindow = grace
	sched := scheduler.New(st, reg)
	t.Cleanup(sched.Shutdown)

	srv := &httpapi.Server{
		Store:         st,
		Sched:         sched,
		Config:        func() *config.Config { return cfg },
		Hub:           hub,
		EdgeHeartbeat: 200 * time.Millisecond,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	m := &sseMain{t: t, ts: ts, proxy: newCutProxy(t, ts.URL), store: st, hub: hub}
	m.token = token.New(token.KindServer)
	st.InsertToken(string(token.KindServer), "e1", token.Hash(m.token))
	m.hook = token.New(token.KindService)
	st.InsertToken(string(token.KindService), "app", token.Hash(m.hook))
	return m
}

// startEdgeSSE runs a real edge against main through the cuttable proxy.
func (m *sseMain) startEdgeSSE(fr *runner.FakeRunner) context.CancelFunc {
	m.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := edge.Run(ctx, edge.Options{
			MainURL:     m.proxy.url(),
			Token:       m.token,
			Transport:   edge.TransportSSE,
			Engine:      &engine.Engine{Runner: fr, Sleep: func(context.Context, time.Duration) error { return nil }},
			Logger:      log.New(io.Discard, "", 0),
			BackoffBase: 20 * time.Millisecond,
			BackoffMax:  100 * time.Millisecond,
		})
		if err != nil {
			m.t.Errorf("edge.Run: %v", err)
		}
	}()
	m.t.Cleanup(func() { cancel(); <-done })
	return cancel
}

// trigger fires the webhook and returns the deploy id.
func (m *sseMain) trigger() string {
	m.t.Helper()
	req, _ := http.NewRequest("POST", m.ts.URL+"/hooks/app", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+m.hook)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		m.t.Fatalf("webhook = %d: %s", resp.StatusCode, body)
	}
	var acc api.Accepted
	if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
		m.t.Fatal(err)
	}
	return acc.DeployID
}

func (m *sseMain) waitDeploy(id string, want model.Status) {
	m.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := m.store.GetDeploy(id)
		if d != nil && d.Status == want {
			return
		}
		if d != nil && d.Status.Terminal() && d.Status != want {
			m.t.Fatalf("deploy %s reached %s, want %s (error: %s)", id, d.Status, want, d.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.t.Fatalf("timed out waiting for deploy %s to reach %s", id, want)
}

func (m *sseMain) waitOnline() {
	m.t.Helper()
	waitFor(m.t, "edge online over sse", func() bool {
		info, ok := m.hub.Edges()["e1"]
		return ok && info.Transport == "sse"
	})
}

// ── behavior ───────────────────────────────────────────────────────────────

// Behavior: a deploy travels the whole SSE path — webhook, scheduler, hub,
// session stream, edge engine, result POST — and lands as succeeded.
func TestSSEDeploySucceedsEndToEnd(t *testing.T) {
	m := newSSEMain(t, edgehub.DefaultGraceWindow)
	fr := &runner.FakeRunner{}
	fr.On("deploy-app").Returning("deploying\n", 0)
	m.startEdgeSSE(fr)
	m.waitOnline()

	id := m.trigger()
	m.waitDeploy(id, model.StatusSucceeded)

	if calls := fr.JoinedCalls(); len(calls) != 1 || calls[0] != "deploy-app" {
		t.Fatalf("edge ran %v, want [deploy-app]", calls)
	}
	logs, err := m.store.GetDeployLogs(id)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, l := range logs {
		sb.WriteString(l.Data)
	}
	if !strings.Contains(sb.String(), "deploying") {
		t.Fatalf("logs did not stream back over SSE: %q", sb.String())
	}
}

// Behavior (the case this whole transport exists for): the proxy cuts the
// session stream mid-deploy. The execution keeps running on the edge, the
// edge reconnects and re-reports the result, and the deploy is recorded as
// what it actually was — succeeded, not failed.
func TestSSEDeploySurvivesStreamCut(t *testing.T) {
	m := newSSEMain(t, edgehub.DefaultGraceWindow)
	gate := make(chan struct{})
	fr := &runner.FakeRunner{}
	fr.On("deploy-app").Effect = func(runner.Cmd) error {
		<-gate
		return nil
	}
	m.startEdgeSSE(fr)
	m.waitOnline()

	id := m.trigger()
	waitFor(t, "execution started on the edge", func() bool { return len(fr.ArgvList()) > 0 })

	m.proxy.cut() // the stream dies while the deploy is running
	waitFor(t, "main to see the stream drop", func() bool {
		_, online := m.hub.Edges()["e1"]
		return !online
	})
	waitFor(t, "edge to reconnect", func() bool {
		info, ok := m.hub.Edges()["e1"]
		return ok && info.Transport == "sse"
	})

	close(gate) // the work finishes on an edge that never stopped
	m.waitDeploy(id, model.StatusSucceeded)
}

// Behavior: an edge that goes away mid-deploy and never comes back leaves
// the deploy unreachable — main never learned the outcome, so it must not
// report a failure it did not observe.
func TestSSEDeployUnreachableWhenEdgeNeverReturns(t *testing.T) {
	m := newSSEMain(t, 300*time.Millisecond)
	gate := make(chan struct{})
	defer close(gate)
	fr := &runner.FakeRunner{}
	fr.On("deploy-app").Effect = func(runner.Cmd) error {
		<-gate
		return nil
	}
	stop := m.startEdgeSSE(fr)
	m.waitOnline()

	id := m.trigger()
	waitFor(t, "execution started on the edge", func() bool { return len(fr.ArgvList()) > 0 })

	stop() // the edge host dies
	m.waitDeploy(id, model.StatusUnreachable)
}
