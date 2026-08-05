package edge

import (
	"context"
	"time"
)

// Task is one execution as the agent sees it — the transport-neutral form of
// whatever the wire delivered.
type Task struct {
	ExecutionID string
	Kind        string
	Service     string
	Instance    string
	Dir         string
	Image       string
	Digest      string
	OpsJSON     []byte
	Timeout     time.Duration
}

// UpdateKind discriminates the progress events an execution emits.
type UpdateKind string

const (
	UpdateOpStart UpdateKind = "op_start"
	UpdateOpEnd   UpdateKind = "op_end"
	UpdateLog     UpdateKind = "log"
)

// Update is one progress event for an execution.
type Update struct {
	Kind     UpdateKind
	Index    int
	Name     string // op name (op_start / op_end)
	ExitCode *int   // op_end
	Error    string // op_end
	Stream   string // log: stdout | stderr
	Data     string // log
}

// DoneReport is an execution's terminal result.
type DoneReport struct {
	OK     bool
	Error  string
	Digest string
}

// Transport dials main. inflight advertises the executions this edge still
// holds, so a resumable transport can re-adopt them; transports that cannot
// resume ignore it.
type Transport interface {
	Dial(ctx context.Context, inflight []string) (Session, error)
}

// Session is one established connection to main.
type Session interface {
	// Recv blocks until main sends the next task, or the session dies.
	Recv() (*Task, error)
	// ReportUpdate is best-effort: progress lost on a dying session is not
	// worth failing an execution over.
	ReportUpdate(execID string, u Update)
	// ReportDone returns nil only once main has taken the result, which is
	// what lets the agent drop it from its buffer.
	ReportDone(execID string, d DoneReport) error
	// Resumable reports whether executions may outlive this session and be
	// reported over a later one.
	Resumable() bool
	MainVersion() string
	Server() string
	Close()
}
