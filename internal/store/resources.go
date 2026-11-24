package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const resourceSchema = `
CREATE TABLE IF NOT EXISTS resources (
 id INTEGER PRIMARY KEY,
 name TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS task_resource_requirements (
 task_id INTEGER NOT NULL REFERENCES tasks(id),
 resource_id INTEGER NOT NULL REFERENCES resources(id),
 PRIMARY KEY(task_id,resource_id)
);
CREATE INDEX IF NOT EXISTS resource_waiters ON task_resource_requirements(resource_id,task_id);
CREATE TABLE IF NOT EXISTS resource_holds (
 resource_id INTEGER PRIMARY KEY,
 task_id INTEGER NOT NULL,
 claim_id TEXT NOT NULL,
 FOREIGN KEY(task_id,resource_id) REFERENCES task_resource_requirements(task_id,resource_id)
);
CREATE INDEX IF NOT EXISTS resource_holder ON resource_holds(task_id);
`

type Resource struct {
	Name   string `json:"name"`
	Holder string `json:"holder,omitempty"`
}

func (s *Store) CreateResource(name string) (Resource, error) {
	name, err := validateLabel(name, "resource name")
	if err != nil {
		return Resource{}, err
	}
	resource := Resource{Name: name}
	err = s.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec("INSERT INTO resources(name) VALUES(?) ON CONFLICT(name) DO NOTHING", name); err != nil {
			return err
		}
		return tx.QueryRow(`SELECT COALESCE('T'||h.task_id,'') FROM resources r
 LEFT JOIN resource_holds h ON h.resource_id=r.id WHERE r.name=?`, name).Scan(&resource.Holder)
	})
	return resource, err
}

func (s *Store) ListResources(limit, offset int) ([]Resource, bool, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, false, failure("invalid_input", "resource limit must be 1..200 and offset must be nonnegative")
	}
	rows, err := s.DB.Query(`SELECT r.name,COALESCE('T'||h.task_id,'')
 FROM resources r LEFT JOIN resource_holds h ON h.resource_id=r.id ORDER BY r.name LIMIT ? OFFSET ?`, limit+1, offset)
	if err != nil {
		return nil, false, classify(err)
	}
	defer rows.Close()
	items := []Resource{}
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.Name, &r.Holder); err != nil {
			return nil, false, classify(err)
		}
		items = append(items, r)
	}
	if len(items) > limit {
		return items[:limit], true, classify(rows.Err())
	}
	return items, false, classify(rows.Err())
}

func (s *Store) SetResourceRequirement(id, name string, remove bool) (Task, error) {
	return s.taskWrite(id, func(tx *sql.Tx, n int64, t Task) error {
		if t.Status != "pending" {
			return failure("conflict", "resource requirements can only change while a task is pending")
		}
		resource, err := findResource(tx, name)
		if err != nil {
			return err
		}
		query, kind := "INSERT OR IGNORE INTO task_resource_requirements(task_id,resource_id) VALUES(?,?)", "resource_required"
		if remove {
			query, kind = "DELETE FROM task_resource_requirements WHERE task_id=? AND resource_id=?", "resource_unrequired"
		}
		wasAvailable, err := availableWith(tx, n)
		if err != nil {
			return err
		}
		result, err := tx.Exec(query, n, resource)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		if err := s.bumpTask(tx, n); err != nil {
			return err
		}
		if _, err := s.emit(tx, n, kind, name, map[string]string{"resource": name}); err != nil {
			return err
		}
		if remove && !wasAvailable {
			return s.emitAvailableTasks(tx, "t.id=?", n)
		}
		return nil
	})
}

func (s *Store) ReleaseResource(name, claim string) error {
	return s.write(func(tx *sql.Tx) error {
		resource, err := findResource(tx, name)
		if err != nil {
			return err
		}
		var task int64
		err = tx.QueryRow(`DELETE FROM resource_holds WHERE resource_id=? AND claim_id=?
 RETURNING task_id`, resource, claim).Scan(&task)
		if errors.Is(err, sql.ErrNoRows) {
			return failure("stale_claim", "claim does not hold resource "+name)
		}
		if err != nil {
			return err
		}
		if _, err = s.emit(tx, task, "resource_released", name, map[string]string{"resource": name}); err != nil {
			return err
		}
		return s.emitAvailableTasks(tx, resourceWaiters, releasedResources([]int64{resource}))
	})
}

func findResource(tx *sql.Tx, name string) (int64, error) {
	var id int64
	name, err := validateLabel(name, "resource name")
	if err != nil {
		return 0, err
	}
	err = tx.QueryRow("SELECT id FROM resources WHERE name=?", name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, failure("not_found", "resource "+name+" does not exist")
	}
	return id, err
}

func acquireResources(tx *sql.Tx, task int64, claim string) error {
	var resource string
	var holder int64
	err := tx.QueryRow(`SELECT r.name,h.task_id
 FROM task_resource_requirements req JOIN resource_holds h USING(resource_id)
 JOIN resources r ON r.id=req.resource_id WHERE req.task_id=? ORDER BY r.name LIMIT 1`, task).Scan(&resource, &holder)
	if err == nil {
		return failure("blocked", fmt.Sprintf("task %s is blocked by resource %s held by %s", taskID(task), resource, taskID(holder)))
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.Exec(`INSERT INTO resource_holds(resource_id,task_id,claim_id)
 SELECT resource_id,task_id,? FROM task_resource_requirements WHERE task_id=?`, claim, task)
	return err
}

func clearResourceHolds(tx *sql.Tx, task int64) ([]int64, error) {
	rows, err := tx.Query("DELETE FROM resource_holds WHERE task_id=? RETURNING resource_id", task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const resourceWaiters = `t.id IN (SELECT task_id FROM task_resource_requirements
 WHERE resource_id IN (SELECT value FROM json_each(?)))`

func releasedResources(ids []int64) string {
	data, _ := json.Marshal(ids)
	return string(data)
}
