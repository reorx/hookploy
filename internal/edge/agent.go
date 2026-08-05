package edge

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/ops"
)

const (
	// defaultPendingCap bounds the unacknowledged-result buffer. Results are
	// tiny, but a main that stays away forever must not grow this without
	// limit; the oldest go first, since the newest are the likeliest to still
	// matter to a deploy someone is watching.
	defaultPendingCap = 128
	// defaultDoneRetry re-tries buffered results even while a session stays
	// up, covering a report that failed for its own reasons.
	defaultDoneRetry = 5 * time.Second
)

// pendingDone is a result main has not acknowledged yet.
type pendingDone struct {
	execID   string
	report   DoneReport
	attempts int
}

// agent is the transport-independent edge core: it keeps one session alive
// with exponential-backoff reconnects, runs whatever main sends through the
// local op engine, and makes sure every result eventually gets through.
type agent struct {
	eng    *engine.Engine
	logger *log.Logger

	backoffBase time.Duration
	backoffMax  time.Duration
	pendingCap  int
	doneRetry   time.Duration

	wake chan struct{} // nudges the result pump

	mu      sync.Mutex
	running map[string]context.CancelFunc
	pending []pendingDone
	current Session // the session results should go out on; nil while offline
}

func newAgent(eng *engine.Engine, logger *log.Logger, base, max time.Duration) *agent {
	return &agent{
		eng:         eng,
		logger:      logger,
		backoffBase: base,
		backoffMax:  max,
		pendingCap:  defaultPendingCap,
		doneRetry:   defaultDoneRetry,
		wake:        make(chan struct{}, 1),
		running:     map[string]context.CancelFunc{},
	}
}

// run reconnects forever until ctx is canceled.
func (a *agent) run(ctx context.Context, tr Transport) {
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		a.pumpResults(ctx)
	}()
	defer func() { <-pumpDone }()

	backoff := a.backoffBase
	for {
		if a.runSession(ctx, tr) {
			backoff = a.backoffBase // the session was established; start over gently
		}
		if ctx.Err() != nil {
			return
		}
		a.logger.Printf("reconnecting in %s", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > a.backoffMax {
			backoff = a.backoffMax
		}
	}
}

// runSession runs one connect→handshake→serve cycle. Returns whether the
// session got past the handshake (for backoff reset).
func (a *agent) runSession(ctx context.Context, tr Transport) bool {
	// sessCtx tears the stream down when this cycle ends.
	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sess, err := tr.Dial(sessCtx, a.inflightIDs())
	if err != nil {
		if ctx.Err() == nil {
			a.logger.Printf("connect: %v", err)
		}
		return false
	}
	defer sess.Close()
	a.logger.Printf("connected to main %s as server %q", sess.MainVersion(), sess.Server())
	a.setSession(sess)
	defer a.clearSession(sess)

	// Where an execution's lifetime ends. A resumable session lets the work
	// outlive its stream — main holds the execution open and takes the result
	// over the next connection. Without that, main fails the execution as
	// soon as the stream dies, so keeping it running here would only let the
	// two sides diverge.
	execCtx := sessCtx
	if sess.Resumable() {
		execCtx = ctx
	}

	for {
		task, err := sess.Recv()
		if err != nil {
			if ctx.Err() == nil {
				a.logger.Printf("session lost: %v", err)
			}
			return true
		}
		a.start(execCtx, sess, task)
	}
}

func (a *agent) setSession(sess Session) {
	a.mu.Lock()
	a.current = sess
	a.mu.Unlock()
	a.kick() // a fresh session is the moment to retry buffered results
}

func (a *agent) clearSession(sess Session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.current == sess {
		a.current = nil
	}
}

func (a *agent) kick() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// ── executions ─────────────────────────────────────────────────────────────

// inflightIDs is what the next Dial advertises as still live here: work in
// progress plus results main has not taken yet.
func (a *agent) inflightIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := make(map[string]struct{}, len(a.running)+len(a.pending))
	out := make([]string, 0, len(a.running)+len(a.pending))
	for id := range a.running {
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, p := range a.pending {
		if _, dup := seen[p.execID]; dup {
			continue
		}
		out = append(out, p.execID)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// start launches one task, ignoring a redelivery of something already
// running (defensive: main dispatches each execution once).
func (a *agent) start(ctx context.Context, sess Session, task *Task) {
	execCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if _, dup := a.running[task.ExecutionID]; dup {
		a.mu.Unlock()
		cancel()
		a.logger.Printf("execution %s already running, ignoring redelivery", task.ExecutionID)
		return
	}
	a.running[task.ExecutionID] = cancel
	a.mu.Unlock()
	go func() {
		defer cancel()
		defer a.finish(task.ExecutionID)
		a.runTask(execCtx, sess, task)
	}()
}

func (a *agent) finish(execID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.running, execID)
}

func (a *agent) runningCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.running)
}

// runTask executes one task and reports its terminal state. The timeout is
// enforced here: the engine's runner kills the process group when the
// deadline passes.
func (a *agent) runTask(ctx context.Context, sess Session, task *Task) {
	report := func(ok bool, errMsg, digest string) {
		a.reportDone(sess, task.ExecutionID, DoneReport{OK: ok, Error: errMsg, Digest: digest})
	}
	var steps []ops.Step
	if err := json.Unmarshal(task.OpsJSON, &steps); err != nil {
		report(false, "edge cannot decode ops (version mismatch?): "+err.Error(), "")
		return
	}
	a.logger.Printf("execution %s: %s/%s starting (%d ops)", task.ExecutionID, task.Service, task.Instance, len(steps))
	if task.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, task.Timeout)
		defer cancel()
	}
	res, err := a.eng.Execute(ctx, engine.Spec{
		ExecutionID: task.ExecutionID,
		Kind:        task.Kind,
		Service:     task.Service,
		Instance:    task.Instance,
		Dir:         task.Dir,
		Image:       task.Image,
		Digest:      task.Digest,
		Steps:       steps,
	}, &sessionSink{sess: sess, execID: task.ExecutionID})
	if err != nil {
		a.logger.Printf("execution %s failed: %v", task.ExecutionID, err)
		report(false, err.Error(), res.Digest)
		return
	}
	a.logger.Printf("execution %s succeeded", task.ExecutionID)
	report(true, "", res.Digest)
}

// ── result delivery ────────────────────────────────────────────────────────

// reportDone hands a result off for delivery. On a resumable transport it
// goes through the buffer, so a result produced while the stream is down is
// still delivered later. Without resumption there is no acknowledgement to
// wait for and no session that could ever adopt the execution, so the report
// is a single best-effort send on the session that ran it.
func (a *agent) reportDone(sess Session, execID string, d DoneReport) {
	if sess.Resumable() {
		a.bufferDone(execID, d)
		a.kick()
		return
	}
	if err := sess.ReportDone(execID, d); err != nil {
		a.logger.Printf("execution %s: could not report result (%v)", execID, err)
	}
}

func (a *agent) bufferDone(execID string, d DoneReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, p := range a.pending {
		if p.execID == execID {
			a.pending[i].report = d
			return
		}
	}
	if len(a.pending) >= a.pendingCap {
		dropped := a.pending[0]
		a.pending = a.pending[1:]
		a.logger.Printf("result buffer full (%d): dropping the result of execution %s",
			a.pendingCap, dropped.execID)
	}
	a.pending = append(a.pending, pendingDone{execID: execID, report: d})
}

func (a *agent) pendingCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

func (a *agent) pendingIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.pending))
	for _, p := range a.pending {
		out = append(out, p.execID)
	}
	return out
}

// pumpResults is the single writer of buffered results: it drains them
// whenever a session is up, and retries on a timer for the rest.
func (a *agent) pumpResults(ctx context.Context) {
	t := time.NewTicker(a.doneRetry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-t.C:
		}
		a.flushResults()
	}
}

// flushResults delivers buffered results in order, stopping at the first one
// main does not take — order matters less than not spinning on a dead link.
func (a *agent) flushResults() {
	for {
		a.mu.Lock()
		if len(a.pending) == 0 || a.current == nil {
			a.mu.Unlock()
			return
		}
		a.pending[0].attempts++
		p := a.pending[0]
		sess := a.current
		a.mu.Unlock()

		if err := sess.ReportDone(p.execID, p.report); err != nil {
			a.logger.Printf("execution %s: result not acknowledged (%v), keeping it for retry", p.execID, err)
			return
		}
		a.ackDone(p.execID)
		if p.attempts > 1 {
			a.logger.Printf("execution %s: result delivered on attempt %d", p.execID, p.attempts)
		}
	}
}

func (a *agent) ackDone(execID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, p := range a.pending {
		if p.execID == execID {
			a.pending = append(a.pending[:i], a.pending[i+1:]...)
			return
		}
	}
}

// sessionSink forwards engine progress over the session. Progress is
// best-effort: a broken stream loses it, and only the final result is worth
// buffering.
type sessionSink struct {
	sess   Session
	execID string
}

func (w *sessionSink) OpStart(i int, name string) {
	w.sess.ReportUpdate(w.execID, Update{Kind: UpdateOpStart, Index: i, Name: name})
}

func (w *sessionSink) OpEnd(i int, name string, exit *int, err error) {
	u := Update{Kind: UpdateOpEnd, Index: i, Name: name, ExitCode: exit}
	if err != nil {
		u.Error = err.Error()
	}
	w.sess.ReportUpdate(w.execID, u)
}

func (w *sessionSink) Log(i int, stream, data string) {
	w.sess.ReportUpdate(w.execID, Update{Kind: UpdateLog, Index: i, Stream: stream, Data: data})
}
