// Package edgewatch reports edges that stop answering. It samples the gap
// between the servers hookploy.yaml declares and the edges actually attached
// to the hub, and calls back when one has been missing longer than the
// configured threshold — and again when it returns.
//
// Polling rather than hooking edgehub's disconnect is what makes the case
// that matters work: after main restarts, an edge that never comes back
// never disconnects either, so there is nothing to hook. Sampling both sides
// sees it, and leaves edgehub — whose job is dispatch, not monitoring —
// untouched.
//
// The callbacks are bare functions for the same reason scheduler.Notify is:
// this package must not know who is listening. main wires them to the
// notifier.
package edgewatch

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/config"
	"github.com/reorx/hookploy/internal/model"
)

// DefaultInterval is how often the fleet is sampled. It bounds how late a
// report can be, so it wants to stay well under any sensible threshold
// without polling for its own sake.
const DefaultInterval = 30 * time.Second

// Outage is one server's absence, as of the moment it was reported.
type Outage struct {
	Server string
	// Version is what the edge last handshaked with, empty if it has not
	// attached at all since this process started.
	Version string
	// Duration is how long the server has been missing — since it was last
	// seen attached, or since the watcher first looked, whichever is later.
	Duration time.Duration
}

// Watcher polls the fleet. Build one with a struct literal, then Start.
type Watcher struct {
	Config func() *config.Config
	Edges  func() map[string]model.EdgeInfo
	Logger *log.Logger

	// Interval is how often to sample. Zero means DefaultInterval; tests
	// shrink it.
	Interval time.Duration

	// OnOffline fires once per outage, when it first passes the threshold.
	// OnOnline fires when a server that had an announced outage comes back —
	// an absence nobody was told about gets no all-clear either.
	OnOffline func(Outage)
	OnOnline  func(Outage)

	// state is touched only by the polling goroutine (or, in tests, by
	// whoever drives tick), so it needs no lock.
	state map[string]*serverState

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type serverState struct {
	lastSeen time.Time
	version  string
	alerted  bool
}

// outage measures an ended absence. The edge came back when it attached, not
// when this poll happened to notice — timing it to now would pad every
// all-clear with up to a full interval. The attach time is only trusted when
// it falls inside the window we actually observed; anything else (a zero
// value, a clock that moved) falls back to what the poll itself knows.
func (st *serverState) outage(info model.EdgeInfo, now time.Time) time.Duration {
	back := info.ConnectedAt
	if back.Before(st.lastSeen) || back.After(now) {
		back = now
	}
	return back.Sub(st.lastSeen)
}

// Start begins polling in the background.
func (w *Watcher) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run(ctx)
	}()
}

// Shutdown stops polling and waits for the goroutine to leave, so no report
// can still be in flight when the caller moves on to draining the notifier.
func (w *Watcher) Shutdown() {
	if w.cancel == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
}

func (w *Watcher) run(ctx context.Context) {
	// Sample once up front: this is the baseline every server's clock starts
	// from, and it makes Start's effect immediate rather than one tick away.
	w.tick()
	t := time.NewTicker(w.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick()
		}
	}
}

// tick compares the declared fleet against the attached one and reports what
// changed. Config is read here rather than cached, so a reload — a lowered
// threshold, a server added or removed — lands on the very next sample.
func (w *Watcher) tick() {
	if w.state == nil {
		w.state = map[string]*serverState{}
	}
	cfg := w.Config()
	attached := w.Edges()
	now := time.Now()
	threshold := cfg.Notify.EdgeOfflineAfter

	for name, srv := range cfg.Servers {
		if srv.Local {
			continue // main runs these in-process; there is no edge to lose
		}
		st := w.state[name]
		if st == nil {
			// First sight of this server: main just started, or a reload just
			// declared it. Its clock starts now — we cannot speak for time we
			// were not watching, and an empty version says as much.
			st = &serverState{lastSeen: now}
			w.state[name] = st
		}
		if info, up := attached[name]; up {
			if st.alerted {
				st.alerted = false
				w.report(w.OnOnline, "is back after %s offline",
					Outage{Server: name, Version: info.Version, Duration: st.outage(info, now)})
			}
			st.lastSeen, st.version = now, info.Version
			continue
		}
		// A non-positive threshold disables reporting rather than firing on
		// everything at once; config never produces one.
		if st.alerted || threshold <= 0 || now.Sub(st.lastSeen) < threshold {
			continue
		}
		st.alerted = true
		w.report(w.OnOffline, "has been offline for %s",
			Outage{Server: name, Version: st.version, Duration: now.Sub(st.lastSeen)})
	}

	// Forget servers the config no longer declares, so one that comes back
	// starts from a fresh clock instead of firing on time it was not even
	// configured for.
	for name := range w.state {
		if srv := cfg.Servers[name]; srv == nil || srv.Local {
			delete(w.state, name)
		}
	}
}

func (w *Watcher) report(fn func(Outage), format string, o Outage) {
	if w.Logger != nil {
		w.Logger.Printf("edgewatch: %q "+format, o.Server, o.Duration.Round(time.Second))
	}
	if fn != nil {
		fn(o)
	}
}

func (w *Watcher) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return DefaultInterval
}
