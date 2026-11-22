package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Subscription struct {
	ID                   string `json:"id"`
	TaskID               string `json:"task_id"`
	Session              string `json:"session"`
	ResumeIdle           bool   `json:"resume_idle"`
	Cursor               int64  `json:"cursor"`
	SnapshotPending      bool   `json:"snapshot_pending"`
	QueuedThrough        int64  `json:"queued_through"`
	QueueCursor          int64  `json:"queue_cursor"`
	QueueSnapshotPending bool   `json:"queue_snapshot_pending"`
	LeaseUntil           string `json:"lease_until,omitempty"`
	LastOfferedAt        string `json:"last_offered_at,omitempty"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
}

type TaskState struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	Available bool   `json:"available"`
	Owner     string `json:"owner,omitempty"`
	Progress  string `json:"progress,omitempty"`
	Result    string `json:"result,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

type Snapshot struct {
	TaskID    string      `json:"task_id"`
	Outcome   string      `json:"outcome"`
	Available bool        `json:"available"`
	Tasks     []TaskState `json:"tasks"`
	Omitted   int         `json:"omitted"`
	Truncated bool        `json:"truncated,omitempty"`
}

type Delivery struct {
	Subscription   Subscription `json:"subscription"`
	Token          string       `json:"token,omitempty"`
	Mode           string       `json:"mode,omitempty"`
	Through        int64        `json:"through"`
	Events         []Event      `json:"events"`
	LatestProgress []Event      `json:"latest_progress,omitempty"`
	Snapshot       *Snapshot    `json:"snapshot,omitempty"`
	More           bool         `json:"more"`
	LeaseUntil     string       `json:"lease_until,omitempty"`
}

type subscriptionRecord struct {
	Subscription
	target          int64
	token           string
	mode            string
	offeredThrough  int64
	offeredSnapshot bool
	lastAckToken    string
	lastAckThrough  int64
}

const subscriptionColumns = `id,task_id,session,resume_idle,cursor,snapshot_pending,queued_through,queue_cursor,queue_snapshot_pending,
 lease_until,last_offered_at,created_at,updated_at,delivery_token,delivery_mode,offered_through,
 offered_snapshot,last_ack_token,last_ack_through`

type rowScanner interface{ Scan(...any) error }

func scanSubscription(row rowScanner) (subscriptionRecord, error) {
	var record subscriptionRecord
	err := row.Scan(&record.ID, &record.target, &record.Session, &record.ResumeIdle,
		&record.Cursor, &record.SnapshotPending, &record.QueuedThrough, &record.QueueCursor, &record.QueueSnapshotPending, &record.LeaseUntil, &record.LastOfferedAt,
		&record.CreatedAt, &record.UpdatedAt, &record.token, &record.mode, &record.offeredThrough,
		&record.offeredSnapshot, &record.lastAckToken, &record.lastAckThrough)
	record.TaskID = taskID(record.target)
	return record, err
}

func loadSubscription(q Queryer, id string) (subscriptionRecord, error) {
	record, err := scanSubscription(q.QueryRow(`SELECT `+subscriptionColumns+` FROM subscriptions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return record, failure("not_found", "subscription not found")
	}
	return record, err
}

func subscriptionOwner(record subscriptionRecord, session string) error {
	if record.Session != session {
		return failure("conflict", "subscription belongs to a different session")
	}
	return nil
}

func (s *Store) Subscribe(id, session string, resumeIdle bool) (Subscription, error) {
	n, err := taskNumber(id)
	if err != nil {
		return Subscription{}, err
	}
	if err := validateSession(session); err != nil {
		return Subscription{}, err
	}
	var result Subscription
	err = s.write(func(tx *sql.Tx) error {
		if _, err := getTask(tx, n); err != nil {
			return err
		}
		record, err := scanSubscription(tx.QueryRow(`SELECT `+subscriptionColumns+` FROM subscriptions WHERE task_id=? AND session=?`, n, session))
		if err == nil {
			if record.ResumeIdle != resumeIdle {
				record.ResumeIdle = resumeIdle
				record.UpdatedAt = timestamp(s.Now())
				if _, err := tx.Exec(`UPDATE subscriptions SET resume_idle=?,updated_at=? WHERE id=?`, resumeIdle, record.UpdatedAt, record.ID); err != nil {
					return err
				}
			}
			result = record.Subscription
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var cursor int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(id),0) FROM events`).Scan(&cursor); err != nil {
			return err
		}
		now := timestamp(s.Now())
		result = Subscription{ID: newID("S"), TaskID: id, Session: session, ResumeIdle: resumeIdle,
			Cursor: cursor, QueueCursor: cursor, SnapshotPending: true, QueueSnapshotPending: true, CreatedAt: now, UpdatedAt: now}
		_, err = tx.Exec(`INSERT INTO subscriptions(id,task_id,session,resume_idle,cursor,queue_cursor,created_at,updated_at)
 VALUES(?,?,?,?,?,?,?,?)`, result.ID, n, session, resumeIdle, cursor, cursor, now, now)
		return err
	})
	return result, err
}

func (s *Store) SubscriptionsPage(session string, limit, offset int) ([]Subscription, bool, error) {
	if session != "" {
		if err := validateSession(session); err != nil {
			return nil, false, err
		}
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, false, failure("invalid_limit", "subscription limit must be between 1 and 200 and offset must be nonnegative")
	}
	query := `SELECT ` + subscriptionColumns + ` FROM subscriptions`
	var args []any
	if session != "" {
		query += ` WHERE session=?`
		args = append(args, session)
	}
	query += ` ORDER BY last_offered_at,created_at,id LIMIT ? OFFSET ?`
	args = append(args, limit+1, offset)
	items, err := s.querySubscriptions(query, args...)
	more := len(items) > limit
	if more {
		items = items[:limit]
	}
	return items, more, err
}

func (s *Store) HookSubscriptions(session string, includeQuiet bool, limit int) ([]Subscription, error) {
	if err := validateSession(session); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 200 {
		return nil, failure("invalid_limit", "subscription limit must be between 1 and 200")
	}
	const pending = `(delivery_token='' OR lease_until<=?) AND (snapshot_pending=1 OR EXISTS(
 WITH RECURSIVE scope(id) AS (
  SELECT subscriptions.task_id
  UNION SELECT t.id FROM tasks t JOIN scope s ON t.parent_id=s.id
 ) SELECT 1 FROM scope s CROSS JOIN events e INDEXED BY events_hook_task_id
 WHERE e.task_id=s.id AND e.kind<>'progress' AND e.kind<>'note_deleted' AND e.id>subscriptions.cursor LIMIT 1))`
	query := `SELECT ` + subscriptionColumns + ` FROM subscriptions WHERE session=?`
	args := []any{session, timestamp(s.Now())}
	if includeQuiet {
		query += ` ORDER BY CASE WHEN ` + pending + ` THEN 0 ELSE 1 END,`
	} else {
		query += ` AND ` + pending + ` ORDER BY `
	}
	query += `last_offered_at,created_at,id LIMIT ?`
	args = append(args, limit)
	return s.querySubscriptions(query, args...)
}

func (s *Store) querySubscriptions(query string, args ...any) ([]Subscription, error) {
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	result := []Subscription{}
	for rows.Next() {
		record, err := scanSubscription(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, record.Subscription)
	}
	return result, rows.Err()
}

func (s *Store) Unsubscribe(id, session string) error {
	return s.write(func(tx *sql.Tx) error {
		record, err := loadSubscription(tx, id)
		if err != nil {
			return err
		}
		if err := subscriptionOwner(record, session); err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM subscriptions WHERE id=? AND session=?`, id, session)
		return err
	})
}

func snapshotWith(q Queryer, id int64) (Snapshot, error) {
	return readSnapshot(q, id, false)
}

func readSnapshot(q Queryer, id int64, summary bool) (Snapshot, error) {
	result := Snapshot{TaskID: taskID(id), Tasks: []TaskState{}}
	var err error
	result.Outcome, err = outcomeWith(q, id)
	if err != nil {
		return result, err
	}
	var total int
	if err := q.QueryRow(descendantScope+`SELECT COUNT(*),COALESCE(MAX(CASE WHEN `+availablePredicate+` THEN 1 ELSE 0 END),0)
 FROM tasks t JOIN scope s ON t.id=s.id WHERE t.status<>'superseded'`, id).Scan(&total, &result.Available); err != nil {
		return result, err
	}
	projection := `t.id,t.title,t.status,t.owner,t.progress,t.result,t.updated_at,0`
	if summary {
		projection = `t.id,substr(t.title,1,256),t.status,substr(t.owner,1,128),substr(t.progress,1,120),substr(t.result,1,1024),t.updated_at,
 octet_length(t.title)>octet_length(substr(t.title,1,256)) OR octet_length(t.owner)>octet_length(substr(t.owner,1,128)) OR octet_length(t.progress)>octet_length(substr(t.progress,1,120)) OR octet_length(t.result)>octet_length(substr(t.result,1,1024))`
	}
	rows, err := q.Query(descendantScope+`SELECT `+projection+`,(`+availablePredicate+`)
 FROM tasks t JOIN scope s ON t.id=s.id WHERE t.status<>'superseded'
 ORDER BY CASE WHEN t.status IN ('pending','in_progress') THEN 0 ELSE 1 END,t.id LIMIT 20`, id)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var task TaskState
		var n int64
		var truncated bool
		if err := rows.Scan(&n, &task.Title, &task.Status, &task.Owner, &task.Progress, &task.Result, &task.UpdatedAt, &truncated, &task.Available); err != nil {
			return result, err
		}
		task.ID = taskID(n)
		result.Truncated = result.Truncated || truncated
		result.Tasks = append(result.Tasks, task)
	}
	result.Omitted = total - len(result.Tasks)
	return result, rows.Err()
}

func (s *Store) Snapshot(id string) (Snapshot, error) {
	result, _, err := s.Current(id)
	return result, err
}

func (s *Store) SnapshotSummary(id string) (Snapshot, error) {
	result, _, err := s.current(id, true)
	return result, err
}

func (s *Store) Current(id string) (Snapshot, int64, error) {
	return s.current(id, false)
}

func (s *Store) current(id string, summary bool) (Snapshot, int64, error) {
	n, err := taskNumber(id)
	if err != nil {
		return Snapshot{}, 0, err
	}
	var result Snapshot
	var cursor int64
	err = s.read(func(q Queryer) error {
		var err error
		result, err = readSnapshot(q, n, summary)
		if err != nil {
			return err
		}
		return q.QueryRow(`SELECT COALESCE(MAX(id),0) FROM events`).Scan(&cursor)
	})
	return result, cursor, err
}

func (s *Store) Offer(id, session string, limit int, lease time.Duration) (Delivery, error) {
	return s.offer(id, session, limit, lease, false, false, false)
}

func (s *Store) OfferQueue(id, session string, limit int, lease time.Duration) (Delivery, error) {
	return s.offer(id, session, limit, lease, true, false, false)
}

func (s *Store) OfferQueueSummary(id, session string, limit int, lease time.Duration) (Delivery, error) {
	return s.offer(id, session, limit, lease, true, true, false)
}

func (s *Store) OfferSummary(id, session string, limit int, lease time.Duration) (Delivery, error) {
	return s.offer(id, session, limit, lease, false, true, false)
}

func (s *Store) OfferHookSummary(id, session string, limit int, lease time.Duration) (Delivery, error) {
	return s.offer(id, session, limit, lease, false, true, true)
}

func (s *Store) offer(id, session string, limit int, lease time.Duration, queue, summary, hook bool) (Delivery, error) {
	limit, err := eventLimit(limit)
	if err != nil {
		return Delivery{}, err
	}
	if lease <= 0 || lease > 24*time.Hour {
		return Delivery{}, failure("invalid_duration", "delivery lease must be positive and no longer than 24 hours")
	}
	var delivery Delivery
	err = s.write(func(tx *sql.Tx) error {
		record, err := loadSubscription(tx, id)
		if err != nil {
			return err
		}
		if err := subscriptionOwner(record, session); err != nil {
			return err
		}
		now := s.Now()
		if record.token != "" && record.LeaseUntil > timestamp(now) {
			return failure("delivery_busy", "subscription has an active delivery claim")
		}
		after, snapshotPending, mode := record.Cursor, record.SnapshotPending, "ack"
		if queue {
			after = max(record.QueueCursor, record.Cursor)
			snapshotPending, mode = record.QueueSnapshotPending, "queue"
		}
		var page EventPage
		if hook {
			page, err = readHookEvents(tx, record.target, after, limit)
		} else {
			page, err = readEvents(tx, record.target, after, limit, summary)
		}
		if err != nil {
			return err
		}
		delivery = Delivery{Subscription: record.Subscription, Mode: mode, Through: page.Cursor, Events: page.Events, More: page.More}
		if len(page.Events) == 0 && !snapshotPending {
			return nil
		}
		if snapshotPending {
			snapshot, err := readSnapshot(tx, record.target, summary)
			if err != nil {
				return err
			}
			delivery.Snapshot = &snapshot
		}
		if hook {
			if err := addHookProgress(tx, &delivery); err != nil {
				return err
			}
		}
		delivery.Token = newID("D")
		delivery.LeaseUntil = timestamp(now.Add(lease))
		delivery.Subscription.LeaseUntil = delivery.LeaseUntil
		delivery.Subscription.LastOfferedAt = timestamp(now)
		delivery.Subscription.UpdatedAt = timestamp(now)
		_, err = tx.Exec(`UPDATE subscriptions SET delivery_token=?,delivery_mode=?,lease_until=?,offered_through=?,
 offered_snapshot=?,last_offered_at=?,updated_at=? WHERE id=?`, delivery.Token,
			mode, delivery.LeaseUntil, delivery.Through, snapshotPending, timestamp(now), timestamp(now), id)
		return err
	})
	return delivery, err
}

func addHookProgress(q Queryer, delivery *Delivery) error {
	tasks := map[string]bool{}
	eventTasks := map[string]bool{}
	var args []any
	add := func(id string) error {
		if tasks[id] {
			return nil
		}
		n, err := taskNumber(id)
		if err != nil {
			return err
		}
		tasks[id] = true
		args = append(args, n)
		return nil
	}
	for _, event := range delivery.Events {
		eventTasks[event.TaskID] = true
		if err := add(event.TaskID); err != nil {
			return err
		}
	}
	if delivery.Snapshot != nil {
		for i := range delivery.Snapshot.Tasks {
			task := &delivery.Snapshot.Tasks[i]
			task.Progress = ""
			if err := add(task.ID); err != nil {
				return err
			}
		}
	}
	if len(args) == 0 {
		return nil
	}
	values := strings.TrimSuffix(strings.Repeat("(?),", len(args)), ",")
	args = append(args, delivery.Through)
	rows, err := q.Query(`WITH selected_tasks(id) AS (VALUES `+values+`)
 SELECT n.event_id,n.task_id,substr(n.title,1,120),n.created_at,
 octet_length(n.title)>octet_length(substr(n.title,1,120))
 FROM selected_tasks t JOIN notes n ON n.event_id=(
 SELECT event_id FROM notes INDEXED BY notes_task_order
 WHERE task_id=t.id AND event_id<=? ORDER BY event_id DESC LIMIT 1)
 WHERE n.deleted_at=''
 ORDER BY n.task_id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var event Event
		var n int64
		if err := rows.Scan(&event.ID, &n, &event.Message, &event.CreatedAt, &event.Truncated); err != nil {
			return err
		}
		event.TaskID, event.Kind = taskID(n), "progress"
		if eventTasks[event.TaskID] {
			delivery.LatestProgress = append(delivery.LatestProgress, event)
		}
		if delivery.Snapshot != nil {
			for i := range delivery.Snapshot.Tasks {
				task := &delivery.Snapshot.Tasks[i]
				if task.ID == event.TaskID {
					task.Progress = event.Message
				}
			}
		}
	}
	return rows.Err()
}

func (s *Store) Acknowledge(id, session, token string, through int64) error {
	if token == "" || through < 0 {
		return failure("invalid_acknowledgement", "acknowledgement requires a token and nonnegative cursor")
	}
	return s.write(func(tx *sql.Tx) error {
		record, err := loadSubscription(tx, id)
		if err != nil {
			return err
		}
		if err := subscriptionOwner(record, session); err != nil {
			return err
		}
		if record.token == "" && record.lastAckToken == token && record.lastAckThrough == through {
			return nil
		}
		if record.token != token || record.mode != "ack" || record.LeaseUntil <= timestamp(s.Now()) {
			return failure("conflict", "delivery claim is stale or expired; request a fresh delivery")
		}
		if through != record.offeredThrough {
			return failure("invalid_acknowledgement", "cursor must exactly match the offered delivery")
		}
		_, err = tx.Exec(`UPDATE subscriptions SET cursor=?,snapshot_pending=CASE WHEN offered_snapshot=1 THEN 0 ELSE snapshot_pending END,
 queue_snapshot_pending=CASE WHEN offered_snapshot=1 THEN 0 ELSE queue_snapshot_pending END,
 delivery_token='',delivery_mode='',lease_until='',offered_through=0,offered_snapshot=0,last_ack_token=?,last_ack_through=?,updated_at=?
 WHERE id=? AND delivery_token=?`, through, token, through, timestamp(s.Now()), id, token)
		return err
	})
}

func (s *Store) ReleaseDelivery(id, token string) error {
	if token == "" {
		return failure("invalid_delivery", "delivery token is required")
	}
	return s.write(func(tx *sql.Tx) error {
		record, err := loadSubscription(tx, id)
		if err != nil {
			return err
		}
		if record.token != token {
			return failure("conflict", "delivery claim is stale")
		}
		_, err = tx.Exec(`UPDATE subscriptions SET delivery_token='',delivery_mode='',lease_until='',offered_through=0,
 offered_snapshot=0,updated_at=? WHERE id=? AND delivery_token=?`, timestamp(s.Now()), id, token)
		return err
	})
}

func (s *Store) MarkQueued(id, session, token string, through int64) error {
	return s.recordQueueScan(id, session, token, through, true)
}

func (s *Store) AdvanceQueueScan(id, session, token string, through int64) error {
	return s.recordQueueScan(id, session, token, through, false)
}

func (s *Store) recordQueueScan(id, session, token string, through int64, accepted bool) error {
	if token == "" || through < 0 {
		return failure("invalid_delivery", "queue receipt requires a token and nonnegative cursor")
	}
	return s.write(func(tx *sql.Tx) error {
		record, err := loadSubscription(tx, id)
		if err != nil {
			return err
		}
		if err := subscriptionOwner(record, session); err != nil {
			return err
		}
		if record.token != token || record.mode != "queue" || record.LeaseUntil <= timestamp(s.Now()) {
			return failure("conflict", "delivery claim is stale or expired")
		}
		if through != record.offeredThrough {
			return failure("invalid_delivery", "queued cursor must exactly match the offered delivery")
		}
		_, err = tx.Exec(`UPDATE subscriptions SET queue_cursor=MAX(queue_cursor,?),
 queued_through=CASE WHEN ? THEN MAX(queued_through,?) ELSE queued_through END,
 queue_snapshot_pending=CASE WHEN offered_snapshot=1 THEN 0 ELSE queue_snapshot_pending END,
 updated_at=? WHERE id=? AND delivery_token=?`,
			through, accepted, through, timestamp(s.Now()), id, token)
		return err
	})
}
