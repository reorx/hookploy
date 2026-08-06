package httpapi

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/api"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/token"
)

// notifySpy records the deploys the HTTP layer hands off.
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

// simulToken registers a service token for `simul`, whose pipeline needs a
// payload key and so fails to build when the key is missing.
func simulToken(h *harness) string {
	tok := token.New(token.KindService)
	h.store.InsertToken(string(token.KindService), "simul", token.Hash(tok))
	return tok
}

// Behavior: a deploy that fails while being built is reported. It never
// reaches the scheduler, so nothing downstream would ever notice it — and a
// pipeline that cannot even be built is precisely the failure worth hearing
// about.
func TestBuildFailureIsHandedOff(t *testing.T) {
	h := newHarness(t)
	spy := &notifySpy{}
	h.srv.Notify = spy.hook()

	resp := h.hook("simul", simulToken(h), `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	acc := decodeJSON[api.Accepted](t, resp.Body)
	d := h.waitDeploy(acc.DeployID, model.StatusFailed)

	got := spy.seen()
	if len(got) != 1 || got[0] != d.ID {
		t.Fatalf("handed off %v, want exactly one hand-off of %s", got, d.ID)
	}
}

// Behavior: a deploy that builds fine is not reported here. The scheduler
// owns that hand-off, and doing it in both places would double every message.
func TestSuccessfulEnqueueIsNotHandedOffByHTTP(t *testing.T) {
	h := newHarness(t)
	spy := &notifySpy{}
	h.srv.Notify = spy.hook()
	h.fake.On("deploy-linkmind").Returning("ok", 0)

	resp := h.hook("linkmind", h.svcToken, `{}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	acc := decodeJSON[api.Accepted](t, resp.Body)
	h.waitDeploy(acc.DeployID, model.StatusSucceeded)

	if got := spy.seen(); len(got) != 0 {
		t.Fatalf("the HTTP layer handed off %v; that is the scheduler's job", got)
	}
}

// Behavior: reporting a build failure does not slow the webhook down. The
// hand-off happens on the request goroutine, so its cost is the response's
// cost — and CI systems retry on a slow webhook. (That the production hook
// really is instant is pinned in internal/notify; here we only check the
// handler adds nothing of its own.)
func TestBuildFailureStillAnswersPromptly(t *testing.T) {
	h := newHarness(t)
	spy := &notifySpy{}
	h.srv.Notify = spy.hook()
	tok := simulToken(h)

	start := time.Now()
	resp := h.hook("simul", tok, `{}`)
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the webhook took %s to answer", elapsed)
	}
}
