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

// EdgeInfo is the live state of one connected edge session.
type EdgeInfo struct {
	Server      string
	Version     string
	ConnectedAt time.Time
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
