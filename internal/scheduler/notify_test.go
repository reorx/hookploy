package scheduler

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/model"
)

// notifySpy records every deploy the scheduler hands off.
type notifySpy struct {
	mu  sync.Mutex
	ids []string
}

func (s *notifySpy) hook() func(string) {
	return func(deployID string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.ids = append(s.ids, deployID)
	}
}

func (s *notifySpy) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ids...)
}

// settleQuiet gives the scheduler a moment past the settle to prove it does
// not go on notifying — the redundant recomputes it makes after a rollout
// ends are exactly what this guards against.
func settleQuiet() { time.Sleep(50 * time.Millisecond) }

// Behavior: a failed deploy is handed off exactly once, even though the
// scheduler recomputes its status several times over — once per execution
// transition and again at every wave boundary. A hook keyed off the
// aggregate status alone would fire on each of them.
func TestNotifyFiresOnceOnFailure(t *testing.T) {
	h := newHarness(t, false, 0)
	spy := &notifySpy{}
	h.sched.Notify = spy.hook()
	svc := service("app", nil, `[{run: {argv: [deploy-step]}}]`)
	h.fake.On("deploy-step").Returning("", 1)

	d := h.enqueue(svc, "")
	h.waitFinished(d.ID, model.StatusFailed)
	settleQuiet()

	if got := spy.seen(); len(got) != 1 || got[0] != d.ID {
		t.Fatalf("handed off %v, want exactly one hand-off of %s", got, d.ID)
	}
}

// Behavior: the same holds for a multi-wave rollout, where the failure also
// cancels the waves it gated — those cancellations are written straight to
// the store and swept up by a wave-boundary recompute, a second path into
// the same settle.
func TestNotifyFiresOnceOnGatedRolloutFailure(t *testing.T) {
	h := newHarness(t, false, 0)
	spy := &notifySpy{}
	h.sched.Notify = spy.hook()
	svc := service("app", [][]string{{"a"}, {"b"}}, `[{run: {argv: [deploy-step]}}]`)
	h.fake.On("deploy-step").Returning("", 1)

	d := h.enqueue(svc, "")
	h.waitFinished(d.ID, model.StatusFailed)
	settleQuiet()

	if got := spy.seen(); len(got) != 1 {
		t.Fatalf("handed off %v, want exactly one", got)
	}
	execs, err := h.store.ListExecutions(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(execs) != 2 || execs[1].Status != model.StatusCanceled {
		t.Fatalf("test setup: wave 2 should have been canceled, got %+v", execs[1])
	}
}

// Behavior: instances that finish together race to close the rollout, and
// still only one of them hands it off.
func TestNotifyFiresOnceWhenAParallelWaveSettles(t *testing.T) {
	h := newHarness(t, false, 0)
	spy := &notifySpy{}
	h.sched.Notify = spy.hook()
	svc := service("app", [][]string{{"a", "b"}}, `[{run: {argv: [deploy-step]}}]`)
	h.fake.On("deploy-step").Returning("", 1)

	d := h.enqueue(svc, "")
	h.waitFinished(d.ID, model.StatusFailed)
	settleQuiet()

	if got := spy.seen(); len(got) != 1 {
		t.Fatalf("handed off %v, want exactly one", got)
	}
}

// Behavior: successes are handed off too. Deciding which outcomes are worth
// a message is the notify layer's job, reading live config; baking that
// choice into the scheduler would make it a restart-only setting.
func TestNotifyFiresOnSuccessAsWell(t *testing.T) {
	h := newHarness(t, false, 0)
	spy := &notifySpy{}
	h.sched.Notify = spy.hook()
	svc := service("app", nil, `[{run: {argv: [deploy-step]}}]`)
	h.fake.On("deploy-step").Returning("ok", 0)

	d := h.enqueue(svc, "")
	h.waitFinished(d.ID, model.StatusSucceeded)
	settleQuiet()

	if got := spy.seen(); len(got) != 1 || got[0] != d.ID {
		t.Fatalf("handed off %v, want exactly one hand-off of %s", got, d.ID)
	}
}

// Behavior: a superseded deploy is never handed off. It never ran, and its
// replacement will report for it.
func TestNotifyIgnoresSupersededDeploys(t *testing.T) {
	h := newHarness(t, true, 0)
	spy := &notifySpy{}
	h.sched.Notify = spy.hook()
	svc := service("app", nil, `[{run: {argv: [deploy-step]}}]`)
	h.fake.On("deploy-step").Returning("ok", 0)

	first := h.enqueue(svc, "")
	<-h.gate.notify // first is running and blocked
	superseded := h.enqueue(svc, "")
	last := h.enqueue(svc, "")

	h.waitStatus(superseded.ID, model.StatusSuperseded)
	h.gate.releaseAll()
	h.waitFinished(first.ID, model.StatusSucceeded)
	<-h.gate.notify // last is running now
	h.gate.releaseAll()
	h.waitFinished(last.ID, model.StatusSucceeded)
	settleQuiet()

	got := spy.seen()
	for _, id := range got {
		if id == superseded.ID {
			t.Fatalf("superseded deploy %s was handed off: %v", superseded.ID, got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("handed off %v, want one per deploy that actually ran", got)
	}
}

// Behavior: closing out rollouts abandoned by a previous process notifies
// nobody. A restart would otherwise send one message per interrupted deploy,
// and Recover runs before the listeners are even up.
func TestRecoverNeverNotifies(t *testing.T) {
	h := newHarness(t, false, 0)
	spy := &notifySpy{}
	svc := service("app", nil, `[{run: {argv: [deploy-step]}}]`)
	h.fake.On("deploy-step").Returning("ok", 0)

	// Pre-restart state: a rollout the previous process left running.
	d, execs, err := BuildDeploy(svc, model.KindDeploy, "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.CreateDeploy(d, execs); err != nil {
		t.Fatal(err)
	}
	for _, ex := range execs {
		if _, err := h.store.TransitionExecution(ex.ID, model.StatusQueued, model.StatusDispatching, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.TransitionExecution(ex.ID, model.StatusDispatching, model.StatusRunning, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := h.store.RecomputeDeployStatus(d.ID); err != nil {
		t.Fatal(err)
	}

	h.sched.Notify = spy.hook()
	if err := h.sched.Recover(); err != nil {
		t.Fatal(err)
	}
	settleQuiet()

	got, err := h.store.GetDeploy(d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.StatusFailed {
		t.Fatalf("test setup: recovery should have failed the rollout, got %s", got.Status)
	}
	if ids := spy.seen(); len(ids) != 0 {
		t.Fatalf("recovery handed off %v, want nothing", ids)
	}
}

// Behavior: a scheduler with no hook wired runs exactly as before.
func TestNilNotifyHookIsHarmless(t *testing.T) {
	h := newHarness(t, false, 0)
	svc := service("app", nil, `[{run: {argv: [deploy-step]}}]`)
	h.fake.On("deploy-step").Returning("", 1)

	d := h.enqueue(svc, "")
	got := h.waitFinished(d.ID, model.StatusFailed)
	if !strings.Contains(execError(t, h, got.ID), "exit") {
		t.Errorf("the failure itself should still be recorded: %s", execError(t, h, got.ID))
	}
}
