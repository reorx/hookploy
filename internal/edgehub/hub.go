// Package edgehub is main's transport-agnostic core for edge sessions: it
// tracks which edges are attached, exposes every server as an
// executor.Executor, and routes execution progress back into engine sinks.
// The gRPC and SSE adapters both drive this hub; nothing here knows about
// either wire format.
package edgehub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/model"
)

// Conn is one live downstream channel to an attached edge. Adapters
// implement it; the hub calls Close when a newer attachment takes over, so
// the stale stream cannot keep serving the same server.
type Conn interface {
	SendExecution(spec engine.Spec) error
	Close()
}

// AttachInfo describes an edge that just finished its handshake.
type AttachInfo struct {
	Server    string
	Version   string
	Transport string   // grpc | sse
	Inflight  []string // execution ids the edge claims to still be running
}

// DefaultGraceWindow is how long a disconnected edge's executions are held
// before main gives up on them. Proxies in front of main (Cloudflare, for
// one) cut idle streams routinely, and an execution that is still running on
// a healthy edge must survive its stream flapping.
const DefaultGraceWindow = 60 * time.Second

// Hub owns the per-server attachment state. The zero value is unusable —
// build one with New.
type Hub struct {
	Registry *executor.Registry
	Logger   *log.Logger
	// GraceWindow holds in-flight executions after a disconnect, waiting for
	// the edge to come back. Zero settles them immediately.
	GraceWindow time.Duration

	mu      sync.Mutex
	servers map[string]*serverState
}

func New(reg *executor.Registry, logger *log.Logger) *Hub {
	return &Hub{
		Registry:    reg,
		Logger:      logger,
		GraceWindow: DefaultGraceWindow,
		servers:     map[string]*serverState{},
	}
}

// serverState is everything the hub knows about one server. It outlives
// individual attachments: the executor identity must stay stable across
// reconnects, and in-flight executions survive a stream flapping.
type serverState struct {
	ex       *serverExecutor
	current  *Attachment
	conn     Conn
	info     model.EdgeInfo
	inflight map[string]*inflight
	// graceGen invalidates outstanding grace timers: a reconnect bumps it so
	// a timer firing right as the edge returns cannot kill adopted work.
	graceGen int
}

// settleAll delivers the same result to every execution the server holds.
// Callers must hold h.mu.
func (st *serverState) settleAll(r execResult) int {
	for _, infl := range st.inflight {
		infl.settle(r)
	}
	return len(st.inflight)
}

// Attachment is one edge session's handle on the hub. Adapters must Detach
// it (deferred) when their stream ends.
type Attachment struct {
	hub    *Hub
	server string
}

type execResult struct {
	ok         bool
	errMsg     string
	digest     string
	sessionErr error // stream-level failure (disconnect)
}

type inflight struct {
	sink engine.Sink
	done chan execResult
}

// settle delivers a terminal result; the first one wins and later ones are
// dropped, which makes repeated Done reports idempotent.
func (i *inflight) settle(r execResult) {
	select {
	case i.done <- r:
	default:
	}
}

func (h *Hub) logf(format string, args ...any) {
	if h.Logger != nil {
		h.Logger.Printf(format, args...)
	}
}

// Attach makes an edge the current owner of its server: any previous
// connection is closed (last writer wins) and the server's executor becomes
// acquirable.
func (h *Hub) Attach(info AttachInfo, conn Conn) *Attachment {
	att := &Attachment{hub: h, server: info.Server}
	h.mu.Lock()
	st := h.servers[info.Server]
	if st == nil {
		st = &serverState{inflight: map[string]*inflight{}}
		st.ex = &serverExecutor{hub: h, server: info.Server}
		h.servers[info.Server] = st
	}
	st.graceGen++ // any pending grace timer is now stale
	adopted, orphans := st.reconcile(info)
	old := st.conn
	st.current = att
	st.conn = conn
	st.info = model.EdgeInfo{
		Server:      info.Server,
		Version:     info.Version,
		Transport:   info.Transport,
		ConnectedAt: time.Now(),
	}
	ex := st.ex
	h.mu.Unlock()

	if old != nil {
		// Outside the lock: the old adapter's Detach will want h.mu.
		old.Close()
	}
	h.Registry.Register(info.Server, ex)
	h.logf("edge %q connected (version %s, transport %s)", info.Server, info.Version, info.Transport)
	if adopted > 0 {
		h.logf("edge %q resumed %d in-flight execution(s)", info.Server, adopted)
	}
	if orphans > 0 {
		h.logf("edge %q came back without %d execution(s); failing them now", info.Server, orphans)
	}
	return att
}

// reconcile settles the executions the hub still holds but the returning
// edge no longer claims: nothing will ever report them, so failing now beats
// waiting out the grace window. Callers must hold h.mu, and must do this
// before the new connection is installed.
func (st *serverState) reconcile(info AttachInfo) (adopted, orphans int) {
	if len(st.inflight) == 0 {
		return 0, 0
	}
	claimed := make(map[string]struct{}, len(info.Inflight))
	for _, id := range info.Inflight {
		claimed[id] = struct{}{}
	}
	for id, infl := range st.inflight {
		if _, ok := claimed[id]; ok {
			adopted++
			continue
		}
		infl.settle(execResult{sessionErr: fmt.Errorf(
			"edge %q reconnected without execution %s", info.Server, id)})
		orphans++
	}
	return adopted, orphans
}

// Detach ends this attachment. It is a no-op once a newer attachment has
// taken the server over — the identity guard lives here because the
// executor value is shared across attachments and cannot discriminate.
func (a *Attachment) Detach() {
	h := a.hub
	h.mu.Lock()
	st := h.servers[a.server]
	if st == nil || st.current != a {
		h.mu.Unlock()
		return
	}
	st.current = nil
	st.conn = nil
	ex := st.ex
	held := len(st.inflight)
	grace := h.GraceWindow
	if held > 0 && grace > 0 {
		st.graceGen++
		gen := st.graceGen
		time.AfterFunc(grace, func() { h.expireGrace(a.server, gen) })
	} else {
		st.settleAll(execResult{sessionErr: fmt.Errorf("edge %q disconnected", a.server)})
	}
	h.mu.Unlock()

	// Unregistering immediately is what keeps new work off a dead edge: the
	// grace window only protects executions already under way.
	h.Registry.Unregister(a.server, ex)
	if held > 0 && grace > 0 {
		h.logf("edge %q disconnected, holding %d execution(s) for up to %s", a.server, held, grace)
	} else {
		h.logf("edge %q disconnected", a.server)
	}
}

// expireGrace gives up on a server that never came back. gen guards against
// a timer that fires just as the edge reconnects.
func (h *Hub) expireGrace(server string, gen int) {
	h.mu.Lock()
	st := h.servers[server]
	if st == nil || st.graceGen != gen || st.conn != nil {
		h.mu.Unlock()
		return
	}
	err := fmt.Errorf("%w: %s (did not reconnect within %s)", executor.ErrUnreachable, server, h.GraceWindow)
	n := st.settleAll(execResult{sessionErr: err})
	h.mu.Unlock()
	if n > 0 {
		h.logf("edge %q did not reconnect within %s: %d execution(s) unreachable", server, h.GraceWindow, n)
	}
}

// Edges snapshots the currently attached edges.
func (h *Hub) Edges() map[string]model.EdgeInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]model.EdgeInfo, len(h.servers))
	for name, st := range h.servers {
		if st.conn != nil {
			out[name] = st.info
		}
	}
	return out
}

// lookup finds an in-flight execution. Inbound events are keyed by server
// rather than by attachment: on a resumable transport the result of an
// execution may arrive over a different connection than the one that
// started it.
func (h *Hub) lookup(server, execID string) *inflight {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.servers[server]
	if st == nil {
		return nil
	}
	return st.inflight[execID]
}

// HandleOpStart/HandleOpEnd/HandleLog forward progress to the execution's
// sink. Events for unknown executions (already finished or timed out on
// main) are dropped.

func (h *Hub) HandleOpStart(server, execID string, index int, name string) {
	if infl := h.lookup(server, execID); infl != nil {
		infl.sink.OpStart(index, name)
	}
}

func (h *Hub) HandleOpEnd(server, execID string, index int, name string, exitCode *int, err error) {
	if infl := h.lookup(server, execID); infl != nil {
		infl.sink.OpEnd(index, name, exitCode, err)
	}
}

func (h *Hub) HandleLog(server, execID string, index int, stream, data string) {
	if infl := h.lookup(server, execID); infl != nil {
		infl.sink.Log(index, stream, data)
	}
}

// HandleDone settles an execution. It accepts unconditionally — including
// for unknown ids — so the edge can always clear its side once the hub has
// seen the report.
func (h *Hub) HandleDone(server, execID string, ok bool, errMsg, digest string) {
	if infl := h.lookup(server, execID); infl != nil {
		infl.settle(execResult{ok: ok, errMsg: errMsg, digest: digest})
	}
}

// serverExecutor is a server's stable Executor value: it resolves the
// current connection at dispatch time, so reconnects are invisible to the
// scheduler.
type serverExecutor struct {
	hub    *Hub
	server string
}

// Execute ships the spec to the attached edge and blocks until its terminal
// report, the edge disconnecting, or ctx expiry (the scheduler's timeout
// backstop — the edge enforces the same timeout locally).
func (e *serverExecutor) Execute(ctx context.Context, spec engine.Spec, sink engine.Sink) (engine.Result, error) {
	h := e.hub
	res := engine.Result{Digest: spec.Digest}
	infl := &inflight{sink: sink, done: make(chan execResult, 1)}

	h.mu.Lock()
	st := h.servers[e.server]
	if st == nil || st.conn == nil {
		h.mu.Unlock()
		return res, fmt.Errorf("%w: %s (edge not attached)", executor.ErrUnreachable, e.server)
	}
	st.inflight[spec.ExecutionID] = infl
	conn := st.conn
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		if st := h.servers[e.server]; st != nil {
			delete(st.inflight, spec.ExecutionID)
		}
		h.mu.Unlock()
	}()

	// Sending outside the lock: a racing Detach turns this into a stream
	// error, which fails the execution the same way a dead edge would.
	if err := conn.SendExecution(spec); err != nil {
		return res, fmt.Errorf("send execution to edge: %w", err)
	}

	select {
	case r := <-infl.done:
		if r.digest != "" {
			res.Digest = r.digest
		}
		if r.sessionErr != nil {
			return res, r.sessionErr
		}
		if !r.ok {
			if r.errMsg == "" {
				r.errMsg = "execution failed on edge"
			}
			return res, errors.New(r.errMsg)
		}
		return res, nil
	case <-ctx.Done():
		return res, ctx.Err()
	}
}
