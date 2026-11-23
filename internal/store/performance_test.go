package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func performanceFixture(b *testing.B, total int, frontier bool) (*Store, string) {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "tasks.sqlite"), "performance")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	tx, err := s.DB.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	insert, err := tx.Prepare(`INSERT INTO tasks(id,title,brief,status,origin_id,owner,claim_id,created_at,updated_at,last_activity_at)
 VALUES(?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer insert.Close()
	event, err := tx.Prepare(`INSERT INTO events(task_id,kind,message,data,created_at) VALUES(?,'created','','{}',?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer event.Close()
	for id := 1; id <= total; id++ {
		status, owner, claim := "completed", "", ""
		if id <= 5 {
			status, owner, claim = "in_progress", "bench", fmt.Sprintf("Cperf%d", id)
		} else if id <= 20 || (frontier && id != total) {
			status = "pending"
		}
		if _, err = insert.Exec(id, fmt.Sprintf("Task %d", id), "A small representative task brief.", status, id, owner, claim, timestamp(now), timestamp(now), timestamp(now)); err != nil {
			b.Fatal(err)
		}
		if _, err = event.Exec(id, timestamp(now)); err != nil {
			b.Fatal(err)
		}
	}
	for id := 6; id <= 20; id++ {
		if _, err = tx.Exec(`INSERT INTO prerequisites(task_id,prerequisite_id) VALUES(?,?)`, id, total); err != nil {
			b.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	sub, err := s.Subscribe("T6", "bench", false)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = s.DB.Exec(`UPDATE subscriptions SET snapshot_pending=0 WHERE id=?`, sub.ID); err != nil {
		b.Fatal(err)
	}
	return s, sub.ID
}

func BenchmarkStoreScale(b *testing.B) {
	for _, frontier := range []bool{false, true} {
		name := "retired"
		if frontier {
			name = "frontier"
		}
		for _, total := range []int{100, 1000, 10000, 100000} {
			b.Run(fmt.Sprintf("%s/%d", name, total), func(b *testing.B) {
				s, sub := performanceFixture(b, total, frontier)
				ops := []struct {
					name string
					fn   func() error
				}{
					{"Get", func() error { _, err := s.Get("T6"); return err }},
					{"ListAvailable", func() error { _, _, err := s.List("available", nil, nil, 20, 0); return err }},
					{"ListInProgress", func() error { _, _, err := s.List("in-progress", nil, nil, 20, 0); return err }},
					{"Snapshot", func() error { _, err := s.Snapshot("T6"); return err }},
					{"Outcome", func() error { _, err := s.Outcome("T6"); return err }},
					{"QuietOffer", func() error { _, err := s.Offer(sub, "bench", 5, time.Minute); return err }},
					{"QuietInactivity", func() error { _, err := s.DetectInactivity(30 * time.Minute); return err }},
					{"Progress", func() error { _, err := s.Progress("T1", "Cperf1", "A meaningful checkpoint"); return err }},
				}
				for _, op := range ops {
					b.Run(op.name, func(b *testing.B) {
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							if err := op.fn(); err != nil {
								b.Fatal(err)
							}
						}
					})
				}
			})
		}
	}
}

func performanceLineageFixture(b *testing.B, depth int) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "tasks.sqlite"), "performance-lineage")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	tx, err := s.DB.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	insert, err := tx.Prepare(`INSERT INTO tasks(id,title,brief,status,origin_id,parent_id,owner,claim_id,created_at,updated_at,last_activity_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer insert.Close()
	for id := 1; id <= depth; id++ {
		status, owner, claim := "superseded", "", ""
		if id == depth {
			status, owner, claim = "in_progress", "bench", "Clineage"
		}
		var parent any
		if id > 1 {
			parent = id - 1
		}
		if _, err = insert.Exec(id, fmt.Sprintf("Replacement %d", id), "Remaining work after the previous checkpoint.", status, 1, parent, owner, claim, timestamp(now), timestamp(now), timestamp(now)); err != nil {
			b.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO events(task_id,kind,message,data,created_at) VALUES(?,'created','','{}',?)`, id, timestamp(now)); err != nil {
			b.Fatal(err)
		}
	}
	if _, err = insert.Exec(depth+1, "Dependent work", "Requires the original intention to finish.", "pending", depth+1, nil, "", "", timestamp(now), timestamp(now), timestamp(now)); err != nil {
		b.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO prerequisites(task_id,prerequisite_id) VALUES(?,1)`, depth+1); err != nil {
		b.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkLineageScale(b *testing.B) {
	for _, depth := range []int{100, 300, 1000} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			s := performanceLineageFixture(b, depth)
			leaf := taskID(int64(depth))
			for _, op := range []struct {
				name string
				fn   func() error
			}{
				{"Progress", func() error { _, err := s.Progress(leaf, "Clineage", "Saved a further checkpoint"); return err }},
				{"SnapshotLeaf", func() error { _, err := s.Snapshot(leaf); return err }},
			} {
				b.Run(op.name, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if err := op.fn(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
