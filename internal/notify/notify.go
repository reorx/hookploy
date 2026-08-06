// Package notify tells someone when a deploy ends badly. Producers — the
// scheduler when a rollout settles, the webhook handler when a deploy is born
// already failed — hand Hub a deploy id and return; a single background
// worker then loads the deploy, decides whether it is worth reporting, and
// delivers it through the one configured backend.
//
// The split follows internal/edge's: Event is the transport-neutral payload
// (like edge.Task) and Provider is the swappable backend (like
// edge.Transport), with the two implementations sitting side by side in this
// package rather than in sub-packages. Rendering is private to each Provider,
// so the message formatting written for Telegram is not in the way of a
// notification-center backend that only wants the structured event.
//
// Delivery is best effort by design: a bounded in-memory queue (oldest
// dropped when it overflows), a handful of retries with backoff, then a log
// line and on to the next one. Nothing here is persisted, and nothing here
// can hold up a deploy — Notify does no I/O at all.
package notify

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/store"
)

const (
	defaultQueueCap    = 64
	defaultMaxAttempts = 4
	defaultBackoffBase = 2 * time.Second
	defaultBackoffMax  = 30 * time.Second
	// sendTimeout bounds one delivery attempt, so a hung backend gives up
	// well inside a single backoffMax window rather than wedging the queue.
	sendTimeout = 10 * time.Second
	// shutdownGrace lets events queued moments before shutdown still go out.
	// A rollout whose later waves get canceled by the shutdown itself settles
	// during Scheduler.Shutdown, so there is usually something here to send.
	shutdownGrace = 3 * time.Second
)

// InstanceResult is one execution that did not succeed.
type InstanceResult struct {
	Instance string
	Server   string
	Status   model.Status
	Error    string
}

// Event is one notification-worthy deploy outcome, fully resolved: a Provider
// can render and deliver it without reading the store or the config again.
// It carries no wire tags on purpose — same reason edge.Task carries none:
// each backend owns its own encoding, so the neutral shape stays free to
// change without silently becoming an external contract.
type Event struct {
	Kind     model.EventKind
	Service  string
	Task     string // set when the deploy ran a named task
	DeployID string
	Status   model.Status

	// Error is the deploy-level error, set when a deploy failed before it
	// ever reached the scheduler (a bad interpolation or digest). Instances
	// is empty in that case, because no execution ever existed.
	Error     string
	Instances []InstanceResult

	DeployURL  string // empty when notify.base_url is unset
	CreatedAt  time.Time
	FinishedAt time.Time
}

// Provider delivers one Event to a specific backend.
type Provider interface {
	// Send delivers ev, once. A non-nil error is retried by the Hub with
	// backoff, so Send must not retry on its own.
	Send(ctx context.Context, ev Event) error
}

// pending is one deploy waiting to be reported.
type pending struct {
	deployID string
	attempts int
}

// Hub is the async delivery worker: a bounded queue drained by one
// goroutine. Same skeleton as internal/edge/agent.go's result pump (pending
// slice, cap-1 wake channel, single writer, drop-oldest when full), with
// bounded retries instead of that one's forever-until-acknowledged loop —
// an undeliverable notification is meant to be dropped, not to pile up.
type Hub struct {
	Store  *store.Store
	Config func() *config.Config
	Logger *log.Logger

	// Tunables; zero means the package default. Tests shrink them.
	QueueCap    int
	MaxAttempts int
	BackoffBase time.Duration
	BackoffMax  time.Duration

	// newProvider resolves the active backend. A field so tests can swap in
	// a fake without standing up an HTTP endpoint.
	newProvider func(config.Notify) (Provider, error)
	sleep       func(ctx context.Context, d time.Duration) error

	wake chan struct{}

	mu    sync.Mutex
	queue []pending

	root   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New builds a Hub. Call Start to begin delivering and Shutdown to stop.
func New(st *store.Store, cfg func() *config.Config, logger *log.Logger) *Hub {
	root, cancel := context.WithCancel(context.Background())
	return &Hub{
		Store:       st,
		Config:      cfg,
		Logger:      logger,
		newProvider: func(n config.Notify) (Provider, error) { return providerFor(n, nil) },
		sleep:       sleepCtx,
		wake:        make(chan struct{}, 1),
		root:        root,
		cancel:      cancel,
	}
}

// Start launches the delivery worker.
func (h *Hub) Start() {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		h.pump()
	}()
}

// Shutdown stops the worker, giving whatever is queued one bounded last pass
// so a failure reported moments before shutdown still goes out.
func (h *Hub) Shutdown() {
	h.cancel()
	h.wg.Wait()
}

func (h *Hub) logf(format string, args ...any) {
	if h.Logger != nil {
		h.Logger.Printf(format, args...)
	}
}

// Notify queues a deploy for possible notification. It never blocks and
// never touches the store, so it is safe on any hot path — including the
// webhook handler's own request goroutine.
func (h *Hub) Notify(deployID string) {
	if h.Config().Notify.Provider == "" {
		return // notifications are off; nothing can turn them on per service
	}
	h.mu.Lock()
	for _, p := range h.queue {
		if p.deployID == deployID {
			h.mu.Unlock()
			return // defensive: the store's settle CAS already gates callers
		}
	}
	if len(h.queue) >= h.queueCap() {
		dropped := h.queue[0]
		h.queue = h.queue[1:]
		h.logf("notify: queue full (%d), dropping the notification for deploy %s",
			h.queueCap(), dropped.deployID)
	}
	h.queue = append(h.queue, pending{deployID: deployID})
	h.mu.Unlock()
	h.kick()
}

func (h *Hub) kick() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Hub) pump() {
	for {
		select {
		case <-h.root.Done():
			h.finalDrain()
			return
		case <-h.wake:
		}
		h.drain(h.root)
	}
}

// finalDrain makes one last pass on a fresh, short-lived context — the root
// one is already canceled by the time we get here.
func (h *Hub) finalDrain() {
	if h.queueLen() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	h.drain(ctx)
	if n := h.queueLen(); n > 0 {
		h.logf("notify: shutting down with %d undelivered notification(s)", n)
	}
}

// drain delivers the queue in order. A failing event keeps its place at the
// head and is retried with backoff until MaxAttempts, then dropped — one
// unreachable backend must not strand everything queued behind it.
func (h *Hub) drain(ctx context.Context) {
	for ctx.Err() == nil {
		p, ok := h.peek()
		if !ok {
			return
		}
		err := h.deliver(ctx, p.deployID)
		if err == nil {
			h.popIf(p.deployID)
			continue
		}
		attempts := p.attempts + 1
		h.logf("notify: deploy %s: delivery attempt %d/%d failed: %v",
			p.deployID, attempts, h.maxAttempts(), err)
		if attempts >= h.maxAttempts() {
			h.logf("notify: deploy %s: giving up after %d attempts", p.deployID, attempts)
			h.popIf(p.deployID)
			continue
		}
		h.setAttempts(p.deployID, attempts)
		if err := h.sleep(ctx, h.backoff(attempts)); err != nil {
			return
		}
	}
}

// deliver reports one deploy. Config is read here, per attempt, so a reload
// mid-retry — a rotated token, a corrected chat id — takes effect on the
// next try. A nil error means "done with this event", whether it was sent or
// policy said it should not be.
func (h *Hub) deliver(ctx context.Context, deployID string) error {
	cfg := h.Config()
	if cfg.Notify.Provider == "" {
		return nil
	}
	d, err := h.Store.GetDeploy(deployID)
	if err != nil {
		return err
	}
	if d == nil {
		return nil // retention reclaimed it; there is nothing left to report
	}
	ev, ok, err := h.buildEvent(cfg, d)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	provider, err := h.newProvider(cfg.Notify)
	if err != nil {
		// A misconfigured backend is not transient; retrying cannot fix it.
		h.logf("notify: %v", err)
		return nil
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	return provider.Send(sendCtx, ev)
}

// ── queue helpers ──────────────────────────────────────────────────────────

func (h *Hub) peek() (pending, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) == 0 {
		return pending{}, false
	}
	return h.queue[0], true
}

// popIf drops the head only if it is still the event the caller was working
// on: a full-queue drop can have taken it out from under a retry.
func (h *Hub) popIf(deployID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) > 0 && h.queue[0].deployID == deployID {
		h.queue = h.queue[1:]
	}
}

func (h *Hub) setAttempts(deployID string, attempts int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) > 0 && h.queue[0].deployID == deployID {
		h.queue[0].attempts = attempts
	}
}

func (h *Hub) queueLen() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.queue)
}

// ── tunables ───────────────────────────────────────────────────────────────

func (h *Hub) queueCap() int    { return orDefaultInt(h.QueueCap, defaultQueueCap) }
func (h *Hub) maxAttempts() int { return orDefaultInt(h.MaxAttempts, defaultMaxAttempts) }

func (h *Hub) backoff(attempts int) time.Duration {
	base := orDefaultDur(h.BackoffBase, defaultBackoffBase)
	max := orDefaultDur(h.BackoffMax, defaultBackoffMax)
	d := base
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return d
}

func orDefaultInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func orDefaultDur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
