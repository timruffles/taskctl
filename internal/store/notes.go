package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const noteSchema = `
CREATE TABLE IF NOT EXISTS notes(
 id TEXT PRIMARY KEY,
 task_id INTEGER NOT NULL REFERENCES tasks(id),
 event_id INTEGER NOT NULL UNIQUE REFERENCES events(id),
 title TEXT NOT NULL CHECK(length(title)<=120 AND (length(title)>0 OR deleted_at<>'')),
 author TEXT NOT NULL,
 claim_id TEXT NOT NULL,
 created_at TEXT NOT NULL,
 body_bytes INTEGER NOT NULL DEFAULT 0 CHECK(body_bytes>=0),
 sha256 TEXT NOT NULL DEFAULT '' CHECK(sha256='' OR length(sha256)=64),
 deleted_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS notes_task_order ON notes(task_id,event_id DESC);
CREATE INDEX IF NOT EXISTS notes_task_live_order ON notes(task_id,deleted_at,event_id DESC);
CREATE INDEX IF NOT EXISTS notes_with_bodies ON notes(event_id) WHERE deleted_at='' AND sha256<>'';
`

type Note struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	EventID   int64  `json:"event_id"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	ClaimID   string `json:"claim_id"`
	CreatedAt string `json:"created_at"`
	BodyBytes int64  `json:"body_bytes"`
	SHA256    string `json:"sha256"`
	DeletedAt string `json:"deleted_at,omitempty"`
}

const noteColumns = `id,task_id,event_id,title,author,claim_id,created_at,body_bytes,sha256,deleted_at`

func validNoteID(id string) bool {
	if len(id) != 33 || id[0] != 'N' {
		return false
	}
	_, err := hex.DecodeString(id[1:])
	return err == nil && strings.ToLower(id[1:]) == id[1:]
}

func validateNoteTitle(title string) (string, error) {
	title = strings.TrimSpace(title)
	if title == "" || !utf8.ValidString(title) || utf8.RuneCountInString(title) > 120 || strings.ContainsFunc(title, invalidTitleRune) {
		return "", failure("invalid_note_title", "note title must be a nonempty single line of at most 120 characters")
	}
	return title, nil
}

func invalidTitleRune(r rune) bool {
	return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
}

func legacyNoteTitle(text string) string {
	text = strings.TrimSpace(text)
	var title [120]rune
	count := 0
	for _, r := range text {
		if r == '\r' || r == '\n' || r == '\u2028' || r == '\u2029' {
			break
		}
		if count == len(title) {
			title[count-1] = '…'
			break
		}
		if unicode.IsControl(r) {
			r = ' '
		}
		title[count] = r
		count++
	}
	result := strings.TrimSpace(string(title[:count]))
	if result == "" {
		return "Progress update"
	}
	return result
}

func scanNote(row rowScanner) (Note, error) {
	var note Note
	var task int64
	err := row.Scan(&note.ID, &task, &note.EventID, &note.Title, &note.Author, &note.ClaimID, &note.CreatedAt, &note.BodyBytes, &note.SHA256, &note.DeletedAt)
	note.TaskID = taskID(task)
	return note, err
}

func getNote(q Queryer, id string) (Note, error) {
	if !validNoteID(id) {
		return Note{}, failure("invalid_note_id", "invalid note ID")
	}
	note, err := scanNote(q.QueryRow("SELECT "+noteColumns+" FROM notes WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Note{}, failure("not_found", "note "+id+" does not exist")
	}
	return note, classify(err)
}

func (s *Store) GetNote(id string) (Note, error) { return getNote(s.DB, id) }

func (s *Store) NotesPage(task string, limit int, before int64) ([]Note, bool, error) {
	n, err := taskNumber(task)
	if err != nil {
		return nil, false, err
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 200 || before < 0 {
		return nil, false, failure("invalid_limit", "note limit must be between 1 and 200 and before must be nonnegative")
	}
	var exists bool
	if err = s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)", n).Scan(&exists); err != nil {
		return nil, false, classify(err)
	}
	if !exists {
		return nil, false, failure("not_found", "task "+task+" does not exist")
	}
	query := "SELECT " + noteColumns + " FROM notes WHERE task_id=? AND deleted_at=''"
	args := []any{n}
	if before > 0 {
		query += " AND event_id<?"
		args = append(args, before)
	}
	query += " ORDER BY event_id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, false, classify(err)
	}
	defer rows.Close()
	notes := make([]Note, 0, limit)
	for rows.Next() {
		if len(notes) == limit {
			return notes, true, nil
		}
		note, err := scanNote(rows)
		if err != nil {
			return nil, false, classify(err)
		}
		notes = append(notes, note)
	}
	return notes, false, classify(rows.Err())
}

func (s *Store) AddNote(task, claim, title string, body io.Reader) (Note, error) {
	n, err := taskNumber(task)
	if err != nil {
		return Note{}, err
	}
	title, err = validateNoteTitle(title)
	if err != nil {
		return Note{}, err
	}
	unlock, err := s.noteLock(false)
	if err != nil {
		return Note{}, err
	}
	defer unlock()
	current, err := getTask(s.DB, n)
	if err != nil {
		return Note{}, err
	}
	if err = requireCurrentClaim(current, claim); err != nil {
		return Note{}, err
	}
	note := Note{ID: newID("N"), TaskID: task, Title: title, Author: current.Owner, ClaimID: claim, CreatedAt: timestamp(s.Now())}
	if body != nil {
		note.BodyBytes, note.SHA256, err = s.putBody(note.ID, body)
		if err != nil {
			return Note{}, err
		}
	}
	err = s.write(func(tx *sql.Tx) error {
		current, err := getTask(tx, n)
		if err != nil {
			return err
		}
		if err = requireCurrentClaim(current, claim); err != nil {
			return err
		}
		note.CreatedAt = timestamp(s.Now())
		if _, err = tx.Exec("UPDATE tasks SET progress=?,revision=revision+1,updated_at=?,last_activity_at=? WHERE id=?", title, note.CreatedAt, note.CreatedAt, n); err != nil {
			return err
		}
		note.EventID, err = s.emit(tx, n, "progress", title, map[string]any{"owner": current.Owner, "note_id": note.ID})
		if err != nil {
			return err
		}
		return insertNote(tx, note)
	})
	if err != nil && note.SHA256 != "" {
		var exists bool
		// A failed COMMIT can be ambiguous; never delete a possibly committed body's reference.
		if check := s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM notes WHERE id=?)", note.ID).Scan(&exists); check == nil && !exists {
			_ = s.removeBody(note.ID)
		}
	}
	return note, err
}

func insertNote(tx *sql.Tx, note Note) error {
	n, err := taskNumber(note.TaskID)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO notes("+noteColumns+") VALUES(?,?,?,?,?,?,?,?,?,?)", note.ID, n, note.EventID, note.Title, note.Author, note.ClaimID, note.CreatedAt, note.BodyBytes, note.SHA256, note.DeletedAt)
	return err
}

func (s *Store) DeleteNote(id, claim string) error {
	if !validNoteID(id) {
		return failure("invalid_note_id", "invalid note ID")
	}
	unlock, err := s.noteLock(false)
	if err != nil {
		return err
	}
	defer unlock()
	err = s.write(func(tx *sql.Tx) error {
		note, err := getNote(tx, id)
		if err != nil {
			return err
		}
		if note.DeletedAt != "" {
			return nil
		}
		n, _ := taskNumber(note.TaskID)
		task, err := getTask(tx, n)
		if err != nil {
			return err
		}
		if task.Status == "in_progress" {
			if err = requireCurrentClaim(task, claim); err != nil {
				return err
			}
		} else if claim != "" {
			return failure("conflict", "task has no active claim")
		}
		now := timestamp(s.Now())
		if _, err = tx.Exec("UPDATE notes SET title='',body_bytes=0,sha256='',deleted_at=? WHERE id=?", now, id); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE tasks SET progress='',revision=revision+1,updated_at=? WHERE id=? AND ?=(SELECT id FROM notes WHERE task_id=? ORDER BY event_id DESC LIMIT 1)", now, n, id, n); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE events SET message='Note deleted' WHERE id=?", note.EventID); err != nil {
			return err
		}
		_, err = s.emit(tx, n, "note_deleted", "Note "+id+" deleted", map[string]any{"note_id": id})
		return err
	})
	if err != nil {
		return err
	}
	return s.removeBody(id)
}

func (s *Store) migrateNotesTx(tx *sql.Tx) error {
	return s.migrateNotesFrom(tx, tx)
}

func (s *Store) migrateNotesFrom(source Queryer, target *sql.Tx) error {
	live, err := hasNoteBodies(target)
	if err != nil {
		return err
	}
	writer := s.newBodyWriter(!live)
	defer writer.Close()
	bodies := &legacyBodySource{q: source}
	defer bodies.Close()
	var after int64
	for {
		var event, row, task, length int64
		var author, created string
		err := source.QueryRow(`SELECT e.id,e.rowid,e.task_id,octet_length(e.message),COALESCE(json_extract(e.data,'$.owner'),''),e.created_at
 FROM events e WHERE e.kind='progress' AND e.id>? ORDER BY e.id LIMIT 1`, after).Scan(&event, &row, &task, &length, &author, &created)
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return err
		}
		after = event
		var exists bool
		if err = target.QueryRow("SELECT EXISTS(SELECT 1 FROM notes WHERE event_id=?)", event).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		note := Note{ID: newID("N"), TaskID: taskID(task), EventID: event, Author: author, CreatedAt: created}
		if err = func() error {
			reader, err := bodies.reader("events", "message", row, length)
			if err != nil {
				return err
			}
			defer reader.Close()
			prefix, err := reader.prefix()
			if err != nil {
				return err
			}
			note.Title = legacyNoteTitle(prefix)
			if title, e := validateNoteTitle(prefix); e == nil && int64(len(prefix)) == length && title == prefix {
				note.Title = title
				return nil
			}
			note.BodyBytes, note.SHA256, err = writer.Put(note.ID, reader)
			return err
		}(); err != nil {
			return err
		}
		if err = insertNote(target, note); err != nil {
			return err
		}
		if _, err = target.Exec("UPDATE events SET message=?,data=json_set(CASE WHEN json_type(data)='object' THEN data ELSE '{}' END,'$.note_id',?) WHERE id=?", note.Title, note.ID, event); err != nil {
			return err
		}
	}
	after = 0
	for {
		var task, row, length int64
		var owner, updated string
		err := source.QueryRow("SELECT id,rowid,octet_length(progress),owner,updated_at FROM tasks WHERE id>? AND octet_length(progress)>0 ORDER BY id LIMIT 1", after).Scan(&task, &row, &length, &owner, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			return writer.Commit()
		}
		if err != nil {
			return err
		}
		if err = func() error {
			reader, err := bodies.reader("tasks", "progress", row, length)
			if err != nil {
				return err
			}
			defer reader.Close()
			prefix, err := reader.prefix()
			if err != nil {
				return err
			}
			latest, latestErr := scanNote(target.QueryRow("SELECT "+noteColumns+" FROM notes WHERE task_id=? ORDER BY event_id DESC LIMIT 1", task))
			if latestErr != nil && !errors.Is(latestErr, sql.ErrNoRows) {
				return latestErr
			}
			matches := latestErr == nil && latest.SHA256 == "" && int64(len(prefix)) == length && latest.Title == prefix
			if latestErr == nil && latest.SHA256 != "" && latest.BodyBytes == length {
				hash := sha256.New()
				if _, err = io.CopyBuffer(hash, reader, make([]byte, noteChunkBytes)); err != nil {
					return err
				}
				reader.position = 0
				matches = hex.EncodeToString(hash.Sum(nil)) == latest.SHA256
			}
			if !matches {
				latest = Note{ID: newID("N"), TaskID: taskID(task), Title: legacyNoteTitle(prefix), Author: owner, CreatedAt: updated}
				if title, e := validateNoteTitle(prefix); e == nil && int64(len(prefix)) == length && title == prefix {
					latest.Title = title
				} else {
					latest.BodyBytes, latest.SHA256, err = writer.Put(latest.ID, reader)
					if err != nil {
						return err
					}
				}
				latest.EventID, err = s.emit(target, task, "progress", latest.Title, map[string]any{"owner": owner, "note_id": latest.ID, "migrated": true})
				if err != nil {
					return err
				}
				if err = insertNote(target, latest); err != nil {
					return err
				}
			}
			_, err = target.Exec("UPDATE tasks SET progress=? WHERE id=?", latest.Title, task)
			return err
		}(); err != nil {
			return err
		}
		after = task
	}
}
