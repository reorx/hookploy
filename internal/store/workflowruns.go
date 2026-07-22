package store

import (
	"database/sql"

	"github.com/reorx/hookploy/internal/model"
)

const workflowRunCols = "id, repo, workflow_name, run_number, status, conclusion, head_branch, head_sha, html_url, event, actor, display_title, created_at, updated_at, run_started_at, received_at"

func scanWorkflowRun(row interface{ Scan(...any) error }) (*model.WorkflowRun, error) {
	var wr model.WorkflowRun
	var createdAt, updatedAt, receivedAt string
	var runStartedAt sql.NullString
	err := row.Scan(&wr.ID, &wr.Repo, &wr.WorkflowName, &wr.RunNumber, &wr.Status,
		&wr.Conclusion, &wr.HeadBranch, &wr.HeadSHA, &wr.HTMLURL, &wr.Event,
		&wr.Actor, &wr.DisplayTitle, &createdAt, &updatedAt, &runStartedAt, &receivedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	wr.CreatedAt = parseTime(createdAt)
	wr.UpdatedAt = parseTime(updatedAt)
	wr.RunStartedAt = parseTimePtr(runStartedAt)
	wr.ReceivedAt = parseTime(receivedAt)
	return &wr, nil
}

// UpsertWorkflowRun inserts or updates a run keyed by GitHub's run id.
// Deliveries can arrive out of order, so an update only wins if its
// updated_at is not older than the stored one. The TEXT comparison is sound
// because GitHub timestamps have whole-second precision and fmtTime renders
// them without a fractional part, making lexicographic order time order.
func (s *Store) UpsertWorkflowRun(wr *model.WorkflowRun) error {
	var runStartedAt any
	if wr.RunStartedAt != nil {
		runStartedAt = fmtTime(*wr.RunStartedAt)
	}
	_, err := s.db.Exec(
		`INSERT INTO workflow_runs (`+workflowRunCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   repo = excluded.repo, workflow_name = excluded.workflow_name,
		   run_number = excluded.run_number, status = excluded.status,
		   conclusion = excluded.conclusion, head_branch = excluded.head_branch,
		   head_sha = excluded.head_sha, html_url = excluded.html_url,
		   event = excluded.event, actor = excluded.actor,
		   display_title = excluded.display_title, created_at = excluded.created_at,
		   updated_at = excluded.updated_at,
		   run_started_at = COALESCE(excluded.run_started_at, workflow_runs.run_started_at),
		   received_at = excluded.received_at
		 WHERE excluded.updated_at >= workflow_runs.updated_at`,
		wr.ID, wr.Repo, wr.WorkflowName, wr.RunNumber, wr.Status, wr.Conclusion,
		wr.HeadBranch, wr.HeadSHA, wr.HTMLURL, wr.Event, wr.Actor, wr.DisplayTitle,
		fmtTime(wr.CreatedAt), fmtTime(wr.UpdatedAt), runStartedAt, fmtTime(wr.ReceivedAt))
	return err
}

// ListWorkflowRuns returns the newest runs, filtered by repo when non-empty
// (case-insensitive via the column collation).
func (s *Store) ListWorkflowRuns(repo string, limit int) ([]*model.WorkflowRun, error) {
	query := "SELECT " + workflowRunCols + " FROM workflow_runs"
	args := []any{}
	if repo != "" {
		query += " WHERE repo = ?"
		args = append(args, repo)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.WorkflowRun
	for rows.Next() {
		wr, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wr)
	}
	return out, rows.Err()
}

// ListActiveWorkflowRuns returns every run that has not completed yet
// (queued, in_progress, waiting, pending, requested), newest first.
func (s *Store) ListActiveWorkflowRuns() ([]*model.WorkflowRun, error) {
	rows, err := s.db.Query(
		"SELECT " + workflowRunCols + " FROM workflow_runs WHERE status != 'completed' ORDER BY created_at DESC, id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.WorkflowRun
	for rows.Next() {
		wr, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, wr)
	}
	return out, rows.Err()
}

// CleanupWorkflowRuns deletes a repo's oldest runs beyond `keep`.
func (s *Store) CleanupWorkflowRuns(repo string, keep int) error {
	_, err := s.db.Exec(
		`DELETE FROM workflow_runs WHERE repo = ?
		   AND id NOT IN (SELECT id FROM workflow_runs WHERE repo = ?
		                  ORDER BY created_at DESC, id DESC LIMIT ?)`,
		repo, repo, keep)
	return err
}
