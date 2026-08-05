package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/reorx/hookploy/internal/edgehub"
	"github.com/reorx/hookploy/internal/edgewire"
	"github.com/reorx/hookploy/internal/engine"
	"github.com/reorx/hookploy/internal/token"
	"github.com/reorx/hookploy/internal/version"
)

const (
	// transportName tags SSE attachments in EdgeInfo and logs.
	transportName = "sse"
	// defaultEdgeHeartbeat keeps the stream visibly alive. Proxies in front
	// of main cut idle connections — Cloudflare at roughly 100s — and unlike
	// h2 PING frames a comment frame is real traffic that every proxy counts.
	defaultEdgeHeartbeat = 45 * time.Second
	// edgeWriteTimeout stops a client that stopped reading from pinning the
	// handler goroutine forever; the kick channel cannot interrupt a blocked
	// write, only a deadline can.
	edgeWriteTimeout = 30 * time.Second
	// edgeQueueDepth is how many executions may await the writer goroutine.
	edgeQueueDepth = 16
)

// serverAuth guards the edge endpoints with a server token. The server name
// comes from the token's subject, exactly as in the gRPC handshake; a
// ?server= value is only an assertion that must agree.
//
// It deliberately does not accept the web UI's session cookie: these
// endpoints are machine-to-machine and must not be reachable from a browser
// that merely happens to be logged in.
func (s *Server) serverAuth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		plain := bearerToken(r)
		if plain == "" {
			writeError(w, http.StatusUnauthorized, "missing token")
			return
		}
		rec, err := s.Store.LookupToken(token.Hash(plain))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		assert := r.URL.Query().Get("server")
		if rec == nil || rec.Kind != string(token.KindServer) ||
			(assert != "" && assert != rec.Subject) {
			writeError(w, http.StatusUnauthorized, "invalid server token")
			return
		}
		name := rec.Subject
		if s.Config().Servers[name] == nil {
			writeError(w, http.StatusForbidden,
				fmt.Sprintf("server %q is not declared in hookploy.yaml", name))
			return
		}
		next(w, r, name)
	}
}

func (s *Server) heartbeat() time.Duration {
	if s.EdgeHeartbeat > 0 {
		return s.EdgeHeartbeat
	}
	return defaultEdgeHeartbeat
}

// handleEdgeSession serves GET /edge/session: the downstream half of the SSE
// edge protocol. Results travel back over separate POSTs, so this stream
// carries only the hello frame, executions, and heartbeats.
func (s *Server) handleEdgeSession(w http.ResponseWriter, r *http.Request, server string) {
	hb := s.heartbeat()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // reverse proxies must not buffer this
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	hello := edgewire.Hello{
		MainVersion:      version.Version,
		Server:           server,
		HeartbeatSeconds: int(hb / time.Second),
	}
	if writeEvent(w, rc, edgewire.EventHello, hello) != nil {
		return
	}

	conn := &sseConn{execCh: make(chan edgewire.Exec, edgeQueueDepth), kick: make(chan struct{})}
	att := s.Hub.Attach(edgehub.AttachInfo{
		Server:    server,
		Version:   r.URL.Query().Get("version"),
		Transport: transportName,
		Inflight:  r.URL.Query()["inflight"],
	}, conn)
	defer att.Detach()

	// This goroutine is the only writer: a ResponseWriter is not safe for
	// concurrent use, so dispatch queues and heartbeats both funnel here.
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	for {
		select {
		case ev := <-conn.execCh:
			if writeEvent(w, rc, edgewire.EventExec, ev) != nil {
				return
			}
		case <-ticker.C:
			if writeHeartbeat(w, rc) != nil {
				return
			}
		case <-conn.kick:
			return // a newer session took this server over
		case <-r.Context().Done():
			return
		}
	}
}

// handleEdgeUpdates serves POST /edge/executions/{id}/updates: best-effort
// progress. Updates for executions main has already settled are dropped by
// the hub, so this answers 200 regardless.
func (s *Server) handleEdgeUpdates(w http.ResponseWriter, r *http.Request, server string) {
	var batch edgewire.UpdateBatch
	if !decodeEdgeBody(w, r, &batch) {
		return
	}
	execID := r.PathValue("id")
	for _, u := range batch.Updates {
		switch u.Kind {
		case edgewire.UpdateOpStart:
			s.Hub.HandleOpStart(server, execID, u.Index, u.Name)
		case edgewire.UpdateOpEnd:
			var opErr error
			if u.Error != "" {
				opErr = errors.New(u.Error)
			}
			s.Hub.HandleOpEnd(server, execID, u.Index, u.Name, u.ExitCode, opErr)
		case edgewire.UpdateLog:
			s.Hub.HandleLog(server, execID, u.Index, u.Stream, u.Data)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleEdgeDone serves POST /edge/executions/{id}/done. The 200 is the
// edge's acknowledgement that main has the result and it can stop holding
// it — which is why an unknown execution id also answers 200: main has
// simply moved on, and making the edge retry forever would help no one.
func (s *Server) handleEdgeDone(w http.ResponseWriter, r *http.Request, server string) {
	var done edgewire.Done
	if !decodeEdgeBody(w, r, &done) {
		return
	}
	s.Hub.HandleDone(server, r.PathValue("id"), done.OK, done.Error, done.Digest)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// decodeEdgeBody reads a ≤1MiB JSON body. A malformed body is a bug on the
// edge, not a transient condition, so it gets a 400 rather than an ack.
func decodeEdgeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "cannot read body: "+err.Error())
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		writeError(w, http.StatusBadRequest, "body is not valid JSON: "+err.Error())
		return false
	}
	return true
}

// sseConn is the hub's downstream view of one edge stream. It only ever
// queues: the handler goroutine owns the socket.
type sseConn struct {
	execCh chan edgewire.Exec
	kick   chan struct{}
	once   sync.Once
}

func (c *sseConn) SendExecution(spec engine.Spec) error {
	select {
	case <-c.kick:
		return errors.New("edge stream is closed")
	default:
	}
	opsJSON, err := json.Marshal(spec.Steps)
	if err != nil {
		return fmt.Errorf("marshal ops: %w", err)
	}
	ev := edgewire.Exec{
		ExecutionID: spec.ExecutionID,
		Kind:        spec.Kind,
		Service:     spec.Service,
		Instance:    spec.Instance,
		Dir:         spec.Dir,
		Image:       spec.Image,
		Digest:      spec.Digest,
		Ops:         opsJSON,
		TimeoutMS:   spec.Timeout.Milliseconds(),
	}
	select {
	case c.execCh <- ev:
		return nil
	default:
		return errors.New("edge stream is backed up")
	}
}

// Close makes the handler return, which ends the response.
func (c *sseConn) Close() {
	c.once.Do(func() { close(c.kick) })
}

func writeEvent(w http.ResponseWriter, rc *http.ResponseController, name string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// json.Marshal escapes newlines, so the payload is always one data line.
	return writeFrame(w, rc, fmt.Sprintf("event: %s\ndata: %s\n\n", name, body))
}

func writeHeartbeat(w http.ResponseWriter, rc *http.ResponseController) error {
	return writeFrame(w, rc, ": hb\n\n")
}

func writeFrame(w http.ResponseWriter, rc *http.ResponseController, frame string) error {
	// Best effort: some ResponseWriters have no deadline support, and there
	// the heartbeat write failing is still what detects a dead peer.
	_ = rc.SetWriteDeadline(time.Now().Add(edgeWriteTimeout))
	if _, err := io.WriteString(w, frame); err != nil {
		return err
	}
	return rc.Flush()
}
