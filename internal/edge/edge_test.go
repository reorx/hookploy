package edge_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/edge"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/grpcapi"
	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/pb"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

type mainEnv struct {
	srv   *grpcapi.Server
	reg   *executor.Registry
	gs    *grpc.Server
	addr  string
	token string
}

// newMain starts a real main-side gRPC server on a loopback port.
func newMain(t *testing.T) *mainEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plain := token.New(token.KindServer)
	if err := st.InsertToken(string(token.KindServer), "edge1", token.Hash(plain)); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Servers: map[string]*config.Server{"edge1": {Name: "edge1"}}}
	reg := executor.NewRegistry(2 * time.Second)
	srv := &grpcapi.Server{Store: st, Registry: reg, Config: func() *config.Config { return cfg }}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterHookployServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	return &mainEnv{srv: srv, reg: reg, gs: gs, addr: lis.Addr().String(), token: plain}
}

// startEdge runs an edge against the main env with a fake runner engine.
func startEdge(t *testing.T, m *mainEnv, fr *runner.FakeRunner) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		edge.Run(ctx, edge.Options{
			MainURL:     "http://" + m.addr,
			Token:       m.token,
			Engine:      &engine.Engine{Runner: fr, Sleep: func(context.Context, time.Duration) error { return nil }},
			BackoffBase: 10 * time.Millisecond,
			BackoffMax:  50 * time.Millisecond,
		})
	}()
	t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func steps(t *testing.T, js string) []ops.Step {
	t.Helper()
	var out []ops.Step
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type recordSink struct {
	mu    sync.Mutex
	lines []string
	logs  []string
}

func (r *recordSink) OpStart(i int, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, "start:"+name)
}
func (r *recordSink) OpEnd(i int, name string, exit *int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, "end:"+name)
}
func (r *recordSink) Log(i int, stream, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, stream+":"+data)
}

func TestEdgeConnectsAndReportsVersion(t *testing.T) {
	m := newMain(t)
	startEdge(t, m, &runner.FakeRunner{})
	waitFor(t, "edge online", func() bool {
		_, ok := m.srv.Edges()["edge1"]
		return ok
	})
}

func TestEdgeExecutesAndStreamsBack(t *testing.T) {
	m := newMain(t)
	fr := &runner.FakeRunner{}
	fr.On("docker", "compose", "pull").Returning("pulling images\n", 0)
	startEdge(t, m, fr)

	ex, err := m.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordSink{}
	res, err := ex.Execute(context.Background(), engine.Spec{
		ExecutionID: "ex_ok",
		Service:     "svc",
		Dir:         "/opt/apps/svc",
		Digest:      "sha256:keep",
		Timeout:     time.Minute,
		Steps:       steps(t, `[{"op":"compose.pull"},{"op":"compose.up"}]`),
	}, sink)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Digest != "sha256:keep" {
		t.Fatalf("digest = %q, want sha256:keep", res.Digest)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	want := []string{"start:compose.pull", "end:compose.pull", "start:compose.up", "end:compose.up"}
	if strings.Join(sink.lines, ",") != strings.Join(want, ",") {
		t.Fatalf("op events = %v, want %v", sink.lines, want)
	}
	found := false
	for _, l := range sink.logs {
		if strings.Contains(l, "pulling images") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs did not stream back: %v", sink.logs)
	}

	// commands ran in the service dir on the edge side
	if len(fr.Calls) == 0 || fr.Calls[0].Dir != "/opt/apps/svc" {
		t.Fatalf("edge did not run in service dir: %+v", fr.ArgvList())
	}
}

func TestEdgeReportsOpFailure(t *testing.T) {
	m := newMain(t)
	fr := &runner.FakeRunner{}
	fr.On("docker", "compose", "up").Returning("boom\n", 1)
	startEdge(t, m, fr)

	ex, err := m.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ex.Execute(context.Background(), engine.Spec{
		ExecutionID: "ex_fail",
		Service:     "svc",
		Dir:         "/x",
		Timeout:     time.Minute,
		Steps:       steps(t, `[{"op":"compose.up"}]`),
	}, &recordSink{})
	if err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("want op failure with exit 1, got %v", err)
	}
}

func TestEdgeEnforcesTimeout(t *testing.T) {
	m := newMain(t)
	fr := &runner.FakeRunner{}
	fr.On("docker", "compose", "up").BlockUntilCancel = true
	startEdge(t, m, fr)

	ex, err := m.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	// no deadline on the main-side ctx: the edge must cut it off itself
	_, err = ex.Execute(context.Background(), engine.Spec{
		ExecutionID: "ex_to",
		Service:     "svc",
		Dir:         "/x",
		Timeout:     150 * time.Millisecond,
		Steps:       steps(t, `[{"op":"compose.up"}]`),
	}, &recordSink{})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("want edge-side timeout, got %v", err)
	}
}

func TestEdgeReconnects(t *testing.T) {
	m := newMain(t)
	startEdge(t, m, &runner.FakeRunner{})
	waitFor(t, "edge online", func() bool { return len(m.srv.Edges()) == 1 })

	// take main down; edge must fall offline
	m.gs.Stop()
	waitFor(t, "edge offline", func() bool { return len(m.srv.Edges()) == 0 })

	// bring main back on the same port; edge must reconnect by itself
	lis, err := net.Listen("tcp", m.addr)
	if err != nil {
		t.Fatal(err)
	}
	gs2 := grpc.NewServer()
	pb.RegisterHookployServer(gs2, m.srv)
	go gs2.Serve(lis)
	t.Cleanup(gs2.Stop)
	waitFor(t, "edge reconnected", func() bool { return len(m.srv.Edges()) == 1 })
}

func TestEdgeRunFailsOnBadURL(t *testing.T) {
	err := edge.Run(context.Background(), edge.Options{MainURL: "ftp://x", Token: "t"})
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
	var ctxErr error = err
	if errors.Is(ctxErr, context.Canceled) {
		t.Fatal("should be a config error, not context")
	}
}
