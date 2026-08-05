package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/edgehub"
	"github.com/reorx/hookploy/internal/edgewire"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

const edgeTestConfig = `
servers:
  s1: { local: true }
  e1: {}
services:
  app:
    server: e1
    dir: /opt/apps/app
    deploy:
      - run: { argv: [deploy-app] }
`

type edgeHarness struct {
	t          *testing.T
	ts         *httptest.Server
	store      *store.Store
	hub        *edgehub.Hub
	reg        *executor.Registry
	srvToken   string // e1's server token
	ghostToken string // a server token whose subject is not in the config
	adminToken string
}

func newEdgeHarness(t *testing.T, heartbeat time.Duration) *edgeHarness {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hookploy.yaml")
	if err := os.WriteFile(cfgPath, []byte(edgeTestConfig), 0o644); err != nil {
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

	reg := executor.NewRegistry(2 * time.Second)
	hub := edgehub.New(reg, nil)
	hub.GraceWindow = 200 * time.Millisecond
	srv := &Server{
		Store:         st,
		Config:        func() *config.Config { return cfg },
		Hub:           hub,
		EdgeHeartbeat: heartbeat,
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &edgeHarness{t: t, ts: ts, store: st, hub: hub, reg: reg}
	h.srvToken = token.New(token.KindServer)
	st.InsertToken(string(token.KindServer), "e1", token.Hash(h.srvToken))
	h.ghostToken = token.New(token.KindServer)
	st.InsertToken(string(token.KindServer), "ghost", token.Hash(h.ghostToken))
	h.adminToken = token.New(token.KindAdmin)
	st.InsertToken(string(token.KindAdmin), "admin", token.Hash(h.adminToken))
	return h
}

// ── SSE client ─────────────────────────────────────────────────────────────

type sseClient struct {
	resp *http.Response
	br   *bufio.Reader
}

type frame struct {
	name string
	data []byte
}

// open starts a session stream. inflight advertises executions the edge
// claims to still hold.
func (h *edgeHarness) open(tok string, query string) (*sseClient, *http.Response) {
	h.t.Helper()
	url := h.ts.URL + "/edge/session"
	if query != "" {
		url += "?" + query
	}
	req, _ := http.NewRequest("GET", url, nil)
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp
	}
	c := &sseClient{resp: resp, br: bufio.NewReader(resp.Body)}
	h.t.Cleanup(func() { resp.Body.Close() })
	return c, resp
}

// next reads one frame; a comment (heartbeat) comes back with an empty name.
func (c *sseClient) next() (frame, error) {
	var f frame
	for {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return frame{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if f.name == "" && f.data == nil {
				continue
			}
			return f, nil
		case strings.HasPrefix(line, ":"):
			return frame{}, nil
		case strings.HasPrefix(line, "event: "):
			f.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.data = []byte(strings.TrimPrefix(line, "data: "))
		}
	}
}

// nextNamed skips heartbeats.
func (c *sseClient) nextNamed(t *testing.T) frame {
	t.Helper()
	for {
		f, err := c.next()
		if err != nil {
			t.Fatalf("reading session stream: %v", err)
		}
		if f.name != "" {
			return f
		}
	}
}

func (h *edgeHarness) hello(c *sseClient) edgewire.Hello {
	h.t.Helper()
	f := c.nextNamed(h.t)
	if f.name != edgewire.EventHello {
		h.t.Fatalf("first frame = %q, want hello", f.name)
	}
	var hello edgewire.Hello
	if err := json.Unmarshal(f.data, &hello); err != nil {
		h.t.Fatal(err)
	}
	return hello
}

func (h *edgeHarness) post(tok, path, body string) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.ts.URL+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(3 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ── behavior ───────────────────────────────────────────────────────────────

// Behavior: the edge endpoints take a server token and nothing else — not an
// admin token, and not a token whose server is absent from the config.
func TestEdgeEndpointsAuthMatrix(t *testing.T) {
	h := newEdgeHarness(t, 0)
	cases := []struct {
		name string
		tok  string
		want int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"garbage token", "hps_nope", http.StatusUnauthorized},
		{"admin token", h.adminToken, http.StatusUnauthorized},
		{"server not in config", h.ghostToken, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, resp := h.open(tc.tok, ""); resp.StatusCode != tc.want {
				t.Errorf("GET /edge/session = %d, want %d", resp.StatusCode, tc.want)
			}
			resp := h.post(tc.tok, "/edge/executions/ex_1/done", `{"ok":true}`)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("POST done = %d, want %d", resp.StatusCode, tc.want)
			}
			resp = h.post(tc.tok, "/edge/executions/ex_1/updates", `{"updates":[]}`)
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("POST updates = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// Behavior: a ?server= that disagrees with the token's subject is rejected —
// the name always comes from the token.
func TestEdgeSessionRejectsServerMismatch(t *testing.T) {
	h := newEdgeHarness(t, 0)
	if _, resp := h.open(h.srvToken, "server=someone-else"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

// Behavior: the stream opens with a hello naming the server, and keeps
// emitting heartbeats so proxies never see it as idle.
func TestEdgeSessionHelloAndHeartbeat(t *testing.T) {
	h := newEdgeHarness(t, 30*time.Millisecond)
	c, resp := h.open(h.srvToken, "version=v9.9.9")
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatal("X-Accel-Buffering: no is required or proxies will buffer the stream")
	}
	hello := h.hello(c)
	if hello.Server != "e1" {
		t.Fatalf("hello.server = %q, want e1", hello.Server)
	}

	// The edge is online, tagged with the transport it arrived on.
	waitUntil(t, "edge online", func() bool { return len(h.hub.Edges()) == 1 })
	info := h.hub.Edges()["e1"]
	if info.Version != "v9.9.9" || info.Transport != "sse" {
		t.Fatalf("edge info = %+v, want version v9.9.9 over sse", info)
	}

	for i := 0; i < 2; i++ {
		f, err := c.next()
		if err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
		if f.name != "" {
			t.Fatalf("heartbeat %d: got a %q frame", i, f.name)
		}
	}
}

// Behavior: the full round trip — main dispatches over the stream, the edge
// reports progress and the result over POSTs, and the execution settles.
func TestEdgeSessionDeliversExecutionAndTakesResult(t *testing.T) {
	h := newEdgeHarness(t, 0)
	c, _ := h.open(h.srvToken, "")
	h.hello(c)

	ex, err := h.reg.Acquire(context.Background(), "e1")
	if err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	type outcome struct {
		digest string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := ex.Execute(context.Background(), engine.Spec{
			ExecutionID: "ex_1", Service: "app", Dir: "/opt/apps/app", Timeout: time.Minute,
			Steps: testSteps(t),
		}, sink)
		done <- outcome{res.Digest, err}
	}()

	f := c.nextNamed(t)
	if f.name != edgewire.EventExec {
		t.Fatalf("frame = %q, want exec", f.name)
	}
	var got edgewire.Exec
	if err := json.Unmarshal(f.data, &got); err != nil {
		t.Fatal(err)
	}
	if got.ExecutionID != "ex_1" || got.Dir != "/opt/apps/app" || got.TimeoutMS != 60000 {
		t.Fatalf("exec frame = %+v", got)
	}
	if !strings.Contains(string(got.Ops), "compose.pull") {
		t.Fatalf("ops did not round-trip: %s", got.Ops)
	}

	resp := h.post(h.srvToken, "/edge/executions/ex_1/updates",
		`{"updates":[{"kind":"op_start","index":0,"name":"compose.pull"},`+
			`{"kind":"log","index":0,"stream":"stdout","data":"pulling\n"},`+
			`{"kind":"op_end","index":0,"name":"compose.pull","exit_code":0}]}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST updates = %d", resp.StatusCode)
	}

	resp = h.post(h.srvToken, "/edge/executions/ex_1/done", `{"ok":true,"digest":"sha256:abc"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST done = %d", resp.StatusCode)
	}

	out := <-done
	if out.err != nil {
		t.Fatalf("execute: %v", out.err)
	}
	if out.digest != "sha256:abc" {
		t.Fatalf("digest = %q", out.digest)
	}
	if want := "start:compose.pull,log:pulling\n,end:compose.pull"; sink.joined() != want {
		t.Fatalf("sink events = %q, want %q", sink.joined(), want)
	}
}

// Behavior: a result main no longer knows about is still acknowledged — the
// edge must be able to stop holding it.
func TestEdgeDoneForUnknownExecutionIsAcknowledged(t *testing.T) {
	h := newEdgeHarness(t, 0)
	c, _ := h.open(h.srvToken, "")
	h.hello(c)

	resp := h.post(h.srvToken, "/edge/executions/ex_forgotten/done", `{"ok":true}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST done for an unknown execution = %d, want 200", resp.StatusCode)
	}
}

// Behavior: a result can land while the download stream is down — that is
// exactly the case the SSE split exists to survive.
func TestEdgeDoneAcceptedWithoutASession(t *testing.T) {
	h := newEdgeHarness(t, 0)
	c, _ := h.open(h.srvToken, "")
	h.hello(c)

	ex, err := h.reg.Acquire(context.Background(), "e1")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ex.Execute(context.Background(), engine.Spec{
			ExecutionID: "ex_1", Service: "app", Steps: testSteps(t),
		}, &captureSink{})
		done <- err
	}()
	c.nextNamed(t) // the exec frame reached the edge

	c.resp.Body.Close() // the stream breaks
	waitUntil(t, "edge offline", func() bool { return len(h.hub.Edges()) == 0 })

	resp := h.post(h.srvToken, "/edge/executions/ex_1/done", `{"ok":true,"digest":"sha256:abc"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST done = %d", resp.StatusCode)
	}
	if err := <-done; err != nil {
		t.Fatalf("execution should have settled from the out-of-band result: %v", err)
	}
}

// Behavior: one server has one stream. A second session takes over and the
// first is closed, so executions can never be dispatched to a stale reader.
func TestSecondEdgeSessionReplacesTheFirst(t *testing.T) {
	h := newEdgeHarness(t, 0)
	c1, _ := h.open(h.srvToken, "version=v1")
	h.hello(c1)

	c2, _ := h.open(h.srvToken, "version=v2")
	h.hello(c2)

	closed := make(chan error, 1)
	go func() {
		_, err := c1.next()
		closed <- err
	}()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("the first stream kept delivering frames after being replaced")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the first stream was not closed when the second session attached")
	}
	waitUntil(t, "second session is current", func() bool {
		return h.hub.Edges()["e1"].Version == "v2"
	})
}

// ── helpers ────────────────────────────────────────────────────────────────

type captureSink struct {
	mu     sync.Mutex
	events []string
}

func (c *captureSink) OpStart(i int, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "start:"+name)
}

func (c *captureSink) OpEnd(i int, name string, exit *int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "end:"+name)
}

func (c *captureSink) Log(i int, stream, data string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, "log:"+data)
}

func (c *captureSink) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.events, ",")
}

func testSteps(t *testing.T) []ops.Step {
	t.Helper()
	var steps []ops.Step
	if err := json.Unmarshal([]byte(`[{"op":"compose.pull"},{"op":"compose.up"}]`), &steps); err != nil {
		t.Fatal(err)
	}
	return steps
}
