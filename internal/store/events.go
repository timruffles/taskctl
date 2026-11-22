package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
)

const notificationSchema = `
CREATE TABLE IF NOT EXISTS events(
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 task_id INTEGER NOT NULL REFERENCES tasks(id),
 kind TEXT NOT NULL,
 message TEXT NOT NULL,
 data TEXT NOT NULL,
 created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_task_id ON events(task_id,id);
CREATE TABLE IF NOT EXISTS subscriptions(
 id TEXT PRIMARY KEY,
 task_id INTEGER NOT NULL REFERENCES tasks(id),
 session TEXT NOT NULL,
 resume_idle INTEGER NOT NULL DEFAULT 0,
 cursor INTEGER NOT NULL CHECK(cursor>=0),
 snapshot_pending INTEGER NOT NULL DEFAULT 1,
 queued_through INTEGER NOT NULL DEFAULT 0,
 queue_cursor INTEGER NOT NULL DEFAULT 0,
 queue_snapshot_pending INTEGER NOT NULL DEFAULT 1,
 delivery_token TEXT NOT NULL DEFAULT '',
 delivery_mode TEXT NOT NULL DEFAULT '',
 lease_until TEXT NOT NULL DEFAULT '',
 offered_through INTEGER NOT NULL DEFAULT 0,
 offered_snapshot INTEGER NOT NULL DEFAULT 0,
 last_ack_token TEXT NOT NULL DEFAULT '',
 last_ack_through INTEGER NOT NULL DEFAULT 0,
 last_offered_at TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 UNIQUE(task_id,session)
);
CREATE INDEX IF NOT EXISTS subscriptions_session ON subscriptions(session);
CREATE INDEX IF NOT EXISTS subscriptions_session_order ON subscriptions(session,last_offered_at,created_at,id);
CREATE TABLE IF NOT EXISTS inactivity_marks(
 task_id INTEGER PRIMARY KEY REFERENCES tasks(id),
 claim_id TEXT NOT NULL,
 activity_at TEXT NOT NULL,
 event_id INTEGER NOT NULL REFERENCES events(id),
 detected_at TEXT NOT NULL
);
`

const hookIndexSQL = `CREATE INDEX events_hook_task_id ON events(task_id,id) WHERE kind<>'progress' AND kind<>'note_deleted'`

func hookIndexCurrent(q Queryer) (bool, error) {
	var statement string
	err := q.QueryRow("SELECT sql FROM sqlite_schema WHERE type='index' AND name='events_hook_task_id'").Scan(&statement)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return statement == hookIndexSQL, err
}

func ensureHookIndex(tx *sql.Tx) error {
	current, err := hookIndexCurrent(tx)
	if err != nil || current {
		return err
	}
	if _, err := tx.Exec("DROP INDEX IF EXISTS events_hook_task_id"); err != nil {
		return err
	}
	_, err = tx.Exec(hookIndexSQL)
	return err
}

type Event struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"task_id"`
	Kind      string          `json:"kind"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	CreatedAt string          `json:"created_at"`
	Truncated bool            `json:"truncated,omitempty"`
}

type EventPage struct {
	Events []Event `json:"events"`
	Cursor int64   `json:"cursor"`
	More   bool    `json:"more"`
}

const descendantScope = `WITH RECURSIVE scope(id) AS (
 SELECT id FROM tasks WHERE id=?
 UNION SELECT t.id FROM tasks t JOIN scope s ON t.parent_id=s.id
) `

func eventLimit(limit int) (int, error) {
	if limit == 0 {
		return 20, nil
	}
	if limit < 1 || limit > 100 {
		return 0, failure("invalid_limit", "event limit must be between 1 and 100")
	}
	return limit, nil
}

const eventPageBytes int64 = 1024 * 1024

const selectedEvents = descendantScope + `, selected(id) AS MATERIALIZED (
 SELECT e.id FROM scope s CROSS JOIN events e
 WHERE e.id IN (SELECT id FROM events INDEXED BY events_task_id
  WHERE task_id=s.id AND id>? ORDER BY id LIMIT ?)
 ORDER BY e.id LIMIT ?
) `

const selectedHookEvents = descendantScope + `, selected(id) AS MATERIALIZED (
 SELECT e.id FROM scope s CROSS JOIN events e
 WHERE e.id IN (SELECT id FROM events INDEXED BY events_hook_task_id
  WHERE task_id=s.id AND kind<>'progress' AND kind<>'note_deleted' AND id>? ORDER BY id LIMIT ?)
 ORDER BY e.id LIMIT ?
) `

func readEventPage(q Queryer, id, after int64, limit int) (EventPage, error) {
	return readEvents(q, id, after, limit, false)
}

func readEvents(q Queryer, id, after int64, limit int, summary bool) (EventPage, error) {
	return readSelectedEvents(q, id, after, limit, summary, selectedEvents)
}

func readHookEvents(q Queryer, id, after int64, limit int) (EventPage, error) {
	return readSelectedEvents(q, id, after, limit, true, selectedHookEvents)
}

func readSelectedEvents(q Queryer, id, after int64, limit int, summary bool, selection string) (EventPage, error) {
	page := EventPage{Events: []Event{}, Cursor: after}
	ids := make([]any, 0, limit)
	if !summary {
		rows, err := q.Query(selection+`SELECT e.id,
 octet_length(e.kind)+octet_length(e.message)+octet_length(e.data)+octet_length(e.created_at)+64
 FROM selected s JOIN events e ON e.id=s.id ORDER BY e.id`, id, after, limit+1, limit+1)
		if err != nil {
			return page, err
		}
		var size int64
		for rows.Next() {
			var eventID, bytes int64
			if err := rows.Scan(&eventID, &bytes); err != nil {
				rows.Close()
				return page, err
			}
			if len(ids) == limit || len(ids) > 0 && size+bytes > eventPageBytes {
				page.More = true
				break
			}
			ids = append(ids, eventID)
			size += bytes
		}
		err = rows.Err()
		rows.Close()
		if err != nil || len(ids) == 0 {
			return page, err
		}
	}
	query := `SELECT id,task_id,kind,message,data,created_at,0 FROM events WHERE id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `) ORDER BY id`
	args := ids
	if summary {
		query = selection + `SELECT e.id,e.task_id,substr(e.kind,1,64),substr(e.message,1,1024),'null',e.created_at,
 octet_length(e.kind)>octet_length(substr(e.kind,1,64)) OR octet_length(e.message)>octet_length(substr(e.message,1,1024)) FROM selected s JOIN events e ON e.id=s.id ORDER BY e.id`
		args = []any{id, after, limit + 1, limit + 1}
	}
	rows, err := q.Query(query, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Events) == limit {
			page.More = true
			break
		}
		var event Event
		var task int64
		var data string
		if err := rows.Scan(&event.ID, &task, &event.Kind, &event.Message, &data, &event.CreatedAt, &event.Truncated); err != nil {
			return page, err
		}
		event.TaskID = taskID(task)
		event.Kind = publicEvolutionName(event.Kind)
		event.Data = json.RawMessage(data)
		page.Events = append(page.Events, event)
		page.Cursor = event.ID
	}
	return page, rows.Err()
}

func (s *Store) Events(id string, after int64, limit int) (EventPage, error) {
	n, err := taskNumber(id)
	if err != nil {
		return EventPage{}, err
	}
	if after < 0 {
		return EventPage{}, failure("invalid_cursor", "event cursor must be nonnegative")
	}
	limit, err = eventLimit(limit)
	if err != nil {
		return EventPage{}, err
	}
	var page EventPage
	err = s.read(func(tx Queryer) error {
		var exists bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)`, n).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return failure("not_found", "task "+taskID(n)+" does not exist")
		}
		var latest int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(id),0) FROM events`).Scan(&latest); err != nil {
			return err
		}
		if after > latest {
			return failure("invalid_cursor", "event cursor is ahead of this database; after restoring a backup, start a new watch without --after")
		}
		var err error
		page, err = readEventPage(tx, n, after, limit)
		return err
	})
	return page, err
}

func validateSession(session string) error {
	if strings.TrimSpace(session) == "" || len(session) > 1024 || strings.ContainsFunc(session, unicode.IsControl) {
		return failure("invalid_session", "session must be nonempty, single line, and at most 1024 bytes")
	}
	return nil
}

func (s *Store) TouchOwned(session string) (int, error) {
	if err := validateSession(session); err != nil {
		return 0, err
	}
	count := 0
	err := s.write(func(tx *sql.Tx) error {
		result, err := tx.Exec(`UPDATE tasks SET last_activity_at=? WHERE owner=? AND status='in_progress'`, timestamp(s.Now()), session)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		count = int(n)
		return err
	})
	return count, err
}

func (s *Store) DetectInactivity(age time.Duration) (int, error) {
	if age <= 0 {
		return 0, failure("invalid_duration", "inactivity age must be positive")
	}
	count := 0
	err := s.write(func(tx *sql.Tx) error {
		now := s.Now()
		var after int64
		type inactive struct {
			id                     int64
			owner, claim, activity string
		}
		for {
			rows, err := tx.Query(`SELECT t.id,t.owner,t.claim_id,t.last_activity_at
 FROM tasks t LEFT JOIN inactivity_marks m ON m.task_id=t.id
 WHERE t.status='in_progress' AND t.last_activity_at<=? AND t.id>?
 AND (m.task_id IS NULL OR m.claim_id<>t.claim_id OR m.activity_at<>t.last_activity_at)
 ORDER BY t.id LIMIT 128`, timestamp(now.Add(-age)), after)
			if err != nil {
				return err
			}
			tasks := make([]inactive, 0, 128)
			for rows.Next() {
				var task inactive
				if err := rows.Scan(&task.id, &task.owner, &task.claim, &task.activity); err != nil {
					rows.Close()
					return err
				}
				tasks = append(tasks, task)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if len(tasks) == 0 {
				return nil
			}
			for _, task := range tasks {
				event, err := s.emit(tx, task.id, "inactive", "No activity since "+task.activity, map[string]string{
					"owner": task.owner, "claim_id": task.claim, "last_activity_at": task.activity,
				})
				if err != nil {
					return err
				}
				_, err = tx.Exec(`INSERT INTO inactivity_marks(task_id,claim_id,activity_at,event_id,detected_at)
 VALUES(?,?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET claim_id=excluded.claim_id,
 activity_at=excluded.activity_at,event_id=excluded.event_id,detected_at=excluded.detected_at`,
					task.id, task.claim, task.activity, event, timestamp(now))
				if err != nil {
					return err
				}
				count++
				after = task.id
			}
		}
	})
	return count, err
}
