// Package scheduler drives deploys: strict per-service serialization,
// latest-wins dedup, wave gating with rollout-level digest promotion,
// timeouts, and restart recovery. It only ever reads execution snapshots —
// config hot reloads never affect in-flight work.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/executor"
	"github.com/reorx/hookploy/internal/model"
	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/store"
)

// RetainPerService is how many deploy records each service keeps.
const RetainPerService = 50

type svcState struct {
	busy    bool
	pending string // deploy id waiting; older pendings get superseded
}

type Scheduler struct {
	store *store.Store
	reg   *executor.Registry

	mu       sync.Mutex
	services map[string]*svcState

	root   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(st *store.Store, reg *executor.Registry) *Scheduler {
	root, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		store:    st,
		reg:      reg,
		services: map[string]*svcState{},
		root:     root,
		cancel:   cancel,
	}
}

// Enqueue schedules a queued deploy. If the service is busy the deploy
// waits as the single pending slot; an already-pending deploy is superseded
// (latest wins).
func (s *Scheduler) Enqueue(service, deployID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.services[service]
	if st == nil {
		st = &svcState{}
		s.services[service] = st
	}
	if st.busy {
		if st.pending != "" {
			_ = s.store.MarkDeploySuperseded(st.pending)
		}
		st.pending = deployID
		return
	}
	st.busy = true
	s.wg.Add(1)
	go s.loop(service, deployID)
}

// Shutdown cancels in-flight work and waits for loops to exit.
func (s *Scheduler) Shutdown() {
	s.cancel()
	s.wg.Wait()
}

func (s *Scheduler) loop(service, deployID string) {
	defer s.wg.Done()
	for {
		s.runDeploy(deployID)
		s.mu.Lock()
		st := s.services[service]
		if st.pending != "" && s.root.Err() == nil {
			deployID = st.pending
			st.pending = ""
			s.mu.Unlock()
			continue
		}
		st.busy = false
		s.mu.Unlock()
		return
	}
}

func (s *Scheduler) runDeploy(deployID string) {
	d, err := s.store.GetDeploy(deployID)
	if err != nil || d == nil || d.Status != model.StatusQueued {
		return
	}
	execs, err := s.store.ListExecutions(deployID)
	if err != nil || len(execs) == 0 {
		return
	}
	waves := groupWaves(execs)
	digest := d.Digest
	failed := false

	for _, wave := range waves {
		if failed || s.root.Err() != nil {
			for _, ex := range wave {
				_, _ = s.store.TransitionExecution(ex.ID, model.StatusQueued, model.StatusCanceled, "earlier wave failed")
			}
			continue
		}
		rest := wave
		// Rollout-level digest resolution: with no payload digest, the first
		// instance of wave 1 resolves :latest and its digest is promoted to
		// the whole rollout, so every node pins the same image.
		if digest == "" {
			first := wave[0]
			resolved, ok := s.runExecution(first, string(d.Kind), digest)
			if resolved != "" && resolved != digest {
				digest = resolved
				_ = s.store.SetDeployDigest(deployID, digest)
			}
			if !ok {
				failed = true
			}
			rest = wave[1:]
		}
		if !failed && len(rest) > 0 {
			var wg sync.WaitGroup
			var mu sync.Mutex
			for _, ex := range rest {
				wg.Add(1)
				go func(ex *model.Execution) {
					defer wg.Done()
					if _, ok := s.runExecution(ex, string(d.Kind), digest); !ok {
						mu.Lock()
						failed = true
						mu.Unlock()
					}
				}(ex)
			}
			wg.Wait()
		} else if failed {
			for _, ex := range rest {
				_, _ = s.store.TransitionExecution(ex.ID, model.StatusQueued, model.StatusCanceled, "earlier wave failed")
			}
		}
		_, _ = s.store.RecomputeDeployStatus(deployID)
	}
	_, _ = s.store.RecomputeDeployStatus(deployID)
	_ = s.store.CleanupService(d.Service, RetainPerService)
}

// transition moves an execution between states and immediately re-aggregates
// the deploy row, so deploy-level status is live (not just at wave ends).
func (s *Scheduler) transition(ex *model.Execution, from, to model.Status, errMsg string) (bool, error) {
	ok, err := s.store.TransitionExecution(ex.ID, from, to, errMsg)
	if ok && err == nil {
		_, _ = s.store.RecomputeDeployStatus(ex.DeployID)
	}
	return ok, err
}

// runExecution drives one execution through its lifecycle. Returns the
// digest the engine resolved (if any) and whether the execution succeeded.
func (s *Scheduler) runExecution(ex *model.Execution, kind, digest string) (string, bool) {
	ok, err := s.transition(ex, model.StatusQueued, model.StatusDispatching, "")
	if err != nil || !ok {
		return "", false
	}
	exec, err := s.reg.Acquire(s.root, ex.Server)
	if err != nil {
		to := model.StatusFailed
		if errors.Is(err, executor.ErrUnreachable) {
			to = model.StatusUnreachable
		}
		_, _ = s.transition(ex, model.StatusDispatching, to, err.Error())
		return "", false
	}
	ok, err = s.transition(ex, model.StatusDispatching, model.StatusRunning, "")
	if err != nil || !ok {
		return "", false
	}

	var steps []ops.Step
	if err := json.Unmarshal(ex.OpsJSON, &steps); err != nil {
		_, _ = s.transition(ex, model.StatusRunning, model.StatusFailed, "corrupt ops snapshot: "+err.Error())
		return "", false
	}
	ctx, cancel := context.WithTimeout(s.root, time.Duration(ex.Timeout))
	defer cancel()
	res, err := exec.Execute(ctx, engine.Spec{
		ExecutionID: ex.ID,
		Kind:        kind,
		Service:     ex.Service,
		Instance:    ex.Instance,
		Dir:         ex.Dir,
		Image:       ex.Image,
		Digest:      digest,
		Timeout:     time.Duration(ex.Timeout),
		Steps:       steps,
	}, &storeSink{store: s.store, execID: ex.ID})
	if err != nil {
		msg := err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = fmt.Sprintf("timeout after %s: %s", ex.Timeout, msg)
		}
		_, _ = s.transition(ex, model.StatusRunning, model.StatusFailed, msg)
		return res.Digest, false
	}
	_, _ = s.transition(ex, model.StatusRunning, model.StatusSucceeded, "")
	return res.Digest, true
}

// Recover restores scheduler state after a main restart: in-flight
// executions are failed (their executors died with the old process), and per
// service only the newest queued deploy is rescheduled.
func (s *Scheduler) Recover() error {
	ids, err := s.store.RecoverInFlight("main restarted")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.store.RecomputeDeployStatus(id); err != nil {
			return err
		}
	}
	queued, err := s.store.ListQueuedDeploys()
	if err != nil {
		return err
	}
	latest := map[string]*model.Deploy{} // oldest-first list → last write wins
	for _, d := range queued {
		if prev := latest[d.Service]; prev != nil {
			if err := s.store.MarkDeploySuperseded(prev.ID); err != nil {
				return err
			}
		}
		latest[d.Service] = d
	}
	for _, d := range latest {
		s.Enqueue(d.Service, d.ID)
	}
	return nil
}

func groupWaves(execs []*model.Execution) [][]*model.Execution {
	maxWave := 0
	for _, ex := range execs {
		if ex.Wave > maxWave {
			maxWave = ex.Wave
		}
	}
	waves := make([][]*model.Execution, maxWave+1)
	for _, ex := range execs {
		waves[ex.Wave] = append(waves[ex.Wave], ex)
	}
	var out [][]*model.Execution
	for _, w := range waves {
		if len(w) > 0 {
			out = append(out, w)
		}
	}
	return out
}

// storeSink persists engine progress into the store (and thereby streams to
// followers).
type storeSink struct {
	store  *store.Store
	execID string
}

func (s *storeSink) OpStart(i int, name string) {
	_ = s.store.StartOp(s.execID, i, name)
}

func (s *storeSink) OpEnd(i int, name string, exit *int, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	_ = s.store.FinishOp(s.execID, i, exit, msg)
}

func (s *storeSink) Log(i int, stream, data string) {
	_ = s.store.AppendLog(&model.LogLine{
		ExecutionID: s.execID,
		OpIndex:     i,
		Stream:      stream,
		Data:        data,
		At:          time.Now(),
	})
}
