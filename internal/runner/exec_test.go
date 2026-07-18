package runner

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Behavior: commands run with argv semantics (no shell) and stream output.
func TestExecRunnerBasics(t *testing.T) {
	r := &ExecRunner{}
	var out bytes.Buffer
	code, err := r.Run(context.Background(), Cmd{
		Argv:   []string{"echo", "hello; rm -rf /"},
		Stdout: &out,
	})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if out.String() != "hello; rm -rf /\n" {
		t.Fatalf("shell metacharacters must be inert: %q", out.String())
	}

	code, err = r.Run(context.Background(), Cmd{Argv: []string{"false"}})
	if err != nil || code == 0 {
		t.Fatalf("non-zero exit should be (code, nil): code=%d err=%v", code, err)
	}
}

// Behavior: on context cancellation the process group is killed promptly.
func TestExecRunnerKillsProcessGroupOnTimeout(t *testing.T) {
	r := &ExecRunner{KillGrace: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, Cmd{Argv: []string{"sleep", "30"}})
	if err == nil {
		t.Fatal("expected a context error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("kill took too long: %v", elapsed)
	}
}
