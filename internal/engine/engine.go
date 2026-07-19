// Package engine executes an interpolated op pipeline in a service
// directory. All side effects (processes, HTTP, sleeping) are injected so
// behavior tests run without docker or delays.
package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/runner"
)

// HTTPDoer is the injectable HTTP client (artifact download, healthcheck).
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Sink receives execution progress: per-op boundaries and log chunks.
// Streams are "stdout", "stderr" and "system" (runtime-internal messages
// such as the image.pin verification).
type Sink interface {
	OpStart(opIndex int, opName string)
	OpEnd(opIndex int, opName string, exitCode *int, err error)
	Log(opIndex int, stream, data string)
}

// Spec is one execution: the interpolated snapshot plus its target context.
type Spec struct {
	ExecutionID string
	Kind        string // deploy | task
	Service     string
	Instance    string
	Dir         string
	Image       string // service image: declaration, "" if none
	Digest      string // rollout-resolved digest; "" = resolve from :latest
	Timeout     time.Duration
	Steps       []ops.Step
}

// Result reports what the execution learned.
type Result struct {
	Digest string // digest actually pinned (promoted to the rollout on wave 1)
}

// Engine runs Specs. Zero values of Sleep/HTTP fall back to real
// implementations.
type Engine struct {
	Runner runner.Runner
	HTTP   HTTPDoer
	// Sleep is the retry/poll delay hook; tests inject a no-op.
	Sleep func(ctx context.Context, d time.Duration) error

	// PullRetries/PullInterval govern the image.pin digest pull.
	PullRetries  int
	PullInterval time.Duration
	// DownloadRetries governs artifact.extract.
	DownloadRetries int
}

type execState struct {
	pinnedImageID string
	digest        string
}

// Execute runs the pipeline, streaming into sink. It stops at the first
// failing op. The image.pin verification runs right after the last
// compose.up of the pipeline.
func (e *Engine) Execute(ctx context.Context, spec Spec, sink Sink) (Result, error) {
	res := Result{Digest: spec.Digest}
	st := &execState{}
	lastUp := -1
	for i, s := range spec.Steps {
		if s.Op == "compose.up" {
			lastUp = i
		}
	}
	for i, step := range spec.Steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		sink.OpStart(i, step.Op)
		exit, err := e.runStep(ctx, spec, i, step, st, sink)
		sink.OpEnd(i, step.Op, exit, err)
		if st.digest != "" {
			res.Digest = st.digest
		}
		if err != nil {
			return res, fmt.Errorf("op %d (%s): %w", i+1, step.Op, err)
		}
		if i == lastUp && st.pinnedImageID != "" {
			if err := e.verifyPin(ctx, spec, i, st, sink); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func (e *Engine) runStep(ctx context.Context, spec Spec, idx int, step ops.Step, st *execState, sink Sink) (*int, error) {
	switch a := step.Args.(type) {
	case *ops.ImagePin:
		return e.imagePin(ctx, spec, idx, st, sink)
	case *ops.ImageExtract:
		return e.imageExtract(ctx, spec, idx, a, sink)
	case *ops.ArtifactExtract:
		return nil, e.artifactExtract(ctx, spec, idx, a, sink)
	case *ops.ComposePull:
		return e.runCmd(ctx, spec, idx, sink, append([]string{"docker", "compose", "pull"}, a.Services...))
	case *ops.ComposeUp:
		argv := []string{"docker", "compose", "up", "-d"}
		if a.ForceRecreate {
			argv = append(argv, "--force-recreate")
		}
		return e.runCmd(ctx, spec, idx, sink, append(argv, a.Services...))
	case *ops.ComposeRun:
		argv := append([]string{"docker", "compose", "run", "--rm", a.Service}, a.Argv...)
		return e.runCmd(ctx, spec, idx, sink, argv)
	case *ops.ComposeExec:
		argv := append([]string{"docker", "compose", "exec", "-T", a.Service}, a.Argv...)
		return e.runCmd(ctx, spec, idx, sink, argv)
	case *ops.ComposeRestart:
		return e.runCmd(ctx, spec, idx, sink, append([]string{"docker", "compose", "restart"}, a.Services...))
	case *ops.EnvRequire:
		return nil, e.envRequire(spec, a)
	case *ops.EnvWrite:
		return nil, e.envWrite(spec, a)
	case *ops.Healthcheck:
		return nil, e.healthcheck(ctx, idx, a, sink)
	case *ops.Run:
		return e.runCmd(ctx, spec, idx, sink, a.Argv)
	default:
		return nil, fmt.Errorf("op %s not supported by this engine", step.Op)
	}
}

// runCmd executes argv streaming output into the sink; a non-zero exit is
// returned as (*code, error).
func (e *Engine) runCmd(ctx context.Context, spec Spec, idx int, sink Sink, argv []string) (*int, error) {
	return e.runCmdStream(ctx, spec, idx, sink, argv, "stdout", nil)
}

// runCapture executes argv capturing stdout (also streamed to the sink under
// `stream`) and returns it trimmed.
func (e *Engine) runCapture(ctx context.Context, spec Spec, idx int, sink Sink, argv []string, stream string) (string, error) {
	var buf bytes.Buffer
	exit, err := e.runCmdStream(ctx, spec, idx, sink, argv, stream, &buf)
	if err != nil {
		return "", err
	}
	_ = exit
	return strings.TrimSpace(buf.String()), nil
}

func (e *Engine) runCmdStream(ctx context.Context, spec Spec, idx int, sink Sink, argv []string, stream string, capture *bytes.Buffer) (*int, error) {
	var stdout io.Writer = &sinkWriter{sink: sink, idx: idx, stream: stream}
	if capture != nil {
		stdout = io.MultiWriter(capture, stdout)
	}
	code, err := e.Runner.Run(ctx, runner.Cmd{
		Argv:   argv,
		Dir:    spec.Dir,
		Stdout: stdout,
		Stderr: &sinkWriter{sink: sink, idx: idx, stream: "stderr"},
	})
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return &code, fmt.Errorf("%s: exit %d", argv[0], code)
	}
	return &code, nil
}

type sinkWriter struct {
	sink   Sink
	idx    int
	stream string
}

func (w *sinkWriter) Write(p []byte) (int, error) {
	w.sink.Log(w.idx, w.stream, string(p))
	return len(p), nil
}

func (e *Engine) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) pullRetries() int {
	if e.PullRetries > 0 {
		return e.PullRetries
	}
	return 3
}

func (e *Engine) pullInterval() time.Duration {
	if e.PullInterval > 0 {
		return e.PullInterval
	}
	return 5 * time.Second
}

func (e *Engine) downloadRetries() int {
	if e.DownloadRetries > 0 {
		return e.DownloadRetries
	}
	return 3
}

// resolveWithin joins rel to base ensuring the result stays inside base.
func resolveWithin(base, rel string) (string, error) {
	p := filepath.Join(base, rel)
	r, err := filepath.Rel(base, p)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the service directory", rel)
	}
	return p, nil
}
