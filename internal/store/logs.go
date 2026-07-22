package store

import (
	"database/sql"
	"errors"
	"sync"

	"github.com/reorx/hookploy/internal/model"
)

// Event is what a deploy follower receives: a log line, or a terminal
// status notification (Done).
type Event struct {
	Line   *model.LogLine
	Done   bool
	Status model.Status
}

// AppendLog persists a log line and pushes it to followers of its deploy.
// line.ID is filled with the DB rowid.
func (s *Store) AppendLog(line *model.LogLine) error {
	res, err := s.db.Exec(
		"INSERT INTO op_logs (execution_id, op_index, stream, data, at) VALUES (?, ?, ?, ?, ?)",
		line.ExecutionID, line.OpIndex, line.Stream, line.Data, fmtTime(line.At))
	if err != nil {
		return err
	}
	line.ID, _ = res.LastInsertId()
	deployID, err := s.deployOf(line.ExecutionID)
	if err != nil {
		return err
	}
	s.bc.publish(deployID, Event{Line: line})
	return nil
}

func (s *Store) deployOf(execID string) (string, error) {
	s.mu.Lock()
	if id, ok := s.execDeploys[execID]; ok {
		s.mu.Unlock()
		return id, nil
	}
	s.mu.Unlock()
	var id string
	if err := s.db.QueryRow("SELECT deploy_id FROM executions WHERE id = ?", execID).Scan(&id); err != nil {
		return "", err
	}
	s.mu.Lock()
	s.execDeploys[execID] = id
	s.mu.Unlock()
	return id, nil
}

// GetDeployLogs returns all persisted log lines of a deploy in order.
func (s *Store) GetDeployLogs(deployID string) ([]*model.LogLine, error) {
	rows, err := s.db.Query(
		`SELECT l.id, l.execution_id, l.op_index, l.stream, l.data, l.at
		 FROM op_logs l JOIN executions e ON l.execution_id = e.id
		 WHERE e.deploy_id = ? ORDER BY l.id`, deployID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.LogLine
	for rows.Next() {
		var l model.LogLine
		var at string
		if err := rows.Scan(&l.ID, &l.ExecutionID, &l.OpIndex, &l.Stream, &l.Data, &at); err != nil {
			return nil, err
		}
		l.At = parseTime(at)
		out = append(out, &l)
	}
	return out, rows.Err()
}

// FollowDeploy streams a deploy's logs: first the persisted replay, then
// live lines, ending with a Done event when the deploy reaches a terminal
// state (or immediately after replay if it already has). The returned cancel
// must be called when the consumer stops early.
func (s *Store) FollowDeploy(deployID string) (<-chan Event, func(), error) {
	d, err := s.GetDeploy(deployID)
	if err != nil {
		return nil, nil, err
	}
	if d == nil {
		return nil, nil, errors.New("deploy not found: " + deployID)
	}

	// Subscribe before replaying so no line can fall between the two;
	// replayed IDs are skipped from the live feed.
	sub := s.bc.subscribe(deployID)
	replay, err := s.GetDeployLogs(deployID)
	if err != nil {
		s.bc.unsubscribe(deployID, sub)
		return nil, nil, err
	}

	out := make(chan Event, 64)
	quit := make(chan struct{})
	var once sync.Once
	cancel := func() { once.Do(func() { close(quit) }) }

	go func() {
		defer close(out)
		defer s.bc.unsubscribe(deployID, sub)
		send := func(ev Event) bool {
			select {
			case out <- ev:
				return true
			case <-quit:
				return false
			}
		}
		var maxReplayed int64
		for _, l := range replay {
			if !send(Event{Line: l}) {
				return
			}
			maxReplayed = l.ID
		}
		// Re-read status after subscribing: the deploy may have finished
		// between GetDeploy and subscribe. A terminal status alone is not
		// enough — it reads failed from the first dead instance onward, while
		// its siblings keep running and logging, so settle on the executions.
		cur, err := s.GetDeploy(deployID)
		if err == nil && cur != nil && cur.Status.Terminal() {
			if settled, serr := s.DeploySettled(deployID); serr == nil && settled {
				send(Event{Done: true, Status: cur.Status})
				return
			}
		}
		for {
			select {
			case ev, ok := <-sub:
				if !ok {
					return
				}
				if ev.Line != nil && ev.Line.ID <= maxReplayed {
					continue
				}
				if !send(ev) {
					return
				}
				if ev.Done {
					return
				}
			case <-quit:
				return
			}
		}
	}()
	return out, cancel, nil
}

// broadcaster fan-outs per-deploy events to in-process subscribers.
type broadcaster struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{}
}

func newBroadcaster() *broadcaster {
	return &broadcaster{subs: map[string]map[chan Event]struct{}{}}
}

func (b *broadcaster) subscribe(key string) chan Event {
	ch := make(chan Event, 1024)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[key] == nil {
		b.subs[key] = map[chan Event]struct{}{}
	}
	b.subs[key][ch] = struct{}{}
	return ch
}

func (b *broadcaster) unsubscribe(key string, ch chan Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if set, ok := b.subs[key]; ok {
		if _, ok := set[ch]; ok {
			delete(set, ch)
			close(ch)
		}
		if len(set) == 0 {
			delete(b.subs, key)
		}
	}
}

func (b *broadcaster) publish(key string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[key] {
		select {
		case ch <- ev:
		default: // slow consumer: drop rather than block the writer
		}
	}
}

var _ = sql.ErrNoRows
