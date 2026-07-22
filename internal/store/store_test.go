package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/token"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mkDeploy(t *testing.T, s *Store, service string) (*model.Deploy, *model.Execution) {
	t.Helper()
	d := &model.Deploy{
		ID:        model.NewDeployID(),
		Service:   service,
		Kind:      model.KindDeploy,
		Payload:   json.RawMessage(`{"digest":"sha256:abc"}`),
		Status:    model.StatusQueued,
		CreatedAt: time.Now(),
	}
	ex := &model.Execution{
		ID:        model.NewExecutionID(),
		DeployID:  d.ID,
		Service:   service,
		Instance:  service,
		Server:    "s1",
		Dir:       "/opt/x",
		OpsJSON:   json.RawMessage(`[{"op":"compose.up"}]`),
		Timeout:   model.Duration(10 * time.Minute),
		Status:    model.StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := s.CreateDeploy(d, []*model.Execution{ex}); err != nil {
		t.Fatal(err)
	}
	return d, ex
}

// Behavior: token lifecycle — create/lookup, revoke kills lookup,
// rotate switches to a new token atomically.
func TestTokenLifecycle(t *testing.T) {
	s := openTest(t)
	plain := token.New(token.KindService)
	if err := s.InsertToken(string(token.KindService), "linkmind", token.Hash(plain)); err != nil {
		t.Fatal(err)
	}
	rec, err := s.LookupToken(token.Hash(plain))
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.Subject != "linkmind" || rec.Kind != "service" {
		t.Fatalf("lookup: %+v", rec)
	}

	// rotate: new token valid, old revoked, in one call
	plain2 := token.New(token.KindService)
	if err := s.RotateToken(string(token.KindService), "linkmind", token.Hash(plain2)); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.LookupToken(token.Hash(plain)); rec != nil {
		t.Fatal("old token must be revoked after rotate")
	}
	if rec, _ := s.LookupToken(token.Hash(plain2)); rec == nil {
		t.Fatal("new token must be valid after rotate")
	}

	n, err := s.RevokeTokens(string(token.KindService), "linkmind")
	if err != nil || n != 1 {
		t.Fatalf("revoke: n=%d err=%v", n, err)
	}
	if rec, _ := s.LookupToken(token.Hash(plain2)); rec != nil {
		t.Fatal("token must be invalid after revoke")
	}
}

// Behavior: execution status transitions are guarded — a stale writer
// (wrong expected status) does not win.
func TestGuardedTransitions(t *testing.T) {
	s := openTest(t)
	_, ex := mkDeploy(t, s, "svc")

	ok, err := s.TransitionExecution(ex.ID, model.StatusQueued, model.StatusDispatching, "")
	if err != nil || !ok {
		t.Fatalf("queued→dispatching should succeed: ok=%v err=%v", ok, err)
	}
	ok, _ = s.TransitionExecution(ex.ID, model.StatusQueued, model.StatusDispatching, "")
	if ok {
		t.Fatal("second queued→dispatching must fail (guard)")
	}
	ok, _ = s.TransitionExecution(ex.ID, model.StatusDispatching, model.StatusRunning, "")
	if !ok {
		t.Fatal("dispatching→running should succeed")
	}
	got, err := s.GetExecution(ex.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusRunning || got.StartedAt == nil {
		t.Fatalf("running exec should have started_at: %+v", got)
	}
	ok, _ = s.TransitionExecution(ex.ID, model.StatusRunning, model.StatusFailed, "boom")
	if !ok {
		t.Fatal("running→failed should succeed")
	}
	got, _ = s.GetExecution(ex.ID)
	if got.FinishedAt == nil || got.Error != "boom" {
		t.Fatalf("terminal exec should have finished_at and error: %+v", got)
	}
}

// Behavior: deploy status is recomputed as the aggregate of its executions.
func TestRecomputeDeployStatus(t *testing.T) {
	s := openTest(t)
	d, ex := mkDeploy(t, s, "svc")
	s.TransitionExecution(ex.ID, model.StatusQueued, model.StatusDispatching, "")
	s.TransitionExecution(ex.ID, model.StatusDispatching, model.StatusRunning, "")
	st, err := s.RecomputeDeployStatus(d.ID)
	if err != nil || st != model.StatusRunning {
		t.Fatalf("aggregate = %s err=%v", st, err)
	}
	s.TransitionExecution(ex.ID, model.StatusRunning, model.StatusSucceeded, "")
	st, _ = s.RecomputeDeployStatus(d.ID)
	if st != model.StatusSucceeded {
		t.Fatalf("aggregate = %s", st)
	}
	got, _ := s.GetDeploy(d.ID)
	if got.Status != model.StatusSucceeded || got.FinishedAt == nil {
		t.Fatalf("deploy row not updated: %+v", got)
	}
}

// Behavior: per-service retention keeps the newest 50 terminal deploys;
// logs vanish with their deploy (FK cascade).
func TestRetention(t *testing.T) {
	s := openTest(t)
	var first *model.Deploy
	var firstEx *model.Execution
	for i := 0; i < 52; i++ {
		d, ex := mkDeploy(t, s, "svc")
		if i == 0 {
			first, firstEx = d, ex
		}
		s.TransitionExecution(ex.ID, model.StatusQueued, model.StatusSucceeded, "")
		s.RecomputeDeployStatus(d.ID)
		s.AppendLog(&model.LogLine{ExecutionID: ex.ID, OpIndex: 0, Stream: "stdout", Data: fmt.Sprintf("line %d", i), At: time.Now()})
	}
	if err := s.CleanupService("svc", 50); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListDeploys("svc", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 50 {
		t.Fatalf("want 50 deploys after cleanup, got %d", len(list))
	}
	if got, _ := s.GetDeploy(first.ID); got != nil {
		t.Fatal("oldest deploy should be gone")
	}
	lines, err := s.GetDeployLogs(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("logs of deleted deploy must cascade away, got %d", len(lines))
	}
	_ = firstEx
}

// Behavior: a follower first gets the replay of existing lines, then new
// lines live, and is told when the deploy reaches a terminal state.
func TestFollowLogs(t *testing.T) {
	s := openTest(t)
	d, ex := mkDeploy(t, s, "svc")
	s.AppendLog(&model.LogLine{ExecutionID: ex.ID, Stream: "stdout", Data: "early", At: time.Now()})

	events, cancel, err := s.FollowDeploy(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	ev := <-events
	if ev.Line == nil || ev.Line.Data != "early" {
		t.Fatalf("first event should replay 'early': %+v", ev)
	}

	s.AppendLog(&model.LogLine{ExecutionID: ex.ID, Stream: "stderr", Data: "live", At: time.Now()})
	ev = <-events
	if ev.Line == nil || ev.Line.Data != "live" || ev.Line.Stream != "stderr" {
		t.Fatalf("second event should be the live line: %+v", ev)
	}

	s.TransitionExecution(ex.ID, model.StatusQueued, model.StatusFailed, "x")
	s.RecomputeDeployStatus(d.ID)
	for ev = range events {
		if ev.Done {
			if ev.Status != model.StatusFailed {
				t.Fatalf("done event status: %s", ev.Status)
			}
			return
		}
	}
	t.Fatal("events channel closed without a Done event")
}

// Behavior: recent deploys across all services, newest first, truncated to limit.
func TestListRecentDeploys(t *testing.T) {
	s := openTest(t)
	var ids []string // creation order: oldest first
	for i, svc := range []string{"a", "b", "a", "c"} {
		if i > 0 {
			time.Sleep(2 * time.Millisecond)
		}
		d, _ := mkDeploy(t, s, svc)
		ids = append(ids, d.ID)
	}
	all, err := s.ListRecentDeploys(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("want 4 deploys, got %d", len(all))
	}
	for i, d := range all {
		if want := ids[len(ids)-1-i]; d.ID != want {
			t.Fatalf("position %d: got %s want %s (newest first)", i, d.ID, want)
		}
	}
	top, err := s.ListRecentDeploys(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0].ID != ids[3] || top[1].ID != ids[2] {
		t.Fatalf("limit truncation wrong: %+v", top)
	}
}

// Behavior: latest deploy per service for the /services overview.
func TestLatestDeploys(t *testing.T) {
	s := openTest(t)
	mkDeploy(t, s, "a")
	time.Sleep(2 * time.Millisecond)
	d2, _ := mkDeploy(t, s, "a")
	d3, _ := mkDeploy(t, s, "b")
	latest, err := s.LatestDeploys()
	if err != nil {
		t.Fatal(err)
	}
	if latest["a"].ID != d2.ID || latest["b"].ID != d3.ID {
		t.Fatalf("latest wrong: %+v", latest)
	}
}
