// Package notify tells someone when a deploy ends badly, or when a node
// changes state. Producers — the scheduler when a rollout settles, the
// webhook handler when a deploy is born already failed, cmd_main when this
// process comes up, the edge watcher when a server stops answering — hand
// Hub the news and return; a single background worker then decides whether
// it is worth reporting and delivers it through the one configured backend.
//
// Deploy news arrives as an id and is resolved from the store at delivery
// time, so a config reload mid-retry is honored and a deploy that retention
// reclaimed is dropped quietly. Node news arrives already complete: it lives
// only in memory and there is nothing to look it up in later.
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

// Event is one notification-worthy occurrence, fully resolved: a Provider
// can render and deliver it without reading the store or the config again.
// It carries no wire tags on purpose — same reason edge.Task carries none:
// each backend owns its own encoding, so the neutral shape stays free to
// change without silently becoming an external contract.
//
// Exactly one payload is set, and which one is not a matter of trust:
// Kind.Scope() says. ScopeDeploy fills Deploy, ScopeNode fills Node.
type Event struct {
	Kind      model.EventKind
	CreatedAt time.Time

	Deploy *DeployEvent
	Node   *NodeEvent
}

// DeployEvent is the outcome of one rollout.
type DeployEvent struct {
	Service  string
	Task     string // set when the deploy ran a named task
	DeployID string
	Status   model.Status

	// Error is the deploy-level error, set when a deploy failed before it
	// ever reached the scheduler (a bad interpolation or digest). Instances
	// is empty in that case, because no execution ever existed.
	Error     string
	Instances []InstanceResult

	URL        string // deploy detail page; empty when notify.base_url is unset
	FinishedAt time.Time
}

// NodeType names which half of the deployment a node is. main and the edges
// are reported through one shape because they are the same kind of fact —
// a participant in this installation changed state.
type NodeType string

const (
	NodeMain NodeType = "main"
	NodeEdge NodeType = "edge"
)

// NodeEvent is one node's state or change of state.
type NodeEvent struct {
	Type NodeType
	Name string // server name; "main" for the main process
	// ReleaseVersion is the version this node runs. For an edge going
	// offline it is the last one it handshaked with, and empty if it has not
	// been seen at all since main started.
	ReleaseVersion string
	// DownDuration is how long the node has been gone (edge.offline) or was
	// gone (edge.online). Zero for main.started.
	DownDuration time.Duration
}

// Provider delivers one Event to a specific backend.
type Provider interface {
	// Send delivers ev, once. A non-nil error is retried by the Hub with
	// backoff, so Send must not retry on its own.
	Send(ctx context.Context, ev Event) error
}

// pending is one thing waiting to be reported: either a deploy id still to
// be resolved from the store, or an event that already arrived complete.
type pending struct {
	deployID string
	event    *Event
	attempts int
}

// key identifies a queued item across the peek/retry/pop cycle, and doubles
// as the dedup key: a repeat of the same deploy, or of the same news about
// the same node, is one message.
func (p pending) key() string {
	if p.event == nil {
		return "deploy:" + p.deployID
	}
	k := "event:" + string(p.event.Kind)
	if p.event.Node != nil {
		k += ":" + p.event.Node.Name
	}
	return k
}

// describe names a queued item for the log.
func (p pending) describe() string {
	if p.event == nil {
		return "deploy " + p.deployID
	}
	if p.event.Node != nil {
		return string(p.event.Kind) + " on " + p.event.Node.Name
	}
	return string(p.event.Kind)
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
func (h *Hub) Notify(deployID string) { h.enqueue(pending{deployID: deployID}) }

// MainStarted reports that this process is up. cmd_main calls it once, after
// both listeners are bound, so a port already in use never announces a start
// that did not happen.
func (h *Hub) MainStarted(version string) {
	h.node(model.EventMainStarted, NodeEvent{Type: NodeMain, Name: "main", ReleaseVersion: version})
}

// EdgeOffline reports an edge that has been gone past the configured
// threshold. version is the last one it handshaked with, empty if it has not
// connected at all since main started.
func (h *Hub) EdgeOffline(server, version string, down time.Duration) {
	h.node(model.EventEdgeOffline, NodeEvent{
		Type: NodeEdge, Name: server, ReleaseVersion: version, DownDuration: down,
	})
}

// EdgeOnline is the all-clear for an edge that was reported offline. Only an
// outage that was announced gets one, so a reconnect nobody was told about
// stays quiet.
func (h *Hub) EdgeOnline(server, version string, down time.Duration) {
	h.node(model.EventEdgeOnline, NodeEvent{
		Type: NodeEdge, Name: server, ReleaseVersion: version, DownDuration: down,
	})
}

// node queues one node event, so the three methods above each say only what
// their event carries.
func (h *Hub) node(kind model.EventKind, n NodeEvent) {
	h.enqueue(pending{event: &Event{Kind: kind, Node: &n}})
}

// enqueue is the one ingress: it drops the item when notifications are off,
// collapses a repeat of something already waiting, and evicts the oldest
// when the queue is full.
func (h *Hub) enqueue(p pending) {
	if h.Config().Notify.Provider == "" {
		return // notifications are off; nothing can turn them on per service
	}
	if p.event != nil {
		p.event.CreatedAt = time.Now()
	}
	key := p.key()
	h.mu.Lock()
	for _, q := range h.queue {
		if q.key() == key {
			h.mu.Unlock()
			// Defensive: the store's settle CAS gates the deploy path and the
			// watcher's own alerted flag gates the node one.
			return
		}
	}
	if len(h.queue) >= h.queueCap() {
		dropped := h.queue[0]
		h.queue = h.queue[1:]
		h.logf("notify: queue full (%d), dropping the notification for %s",
			h.queueCap(), dropped.describe())
	}
	h.queue = append(h.queue, p)
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
		key := p.key()
		err := h.deliver(ctx, p)
		if err == nil {
			h.popIf(key)
			continue
		}
		attempts := p.attempts + 1
		h.logf("notify: %s: delivery attempt %d/%d failed: %v",
			p.describe(), attempts, h.maxAttempts(), err)
		if attempts >= h.maxAttempts() {
			h.logf("notify: %s: giving up after %d attempts", p.describe(), attempts)
			h.popIf(key)
			continue
		}
		h.setAttempts(key, attempts)
		if err := h.sleep(ctx, h.backoff(attempts)); err != nil {
			return
		}
	}
}

// deliver reports one queued item. Config is read here, per attempt, so a
// reload mid-retry — a rotated token, a corrected chat id, an event dropped
// from the policy — takes effect on the next try. A nil error means "done
// with this one", whether it was sent or policy said it should not be.
func (h *Hub) deliver(ctx context.Context, p pending) error {
	cfg := h.Config()
	if cfg.Notify.Provider == "" {
		return nil
	}
	ev, ok, err := h.resolve(cfg, p)
	if err != nil || !ok {
		return err
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

// resolve produces the event to send under the current policy, or reports
// that there is nothing to send. A node event arrived complete and only has
// its policy re-checked; a deploy is loaded and classified here.
func (h *Hub) resolve(cfg *config.Config, p pending) (Event, bool, error) {
	if p.event != nil {
		if !cfg.Notify.Wants(p.event.Kind) {
			return Event{}, false, nil
		}
		return *p.event, true, nil
	}
	d, err := h.Store.GetDeploy(p.deployID)
	if err != nil {
		return Event{}, false, err
	}
	if d == nil {
		return Event{}, false, nil // retention reclaimed it; nothing left to report
	}
	return h.buildEvent(cfg, d)
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

// popIf drops the head only if it is still the item the caller was working
// on: a full-queue drop can have taken it out from under a retry.
func (h *Hub) popIf(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) > 0 && h.queue[0].key() == key {
		h.queue = h.queue[1:]
	}
}

func (h *Hub) setAttempts(key string, attempts int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.queue) > 0 && h.queue[0].key() == key {
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
