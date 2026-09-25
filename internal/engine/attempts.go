package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/reorx/hookploy/internal/ops"
)

// permanentError marks a failure another attempt cannot fix — bad input, or
// a download that arrived intact with the wrong content — so the retry loop
// gives up at once.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return &permanentError{err: err} }

// runAttempts runs one step up to step.Attempts() times, each attempt a fresh
// run bounded by the step's timeout, pausing ops.RetryInterval in between.
// The execution context stays the backstop: once it is done no further
// attempt starts. A single-attempt step fails with its plain error, so a
// pipeline without modifiers reports exactly what it always did.
func (e *Engine) runAttempts(ctx context.Context, spec Spec, idx int, step ops.Step, st *execState, sink Sink) (*int, error) {
	total := step.Attempts()
	var reasons []string
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			sink.Log(idx, "system", fmt.Sprintf("%s attempt %d/%d\n", step.Op, attempt, total))
		}
		exit, err := e.runAttempt(ctx, spec, idx, step, st, sink)
		if err == nil {
			return exit, nil
		}
		reasons = append(reasons, fmt.Sprintf("[%d/%d] %v", attempt, total, err))
		var perm *permanentError
		if attempt == total || ctx.Err() != nil || errors.As(err, &perm) {
			if attempt == 1 {
				return exit, err
			}
			return exit, &attemptsError{last: err, summary: fmt.Sprintf(
				"failed after %d attempts: %s", attempt, strings.Join(reasons, "; "))}
		}
		sink.Log(idx, "system", fmt.Sprintf("%s attempt %d/%d failed: %v; retrying in %s\n",
			step.Op, attempt, total, err, ops.RetryInterval))
		if serr := e.sleep(ctx, ops.RetryInterval); serr != nil {
			return exit, &attemptsError{last: serr, summary: fmt.Sprintf(
				"%v while waiting to retry: %s", serr, strings.Join(reasons, "; "))}
		}
	}
}

// runAttempt runs the step once, under its own timeout when it has one. A
// step timeout is reported as such rather than as whatever the killed
// command happened to return.
func (e *Engine) runAttempt(ctx context.Context, spec Spec, idx int, step ops.Step, st *execState, sink Sink) (*int, error) {
	if step.Timeout <= 0 {
		return e.runStep(ctx, spec, idx, step, st, sink)
	}
	actx, cancel := context.WithTimeout(ctx, step.Timeout)
	defer cancel()
	exit, err := e.runStep(actx, spec, idx, step, st, sink)
	if err != nil && ctx.Err() == nil && errors.Is(actx.Err(), context.DeadlineExceeded) {
		return exit, fmt.Errorf("timed out after %s", step.Timeout)
	}
	return exit, err
}

// attemptsError is the failure of a step that ran more than once: its
// message lists every attempt, and it unwraps to the last attempt's error so
// callers can still tell, say, an execution deadline apart.
type attemptsError struct {
	last    error
	summary string
}

func (e *attemptsError) Error() string { return e.summary }
func (e *attemptsError) Unwrap() error { return e.last }
