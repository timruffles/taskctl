package store

import (
	"database/sql"
	"fmt"
	"testing"
)

func seedCoreLineage(t *testing.T, s *Store, depth int) Task {
	t.Helper()
	err := s.write(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(`INSERT INTO tasks(id,title,brief,status,origin_id,parent_id,owner,claim_id,created_at,updated_at,last_activity_at)
 VALUES(?,?,?,'superseded',1,?,'','',?,?,?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		now := timestamp(s.Now())
		for id := 1; id <= depth; id++ {
			var parent any
			if id > 1 {
				parent = id - 1
			}
			if _, err := stmt.Exec(id, fmt.Sprintf("Definition %d", id), "Current remaining scope", parent, now, now, now); err != nil {
				return err
			}
		}
		_, err = tx.Exec("UPDATE tasks SET status='in_progress',owner='worker',claim_id='Cdeep' WHERE id=?", depth)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := s.Get(taskID(int64(depth)))
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func TestCoreDeepCompletionNotifiesEveryAncestorDependentOnce(t *testing.T) {
	s := newTaskTestStore(t)
	leaf := seedCoreLineage(t, s, 1000)
	rootDependent := mustCreateTask(t, s, CreateSpec{Title: "Original scope dependent", Prerequisites: []string{"T1"}})
	middleDependent := mustCreateTask(t, s, CreateSpec{Title: "Intermediate scope dependent", Prerequisites: []string{"T500"}})
	multipleDependent := mustCreateTask(t, s, CreateSpec{Title: "Multiple affected prerequisites", Prerequisites: []string{"T1", "T500", leaf.ID}})
	for _, task := range []Task{rootDependent, middleDependent, multipleDependent} {
		ready, err := availableWith(s.DB, mustTaskNumber(t, task.ID))
		if err != nil || ready {
			t.Fatalf("task was ready before completion: %s ready=%v error=%v", task.ID, ready, err)
		}
	}
	mustCompleteTask(t, s, leaf)
	for _, task := range []Task{rootDependent, middleDependent, multipleDependent} {
		var count int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM events WHERE task_id=? AND kind='available'", mustTaskNumber(t, task.ID)).Scan(&count); err != nil || count != 1 {
			t.Fatalf("availability events for %s = %d, %v", task.ID, count, err)
		}
	}
	items, total, err := s.List("available", nil, 20, 0)
	if err != nil || total != 3 || len(items) != 3 {
		t.Fatalf("available after completion: %v total=%d error=%v", items, total, err)
	}
	if outcome, err := s.Outcome("T1"); err != nil || outcome != "completed" {
		t.Fatalf("original outcome=%s error=%v", outcome, err)
	}
}

func mustTaskNumber(t *testing.T, id string) int64 {
	t.Helper()
	n, err := taskNumber(id)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCoreAvailabilityTreatsEmptyEvolvedAsPending(t *testing.T) {
	s := newTaskTestStore(t)
	original := mustCreateTask(t, s, CreateSpec{Title: "Empty historical definition"})
	if _, err := s.DB.Exec("UPDATE tasks SET status='superseded' WHERE id=?", mustTaskNumber(t, original.ID)); err != nil {
		t.Fatal(err)
	}
	dependent := mustCreateTask(t, s, CreateSpec{Title: "Wait for evidence of completion", Prerequisites: []string{original.ID}})
	items, total, err := s.List("blocked", nil, 20, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != dependent.ID {
		t.Fatalf("empty superseded task unblocked dependent: %v total=%d error=%v", items, total, err)
	}
	if outcome, err := s.Outcome(original.ID); err != nil || outcome != "pending" {
		t.Fatalf("empty original outcome=%s error=%v", outcome, err)
	}
}

func TestCoreWholeGraphValidationWithMixedEdges(t *testing.T) {
	for _, test := range []struct {
		name  string
		edges string
		cycle bool
	}{
		{"empty", "", false},
		{"diamond and duplicate edge", `INSERT INTO prerequisites VALUES(1,2),(1,3),(2,4),(3,4); UPDATE tasks SET parent_id=1 WHERE id=2;`, false},
		{"prerequisite cycle", `INSERT INTO prerequisites VALUES(1,2),(2,3),(3,1);`, true},
		{"replacement cycle", `UPDATE tasks SET parent_id=2 WHERE id=1; UPDATE tasks SET parent_id=1 WHERE id=2;`, true},
		{"mixed cycle", `INSERT INTO prerequisites VALUES(2,3),(3,1); UPDATE tasks SET parent_id=1 WHERE id=2;`, true},
		{"acyclic prefix before cycle", `INSERT INTO prerequisites VALUES(1,2),(2,3),(3,4),(4,3);`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := newTaskTestStore(t)
			for i := 0; i < 5; i++ {
				mustCreateTask(t, s, CreateSpec{Title: "Graph node"})
			}
			if test.edges != "" {
				if _, err := s.DB.Exec(test.edges); err != nil {
					t.Fatal(err)
				}
			}
			err := s.write(checkTaskCycles)
			if test.cycle {
				assertCoreError(t, err, "conflict")
			} else if err != nil {
				t.Fatal(err)
			}
			var leftovers int
			if err := s.DB.QueryRow("SELECT COUNT(*) FROM sqlite_temp_schema WHERE name LIKE 'cycle_%'").Scan(&leftovers); err != nil || leftovers != 0 {
				t.Fatalf("cycle validation left temporary state: count=%d error=%v", leftovers, err)
			}
		})
	}
}

func TestCoreWholeGraphValidationHandlesDeepLineage(t *testing.T) {
	s := newTaskTestStore(t)
	seedCoreLineage(t, s, 1000)
	if err := s.write(checkTaskCycles); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO prerequisites(task_id,prerequisite_id) VALUES(1000,1)"); err != nil {
		t.Fatal(err)
	}
	assertCoreError(t, s.write(checkTaskCycles), "conflict")
}

func TestCoreHistoryPaginationReportsMore(t *testing.T) {
	s := newTaskTestStore(t)
	leaf := seedCoreLineage(t, s, 205)
	page, more, err := s.HistoryPage(leaf.ID, 0, 0)
	if err != nil || !more || len(page) != 20 || page[0].ID != "T1" || page[19].ID != "T20" {
		t.Fatalf("default page=%v more=%v error=%v", page, more, err)
	}
	page, more, err = s.HistoryPage(leaf.ID, 1000, 0)
	if err != nil || !more || len(page) != 200 || page[199].ID != "T200" {
		t.Fatalf("capped page count=%d more=%v error=%v", len(page), more, err)
	}
	page, more, err = s.HistoryPage(leaf.ID, 20, 200)
	if err != nil || more || len(page) != 5 || page[0].ID != "T201" || page[4].ID != "T205" {
		t.Fatalf("last page=%v more=%v error=%v", page, more, err)
	}
	page, more, err = s.HistoryPage(leaf.ID, 20, 205)
	if err != nil || more || len(page) != 0 {
		t.Fatalf("past end=%v more=%v error=%v", page, more, err)
	}
	_, _, err = s.HistoryPage("T999", 20, 205)
	assertCoreError(t, err, "not_found")
	for _, pagination := range [][2]int{{-1, 0}, {20, -1}} {
		_, _, err = s.HistoryPage(leaf.ID, pagination[0], pagination[1])
		assertCoreError(t, err, "invalid_input")
	}
}
