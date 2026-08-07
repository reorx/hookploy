// Package model holds pure domain types shared by every other package.
// It must not import any other hookploy package.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
)

// Status is the lifecycle state of a Deploy or an Execution.
type Status string

const (
	StatusQueued      Status = "queued"
	StatusDispatching Status = "dispatching"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusSuperseded  Status = "superseded"
	StatusUnreachable Status = "unreachable"
	// StatusCanceled marks executions of later waves after an earlier wave failed.
	StatusCanceled Status = "canceled"
)

// Terminal reports whether no further transition can happen from s.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusSuperseded, StatusUnreachable, StatusCanceled:
		return true
	}
	return false
}

// Kind distinguishes what an Execution runs.
type Kind string

const (
	KindDeploy Kind = "deploy"
	KindTask   Kind = "task"
)

// Deploy is one webhook/manual trigger: a rollout containing N executions.
type Deploy struct {
	ID         string
	Service    string
	Kind       Kind
	Task       string // task name when Kind == KindTask
	Payload    json.RawMessage
	Digest     string // resolved image digest for the whole rollout
	Status     Status
	Error      string
	CreatedAt  time.Time
	FinishedAt *time.Time
}

// Execution is one per-instance run of the op pipeline. Ops is the
// interpolated snapshot taken at enqueue time (the exact message an edge
// would receive in M2); config reloads never affect in-flight executions.
type Execution struct {
	ID         string
	DeployID   string
	Service    string
	Instance   string
	Server     string
	Dir        string
	Image      string // service image declaration, snapshotted for image.* ops
	Wave       int    // 0-based wave index
	OpsJSON    json.RawMessage
	Timeout    Duration
	Status     Status
	Error      string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// OpRecord is the per-op timeline of an execution.
type OpRecord struct {
	ExecutionID string
	OpIndex     int
	OpName      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	ExitCode    *int
	Error       string
}

// LogLine is one chunk of op output.
type LogLine struct {
	ID          int64 // DB rowid, monotonic; used to dedupe follow replay
	ExecutionID string
	OpIndex     int
	Stream      string // stdout | stderr | system
	Data        string
	At          time.Time
}

// AggregateStatus derives a deploy's status from its executions' statuses.
//
// Rules: identical statuses aggregate to themselves; any bad terminal
// (failed/unreachable/canceled) mixed with anything else means failed;
// otherwise any activity means running; a queued/succeeded mix (waves not
// yet dispatched) is also running.
func AggregateStatus(statuses []Status) Status {
	if len(statuses) == 0 {
		return StatusQueued
	}
	same := true
	for _, s := range statuses[1:] {
		if s != statuses[0] {
			same = false
			break
		}
	}
	if same {
		return statuses[0]
	}
	for _, s := range statuses {
		if s == StatusFailed || s == StatusUnreachable || s == StatusCanceled {
			return StatusFailed
		}
	}
	return StatusRunning
}

// AllTerminal reports whether every execution has settled. A deploy's
// aggregate can read failed while instances are still moving (a wave-1
// failure surfaces before the later waves are canceled), so callers use this
// to tell "something already failed" from "the rollout is over".
func AllTerminal(statuses []Status) bool {
	for _, s := range statuses {
		if !s.Terminal() {
			return false
		}
	}
	return true
}

// EventKind names one notification-worthy occurrence in the installation's
// life. The vocabulary is deliberately wider than Status: EventDeployRecovered
// names a *transition* (this run succeeded, the one before it did not) that
// no single status captures, and the node kinds do not describe a deploy at
// all.
//
// It lives here rather than in internal/notify because internal/config has
// to validate hookploy.yaml's notify.events lists, and config cannot import
// notify — notify holds a func() *config.Config closure. model owns the
// vocabulary; internal/notify owns the rules that resolve one from a
// settled deploy or from a node changing state.
type EventKind string

const (
	EventDeployFailed      EventKind = "deploy.failed"
	EventDeploySucceeded   EventKind = "deploy.succeeded"
	EventDeployRecovered   EventKind = "deploy.recovered"
	EventDeployUnreachable EventKind = "deploy.unreachable"
	EventMainStarted       EventKind = "main.started"
	EventEdgeOffline       EventKind = "edge.offline"
	EventEdgeOnline        EventKind = "edge.online"
)

// EventScope tells a deploy outcome apart from a node's own comings and
// goings. A service can only ask about its own deploys — main restarting or
// an edge dropping off the network is a fact about the installation, not
// about any one service — so a service's notify.events may only name
// ScopeDeploy kinds, and node kinds are matched against the global list
// instead.
type EventScope string

const (
	ScopeDeploy EventScope = "deploy"
	ScopeNode   EventScope = "node"
)

// EventKinds is the complete vocabulary in a stable order. Config validation
// and the JSON Schema both derive from it, so a new kind is added once.
func EventKinds() []EventKind {
	return []EventKind{
		EventDeployFailed, EventDeploySucceeded, EventDeployRecovered, EventDeployUnreachable,
		EventMainStarted, EventEdgeOffline, EventEdgeOnline,
	}
}

// Scope reports which half of the vocabulary k belongs to. It doubles as the
// discriminator for notify.Event's payload: ScopeDeploy fills its deploy
// half, ScopeNode its node half.
func (k EventKind) Scope() EventScope {
	switch k {
	case EventMainStarted, EventEdgeOffline, EventEdgeOnline:
		return ScopeNode
	default:
		return ScopeDeploy
	}
}

// Valid reports whether k is one of EventKinds.
func (k EventKind) Valid() bool {
	for _, v := range EventKinds() {
		if v == k {
			return true
		}
	}
	return false
}

// DefaultEventKinds is what notify.events falls back to when omitted:
// deploy failures, plus every node kind. The node kinds are cheap — one
// message per main restart, one per edge outage — and an install that turned
// notifications on almost certainly wants to hear that half of its fleet
// went dark. The remaining deploy kinds stay opt-in because they fire on
// every green deploy.
func DefaultEventKinds() []EventKind {
	return []EventKind{EventDeployFailed, EventMainStarted, EventEdgeOffline, EventEdgeOnline}
}

// EdgeInfo is the live state of one connected edge session.
type EdgeInfo struct {
	Server      string
	Version     string
	Transport   string // grpc | sse
	ConnectedAt time.Time
}

// WorkflowRun is one GitHub Actions run received via the workflow_run
// webhook. ID is GitHub's run id and the upsert key; UpdatedAt guards
// against out-of-order deliveries.
type WorkflowRun struct {
	ID           int64
	Repo         string // repository.full_name, owner/repo
	WorkflowName string
	RunNumber    int
	Status       string // queued | in_progress | completed | waiting | pending | requested
	Conclusion   string // success | failure | cancelled | ...; empty until completed
	HeadBranch   string
	HeadSHA      string
	HTMLURL      string
	Event        string // triggering event: push, pull_request, ...
	Actor        string
	DisplayTitle string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	RunStartedAt *time.Time // nil while the run is still queued
	ReceivedAt   time.Time
}

// NewDeployID returns a fresh dp_ id.
func NewDeployID() string { return newID("dp_") }

// NewExecutionID returns a fresh ex_ id.
func NewExecutionID() string { return newID("ex_") }

func newID(prefix string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is not recoverable
	}
	return prefix + strconv.FormatInt(time.Now().UnixMilli(), 16) + hex.EncodeToString(b[:])
}

// Duration marshals as a human-readable string ("10m", "3s") in YAML and JSON.
type Duration time.Duration

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) String() string { return time.Duration(d).String() }
