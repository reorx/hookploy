// Package edgewire is the JSON wire format of the SSE edge protocol: what
// internal/pb is to the gRPC transport. Both main's adapter and the edge
// client encode against these types.
//
// It is deliberately separate from internal/api, whose DTOs are frozen as
// the public HTTP/CLI contract — this is an internal protocol between two
// halves of the same binary and evolves with them.
package edgewire

import "encoding/json"

// Event names on the session stream.
const (
	EventHello = "hello"
	EventExec  = "exec"
)

// Hello is the first frame of a session stream. HeartbeatSeconds tells the
// edge how often to expect a comment frame, so it can tell a quiet stream
// from a dead one.
type Hello struct {
	MainVersion      string `json:"main_version"`
	Server           string `json:"server"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
}

// Exec is one execution dispatched to the edge. Ops carries the op snapshot
// verbatim — main sends the same bytes it stored.
type Exec struct {
	ExecutionID string          `json:"execution_id"`
	Kind        string          `json:"kind,omitempty"`
	Service     string          `json:"service"`
	Instance    string          `json:"instance,omitempty"`
	Dir         string          `json:"dir,omitempty"`
	Image       string          `json:"image,omitempty"`
	Digest      string          `json:"digest,omitempty"`
	Ops         json.RawMessage `json:"ops"`
	TimeoutMS   int64           `json:"timeout_ms,omitempty"`
}

// Update kinds.
const (
	UpdateOpStart = "op_start"
	UpdateOpEnd   = "op_end"
	UpdateLog     = "log"
)

// Update is one progress event for an execution.
type Update struct {
	Kind     string `json:"kind"`
	Index    int    `json:"index"`
	Name     string `json:"name,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	Stream   string `json:"stream,omitempty"`
	Data     string `json:"data,omitempty"`
}

// UpdateBatch is the body of POST /edge/executions/{id}/updates.
type UpdateBatch struct {
	Updates []Update `json:"updates"`
}

// Done is the body of POST /edge/executions/{id}/done — an execution's
// terminal result. A 200 response is main's acknowledgement, which is what
// lets the edge stop holding on to it.
type Done struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Digest string `json:"digest,omitempty"`
}
