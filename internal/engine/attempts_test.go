package engine

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/runner"
)

// sleepRecorder is an engine Sleep hook that returns at once and remembers
// every pause it was asked for.
type sleepRecorder struct {
	mu     sync.Mutex
	pauses []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pauses = append(s.pauses, d)
	return ctx.Err()
}

func countCalls(fr *runner.FakeRunner, joined string) int {
	n := 0
	for _, c := range fr.JoinedCalls() {
		if c == joined {
			n++
		}
	}
	return n
}

func opEvents(sink *testSink, kind string) []sinkEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []sinkEvent
	for _, e := range sink.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Behavior: a step's timeout cancels the attempt that hangs (the stuck
// process is killed), and the next attempt is a fresh run that can succeed.
// The op is still one op: one start, one successful end; the retry shows up
// in its system log.
func TestStepTimeoutThenRetrySucceeds(t *testing.T) {
	fr := &runner.FakeRunner{}
	stuck := fr.On("docker", "compose", "pull")
	stuck.BlockUntilCancel, stuck.Once = true, true
	fr.On("docker", "compose", "pull").Returning("pulled\n", 0)

	e := newEngine(fr, nil)
	spec := Spec{Dir: "/tmp", Steps: steps(t, `
- compose.pull:
  timeout: 50ms
  retries: 1
- compose.up
`)}
	_, sink, err := execute(t, e, spec)
	if err != nil {
		t.Fatalf("second attempt should have succeeded: %v", err)
	}
	if n := countCalls(fr, "docker compose pull"); n != 2 {
		t.Fatalf("want 2 pull attempts, got %d", n)
	}
	if starts := opEvents(sink, "start"); len(starts) != 2 || starts[0].OpName != "compose.pull" {
		t.Fatalf("the retried op must start once: %+v", starts)
	}
	if end := opEvents(sink, "end")[0]; end.Err != nil || end.Exit == nil || *end.Exit != 0 {
		t.Fatalf("the retried op must end once, successfully: %+v", end)
	}
	sys := strings.Join(sink.logs("system"), "")
	for _, want := range []string{
		"compose.pull attempt 1/2 failed: timed out after 50ms; retrying in 5s",
		"compose.pull attempt 2/2\n",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system log lacks %q:\n%s", want, sys)
		}
	}
}

// Behavior: when every attempt fails the op fails, and its error names the
// op, the number of attempts and each attempt's reason. Attempts are spaced
// by the fixed retry interval, and the pipeline stops there.
func TestStepRetriesExhausted(t *testing.T) {
	fr := &runner.FakeRunner{}
	fr.On("docker", "compose", "pull").Returning("", 1)
	rec := &sleepRecorder{}
	e := &Engine{Runner: fr, Sleep: rec.sleep}
	spec := Spec{Dir: "/tmp", Steps: steps(t, `
- compose.pull:
  retries: 2
- compose.up
`)}
	_, sink, err := execute(t, e, spec)
	if err == nil {
		t.Fatal("exhausted retries must fail the execution")
	}
	for _, want := range []string{
		"op 1 (compose.pull): failed after 3 attempts",
		"[1/3] docker: exit 1", "[2/3] docker: exit 1", "[3/3] docker: exit 1",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if n := countCalls(fr, "docker compose pull"); n != 3 {
		t.Fatalf("want 3 attempts, got %d", n)
	}
	if countCalls(fr, "docker compose up -d") != 0 {
		t.Fatal("the pipeline must stop at the failed op")
	}
	if len(rec.pauses) != 2 || rec.pauses[0] != ops.RetryInterval || rec.pauses[1] != ops.RetryInterval {
		t.Fatalf("want two %s pauses between three attempts, got %v", ops.RetryInterval, rec.pauses)
	}
	if end := opEvents(sink, "end")[0]; end.Err == nil || !strings.Contains(end.Err.Error(), "3 attempts") {
		t.Fatalf("the op record must carry the attempts summary: %+v", end)
	}
}

// Behavior: a timeout without retries is a single bounded attempt — the op
// fails as "timed out", the execution's own context is untouched, and later
// ops do not run.
func TestStepTimeoutWithoutRetries(t *testing.T) {
	fr := &runner.FakeRunner{}
	fr.On("docker", "compose", "up").BlockUntilCancel = true
	spec := Spec{Dir: "/tmp", Steps: steps(t, `
- compose.up:
  timeout: 30ms
- run: { argv: [echo, after] }
`)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &testSink{}
	_, err := newEngine(fr, nil).Execute(ctx, spec, sink)
	if err == nil || err.Error() != "op 1 (compose.up): timed out after 30ms" {
		t.Fatalf("want a plain step timeout, got %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("a step timeout must not cancel the execution context")
	}
	if countCalls(fr, "echo after") != 0 {
		t.Fatal("ops after a timed-out step must not run")
	}
	if sys := sink.logs("system"); len(sys) != 0 {
		t.Fatalf("a single attempt logs no retry lines: %v", sys)
	}
}

// Behavior: the execution timeout stays the backstop: once it fires, no
// further attempt starts, and the error still reports the deadline.
func TestExecutionTimeoutStopsRetries(t *testing.T) {
	fr := &runner.FakeRunner{}
	fr.On("docker", "compose", "pull").BlockUntilCancel = true
	spec := Spec{Dir: "/tmp", Steps: steps(t, `
- compose.pull:
  timeout: 1h
  retries: 5
`)}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := newEngine(fr, nil).Execute(ctx, spec, &testSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the execution deadline, got %v", err)
	}
	if n := countCalls(fr, "docker compose pull"); n != 1 {
		t.Fatalf("no attempt may start after the execution deadline, got %d", n)
	}

	// the deadline landing in the pause between attempts keeps the history
	fr = &runner.FakeRunner{}
	fr.On("docker", "compose", "pull").Returning("", 1)
	e := &Engine{Runner: fr, Sleep: func(context.Context, time.Duration) error { return context.DeadlineExceeded }}
	spec.Steps = steps(t, "- compose.pull:\n  retries: 2\n")
	_, err = e.Execute(context.Background(), spec, &testSink{})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "[1/3] docker: exit 1") {
		t.Fatalf("want the deadline plus the attempt so far, got %v", err)
	}
}

// Behavior: image.pin retries as a whole op — the three tries its built-in
// pull loop used to give it are now its default retries, and `retries: 0`
// switches them off.
func TestImagePinDefaultRetries(t *testing.T) {
	img := "img"
	pinned := img + "@" + digest
	mk := func() *runner.FakeRunner {
		fr := &runner.FakeRunner{}
		fr.On("docker", "image", "inspect", "--format", "{{.Id}}", pinned).Returning("sha256:id\n", 0)
		fr.On("docker", "compose", "ps", "-q").Returning("c1\n", 0)
		fr.On("docker", "inspect", "--format", "{{.Image}}").Returning("sha256:id\n", 0)
		return fr
	}

	// two failed pulls, then success: the default gives it three attempts
	fr := mk()
	fr.Rules = append([]*runner.Rule{
		{Match: []string{"docker", "pull", pinned}, Stdout: "manifest unknown", Exit: 1, Once: true},
		{Match: []string{"docker", "pull", pinned}, Stdout: "manifest unknown", Exit: 1, Once: true},
	}, fr.Rules...)
	spec := Spec{Dir: t.TempDir(), Image: img, Digest: digest, Steps: steps(t, "[image.pin, compose.up]")}
	_, sink, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatal(err)
	}
	if n := countCalls(fr, "docker pull "+pinned); n != 3 {
		t.Fatalf("want 3 pull attempts, got %d", n)
	}
	if sys := strings.Join(sink.logs("system"), ""); !strings.Contains(sys, "image.pin attempt 3/3") {
		t.Fatalf("retries should be logged under image.pin:\n%s", sys)
	}

	// every pull fails → the op fails with the attempts summary
	fr = mk()
	fr.Rules = append([]*runner.Rule{{Match: []string{"docker", "pull", pinned}, Exit: 1}}, fr.Rules...)
	_, _, err = execute(t, newEngine(fr, nil), spec)
	if err == nil || !strings.Contains(err.Error(), "image.pin): failed after 3 attempts") {
		t.Fatalf("want the attempts summary, got %v", err)
	}

	// retries: 0 → one pull, plain error
	fr = mk()
	fr.Rules = append([]*runner.Rule{{Match: []string{"docker", "pull", pinned}, Exit: 1}}, fr.Rules...)
	spec.Steps = steps(t, "- image.pin:\n  retries: 0\n- compose.up\n")
	_, _, err = execute(t, newEngine(fr, nil), spec)
	if err == nil || strings.Contains(err.Error(), "attempts") {
		t.Fatalf("a single attempt fails with the plain error, got %v", err)
	}
	if n := countCalls(fr, "docker pull "+pinned); n != 1 {
		t.Fatalf("retries: 0 means one pull, got %d", n)
	}
}

// Behavior: a failure another attempt cannot fix gives up at once, whatever
// the retries: a download that arrived intact but with the wrong sha256,
// or an image.pin on a service that declares no image.
func TestPermanentFailuresAreNotRetried(t *testing.T) {
	body := tarGz(t, map[string]string{"a": "x"})
	doer := &fakeDoer{fn: func(*http.Request, int) (*http.Response, error) { return resp(200, string(body)), nil }}
	spec := Spec{Dir: t.TempDir(), Steps: steps(t, `
- artifact.extract: { url: "https://x/a.tar.gz", sha256: deadbeef, to: webdist }
  retries: 4
`)}
	_, _, err := execute(t, newEngine(&runner.FakeRunner{}, doer), spec)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") || strings.Contains(err.Error(), "attempts") {
		t.Fatalf("want the bare sha256 error, got %v", err)
	}
	if doer.calls != 1 {
		t.Fatalf("a sha256 mismatch must not be downloaded again, got %d downloads", doer.calls)
	}

	fr := &runner.FakeRunner{}
	spec = Spec{Dir: t.TempDir(), Steps: steps(t, "[image.pin, compose.up]")}
	_, sink, err := execute(t, newEngine(fr, nil), spec)
	if err == nil || !strings.Contains(err.Error(), "no \"image\" declared") {
		t.Fatalf("want the missing-image error, got %v", err)
	}
	if sys := sink.logs("system"); len(sys) != 0 {
		t.Fatalf("a permanent failure must not be retried: %v", sys)
	}
}

// Behavior: a pipeline without modifiers logs exactly what it did before
// modifiers existed — no attempt lines on the way to success.
func TestNoModifiersNoRetryLogs(t *testing.T) {
	fr := &runner.FakeRunner{}
	fr.On("docker", "image", "inspect", "--format", "{{.Id}}", "img@"+digest).Returning("sha256:id\n", 0)
	fr.On("docker", "compose", "ps", "-q").Returning("c1\n", 0)
	fr.On("docker", "inspect", "--format", "{{.Image}}").Returning("sha256:id\n", 0)
	spec := Spec{Dir: t.TempDir(), Image: "img", Digest: digest, Steps: steps(t, "[image.pin, compose.pull, compose.up]")}
	_, sink, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range sink.logs("system") {
		if strings.Contains(line, "attempt") {
			t.Fatalf("unexpected retry log on the happy path: %q", line)
		}
	}
}

// Behavior: a step timeout that cuts image.extract short still removes the
// temp container — cleanup must not share the expired attempt context.
func TestImageExtractCleansUpAfterStepTimeout(t *testing.T) {
	fr := &runner.FakeRunner{}
	fr.On("docker", "create").Returning("cid123\n", 0)
	fr.On("docker", "cp").BlockUntilCancel = true
	spec := Spec{Dir: t.TempDir(), Image: "img", Steps: steps(t, `
- image.extract: { from: /app/static, to: static }
  timeout: 30ms
`)}
	_, sink, err := execute(t, newEngine(fr, nil), spec)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want a step timeout, got %v", err)
	}
	if countCalls(fr, "docker rm -f cid123") != 1 {
		t.Fatalf("temp container must be removed: %v", fr.JoinedCalls())
	}
	for _, line := range sink.logs("system") {
		if strings.Contains(line, "failed to remove temp container") {
			t.Fatalf("cleanup ran on the expired context: %q", line)
		}
	}
}

// Behavior (real processes): a step timeout kills a genuinely stuck child
// process — here a fake `docker` on PATH whose first run hangs the way the
// 2026-09-24 pull did — and the retry runs a new process that succeeds.
func TestStepTimeoutKillsStuckProcess(t *testing.T) {
	bin := t.TempDir()
	script := `#!/bin/sh
[ "$1" = warmup ] && exit 0
n=$(cat .attempts 2>/dev/null || echo 0)
n=$((n+1))
echo $n > .attempts
if [ "$n" = 1 ]; then
  echo "Downloading 30MiB/65MiB"
  sleep 30
fi
echo "Pull complete"
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// The first exec of a freshly written file can stall for seconds on macOS
	// (it gets scanned), which would eat the timeout of the attempt that is
	// meant to succeed. Pay that cost before anything is timed.
	if out, err := exec.Command("docker", "warmup").CombinedOutput(); err != nil {
		t.Fatalf("warm up fake docker: %v %s", err, out)
	}

	dir := t.TempDir()
	e := &Engine{
		Runner: &runner.ExecRunner{KillGrace: 200 * time.Millisecond},
		Sleep:  func(ctx context.Context, d time.Duration) error { return nil },
	}
	spec := Spec{Dir: dir, Steps: steps(t, `
- compose.pull:
  timeout: 1s
  retries: 1
`)}
	start := time.Now()
	_, sink, err := execute(t, e, spec)
	if err != nil {
		t.Fatalf("retry after the killed attempt should succeed: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the stuck attempt was not killed in time (%s)", took)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".attempts"))
	if strings.TrimSpace(string(b)) != "2" {
		t.Fatalf("want 2 process runs, got %q", b)
	}
	out := strings.Join(sink.logs("stdout"), "")
	if !strings.Contains(out, "Downloading 30MiB/65MiB") || !strings.Contains(out, "Pull complete") {
		t.Fatalf("both attempts' output should be in the op log:\n%s", out)
	}
}
