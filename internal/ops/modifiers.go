package ops

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/reorx/hookploy/internal/model"
	"gopkg.in/yaml.v3"
)

// Step modifiers are the reserved keys that sit next to the op name in a step
// map. None of them is an op name, so they can never collide with the
// vocabulary.
const (
	// OnKey restricts an op to some instances (config-only, resolved at
	// enqueue time).
	OnKey = "on"
	// TimeoutKey bounds each attempt of a step.
	TimeoutKey = "timeout"
	// RetriesKey re-runs a failed step, for the ops in retryable.
	RetriesKey = "retries"
)

var modifierKeys = map[string]bool{OnKey: true, TimeoutKey: true, RetriesKey: true}

// modifierList renders the modifier keys for error messages.
const modifierList = `"on", "timeout", "retries"`

// retryable lists the ops `retries:` may be set on — the ones that are safe to
// run again from scratch — with the number of retries each gets when the step
// says nothing. image.pin and artifact.extract default to 2: they used to
// carry their own three-try loops, which step retries replaced.
var retryable = map[string]int{
	"image.pin":        2,
	"artifact.extract": 2,
	"compose.pull":     0,
}

// retryHints explains, per op, why it is not retryable when that is not
// obvious from the op itself.
var retryHints = map[string]string{
	"healthcheck": "it already polls on its own; raise its attempts instead",
}

// RetryInterval is the pause between two attempts of a step.
const RetryInterval = 5 * time.Second

// ModifiersSince is the first release whose engine honors step timeout and
// retries (and healthcheck's attempts). Older engines decode a snapshot that
// uses them without complaint and silently run it the old way, so main must
// not hand such a snapshot to an older edge.
const ModifiersSince = "v0.7.0"

// Retryable reports whether `retries:` may be set on op.
func Retryable(op string) bool {
	_, ok := retryable[op]
	return ok
}

// Attempts is how many times the step may run in total.
func (s Step) Attempts() int {
	if s.Retries != nil {
		return *s.Retries + 1
	}
	return retryable[s.Op] + 1
}

// NeedsModifierSupport returns the index of the first step an engine older
// than ModifiersSince would run differently from what the snapshot says: one
// with a timeout or retries, or a healthcheck whose attempts differ from the
// default an old engine falls back to (it looks for the pre-rename "retries"
// key and finds nothing).
func NeedsModifierSupport(steps []Step) (int, bool) {
	for i, s := range steps {
		if s.Timeout > 0 || s.Retries != nil {
			return i, true
		}
		if hc, ok := s.Args.(*Healthcheck); ok && hc.Attempts != defaultHealthcheckAttempts {
			return i, true
		}
	}
	return -1, false
}

// parseTimeout reads the `timeout:` modifier.
func parseTimeout(node *yaml.Node) (time.Duration, error) {
	if node == nil {
		return 0, nil
	}
	var d model.Duration
	if err := node.Decode(&d); err != nil {
		return 0, fmt.Errorf("line %d: %q must be a duration like 3m or 90s: %w", node.Line, TimeoutKey, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("line %d: %q must be positive", node.Line, TimeoutKey)
	}
	return time.Duration(d), nil
}

// parseRetries reads the `retries:` modifier of a step running op.
func parseRetries(node *yaml.Node, op string) (*int, error) {
	if node == nil {
		return nil, nil
	}
	if !Retryable(op) {
		reason := fmt.Sprintf("%q only applies to ops that are safe to run again from scratch (%s)",
			RetriesKey, retryableList())
		if hint, ok := retryHints[op]; ok {
			reason = hint
		}
		return nil, fmt.Errorf("line %d: %s cannot be retried: %s", node.Line, op, reason)
	}
	var n int
	if err := node.Decode(&n); err != nil || n < 0 {
		return nil, fmt.Errorf("line %d: %q must be a non-negative integer", node.Line, RetriesKey)
	}
	return &n, nil
}

func retryableList() string {
	names := make([]string, 0, len(retryable))
	for name := range retryable {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
