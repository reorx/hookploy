package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/store"
)

// ── harness ────────────────────────────────────────────────────────────────

// fakeProvider records what it was asked to send and can be scripted to fail.
type fakeProvider struct {
	mu    sync.Mutex
	sent  []Event
	fails int // fail this many times before succeeding
	err   error
}

func (f *fakeProvider) Send(ctx context.Context, ev Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		if f.err != nil {
			return f.err
		}
		return errors.New("telegram is down")
	}
	f.sent = append(f.sent, ev)
	return nil
}

func (f *fakeProvider) events() []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Event(nil), f.sent...)
}

func (f *fakeProvider) count() int { return len(f.events()) }

type harness struct {
	t        *testing.T
	hub      *Hub
	store    *store.Store
	provider *fakeProvider
	cfg      *config.Config
	mu       sync.Mutex
	attempts int
}

const baseYAML = `
servers:
  s1: { local: true }
  s2: { local: true }
  edge-01: {}
`

// newHarness builds a Hub over a real temp store and a fake provider. notify
// is the yaml notify block; svcExtra is extra yaml nested under service web.
func newHarness(t *testing.T, notify, svcExtra string) *harness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hookploy.yaml")
	src := baseYAML + notify + `
services:
  web:
    dir: /opt/web
    instances:
      a: { server: s1 }
      b: { server: s2 }
` + svcExtra + `    deploy:
      - run: { argv: [deploy] }
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("test config does not load: %v\n%s", err, src)
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := &harness{t: t, store: st, provider: &fakeProvider{}, cfg: cfg}
	hub := New(st, h.config, nil)
	hub.MaxAttempts = 3
	hub.BackoffBase = time.Millisecond
	hub.BackoffMax = 2 * time.Millisecond
	hub.newProvider = func(config.Notify) (Provider, error) {
		h.mu.Lock()
		h.attempts++
		h.mu.Unlock()
		return h.provider, nil
	}
	h.hub = hub
	t.Cleanup(hub.Shutdown)
	return h
}

// config is the live-config closure the Hub holds. Locked because a test may
// swap the config from the delivery goroutine to watch a reload take effect.
func (h *harness) config() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

func (h *harness) setConfig(cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
}

// settle writes a deploy whose executions already hold the given statuses and
// recomputes it, exactly as the scheduler would leave it.
func (h *harness) settle(service string, execStatuses ...model.Status) *model.Deploy {
	h.t.Helper()
	now := time.Now()
	d := &model.Deploy{
		ID: model.NewDeployID(), Service: service, Kind: model.KindDeploy,
		Payload: json.RawMessage(`{}`), Status: model.StatusQueued, CreatedAt: now,
	}
	var execs []*model.Execution
	for i, st := range execStatuses {
		execs = append(execs, &model.Execution{
			ID: model.NewExecutionID(), DeployID: d.ID, Service: service,
			Instance: fmt.Sprintf("i%d", i), Server: fmt.Sprintf("s%d", i+1),
			Dir: "/opt/x", Wave: i, OpsJSON: json.RawMessage(`[]`),
			Timeout: model.Duration(time.Minute), Status: model.StatusQueued, CreatedAt: now,
		})
		_ = st
	}
	if err := h.store.CreateDeploy(d, execs); err != nil {
		h.t.Fatal(err)
	}
	for i, want := range execStatuses {
		errMsg := ""
		if want == model.StatusFailed || want == model.StatusUnreachable {
			errMsg = string(want) + " on " + execs[i].Instance
		}
		if _, err := h.store.TransitionExecution(execs[i].ID, model.StatusQueued, want, errMsg); err != nil {
			h.t.Fatal(err)
		}
	}
	if _, _, err := h.store.RecomputeDeployStatus(d.ID); err != nil {
		h.t.Fatal(err)
	}
	got, err := h.store.GetDeploy(d.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// waitFor polls until cond holds, the same shape the edge agent tests use
// instead of sleeping a fixed amount.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const telegramOn = `
notify:
  provider: telegram
  telegram: { bot_token: tok, chat_id: "-100" }
`

// ── behavior ───────────────────────────────────────────────────────────────

// Behavior: a failed rollout is reported once, naming every instance that did
// not succeed and carrying each one's error — the message has to say what
// broke, not just that something did.
func TestFailedDeployIsReportedWithPerInstanceErrors(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusCanceled)

	h.hub.Notify(d.ID)
	waitFor(t, "the failure notification", func() bool { return h.provider.count() == 1 })

	ev := h.provider.events()[0]
	if ev.Kind != model.EventDeployFailed {
		t.Errorf("kind = %q, want deploy.failed", ev.Kind)
	}
	if ev.Deploy.Service != "web" || ev.Deploy.DeployID != d.ID {
		t.Errorf("event does not identify the deploy: %+v", ev)
	}
	if len(ev.Deploy.Instances) != 2 {
		t.Fatalf("want both non-succeeded instances listed, got %+v", ev.Deploy.Instances)
	}
	if ev.Deploy.Instances[0].Error == "" {
		t.Error("the failing instance's error must survive into the event")
	}
	if ev.Deploy.Instances[1].Status != model.StatusCanceled {
		t.Errorf("gated instance status = %q, want canceled", ev.Deploy.Instances[1].Status)
	}
}

// Behavior: a deploy that failed before it ever produced an execution — a bad
// interpolation caught at build time — still reports, carrying the
// deploy-level error since there are no instances to blame.
func TestBuildFailureReportsDeployLevelError(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()
	now := time.Now()
	d := &model.Deploy{
		ID: model.NewDeployID(), Service: "web", Kind: model.KindDeploy,
		Payload: json.RawMessage(`{}`), Status: model.StatusFailed,
		Error: "payload.digest is not a sha256", CreatedAt: now, FinishedAt: &now,
	}
	if err := h.store.CreateDeploy(d, nil); err != nil {
		t.Fatal(err)
	}

	h.hub.Notify(d.ID)
	waitFor(t, "the build-failure notification", func() bool { return h.provider.count() == 1 })

	ev := h.provider.events()[0]
	if ev.Kind != model.EventDeployFailed {
		t.Errorf("kind = %q, want deploy.failed", ev.Kind)
	}
	if ev.Deploy.Error != "payload.digest is not a sha256" {
		t.Errorf("deploy-level error lost: %q", ev.Deploy.Error)
	}
	if len(ev.Deploy.Instances) != 0 {
		t.Errorf("a deploy with no executions must list no instances, got %+v", ev.Deploy.Instances)
	}
}

// Behavior: notifications stay off until a provider is named, so a config
// that has not opted in never even queues.
func TestNoProviderMeansNothingIsQueued(t *testing.T) {
	h := newHarness(t, "", "")
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusFailed)

	h.hub.Notify(d.ID)
	time.Sleep(20 * time.Millisecond)
	if n := h.hub.queueLen(); n != 0 {
		t.Errorf("queued %d events with notify off, want 0", n)
	}
	if h.provider.count() != 0 {
		t.Error("nothing may be delivered with notify off")
	}
}

// Behavior: a successful deploy is not reported under the default policy —
// the point of the feature is failures, and success is the common case.
func TestSucceededDeployIsSilentByDefault(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()
	d := h.settle("web", model.StatusSucceeded, model.StatusSucceeded)

	h.hub.Notify(d.ID)
	waitFor(t, "the event to be dropped", func() bool { return h.hub.queueLen() == 0 })
	if h.provider.count() != 0 {
		t.Errorf("a success must not notify under the default policy, got %+v", h.provider.events())
	}
}

// Behavior: a shutting-down main cancels the waves it never dispatched,
// settling those deploys as canceled. That is not an incident and must not
// page anyone.
func TestWhollyCanceledDeployIsNotAnEvent(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()
	d := h.settle("web", model.StatusCanceled, model.StatusCanceled)
	if d.Status != model.StatusCanceled {
		t.Fatalf("test setup: deploy status = %q, want canceled", d.Status)
	}

	h.hub.Notify(d.ID)
	waitFor(t, "the event to be dropped", func() bool { return h.hub.queueLen() == 0 })
	if h.provider.count() != 0 {
		t.Errorf("a canceled rollout must not notify, got %+v", h.provider.events())
	}
}

// Behavior: an all-unreachable rollout reports as deploy.unreachable, not as
// a failure — main never learned the outcome, so it must not claim the
// deploy broke.
func TestUnreachableDeployGetsItsOwnEventKind(t *testing.T) {
	h := newHarness(t, telegramOn+"  events: [deploy.unreachable]\n", "")
	h.hub.Start()
	d := h.settle("web", model.StatusUnreachable, model.StatusUnreachable)
	if d.Status != model.StatusUnreachable {
		t.Fatalf("test setup: deploy status = %q, want unreachable", d.Status)
	}

	h.hub.Notify(d.ID)
	waitFor(t, "the unreachable notification", func() bool { return h.provider.count() == 1 })
	if got := h.provider.events()[0].Kind; got != model.EventDeployUnreachable {
		t.Errorf("kind = %q, want deploy.unreachable", got)
	}
}

// Behavior: a success right after a failure is a recovery, and reports as
// deploy.recovered rather than deploy.succeeded — subscribing to recoveries
// must not mean subscribing to every green deploy.
func TestSuccessAfterFailureReportsAsRecovered(t *testing.T) {
	h := newHarness(t, telegramOn+"  events: [deploy.recovered]\n", "")
	h.hub.Start()
	h.settle("web", model.StatusFailed, model.StatusFailed)
	good := h.settle("web", model.StatusSucceeded, model.StatusSucceeded)

	h.hub.Notify(good.ID)
	waitFor(t, "the recovery notification", func() bool { return h.provider.count() == 1 })
	if got := h.provider.events()[0].Kind; got != model.EventDeployRecovered {
		t.Errorf("kind = %q, want deploy.recovered", got)
	}
}

// Behavior: a success after a success is not a recovery, so a service
// subscribed only to recoveries stays quiet on an ordinary green deploy.
func TestSuccessAfterSuccessIsNotARecovery(t *testing.T) {
	h := newHarness(t, telegramOn+"  events: [deploy.recovered]\n", "")
	h.hub.Start()
	h.settle("web", model.StatusSucceeded, model.StatusSucceeded)
	good := h.settle("web", model.StatusSucceeded, model.StatusSucceeded)

	h.hub.Notify(good.ID)
	waitFor(t, "the event to be dropped", func() bool { return h.hub.queueLen() == 0 })
	if h.provider.count() != 0 {
		t.Errorf("an ordinary success is not a recovery, got %+v", h.provider.events())
	}
}

// Behavior: a muted service reports nothing, whatever the global policy says.
func TestMutedServiceReportsNothing(t *testing.T) {
	h := newHarness(t, telegramOn, "    notify: { enabled: false }\n")
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusFailed)

	h.hub.Notify(d.ID)
	waitFor(t, "the event to be dropped", func() bool { return h.hub.queueLen() == 0 })
	if h.provider.count() != 0 {
		t.Errorf("a muted service must report nothing, got %+v", h.provider.events())
	}
}

// Behavior: a service can subscribe to more than the global default, and its
// override replaces the global list rather than adding to it.
func TestServiceOverrideWidensTheVocabulary(t *testing.T) {
	h := newHarness(t, telegramOn, "    notify: { events: [deploy.succeeded] }\n")
	h.hub.Start()
	d := h.settle("web", model.StatusSucceeded, model.StatusSucceeded)

	h.hub.Notify(d.ID)
	waitFor(t, "the success notification", func() bool { return h.provider.count() == 1 })
	if got := h.provider.events()[0].Kind; got != model.EventDeploySucceeded {
		t.Errorf("kind = %q, want deploy.succeeded", got)
	}
}

// Behavior: a delivery that fails is retried, and one that keeps failing is
// eventually dropped with a log line — a dead backend must not strand the
// events queued behind it.
func TestDeliveryRetriesThenGivesUp(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.provider.fails = 99
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusFailed)

	h.hub.Notify(d.ID)
	waitFor(t, "the event to be given up on", func() bool { return h.hub.queueLen() == 0 })

	h.mu.Lock()
	attempts := h.attempts
	h.mu.Unlock()
	if attempts != h.hub.maxAttempts() {
		t.Errorf("tried %d times, want MaxAttempts (%d)", attempts, h.hub.maxAttempts())
	}
	if h.provider.count() != 0 {
		t.Error("nothing should have been delivered")
	}
}

// Behavior: a backend that recovers mid-retry gets the event through, and
// gets it exactly once.
func TestDeliverySucceedsAfterATransientFailure(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.provider.fails = 1
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusFailed)

	h.hub.Notify(d.ID)
	waitFor(t, "the retried notification", func() bool { return h.provider.count() == 1 })
	time.Sleep(20 * time.Millisecond)
	if h.provider.count() != 1 {
		t.Errorf("delivered %d times, want exactly 1", h.provider.count())
	}
}

// Behavior: the queue is bounded, and under pressure it drops the oldest —
// the newest failure is the one someone is most likely still looking at.
func TestFullQueueDropsTheOldest(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.QueueCap = 2 // no Start: nothing drains, so the queue can fill
	for _, id := range []string{"dp_1", "dp_2", "dp_3"} {
		h.hub.Notify(id)
	}
	if n := h.hub.queueLen(); n != 2 {
		t.Fatalf("queue holds %d, want the cap of 2", n)
	}
	h.hub.mu.Lock()
	defer h.hub.mu.Unlock()
	if h.hub.queue[0].deployID != "dp_2" || h.hub.queue[1].deployID != "dp_3" {
		t.Errorf("queue = %+v, want the oldest dropped", h.hub.queue)
	}
}

// Behavior: queueing the same deploy twice reports it once. The store's
// settle CAS already guarantees one call per deploy; this keeps a future
// second producer from turning into a double message.
func TestDuplicateQueueingReportsOnce(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.QueueCap = 8
	h.hub.Notify("dp_same")
	h.hub.Notify("dp_same")
	if n := h.hub.queueLen(); n != 1 {
		t.Errorf("queued %d copies, want 1", n)
	}
}

// Behavior: a deploy that retention already reclaimed is dropped quietly
// rather than retried to exhaustion — there is nothing left to report.
func TestVanishedDeployIsDroppedQuietly(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()
	h.hub.Notify("dp_gone")
	waitFor(t, "the event to be dropped", func() bool { return h.hub.queueLen() == 0 })
	if h.provider.count() != 0 {
		t.Error("a missing deploy must not produce a message")
	}
}

// Behavior: an event queued moments before shutdown still goes out. A
// rollout whose later waves are canceled by the shutdown itself settles
// during Scheduler.Shutdown, so this is the normal case, not a corner.
func TestShutdownFlushesWhatIsQueued(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusFailed)

	h.hub.Notify(d.ID)
	h.hub.Shutdown()
	if h.provider.count() != 1 {
		t.Errorf("delivered %d events across shutdown, want 1", h.provider.count())
	}
}

// Behavior: the deploy link is built from notify.base_url, and stays absent
// when there is no base_url to build it from — main does not know its own
// public address.
func TestDeployLinkFollowsBaseURL(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusFailed)
	h.hub.Notify(d.ID)
	waitFor(t, "the notification", func() bool { return h.provider.count() == 1 })
	if url := h.provider.events()[0].Deploy.URL; url != "" {
		t.Errorf("no base_url configured, want no link, got %q", url)
	}

	h2 := newHarness(t, telegramOn+"  base_url: https://deploy.example.com/\n", "")
	h2.hub.Start()
	d2 := h2.settle("web", model.StatusFailed, model.StatusFailed)
	h2.hub.Notify(d2.ID)
	waitFor(t, "the linked notification", func() bool { return h2.provider.count() == 1 })
	want := "https://deploy.example.com/ui/deploys/" + d2.ID
	if url := h2.provider.events()[0].Deploy.URL; url != want {
		t.Errorf("link = %q, want %q", url, want)
	}
}

// Behavior: config is re-read on every attempt, so a policy change lands on
// the retry rather than waiting for the next deploy.
func TestConfigIsRereadBetweenAttempts(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.provider.fails = 1
	// Mute the service between the failing attempt and the retry. Installed
	// before Start so the delivery goroutine never races the assignment.
	h.hub.newProvider = func(config.Notify) (Provider, error) {
		h.mu.Lock()
		h.attempts++
		first := h.attempts == 1
		cur := h.cfg
		h.mu.Unlock()
		if first {
			svc := *cur.Services["web"]
			svc.Notify = config.ServiceNotify{Enabled: false}
			next := *cur
			next.Services = map[string]*config.Service{"web": &svc}
			h.setConfig(&next)
		}
		return h.provider, nil
	}
	h.hub.Start()
	d := h.settle("web", model.StatusFailed, model.StatusFailed)

	h.hub.Notify(d.ID)
	waitFor(t, "the event to be dropped", func() bool { return h.hub.queueLen() == 0 })
	if h.provider.count() != 0 {
		t.Errorf("the retry must honor the muted config, got %+v", h.provider.events())
	}
}

// Behavior: Notify returns at once and does no I/O. It is called straight
// from the webhook's request goroutine, where a hand-off that waited on the
// network would surface as a CI-visible webhook timeout — and from the
// scheduler's own loop, where it would delay the next deploy.
func TestNotifyReturnsImmediatelyWhileDeliveryIsStuck(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	blocked := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.hub.newProvider = func(config.Notify) (Provider, error) {
		return providerFunc(func(context.Context, Event) error {
			once.Do(func() { close(blocked) })
			<-release
			return nil
		}), nil
	}
	h.hub.Start()
	defer close(release)

	stuck := h.settle("web", model.StatusFailed, model.StatusFailed)
	h.hub.Notify(stuck.ID)
	<-blocked // delivery is now wedged inside the provider

	next := h.settle("web", model.StatusFailed, model.StatusFailed)
	start := time.Now()
	h.hub.Notify(next.ID)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Notify took %s while a delivery was stuck; it must never wait on one", elapsed)
	}
}

// providerFunc adapts a function to Provider.
type providerFunc func(ctx context.Context, ev Event) error

func (f providerFunc) Send(ctx context.Context, ev Event) error { return f(ctx, ev) }

// ── node events ────────────────────────────────────────────────────────────

// Behavior: main coming up reports its version, under the default policy and
// without anyone editing notify.events first.
func TestMainStartedReportsTheVersion(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()

	h.hub.MainStarted("v1.2.3")
	waitFor(t, "the startup notification", func() bool { return h.provider.count() == 1 })

	ev := h.provider.events()[0]
	if ev.Kind != model.EventMainStarted {
		t.Errorf("kind = %q, want main.started", ev.Kind)
	}
	if ev.Deploy != nil {
		t.Errorf("a node event must carry no deploy payload, got %+v", ev.Deploy)
	}
	if ev.Node == nil || ev.Node.Type != NodeMain || ev.Node.ReleaseVersion != "v1.2.3" {
		t.Errorf("node payload = %+v, want the main node at v1.2.3", ev.Node)
	}
	if ev.CreatedAt.IsZero() {
		t.Error("the Hub stamps CreatedAt when a node event is queued")
	}
}

// Behavior: an edge outage names the server, how long it has been gone and
// the version it was last seen running — enough to act on without opening
// the UI.
func TestEdgeOfflineNamesTheServerAndTheOutage(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()

	h.hub.EdgeOffline("edge-01", "v1.2.3", 7*time.Minute)
	waitFor(t, "the outage notification", func() bool { return h.provider.count() == 1 })

	n := h.provider.events()[0].Node
	if n == nil {
		t.Fatal("edge.offline must carry a node payload")
	}
	if n.Type != NodeEdge || n.Name != "edge-01" {
		t.Errorf("node = %+v, want the edge-01 edge", n)
	}
	if n.DownDuration != 7*time.Minute || n.ReleaseVersion != "v1.2.3" {
		t.Errorf("node = %+v, want 7m down at v1.2.3", n)
	}
}

// Behavior: the all-clear is its own kind, so subscribing to outages and
// subscribing to recoveries are separate choices.
func TestEdgeOnlineIsItsOwnKind(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()

	h.hub.EdgeOnline("edge-01", "v1.2.4", 9*time.Minute)
	waitFor(t, "the all-clear", func() bool { return h.provider.count() == 1 })

	ev := h.provider.events()[0]
	if ev.Kind != model.EventEdgeOnline {
		t.Errorf("kind = %q, want edge.online", ev.Kind)
	}
	if ev.Node.DownDuration != 9*time.Minute {
		t.Errorf("the all-clear should report the whole outage, got %s", ev.Node.DownDuration)
	}
}

// Behavior: node events answer to the global list and nothing else. A
// service cannot mute them, because they are not about a service — this is
// the counterpart of TestMutedServiceReportsNothing.
func TestNodeEventsIgnoreServicePolicy(t *testing.T) {
	h := newHarness(t, telegramOn, "    notify: { enabled: false }\n")
	h.hub.Start()

	h.hub.MainStarted("v1.2.3")
	waitFor(t, "the startup notification", func() bool { return h.provider.count() == 1 })
}

// Behavior: dropping a node kind from the global list silences it, and does
// so at delivery time — a reload during a retry is honored, exactly as it is
// on the deploy path.
func TestNodeEventDroppedFromTheGlobalListIsSilent(t *testing.T) {
	h := newHarness(t, telegramOn+"  events: [deploy.failed]\n", "")
	h.hub.Start()

	h.hub.EdgeOffline("edge-01", "", time.Minute)
	waitFor(t, "the event to be dropped", func() bool { return h.hub.queueLen() == 0 })
	if h.provider.count() != 0 {
		t.Errorf("edge.offline is not subscribed, got %+v", h.provider.events())
	}
}

// Behavior: notifications being off drops node events before they ever
// queue, the same as it does deploys.
func TestNoProviderMeansNoNodeEventIsQueued(t *testing.T) {
	h := newHarness(t, "", "")
	h.hub.Start()

	h.hub.MainStarted("v1.2.3")
	h.hub.EdgeOffline("edge-01", "", time.Minute)
	time.Sleep(20 * time.Millisecond)
	if n := h.hub.queueLen(); n != 0 {
		t.Errorf("queued %d node events with notify off, want 0", n)
	}
}

// Behavior: a repeat of the same news about the same node collapses, while
// the offline/online pair and two different servers stay distinct. The
// watcher already fires once per outage; this keeps a future second producer
// from turning into a double message.
func TestQueuedNodeEventsDedupePerNodeAndKind(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.QueueCap = 8 // no Start: nothing drains, so the queue can be read

	h.hub.EdgeOffline("edge-01", "", time.Minute)
	h.hub.EdgeOffline("edge-01", "", 2*time.Minute)
	if n := h.hub.queueLen(); n != 1 {
		t.Fatalf("queued %d copies of one outage, want 1", n)
	}
	h.hub.EdgeOffline("edge-02", "", time.Minute)
	h.hub.EdgeOnline("edge-01", "", 3*time.Minute)
	if n := h.hub.queueLen(); n != 3 {
		t.Errorf("queue holds %d, want the other server and the all-clear alongside", n)
	}
}

// Behavior: a node event queued moments before shutdown still goes out —
// main.started is queued during startup, but an outage detected seconds
// before a restart is exactly the one worth keeping.
func TestShutdownFlushesQueuedNodeEvents(t *testing.T) {
	h := newHarness(t, telegramOn, "")
	h.hub.Start()

	h.hub.EdgeOffline("edge-01", "v1", time.Hour)
	h.hub.Shutdown()
	if h.provider.count() != 1 {
		t.Errorf("delivered %d events across shutdown, want 1", h.provider.count())
	}
}
