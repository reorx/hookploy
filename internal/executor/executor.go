// Package executor abstracts "something that can run an execution on a
// server": M1 has only the built-in local executor; M2 adds gRPC edges.
package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/engine"
)

// Executor runs one execution spec to completion.
type Executor interface {
	Execute(ctx context.Context, spec engine.Spec, sink engine.Sink) (engine.Result, error)
}

// Local wraps the op engine as the executor of the main host.
type Local struct {
	Engine *engine.Engine
}

func (l *Local) Execute(ctx context.Context, spec engine.Spec, sink engine.Sink) (engine.Result, error) {
	return l.Engine.Execute(ctx, spec, sink)
}

// ErrUnreachable means no executor for the server appeared within the
// acquire window.
var ErrUnreachable = errors.New("server unreachable")

// Registry maps server names to their executors. Acquire waits up to Window
// for an executor to appear (edge reconnect grace); in M1 only local
// executors are ever registered, so remote servers time out to unreachable.
type Registry struct {
	Window time.Duration

	mu      sync.Mutex
	current map[string]Executor
	arrived chan struct{} // closed & replaced on every Register
}

func NewRegistry(window time.Duration) *Registry {
	return &Registry{
		Window:  window,
		current: map[string]Executor{},
		arrived: make(chan struct{}),
	}
}

// Register makes an executor available for a server and wakes waiters.
func (r *Registry) Register(server string, ex Executor) {
	r.mu.Lock()
	r.current[server] = ex
	close(r.arrived)
	r.arrived = make(chan struct{})
	r.mu.Unlock()
}

// Unregister removes a server's executor if it is still ex (edge
// disconnect). A stale session closing must not knock out its replacement.
func (r *Registry) Unregister(server string, ex Executor) {
	r.mu.Lock()
	if r.current[server] == ex {
		delete(r.current, server)
	}
	r.mu.Unlock()
}

// Acquire returns the server's executor, waiting up to Window for it to
// register. Returns ErrUnreachable when the window elapses.
func (r *Registry) Acquire(ctx context.Context, server string) (Executor, error) {
	deadline := time.NewTimer(r.Window)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		ex, ok := r.current[server]
		arrived := r.arrived
		r.mu.Unlock()
		if ok {
			return ex, nil
		}
		select {
		case <-arrived:
		case <-deadline.C:
			return nil, fmt.Errorf("%w: %s (no executor within %s)", ErrUnreachable, server, r.Window)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
