// Package store is the SQLite persistence layer: tokens, deploys,
// executions, per-op records, logs, and an in-memory log broadcaster for
// ?follow=1 streaming.
package store

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const timeFmt = time.RFC3339Nano

// Store wraps the SQLite database plus the in-memory log broadcaster.
type Store struct {
	db *sql.DB
	bc *broadcaster

	mu          sync.Mutex
	execDeploys map[string]string // execution id → deploy id cache
}

// Open opens (creating if needed) the database and applies migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite serializes writes per connection; a single connection
	// avoids in-process SQLITE_BUSY at our scale.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{
		db:          db,
		bc:          newBroadcaster(),
		execDeploys: map[string]string{},
	}, nil
}

func (s *Store) Close() error { return s.db.Close() }

var migrations = []string{
	`
CREATE TABLE tokens (
	id          INTEGER PRIMARY KEY,
	kind        TEXT NOT NULL,
	subject     TEXT NOT NULL,
	hash        TEXT NOT NULL UNIQUE,
	created_at  TEXT NOT NULL,
	revoked_at  TEXT
);
CREATE TABLE deploys (
	id          TEXT PRIMARY KEY,
	service     TEXT NOT NULL,
	kind        TEXT NOT NULL,
	task        TEXT NOT NULL DEFAULT '',
	payload     TEXT,
	digest      TEXT NOT NULL DEFAULT '',
	status      TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	finished_at TEXT
);
CREATE INDEX idx_deploys_service ON deploys(service, created_at DESC);
CREATE TABLE executions (
	id          TEXT PRIMARY KEY,
	deploy_id   TEXT NOT NULL REFERENCES deploys(id) ON DELETE CASCADE,
	service     TEXT NOT NULL,
	instance    TEXT NOT NULL,
	server      TEXT NOT NULL,
	dir         TEXT NOT NULL,
	image       TEXT NOT NULL DEFAULT '',
	wave        INTEGER NOT NULL DEFAULT 0,
	ops_json    TEXT NOT NULL,
	timeout_ms  INTEGER NOT NULL,
	status      TEXT NOT NULL,
	error       TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	started_at  TEXT,
	finished_at TEXT
);
CREATE INDEX idx_exec_deploy ON executions(deploy_id);
CREATE TABLE execution_ops (
	execution_id TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
	op_index     INTEGER NOT NULL,
	op_name      TEXT NOT NULL,
	started_at   TEXT NOT NULL,
	finished_at  TEXT,
	exit_code    INTEGER,
	error        TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (execution_id, op_index)
);
CREATE TABLE op_logs (
	id           INTEGER PRIMARY KEY,
	execution_id TEXT NOT NULL REFERENCES executions(id) ON DELETE CASCADE,
	op_index     INTEGER NOT NULL,
	stream       TEXT NOT NULL,
	data         TEXT NOT NULL,
	at           TEXT NOT NULL
);
CREATE INDEX idx_op_logs_exec ON op_logs(execution_id, id);
`,
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		if _, err := db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}
	return nil
}

func now() string { return time.Now().UTC().Format(timeFmt) }

func fmtTime(t time.Time) string { return t.UTC().Format(timeFmt) }

func parseTime(s string) time.Time {
	t, _ := time.Parse(timeFmt, s)
	return t
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := parseTime(s.String)
	return &t
}
