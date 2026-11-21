package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"time"
)

type ScratchArea struct {
	ID                string `json:"id"`
	CreatedAt         string `json:"created_at"`
	UnreferencedAt    string `json:"unreferenced_at,omitempty"`
	CleanupToken      string `json:"cleanup_token,omitempty"`
	RemovedAt         string `json:"removed_at,omitempty"`
	CleanupLeaseUntil string `json:"cleanup_lease_until,omitempty"`
	CleanupCursor     string `json:"-"`
	CleanupFinalizing bool   `json:"-"`
}

var areaIDPattern = regexp.MustCompile(`^A[0-9a-f]{32}$`)

func ValidAreaID(id string) bool { return areaIDPattern.MatchString(id) }

func (s *Store) scratchOwner(tx *sql.Tx, id int64, claim string) error {
	var status, current string
	err := tx.QueryRow("SELECT status,claim_id FROM tasks WHERE id=?", id).Scan(&status, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return failure("not_found", "task not found")
	}
	if err != nil {
		return err
	}
	if status != "pending" && status != "in_progress" {
		return failure("terminal", "scratch can only be attached to current work")
	}
	if status == "in_progress" && (claim == "" || claim != current) {
		return failure("stale_claim", "a current claim is required")
	}
	if status == "pending" && claim != "" {
		return failure("stale_claim", "task has no current claim")
	}
	return nil
}
func (s *Store) ScratchCreate(task, claim, area string) error {
	n, err := taskNumber(task)
	if err != nil {
		return err
	}
	if !ValidAreaID(area) {
		return failure("invalid_id", "invalid scratch area ID")
	}
	return s.write(func(tx *sql.Tx) error {
		if err := s.scratchOwner(tx, n, claim); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO scratch_areas(id,created_at) VALUES(?,?)", area, timestamp(s.Now())); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO scratch_refs(task_id,area_id) VALUES(?,?)", n, area); err != nil {
			return err
		}
		_, err := s.emit(tx, n, "scratch_attached", "scratch area attached", map[string]string{"area_id": area})
		return err
	})
}
func (s *Store) ScratchAttach(task, claim, area string) error {
	n, err := taskNumber(task)
	if err != nil {
		return err
	}
	if !ValidAreaID(area) {
		return failure("invalid_id", "invalid scratch area ID")
	}
	return s.write(func(tx *sql.Tx) error {
		if err := s.scratchOwner(tx, n, claim); err != nil {
			return err
		}
		var token, removed sql.NullString
		err := tx.QueryRow("SELECT cleanup_token,removed_at FROM scratch_areas WHERE id=?", area).Scan(&token, &removed)
		if errors.Is(err, sql.ErrNoRows) {
			return failure("not_found", "scratch area not found")
		}
		if err != nil {
			return err
		}
		if token.Valid || removed.Valid {
			return failure("conflict", "scratch area is being removed or has expired")
		}
		r, err := tx.Exec("INSERT OR IGNORE INTO scratch_refs(task_id,area_id) VALUES(?,?)", n, area)
		if err != nil {
			return err
		}
		count, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if count > 0 {
			if _, err := s.emit(tx, n, "scratch_attached", "scratch area attached", map[string]string{"area_id": area}); err != nil {
				return err
			}
		}
		_, err = tx.Exec("UPDATE scratch_areas SET unreferenced_at=NULL WHERE id=?", area)
		return err
	})
}

const scratchColumns = `a.id,a.created_at,COALESCE(a.unreferenced_at,''),COALESCE(a.cleanup_token,''),COALESCE(a.removed_at,''),COALESCE(c.lease_until,''),COALESCE(c.cursor,'.'),COALESCE(c.finalizing,0)`

func scanScratch(row taskScanner) (ScratchArea, error) {
	var a ScratchArea
	err := row.Scan(&a.ID, &a.CreatedAt, &a.UnreferencedAt, &a.CleanupToken, &a.RemovedAt, &a.CleanupLeaseUntil, &a.CleanupCursor, &a.CleanupFinalizing)
	return a, err
}
func (s *Store) ScratchGet(task, area string) (ScratchArea, error) {
	n, err := taskNumber(task)
	if err != nil {
		return ScratchArea{}, err
	}
	if !ValidAreaID(area) {
		return ScratchArea{}, failure("invalid_id", "invalid scratch area ID")
	}
	a, err := scanScratch(s.DB.QueryRow(`SELECT `+scratchColumns+` FROM scratch_refs r JOIN scratch_areas a ON a.id=r.area_id LEFT JOIN scratch_cleanup_state c ON c.area_id=a.id WHERE r.task_id=? AND r.area_id=?`, n, area))
	if errors.Is(err, sql.ErrNoRows) {
		return ScratchArea{}, failure("not_found", "scratch reference not found")
	}
	return a, err
}

func (s *Store) ScratchListPage(task string, limit, offset int) ([]ScratchArea, bool, error) {
	n, err := taskNumber(task)
	if err != nil {
		return nil, false, err
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, false, failure("invalid_limit", "limit must be 1..200 and offset nonnegative")
	}
	result := []ScratchArea{}
	more := false
	err = s.read(func(q Queryer) error {
		var exists bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM tasks WHERE id=?)", n).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return failure("not_found", "task "+task+" does not exist")
		}
		rows, err := q.Query(`SELECT `+scratchColumns+` FROM scratch_areas a JOIN scratch_refs r ON r.area_id=a.id LEFT JOIN scratch_cleanup_state c ON c.area_id=a.id WHERE r.task_id=? ORDER BY a.id LIMIT ? OFFSET ?`, n, limit+1, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(result) == limit {
				more = true
				break
			}
			a, err := scanScratch(rows)
			if err != nil {
				return err
			}
			result = append(result, a)
		}
		return rows.Err()
	})
	return result, more, err
}

const scratchCleanupLease = 30 * time.Second

func (s *Store) ScratchCleanupCandidates(grace time.Duration, reserve bool, limit int) ([]ScratchArea, error) {
	if grace < 0 {
		return nil, failure("invalid_duration", "grace cannot be negative")
	}
	if limit < 0 {
		return nil, failure("invalid_input", "limit cannot be negative")
	}
	if limit == 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	result := []ScratchArea{}
	collect := func(q Queryer) error {
		now := s.Now()
		rows, err := q.Query(`SELECT `+scratchColumns+` FROM scratch_areas a LEFT JOIN scratch_cleanup_state c ON c.area_id=a.id
   WHERE a.removed_at IS NULL AND (a.cleanup_token IS NOT NULL OR (a.unreferenced_at IS NOT NULL AND a.unreferenced_at<=?))
   AND (c.area_id IS NULL OR c.lease_until<=?)
   AND NOT EXISTS(SELECT 1 FROM scratch_refs r JOIN tasks t ON t.id=r.task_id WHERE r.area_id=a.id AND t.status IN ('pending','in_progress'))
   ORDER BY a.unreferenced_at,a.id LIMIT ?`, timestamp(now.Add(-grace)), timestamp(now), limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			a, err := scanScratch(rows)
			if err != nil {
				rows.Close()
				return err
			}
			result = append(result, a)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		return nil
	}
	if !reserve {
		err := s.read(collect)
		return result, err
	}
	err := s.write(func(tx *sql.Tx) error {
		if err := collect(tx); err != nil {
			return err
		}
		now := s.Now()
		for i := range result {
			a := &result[i]
			a.CleanupToken = newID("D")
			a.CleanupLeaseUntil = timestamp(now.Add(scratchCleanupLease))
			if _, err := tx.Exec("UPDATE scratch_areas SET cleanup_token=? WHERE id=?", a.CleanupToken, a.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO scratch_cleanup_state(area_id,lease_token,lease_until) VALUES(?,?,?) ON CONFLICT(area_id) DO UPDATE SET lease_token=excluded.lease_token,lease_until=excluded.lease_until`, a.ID, a.CleanupToken, a.CleanupLeaseUntil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
func validateScratchCleanup(id, token string) error {
	if !ValidAreaID(id) {
		return failure("invalid_id", "invalid scratch area ID")
	}
	if token == "" {
		return failure("conflict", "scratch cleanup claim is required")
	}
	return nil
}
func (s *Store) ScratchCleanupSave(id, token, cursor string, finalizing bool) error {
	if err := validateScratchCleanup(id, token); err != nil {
		return err
	}
	if !filepath.IsLocal(cursor) || filepath.Clean(cursor) != cursor || (finalizing && cursor != ".") {
		return failure("invalid_input", "invalid scratch cleanup cursor")
	}
	return s.write(func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE scratch_cleanup_state SET cursor=?,finalizing=? WHERE area_id=? AND lease_token=? AND lease_until>? AND (finalizing=0 OR ?=1)
   AND EXISTS(SELECT 1 FROM scratch_areas a WHERE a.id=area_id AND a.cleanup_token=? AND a.removed_at IS NULL)
   AND NOT EXISTS(SELECT 1 FROM scratch_refs r JOIN tasks t ON t.id=r.task_id WHERE r.area_id=scratch_cleanup_state.area_id AND t.status IN ('pending','in_progress'))`, cursor, finalizing, id, token, timestamp(s.Now()), finalizing, token)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return failure("conflict", "scratch cleanup claim changed or expired")
		}
		return nil
	})
}
func (s *Store) ScratchCleanupRelease(id, token string) error {
	if err := validateScratchCleanup(id, token); err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		r, err := tx.Exec(`UPDATE scratch_cleanup_state SET lease_token='',lease_until='' WHERE area_id=? AND lease_token=? AND EXISTS(SELECT 1 FROM scratch_areas a WHERE a.id=area_id AND a.cleanup_token=? AND a.removed_at IS NULL)`, id, token, token)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return failure("conflict", "scratch cleanup claim changed")
		}
		return nil
	})
}
func (s *Store) ScratchCleanupFinish(id, token string) error {
	if err := validateScratchCleanup(id, token); err != nil {
		return err
	}
	return s.write(func(tx *sql.Tx) error {
		now := timestamp(s.Now())
		r, err := tx.Exec(`UPDATE scratch_areas SET removed_at=?,cleanup_token=NULL WHERE id=? AND cleanup_token=? AND removed_at IS NULL
   AND EXISTS(SELECT 1 FROM scratch_cleanup_state c WHERE c.area_id=scratch_areas.id AND c.lease_token=? AND c.lease_until>?)
   AND NOT EXISTS(SELECT 1 FROM scratch_refs r JOIN tasks t ON t.id=r.task_id WHERE r.area_id=scratch_areas.id AND t.status IN ('pending','in_progress'))`, now, id, token, token, now)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return failure("conflict", "scratch cleanup claim changed, expired, or has a live reference")
		}
		_, err = tx.Exec("DELETE FROM scratch_cleanup_state WHERE area_id=? AND lease_token=?", id, token)
		return err
	})
}
