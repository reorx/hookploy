package engine

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reorx/hookploy/internal/ops"
	"github.com/reorx/hookploy/internal/runner"
	"gopkg.in/yaml.v3"
)

// ── harness ────────────────────────────────────────────────────────────────

type sinkEvent struct {
	Kind    string // start | end | log
	OpIndex int
	OpName  string
	Stream  string
	Data    string
	Exit    *int
	Err     error
}

type testSink struct {
	mu     sync.Mutex
	events []sinkEvent
}

func (s *testSink) OpStart(i int, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{Kind: "start", OpIndex: i, OpName: name})
}
func (s *testSink) OpEnd(i int, name string, exit *int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{Kind: "end", OpIndex: i, OpName: name, Exit: exit, Err: err})
}
func (s *testSink) Log(i int, stream, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{Kind: "log", OpIndex: i, Stream: stream, Data: data})
}

func (s *testSink) logs(stream string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		if e.Kind == "log" && e.Stream == stream {
			out = append(out, e.Data)
		}
	}
	return out
}

func steps(t *testing.T, src string) []ops.Step {
	t.Helper()
	var nodes []yaml.Node
	if err := yaml.Unmarshal([]byte(src), &nodes); err != nil {
		t.Fatal(err)
	}
	var out []ops.Step
	for i := range nodes {
		s, err := ops.ParseStep(&nodes[i])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

type fakeDoer struct {
	mu    sync.Mutex
	calls int
	fn    func(req *http.Request, call int) (*http.Response, error)
}

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(req, n)
}

func resp(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

func newEngine(fr *runner.FakeRunner, doer HTTPDoer) *Engine {
	return &Engine{
		Runner: fr,
		HTTP:   doer,
		Sleep:  func(ctx context.Context, d time.Duration) error { return nil }, // zero-delay retries
	}
}

func execute(t *testing.T, e *Engine, spec Spec) (Result, *testSink, error) {
	t.Helper()
	sink := &testSink{}
	res, err := e.Execute(context.Background(), spec, sink)
	return res, sink, err
}

// ── plain ops → argv translation ───────────────────────────────────────────

// Behavior: each compose/run op translates to the documented argv, executed
// in the service dir, never through a shell.
func TestOpArgvTranslation(t *testing.T) {
	fr := &runner.FakeRunner{}
	dir := t.TempDir()
	spec := Spec{
		Dir: dir,
		Steps: steps(t, `
- compose.pull
- compose.pull: { services: [web, worker] }
- compose.up
- compose.up: { force_recreate: true, services: [web] }
- compose.run: { service: web, argv: [python, manage.py, migrate, --noinput] }
- compose.exec: { service: server, argv: [pnpm, db:push] }
- compose.restart: { services: [web] }
- run: { argv: [./deploy.sh, "--flag with space"] }
`),
	}
	_, _, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"docker", "compose", "pull"},
		{"docker", "compose", "pull", "web", "worker"},
		{"docker", "compose", "up", "-d"},
		{"docker", "compose", "up", "-d", "--force-recreate", "web"},
		{"docker", "compose", "run", "--rm", "web", "python", "manage.py", "migrate", "--noinput"},
		{"docker", "compose", "exec", "-T", "server", "pnpm", "db:push"},
		{"docker", "compose", "restart", "web"},
		{"./deploy.sh", "--flag with space"},
	}
	if !reflect.DeepEqual(fr.ArgvList(), want) {
		t.Fatalf("argv mismatch:\ngot  %v\nwant %v", fr.JoinedCalls(), want)
	}
	for _, c := range fr.Calls {
		if c.Dir != dir {
			t.Fatalf("command must run in service dir, got %q", c.Dir)
		}
	}
}

// Behavior: a failing op stops the pipeline; its exit code is reported.
func TestPipelineStopsOnFailure(t *testing.T) {
	fr := &runner.FakeRunner{}
	fr.On("docker", "compose", "pull").Returning("", 7)
	spec := Spec{Dir: t.TempDir(), Steps: steps(t, "[compose.pull, compose.up]")}
	_, sink, err := execute(t, newEngine(fr, nil), spec)
	if err == nil {
		t.Fatal("expected pipeline failure")
	}
	if len(fr.Calls) != 1 {
		t.Fatalf("compose.up must not run after failure, calls: %v", fr.JoinedCalls())
	}
	last := sink.events[len(sink.events)-1]
	if last.Kind != "end" || last.Exit == nil || *last.Exit != 7 {
		t.Fatalf("OpEnd should carry exit 7: %+v", last)
	}
}

// ── image.pin ──────────────────────────────────────────────────────────────

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Behavior: with a payload digest, pin pulls image@digest (with retries),
// records the pinned image id and re-points the local :latest tag.
func TestImagePinWithDigest(t *testing.T) {
	fr := &runner.FakeRunner{}
	img := "ghcr.io/reorx/linkmind"
	fr.On("docker", "image", "inspect", "--format", "{{.Id}}", img+"@"+digest).Returning("sha256:imageid1\n", 0)
	fr.On("docker", "compose", "ps", "-q").Returning("c1\n", 0)
	fr.On("docker", "inspect", "--format", "{{.Image}}").Returning("sha256:imageid1\n", 0)
	spec := Spec{Dir: t.TempDir(), Image: img, Digest: digest, Steps: steps(t, "[image.pin, compose.up]")}
	res, _, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != digest {
		t.Fatalf("resolved digest = %q", res.Digest)
	}
	calls := fr.JoinedCalls()
	want := []string{
		"docker pull " + img + "@" + digest,
		"docker image inspect --format {{.Id}} " + img + "@" + digest,
		"docker tag " + img + "@" + digest + " " + img + ":latest",
		"docker compose up -d",
		"docker compose ps -q", // built-in post-deploy verification begins
	}
	for i, w := range want {
		if i >= len(calls) || calls[i] != w {
			t.Fatalf("call %d:\ngot  %v\nwant %q", i, calls, w)
		}
	}
}

// Behavior: without a digest (manual trigger), pin resolves :latest's
// RepoDigest and follows the same path.
func TestImagePinWithoutDigest(t *testing.T) {
	fr := &runner.FakeRunner{}
	img := "ghcr.io/reorx/linkmind"
	fr.On("docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", img+":latest").
		Returning(img+"@"+digest+"\n", 0)
	fr.On("docker", "image", "inspect", "--format", "{{.Id}}", img+"@"+digest).Returning("sha256:id\n", 0)
	fr.On("docker", "compose", "ps", "-q").Returning("c1\n", 0)
	fr.On("docker", "inspect", "--format", "{{.Image}}").Returning("sha256:id\n", 0)
	spec := Spec{Dir: t.TempDir(), Image: img, Steps: steps(t, "[image.pin, compose.up]")}
	res, _, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != digest {
		t.Fatalf("resolved digest = %q", res.Digest)
	}
	calls := fr.JoinedCalls()
	if calls[0] != "docker pull "+img+":latest" {
		t.Fatalf("first call should pull :latest, got %v", calls)
	}
}

// Behavior: the digest pull retries 3 times before giving up.
func TestImagePinPullRetries(t *testing.T) {
	fr := &runner.FakeRunner{}
	img := "img"
	// first two pulls fail, third succeeds
	fr.On("docker", "pull", img+"@"+digest).Returning("manifest unknown", 1).Once = true
	fr.On("docker", "pull", img+"@"+digest).Returning("manifest unknown", 1).Once = true
	fr.On("docker", "image", "inspect", "--format", "{{.Id}}", img+"@"+digest).Returning("sha256:id\n", 0)
	fr.On("docker", "compose", "ps", "-q").Returning("c1\n", 0)
	fr.On("docker", "inspect", "--format", "{{.Image}}").Returning("sha256:id\n", 0)
	spec := Spec{Dir: t.TempDir(), Image: img, Digest: digest, Steps: steps(t, "[image.pin, compose.up]")}
	if _, _, err := execute(t, newEngine(fr, nil), spec); err != nil {
		t.Fatal(err)
	}
	pulls := 0
	for _, c := range fr.JoinedCalls() {
		if c == "docker pull "+img+"@"+digest {
			pulls++
		}
	}
	if pulls != 3 {
		t.Fatalf("want 3 pull attempts, got %d", pulls)
	}

	// all three fail → op fails
	fr2 := &runner.FakeRunner{}
	fr2.On("docker", "pull", img+"@"+digest).Returning("nope", 1)
	if _, _, err := execute(t, newEngine(fr2, nil), spec); err == nil {
		t.Fatal("pin must fail after 3 failed pulls")
	}
}

// Behavior: after the last compose.up, at least one running container must
// use the pinned image id — otherwise the whole execution fails.
func TestImagePinVerification(t *testing.T) {
	img := "img"
	mk := func(psOut, inspectOut string) (*runner.FakeRunner, Spec) {
		fr := &runner.FakeRunner{}
		fr.On("docker", "image", "inspect", "--format", "{{.Id}}", img+"@"+digest).Returning("sha256:pinned\n", 0)
		fr.On("docker", "compose", "ps", "-q").Returning(psOut, 0)
		fr.On("docker", "inspect", "--format", "{{.Image}}").Returning(inspectOut, 0)
		return fr, Spec{Dir: "/tmp", Image: img, Digest: digest, Steps: steps(t, "[image.pin, compose.up]")}
	}

	// match → success, logged on the system stream
	fr, spec := mk("c1\n", "sha256:pinned\n")
	_, sink, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatalf("verification should pass: %v", err)
	}
	if sys := sink.logs("system"); len(sys) == 0 {
		t.Fatal("verification should log to the system stream")
	}

	// mismatch → failed
	fr, spec = mk("c1\n", "sha256:other\n")
	if _, _, err := execute(t, newEngine(fr, nil), spec); err == nil || !strings.Contains(err.Error(), "verif") {
		t.Fatalf("verification mismatch must fail the execution: %v", err)
	}

	// no containers → failed
	fr, spec = mk("", "")
	if _, _, err := execute(t, newEngine(fr, nil), spec); err == nil {
		t.Fatal("no running containers must fail verification")
	}
}

// Behavior: verification runs after the LAST compose.up, not the first.
func TestImagePinVerifiesAfterLastUp(t *testing.T) {
	fr := &runner.FakeRunner{}
	img := "img"
	fr.On("docker", "image", "inspect", "--format", "{{.Id}}", img+"@"+digest).Returning("sha256:pinned\n", 0)
	fr.On("docker", "compose", "ps", "-q").Returning("c1\n", 0)
	fr.On("docker", "inspect", "--format", "{{.Image}}").Returning("sha256:pinned\n", 0)
	spec := Spec{Dir: "/tmp", Image: img, Digest: digest,
		Steps: steps(t, "[image.pin, compose.up, compose.restart, compose.up]")}
	if _, _, err := execute(t, newEngine(fr, nil), spec); err != nil {
		t.Fatal(err)
	}
	calls := fr.JoinedCalls()
	// ps -q must appear after the second `docker compose up -d`
	lastUp, psIdx := -1, -1
	for i, c := range calls {
		if c == "docker compose up -d" {
			lastUp = i
		}
		if c == "docker compose ps -q" {
			psIdx = i
		}
	}
	if psIdx < lastUp {
		t.Fatalf("verification ran before the last compose.up: %v", calls)
	}
}

// ── image.extract ──────────────────────────────────────────────────────────

// Behavior: image.extract creates a temp container, cps the path to <to>.new,
// swaps near-atomically, and removes the container even on failure.
func TestImageExtract(t *testing.T) {
	dir := t.TempDir()
	// pre-existing target with old content
	os.MkdirAll(filepath.Join(dir, "static"), 0o755)
	os.WriteFile(filepath.Join(dir, "static", "old.txt"), []byte("old"), 0o644)

	fr := &runner.FakeRunner{}
	fr.On("docker", "create").Returning("cid123\n", 0)
	fr.On("docker", "cp").Effect = func(c runner.Cmd) error {
		dst := c.Argv[len(c.Argv)-1]
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, "new.txt"), []byte("new"), 0o644)
	}
	spec := Spec{Dir: dir, Image: "ghcr.io/x/breeze",
		Steps: steps(t, `[{image.extract: {from: /app/static, to: static}}]`)}
	_, _, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatal(err)
	}

	calls := fr.JoinedCalls()
	want := []string{
		"docker create ghcr.io/x/breeze:latest",
		"docker cp cid123:/app/static " + filepath.Join(dir, "static.new"),
		"docker rm -f cid123",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls:\ngot  %v\nwant %v", calls, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "static", "new.txt")); err != nil {
		t.Fatal("swap did not install new content")
	}
	if _, err := os.Stat(filepath.Join(dir, "static", "old.txt")); err == nil {
		t.Fatal("old content still present after swap")
	}
	if _, err := os.Stat(filepath.Join(dir, "static.old")); err == nil {
		t.Fatal("static.old must be cleaned up")
	}

	// failure path: cp fails → container still removed
	fr2 := &runner.FakeRunner{}
	fr2.On("docker", "create").Returning("cid9\n", 0)
	fr2.On("docker", "cp").Returning("", 1)
	if _, _, err := execute(t, newEngine(fr2, nil), spec); err == nil {
		t.Fatal("cp failure must fail the op")
	}
	joined := strings.Join(fr2.JoinedCalls(), "|")
	if !strings.Contains(joined, "docker rm -f cid9") {
		t.Fatalf("temp container must be removed on failure: %v", fr2.JoinedCalls())
	}

	// explicit image + pull
	fr3 := &runner.FakeRunner{}
	fr3.On("docker", "create").Returning("c\n", 0)
	fr3.On("docker", "cp").Effect = func(c runner.Cmd) error { return os.MkdirAll(c.Argv[len(c.Argv)-1], 0o755) }
	spec3 := Spec{Dir: t.TempDir(), Steps: steps(t, `[{image.extract: {from: /f, to: d, image: "other:tag", pull: true}}]`)}
	if _, _, err := execute(t, newEngine(fr3, nil), spec3); err != nil {
		t.Fatal(err)
	}
	if fr3.JoinedCalls()[0] != "docker pull other:tag" {
		t.Fatalf("pull:true must docker pull first: %v", fr3.JoinedCalls())
	}

	// escaping `to` is rejected
	specBad := Spec{Dir: t.TempDir(), Image: "x",
		Steps: steps(t, `[{image.extract: {from: /a, to: ../../etc}}]`)}
	if _, _, err := execute(t, newEngine(&runner.FakeRunner{}, nil), specBad); err == nil {
		t.Fatal("path escaping the service dir must be rejected")
	}
}

// ── artifact.extract ───────────────────────────────────────────────────────

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(content))
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Behavior: artifact.extract downloads (with retries), verifies sha256,
// unpacks tar.gz and swaps into place.
func TestArtifactExtract(t *testing.T) {
	body := tarGz(t, map[string]string{"index.html": "<html>", "assets/app.js": "js"})
	doer := &fakeDoer{fn: func(req *http.Request, call int) (*http.Response, error) {
		if call == 1 { // first attempt fails → retry
			return nil, fmt.Errorf("connection reset")
		}
		return resp(200, string(body)), nil
	}}
	dir := t.TempDir()
	spec := Spec{Dir: dir, Steps: steps(t, fmt.Sprintf(
		`[{artifact.extract: {url: "https://x/webdist.tar.gz", sha256: %q, to: webdist}}]`, sha256hex(body)))}
	_, _, err := execute(t, newEngine(&runner.FakeRunner{}, doer), spec)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "webdist", "index.html"))
	if err != nil || string(got) != "<html>" {
		t.Fatalf("extracted content wrong: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "webdist", "assets", "app.js")); err != nil {
		t.Fatal("nested file missing")
	}
	if doer.calls != 2 {
		t.Fatalf("expected 1 retry (2 calls), got %d", doer.calls)
	}
}

// Behavior: sha256 mismatch fails before anything is installed.
func TestArtifactExtractShaMismatch(t *testing.T) {
	body := tarGz(t, map[string]string{"a": "x"})
	doer := &fakeDoer{fn: func(*http.Request, int) (*http.Response, error) { return resp(200, string(body)), nil }}
	dir := t.TempDir()
	spec := Spec{Dir: dir, Steps: steps(t,
		`[{artifact.extract: {url: "https://x/a.tar.gz", sha256: deadbeef, to: webdist}}]`)}
	_, _, err := execute(t, newEngine(&runner.FakeRunner{}, doer), spec)
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("want sha256 error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "webdist")); err == nil {
		t.Fatal("nothing must be installed on mismatch")
	}
}

// Behavior: zip archives are supported; zip-slip entries are rejected.
func TestArtifactExtractZipAndZipSlip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("ok.txt")
	w.Write([]byte("fine"))
	zw.Close()
	body := buf.Bytes()
	doer := &fakeDoer{fn: func(*http.Request, int) (*http.Response, error) { return resp(200, string(body)), nil }}
	dir := t.TempDir()
	spec := Spec{Dir: dir, Steps: steps(t, fmt.Sprintf(
		`[{artifact.extract: {url: "https://x/d.zip", sha256: %q, to: dist}}]`, sha256hex(body)))}
	if _, _, err := execute(t, newEngine(&runner.FakeRunner{}, doer), spec); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "dist", "ok.txt")); string(b) != "fine" {
		t.Fatal("zip content missing")
	}

	// slip: entry escaping the target
	var evil bytes.Buffer
	zw = zip.NewWriter(&evil)
	w, _ = zw.Create("../../pwned.txt")
	w.Write([]byte("evil"))
	zw.Close()
	doer2 := &fakeDoer{fn: func(*http.Request, int) (*http.Response, error) { return resp(200, evil.String()), nil }}
	dir2 := t.TempDir()
	spec2 := Spec{Dir: dir2, Steps: steps(t, fmt.Sprintf(
		`[{artifact.extract: {url: "https://x/d.zip", sha256: %q, to: dist}}]`, sha256hex(evil.Bytes())))}
	if _, _, err := execute(t, newEngine(&runner.FakeRunner{}, doer2), spec2); err == nil {
		t.Fatal("zip-slip must be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir2), "pwned.txt")); err == nil {
		t.Fatal("zip-slip wrote outside the target!")
	}
}

// ── env ops ────────────────────────────────────────────────────────────────

// Behavior: env.require passes only when every key has a non-empty value.
func TestEnvRequire(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("KAGI_EMAIL=me@x.com\nEMPTY=\n#C=1\n"), 0o644)
	e := newEngine(&runner.FakeRunner{}, nil)

	spec := Spec{Dir: dir, Steps: steps(t, `[{env.require: {file: .env, keys: [KAGI_EMAIL]}}]`)}
	if _, _, err := execute(t, e, spec); err != nil {
		t.Fatalf("filled key should pass: %v", err)
	}
	spec = Spec{Dir: dir, Steps: steps(t, `[{env.require: {file: .env, keys: [EMPTY]}}]`)}
	if _, _, err := execute(t, e, spec); err == nil || !strings.Contains(err.Error(), "EMPTY") {
		t.Fatalf("empty value must fail naming the key: %v", err)
	}
	spec = Spec{Dir: dir, Steps: steps(t, `[{env.require: {file: .env, keys: [MISSING]}}]`)}
	if _, _, err := execute(t, e, spec); err == nil {
		t.Fatal("missing key must fail")
	}
	spec = Spec{Dir: dir, Steps: steps(t, `[{env.require: {file: nope.env, keys: [A]}}]`)}
	if _, _, err := execute(t, e, spec); err == nil {
		t.Fatal("missing file must fail")
	}
}

// Behavior: env.write upserts KEY=VALUE lines, preserving unrelated content,
// atomically (tmp+rename).
func TestEnvWrite(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("# comment\nA=1\nB=2\n"), 0o644)
	e := newEngine(&runner.FakeRunner{}, nil)
	spec := Spec{Dir: dir, Steps: steps(t, `[{env.write: {file: .env, set: {B: "20", C: "3"}}}]`)}
	if _, _, err := execute(t, e, spec); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, ".env"))
	s := string(got)
	if !strings.Contains(s, "# comment\n") || !strings.Contains(s, "A=1\n") {
		t.Fatalf("unrelated lines must be preserved: %q", s)
	}
	if !strings.Contains(s, "B=20\n") || !strings.Contains(s, "C=3\n") {
		t.Fatalf("upsert failed: %q", s)
	}
	if strings.Contains(s, "B=2\n") {
		t.Fatalf("old value must be replaced: %q", s)
	}

	// file may not exist yet
	spec = Spec{Dir: dir, Steps: steps(t, `[{env.write: {file: fresh.env, set: {X: "1"}}}]`)}
	if _, _, err := execute(t, e, spec); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "fresh.env")); !strings.Contains(string(b), "X=1\n") {
		t.Fatalf("fresh file: %q", b)
	}
}

// ── healthcheck ────────────────────────────────────────────────────────────

// Behavior: healthcheck polls until the expected status, failing when
// retries are exhausted.
func TestHealthcheck(t *testing.T) {
	doer := &fakeDoer{fn: func(req *http.Request, call int) (*http.Response, error) {
		if call < 3 {
			return resp(502, "bad"), nil
		}
		return resp(200, "ok"), nil
	}}
	e := newEngine(&runner.FakeRunner{}, doer)
	spec := Spec{Dir: "/tmp", Steps: steps(t, `[{healthcheck: {url: "http://127.0.0.1/healthz", retries: 5}}]`)}
	if _, _, err := execute(t, e, spec); err != nil {
		t.Fatal(err)
	}
	if doer.calls != 3 {
		t.Fatalf("calls = %d, want 3", doer.calls)
	}

	always502 := &fakeDoer{fn: func(*http.Request, int) (*http.Response, error) { return resp(502, ""), nil }}
	e2 := newEngine(&runner.FakeRunner{}, always502)
	spec2 := Spec{Dir: "/tmp", Steps: steps(t, `[{healthcheck: {url: "http://x/h", retries: 4}}]`)}
	if _, _, err := execute(t, e2, spec2); err == nil {
		t.Fatal("exhausted retries must fail")
	}
	if always502.calls != 4 {
		t.Fatalf("calls = %d, want 4", always502.calls)
	}
}

// ── logging ────────────────────────────────────────────────────────────────

// Behavior: op stdout/stderr stream into the sink under the right op index.
func TestOutputStreamsToSink(t *testing.T) {
	fr := &runner.FakeRunner{}
	r := fr.On("docker", "compose", "pull")
	r.Stdout = "pulling web\n"
	r.Stderr = "warn: platform\n"
	spec := Spec{Dir: "/tmp", Steps: steps(t, "[compose.pull]")}
	_, sink, err := execute(t, newEngine(fr, nil), spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.logs("stdout"); len(got) == 0 || !strings.Contains(strings.Join(got, ""), "pulling web") {
		t.Fatalf("stdout not streamed: %v", got)
	}
	if got := sink.logs("stderr"); len(got) == 0 || !strings.Contains(strings.Join(got, ""), "warn") {
		t.Fatalf("stderr not streamed: %v", got)
	}
}
