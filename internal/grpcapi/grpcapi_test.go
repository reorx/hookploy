package grpcapi_test

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
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/grpcapi"
	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/pb"
	"github.com/reorx/hookploy/internal/store"
	"github.com/reorx/hookploy/internal/token"
)

type env struct {
	client pb.HookployClient
	reg    *executor.Registry
	srv    *grpcapi.Server
	tokens map[string]string // server name → plaintext token
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tokens := map[string]string{}
	for _, name := range []string{"edge1", "edge2"} {
		plain := token.New(token.KindServer)
		if err := st.InsertToken(string(token.KindServer), name, token.Hash(plain)); err != nil {
			t.Fatal(err)
		}
		tokens[name] = plain
	}
	// a token whose subject is not in config
	ghost := token.New(token.KindServer)
	if err := st.InsertToken(string(token.KindServer), "ghost", token.Hash(ghost)); err != nil {
		t.Fatal(err)
	}
	tokens["ghost"] = ghost

	cfg := &config.Config{Servers: map[string]*config.Server{
		"edge1": {Name: "edge1"},
		"edge2": {Name: "edge2"},
	}}

	reg := executor.NewRegistry(200 * time.Millisecond)
	srv := &grpcapi.Server{
		Store:    st,
		Registry: reg,
		Config:   func() *config.Config { return cfg },
	}

	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	pb.RegisterHookployServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return &env{client: pb.NewHookployClient(conn), reg: reg, srv: srv, tokens: tokens}
}

// hello opens a session and performs the handshake.
func (e *env) hello(t *testing.T, server, tok, version string) pb.Hookploy_SessionClient {
	t.Helper()
	stream, err := e.client.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&pb.EdgeMessage{Msg: &pb.EdgeMessage_Hello{Hello: &pb.Hello{
		Server: server, Token: tok, Version: version,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func recvAck(t *testing.T, stream pb.Hookploy_SessionClient) *pb.HelloAck {
	t.Helper()
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("expected ack, got error: %v", err)
	}
	ack := msg.GetAck()
	if ack == nil {
		t.Fatalf("expected ack, got %v", msg)
	}
	return ack
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHelloRegistersEdge(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", e.tokens["edge1"], "v1.2.3")
	recvAck(t, stream)

	edges := e.srv.Edges()
	info, ok := edges["edge1"]
	if !ok {
		t.Fatal("edge1 not tracked as online")
	}
	if info.Version != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", info.Version)
	}
	if _, err := e.reg.Acquire(context.Background(), "edge1"); err != nil {
		t.Fatalf("executor not registered: %v", err)
	}
}

func TestHelloRejectsBadToken(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", "hps_wrong", "v1")
	if _, err := stream.Recv(); err == nil {
		t.Fatal("expected auth error, got message")
	}
	if len(e.srv.Edges()) != 0 {
		t.Fatal("edge must not be tracked after failed auth")
	}
}

func TestHelloRejectsSubjectMismatch(t *testing.T) {
	e := newEnv(t)
	// edge2's token presented for server name edge1
	stream := e.hello(t, "edge1", e.tokens["edge2"], "v1")
	if _, err := stream.Recv(); err == nil {
		t.Fatal("expected auth error, got message")
	}
}

func TestHelloRejectsServerNotInConfig(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "ghost", e.tokens["ghost"], "v1")
	if _, err := stream.Recv(); err == nil {
		t.Fatal("expected error for unknown server, got message")
	}
}

func TestHelloRejectsNonHelloFirstMessage(t *testing.T) {
	e := newEnv(t)
	stream, err := e.client.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = stream.Send(&pb.EdgeMessage{Msg: &pb.EdgeMessage_Update{Update: &pb.ExecUpdate{ExecutionId: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("expected error, got message")
	}
}

func TestDisconnectUnregistersEdge(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", e.tokens["edge1"], "v1")
	recvAck(t, stream)
	stream.CloseSend()
	waitFor(t, "edge offline", func() bool { return len(e.srv.Edges()) == 0 })
	if _, err := e.reg.Acquire(context.Background(), "edge1"); !errors.Is(err, executor.ErrUnreachable) {
		t.Fatalf("expected unreachable after disconnect, got %v", err)
	}
}

func TestReconnectReplacesSession(t *testing.T) {
	e := newEnv(t)
	s1 := e.hello(t, "edge1", e.tokens["edge1"], "v1")
	recvAck(t, s1)
	s2 := e.hello(t, "edge1", e.tokens["edge1"], "v2")
	recvAck(t, s2)
	// closing the old session must not knock the new one offline
	s1.CloseSend()
	time.Sleep(50 * time.Millisecond)
	info, ok := e.srv.Edges()["edge1"]
	if !ok {
		t.Fatal("edge1 should still be online via the second session")
	}
	if info.Version != "v2" {
		t.Fatalf("version = %q, want v2 (new session)", info.Version)
	}
	if _, err := e.reg.Acquire(context.Background(), "edge1"); err != nil {
		t.Fatalf("executor should still be registered: %v", err)
	}
}

// fakeEdge runs a scripted edge behind a handshaked stream: it receives
// Execution messages and passes them to handle.
func fakeEdge(t *testing.T, stream pb.Hookploy_SessionClient, handle func(*pb.Execution, func(*pb.ExecUpdate))) {
	t.Helper()
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			exec := msg.GetExec()
			if exec == nil {
				continue
			}
			send := func(u *pb.ExecUpdate) {
				u.ExecutionId = exec.ExecutionId
				_ = stream.Send(&pb.EdgeMessage{Msg: &pb.EdgeMessage_Update{Update: u}})
			}
			go handle(exec, send)
		}
	}()
}

type sinkEvent struct {
	kind   string // start | end | log
	idx    int
	name   string
	stream string
	data   string
}

type recordSink struct {
	mu     sync.Mutex
	events []sinkEvent
}

func (r *recordSink) OpStart(i int, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sinkEvent{kind: "start", idx: i, name: name})
}
func (r *recordSink) OpEnd(i int, name string, exit *int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sinkEvent{kind: "end", idx: i, name: name})
}
func (r *recordSink) Log(i int, stream, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, sinkEvent{kind: "log", idx: i, stream: stream, data: data})
}

func testSpec(t *testing.T) engine.Spec {
	t.Helper()
	var steps []ops.Step
	if err := json.Unmarshal([]byte(`[{"op":"compose.pull"},{"op":"compose.up"}]`), &steps); err != nil {
		t.Fatal(err)
	}
	return engine.Spec{
		ExecutionID: "ex_1",
		Service:     "svc",
		Instance:    "svc",
		Dir:         "/opt/apps/svc",
		Image:       "ghcr.io/x/svc",
		Digest:      "",
		Steps:       steps,
	}
}

func TestRemoteExecuteSuccess(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", e.tokens["edge1"], "v1")
	recvAck(t, stream)

	var gotExec *pb.Execution
	var mu sync.Mutex
	fakeEdge(t, stream, func(exec *pb.Execution, send func(*pb.ExecUpdate)) {
		mu.Lock()
		gotExec = exec
		mu.Unlock()
		send(&pb.ExecUpdate{Event: &pb.ExecUpdate_OpStart{OpStart: &pb.OpStart{Index: 0, Name: "compose.pull"}}})
		send(&pb.ExecUpdate{Event: &pb.ExecUpdate_Log{Log: &pb.LogChunk{Index: 0, Stream: "stdout", Data: []byte("pulling\n")}}})
		zero := int32(0)
		send(&pb.ExecUpdate{Event: &pb.ExecUpdate_OpEnd{OpEnd: &pb.OpEnd{Index: 0, Name: "compose.pull", ExitCode: &zero}}})
		send(&pb.ExecUpdate{Event: &pb.ExecUpdate_Done{Done: &pb.ExecDone{Ok: true, Digest: "sha256:abc"}}})
	})

	ex, err := e.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordSink{}
	res, err := ex.Execute(context.Background(), testSpec(t), sink)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res.Digest != "sha256:abc" {
		t.Fatalf("digest = %q, want sha256:abc", res.Digest)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotExec == nil {
		t.Fatal("edge never received the execution")
	}
	if gotExec.Dir != "/opt/apps/svc" || gotExec.Service != "svc" {
		t.Fatalf("exec fields wrong: %+v", gotExec)
	}
	var steps []ops.Step
	if err := json.Unmarshal(gotExec.OpsJson, &steps); err != nil {
		t.Fatalf("ops_json does not round-trip: %v", err)
	}
	if len(steps) != 2 || steps[0].Op != "compose.pull" {
		t.Fatalf("steps wrong: %+v", steps)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var kinds []string
	for _, ev := range sink.events {
		kinds = append(kinds, ev.kind)
	}
	if strings.Join(kinds, ",") != "start,log,end" {
		t.Fatalf("sink events = %v", kinds)
	}
	if sink.events[1].data != "pulling\n" {
		t.Fatalf("log data = %q", sink.events[1].data)
	}
}

func TestRemoteExecuteFailure(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", e.tokens["edge1"], "v1")
	recvAck(t, stream)
	fakeEdge(t, stream, func(exec *pb.Execution, send func(*pb.ExecUpdate)) {
		send(&pb.ExecUpdate{Event: &pb.ExecUpdate_Done{Done: &pb.ExecDone{Ok: false, Error: "op 1 (compose.pull): exit 1", Digest: "sha256:def"}}})
	})
	ex, err := e.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Execute(context.Background(), testSpec(t), &recordSink{})
	if err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("expected op failure error, got %v", err)
	}
	// digest still reported so wave-1 promotion works even on failure
	if res.Digest != "sha256:def" {
		t.Fatalf("digest = %q, want sha256:def", res.Digest)
	}
}

func TestRemoteExecuteEdgeDisconnect(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", e.tokens["edge1"], "v1")
	recvAck(t, stream)
	fakeEdge(t, stream, func(exec *pb.Execution, send func(*pb.ExecUpdate)) {
		send(&pb.ExecUpdate{Event: &pb.ExecUpdate_OpStart{OpStart: &pb.OpStart{Index: 0, Name: "compose.pull"}}})
		stream.CloseSend()
	})
	ex, err := e.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ex.Execute(context.Background(), testSpec(t), &recordSink{})
	if err == nil {
		t.Fatal("expected error when edge disconnects mid-execution")
	}
}

func TestRemoteExecuteContextTimeout(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", e.tokens["edge1"], "v1")
	recvAck(t, stream)
	fakeEdge(t, stream, func(exec *pb.Execution, send func(*pb.ExecUpdate)) {
		// edge never finishes
	})
	ex, err := e.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = ex.Execute(ctx, testSpec(t), &recordSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestConcurrentExecutions(t *testing.T) {
	e := newEnv(t)
	stream := e.hello(t, "edge1", e.tokens["edge1"], "v1")
	recvAck(t, stream)
	fakeEdge(t, stream, func(exec *pb.Execution, send func(*pb.ExecUpdate)) {
		send(&pb.ExecUpdate{Event: &pb.ExecUpdate_Done{Done: &pb.ExecDone{Ok: true, Digest: "sha256:" + exec.Service}}})
	})
	ex, err := e.reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, svc := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(svc string) {
			defer wg.Done()
			spec := testSpec(t)
			spec.ExecutionID = "ex_" + svc
			spec.Service = svc
			res, err := ex.Execute(context.Background(), spec, &recordSink{})
			if err != nil {
				t.Errorf("execute %s: %v", svc, err)
				return
			}
			if res.Digest != "sha256:"+svc {
				t.Errorf("execution %s got digest %q — updates crossed streams", svc, res.Digest)
			}
		}(svc)
	}
	wg.Wait()
}
