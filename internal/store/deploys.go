package store

import (
	"database/sql"
	"encoding/json"

	"github.com/reorx/hookploy/internal/model"
)

// CreateDeploy inserts a deploy and its executions in one transaction.
func (s *Store) CreateDeploy(d *model.Deploy, execs []*model.Execution) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var finishedAt any
	if d.FinishedAt != nil {
		finishedAt = fmtTime(*d.FinishedAt)
	}
	if _, err := tx.Exec(
		`INSERT INTO deploys (id, service, kind, task, payload, digest, status, error, created_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Service, string(d.Kind), d.Task, string(d.Payload), d.Digest,
		string(d.Status), d.Error, fmtTime(d.CreatedAt), finishedAt); err != nil {
		return err
	}
	for _, ex := range execs {
		if _, err := tx.Exec(
			`INSERT INTO executions (id, deploy_id, service, instance, server, dir, image, wave,
			   ops_json, timeout_ms, status, error, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ex.ID, ex.DeployID, ex.Service, ex.Instance, ex.Server, ex.Dir, ex.Image, ex.Wave,
			string(ex.OpsJSON), int64(ex.Timeout)/1e6, string(ex.Status), ex.Error,
			fmtTime(ex.CreatedAt)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const deployCols = "id, service, kind, task, payload, digest, status, error, created_at, finished_at"

func scanDeploy(row interface{ Scan(...any) error }) (*model.Deploy, error) {
	var d model.Deploy
	var kind, status, createdAt, payload string
	var finishedAt sql.NullString
	err := row.Scan(&d.ID, &d.Service, &kind, &d.Task, &payload, &d.Digest,
		&status, &d.Error, &createdAt, &finishedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.Kind, d.Status = model.Kind(kind), model.Status(status)
	d.Payload = json.RawMessage(payload)
	d.CreatedAt = parseTime(createdAt)
	d.FinishedAt = parseTimePtr(finishedAt)
	return &d, nil
}

// GetDeploy returns a deploy by id, nil when absent.
func (s *Store) GetDeploy(id string) (*model.Deploy, error) {
	return scanDeploy(s.db.QueryRow("SELECT "+deployCols+" FROM deploys WHERE id = ?", id))
}

// ListDeploys returns the newest deploys of a service.
func (s *Store) ListDeploys(service string, limit int) ([]*model.Deploy, error) {
	rows, err := s.db.Query(
		"SELECT "+deployCols+" FROM deploys WHERE service = ? ORDER BY created_at DESC, rowid DESC LIMIT ?",
		service, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListRecentDeploys returns the newest deploys across all services.
func (s *Store) ListRecentDeploys(limit int) ([]*model.Deploy, error) {
	rows, err := s.db.Query(
		"SELECT "+deployCols+" FROM deploys ORDER BY created_at DESC, rowid DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LatestDeploys returns the most recent deploy per service.
func (s *Store) LatestDeploys() (map[string]*model.Deploy, error) {
	rows, err := s.db.Query(
		`SELECT ` + deployCols + ` FROM deploys d1
		 WHERE rowid = (SELECT rowid FROM deploys d2 WHERE d2.service = d1.service
		                ORDER BY created_at DESC, rowid DESC LIMIT 1)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.Deploy{}
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out[d.Service] = d
	}
	return out, rows.Err()
}

const execCols = "id, deploy_id, service, instance, server, dir, image, wave, ops_json, timeout_ms, status, error, created_at, started_at, finished_at"

func scanExecution(row interface{ Scan(...any) error }) (*model.Execution, error) {
	var ex model.Execution
	var status, createdAt, opsJSON string
	var startedAt, finishedAt sql.NullString
	var timeoutMS int64
	err := row.Scan(&ex.ID, &ex.DeployID, &ex.Service, &ex.Instance, &ex.Server, &ex.Dir,
		&ex.Image, &ex.Wave, &opsJSON, &timeoutMS, &status, &ex.Error, &createdAt, &startedAt, &finishedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ex.Status = model.Status(status)
	ex.OpsJSON = json.RawMessage(opsJSON)
	ex.Timeout = model.Duration(timeoutMS * 1e6)
	ex.CreatedAt = parseTime(createdAt)
	ex.StartedAt = parseTimePtr(startedAt)
	ex.FinishedAt = parseTimePtr(finishedAt)
	return &ex, nil
}

// GetExecution returns one execution by id, nil when absent.
func (s *Store) GetExecution(id string) (*model.Execution, error) {
	return scanExecution(s.db.QueryRow("SELECT "+execCols+" FROM executions WHERE id = ?", id))
}

// ListExecutions returns a deploy's executions in wave order.
func (s *Store) ListExecutions(deployID string) ([]*model.Execution, error) {
	rows, err := s.db.Query(
		"SELECT "+execCols+" FROM executions WHERE deploy_id = ? ORDER BY wave, rowid", deployID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Execution
	for rows.Next() {
		ex, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ex)
	}
	return out, rows.Err()
}

// TransitionExecution performs a guarded status transition: it only wins if
// the row is still in the expected `from` status. started_at/finished_at are
// stamped on entering running / a terminal status.
func (s *Store) TransitionExecution(id string, from, to model.Status, errMsg string) (bool, error) {
	var res sql.Result
	var err error
	switch {
	case to == model.StatusRunning:
		res, err = s.db.Exec(
			"UPDATE executions SET status = ?, started_at = COALESCE(started_at, ?) WHERE id = ? AND status = ?",
			string(to), now(), id, string(from))
	case to.Terminal():
		res, err = s.db.Exec(
			"UPDATE executions SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?",
			string(to), errMsg, now(), id, string(from))
	default:
		res, err = s.db.Exec(
			"UPDATE executions SET status = ? WHERE id = ? AND status = ?",
			string(to), id, string(from))
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// execStatuses returns the status of every execution of a deploy.
func (s *Store) execStatuses(deployID string) ([]model.Status, error) {
	rows, err := s.db.Query("SELECT status FROM executions WHERE deploy_id = ?", deployID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []model.Status
	for rows.Next() {
		var st string
		if err := rows.Scan(&st); err != nil {
			return nil, err
		}
		statuses = append(statuses, model.Status(st))
	}
	return statuses, rows.Err()
}

// RecomputeDeployStatus aggregates execution statuses into the deploy row
// and notifies followers when the deploy reaches a terminal state.
func (s *Store) RecomputeDeployStatus(deployID string) (model.Status, error) {
	statuses, err := s.execStatuses(deployID)
	if err != nil {
		return "", err
	}
	agg := model.AggregateStatus(statuses)
	// The aggregate goes failed as soon as any instance fails, so followers
	// see the bad news early — but the rollout is only over once every
	// execution has settled. Stamping finished_at or publishing Done on the
	// first failure would end the stream while later waves are still queued,
	// leaving followers with a finished deploy holding unsettled instances.
	if agg.Terminal() && model.AllTerminal(statuses) {
		if _, err := s.db.Exec(
			"UPDATE deploys SET status = ?, finished_at = COALESCE(finished_at, ?) WHERE id = ?",
			string(agg), now(), deployID); err != nil {
			return "", err
		}
		s.bc.publish(deployID, Event{Done: true, Status: agg})
	} else {
		if _, err := s.db.Exec("UPDATE deploys SET status = ? WHERE id = ?", string(agg), deployID); err != nil {
			return "", err
		}
	}
	return agg, nil
}

// MarkDeploySuperseded supersedes a still-queued deploy and its executions.
func (s *Store) MarkDeploySuperseded(deployID string) error {
	if _, err := s.db.Exec(
		"UPDATE executions SET status = ?, finished_at = ? WHERE deploy_id = ? AND status = ?",
		string(model.StatusSuperseded), now(), deployID, string(model.StatusQueued)); err != nil {
		return err
	}
	if _, err := s.db.Exec(
		"UPDATE deploys SET status = ?, finished_at = ? WHERE id = ? AND status = ?",
		string(model.StatusSuperseded), now(), deployID, string(model.StatusQueued)); err != nil {
		return err
	}
	s.bc.publish(deployID, Event{Done: true, Status: model.StatusSuperseded})
	return nil
}

// SetDeployDigest records the digest resolved at rollout level.
func (s *Store) SetDeployDigest(deployID, digest string) error {
	_, err := s.db.Exec("UPDATE deploys SET digest = ? WHERE id = ?", digest, deployID)
	return err
}

// SetDeployError records a deploy-level error message.
func (s *Store) SetDeployError(deployID, msg string) error {
	_, err := s.db.Exec("UPDATE deploys SET error = ? WHERE id = ?", msg, deployID)
	return err
}

// CleanupService deletes the oldest terminal deploys beyond `keep` per
// service; executions, op records and logs cascade away.
func (s *Store) CleanupService(service string, keep int) error {
	_, err := s.db.Exec(
		`DELETE FROM deploys WHERE service = ?
		   AND status IN ('succeeded','failed','superseded','unreachable','canceled')
		   AND id NOT IN (SELECT id FROM deploys WHERE service = ?
		                  ORDER BY created_at DESC, rowid DESC LIMIT ?)`,
		service, service, keep)
	return err
}

// RecoverInFlight closes out every rollout the previous process left
// unfinished (used at startup). Returns affected deploy ids.
//
// It sweeps by deploy rather than by in-flight execution: a crash can land
// between marking one wave failed and canceling the waves it gated, or
// between two waves, leaving a started rollout with no dispatching or running
// execution at all. Nothing else would ever close those — the scheduler only
// reschedules deploys that never started — so they would strand short of
// finished, and a `running` one would not even be cleaned up by retention.
//
// Deploys that never started are left alone: they still hold nothing but
// queued executions and are rescheduled wholesale by Scheduler.Recover.
func (s *Store) RecoverInFlight(reason string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT d.id FROM deploys d JOIN executions e ON e.deploy_id = d.id
		 WHERE d.finished_at IS NULL AND e.status IN ('queued','dispatching','running')
		   AND (d.status != 'queued' OR e.status IN ('dispatching','running'))`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		// Their executors died with the previous process.
		if _, err := s.db.Exec(
			`UPDATE executions SET status = 'failed', error = ?, finished_at = ?
			 WHERE deploy_id = ? AND status IN ('dispatching','running')`,
			reason, now(), id); err != nil {
			return nil, err
		}
		// Waves gated behind them will never be dispatched.
		if _, err := s.db.Exec(
			`UPDATE executions SET status = ?, error = ?, finished_at = ?
			 WHERE deploy_id = ? AND status = ?`,
			string(model.StatusCanceled), reason, now(), id, string(model.StatusQueued)); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// ListQueuedDeploys returns queued deploys, oldest first.
func (s *Store) ListQueuedDeploys() ([]*model.Deploy, error) {
	rows, err := s.db.Query(
		"SELECT " + deployCols + " FROM deploys WHERE status = 'queued' ORDER BY created_at, rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Deploy
	for rows.Next() {
		d, err := scanDeploy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// StartOp records the beginning of one op of an execution.
func (s *Store) StartOp(execID string, opIndex int, opName string) error {
	_, err := s.db.Exec(
		`INSERT INTO execution_ops (execution_id, op_index, op_name, started_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (execution_id, op_index) DO UPDATE SET started_at = excluded.started_at`,
		execID, opIndex, opName, now())
	return err
}

// FinishOp records the end of one op.
func (s *Store) FinishOp(execID string, opIndex int, exitCode *int, errMsg string) error {
	var ec any
	if exitCode != nil {
		ec = *exitCode
	}
	_, err := s.db.Exec(
		"UPDATE execution_ops SET finished_at = ?, exit_code = ?, error = ? WHERE execution_id = ? AND op_index = ?",
		now(), ec, errMsg, execID, opIndex)
	return err
}

// ListOpRecords returns the per-op timeline of an execution.
func (s *Store) ListOpRecords(execID string) ([]*model.OpRecord, error) {
	rows, err := s.db.Query(
		`SELECT execution_id, op_index, op_name, started_at, finished_at, exit_code, error
		 FROM execution_ops WHERE execution_id = ? ORDER BY op_index`, execID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.OpRecord
	for rows.Next() {
		var r model.OpRecord
		var startedAt string
		var finishedAt sql.NullString
		var exitCode sql.NullInt64
		if err := rows.Scan(&r.ExecutionID, &r.OpIndex, &r.OpName, &startedAt, &finishedAt, &exitCode, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt = parseTime(startedAt)
		r.FinishedAt = parseTimePtr(finishedAt)
		if exitCode.Valid {
			v := int(exitCode.Int64)
			r.ExitCode = &v
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
