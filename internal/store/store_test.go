package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
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

// Behavior: a failure surfaces on the deploy immediately, but the deploy is
// only *finished* once every execution has settled — a follower must not be
// told "done" while a later wave is still queued for cancellation.
func TestRecomputeFailureIsLiveButNotFinishedEarly(t *testing.T) {
	s := openTest(t)
	d := &model.Deploy{
		ID: model.NewDeployID(), Service: "svc", Kind: model.KindDeploy,
		Payload: json.RawMessage(`{}`), Status: model.StatusQueued, CreatedAt: time.Now(),
	}
	mkExec := func(inst string, wave int) *model.Execution {
		return &model.Execution{
			ID: model.NewExecutionID(), DeployID: d.ID, Service: "svc", Instance: inst,
			Server: "s1", Dir: "/opt/x", Wave: wave,
			OpsJSON: json.RawMessage(`[{"op":"compose.up"}]`),
			Timeout: model.Duration(10 * time.Minute),
			Status:  model.StatusQueued, CreatedAt: time.Now(),
		}
	}
	w1, w2 := mkExec("m0", 0), mkExec("sg0", 1)
	if err := s.CreateDeploy(d, []*model.Execution{w1, w2}); err != nil {
		t.Fatal(err)
	}
	events, stop, err := s.FollowDeploy(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	s.TransitionExecution(w1.ID, model.StatusQueued, model.StatusDispatching, "")
	s.TransitionExecution(w1.ID, model.StatusDispatching, model.StatusRunning, "")
	s.TransitionExecution(w1.ID, model.StatusRunning, model.StatusFailed, "boom")

	// wave 2 is still queued: failure is visible, the rollout is not over.
	st, err := s.RecomputeDeployStatus(d.ID)
	if err != nil || st != model.StatusFailed {
		t.Fatalf("failure should be visible right away: %s err=%v", st, err)
	}
	got, _ := s.GetDeploy(d.ID)
	if got.Status != model.StatusFailed {
		t.Fatalf("deploy status = %s, want failed", got.Status)
	}
	if got.FinishedAt != nil {
		t.Fatal("deploy must not be finished while a wave is still queued")
	}
	select {
	case ev := <-events:
		if ev.Done {
			t.Fatal("followers must not be told done before the rollout settles")
		}
	default:
	}

	// wave 2 canceled → now the rollout is over.
	s.TransitionExecution(w2.ID, model.StatusQueued, model.StatusCanceled, "earlier wave failed")
	if st, _ = s.RecomputeDeployStatus(d.ID); st != model.StatusFailed {
		t.Fatalf("settled rollout = %s, want failed", st)
	}
	if got, _ = s.GetDeploy(d.ID); got.FinishedAt == nil {
		t.Fatal("settled rollout should have finished_at")
	}
	for {
		select {
		case ev := <-events:
			if ev.Done {
				return // expected
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no done event after the rollout settled")
		}
	}
}

// Behavior: a restart mid-rollout leaves no execution able to transition —
// in-flight ones fail, and the later waves they gated are canceled. Nothing
// will ever run them again, so leaving them queued would strand the deploy
// short of a terminal status.
func TestRecoverInFlightCancelsGatedWaves(t *testing.T) {
	s := openTest(t)
	d := &model.Deploy{
		ID: model.NewDeployID(), Service: "svc", Kind: model.KindDeploy,
		Payload: json.RawMessage(`{}`), Status: model.StatusQueued, CreatedAt: time.Now(),
	}
	mkExec := func(inst string, wave int) *model.Execution {
		return &model.Execution{
			ID: model.NewExecutionID(), DeployID: d.ID, Service: "svc", Instance: inst,
			Server: "s1", Dir: "/opt/x", Wave: wave,
			OpsJSON: json.RawMessage(`[{"op":"compose.up"}]`),
			Timeout: model.Duration(10 * time.Minute),
			Status:  model.StatusQueued, CreatedAt: time.Now(),
		}
	}
	w1, w2 := mkExec("m0", 0), mkExec("sg0", 1)
	if err := s.CreateDeploy(d, []*model.Execution{w1, w2}); err != nil {
		t.Fatal(err)
	}
	// wave 1 is running when the process dies; wave 2 never started.
	s.TransitionExecution(w1.ID, model.StatusQueued, model.StatusDispatching, "")
	s.TransitionExecution(w1.ID, model.StatusDispatching, model.StatusRunning, "")

	ids, err := s.RecoverInFlight("main restarted")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != d.ID {
		t.Fatalf("affected deploys = %v, want [%s]", ids, d.ID)
	}
	got1, _ := s.GetExecution(w1.ID)
	got2, _ := s.GetExecution(w2.ID)
	if got1.Status != model.StatusFailed {
		t.Fatalf("in-flight execution should fail: %s", got1.Status)
	}
	if got2.Status != model.StatusCanceled {
		t.Fatalf("gated wave should be canceled, got %s", got2.Status)
	}
	if got2.FinishedAt == nil {
		t.Fatal("canceled execution should have finished_at")
	}
	st, err := s.RecomputeDeployStatus(d.ID)
	if err != nil || st != model.StatusFailed {
		t.Fatalf("recovered deploy must reach a terminal status: %s err=%v", st, err)
	}
}

// mkRollout creates a deploy with one execution per given instance name,
// each in its own wave, and forces their statuses. Used to reconstruct the
// exact on-disk shapes a crash can leave behind.
func mkRollout(t *testing.T, s *Store, service string, deployStatus model.Status, execStatuses ...model.Status) (*model.Deploy, []*model.Execution) {
	t.Helper()
	d := &model.Deploy{
		ID: model.NewDeployID(), Service: service, Kind: model.KindDeploy,
		Payload: json.RawMessage(`{}`), Status: model.StatusQueued, CreatedAt: time.Now(),
	}
	var execs []*model.Execution
	for i := range execStatuses {
		execs = append(execs, &model.Execution{
			ID: model.NewExecutionID(), DeployID: d.ID, Service: service,
			Instance: service + "-" + strconv.Itoa(i), Server: "s1", Dir: "/opt/x", Wave: i,
			OpsJSON: json.RawMessage(`[{"op":"compose.up"}]`),
			Timeout: model.Duration(10 * time.Minute),
			Status:  model.StatusQueued, CreatedAt: time.Now(),
		})
	}
	if err := s.CreateDeploy(d, execs); err != nil {
		t.Fatal(err)
	}
	for i, want := range execStatuses {
		if want == model.StatusQueued {
			continue
		}
		if _, err := s.db.Exec("UPDATE executions SET status = ? WHERE id = ?", string(want), execs[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec("UPDATE deploys SET status = ? WHERE id = ?", string(deployStatus), d.ID); err != nil {
		t.Fatal(err)
	}
	return d, execs
}

// Behavior: a crash can leave an interrupted rollout with no dispatching or
// running execution at all — the process died between marking wave 1 failed
// and canceling the waves it gated, or between two waves. Nothing reschedules
// those (the scheduler only re-runs deploys that never started), so recovery
// must sweep them by deploy, not by looking for in-flight executions.
func TestRecoverSweepsInterruptedRolloutsWithNoInFlightExecutions(t *testing.T) {
	// W2: died between "wave 1 failed" and "cancel wave 2".
	t.Run("failure not yet propagated to gated wave", func(t *testing.T) {
		s := openTest(t)
		d, execs := mkRollout(t, s, "svc", model.StatusFailed, model.StatusFailed, model.StatusQueued)
		if _, err := s.RecoverInFlight("main restarted"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetExecution(execs[1].ID)
		if got.Status != model.StatusCanceled {
			t.Fatalf("gated wave should be canceled, got %s", got.Status)
		}
		st, _ := s.RecomputeDeployStatus(d.ID)
		if st != model.StatusFailed {
			t.Fatalf("deploy status = %s, want failed", st)
		}
		if cur, _ := s.GetDeploy(d.ID); cur.FinishedAt == nil {
			t.Fatal("interrupted rollout must reach a finished state, not strand forever")
		}
	})

	// W1: died between two waves — wave 1 succeeded, wave 2 never dispatched.
	t.Run("died between waves", func(t *testing.T) {
		s := openTest(t)
		d, execs := mkRollout(t, s, "svc", model.StatusRunning, model.StatusSucceeded, model.StatusQueued)
		if _, err := s.RecoverInFlight("main restarted"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetExecution(execs[1].ID)
		if got.Status != model.StatusCanceled {
			t.Fatalf("undispatched wave should be canceled, got %s", got.Status)
		}
		s.RecomputeDeployStatus(d.ID)
		cur, _ := s.GetDeploy(d.ID)
		if cur.FinishedAt == nil {
			t.Fatal("rollout interrupted between waves must not stay running forever")
		}
		if !cur.Status.Terminal() {
			t.Fatalf("deploy should be terminal, got %s", cur.Status)
		}
	})

	// A deploy that already finished must not be touched.
	t.Run("finished deploy untouched", func(t *testing.T) {
		s := openTest(t)
		d, execs := mkRollout(t, s, "svc", model.StatusSucceeded, model.StatusSucceeded)
		if _, err := s.db.Exec("UPDATE deploys SET finished_at = ? WHERE id = ?", now(), d.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecoverInFlight("main restarted"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetExecution(execs[0].ID)
		if got.Status != model.StatusSucceeded {
			t.Fatalf("finished deploy's execution changed to %s", got.Status)
		}
	})
}

// Behavior: recovery must not touch a deploy that never started — those are
// rescheduled wholesale, so their queued executions stay runnable.
func TestRecoverInFlightLeavesUnstartedDeploys(t *testing.T) {
	s := openTest(t)
	d, ex := mkDeploy(t, s, "svc")
	ids, err := s.RecoverInFlight("main restarted")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("no deploy was in flight, got %v", ids)
	}
	got, _ := s.GetExecution(ex.ID)
	if got.Status != model.StatusQueued {
		t.Fatalf("unstarted execution must stay queued, got %s", got.Status)
	}
	if d2, _ := s.GetDeploy(d.ID); d2.Status != model.StatusQueued {
		t.Fatalf("unstarted deploy must stay queued, got %s", d2.Status)
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
// Behavior: attaching to a deploy whose status already reads failed must not
// end the stream while a sibling instance is still running — the failure
// surfaces on the deploy as soon as one instance dies, but the others keep
// producing logs the follower needs to see.
func TestFollowDeployStreamsUntilAllInstancesSettle(t *testing.T) {
	s := openTest(t)
	d := &model.Deploy{
		ID: model.NewDeployID(), Service: "svc", Kind: model.KindDeploy,
		Payload: json.RawMessage(`{}`), Status: model.StatusQueued, CreatedAt: time.Now(),
	}
	mkExec := func(inst string) *model.Execution {
		return &model.Execution{
			ID: model.NewExecutionID(), DeployID: d.ID, Service: "svc", Instance: inst,
			Server: "s1", Dir: "/opt/x",
			OpsJSON: json.RawMessage(`[{"op":"compose.up"}]`),
			Timeout: model.Duration(10 * time.Minute),
			Status:  model.StatusQueued, CreatedAt: time.Now(),
		}
	}
	m0, sg0 := mkExec("m0"), mkExec("sg0")
	if err := s.CreateDeploy(d, []*model.Execution{m0, sg0}); err != nil {
		t.Fatal(err)
	}
	for _, ex := range []*model.Execution{m0, sg0} {
		s.TransitionExecution(ex.ID, model.StatusQueued, model.StatusDispatching, "")
		s.TransitionExecution(ex.ID, model.StatusDispatching, model.StatusRunning, "")
	}
	// m0 dies; sg0 keeps running. The deploy already reads failed.
	s.TransitionExecution(m0.ID, model.StatusRunning, model.StatusFailed, "boom")
	if st, _ := s.RecomputeDeployStatus(d.ID); st != model.StatusFailed {
		t.Fatalf("deploy should read failed already, got %s", st)
	}

	// A follower attaching *now* must still receive sg0's output.
	events, stop, err := s.FollowDeploy(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	s.AppendLog(&model.LogLine{ExecutionID: sg0.ID, Stream: "stdout", Data: "still working", At: time.Now()})

	select {
	case ev := <-events:
		if ev.Done {
			t.Fatal("stream ended while a sibling instance was still running")
		}
		if ev.Line == nil || ev.Line.Data != "still working" {
			t.Fatalf("expected sg0's log line, got %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower never received the running instance's output")
	}

	// sg0 settles → the rollout is over and the stream ends.
	s.TransitionExecution(sg0.ID, model.StatusRunning, model.StatusSucceeded, "")
	s.RecomputeDeployStatus(d.ID)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("stream closed without a done event")
			}
			if ev.Done {
				return // expected
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no done event after every instance settled")
		}
	}
}

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

func mkRun(t *testing.T, s *Store, id int64, repo, status, conclusion string, at time.Time) *model.WorkflowRun {
	t.Helper()
	wr := &model.WorkflowRun{
		ID:           id,
		Repo:         repo,
		WorkflowName: "CI",
		RunNumber:    int(id),
		Status:       status,
		Conclusion:   conclusion,
		HeadBranch:   "master",
		HeadSHA:      "abcdef1234567890abcdef1234567890abcdef12",
		HTMLURL:      fmt.Sprintf("https://github.com/%s/actions/runs/%d", repo, id),
		Event:        "push",
		Actor:        "reorx",
		DisplayTitle: "some commit",
		CreatedAt:    at,
		UpdatedAt:    at,
		ReceivedAt:   at,
	}
	if err := s.UpsertWorkflowRun(wr); err != nil {
		t.Fatal(err)
	}
	return wr
}

// Behavior: workflow runs upsert by GitHub run id — a later delivery of the
// same run updates the row in place instead of adding one.
func TestWorkflowRunUpsert(t *testing.T) {
	s := openTest(t)
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	wr := mkRun(t, s, 1, "reorx/hookploy", "in_progress", "", base)

	started := base.Add(10 * time.Second)
	wr.Status, wr.Conclusion = "completed", "success"
	wr.UpdatedAt = base.Add(time.Minute)
	wr.RunStartedAt = &started
	if err := s.UpsertWorkflowRun(wr); err != nil {
		t.Fatal(err)
	}

	runs, err := s.ListWorkflowRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 row after upsert, got %d", len(runs))
	}
	got := runs[0]
	if got.Status != "completed" || got.Conclusion != "success" {
		t.Fatalf("row not updated: %+v", got)
	}
	if got.RunStartedAt == nil || !got.RunStartedAt.Equal(started) {
		t.Fatalf("run_started_at lost: %+v", got.RunStartedAt)
	}
	if got.Repo != "reorx/hookploy" || got.HeadBranch != "master" || got.Actor != "reorx" {
		t.Fatalf("fields lost: %+v", got)
	}
}

// Behavior: a late event carrying an older updated_at never regresses a
// fresher row (completed stays completed).
func TestWorkflowRunOutOfOrder(t *testing.T) {
	s := openTest(t)
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	wr := mkRun(t, s, 1, "reorx/hookploy", "in_progress", "", base)

	wr.Status, wr.Conclusion = "completed", "success"
	wr.UpdatedAt = base.Add(2 * time.Minute)
	if err := s.UpsertWorkflowRun(wr); err != nil {
		t.Fatal(err)
	}

	stale := *wr
	stale.Status, stale.Conclusion = "in_progress", ""
	stale.UpdatedAt = base.Add(time.Minute)
	if err := s.UpsertWorkflowRun(&stale); err != nil {
		t.Fatal(err)
	}

	runs, err := s.ListWorkflowRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "completed" || runs[0].Conclusion != "success" {
		t.Fatalf("stale event regressed the row: %+v", runs[0])
	}
}

// Behavior: ListWorkflowRuns filters by repo case-insensitively, returns
// newest first, honors the limit; empty repo means every repo.
func TestWorkflowRunList(t *testing.T) {
	s := openTest(t)
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	mkRun(t, s, 1, "reorx/hookploy", "completed", "success", base)
	mkRun(t, s, 2, "reorx/hookploy", "completed", "failure", base.Add(time.Minute))
	mkRun(t, s, 3, "reorx/other", "in_progress", "", base.Add(2*time.Minute))

	runs, err := s.ListWorkflowRuns("Reorx/Hookploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != 2 || runs[1].ID != 1 {
		t.Fatalf("repo filter/order wrong: %+v", runs)
	}

	all, err := s.ListWorkflowRuns("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].ID != 3 {
		t.Fatalf("empty repo should list every repo newest first: %+v", all)
	}

	top, err := s.ListWorkflowRuns("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].ID != 3 {
		t.Fatalf("limit wrong: %+v", top)
	}
}

// Behavior: ListActiveWorkflowRuns returns only runs that have not completed.
func TestWorkflowRunActive(t *testing.T) {
	s := openTest(t)
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	mkRun(t, s, 1, "reorx/hookploy", "completed", "success", base)
	mkRun(t, s, 2, "reorx/hookploy", "in_progress", "", base.Add(time.Minute))
	mkRun(t, s, 3, "reorx/other", "queued", "", base.Add(2*time.Minute))

	active, err := s.ListActiveWorkflowRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active[0].ID != 3 || active[1].ID != 2 {
		t.Fatalf("active runs wrong: %+v", active)
	}
}

// Behavior: CleanupWorkflowRuns keeps the newest `keep` runs of one repo and
// leaves other repos alone.
func TestWorkflowRunCleanup(t *testing.T) {
	s := openTest(t)
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 5; i++ {
		mkRun(t, s, i, "reorx/hookploy", "completed", "success", base.Add(time.Duration(i)*time.Minute))
	}
	mkRun(t, s, 100, "reorx/other", "completed", "success", base)

	if err := s.CleanupWorkflowRuns("reorx/hookploy", 3); err != nil {
		t.Fatal(err)
	}
	runs, err := s.ListWorkflowRuns("reorx/hookploy", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].ID != 5 || runs[2].ID != 3 {
		t.Fatalf("cleanup kept wrong rows: %+v", runs)
	}
	other, err := s.ListWorkflowRuns("reorx/other", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 {
		t.Fatalf("cleanup touched another repo: %+v", other)
	}
}
