package edgewatch

import (
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
)

// ── harness ────────────────────────────────────────────────────────────────

// threshold is short enough to cross with a sleep and long enough that a tick
// landing on the wrong side of it is a real failure rather than scheduler
// noise. Same trick edgehub's grace-window tests use: shrink the real
// duration instead of faking the clock.
const threshold = 60 * time.Millisecond

// fleet is a settable stand-in for edgehub.Hub.Edges.
type fleet struct {
	mu sync.Mutex
	up map[string]model.EdgeInfo
}

func newFleet() *fleet { return &fleet{up: map[string]model.EdgeInfo{}} }

func (f *fleet) edges() map[string]model.EdgeInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]model.EdgeInfo, len(f.up))
	for k, v := range f.up {
		out[k] = v
	}
	return out
}

func (f *fleet) attach(server, version string) {
	f.attachAt(server, version, time.Now())
}

func (f *fleet) attachAt(server, version string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.up[server] = model.EdgeInfo{Server: server, Version: version, ConnectedAt: at}
}

func (f *fleet) detach(server string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.up, server)
}

// recorder collects what the watcher reported.
type recorder struct {
	mu      sync.Mutex
	offline []Outage
	online  []Outage
}

func (r *recorder) off(o Outage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.offline = append(r.offline, o)
}

func (r *recorder) on(o Outage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.online = append(r.online, o)
}

func (r *recorder) counts() (offline, online int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.offline), len(r.online)
}

func (r *recorder) lastOffline(t *testing.T) Outage {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.offline) == 0 {
		t.Fatal("no outage was reported")
	}
	return r.offline[len(r.offline)-1]
}

func (r *recorder) lastOnline(t *testing.T) Outage {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.online) == 0 {
		t.Fatal("no all-clear was reported")
	}
	return r.online[len(r.online)-1]
}

// harness owns a watcher over a settable config and fleet. Tests drive tick
// directly unless they are about the polling loop itself.
type harness struct {
	w   *Watcher
	rec *recorder
	fl  *fleet

	mu  sync.Mutex
	cfg *config.Config
}

// newHarness builds a watcher over the named non-local servers.
func newHarness(t *testing.T, servers ...string) *harness {
	t.Helper()
	h := &harness{rec: &recorder{}, fl: newFleet()}
	h.setServers(threshold, servers...)
	h.w = &Watcher{
		Config:    h.config,
		Edges:     h.fl.edges,
		OnOffline: h.rec.off,
		OnOnline:  h.rec.on,
	}
	return h
}

func (h *harness) config() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

// setServers replaces the config with one declaring exactly these non-local
// servers, standing in for a SIGHUP reload.
func (h *harness) setServers(after time.Duration, servers ...string) {
	cfg := &config.Config{
		Servers: map[string]*config.Server{},
		Notify:  config.Notify{EdgeOfflineAfter: after},
	}
	for _, s := range servers {
		cfg.Servers[s] = &config.Server{Name: s}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = cfg
}

func (h *harness) addLocal(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.Servers[name] = &config.Server{Name: name, Local: true}
}

// past sleeps just far enough that the next tick sees the threshold crossed.
func past() { time.Sleep(threshold + threshold/2) }

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

// ── behavior ───────────────────────────────────────────────────────────────

// Behavior: a server that hookploy.yaml declares but no edge ever attaches
// for is reported once the threshold passes. This is the case a disconnect
// hook cannot see — main restarted and the edge never came back, so there
// was no disconnect to hook.
func TestServerThatNeverAttachesIsReported(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.w.tick() // baseline
	past()
	h.w.tick()

	if off, _ := h.rec.counts(); off != 1 {
		t.Fatalf("reported %d outages, want 1", off)
	}
	o := h.rec.lastOffline(t)
	if o.Server != "edge-01" {
		t.Errorf("outage names %q, want edge-01", o.Server)
	}
	if o.Duration < threshold {
		t.Errorf("outage duration = %s, want at least the %s threshold", o.Duration, threshold)
	}
	if o.Version != "" {
		t.Errorf("an edge never seen has no known version, got %q", o.Version)
	}
}

// Behavior: an absence shorter than the threshold says nothing. The whole
// point of the threshold is that edgehub's own reconnect grace absorbs a
// flapping stream, and those must not reach anyone's phone.
func TestAbsenceShorterThanTheThresholdIsSilent(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.w.tick()
	time.Sleep(threshold / 4)
	h.w.tick()

	if off, on := h.rec.counts(); off != 0 || on != 0 {
		t.Errorf("reported %d outages and %d all-clears before the threshold, want none", off, on)
	}
}

// Behavior: one outage is one message however long it lasts. An edge that
// stays down for a day must not re-alert on every poll.
func TestOneOutageIsReportedOnce(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.w.tick()
	past()
	for i := 0; i < 5; i++ {
		h.w.tick()
		time.Sleep(threshold / 4)
	}

	if off, _ := h.rec.counts(); off != 1 {
		t.Errorf("reported %d outages for one absence, want 1", off)
	}
}

// Behavior: an attached edge is never reported, whatever the threshold.
func TestAttachedEdgeIsNeverReported(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.fl.attach("edge-01", "v1")
	for i := 0; i < 4; i++ {
		h.w.tick()
		time.Sleep(threshold / 2)
	}

	if off, on := h.rec.counts(); off != 0 || on != 0 {
		t.Errorf("a healthy edge produced %d outages and %d all-clears", off, on)
	}
}

// Behavior: an edge coming back reports the whole outage and the version it
// returned on — a rolling edge upgrade is the everyday reason for one, and
// the version is how you tell the upgrade landed.
func TestRecoveryReportsTheWholeOutageAndTheNewVersion(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.fl.attach("edge-01", "v1")
	h.w.tick()
	h.fl.detach("edge-01")
	past()
	h.w.tick() // outage
	time.Sleep(threshold)
	h.fl.attach("edge-01", "v2")
	h.w.tick() // all-clear

	off, on := h.rec.counts()
	if off != 1 || on != 1 {
		t.Fatalf("reported %d outages and %d all-clears, want one of each", off, on)
	}
	o := h.rec.lastOnline(t)
	if o.Version != "v2" {
		t.Errorf("all-clear version = %q, want the one it came back on", o.Version)
	}
	if o.Duration < 2*threshold {
		t.Errorf("all-clear duration = %s, want the whole outage, not just the last leg", o.Duration)
	}
}

// Behavior: the all-clear times the outage to when the edge actually
// attached, not to the poll that noticed. Polling means the two can be a
// whole interval apart, and reporting the later one would inflate every
// outage by however long the watcher happened to take to look.
func TestRecoveryTimesTheOutageToTheAttachNotThePoll(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.w.tick() // baseline
	start := time.Now()
	past()
	h.w.tick() // outage announced

	h.fl.attach("edge-01", "v1")
	attachedAt := time.Now()
	time.Sleep(threshold) // the poll lags the reconnect by this much
	h.w.tick()

	got := h.rec.lastOnline(t).Duration
	trueOutage := attachedAt.Sub(start)
	if got > trueOutage+threshold/2 {
		t.Errorf("all-clear reported %s for a %s outage; the poll lag leaked in", got, trueOutage)
	}
	if got < trueOutage/2 {
		t.Errorf("all-clear reported %s, far short of the %s outage", got, trueOutage)
	}
}

// Behavior: an edge whose attach time is unusable — a zero value, or one
// predating the last time we saw it — still gets an all-clear, timed to the
// poll. A missing timestamp must not produce a negative or absurd duration.
func TestRecoveryFallsBackWhenTheAttachTimeIsUnusable(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.w.tick()
	past()
	h.w.tick() // outage announced

	h.fl.attachAt("edge-01", "v1", time.Time{})
	h.w.tick()

	if got := h.rec.lastOnline(t).Duration; got < threshold {
		t.Errorf("all-clear duration = %s, want at least the %s the outage really lasted", got, threshold)
	}
}

// Behavior: an outage that was never announced gets no all-clear. A brief
// reconnect must not produce a lone "edge online" for something nobody was
// told had gone.
func TestRecoveryWithoutAnAnnouncedOutageIsSilent(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.fl.attach("edge-01", "v1")
	h.w.tick()
	h.fl.detach("edge-01")
	time.Sleep(threshold / 4)
	h.w.tick()
	h.fl.attach("edge-01", "v1")
	h.w.tick()

	if off, on := h.rec.counts(); off != 0 || on != 0 {
		t.Errorf("a flap produced %d outages and %d all-clears, want none", off, on)
	}
}

// Behavior: an outage reports the version the edge was last seen running.
// That is what someone needs to know which build went dark.
func TestOutageReportsTheLastKnownVersion(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.fl.attach("edge-01", "v0.4.1")
	h.w.tick()
	h.fl.detach("edge-01")
	past()
	h.w.tick()

	if v := h.rec.lastOffline(t).Version; v != "v0.4.1" {
		t.Errorf("outage version = %q, want the last one seen", v)
	}
}

// Behavior: a local server is main's own in-process executor. There is no
// edge to lose, so it is never watched.
func TestLocalServersAreNotWatched(t *testing.T) {
	h := newHarness(t)
	h.addLocal("box")
	h.w.tick()
	past()
	h.w.tick()

	if off, _ := h.rec.counts(); off != 0 {
		t.Errorf("a local server was reported offline %d times", off)
	}
}

// Behavior: a server dropped from the config stops being watched, and coming
// back to the config starts it clean. A stale clock would otherwise fire the
// moment it reappears, blaming it for the time it was not even declared.
func TestServerRemovedFromTheConfigIsForgotten(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.w.tick()
	past()
	h.w.tick() // outage announced
	if off, _ := h.rec.counts(); off != 1 {
		t.Fatalf("test setup: want the outage announced first, got %d", off)
	}

	h.setServers(threshold) // reload drops edge-01
	h.w.tick()
	h.setServers(threshold, "edge-01") // and a later reload puts it back
	h.w.tick()

	off, on := h.rec.counts()
	if on != 0 {
		t.Errorf("re-declaring a server is not a recovery, got %d all-clears", on)
	}
	if off != 1 {
		t.Errorf("reported %d outages, want the original one only — the clock must restart", off)
	}
}

// Behavior: the threshold is read every tick, so lowering it over SIGHUP
// takes effect against an outage already under way rather than at the next
// one.
func TestThresholdIsRereadEveryTick(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.setServers(time.Hour, "edge-01")
	h.w.tick()
	time.Sleep(threshold)
	h.w.tick()
	if off, _ := h.rec.counts(); off != 0 {
		t.Fatalf("test setup: an hour-long threshold must not have fired, got %d", off)
	}

	h.setServers(threshold/2, "edge-01")
	h.w.tick()
	if off, _ := h.rec.counts(); off != 1 {
		t.Errorf("reported %d outages after the threshold was lowered, want 1", off)
	}
}

// Behavior: a non-positive threshold turns outage reporting off instead of
// alerting on everything at once. config never produces one, but a zero
// value must fail quiet rather than loud.
func TestNonPositiveThresholdReportsNothing(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.setServers(0, "edge-01")
	h.w.tick()
	past()
	h.w.tick()

	if off, _ := h.rec.counts(); off != 0 {
		t.Errorf("a zero threshold reported %d outages, want none", off)
	}
}

// Behavior: Start polls on its own and Shutdown stops it, so main gets
// outage reports without driving anything and a shutting-down process stops
// producing them before the notifier is drained.
func TestStartPollsAndShutdownStops(t *testing.T) {
	h := newHarness(t, "edge-01")
	h.w.Interval = threshold / 4
	h.w.Start()
	waitFor(t, "the outage to be reported", func() bool {
		off, _ := h.rec.counts()
		return off == 1
	})

	h.w.Shutdown()
	h.fl.attach("edge-01", "v1")
	time.Sleep(4 * h.w.Interval)
	if _, on := h.rec.counts(); on != 0 {
		t.Errorf("a stopped watcher kept reporting: %d all-clears after Shutdown", on)
	}
}

// Behavior: Shutdown on a watcher that never started is a no-op, so a
// startup that fails between construction and Start can still defer it.
func TestShutdownBeforeStartIsSafe(t *testing.T) {
	newHarness(t, "edge-01").w.Shutdown()
}
