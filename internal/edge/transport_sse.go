package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/edgewire"
	"github.com/reorx/hookploy/internal/version"
)

const (
	// defaultHeartbeat mirrors main's default; used only when a hello omits it.
	defaultHeartbeat = 45 * time.Second
	// updateFlushInterval batches progress so a chatty build does not turn
	// into one request per log line.
	updateFlushInterval = 200 * time.Millisecond
	updateBatchSize     = 100
	// maxBufferedUpdates drops progress rather than growing without bound
	// when main stops accepting it. Only the result must survive.
	maxBufferedUpdates = 2000
	// doneAttempts bounds one ReportDone call; giving up returns an error and
	// leaves the result in the agent's buffer for the next attempt.
	doneAttempts = 4
	postTimeout  = 15 * time.Second
)

// errNoSSEEndpoint means main does not serve the SSE protocol at all.
var errNoSSEEndpoint = errors.New(
	"main returned 404 for /edge/session — main is probably older than this edge; " +
		"upgrade main, or run this edge with --transport grpc")

// sseTransport speaks the HTTP/SSE edge protocol: a long-lived GET carries
// executions down, and results travel back as separate POSTs. Because those
// POSTs are independent requests, a result survives the download stream
// breaking — which is what makes these sessions resumable.
type sseTransport struct {
	base   string // main URL without a trailing slash
	token  string
	server string
	logger *log.Logger

	// root outlives any one session: reports must still reach main after the
	// stream that carried the execution is gone.
	root   context.Context
	client *http.Client

	// silenceOverride replaces the heartbeat-derived dead-stream threshold
	// (tests only).
	silenceOverride time.Duration
}

func newSSETransport(ctx context.Context, opts Options, logger *log.Logger) (*sseTransport, error) {
	u, err := url.Parse(opts.MainURL)
	if err != nil {
		return nil, fmt.Errorf("--main %q: %w", opts.MainURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("--main %q: scheme must be http or https", opts.MainURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("--main %q: missing host", opts.MainURL)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &sseTransport{
		base:   strings.TrimSuffix(u.String(), "/"),
		token:  opts.Token,
		server: opts.Server,
		logger: logger,
		root:   ctx,
		// No client-wide Timeout: the session stream is meant to stay open
		// for hours. Per-request deadlines cover the POSTs instead.
		client: &http.Client{Transport: tr},
	}, nil
}

func (t *sseTransport) Dial(ctx context.Context, inflight []string) (Session, error) {
	q := url.Values{}
	q.Set("version", version.Version)
	if t.server != "" {
		q.Set("server", t.server)
	}
	for _, id := range inflight {
		q.Add("inflight", id)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+"/edge/session?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drainAndClose(resp)
		return nil, errNoSSEEndpoint
	}
	if resp.StatusCode != http.StatusOK {
		detail := errorDetail(resp)
		drainAndClose(resp)
		return nil, fmt.Errorf("GET /edge/session: %s%s", resp.Status, detail)
	}

	sess := &sseSession{
		tr:      t,
		body:    resp.Body,
		reader:  newSSEReader(resp.Body),
		events:  make(chan sseEvent),
		done:    make(chan struct{}),
		flushCh: make(chan struct{}, 1),
		updates: map[string][]edgewire.Update{},
	}
	go sess.readLoop()
	if err := sess.readHello(); err != nil {
		sess.Close()
		return nil, err
	}
	go sess.flushLoop()
	return sess, nil
}

// sseSession is one established SSE session.
type sseSession struct {
	tr     *sseTransport
	body   io.ReadCloser
	reader *sseReader

	hello   edgewire.Hello
	silence time.Duration

	events  chan sseEvent
	done    chan struct{}
	once    sync.Once
	flushCh chan struct{}

	errMu  sync.Mutex
	readEr error

	updMu   sync.Mutex
	updates map[string][]edgewire.Update
	updN    int
}

// readLoop is the only reader of the response body.
func (s *sseSession) readLoop() {
	defer close(s.events)
	for {
		ev, err := s.reader.Next()
		if err != nil {
			s.errMu.Lock()
			s.readEr = err
			s.errMu.Unlock()
			return
		}
		select {
		case s.events <- ev:
		case <-s.done:
			return
		}
	}
}

// next returns the next frame, or an error if nothing at all arrives within
// d. Heartbeats come through as zero-value events, so silence — not
// idleness — is what marks a stream dead.
func (s *sseSession) next(d time.Duration) (sseEvent, error) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case ev, ok := <-s.events:
		if !ok {
			s.errMu.Lock()
			err := s.readEr
			s.errMu.Unlock()
			if err == nil {
				err = io.EOF
			}
			return sseEvent{}, err
		}
		return ev, nil
	case <-timer.C:
		return sseEvent{}, fmt.Errorf("no traffic from main for %s", d)
	case <-s.done:
		return sseEvent{}, errors.New("session closed")
	}
}

func (s *sseSession) readHello() error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("main sent no hello frame")
		}
		ev, err := s.next(remaining)
		if err != nil {
			return err
		}
		if ev.Name == "" {
			continue // heartbeat before hello; keep waiting
		}
		if ev.Name != edgewire.EventHello {
			return fmt.Errorf("expected %s frame, got %q", edgewire.EventHello, ev.Name)
		}
		if err := json.Unmarshal(ev.Data, &s.hello); err != nil {
			return fmt.Errorf("hello frame: %w", err)
		}
		s.silence = s.tr.silenceThreshold(s.hello.HeartbeatSeconds)
		return nil
	}
}

// silenceThreshold allows for one missed heartbeat plus slack, so a slow
// proxy does not look like a dead main.
func (t *sseTransport) silenceThreshold(heartbeatSeconds int) time.Duration {
	if t.silenceOverride > 0 {
		return t.silenceOverride
	}
	hb := time.Duration(heartbeatSeconds) * time.Second
	if hb <= 0 {
		hb = defaultHeartbeat
	}
	return 2*hb + 10*time.Second
}

func (s *sseSession) Recv() (*Task, error) {
	for {
		ev, err := s.next(s.silence)
		if err != nil {
			return nil, err
		}
		if ev.Name != edgewire.EventExec {
			continue // heartbeat, or a frame this version does not know
		}
		var e edgewire.Exec
		if err := json.Unmarshal(ev.Data, &e); err != nil {
			return nil, fmt.Errorf("exec frame: %w", err)
		}
		return &Task{
			ExecutionID: e.ExecutionID,
			Kind:        e.Kind,
			Service:     e.Service,
			Instance:    e.Instance,
			Dir:         e.Dir,
			Image:       e.Image,
			Digest:      e.Digest,
			OpsJSON:     e.Ops,
			Timeout:     time.Duration(e.TimeoutMS) * time.Millisecond,
		}, nil
	}
}

func (s *sseSession) Resumable() bool     { return true }
func (s *sseSession) MainVersion() string { return s.hello.MainVersion }
func (s *sseSession) Server() string      { return s.hello.Server }

func (s *sseSession) Close() {
	s.once.Do(func() {
		close(s.done)
		s.body.Close()
	})
}

// ── reporting ──────────────────────────────────────────────────────────────

func (s *sseSession) ReportUpdate(execID string, u Update) {
	s.updMu.Lock()
	if s.updN >= maxBufferedUpdates {
		s.updMu.Unlock()
		return // progress is expendable; the result is not
	}
	s.updates[execID] = append(s.updates[execID], wireUpdateJSON(u))
	s.updN++
	full := s.updN >= updateBatchSize
	s.updMu.Unlock()
	if full {
		select {
		case s.flushCh <- struct{}{}:
		default:
		}
	}
}

// flushLoop batches progress: a build that logs thousands of lines must not
// turn into thousands of requests.
func (s *sseSession) flushLoop() {
	t := time.NewTicker(updateFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			s.flushUpdates()
			return
		case <-s.flushCh:
		case <-t.C:
		}
		s.flushUpdates()
	}
}

func (s *sseSession) flushUpdates() {
	s.updMu.Lock()
	if s.updN == 0 {
		s.updMu.Unlock()
		return
	}
	batch := s.updates
	s.updates = map[string][]edgewire.Update{}
	s.updN = 0
	s.updMu.Unlock()

	for execID, ups := range batch {
		// Best effort by design: main replays nothing, and a lost log line
		// must never hold up the execution.
		_ = s.tr.post(execID, "updates", edgewire.UpdateBatch{Updates: ups})
	}
}

// ReportDone delivers a result and returns nil only on main's 200 — that
// response is the acknowledgement the agent needs before it can forget the
// execution. It retries briefly, then hands the result back to the agent's
// buffer rather than blocking other results behind it.
func (s *sseSession) ReportDone(execID string, d DoneReport) error {
	s.flushUpdates() // a deploy's log must not end after its own result

	body := edgewire.Done{OK: d.OK, Error: d.Error, Digest: d.Digest}
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < doneAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-s.tr.root.Done():
				return s.tr.root.Err()
			}
			backoff *= 2
		}
		if err := s.tr.post(execID, "done", body); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

// post sends one report. It runs on the transport's root context, not the
// session's: reporting has to work while the download stream is broken.
func (t *sseTransport) post(execID, kind string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(t.root, postTimeout)
	defer cancel()
	path := "/edge/executions/" + url.PathEscape(execID) + "/" + kind
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	detail := ""
	if resp.StatusCode != http.StatusOK {
		detail = errorDetail(resp)
	}
	drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: %s%s", path, resp.Status, detail)
	}
	return nil
}

func wireUpdateJSON(u Update) edgewire.Update {
	out := edgewire.Update{
		Index:    u.Index,
		Name:     u.Name,
		ExitCode: u.ExitCode,
		Error:    u.Error,
		Stream:   u.Stream,
		Data:     u.Data,
	}
	switch u.Kind {
	case UpdateOpStart:
		out.Kind = edgewire.UpdateOpStart
	case UpdateOpEnd:
		out.Kind = edgewire.UpdateOpEnd
	default:
		out.Kind = edgewire.UpdateLog
	}
	return out
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
}

// errorDetail extracts main's error message for a failed request, so the
// edge log says why rather than just showing a status code.
func errorDetail(resp *http.Response) string {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil || len(b) == 0 {
		return ""
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return " (" + e.Error + ")"
	}
	return " (" + strings.TrimSpace(string(b)) + ")"
}
