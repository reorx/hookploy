package edgehub_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/edgehub"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/ops"
)

// fakeConn is an edge connection with no wire behind it.
type fakeConn struct {
	mu      sync.Mutex
	sent    []engine.Spec
	closed  int
	sendErr error
}

func (c *fakeConn) SendExecution(spec engine.Spec) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendErr != nil {
		return c.sendErr
	}
	c.sent = append(c.sent, spec)
	return nil
}

func (c *fakeConn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
}

func (c *fakeConn) sentIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		out = append(out, s.ExecutionID)
	}
	return out
}

func (c *fakeConn) closeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type recordSink struct {
	mu     sync.Mutex
	events []string
}

func (r *recordSink) OpStart(i int, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "start:"+name)
}

func (r *recordSink) OpEnd(i int, name string, exit *int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "end:"+name)
}

func (r *recordSink) Log(i int, stream, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "log:"+data)
}

func (r *recordSink) joined() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.events, ",")
}

// newHub builds a hub that settles executions the moment their edge goes
// away; grace behaviour has its own tests.
func newHub(t *testing.T) (*edgehub.Hub, *executor.Registry) {
	t.Helper()
	return newHubGrace(t, 0)
}

func newHubGrace(t *testing.T, grace time.Duration) (*edgehub.Hub, *executor.Registry) {
	t.Helper()
	reg := executor.NewRegistry(100 * time.Millisecond)
	h := edgehub.New(reg, nil)
	h.GraceWindow = grace
	return h, reg
}

func attach(h *edgehub.Hub, server, version string, inflight ...string) (*edgehub.Attachment, *fakeConn) {
	conn := &fakeConn{}
	att := h.Attach(edgehub.AttachInfo{
		Server: server, Version: version, Transport: "test", Inflight: inflight,
	}, conn)
	return att, conn
}

// waitSent blocks until the connection has received an execution.
func waitSent(t *testing.T, conn *fakeConn, execID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, id := range conn.sentIDs() {
			if id == execID {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("execution %s was never sent to the edge", execID)
}

func TestAttachRegistersEdge(t *testing.T) {
	h, reg := newHub(t)
	attach(h, "edge1", "v1.2.3")

	info, ok := h.Edges()["edge1"]
	if !ok {
		t.Fatal("edge1 not reported as attached")
	}
	if info.Version != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", info.Version)
	}
	if _, err := reg.Acquire(context.Background(), "edge1"); err != nil {
		t.Fatalf("executor not registered: %v", err)
	}
}

func TestDetachUnregistersEdge(t *testing.T) {
	h, reg := newHub(t)
	att, _ := attach(h, "edge1", "v1")
	att.Detach()

	if len(h.Edges()) != 0 {
		t.Fatal("edge must not be reported after detach")
	}
	if _, err := reg.Acquire(context.Background(), "edge1"); !errors.Is(err, executor.ErrUnreachable) {
		t.Fatalf("want unreachable after detach, got %v", err)
	}
}

func TestExecuteStreamsUpdatesAndSettles(t *testing.T) {
	h, reg := newHub(t)
	_, conn := attach(h, "edge1", "v1")
	ex, err := reg.Acquire(context.Background(), "edge1")
	if err != nil {
		t.Fatal(err)
	}

	sink := &recordSink{}
	type outcome struct {
		res engine.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := ex.Execute(context.Background(), engine.Spec{ExecutionID: "ex_1", Digest: ""}, sink)
		done <- outcome{res, err}
	}()

	waitSent(t, conn, "ex_1")
	zero := 0
	h.HandleOpStart("edge1", "ex_1", 0, "compose.pull")
	h.HandleLog("edge1", "ex_1", 0, "stdout", "pulling")
	h.HandleOpEnd("edge1", "ex_1", 0, "compose.pull", &zero, nil)
	h.HandleDone("edge1", "ex_1", true, "", "sha256:abc")

	got := <-done
	if got.err != nil {
		t.Fatalf("execute: %v", got.err)
	}
	if got.res.Digest != "sha256:abc" {
		t.Fatalf("digest = %q, want sha256:abc", got.res.Digest)
	}
	if want := "start:compose.pull,log:pulling,end:compose.pull"; sink.joined() != want {
		t.Fatalf("sink events = %q, want %q", sink.joined(), want)
	}
}

func TestExecuteFailureKeepsDigest(t *testing.T) {
	h, reg := newHub(t)
	_, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")

	done := make(chan error, 1)
	digest := make(chan string, 1)
	go func() {
		res, err := ex.Execute(context.Background(), engine.Spec{ExecutionID: "ex_1"}, &recordSink{})
		digest <- res.Digest
		done <- err
	}()
	waitSent(t, conn, "ex_1")
	h.HandleDone("edge1", "ex_1", false, "op 1 (compose.pull): exit 1", "sha256:def")

	if err := <-done; err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("want op failure, got %v", err)
	}
	if d := <-digest; d != "sha256:def" {
		t.Fatalf("digest = %q, want sha256:def (needed for wave-1 promotion)", d)
	}
}

func TestDetachWithoutGraceFailsInflightExecutions(t *testing.T) {
	h, reg := newHub(t)
	att, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")

	done := make(chan error, 1)
	go func() {
		_, err := ex.Execute(context.Background(), engine.Spec{ExecutionID: "ex_1"}, &recordSink{})
		done <- err
	}()
	waitSent(t, conn, "ex_1")
	att.Detach()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "disconnected") {
			t.Fatalf("want disconnect error, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("execution did not settle when the edge detached")
	}
}

func TestExecuteWithoutAttachedEdgeIsUnreachable(t *testing.T) {
	h, reg := newHub(t)
	att, _ := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")
	att.Detach()

	// A stale executor handle must not silently hang: the scheduler needs to
	// see this as unreachable, not as a running execution.
	_, err := ex.Execute(context.Background(), engine.Spec{ExecutionID: "ex_1"}, &recordSink{})
	if !errors.Is(err, executor.ErrUnreachable) {
		t.Fatalf("want unreachable, got %v", err)
	}
}

func TestReattachClosesPreviousConn(t *testing.T) {
	h, reg := newHub(t)
	att1, conn1 := attach(h, "edge1", "v1")
	_, conn2 := attach(h, "edge1", "v2")

	if conn1.closeCount() != 1 {
		t.Fatalf("old connection closed %d times, want 1", conn1.closeCount())
	}
	if conn2.closeCount() != 0 {
		t.Fatal("new connection must stay open")
	}
	// The stale adapter detaches after being kicked; it must not knock the
	// replacement offline.
	att1.Detach()
	info, ok := h.Edges()["edge1"]
	if !ok || info.Version != "v2" {
		t.Fatalf("edge1 should still be attached via the second session, got %+v (ok=%v)", info, ok)
	}
	if _, err := reg.Acquire(context.Background(), "edge1"); err != nil {
		t.Fatalf("executor should still be registered: %v", err)
	}
}

func TestHandleDoneForUnknownExecutionIsAccepted(t *testing.T) {
	h, _ := newHub(t)
	attach(h, "edge1", "v1")
	// No panic, no state: main has already moved on from this execution.
	h.HandleDone("edge1", "ex_gone", true, "", "sha256:abc")
	h.HandleLog("edge1", "ex_gone", 0, "stdout", "late")
	h.HandleDone("ghost", "ex_gone", true, "", "")
}

func TestExecuteContextTimeout(t *testing.T) {
	h, reg := newHub(t)
	_, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := ex.Execute(ctx, engine.Spec{ExecutionID: "ex_1"}, &recordSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	waitSent(t, conn, "ex_1")
}

func TestConcurrentExecutionsDoNotCross(t *testing.T) {
	h, reg := newHub(t)
	_, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")

	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			res, err := ex.Execute(context.Background(), engine.Spec{ExecutionID: "ex_" + id}, &recordSink{})
			if err != nil {
				t.Errorf("execute %s: %v", id, err)
				return
			}
			if res.Digest != "sha256:"+id {
				t.Errorf("execution %s got digest %q — reports crossed executions", id, res.Digest)
			}
		}(id)
	}
	for _, id := range []string{"a", "b", "c"} {
		waitSent(t, conn, "ex_"+id)
		h.HandleDone("edge1", "ex_"+id, true, "", "sha256:"+id)
	}
	wg.Wait()
}

// startExec launches an execution and waits until it has reached the edge.
func startExec(t *testing.T, ex executor.Executor, conn *fakeConn, id string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := ex.Execute(context.Background(), engine.Spec{ExecutionID: id}, &recordSink{})
		done <- err
	}()
	waitSent(t, conn, id)
	return done
}

func TestGraceHoldsExecutionAcrossReconnect(t *testing.T) {
	h, reg := newHubGrace(t, 2*time.Second)
	att, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")
	done := startExec(t, ex, conn, "ex_1")

	att.Detach()
	select {
	case err := <-done:
		t.Fatalf("execution settled inside the grace window: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The edge comes back still holding the execution and reports its result;
	// main must never have written it off.
	attach(h, "edge1", "v1", "ex_1")
	h.HandleDone("edge1", "ex_1", true, "", "sha256:abc")
	if err := <-done; err != nil {
		t.Fatalf("execution should have succeeded after reconnect: %v", err)
	}
}

func TestDetachedEdgeIsOfflineDuringGrace(t *testing.T) {
	h, reg := newHubGrace(t, 2*time.Second)
	att, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")
	startExec(t, ex, conn, "ex_1")
	att.Detach()

	if len(h.Edges()) != 0 {
		t.Fatal("an edge holding executions through grace is still offline")
	}
	if _, err := reg.Acquire(context.Background(), "edge1"); !errors.Is(err, executor.ErrUnreachable) {
		t.Fatalf("no new work may be dispatched during grace, got %v", err)
	}
}

func TestGraceExpiryReportsUnreachable(t *testing.T) {
	h, reg := newHubGrace(t, 60*time.Millisecond)
	att, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")
	done := startExec(t, ex, conn, "ex_1")
	att.Detach()

	select {
	case err := <-done:
		if !errors.Is(err, executor.ErrUnreachable) {
			t.Fatalf("want unreachable once the grace window elapses, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("execution never settled after the grace window")
	}
}

func TestReconnectWithoutExecutionFailsFast(t *testing.T) {
	h, reg := newHubGrace(t, 10*time.Second)
	att, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")
	done := startExec(t, ex, conn, "ex_1")
	att.Detach()

	// gRPC edges never advertise inflight, so a reconnect proves the
	// execution is gone: waiting out the whole grace window would be dead time.
	attach(h, "edge1", "v1")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want failure when the edge comes back without the execution")
		}
		if errors.Is(err, executor.ErrUnreachable) {
			t.Fatalf("a reachable edge that lost an execution is failed, not unreachable: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("execution did not fail fast on reconnect")
	}
}

func TestDoneAcceptedWhileEdgeIsDetached(t *testing.T) {
	// SSE reports results over separate requests, so a result can land while
	// the downstream stream is still broken.
	h, reg := newHubGrace(t, 2*time.Second)
	att, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")
	done := startExec(t, ex, conn, "ex_1")
	att.Detach()

	h.HandleDone("edge1", "ex_1", true, "", "sha256:abc")
	if err := <-done; err != nil {
		t.Fatalf("a result reported without an attachment must still settle: %v", err)
	}
}

func TestGraceTimerSparesReadoptedExecution(t *testing.T) {
	grace := 150 * time.Millisecond
	h, reg := newHubGrace(t, grace)
	att, conn := attach(h, "edge1", "v1")
	ex, _ := reg.Acquire(context.Background(), "edge1")
	done := startExec(t, ex, conn, "ex_1")

	att.Detach()
	time.Sleep(grace / 3)
	attach(h, "edge1", "v1", "ex_1") // re-adopted well before expiry
	time.Sleep(grace)                // the first timer's deadline goes by

	select {
	case err := <-done:
		t.Fatalf("a stale grace timer killed a re-adopted execution: %v", err)
	default:
	}
	h.HandleDone("edge1", "ex_1", true, "", "")
	if err := <-done; err != nil {
		t.Fatalf("re-adopted execution should still succeed: %v", err)
	}
}

// Behavior: main refuses to hand an edge older than ops.ModifiersSince a
// snapshot that uses step modifiers — that edge would decode it without
// complaint and silently run it the old way (a 3m op timeout quietly back to
// the 10m execution budget). The execution fails at once, naming the version
// and asking for an upgrade; nothing is sent. Snapshots without modifiers
// still go to any edge.
func TestExecuteRefusesModifiersOnOldEdge(t *testing.T) {
	plain := []ops.Step{{Op: "compose.pull", Args: &ops.ComposePull{}}, {Op: "compose.up", Args: &ops.ComposeUp{}}}
	timed := []ops.Step{{Op: "compose.up", Args: &ops.ComposeUp{}}, {Op: "compose.pull", Args: &ops.ComposePull{}, Timeout: time.Minute}}

	run := func(edgeVersion string, steps []ops.Step) (error, *fakeConn) {
		h, reg := newHub(t)
		_, conn := attach(h, "edge1", edgeVersion)
		ex, err := reg.Acquire(context.Background(), "edge1")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := ex.Execute(context.Background(), engine.Spec{ExecutionID: "ex_1", Steps: steps}, &recordSink{})
			done <- err
		}()
		select {
		case err := <-done:
			return err, conn
		case <-time.After(200 * time.Millisecond):
			// shipped and now waiting on the edge — settle it
			h.HandleDone("edge1", "ex_1", true, "", "")
			return <-done, conn
		}
	}

	err, conn := run("v0.6.0", timed)
	if err == nil {
		t.Fatal("an old edge must not receive a snapshot with modifiers")
	}
	for _, want := range []string{`"edge1"`, "v0.6.0", "op 2 (compose.pull)", ops.ModifiersSince, "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if errors.Is(err, executor.ErrUnreachable) {
		t.Fatal("a too-old edge is a failure, not unreachable")
	}
	if len(conn.sentIDs()) != 0 {
		t.Fatal("nothing may be sent to the old edge")
	}

	for _, v := range []string{ops.ModifiersSince, "v1.0.0", "dev"} {
		if err, conn := run(v, timed); err != nil || len(conn.sentIDs()) != 1 {
			t.Errorf("edge %s should run modifiers: err=%v sent=%v", v, err, conn.sentIDs())
		}
	}
	if err, conn := run("v0.5.0", plain); err != nil || len(conn.sentIDs()) != 1 {
		t.Errorf("a snapshot without modifiers goes to any edge: err=%v sent=%v", err, conn.sentIDs())
	}
}
