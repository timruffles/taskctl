package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type chaosNode struct {
	id, title, brief, status, origin, parent, owner, claim, progress, result string
	revision                                                                 int64
	prerequisites, children                                                  []string
}

type chaosModel map[string]*chaosNode

func chaosSetting(t *testing.T, key string, fallback int) int {
	t.Helper()
	if value := os.Getenv(key); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			t.Fatalf("%s must be a positive integer", key)
		}
		return n
	}
	return fallback
}

func (m chaosModel) outcome(id string) string {
	n := m[id]
	if n.status == "cancelled" || n.status == "completed" {
		return n.status
	}
	if n.status != "evolved" {
		return "pending"
	}
	pending := len(n.children) == 0
	for _, child := range n.children {
		switch m.outcome(child) {
		case "cancelled":
			return "cancelled"
		case "pending":
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "completed"
}
func (m chaosModel) ready(id string) bool {
	n := m[id]
	if n.status != "pending" {
		return false
	}
	return m.prerequisitesComplete(id)
}
func (m chaosModel) prerequisitesComplete(id string) bool {
	for _, p := range m[id].prerequisites {
		if m.outcome(p) != "completed" {
			return false
		}
	}
	return true
}
func (m chaosModel) available() map[string]bool {
	result := map[string]bool{}
	for id := range m {
		if m.ready(id) {
			result[id] = true
		}
	}
	return result
}
func (m chaosModel) cycle(from, to string) bool {
	seen := map[string]bool{}
	var reaches func(string) bool
	reaches = func(id string) bool {
		if id == from {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		for _, next := range append(append([]string{}, m[id].prerequisites...), m[id].children...) {
			if reaches(next) {
				return true
			}
		}
		return false
	}
	return reaches(to)
}
func chaosSet(ids []string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

func chaosAll(t *testing.T, s *Store) map[string]Task {
	t.Helper()
	rows, err := s.DB.Query("SELECT " + taskProjection + " FROM tasks t ORDER BY t.id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	all := map[string]Task{}
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			t.Fatal(err)
		}
		all[task.ID] = task
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return all
}

func chaosIntegrity(t *testing.T, s *Store) {
	t.Helper()
	var integrity string
	if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%q err=%v", integrity, err)
	}
	rows, err := s.DB.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign-key violation after chaos")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func chaosCheckModel(t *testing.T, s *Store, m chaosModel, frozen map[string]Task, seed int64, step int) {
	t.Helper()
	all := chaosAll(t, s)
	if len(all) != len(m) {
		t.Fatalf("seed=%d step=%d task count=%d want=%d", seed, step, len(all), len(m))
	}
	for id, n := range m {
		a := all[id]
		if a.Status != n.status || a.Title != n.title || a.Brief != n.brief || a.OriginID != n.origin || a.ParentID != n.parent || a.Owner != n.owner || a.ClaimID != n.claim || a.Progress != n.progress || a.Result != n.result || a.Revision != n.revision || !reflect.DeepEqual(chaosSet(a.Prerequisites), chaosSet(n.prerequisites)) {
			t.Fatalf("seed=%d step=%d %s differs from independent model\nactual=%+v\nexpected=%+v", seed, step, id, a, n)
		}
		if previous, ok := frozen[id]; ok {
			if !reflect.DeepEqual(previous, a) {
				t.Fatalf("seed=%d step=%d changed immutable %s", seed, step, id)
			}
		} else if n.status == "completed" || n.status == "cancelled" || n.status == "evolved" {
			frozen[id] = a
		}
		outcome, err := s.Outcome(id)
		if err != nil || outcome != m.outcome(id) {
			t.Fatalf("seed=%d step=%d outcome %s=%q want=%q err=%v", seed, step, id, outcome, m.outcome(id), err)
		}
	}
	available, total, err := s.List("available", nil, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := m.available()
	if total != len(want) || len(available) != len(want) {
		t.Fatalf("seed=%d step=%d available count=%d want=%d", seed, step, total, len(want))
	}
	for _, task := range available {
		if !want[task.ID] {
			t.Fatalf("seed=%d step=%d unavailable task %s was listed", seed, step, task.ID)
		}
	}
}

func TestChaosSeededTaskStateMachine(t *testing.T) {
	seeds := chaosSetting(t, "TASKCTL_CHAOS_SEEDS", 8)
	steps := chaosSetting(t, "TASKCTL_CHAOS_STEPS", 120)
	base := int64(chaosSetting(t, "TASKCTL_CHAOS_SEED", 20261121))
	for index := 0; index < seeds; index++ {
		seed := base + int64(index)
		t.Run(strconv.FormatInt(seed, 10), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			path := filepath.Join(t.TempDir(), "tasks.sqlite")
			now := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
			stores := make([]*Store, 3)
			for i := range stores {
				s, err := Open(path, "chaos-model")
				if err != nil {
					t.Fatal(err)
				}
				s.Now = func() time.Time { return now }
				stores[i] = s
			}
			defer func() {
				for _, s := range stores {
					s.Close()
				}
			}()
			m := chaosModel{}
			frozen := map[string]Task{}
			var ids []string
			var stale [][2]string
			tokens := map[string]bool{}
			observations, immutableChecks, integrityChecks, accepted, rejected, reopens := 0, 0, 0, 0, 0, 0
			var operationCounts [15]int
			create := func(s *Store, title string) {
				task, err := s.Create(CreateSpec{Title: title, Brief: "Self-contained work"})
				if err != nil {
					t.Fatal(err)
				}
				m[task.ID] = &chaosNode{id: task.ID, title: title, brief: "Self-contained work", status: "pending", origin: task.ID, revision: 1}
				ids = append(ids, task.ID)
			}
			for i := 0; i < 6; i++ {
				create(stores[0], fmt.Sprintf("Initial %d", i))
			}
			for step := 0; step < steps; step++ {
				now = now.Add(time.Second)
				s := stores[rng.Intn(len(stores))]
				id := ids[rng.Intn(len(ids))]
				if rng.Intn(5) != 0 {
					var current []string
					for _, candidate := range ids {
						if m[candidate].status == "pending" || m[candidate].status == "in_progress" {
							current = append(current, candidate)
						}
					}
					if len(current) > 0 {
						id = current[rng.Intn(len(current))]
					}
				}
				n := m[id]
				editable := n.status == "pending" || n.status == "in_progress"
				owned := n.status == "in_progress"
				before := m.available()
				beforeEvents := coreEventCount(t, s)
				var through int64
				if err := s.DB.QueryRow("SELECT COALESCE(MAX(id),0) FROM events").Scan(&through); err != nil {
					t.Fatal(err)
				}
				op := rng.Intn(15)
				operationCounts[op]++
				expected := true
				var err error
				var task Task
				remember := func() {
					if n.claim != "" {
						stale = append(stale, [2]string{id, n.claim})
					}
				}
				adopt := func(token string) {
					if token == "" || tokens[token] {
						t.Fatalf("seed=%d step=%d reused/empty token", seed, step)
					}
					tokens[token] = true
					n.claim = token
				}
				switch op {
				case 0:
					if len(ids) < 90 {
						create(s, fmt.Sprintf("New work %d", step))
					}
				case 1:
					expected = m.ready(id)
					owner := fmt.Sprintf("worker-%d", rng.Intn(8))
					task, err = s.Claim(id, owner)
					if err == nil {
						n.status = "in_progress"
						n.owner = owner
						n.revision++
						adopt(task.ClaimID)
					}
				case 2:
					expected = owned
					task, err = s.Takeover(id, n.claim, "replacement-worker", "Previous process interrupted")
					if err == nil {
						remember()
						n.owner = "replacement-worker"
						n.revision++
						adopt(task.ClaimID)
					}
				case 3:
					expected = owned
					_, err = s.Release(id, n.claim)
					if err == nil {
						remember()
						n.status = "pending"
						n.owner = ""
						n.claim = ""
						n.revision++
					}
				case 4:
					expected = owned && m.prerequisitesComplete(id)
					summary := fmt.Sprintf("Verified %d", step)
					_, err = s.Complete(id, n.claim, summary)
					if err == nil {
						remember()
						n.status = "completed"
						n.owner = ""
						n.claim = ""
						n.result = summary
						n.revision++
					}
				case 5:
					expected = editable
					_, err = s.Cancel(id, n.claim, "No longer needed")
					if err == nil {
						remember()
						n.status = "cancelled"
						n.owner = ""
						n.claim = ""
						n.result = "No longer needed"
						n.revision++
					}
				case 6:
					expected = owned
					text := fmt.Sprintf("Checkpoint %d", step)
					_, err = s.Progress(id, n.claim, text)
					if err == nil {
						n.progress = text
						n.revision++
					}
				case 7:
					expected = owned
					err = s.Heartbeat(id, n.claim)
				case 8:
					revision := n.revision
					if rng.Intn(3) == 0 {
						revision++
					}
					expected = editable && revision == n.revision
					brief := fmt.Sprintf("Remaining work at step %d", step)
					_, err = s.Edit(id, n.claim, revision, nil, &brief)
					if err == nil {
						n.brief = brief
						n.revision++
					}
				case 9:
					if len(ids) >= 90 {
						break
					}
					expected = editable
					summary := fmt.Sprintf("Completed chunk %d", step)
					spec := EvolveSpec{Summary: summary, Replacements: []CreateSpec{{Key: "first", Title: "Remaining first", Brief: "Independent remaining first work"}, {Key: "second", Title: "Remaining second", Brief: "Independent remaining second work", Prerequisites: []string{"@first"}}}}
					if owned && rng.Intn(2) == 0 {
						spec.ContinueWith = "first"
						expected = expected && m.prerequisitesComplete(id)
					}
					var children []Task
					children, err = s.Evolve(id, n.claim, n.revision, spec)
					if err == nil {
						oldOwner := n.owner
						remember()
						n.status = "evolved"
						n.owner = ""
						n.claim = ""
						n.result = summary
						n.revision++
						for i, child := range children {
							p := append([]string{}, n.prerequisites...)
							if i == 1 {
								p = append(p, children[0].ID)
							}
							node := &chaosNode{id: child.ID, title: spec.Replacements[i].Title, brief: spec.Replacements[i].Brief, status: "pending", origin: n.origin, parent: id, revision: 1, prerequisites: p}
							if i == 0 && spec.ContinueWith != "" {
								node.status = "in_progress"
								node.owner = oldOwner
								node.revision = 2
								if child.ClaimID == "" || tokens[child.ClaimID] {
									t.Fatal("reused successor claim")
								}
								node.claim = child.ClaimID
								tokens[child.ClaimID] = true
							}
							m[child.ID] = node
							ids = append(ids, child.ID)
							n.children = append(n.children, child.ID)
						}
					}
				case 10, 11:
					prerequisite := ids[rng.Intn(len(ids))]
					exists := false
					for _, p := range n.prerequisites {
						if p == prerequisite {
							exists = true
						}
					}
					remove := op == 11
					expected = editable && (remove || !m.cycle(id, prerequisite))
					err = s.AddPrerequisite(id, n.claim, prerequisite, remove)
					if err == nil && remove && exists {
						out := []string{}
						for _, p := range n.prerequisites {
							if p != prerequisite {
								out = append(out, p)
							}
						}
						n.prerequisites = out
						n.revision++
					} else if err == nil && !remove && !exists {
						n.prerequisites = append(n.prerequisites, prerequisite)
						n.revision++
					}
				case 12:
					expected = false
					target, token := id, "Cnever-current"
					if len(stale) > 0 {
						pair := stale[rng.Intn(len(stale))]
						target, token = pair[0], pair[1]
					}
					_, err = s.Progress(target, token, "Zombie process must not change work")
				case 13:
					reopens++
					index := rng.Intn(len(stores))
					if err = stores[index].Close(); err != nil {
						t.Fatal(err)
					}
					stores[index], err = Open(path, "chaos-model")
					if err != nil {
						t.Fatal(err)
					}
					stores[index].Now = func() time.Time { return now }
					s = stores[index]
				case 14:
					expected = editable
					err = s.MetadataSet(id, n.claim, "checkpoint", strconv.Itoa(step))
					if err == nil {
						n.revision++
					}
				}
				if (err == nil) != expected {
					t.Fatalf("seed=%d step=%d operation=%d target=%s expected_success=%t error=%v", seed, step, op, id, expected, err)
				}
				if err == nil {
					accepted++
				} else {
					rejected++
					if ErrorCode(err) != "conflict" && ErrorCode(err) != "blocked" {
						t.Fatalf("seed=%d step=%d unexpected error class: %v", seed, step, err)
					}
				}
				if err != nil && coreEventCount(t, s) != beforeEvents {
					t.Fatalf("seed=%d step=%d rejected operation emitted events", seed, step)
				}
				observations += len(m)
				immutableChecks += len(frozen)
				chaosCheckModel(t, s, m, frozen, seed, step)
				after := m.available()
				wantAvailable := map[string]bool{}
				for id := range after {
					if !before[id] {
						wantAvailable[id] = true
					}
				}
				gotAvailable := map[string]bool{}
				rows, err := s.DB.Query("SELECT task_id FROM events WHERE id>? AND kind='available'", through)
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var num int64
					if err = rows.Scan(&num); err != nil {
						t.Fatal(err)
					}
					gotAvailable[taskID(num)] = true
				}
				err = rows.Err()
				rows.Close()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(wantAvailable, gotAvailable) {
					t.Fatalf("seed=%d step=%d availability transitions got=%v want=%v", seed, step, gotAvailable, wantAvailable)
				}
				if step%37 == 0 {
					integrityChecks++
					chaosIntegrity(t, s)
				}
			}
			chaosIntegrity(t, stores[0])
			stats, _ := json.Marshal(map[string]any{"seed": seed, "steps": steps, "tasks": len(m), "claims": len(tokens), "task_state_checks": observations, "outcome_checks": observations, "immutable_record_checks": immutableChecks, "availability_checks": steps, "availability_event_checks": steps, "integrity_checks": integrityChecks + 1, "accepted_operations": accepted, "rejected_operations": rejected, "reopens": reopens, "operation_counts": operationCounts})
			t.Log("CHAOS_STATS " + string(stats))
		})
	}
}

func TestChaosCreateReplayAndLostResponse(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustCreateTask(t, s, CreateSpec{Title: "A"})
	b := mustCreateTask(t, s, CreateSpec{Title: "B"})
	spec := CreateSpec{Title: " Recoverable create ", Brief: "Keep this exact context", Tags: []string{"beta", "alpha", "alpha"}, Metadata: map[string]string{"component": "parser"}, Prerequisites: []string{b.ID, a.ID, b.ID}}
	first, err := s.CreateWithRequest(spec, "request-1")
	if err != nil {
		t.Fatal(err)
	}
	events := coreEventCount(t, s)
	retry := spec
	retry.Title = "Recoverable create"
	retry.Tags = []string{"alpha", "beta"}
	retry.Prerequisites = []string{a.ID, b.ID}
	second, err := s.CreateWithRequest(retry, "request-1")
	if err != nil || !reflect.DeepEqual(first, second) || events != coreEventCount(t, s) {
		t.Fatalf("lost-response retry: %+v %v", second, err)
	}
	changed := retry
	changed.Brief = "Different remaining work"
	if _, err = s.CreateWithRequest(changed, "request-1"); ErrorCode(err) != "conflict" {
		t.Fatalf("key changed input: %v", err)
	}
	if _, err = s.Cancel(first.ID, "", "No longer needed"); err != nil {
		t.Fatal(err)
	}
	second, err = s.CreateWithRequest(spec, "request-1")
	if err != nil || second.ID != first.ID || second.Status != "cancelled" {
		t.Fatalf("retry should find current original record: %+v %v", second, err)
	}
	third, err := s.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := s.Create(spec)
	if err != nil || third.ID == fourth.ID {
		t.Fatal("unkeyed Create must remain a new task operation")
	}
	for _, key := range []string{"  ", "leading ", "\nbad", strings.Repeat("x", 257)} {
		if _, err = s.CreateWithRequest(spec, key); ErrorCode(err) != "invalid_input" {
			t.Fatalf("invalid request ID %q: %v", key, err)
		}
	}
	archive := backupTestPath(t, s)
	restored := newTaskTestStore(t)
	if err = restored.Restore(archive); err != nil {
		t.Fatal(err)
	}
	var requests int
	if err = restored.DB.QueryRow("SELECT COUNT(*) FROM create_requests").Scan(&requests); err != nil || requests != 0 {
		t.Fatalf("new project restored old operational request IDs: %d %v", requests, err)
	}
}

func TestChaosEventFailuresRollbackWholeMutation(t *testing.T) {
	type fault struct {
		kind  string
		setup func(*testing.T, *Store) func() error
	}
	owned := func(t *testing.T, s *Store) Task {
		return mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Original", Brief: "Full scope"}).ID, "worker")
	}
	cases := map[string]fault{
		"create": {kind: "created", setup: func(t *testing.T, s *Store) func() error {
			return func() error {
				_, err := s.CreateWithRequest(CreateSpec{Title: "New work"}, "failed-create")
				return err
			}
		}},
		"create-availability": {kind: "available", setup: func(t *testing.T, s *Store) func() error {
			return func() error {
				_, err := s.CreateWithRequest(CreateSpec{Title: "New work"}, "failed-create")
				return err
			}
		}},
		"claim": {kind: "claimed", setup: func(t *testing.T, s *Store) func() error {
			task := mustCreateTask(t, s, CreateSpec{Title: "Claim me"})
			return func() error { _, err := s.Claim(task.ID, "worker"); return err }
		}},
		"progress": {kind: "progress", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error { _, err := s.Progress(task.ID, task.ClaimID, "Durable checkpoint"); return err }
		}},
		"release": {kind: "released", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error { _, err := s.Release(task.ID, task.ClaimID); return err }
		}},
		"release-availability": {kind: "available", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error { _, err := s.Release(task.ID, task.ClaimID); return err }
		}},
		"takeover": {kind: "reassigned", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error {
				_, err := s.Takeover(task.ID, task.ClaimID, "new-worker", "Old worker stopped")
				return err
			}
		}},
		"complete": {kind: "completed", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error { _, err := s.Complete(task.ID, task.ClaimID, "Verified result"); return err }
		}},
		"dependent-availability": {kind: "available", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			mustCreateTask(t, s, CreateSpec{Title: "Blocked dependent", Prerequisites: []string{task.ID}})
			return func() error { _, err := s.Complete(task.ID, task.ClaimID, "Unblocks another task"); return err }
		}},
		"cancel": {kind: "cancelled", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error { _, err := s.Cancel(task.ID, task.ClaimID, "Cancelled deliberately"); return err }
		}},
		"edit": {kind: "edited", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			title := "New definition"
			return func() error { _, err := s.Edit(task.ID, task.ClaimID, task.Revision, &title, nil); return err }
		}},
		"tag": {kind: "tags_changed", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error { return s.Tag(task.ID, task.ClaimID, "important", false) }
		}},
		"metadata": {kind: "metadata_changed", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			return func() error { return s.MetadataSet(task.ID, task.ClaimID, "worktree", "/project/worktree") }
		}},
		"prerequisite": {kind: "prerequisite_changed", setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			prerequisite := mustCreateTask(t, s, CreateSpec{Title: "New dependency"})
			return func() error { return s.AddPrerequisite(task.ID, task.ClaimID, prerequisite.ID, false) }
		}},
	}
	for _, kind := range []string{"superseded", "created", "claimed", "available"} {
		kind := kind
		cases["evolve-"+kind] = fault{kind: kind, setup: func(t *testing.T, s *Store) func() error {
			task := owned(t, s)
			spec := EvolveSpec{Summary: "Partial result preserved", ContinueWith: "first", Replacements: []CreateSpec{{Key: "first", Title: "First", Brief: "Remaining first"}, {Key: "second", Title: "Second", Brief: "Remaining second"}}}
			return func() error { _, err := s.Evolve(task.ID, task.ClaimID, task.Revision, spec); return err }
		}}
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTaskTestStore(t)
			mutate := test.setup(t, s)
			before, err := chaosStateHash(s)
			if err != nil {
				t.Fatal(err)
			}
			var requestsBefore int
			if err = s.DB.QueryRow("SELECT COUNT(*) FROM create_requests").Scan(&requestsBefore); err != nil {
				t.Fatal(err)
			}
			if _, err = s.DB.Exec(`CREATE TRIGGER chaos_fail_event BEFORE INSERT ON events WHEN NEW.kind='` + test.kind + `' BEGIN SELECT RAISE(ABORT,'injected chaos event failure'); END;`); err != nil {
				t.Fatal(err)
			}
			if err = mutate(); err == nil || !strings.Contains(err.Error(), "injected chaos event failure") {
				t.Fatalf("fault was not exercised: %v", err)
			}
			after, err := chaosStateHash(s)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed mutation was partially committed: %v", err)
			}
			var requestsAfter int
			if err = s.DB.QueryRow("SELECT COUNT(*) FROM create_requests").Scan(&requestsAfter); err != nil || requestsBefore != requestsAfter {
				t.Fatalf("failed mutation committed request receipt: %d -> %d %v", requestsBefore, requestsAfter, err)
			}
			if _, err = s.DB.Exec("DROP TRIGGER chaos_fail_event"); err != nil {
				t.Fatal(err)
			}
			if err = mutate(); err != nil {
				t.Fatalf("retry after fault recovery: %v", err)
			}
			chaosIntegrity(t, s)
		})
	}
}

func chaosEvolvePlan() EvolveSpec {
	spec := EvolveSpec{Summary: "A large completed chunk and eight remaining branches", ContinueWith: "branch-0"}
	for i := 0; i < 8; i++ {
		spec.Replacements = append(spec.Replacements, CreateSpec{Key: fmt.Sprintf("branch-%d", i), Title: fmt.Sprintf("Remaining branch %d", i), Brief: strings.Repeat("Large self-contained remaining context. ", 1024), Tags: []string{"survives-crash"}})
	}
	return spec
}

func TestChaosSubprocess(t *testing.T) {
	mode := os.Getenv("TASKCTL_CHAOS_CHILD_MODE")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	pause := func() { fmt.Fprintln(os.Stdout, "ready"); _, _ = io.Copy(io.Discard, os.Stdin) }
	if mode == "kill-before-commit" {
		if err := sqlite.RegisterScalarFunction("taskctl_chaos_pause", 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) { pause(); return int64(0), nil }); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(os.Getenv("TASKCTL_CHAOS_DATABASE"), "chaos-process")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if mode == "request-race" {
		fmt.Fprintln(os.Stdout, "ready")
		if _, err = bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		var spec CreateSpec
		if err = json.Unmarshal([]byte(os.Getenv("TASKCTL_CHAOS_SPEC")), &spec); err != nil {
			t.Fatal(err)
		}
		task, err := s.CreateWithRequest(spec, "process-request")
		if err != nil {
			t.Fatal(err)
		}
		if err = json.NewEncoder(os.Stdout).Encode(task); err != nil {
			t.Fatal(err)
		}
		return
	}
	if mode == "kill-create-after-commit" {
		if _, err = s.CreateWithRequest(CreateSpec{Title: "Recoverable create", Brief: "Committed response deliberately discarded"}, "recover-create"); err != nil {
			t.Fatal(err)
		}
		pause()
		return
	}
	if mode == "kill-before-commit" {
		if _, err = s.DB.Exec("PRAGMA cache_size=2; PRAGMA cache_spill=ON"); err != nil {
			t.Fatal(err)
		}
		if _, err = s.DB.Exec(`CREATE TEMP TRIGGER chaos_pause BEFORE INSERT ON main.events WHEN NEW.kind='available' BEGIN SELECT taskctl_chaos_pause(); END;`); err != nil {
			t.Fatal(err)
		}
	}
	task, err := s.Get("T1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Evolve(task.ID, task.ClaimID, task.Revision, chaosEvolvePlan()); err != nil {
		t.Fatal(err)
	}
	if mode == "kill-after-commit" {
		pause()
	}
}

type chaosProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	stderr  bytes.Buffer
	cancel  context.CancelFunc
}

func chaosStart(t *testing.T, path, mode string, extra ...string) *chaosProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	child := &chaosProcess{cmd: exec.CommandContext(ctx, executable, "-test.run=^TestChaosSubprocess$"), cancel: cancel}
	child.cmd.Env = append(os.Environ(), "TASKCTL_CHAOS_CHILD_MODE="+mode, "TASKCTL_CHAOS_DATABASE="+path)
	child.cmd.Env = append(child.cmd.Env, extra...)
	child.stdin, err = child.cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := child.cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	child.scanner = bufio.NewScanner(stdout)
	child.cmd.Stderr = &child.stderr
	if err = child.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.stdin.Close()
		if child.cmd.ProcessState == nil {
			_ = child.cmd.Process.Kill()
			_ = child.cmd.Wait()
		}
		cancel()
	})
	ready := make(chan bool, 1)
	go func() { ready <- child.scanner.Scan() }()
	select {
	case ok := <-ready:
		if !ok || child.scanner.Text() != "ready" {
			_ = child.cmd.Process.Kill()
			_ = child.cmd.Wait()
			t.Fatalf("child not ready mode=%s output=%q stderr=%s", mode, child.scanner.Text(), child.stderr.String())
		}
	case <-ctx.Done():
		_ = child.cmd.Process.Kill()
		_ = child.cmd.Wait()
		t.Fatalf("child readiness timeout: %s", child.stderr.String())
	}
	return child
}

func TestChaosKilledWriterRecovery(t *testing.T) {
	for _, mode := range []string{"kill-before-commit", "kill-after-commit", "kill-create-after-commit"} {
		for round := 0; round < chaosSetting(t, "TASKCTL_CHAOS_KILLS", 2); round++ {
			t.Run(fmt.Sprintf("%s/%d", mode, round), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "tasks.sqlite")
				s, err := Open(path, "chaos-process")
				if err != nil {
					t.Fatal(err)
				}
				original := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Original intent", Brief: "Full original scope", Tags: []string{"preserve"}}).ID, "original-worker")
				mustCreateTask(t, s, CreateSpec{Title: "Integration", Prerequisites: []string{original.ID}})
				eventsBefore := coreEventCount(t, s)
				before, err := chaosStateHash(s)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				child := chaosStart(t, path, mode)
				if mode == "kill-before-commit" {
					info, err := os.Stat(path + "-wal")
					if err != nil || info.Size() <= 32 {
						t.Fatalf("test did not force dirty WAL spill before kill: %v info=%v", err, info)
					}
					t.Logf("uncommitted WAL bytes before SIGKILL: %d", info.Size())
				}
				if err = child.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				if err = child.cmd.Wait(); err == nil {
					t.Fatal("killed process returned success")
				}
				child.stdin.Close()
				child.cancel()
				reopened, err := Open(path, "chaos-process")
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				chaosIntegrity(t, reopened)
				switch mode {
				case "kill-before-commit":
					after, err := chaosStateHash(reopened)
					if err != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("SIGKILL committed partial state: %v", err)
					}
					if _, err = reopened.Progress(original.ID, original.ClaimID, "Recovered old claim"); err != nil {
						t.Fatalf("precommit crash lost valid owner: %v", err)
					}
				case "kill-after-commit":
					task, err := reopened.Get(original.ID)
					if err != nil || task.Status != "evolved" {
						t.Fatalf("lost committed evolution: %+v %v", task, err)
					}
					if _, err = reopened.Evolve(original.ID, original.ClaimID, original.Revision, chaosEvolvePlan()); ErrorCode(err) != "conflict" {
						t.Fatalf("lost-response retry duplicated evolution: %v", err)
					}
					all := chaosAll(t, reopened)
					if len(all) != 10 {
						t.Fatalf("committed branch count=%d want10", len(all))
					}
					plan := chaosEvolvePlan()
					wantEvents := map[string]int{original.ID + ":superseded": 1}
					for i, spec := range plan.Replacements {
						id := taskID(int64(i + 3))
						child := all[id]
						if child.ParentID != original.ID || child.OriginID != original.ID || child.Title != spec.Title || child.Brief != spec.Brief || !reflect.DeepEqual(child.Tags, []string{"preserve", "survives-crash"}) {
							t.Fatalf("lost committed replacement context: %+v", child)
						}
						wantEvents[id+":created"] = 1
						if i == 0 {
							if child.Status != "in_progress" || child.Owner != original.Owner || child.ClaimID == "" || child.ClaimID == original.ClaimID {
								t.Fatalf("lost committed owner transfer: %+v", child)
							}
							wantEvents[id+":claimed"] = 1
						} else {
							if child.Status != "pending" || child.Owner != "" || child.ClaimID != "" {
								t.Fatalf("corrupted committed pending branch: %+v", child)
							}
							wantEvents[id+":available"] = 1
						}
					}
					gotEvents := map[string]int{}
					rows, err := reopened.DB.Query("SELECT task_id,kind,COUNT(*) FROM events WHERE id>? GROUP BY task_id,kind", eventsBefore)
					if err != nil {
						t.Fatal(err)
					}
					for rows.Next() {
						var id int64
						var kind string
						var count int
						if err = rows.Scan(&id, &kind, &count); err != nil {
							t.Fatal(err)
						}
						gotEvents[taskID(id)+":"+kind] = count
					}
					err = rows.Err()
					rows.Close()
					if err != nil || !reflect.DeepEqual(gotEvents, wantEvents) {
						t.Fatalf("commit lost or duplicated events: got=%v want=%v error=%v", gotEvents, wantEvents, err)
					}
					if _, err = reopened.Progress(original.ID, original.ClaimID, "stale retry"); ErrorCode(err) != "conflict" {
						t.Fatalf("old owner survived commit: %v", err)
					}
				case "kill-create-after-commit":
					task, err := reopened.CreateWithRequest(CreateSpec{Title: "Recoverable create", Brief: "Committed response deliberately discarded"}, "recover-create")
					if err != nil || task.ID != "T3" || len(chaosAll(t, reopened)) != 3 {
						t.Fatalf("lost create response duplicated work: %+v %v", task, err)
					}
					var receipts int
					if err = reopened.DB.QueryRow("SELECT COUNT(*) FROM create_requests").Scan(&receipts); err != nil || receipts != 1 {
						t.Fatalf("request receipt count=%d err=%v", receipts, err)
					}
				}
			})
		}
	}
}

func TestChaosCreateRequestProcessRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	s, err := Open(path, "chaos-process")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	children := make([]*chaosProcess, 8)
	for i := range children {
		tags := []string{"a", "b"}
		if i%2 == 0 {
			tags = []string{"b", "a", "a"}
		}
		spec, _ := json.Marshal(CreateSpec{Title: "One requested task", Brief: "Many consumers retry after ambiguous responses", Tags: tags})
		children[i] = chaosStart(t, path, "request-race", "TASKCTL_CHAOS_SPEC="+string(spec))
	}
	for _, child := range children {
		if _, err = io.WriteString(child.stdin, "go\n"); err != nil {
			t.Fatal(err)
		}
	}
	for _, child := range children {
		if !child.scanner.Scan() {
			t.Fatal("child did not return task")
		}
		var task Task
		if err = json.Unmarshal(child.scanner.Bytes(), &task); err != nil || task.ID != "T1" {
			t.Fatalf("request race result=%q err=%v", child.scanner.Text(), err)
		}
		for child.scanner.Scan() {
		}
		if err = child.cmd.Wait(); err != nil {
			t.Fatalf("request child: %v %s", err, child.stderr.String())
		}
		child.stdin.Close()
		child.cancel()
	}
	if all := chaosAll(t, s); len(all) != 1 {
		t.Fatalf("eight retries created %d tasks", len(all))
	}
	if events := coreEventCount(t, s); events != 2 {
		t.Fatalf("duplicate events after replay: %d", events)
	}
	chaosIntegrity(t, s)
}

func TestChaosBusyWriterRecoversWithoutPartialMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	first, err := Open(path, "busy-chaos")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, "busy-chaos")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	task := mustCreateTask(t, first, CreateSpec{Title: "Contention"})
	before, err := chaosStateHash(first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = second.DB.Exec("PRAGMA busy_timeout=25"); err != nil {
		t.Fatal(err)
	}
	tx, err := first.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = second.Claim(task.ID, "blocked-writer"); ErrorCode(err) != "busy" {
		t.Fatalf("write under held transaction: %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after, err := chaosStateHash(first)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("timed-out writer changed state: %v", err)
	}
	claimed, err := second.Claim(task.ID, "recovered-writer")
	if err != nil || claimed.Owner != "recovered-writer" {
		t.Fatalf("retry after releasing lock: %+v %v", claimed, err)
	}
	chaosIntegrity(t, first)
}

func TestChaosContentionAcrossOwnersAndDefinitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	stores := make([]*Store, 8)
	for i := range stores {
		s, err := Open(path, "contended-chaos")
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = s
		defer s.Close()
	}
	rounds := chaosSetting(t, "TASKCTL_CHAOS_ROUNDS", 8)
	for round := 0; round < rounds; round++ {
		original := mustCreateTask(t, stores[0], CreateSpec{Title: fmt.Sprintf("Work %d", round), Brief: "Original crisp scope"})
		start := make(chan struct{})
		results := make(chan Task, 8)
		failures := make(chan error, 8)
		var wg sync.WaitGroup
		for i, s := range stores {
			wg.Add(1)
			go func(i int, s *Store) {
				defer wg.Done()
				<-start
				task, err := s.Claim(original.ID, fmt.Sprintf("worker-%d", i))
				if err == nil {
					results <- task
				} else {
					failures <- err
				}
			}(i, s)
		}
		close(start)
		wg.Wait()
		close(results)
		close(failures)
		var owner Task
		wins := 0
		for task := range results {
			owner = task
			wins++
		}
		if wins != 1 {
			t.Fatalf("round=%d claim winners=%d", round, wins)
		}
		for err := range failures {
			if ErrorCode(err) != "conflict" && ErrorCode(err) != "busy" {
				t.Fatal(err)
			}
		}
		start = make(chan struct{})
		takeovers := make(chan Task, 8)
		failures = make(chan error, 8)
		for i, s := range stores {
			wg.Add(1)
			go func(i int, s *Store) {
				defer wg.Done()
				<-start
				if i < 5 {
					_, err := s.Progress(owner.ID, owner.ClaimID, fmt.Sprintf("Old owner checkpoint %d", i))
					if err != nil {
						failures <- err
					}
					return
				}
				task, err := s.Takeover(owner.ID, owner.ClaimID, fmt.Sprintf("new-worker-%d", i), "Concurrent explicit takeover")
				if err == nil {
					takeovers <- task
				} else {
					failures <- err
				}
			}(i, s)
		}
		close(start)
		wg.Wait()
		close(takeovers)
		close(failures)
		wins = 0
		var current Task
		for task := range takeovers {
			current = task
			wins++
		}
		if wins != 1 {
			t.Fatalf("round=%d takeover winners=%d", round, wins)
		}
		for err := range failures {
			if ErrorCode(err) != "conflict" && ErrorCode(err) != "busy" {
				t.Fatal(err)
			}
		}
		number, _ := taskNumber(owner.ID)
		var reassigned, lateProgress int64
		if err := stores[0].DB.QueryRow("SELECT MAX(id) FROM events WHERE task_id=? AND kind='reassigned'", number).Scan(&reassigned); err != nil {
			t.Fatal(err)
		}
		if err := stores[0].DB.QueryRow("SELECT COUNT(*) FROM events WHERE task_id=? AND kind='progress' AND id>?", number, reassigned).Scan(&lateProgress); err != nil || lateProgress != 0 {
			t.Fatalf("old owner wrote after takeover: %d %v", lateProgress, err)
		}
		if _, err := stores[0].Progress(owner.ID, owner.ClaimID, "Zombie retry"); ErrorCode(err) != "conflict" {
			t.Fatalf("old claim still valid: %v", err)
		}
		current, err := stores[0].Get(current.ID)
		if err != nil {
			t.Fatal(err)
		}
		start = make(chan struct{})
		success := make(chan bool, 8)
		failures = make(chan error, 8)
		for _, s := range stores {
			wg.Add(1)
			go func(s *Store) {
				defer wg.Done()
				<-start
				_, err := s.Evolve(current.ID, current.ClaimID, current.Revision, EvolveSpec{Summary: "One evolution wins", Replacements: []CreateSpec{{Title: "First remaining", Brief: "First independent remainder"}, {Title: "Second remaining", Brief: "Second independent remainder"}}})
				if err == nil {
					success <- true
				} else {
					failures <- err
				}
			}(s)
		}
		close(start)
		wg.Wait()
		close(success)
		close(failures)
		if len(success) != 1 {
			t.Fatalf("round=%d evolution winners=%d", round, len(success))
		}
		for err := range failures {
			if ErrorCode(err) != "conflict" && ErrorCode(err) != "busy" {
				t.Fatal(err)
			}
		}
		if all := chaosAll(t, stores[0]); len(all) != (round+1)*3 {
			t.Fatalf("partial/duplicate replacement count after round %d: %d", round, len(all))
		}
	}
	chaosIntegrity(t, stores[0])
	t.Logf("rounds=%d concurrent API attempts=%d", rounds, rounds*24)
}

func chaosStateHash(s *Store) (string, error) {
	hash := sha256.New()
	enc := json.NewEncoder(hash)
	order := map[string]string{"tasks": "id", "tags": "task_id,tag", "metadata": "task_id,key", "prerequisites": "task_id,prerequisite_id", "events": "id", "subscriptions": "id", "inactivity_marks": "task_id", "scratch_areas": "id", "scratch_refs": "task_id,area_id", "create_requests": "request_id", "scratch_cleanup_state": "area_id"}
	err := s.read(func(q Queryer) error {
		tables := append(append([]string{}, archiveTables...), "create_requests", "scratch_cleanup_state")
		for _, table := range tables {
			if err := enc.Encode(table); err != nil {
				return err
			}
			rows, err := q.Query("SELECT * FROM " + table + " ORDER BY " + order[table])
			if err != nil {
				return err
			}
			columns, err := rows.Columns()
			if err != nil {
				rows.Close()
				return err
			}
			values, destinations := make([]any, len(columns)), make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			for rows.Next() {
				if err = rows.Scan(destinations...); err != nil {
					rows.Close()
					return err
				}
				if err = enc.Encode(values); err != nil {
					rows.Close()
					return err
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	return fmt.Sprintf("%x", hash.Sum(nil)), err
}
