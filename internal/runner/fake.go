package runner

import (
	"context"
	"io"
	"strings"
	"sync"
)

// Rule scripts the FakeRunner's reaction to commands whose argv starts with
// Match.
type Rule struct {
	Match  []string // argv prefix to match
	Dir    string   // when set, also require an exact Dir match
	Stdout string
	Stderr string
	Exit   int
	Err    error
	// Effect simulates side effects on the filesystem (e.g. `docker cp`
	// materializing a directory). It runs before output is written.
	Effect func(c Cmd) error
	// BlockUntilCancel makes the command hang until the context is
	// canceled (for timeout/kill behavior tests).
	BlockUntilCancel bool
	// Once limits the rule to its first match (for retry scripting).
	Once bool

	used bool
}

// FakeRunner records every call and replays scripted rules. The zero value
// answers everything with exit 0 and no output.
type FakeRunner struct {
	mu    sync.Mutex
	Rules []*Rule
	Calls []Cmd
}

// On appends a rule matching an argv prefix.
func (f *FakeRunner) On(argvPrefix ...string) *Rule {
	r := &Rule{Match: argvPrefix}
	f.mu.Lock()
	f.Rules = append(f.Rules, r)
	f.mu.Unlock()
	return r
}

// Returning configures the rule's output and exit code.
func (r *Rule) Returning(stdout string, exit int) *Rule {
	r.Stdout, r.Exit = stdout, exit
	return r
}

func (f *FakeRunner) Run(ctx context.Context, c Cmd) (int, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	var rule *Rule
	for _, r := range f.Rules {
		if r.used && r.Once {
			continue
		}
		if matches(r.Match, c.Argv) && (r.Dir == "" || r.Dir == c.Dir) {
			r.used = true
			rule = r
			break
		}
	}
	f.mu.Unlock()

	if rule == nil {
		return 0, nil
	}
	if rule.BlockUntilCancel {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	if rule.Effect != nil {
		if err := rule.Effect(c); err != nil {
			return -1, err
		}
	}
	if rule.Stdout != "" && c.Stdout != nil {
		io.WriteString(c.Stdout, rule.Stdout)
	}
	if rule.Stderr != "" && c.Stderr != nil {
		io.WriteString(c.Stderr, rule.Stderr)
	}
	return rule.Exit, rule.Err
}

// ArgvList returns the argv of every recorded call, for assertions.
func (f *FakeRunner) ArgvList() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.Argv
	}
	return out
}

// JoinedCalls renders each call as a space-joined string.
func (f *FakeRunner) JoinedCalls() []string {
	var out []string
	for _, argv := range f.ArgvList() {
		out = append(out, strings.Join(argv, " "))
	}
	return out
}

func matches(prefix, argv []string) bool {
	if len(prefix) > len(argv) {
		return false
	}
	for i, p := range prefix {
		if argv[i] != p {
			return false
		}
	}
	return true
}
