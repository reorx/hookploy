// Package api holds the response DTOs shared by the HTTP API and the CLI's
// --json output — both serialize exactly these types, so the two outputs are
// identical by construction. It also holds the DTOs of CLI-only local
// commands (version/validate/token), which have no HTTP counterpart but share
// the same freeze guarantee.
//
// M3 freezes this package: fields are add-only. New fields must be optional
// (omitempty); renaming, deleting or retyping an existing field is a breaking
// change and is forbidden. The NDJSON frame formats (LogLine, LogDone,
// FollowFrame) are under the same constraint.
package api

import (
	"encoding/json"
	"time"

	"github.com/reorx/hookploy/internal/model"
)

// Accepted is the 202 response of webhook/manual triggers.
type Accepted struct {
	DeployID  string `json:"deploy_id"`
	StatusURL string `json:"status_url"`
}

// Error is the uniform error body.
type Error struct {
	Error string `json:"error"`
}

// Deploy is one rollout.
type Deploy struct {
	ID         string          `json:"id"`
	Service    string          `json:"service"`
	Kind       string          `json:"kind"`
	Task       string          `json:"task,omitempty"`
	Status     string          `json:"status"`
	Digest     string          `json:"digest,omitempty"`
	Error      string          `json:"error,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Executions []Execution     `json:"executions,omitempty"`
}

// Execution is one per-instance run inside a deploy.
type Execution struct {
	ID         string     `json:"id"`
	Instance   string     `json:"instance"`
	Server     string     `json:"server"`
	Dir        string     `json:"dir"`
	Wave       int        `json:"wave"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Ops        []OpRecord `json:"ops,omitempty"`
}

// OpRecord is the timeline entry of one op.
type OpRecord struct {
	Index      int        `json:"index"`
	Name       string     `json:"name"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// LogLine is one log chunk (NDJSON in log streaming).
type LogLine struct {
	ExecutionID string    `json:"execution_id"`
	OpIndex     int       `json:"op_index"`
	Stream      string    `json:"stream"`
	Data        string    `json:"data"`
	At          time.Time `json:"at"`
}

// LogDone is the final NDJSON frame of a log follow stream, emitted once the
// deploy settles.
type LogDone struct {
	Done   bool   `json:"done"`
	Status string `json:"status"`
}

// FollowFrame is the consumer-side view of one NDJSON frame in a follow
// stream: either a LogLine or the terminal LogDone. Done tells them apart.
// Both are embedded rather than copied field by field, so a field added to
// either one is decodable here without a second edit.
type FollowFrame struct {
	LogLine
	LogDone
}

// ServiceSummary is one row of GET /services.
type ServiceSummary struct {
	Name       string   `json:"name"`
	Webhook    bool     `json:"webhook"`
	Servers    []string `json:"servers"`
	LastDeploy *Deploy  `json:"last_deploy,omitempty"`
}

// ServiceDetail is GET /services/{name}: the full normalized service
// definition. Deploy/Tasks steps use the ops wire format {"op":..., "args":...}
// — the same encoding as DB snapshots and gRPC dispatch.
type ServiceDetail struct {
	Name      string                       `json:"name"`
	Image     string                       `json:"image,omitempty"`
	Webhook   bool                         `json:"webhook"`
	Timeout   string                       `json:"timeout"` // Go duration string, e.g. "10m0s"
	Instances []InstanceInfo               `json:"instances"`
	Rollout   [][]string                   `json:"rollout"` // waves × instance names
	Deploy    []json.RawMessage            `json:"deploy"`
	Tasks     map[string][]json.RawMessage `json:"tasks,omitempty"`
}

// InstanceInfo is one deployment target inside ServiceDetail.
type InstanceInfo struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	Dir    string `json:"dir"`
}

// ServerInfo is one row of GET /servers.
type ServerInfo struct {
	Name        string     `json:"name"`
	Local       bool       `json:"local"`
	Status      string     `json:"status"`                 // online | offline
	Version     string     `json:"version,omitempty"`      // edge binary version (edges only)
	ConnectedAt *time.Time `json:"connected_at,omitempty"` // edge session start (edges only)
}

// Status is the `hookploy status` overview.
type Status struct {
	Servers  []ServerInfo     `json:"servers"`
	Services []ServiceSummary `json:"services"`
}

// VersionInfo is `hookploy version --json`.
type VersionInfo struct {
	Version string `json:"version"`
}

// ValidateResult is `hookploy validate --json`. On failure OK is false and
// Error carries the reason; the process still exits 1.
type ValidateResult struct {
	OK       bool   `json:"ok"`
	Servers  int    `json:"servers"`
	Services int    `json:"services"`
	Error    string `json:"error,omitempty"`
}

// TokenCreated is the --json form of the token create/rotate commands. Token
// is the plaintext secret — main never stores or prints it again, so this is
// the only chance to capture it.
type TokenCreated struct {
	Kind    string `json:"kind"` // service | server | admin
	Subject string `json:"subject"`
	Token   string `json:"token"`
}

// TokenRevoked is the --json form of `token revoke`. Revoked is false when
// the subject had no valid token left to revoke.
type TokenRevoked struct {
	Kind    string `json:"kind"` // service | server | admin
	Subject string `json:"subject"`
	Revoked bool   `json:"revoked"`
}

// FromDeploy converts a model deploy (optionally with executions/op records).
func FromDeploy(d *model.Deploy, execs []*model.Execution, opsByExec map[string][]*model.OpRecord) *Deploy {
	out := &Deploy{
		ID:         d.ID,
		Service:    d.Service,
		Kind:       string(d.Kind),
		Task:       d.Task,
		Status:     string(d.Status),
		Digest:     d.Digest,
		Error:      d.Error,
		Payload:    d.Payload,
		CreatedAt:  d.CreatedAt,
		FinishedAt: d.FinishedAt,
	}
	for _, ex := range execs {
		e := Execution{
			ID:         ex.ID,
			Instance:   ex.Instance,
			Server:     ex.Server,
			Dir:        ex.Dir,
			Wave:       ex.Wave,
			Status:     string(ex.Status),
			Error:      ex.Error,
			StartedAt:  ex.StartedAt,
			FinishedAt: ex.FinishedAt,
		}
		for _, op := range opsByExec[ex.ID] {
			e.Ops = append(e.Ops, OpRecord{
				Index:      op.OpIndex,
				Name:       op.OpName,
				StartedAt:  op.StartedAt,
				FinishedAt: op.FinishedAt,
				ExitCode:   op.ExitCode,
				Error:      op.Error,
			})
		}
		out.Executions = append(out.Executions, e)
	}
	return out
}

// FromLogLine converts a model log line.
func FromLogLine(l *model.LogLine) LogLine {
	return LogLine{
		ExecutionID: l.ExecutionID,
		OpIndex:     l.OpIndex,
		Stream:      l.Stream,
		Data:        l.Data,
		At:          l.At,
	}
}
