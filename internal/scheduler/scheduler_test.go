package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/runner"
	"github.com/reorx/hookploy/internal/store"
)

// ── harness ────────────────────────────────────────────────────────────────

// gateRunner blocks every command until released, reporting starts.
type gateRunner struct {
	inner   runner.Runner
	mu      sync.Mutex
	started []runner.Cmd
	release chan struct{}
	notify  chan runner.Cmd
}

func newGateRunner(inner runner.Runner) *gateRunner {
	return &gateRunner{inner: inner, release: make(chan struct{}), notify: make(chan runner.Cmd, 64)}
}

func (g *gateRunner) Run(ctx context.Context, c runner.Cmd) (int, error) {
	g.mu.Lock()
	g.started = append(g.started, c)
	rel := g.release
	g.mu.Unlock()
	g.notify <- c
	select {
	case <-rel:
	case <-ctx.Done():
		return -1, ctx.Err()
	}
	return g.inner.Run(ctx, c)
}

func (g *gateRunner) releaseAll() {
	g.mu.Lock()
	close(g.release)
	g.release = make(chan struct{})
	g.mu.Unlock()
}

type harness struct {
	t     *testing.T
	store *store.Store
	sched *Scheduler
	fake  *runner.FakeRunner
	gate  *gateRunner
	reg   *executor.Registry
}

func newHarness(t *testing.T, gated bool, window time.Duration, servers ...string) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := &runner.FakeRunner{}
	var r runner.Runner = fake
	var gate *gateRunner
	if gated {
		gate = newGateRunner(fake)
		r = gate
	}
	eng := &engine.Engine{
		Runner: r,
		Sleep:  func(ctx context.Context, d time.Duration) error { return nil },
	}
	if window == 0 {
		window = 50 * time.Millisecond
	}
	reg := executor.NewRegistry(window)
	local := &executor.Local{Engine: eng}
	if len(servers) == 0 {
		servers = []string{"s1", "s2"}
	}
	for _, s := range servers {
		reg.Register(s, local)
	}
	sched := New(st, reg)
	t.Cleanup(sched.Shutdown)
	return &harness{t: t, store: st, sched: sched, fake: fake, gate: gate, reg: reg}
}

// service builds a normalized single- or multi-instance service.
func service(name string, waves [][]string, steps string) *config.Service {
	cfg := `
servers:
  s1: { local: true }
  s2: { local: true }
  s3: {}
services:
  ` + name + `:
`
	if waves == nil {
		cfg += "    server: s1\n    dir: /opt/" + name + "\n"
	} else {
		cfg += "    dir: /opt/" + name + "\n    instances:\n"
		seen := map[string]bool{}
		for _, w := range waves {
			for _, inst := range w {
				if seen[inst] {
					continue
				}
				seen[inst] = true
				server := "s1"
				if strings.HasPrefix(inst, "sg") {
					server = "s2"
				}
				if strings.HasPrefix(inst, "off") {
					server = "s3"
				}
				cfg += "      " + inst + ": { server: " + server + ", dir: /opt/" + name + "-" + inst + " }\n"
			}
		}
		cfg += "    rollout:\n"
		for _, w := range waves {
			cfg += "      - [" + strings.Join(w, ", ") + "]\n"
		}
	}
	cfg += "    image: ghcr.io/x/" + name + "\n    deploy: " + steps + "\n"
	c, err := parseConfigString(cfg)
	if err != nil {
		panic(err)
	}
	return c.Services[name]
}

func parseConfigString(y string) (*config.Config, error) {
	dir, err := os.MkdirTemp("", "hookploy-sched-*")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "hookploy.yaml")
	if err := os.WriteFile(path, []byte(y), 0o644); err != nil {
		return nil, err
	}
	return config.Load(path)
}

func (h *harness) enqueue(svc *config.Service, payload string) *model.Deploy {
	h.t.Helper()
	var p map[string]any
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			h.t.Fatal(err)
		}
	}
	d, execs, err := BuildDeploy(svc, model.KindDeploy, "", "", p, json.RawMessage(payload))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.CreateDeploy(d, execs); err != nil {
		h.t.Fatal(err)
	}
	h.sched.Enqueue(d.Service, d.ID)
	return d
}

func (h *harness) waitStatus(id string, want model.Status) *model.Deploy {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d, err := h.store.GetDeploy(id)
		if err != nil {
			h.t.Fatal(err)
		}
		if d.Status == want {
			return d
		}
		if d.Status.Terminal() && d.Status != want {
			h.t.Fatalf("deploy %s reached %s, want %s (error: %s)", id, d.Status, want, d.Error)
		}
		time.Sleep(5 * time.Millisecond)
	}
	d, _ := h.store.GetDeploy(id)
	h.t.Fatalf("timeout waiting for %s to become %s (now %s)", id, want, d.Status)
	return nil
}

// ── behaviors ──────────────────────────────────────────────────────────────

// Behavior: deploys of the same service run strictly serially; a third
// webhook during a run supersedes the queued second one (latest wins:
// N pushes → at most 2 executions).
func TestSerialAndLatestWins(t *testing.T) {
	h := newHarness(t, true, 0)
	svc := service("app", nil, `[{run: {argv: [deploy-step]}}]`)

	d1 := h.enqueue(svc, "")
	<-h.gate.notify // d1 is now running
	d2 := h.enqueue(svc, "")
	d3 := h.enqueue(svc, "")

	h.waitStatus(d2.ID, model.StatusSuperseded)
	h.gate.releaseAll()
	h.waitStatus(d1.ID, model.StatusSucceeded)
	<-h.gate.notify // d3 running
	h.gate.releaseAll()
	h.waitStatus(d3.ID, model.StatusSucceeded)

	if n := len(h.fake.Calls); n != 2 {
		t.Fatalf("3 triggers must yield exactly 2 executions, got %d: %v", n, h.fake.JoinedCalls())
	}
}

// Behavior: different services deploy in parallel.
func TestServicesRunInParallel(t *testing.T) {
	h := newHarness(t, true, 0)
	a := service("aaa", nil, `[{run: {argv: [step-a]}}]`)
	b := service("bbb", nil, `[{run: {argv: [step-b]}}]`)
	da := h.enqueue(a, "")
	db := h.enqueue(b, "")

	// both must start before either is released
	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case c := <-h.gate.notify:
			got[c.Argv[0]] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %v started; services must run in parallel", got)
		}
	}
	if !got["step-a"] || !got["step-b"] {
		t.Fatalf("both services should have started: %v", got)
	}
	h.gate.releaseAll()
	h.waitStatus(da.ID, model.StatusSucceeded)
	h.waitStatus(db.ID, model.StatusSucceeded)
}

// Behavior: wave k+1 starts only after wave k succeeds; a wave failure
// cancels later waves and fails the rollout, keeping per-instance statuses.
func TestWaveGatingAndCancel(t *testing.T) {
	h := newHarness(t, false, 0)
	svc := service("vf", [][]string{{"m0"}, {"sg0", "m1"}}, `[{run: {argv: [deploy-step]}}]`)

	// happy path: all waves run
	d := h.enqueue(svc, "")
	h.waitStatus(d.ID, model.StatusSucceeded)
	if n := len(h.fake.Calls); n != 3 {
		t.Fatalf("3 instances should have run, got %d", n)
	}

	// wave 1 fails → wave 2 canceled
	h2 := newHarness(t, false, 0)
	h2.fake.On("deploy-step").Dir = "/opt/vf2-m0"
	h2.fake.Rules[0].Exit = 1
	svc2 := service("vf2", [][]string{{"m0"}, {"sg0"}}, `[{run: {argv: [deploy-step]}}]`)
	d2 := h2.enqueue(svc2, "")
	h2.waitStatus(d2.ID, model.StatusFailed)
	execs, _ := h2.store.ListExecutions(d2.ID)
	byInst := map[string]model.Status{}
	for _, ex := range execs {
		byInst[ex.Instance] = ex.Status
	}
	if byInst["m0"] != model.StatusFailed || byInst["sg0"] != model.StatusCanceled {
		t.Fatalf("per-instance statuses wrong: %v", byInst)
	}
	if n := len(h2.fake.Calls); n != 1 {
		t.Fatalf("wave 2 must not run after wave 1 failed, calls=%d", n)
	}
}

// Behavior: without a payload digest, wave 1's first instance resolves it
// and every other instance pins the same digest (one notification, one
// image, all nodes).
func TestRolloutDigestPromotion(t *testing.T) {
	h := newHarness(t, false, 0)
	img := "ghcr.io/x/vf"
	dg := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h.fake.On("docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", img+":latest").
		Returning(img+"@"+dg+"\n", 0)
	h.fake.On("docker", "image", "inspect", "--format", "{{.Id}}", img+"@"+dg).Returning("sha256:pinned\n", 0)
	h.fake.On("docker", "compose", "ps", "-q").Returning("c1\n", 0)
	h.fake.On("docker", "inspect", "--format", "{{.Image}}").Returning("sha256:pinned\n", 0)

	svc := service("vf", [][]string{{"m0"}, {"sg0"}}, `[image.pin, compose.up]`)
	d := h.enqueue(svc, "{}")
	got := h.waitStatus(d.ID, model.StatusSucceeded)
	if got.Digest != dg {
		t.Fatalf("rollout digest not recorded: %q", got.Digest)
	}
	latestPulls, digestPulls := 0, 0
	for _, c := range h.fake.JoinedCalls() {
		switch c {
		case "docker pull " + img + ":latest":
			latestPulls++
		case "docker pull " + img + "@" + dg:
			digestPulls++
		}
	}
	if latestPulls != 1 {
		t.Fatalf(":latest must be resolved exactly once for the rollout, got %d", latestPulls)
	}
	if digestPulls != 2 {
		t.Fatalf("both instances must pull the promoted digest, got %d", digestPulls)
	}
}

// Behavior: an execution exceeding the service timeout is killed and fails.
func TestExecutionTimeout(t *testing.T) {
	h := newHarness(t, false, 0)
	h.fake.On("deploy-step").BlockUntilCancel = true
	svc := service("slow", nil, `[{run: {argv: [deploy-step]}}]`)
	svc.Timeout = 60 * time.Millisecond

	d := h.enqueue(svc, "")
	got := h.waitStatus(d.ID, model.StatusFailed)
	execs, _ := h.store.ListExecutions(got.ID)
	if execs[0].Status != model.StatusFailed || !strings.Contains(execs[0].Error, "timeout") {
		t.Fatalf("execution should fail with timeout: %+v", execs[0])
	}
}

// Behavior: a server with no executor becomes unreachable after the window.
func TestUnreachableServer(t *testing.T) {
	h := newHarness(t, false, 40*time.Millisecond)
	svc := service("edge", [][]string{{"off0"}}, `[{run: {argv: [x]}}]`) // off0 → s3, never registered
	d := h.enqueue(svc, "")
	got := h.waitStatus(d.ID, model.StatusUnreachable)
	execs, _ := h.store.ListExecutions(got.ID)
	if execs[0].Status != model.StatusUnreachable {
		t.Fatalf("execution status: %s", execs[0].Status)
	}
	if len(h.fake.Calls) != 0 {
		t.Fatal("nothing must execute for an unreachable server")
	}
}

// Behavior: after a main restart, in-flight executions are failed, and of
// several queued deploys per service only the newest is scheduled.
func TestRecoverAfterRestart(t *testing.T) {
	h := newHarness(t, false, 0)
	svc := service("app", nil, `[{run: {argv: [deploy-step]}}]`)

	// simulate pre-restart state written by a previous process
	mk := func(status model.Status) *model.Deploy {
		d, execs, err := BuildDeploy(svc, model.KindDeploy, "", "", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.store.CreateDeploy(d, execs); err != nil {
			t.Fatal(err)
		}
		if status != model.StatusQueued {
			for _, ex := range execs {
				h.store.TransitionExecution(ex.ID, model.StatusQueued, model.StatusDispatching, "")
				if status == model.StatusRunning {
					h.store.TransitionExecution(ex.ID, model.StatusDispatching, model.StatusRunning, "")
				}
			}
			h.store.RecomputeDeployStatus(d.ID)
		}
		return d
	}
	running := mk(model.StatusRunning)
	queuedOld := mk(model.StatusQueued)
	time.Sleep(2 * time.Millisecond)
	queuedNew := mk(model.StatusQueued)

	if err := h.sched.Recover(); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(queuedNew.ID, model.StatusSucceeded)

	if d, _ := h.store.GetDeploy(running.ID); d.Status != model.StatusFailed || !strings.Contains(d.Error+execError(t, h, d.ID), "restart") {
		t.Fatalf("running deploy after restart: %+v", d)
	}
	if d, _ := h.store.GetDeploy(queuedOld.ID); d.Status != model.StatusSuperseded {
		t.Fatalf("older queued deploy should be superseded, got %s", d.Status)
	}
	if n := len(h.fake.Calls); n != 1 {
		t.Fatalf("only the newest queued deploy runs, calls=%d", n)
	}
}

func execError(t *testing.T, h *harness, deployID string) string {
	execs, err := h.store.ListExecutions(deployID)
	if err != nil {
		t.Fatal(err)
	}
	var s string
	for _, ex := range execs {
		s += ex.Error
	}
	return s
}
