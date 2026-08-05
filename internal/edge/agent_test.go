package edge

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/runner"
)

// ── harness ────────────────────────────────────────────────────────────────

// fakeSession is a session with no wire behind it: tasks are pushed in by
// the test and reports are recorded.
type fakeSession struct {
	resumable bool
	tasks     chan *Task
	dead      chan struct{}
	deadOnce  sync.Once
	ctx       context.Context // the dial context; canceling it ends the stream

	mu      sync.Mutex
	doneErr error
	updates []Update
	dones   []string // "<execID>:ok" / "<execID>:fail"
}

func (s *fakeSession) Recv() (*Task, error) {
	select {
	case t := <-s.tasks:
		return t, nil
	case <-s.dead:
		return nil, errors.New("stream closed")
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

func (s *fakeSession) ReportUpdate(execID string, u Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, u)
}

func (s *fakeSession) ReportDone(execID string, d DoneReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome := ":fail"
	if d.OK {
		outcome = ":ok"
	}
	s.dones = append(s.dones, execID+outcome)
	return s.doneErr
}

func (s *fakeSession) Resumable() bool     { return s.resumable }
func (s *fakeSession) MainVersion() string { return "v-test" }
func (s *fakeSession) Server() string      { return "edge1" }
func (s *fakeSession) Close()              {}

func (s *fakeSession) kill() { s.deadOnce.Do(func() { close(s.dead) }) }

func (s *fakeSession) setDoneErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doneErr = err
}

func (s *fakeSession) doneList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.dones...)
}

type fakeTransport struct {
	resumable bool

	mu       sync.Mutex
	dials    [][]string
	sessions []*fakeSession
}

func (t *fakeTransport) Dial(ctx context.Context, inflight []string) (Session, error) {
	s := &fakeSession{
		resumable: t.resumable,
		tasks:     make(chan *Task, 4),
		dead:      make(chan struct{}),
		ctx:       ctx,
	}
	t.mu.Lock()
	t.dials = append(t.dials, append([]string(nil), inflight...))
	t.sessions = append(t.sessions, s)
	t.mu.Unlock()
	return s, nil
}

func (t *fakeTransport) dialCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.dials)
}

func (t *fakeTransport) advertised(i int) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if i >= len(t.dials) {
		return nil
	}
	return t.dials[i]
}

// waitSession blocks until the agent has dialed at least i+1 times.
func (t *fakeTransport) waitSession(tb testing.TB, i int) *fakeSession {
	tb.Helper()
	waitFor(tb, "session dialed", func() bool { return t.dialCount() > i })
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessions[i]
}

func waitFor(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	tb.Fatalf("timed out waiting for %s", what)
}

// startAgent runs an agent against tr until the test ends.
func startAgent(t *testing.T, tr Transport, fr *runner.FakeRunner) *agent {
	t.Helper()
	eng := &engine.Engine{Runner: fr, Sleep: func(context.Context, time.Duration) error { return nil }}
	a := newAgent(eng, log.New(io.Discard, "", 0), 5*time.Millisecond, 20*time.Millisecond)
	a.doneRetry = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.run(ctx, tr)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return a
}

func runTask(id string) *Task {
	return &Task{
		ExecutionID: id,
		Service:     "svc",
		Instance:    "svc",
		Dir:         "/opt/apps/svc",
		OpsJSON:     []byte(`[{"op":"run","args":{"argv":["deploy-step"]}}]`),
	}
}

// gatedRunner returns a runner whose command blocks until the gate closes.
func gatedRunner(gate <-chan struct{}) *runner.FakeRunner {
	fr := &runner.FakeRunner{}
	fr.On("deploy-step").Effect = func(runner.Cmd) error {
		<-gate
		return nil
	}
	return fr
}

// ── behavior ───────────────────────────────────────────────────────────────

// Behavior: on a resumable transport a broken stream does not abort work.
// The execution finishes and its result is re-reported over the new session,
// which is the whole point of the tolerance design.
func TestResumableExecutionSurvivesStreamBreak(t *testing.T) {
	gate := make(chan struct{})
	fr := gatedRunner(gate)
	tr := &fakeTransport{resumable: true}
	startAgent(t, tr, fr)

	s0 := tr.waitSession(t, 0)
	s0.tasks <- runTask("ex_1")
	waitFor(t, "execution started", func() bool { return len(fr.ArgvList()) > 0 })

	s0.kill() // the proxy cuts the stream mid-execution
	s1 := tr.waitSession(t, 1)
	close(gate) // the execution keeps going and finishes on this healthy edge

	waitFor(t, "result re-reported", func() bool { return len(s1.doneList()) == 1 })
	if got := s1.doneList()[0]; got != "ex_1:ok" {
		t.Fatalf("re-reported result = %q, want ex_1:ok", got)
	}
	if n := len(s0.doneList()); n != 0 {
		t.Fatalf("result was sent to the dead session %d time(s)", n)
	}
}

// Behavior: a resumable edge advertises what it still holds, so main can
// re-adopt those executions instead of writing them off.
func TestResumableRedialAdvertisesInflight(t *testing.T) {
	gate := make(chan struct{})
	defer close(gate)
	fr := gatedRunner(gate)
	tr := &fakeTransport{resumable: true}
	a := startAgent(t, tr, fr)

	s0 := tr.waitSession(t, 0)
	if len(tr.advertised(0)) != 0 {
		t.Fatalf("first dial advertised %v, want nothing", tr.advertised(0))
	}
	s0.tasks <- runTask("ex_1")
	waitFor(t, "execution running", func() bool { return a.runningCount() == 1 })
	s0.kill()

	tr.waitSession(t, 1)
	adv := tr.advertised(1)
	if len(adv) != 1 || adv[0] != "ex_1" {
		t.Fatalf("redial advertised %v, want [ex_1]", adv)
	}
}

// Behavior: a result that main never acknowledged stays buffered and is
// retried until it lands.
func TestUnacknowledgedResultIsRetried(t *testing.T) {
	tr := &fakeTransport{resumable: true}
	a := startAgent(t, tr, &runner.FakeRunner{})

	s0 := tr.waitSession(t, 0)
	s0.setDoneErr(errors.New("main did not ack"))
	s0.tasks <- runTask("ex_1")

	waitFor(t, "first attempt", func() bool { return len(s0.doneList()) > 0 })
	if n := a.pendingCount(); n != 1 {
		t.Fatalf("pending results = %d, want 1 (an unacked result must be kept)", n)
	}

	s0.setDoneErr(nil) // main comes back
	waitFor(t, "result acknowledged", func() bool { return a.pendingCount() == 0 })
}

// Behavior: the gRPC transport cannot resume, so its results are not
// buffered — a stale result would never be adoptable and would only pile up.
func TestNonResumableResultIsNotBuffered(t *testing.T) {
	tr := &fakeTransport{resumable: false}
	a := startAgent(t, tr, &runner.FakeRunner{})

	s0 := tr.waitSession(t, 0)
	s0.setDoneErr(errors.New("stream is gone"))
	s0.tasks <- runTask("ex_1")

	waitFor(t, "done attempted", func() bool { return len(s0.doneList()) == 1 })
	// Give the pump a chance to buffer it if it were going to.
	time.Sleep(60 * time.Millisecond)
	if n := a.pendingCount(); n != 0 {
		t.Fatalf("pending results = %d, want 0 on a non-resumable transport", n)
	}
	s0.kill()
	s1 := tr.waitSession(t, 1)
	time.Sleep(60 * time.Millisecond)
	if n := len(s1.doneList()); n != 0 {
		t.Fatalf("new session got %d re-reported result(s), want 0", n)
	}
}

// Behavior: a non-resumable session dying cancels its executions, since main
// has already written them off.
func TestNonResumableExecutionIsCanceledWithSession(t *testing.T) {
	fr := &runner.FakeRunner{}
	fr.On("deploy-step").BlockUntilCancel = true
	tr := &fakeTransport{resumable: false}
	a := startAgent(t, tr, fr)

	s0 := tr.waitSession(t, 0)
	s0.tasks <- runTask("ex_1")
	waitFor(t, "execution started", func() bool { return len(fr.ArgvList()) > 0 })
	s0.kill()

	waitFor(t, "execution canceled", func() bool { return a.runningCount() == 0 })
}

// Behavior: the result buffer is bounded — a main that stays away must not
// grow the edge's memory without limit.
func TestPendingBufferDropsOldest(t *testing.T) {
	a := newAgent(nil, log.New(io.Discard, "", 0), time.Millisecond, time.Millisecond)
	a.pendingCap = 2
	a.bufferDone("ex_1", DoneReport{OK: true})
	a.bufferDone("ex_2", DoneReport{OK: true})
	a.bufferDone("ex_3", DoneReport{OK: true})

	ids := a.pendingIDs()
	if len(ids) != 2 || ids[0] != "ex_2" || ids[1] != "ex_3" {
		t.Fatalf("pending = %v, want the two newest [ex_2 ex_3]", ids)
	}
}
