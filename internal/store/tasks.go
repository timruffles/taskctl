package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
)

const taskSchema = `
CREATE TABLE IF NOT EXISTS tasks (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 title TEXT NOT NULL CHECK(length(trim(title)) > 0),
 brief TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL CHECK(status IN ('pending','in_progress','completed','cancelled','superseded')),
 origin_id INTEGER REFERENCES tasks(id),
 parent_id INTEGER REFERENCES tasks(id),
 revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0),
 owner TEXT NOT NULL DEFAULT '',
 claim_id TEXT NOT NULL DEFAULT '',
 progress TEXT NOT NULL DEFAULT '',
 result TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 last_activity_at TEXT NOT NULL DEFAULT '',
 CHECK((status = 'in_progress' AND owner <> '' AND claim_id <> '') OR
       (status <> 'in_progress' AND owner = '' AND claim_id = ''))
);
CREATE INDEX IF NOT EXISTS tasks_status_id ON tasks(status,id);
CREATE INDEX IF NOT EXISTS tasks_owner ON tasks(owner,status);
CREATE INDEX IF NOT EXISTS tasks_origin ON tasks(origin_id,id);
CREATE INDEX IF NOT EXISTS tasks_parent ON tasks(parent_id,id);
CREATE TABLE IF NOT EXISTS tags (
 task_id INTEGER NOT NULL REFERENCES tasks(id),
 tag TEXT NOT NULL CHECK(length(trim(tag)) > 0),
 PRIMARY KEY(task_id,tag)
);
CREATE INDEX IF NOT EXISTS tags_lookup ON tags(tag,task_id);
CREATE TABLE IF NOT EXISTS metadata (
 task_id INTEGER NOT NULL REFERENCES tasks(id),
 key TEXT NOT NULL CHECK(length(trim(key)) > 0),
 value TEXT NOT NULL,
 PRIMARY KEY(task_id,key)
);
CREATE TABLE IF NOT EXISTS prerequisites (
 task_id INTEGER NOT NULL REFERENCES tasks(id),
 prerequisite_id INTEGER NOT NULL REFERENCES tasks(id),
 PRIMARY KEY(task_id,prerequisite_id),
 CHECK(task_id <> prerequisite_id)
);
CREATE INDEX IF NOT EXISTS prerequisites_reverse ON prerequisites(prerequisite_id,task_id);
CREATE TABLE IF NOT EXISTS create_requests (
 request_id TEXT PRIMARY KEY,
 request_json TEXT NOT NULL,
 task_id INTEGER NOT NULL REFERENCES tasks(id)
);
`

type Task struct {
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	Brief          string            `json:"brief"`
	Status         string            `json:"status"`
	OriginID       string            `json:"origin_id"`
	ParentID       string            `json:"parent_id"`
	Revision       int64             `json:"revision"`
	Owner          string            `json:"owner"`
	ClaimID        string            `json:"claim_id"`
	Tags           []string          `json:"tags"`
	Metadata       map[string]string `json:"metadata"`
	Prerequisites  []string          `json:"prerequisites"`
	Progress       string            `json:"progress"`
	ProgressNoteID string            `json:"progress_note_id,omitempty"`
	ProgressAt     string            `json:"progress_at,omitempty"`
	Result         string            `json:"result"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	LastActivityAt string            `json:"last_activity_at"`
}

type CreateSpec struct {
	Key           string            `json:"key,omitempty"`
	Title         string            `json:"title"`
	Brief         string            `json:"brief"`
	Tags          []string          `json:"tags,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Prerequisites []string          `json:"prerequisites,omitempty"`
}

type SupersedeSpec struct {
	Summary      string       `json:"summary"`
	Replacements []CreateSpec `json:"replacements"`
	ContinueWith string       `json:"continue_with,omitempty"`
}

const taskProjection = `
 COALESCE(t.id,0),COALESCE(t.title,''),COALESCE(t.brief,''),COALESCE(t.status,''),
 COALESCE(t.origin_id,t.id,0),COALESCE(t.parent_id,0),COALESCE(t.revision,0),
 COALESCE(t.owner,''),COALESCE(t.claim_id,''),COALESCE(t.progress,''),COALESCE(t.result,''),
 COALESCE(t.created_at,''),COALESCE(t.updated_at,''),COALESCE(t.last_activity_at,''),
 (SELECT json_group_array(tag) FROM (SELECT tag FROM tags WHERE task_id=t.id ORDER BY tag)),
 (SELECT json_group_object(key,value) FROM (SELECT key,value FROM metadata WHERE task_id=t.id ORDER BY key)),
 (SELECT json_group_array('T'||prerequisite_id) FROM (SELECT prerequisite_id FROM prerequisites WHERE task_id=t.id ORDER BY prerequisite_id)),
 COALESCE((SELECT CASE WHEN deleted_at='' THEN id ELSE '' END FROM notes WHERE task_id=t.id ORDER BY event_id DESC LIMIT 1),''),
 COALESCE((SELECT CASE WHEN deleted_at='' THEN created_at ELSE '' END FROM notes WHERE task_id=t.id ORDER BY event_id DESC LIMIT 1),'')`

const listTaskProjection = `
 COALESCE(t.id,0),COALESCE(t.title,''),'',COALESCE(t.status,''),
 COALESCE(t.origin_id,t.id,0),COALESCE(t.parent_id,0),COALESCE(t.revision,0),
 COALESCE(t.owner,''),COALESCE(t.claim_id,''),'','',
 COALESCE(t.created_at,''),COALESCE(t.updated_at,''),COALESCE(t.last_activity_at,''),
 (SELECT json_group_array(tag) FROM (SELECT tag FROM tags WHERE task_id=t.id ORDER BY tag)),
 '{}',
 (SELECT json_group_array('T'||prerequisite_id) FROM (SELECT prerequisite_id FROM prerequisites WHERE task_id=t.id ORDER BY prerequisite_id)), '', ''`

type taskScanner interface{ Scan(...any) error }

func scanTask(row taskScanner, prefix ...any) (Task, error) {
	var t Task
	var id, origin, parent int64
	var tagsJSON, metadataJSON, prerequisitesJSON string
	dest := append(prefix, &id, &t.Title, &t.Brief, &t.Status, &origin, &parent,
		&t.Revision, &t.Owner, &t.ClaimID, &t.Progress, &t.Result, &t.CreatedAt,
		&t.UpdatedAt, &t.LastActivityAt, &tagsJSON, &metadataJSON, &prerequisitesJSON, &t.ProgressNoteID, &t.ProgressAt)
	if err := row.Scan(dest...); err != nil {
		return Task{}, err
	}
	t.ID, t.OriginID, t.ParentID = taskID(id), taskID(origin), taskID(parent)
	if err := json.Unmarshal([]byte(tagsJSON), &t.Tags); err != nil {
		return Task{}, err
	}
	if err := json.Unmarshal([]byte(metadataJSON), &t.Metadata); err != nil {
		return Task{}, err
	}
	if err := json.Unmarshal([]byte(prerequisitesJSON), &t.Prerequisites); err != nil {
		return Task{}, err
	}
	return t, nil
}

func getTask(q Queryer, id int64) (Task, error) {
	t, err := scanTask(q.QueryRow("SELECT "+taskProjection+" FROM tasks t WHERE t.id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, failure("not_found", "task "+taskID(id)+" does not exist")
	}
	return t, err
}

func (s *Store) Get(id string) (Task, error) {
	n, err := taskNumber(id)
	if err != nil {
		return Task{}, err
	}
	return getTask(s.DB, n)
}

func validateLabel(value, name string) (string, error) {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", failure("invalid_input", name+" must not contain control characters")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", failure("invalid_input", name+" must not be empty")
	}
	return value, nil
}

func validateMetadataKey(key string) (string, error) {
	key, err := validateLabel(key, "metadata key")
	if err != nil {
		return "", err
	}
	switch strings.ToLower(key) {
	case "id", "status", "owner", "claim_id", "origin_id", "parent_id", "revision":
		return "", failure("invalid_input", "metadata key "+key+" is reserved")
	}
	return key, nil
}

func normaliseSpec(spec CreateSpec, replacement bool) (CreateSpec, error) {
	title, err := validateLabel(spec.Title, "title")
	if err != nil {
		return CreateSpec{}, err
	}
	out := CreateSpec{Key: spec.Key, Title: title, Brief: spec.Brief, Tags: []string{}, Metadata: map[string]string{}, Prerequisites: []string{}}
	if replacement {
		if strings.TrimSpace(out.Brief) == "" {
			return CreateSpec{}, failure("invalid_input", "each replacement requires a self-contained brief")
		}
		if out.Key != "" {
			out.Key, err = validateLabel(out.Key, "replacement key")
			if err != nil {
				return CreateSpec{}, err
			}
			if strings.HasPrefix(out.Key, "@") {
				return CreateSpec{}, failure("invalid_input", "replacement keys must not start with @")
			}
		}
	} else if out.Key != "" {
		return CreateSpec{}, failure("invalid_input", "key is only valid for replacements")
	}
	tags := map[string]bool{}
	for _, tag := range spec.Tags {
		tag, err = validateLabel(tag, "tag")
		if err != nil {
			return CreateSpec{}, err
		}
		tags[tag] = true
	}
	for tag := range tags {
		out.Tags = append(out.Tags, tag)
	}
	sort.Strings(out.Tags)
	for key, value := range spec.Metadata {
		key, err = validateMetadataKey(key)
		if err != nil {
			return CreateSpec{}, err
		}
		if _, exists := out.Metadata[key]; exists {
			return CreateSpec{}, failure("invalid_input", "metadata keys must be distinct after trimming")
		}
		out.Metadata[key] = value
	}
	seen := map[string]bool{}
	for _, p := range spec.Prerequisites {
		if replacement && strings.HasPrefix(p, "@") {
			if len(p) == 1 {
				return CreateSpec{}, failure("invalid_input", "a sibling prerequisite needs a key after @")
			}
		} else if _, err := taskNumber(p); err != nil {
			return CreateSpec{}, err
		}
		if !seen[p] {
			out.Prerequisites = append(out.Prerequisites, p)
			seen[p] = true
		}
	}
	return out, nil
}

func (s *Store) insertTask(tx *sql.Tx, spec CreateSpec, origin, parent int64, now string) (int64, error) {
	var originArg, parentArg any
	if origin != 0 {
		originArg = origin
	}
	if parent != 0 {
		parentArg = parent
	}
	r, err := tx.Exec(`INSERT INTO tasks(title,brief,status,origin_id,parent_id,created_at,updated_at)
 VALUES(?,?,'pending',?,?,?,?)`, spec.Title, spec.Brief, originArg, parentArg, now, now)
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	if origin == 0 {
		if _, err = tx.Exec("UPDATE tasks SET origin_id=? WHERE id=?", id, id); err != nil {
			return 0, err
		}
	}
	for _, tag := range spec.Tags {
		if _, err = tx.Exec("INSERT INTO tags(task_id,tag) VALUES(?,?)", id, tag); err != nil {
			return 0, err
		}
	}
	for key, value := range spec.Metadata {
		if _, err = tx.Exec("INSERT INTO metadata(task_id,key,value) VALUES(?,?,?)", id, key, value); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func prerequisiteExists(q Queryer, id int64) error {
	var found int
	err := q.QueryRow("SELECT 1 FROM tasks WHERE id=?", id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return failure("not_found", "prerequisite "+taskID(id)+" does not exist")
	}
	return err
}

func (s *Store) Create(spec CreateSpec) (Task, error) {
	return s.CreateWithRequest(spec, "")
}

func (s *Store) CreateWithRequest(spec CreateSpec, requestID string) (Task, error) {
	if requestID != "" && (len(requestID) > 256 || strings.TrimSpace(requestID) != requestID || strings.IndexFunc(requestID, unicode.IsControl) >= 0) {
		return Task{}, failure("invalid_input", "request ID must be at most 256 bytes with no surrounding whitespace or control characters")
	}
	spec, err := normaliseSpec(spec, false)
	if err != nil {
		return Task{}, err
	}
	canonical := spec
	canonical.Prerequisites = append([]string{}, spec.Prerequisites...)
	sort.Strings(canonical.Prerequisites)
	requestJSON, err := json.Marshal(canonical)
	if err != nil {
		return Task{}, err
	}
	var result Task
	err = s.write(func(tx *sql.Tx) error {
		if requestID != "" {
			var previousJSON string
			var previousID int64
			err := tx.QueryRow("SELECT request_json,task_id FROM create_requests WHERE request_id=?", requestID).Scan(&previousJSON, &previousID)
			if err == nil {
				if previousJSON != string(requestJSON) {
					return failure("conflict", "request ID was already used with different task input")
				}
				result, err = getTask(tx, previousID)
				return err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		prerequisites := make([]int64, 0, len(spec.Prerequisites))
		for _, prerequisite := range spec.Prerequisites {
			id, _ := taskNumber(prerequisite)
			if err := prerequisiteExists(tx, id); err != nil {
				return err
			}
			prerequisites = append(prerequisites, id)
		}
		id, err := s.insertTask(tx, spec, 0, 0, timestamp(s.Now()))
		if err != nil {
			return err
		}
		for _, prerequisite := range prerequisites {
			if _, err := tx.Exec("INSERT INTO prerequisites(task_id,prerequisite_id) VALUES(?,?)", id, prerequisite); err != nil {
				return err
			}
		}
		if requestID != "" {
			if _, err := tx.Exec("INSERT INTO create_requests(request_id,request_json,task_id) VALUES(?,?,?)", requestID, string(requestJSON), id); err != nil {
				return err
			}
		}
		if _, err := s.emit(tx, id, "created", spec.Title, map[string]any{"prerequisites": spec.Prerequisites}); err != nil {
			return err
		}
		if err := s.emitAvailableTasks(tx, "t.id=?", id); err != nil {
			return err
		}
		result, err = getTask(tx, id)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return result, nil
}

const availablePredicate = `t.status='pending' AND NOT EXISTS(
 SELECT 1 FROM prerequisites p JOIN tasks prerequisite ON prerequisite.id=p.prerequisite_id
 WHERE p.task_id=t.id AND prerequisite.status<>'completed' AND (
 prerequisite.status<>'superseded' OR EXISTS(
  WITH RECURSIVE needed(id,status) AS (
   SELECT prerequisite.id,prerequisite.status
   UNION
   SELECT c.id,c.status FROM needed n JOIN tasks c ON c.parent_id=n.id WHERE n.status='superseded'
  ) SELECT 1 FROM needed n WHERE n.status IN ('pending','in_progress','cancelled')
  OR (n.status='superseded' AND NOT EXISTS(SELECT 1 FROM tasks c WHERE c.parent_id=n.id)))))`

func availableWith(q Queryer, id int64) (bool, error) {
	var ready bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks t WHERE t.id=? AND "+availablePredicate+")", id).Scan(&ready)
	return ready, err
}

// Callers select tasks that were blocked or absent before their mutation.
func (s *Store) emitAvailableTasks(tx *sql.Tx, selection string, args ...any) error {
	args = append([]any{timestamp(s.Now())}, args...)
	_, err := tx.Exec(`INSERT INTO events(task_id,kind,message,data,created_at)
 SELECT t.id,'available','Ready to claim','{}',? FROM tasks t WHERE (`+selection+") AND "+availablePredicate+" ORDER BY t.id", args...)
	return err
}

func (s *Store) emitCompletedDependents(tx *sql.Tx, id int64) error {
	return s.emitAvailableTasks(tx, `t.id IN (
 WITH RECURSIVE ancestors(id,parent_id) AS (
  SELECT id,parent_id FROM tasks WHERE id=?
  UNION
  SELECT t.id,t.parent_id FROM ancestors a JOIN tasks t ON t.id=a.parent_id WHERE t.status='superseded'
 ) SELECT p.task_id FROM ancestors a JOIN prerequisites p ON p.prerequisite_id=a.id)`, id)
}

func outcomeWith(q Queryer, id int64) (string, error) {
	var outcome string
	err := q.QueryRow(`WITH RECURSIVE descendants(id,status) AS (
 SELECT id,status FROM tasks WHERE id=?
 UNION
 SELECT t.id,t.status FROM tasks t JOIN descendants d ON t.parent_id=d.id WHERE d.status='superseded'
) SELECT CASE WHEN COUNT(*)=0 THEN ''
 WHEN MAX(status='cancelled') THEN 'cancelled'
 WHEN MAX(status IN ('pending','in_progress') OR (status='superseded' AND NOT EXISTS(SELECT 1 FROM tasks c WHERE c.parent_id=descendants.id))) THEN 'pending'
 ELSE 'completed' END FROM descendants`, id).Scan(&outcome)
	if err != nil {
		return "", err
	}
	if outcome == "" {
		return "", failure("not_found", "task "+taskID(id)+" does not exist")
	}
	return outcome, nil
}

func (s *Store) Outcome(id string) (string, error) {
	n, err := taskNumber(id)
	if err != nil {
		return "", err
	}
	return outcomeWith(s.DB, n)
}

func (s *Store) List(filter string, tags []string, limit, offset int) ([]Task, int, error) {
	if limit < 0 || offset < 0 {
		return nil, 0, failure("invalid_input", "limit and offset must not be negative")
	}
	if limit == 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	var conditions []string
	switch filter {
	case "", "all":
		conditions = append(conditions, "1")
	case "active", "current":
		conditions = append(conditions, "t.status IN ('pending','in_progress')")
	case "available":
		conditions = append(conditions, availablePredicate)
	case "blocked":
		conditions = append(conditions, "t.status='pending' AND NOT ("+availablePredicate+")")
	case "in-progress", "in_progress":
		conditions = append(conditions, "t.status='in_progress'")
	case "pending", "completed", "cancelled", "superseded":
		conditions = append(conditions, "t.status='"+filter+"'")
	default:
		return nil, 0, failure("invalid_input", "unknown task filter "+filter)
	}
	args := make([]any, 0, len(tags)+2)
	for _, tag := range tags {
		tag, err := validateLabel(tag, "tag")
		if err != nil {
			return nil, 0, err
		}
		conditions = append(conditions, "EXISTS(SELECT 1 FROM tags tg WHERE tg.task_id=t.id AND tg.tag=?)")
		args = append(args, tag)
	}
	query := "WITH filtered AS (SELECT t.id FROM tasks t WHERE " + strings.Join(conditions, " AND ") + `),
 totals AS (SELECT COUNT(*) AS total FROM filtered),
 page AS (SELECT id FROM filtered ORDER BY id LIMIT ? OFFSET ?)
 SELECT totals.total,` + listTaskProjection + ` FROM totals LEFT JOIN page ON 1 LEFT JOIN tasks t ON t.id=page.id ORDER BY t.id`
	args = append(args, limit, offset)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Task{}
	var total int
	for rows.Next() {
		t, err := scanTask(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		if t.ID != "" {
			items = append(items, t)
		}
	}
	return items, total, rows.Err()
}

func requireCurrentClaim(t Task, claim string) error {
	if t.Status != "in_progress" || claim == "" || claim != t.ClaimID {
		return failure("conflict", "task "+t.ID+" does not have this active claim")
	}
	return nil
}

func requireEditable(t Task, claim string) error {
	switch t.Status {
	case "pending":
		if claim != "" {
			return failure("conflict", "pending task "+t.ID+" has no active claim")
		}
		return nil
	case "in_progress":
		return requireCurrentClaim(t, claim)
	default:
		return failure("conflict", "task "+t.ID+" is terminal ("+t.Status+")")
	}
}

func requireRevision(t Task, revision int64) error {
	if revision <= 0 {
		return failure("invalid_input", "an expected revision is required")
	}
	if t.Revision != revision {
		return failure("conflict", fmt.Sprintf("task %s changed: expected revision %d, current revision %d", t.ID, revision, t.Revision))
	}
	return nil
}

func requireReady(q Queryer, t Task) error {
	for _, prerequisite := range t.Prerequisites {
		id, err := taskNumber(prerequisite)
		if err != nil {
			return err
		}
		outcome, err := outcomeWith(q, id)
		if err != nil {
			return err
		}
		if outcome != "completed" {
			return failure("blocked", "task "+t.ID+" is blocked by "+prerequisite+" ("+outcome+")")
		}
	}
	return nil
}

func (s *Store) taskWrite(id string, action func(*sql.Tx, int64, Task) error) (Task, error) {
	n, err := taskNumber(id)
	if err != nil {
		return Task{}, err
	}
	var result Task
	err = s.write(func(tx *sql.Tx) error {
		current, err := getTask(tx, n)
		if err != nil {
			return err
		}
		if err := action(tx, n, current); err != nil {
			return err
		}
		result, err = getTask(tx, n)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return result, nil
}

func (s *Store) Claim(id, session string) (Task, error) {
	session, err := validateLabel(session, "session")
	if err != nil {
		return Task{}, err
	}
	return s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if t.Status != "pending" {
			return failure("conflict", "task "+id+" is "+t.Status+"; claim requires pending")
		}
		if err := requireReady(tx, t); err != nil {
			return err
		}
		claim, now := newID("C"), timestamp(s.Now())
		if _, err := tx.Exec(`UPDATE tasks SET status='in_progress',owner=?,claim_id=?,revision=revision+1,updated_at=?,last_activity_at=? WHERE id=?`, session, claim, now, now, n); err != nil {
			return err
		}
		_, err := s.emit(tx, n, "claimed", "Claimed by "+session, map[string]any{"owner": session, "claim_id": claim})
		return err
	})
}

func (s *Store) Release(id, claim string) (Task, error) {
	return s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireCurrentClaim(t, claim); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE tasks SET status='pending',owner='',claim_id='',revision=revision+1,updated_at=? WHERE id=?`, timestamp(s.Now()), n); err != nil {
			return err
		}
		if _, err := s.emit(tx, n, "released", "Released by "+t.Owner+"; task is pending", map[string]any{"previous_owner": t.Owner, "previous_claim_id": claim}); err != nil {
			return err
		}
		return s.emitAvailableTasks(tx, "t.id=?", n)
	})
}

func (s *Store) Takeover(id, expectedClaim, session, reason string) (Task, error) {
	session, err := validateLabel(session, "session")
	if err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(reason) == "" {
		return Task{}, failure("invalid_input", "takeover requires a reason")
	}
	return s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireCurrentClaim(t, expectedClaim); err != nil {
			return err
		}
		claim, now := newID("C"), timestamp(s.Now())
		if _, err := tx.Exec(`UPDATE tasks SET owner=?,claim_id=?,revision=revision+1,updated_at=?,last_activity_at=? WHERE id=?`, session, claim, now, now, n); err != nil {
			return err
		}
		_, err := s.emit(tx, n, "reassigned", reason, map[string]any{"previous_owner": t.Owner, "owner": session, "previous_claim_id": expectedClaim, "claim_id": claim})
		return err
	})
}

func (s *Store) Progress(id, claim, text string) (Task, error) {
	if strings.TrimSpace(text) == "" {
		return Task{}, failure("invalid_input", "progress must not be empty")
	}
	title, err := validateNoteTitle(text)
	var body io.Reader
	if err != nil || title != text {
		title = legacyNoteTitle(text)
		body = strings.NewReader(text)
	}
	if _, err = s.AddNote(id, claim, title, body); err != nil {
		return Task{}, err
	}
	return s.Get(id)
}

func (s *Store) Heartbeat(id, claim string) error {
	n, err := taskNumber(id)
	if err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		t, err := getTask(tx, n)
		if err != nil {
			return err
		}
		if err := requireCurrentClaim(t, claim); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE tasks SET last_activity_at=? WHERE id=? AND status='in_progress' AND claim_id=?", timestamp(s.Now()), n, claim)
		return err
	})
}

func (s *Store) Complete(id, claim, summary string) (Task, error) {
	if strings.TrimSpace(summary) == "" {
		return Task{}, failure("invalid_input", "completion requires a summary")
	}
	return s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireCurrentClaim(t, claim); err != nil {
			return err
		}
		if err := requireReady(tx, t); err != nil {
			return err
		}
		now := timestamp(s.Now())
		if _, err := tx.Exec(`UPDATE tasks SET status='completed',owner='',claim_id='',result=?,revision=revision+1,updated_at=?,last_activity_at=? WHERE id=?`, summary, now, now, n); err != nil {
			return err
		}
		if _, err := s.emit(tx, n, "completed", summary, map[string]any{"owner": t.Owner}); err != nil {
			return err
		}
		if err := s.refreshScratch(tx, n); err != nil {
			return err
		}
		return s.emitCompletedDependents(tx, n)
	})
}

func (s *Store) Cancel(id, claim, reason string) (Task, error) {
	if strings.TrimSpace(reason) == "" {
		return Task{}, failure("invalid_input", "cancellation requires a reason")
	}
	return s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireEditable(t, claim); err != nil {
			return err
		}
		now := timestamp(s.Now())
		if _, err := tx.Exec(`UPDATE tasks SET status='cancelled',owner='',claim_id='',result=?,revision=revision+1,updated_at=?,last_activity_at=? WHERE id=?`, reason, now, now, n); err != nil {
			return err
		}
		if _, err := s.emit(tx, n, "cancelled", reason, map[string]any{"previous_owner": t.Owner}); err != nil {
			return err
		}
		return s.refreshScratch(tx, n)
	})
}

func (s *Store) Edit(id, claim string, revision int64, title, brief *string) (Task, error) {
	if title == nil && brief == nil {
		return Task{}, failure("invalid_input", "edit requires a title or brief")
	}
	var newTitle string
	if title != nil {
		var err error
		newTitle, err = validateLabel(*title, "title")
		if err != nil {
			return Task{}, err
		}
	}
	return s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireEditable(t, claim); err != nil {
			return err
		}
		if err := requireRevision(t, revision); err != nil {
			return err
		}
		changed := []string{}
		if title != nil && newTitle != t.Title {
			t.Title = newTitle
			changed = append(changed, "title")
		}
		if brief != nil && *brief != t.Brief {
			if t.ParentID != "" && strings.TrimSpace(*brief) == "" {
				return failure("invalid_input", "replacement brief must remain self-contained")
			}
			t.Brief = *brief
			changed = append(changed, "brief")
		}
		if len(changed) == 0 {
			return nil
		}
		now := timestamp(s.Now())
		if _, err := tx.Exec(`UPDATE tasks SET title=?,brief=?,revision=revision+1,updated_at=?,last_activity_at=CASE WHEN status='in_progress' THEN ? ELSE last_activity_at END WHERE id=?`, t.Title, t.Brief, now, now, n); err != nil {
			return err
		}
		_, err := s.emit(tx, n, "edited", "Edited "+strings.Join(changed, " and "), map[string]any{"fields": changed, "revision": revision + 1})
		return err
	})
}

func (s *Store) bumpTask(tx *sql.Tx, id int64) error {
	now := timestamp(s.Now())
	_, err := tx.Exec(`UPDATE tasks SET revision=revision+1,updated_at=?,last_activity_at=CASE WHEN status='in_progress' THEN ? ELSE last_activity_at END WHERE id=?`, now, now, id)
	return err
}

func (s *Store) Tag(id, claim, tag string, remove bool) error {
	tag, err := validateLabel(tag, "tag")
	if err != nil {
		return err
	}
	_, err = s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireEditable(t, claim); err != nil {
			return err
		}
		query := "INSERT OR IGNORE INTO tags(task_id,tag) VALUES(?,?)"
		verb := "Added"
		if remove {
			query, verb = "DELETE FROM tags WHERE task_id=? AND tag=?", "Removed"
		}
		r, err := tx.Exec(query, n, tag)
		if err != nil {
			return err
		}
		count, err := r.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if err := s.bumpTask(tx, n); err != nil {
			return err
		}
		_, err = s.emit(tx, n, "tags_changed", verb+" tag "+tag, map[string]any{"tag": tag, "removed": remove})
		return err
	})
	return err
}

func (s *Store) MetadataSet(id, claim, key, value string) error {
	key, err := validateMetadataKey(key)
	if err != nil {
		return err
	}
	_, err = s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireEditable(t, claim); err != nil {
			return err
		}
		if old, exists := t.Metadata[key]; exists && old == value {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO metadata(task_id,key,value) VALUES(?,?,?) ON CONFLICT(task_id,key) DO UPDATE SET value=excluded.value`, n, key, value); err != nil {
			return err
		}
		if err := s.bumpTask(tx, n); err != nil {
			return err
		}
		_, err := s.emit(tx, n, "metadata_changed", "Set metadata "+key, map[string]any{"key": key, "value": value})
		return err
	})
	return err
}

func checkTaskEdge(q Queryer, from, to int64) error {
	var cycle bool
	err := q.QueryRow(`WITH RECURSIVE reachable(id) AS (
 VALUES(?)
 UNION SELECT p.prerequisite_id FROM reachable r JOIN prerequisites p ON p.task_id=r.id
 UNION SELECT t.id FROM reachable r JOIN tasks t ON t.parent_id=r.id
) SELECT EXISTS(SELECT 1 FROM reachable WHERE id=?)`, to, from).Scan(&cycle)
	if err != nil {
		return err
	}
	if cycle {
		return failure("conflict", "prerequisites and replacement lineage would create a cycle at "+taskID(from))
	}
	return nil
}

func checkTaskCycles(tx *sql.Tx) error {
	const cleanup = `DROP TABLE IF EXISTS temp.cycle_edges; DROP TABLE IF EXISTS temp.cycle_degrees; DROP TABLE IF EXISTS temp.cycle_batch;`
	if _, err := tx.Exec(cleanup + `
 CREATE TEMP TABLE cycle_edges(src INTEGER NOT NULL,dst INTEGER NOT NULL,PRIMARY KEY(src,dst)) WITHOUT ROWID;
 CREATE INDEX cycle_edges_dst ON cycle_edges(dst,src);
 CREATE TEMP TABLE cycle_degrees(id INTEGER PRIMARY KEY,degree INTEGER NOT NULL);
 CREATE INDEX cycle_degrees_zero ON cycle_degrees(degree,id) WHERE degree=0;
 CREATE TEMP TABLE cycle_batch(id INTEGER PRIMARY KEY);
 INSERT OR IGNORE INTO cycle_edges SELECT task_id,prerequisite_id FROM prerequisites;
 INSERT OR IGNORE INTO cycle_edges SELECT parent_id,id FROM tasks WHERE parent_id IS NOT NULL;
 INSERT INTO cycle_degrees SELECT dst,COUNT(*) FROM cycle_edges GROUP BY dst;
 INSERT OR IGNORE INTO cycle_degrees SELECT src,0 FROM cycle_edges;`); err != nil {
		return err
	}
	defer tx.Exec(cleanup)
	for {
		if _, err := tx.Exec("DELETE FROM cycle_batch"); err != nil {
			return err
		}
		result, err := tx.Exec("INSERT INTO cycle_batch SELECT id FROM cycle_degrees WHERE degree=0")
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 0 {
			var id int64
			err := tx.QueryRow("SELECT id FROM cycle_degrees LIMIT 1").Scan(&id)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return failure("conflict", "prerequisites and replacement lineage would create a cycle at "+taskID(id))
		}
		if _, err := tx.Exec(`UPDATE cycle_degrees SET degree=degree-(
 SELECT COUNT(*) FROM cycle_edges e JOIN cycle_batch b ON b.id=e.src WHERE e.dst=cycle_degrees.id)
 WHERE id IN (SELECT e.dst FROM cycle_batch b CROSS JOIN cycle_edges e WHERE e.src=b.id);
 DELETE FROM cycle_edges WHERE src IN (SELECT id FROM cycle_batch);
 DELETE FROM cycle_degrees WHERE id IN (SELECT id FROM cycle_batch);`); err != nil {
			return err
		}
	}
}

func (s *Store) AddPrerequisite(id, claim, prerequisite string, remove bool) error {
	p, err := taskNumber(prerequisite)
	if err != nil {
		return err
	}
	_, err = s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if err := requireEditable(t, claim); err != nil {
			return err
		}
		if err := prerequisiteExists(tx, p); err != nil {
			return err
		}
		if n == p && !remove {
			return failure("conflict", "a task cannot depend on itself")
		}
		wasAvailable := false
		if remove && t.Status == "pending" {
			wasAvailable, err = availableWith(tx, n)
			if err != nil {
				return err
			}
		}
		query, verb := "INSERT OR IGNORE INTO prerequisites(task_id,prerequisite_id) VALUES(?,?)", "Added"
		if remove {
			query, verb = "DELETE FROM prerequisites WHERE task_id=? AND prerequisite_id=?", "Removed"
		}
		r, err := tx.Exec(query, n, p)
		if err != nil {
			return err
		}
		count, err := r.RowsAffected()
		if err != nil || count == 0 {
			return err
		}
		if !remove {
			if err := checkTaskEdge(tx, n, p); err != nil {
				return err
			}
		}
		if err := s.bumpTask(tx, n); err != nil {
			return err
		}
		if _, err = s.emit(tx, n, "prerequisite_changed", verb+" prerequisite "+prerequisite, map[string]any{"prerequisite": prerequisite, "removed": remove}); err != nil {
			return err
		}
		if remove && !wasAvailable {
			return s.emitAvailableTasks(tx, "t.id=?", n)
		}
		return nil
	})
	return err
}

func inheritSpec(original Task, child CreateSpec) CreateSpec {
	tags := map[string]bool{}
	for _, tag := range original.Tags {
		tags[tag] = true
	}
	for _, tag := range child.Tags {
		tags[tag] = true
	}
	child.Tags = make([]string, 0, len(tags))
	for tag := range tags {
		child.Tags = append(child.Tags, tag)
	}
	sort.Strings(child.Tags)
	metadata := make(map[string]string, len(original.Metadata)+len(child.Metadata))
	for key, value := range original.Metadata {
		metadata[key] = value
	}
	for key, value := range child.Metadata {
		metadata[key] = value
	}
	child.Metadata = metadata
	child.Prerequisites = append(append([]string{}, original.Prerequisites...), child.Prerequisites...)
	return child
}

func (s *Store) Supersede(id, claim string, revision int64, spec SupersedeSpec) ([]Task, error) {
	n, err := taskNumber(id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.Summary) == "" {
		return nil, failure("invalid_input", "supersession requires a handoff summary")
	}
	if len(spec.Replacements) == 0 {
		return nil, failure("invalid_input", "supersession requires at least one replacement")
	}
	replacements := make([]CreateSpec, len(spec.Replacements))
	keys := map[string]bool{}
	for i, replacement := range spec.Replacements {
		replacements[i], err = normaliseSpec(replacement, true)
		if err != nil {
			return nil, err
		}
		key := replacements[i].Key
		if key != "" {
			if keys[key] {
				return nil, failure("invalid_input", "duplicate replacement key "+key)
			}
			keys[key] = true
		}
	}
	if spec.ContinueWith != "" && !keys[spec.ContinueWith] {
		return nil, failure("invalid_input", "continue_with must name a replacement key")
	}
	var result []Task
	err = s.write(func(tx *sql.Tx) error {
		original, err := getTask(tx, n)
		if err != nil {
			return err
		}
		if err := requireEditable(original, claim); err != nil {
			return err
		}
		if err := requireRevision(original, revision); err != nil {
			return err
		}
		if spec.ContinueWith != "" && original.Status != "in_progress" {
			return failure("conflict", "continue_with requires an active original claim")
		}
		origin, err := taskNumber(original.OriginID)
		if err != nil {
			return err
		}
		now := timestamp(s.Now())
		ids := make([]int64, len(replacements))
		labels := make([]string, len(replacements))
		byKey := map[string]int64{}
		inherited := make([]CreateSpec, len(replacements))
		for i, replacement := range replacements {
			inherited[i] = inheritSpec(original, replacement)
			ids[i], err = s.insertTask(tx, inherited[i], origin, n, now)
			if err != nil {
				return err
			}
			labels[i] = taskID(ids[i])
			if replacement.Key != "" {
				byKey[replacement.Key] = ids[i]
			}
			if _, err := tx.Exec(`INSERT INTO scratch_refs(task_id,area_id) SELECT ?,area_id FROM scratch_refs WHERE task_id=?`, ids[i], n); err != nil {
				return err
			}
		}
		for i, replacement := range inherited {
			for _, ref := range replacement.Prerequisites {
				var prerequisite int64
				if strings.HasPrefix(ref, "@") {
					var exists bool
					prerequisite, exists = byKey[ref[1:]]
					if !exists {
						return failure("invalid_input", "unknown sibling prerequisite "+ref)
					}
				} else {
					prerequisite, err = taskNumber(ref)
					if err != nil {
						return err
					}
					if err := prerequisiteExists(tx, prerequisite); err != nil {
						return err
					}
				}
				if prerequisite == ids[i] {
					return failure("conflict", "a replacement cannot depend on itself")
				}
				if err := checkTaskEdge(tx, ids[i], prerequisite); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT OR IGNORE INTO prerequisites(task_id,prerequisite_id) VALUES(?,?)`, ids[i], prerequisite); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE tasks SET status='superseded',owner='',claim_id='',result=?,revision=revision+1,updated_at=?,last_activity_at=? WHERE id=?`, spec.Summary, now, now, n); err != nil {
			return err
		}
		data := map[string]any{"replacement_ids": labels, "previous_owner": original.Owner}
		if spec.ContinueWith != "" {
			data["continued_task"] = taskID(byKey[spec.ContinueWith])
		}
		if _, err := s.emit(tx, n, "superseded", spec.Summary, data); err != nil {
			return err
		}
		for i, replacement := range replacements {
			if _, err := s.emit(tx, ids[i], "created", replacement.Title, map[string]any{"parent_id": id, "origin_id": original.OriginID}); err != nil {
				return err
			}
		}
		if spec.ContinueWith != "" {
			continued := byKey[spec.ContinueWith]
			t, err := getTask(tx, continued)
			if err != nil {
				return err
			}
			if err := requireReady(tx, t); err != nil {
				return err
			}
			newClaim := newID("C")
			if _, err := tx.Exec(`UPDATE tasks SET status='in_progress',owner=?,claim_id=?,revision=revision+1,last_activity_at=? WHERE id=?`, original.Owner, newClaim, now, continued); err != nil {
				return err
			}
			if _, err := s.emit(tx, continued, "claimed", "Continued by "+original.Owner, map[string]any{"owner": original.Owner, "claim_id": newClaim, "previous_task": id}); err != nil {
				return err
			}
		}
		if err := s.refreshScratch(tx, n); err != nil {
			return err
		}
		if err := s.emitAvailableTasks(tx, "t.parent_id=?", n); err != nil {
			return err
		}
		result = make([]Task, 0, len(ids))
		for _, id := range ids {
			t, err := getTask(tx, id)
			if err != nil {
				return err
			}
			result = append(result, t)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) HistoryPage(id string, limit, offset int) ([]Task, bool, error) {
	n, err := taskNumber(id)
	if err != nil {
		return nil, false, err
	}
	if limit < 0 || offset < 0 {
		return nil, false, failure("invalid_input", "limit and offset must not be negative")
	}
	if limit == 0 {
		limit = 20
	}
	if limit > 200 {
		limit = 200
	}
	var origin int64
	err = s.DB.QueryRow("SELECT COALESCE(origin_id,id) FROM tasks WHERE id=?", n).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, failure("not_found", "task "+id+" does not exist")
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := s.DB.Query("SELECT "+taskProjection+" FROM tasks t WHERE t.origin_id=? ORDER BY t.id LIMIT ? OFFSET ?", origin, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	history := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, false, err
		}
		if len(history) == limit {
			return history, true, nil
		}
		history = append(history, t)
	}
	return history, false, rows.Err()
}
