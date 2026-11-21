package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestReadPathsDoNotWaitForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	writer, err := Open(path, "read-concurrency")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	task := mustCreateTask(t, writer, CreateSpec{Title: "Committed definition"})
	tx, err := writer.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE tasks SET title='Uncommitted definition' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		reader, err := OpenExisting(path, "read-concurrency")
		if err != nil {
			done <- err
			return
		}
		defer reader.Close()
		if _, err = reader.DB.Exec("PRAGMA busy_timeout=25"); err != nil {
			done <- err
			return
		}
		got, err := reader.Get(task.ID)
		if err == nil && got.Title != task.Title {
			err = fmt.Errorf("reader observed uncommitted title %q", got.Title)
		}
		if err == nil {
			_, _, err = reader.Current(task.ID)
		}
		if err == nil {
			_, err = reader.Events(task.ID, 0, 20)
		}
		if err == nil {
			_, _, err = reader.HistoryPage(task.ID, 20, 0)
		}
		if err == nil {
			_, _, err = reader.ScratchListPage(task.ID, 20, 0)
		}
		done <- err
	}()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		tx.Rollback()
		<-done
		t.Fatal("opening or reading an existing store waited for the writer")
	}
}

func TestReadTransactionKeepsSnapshotWhileWriterCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	reader, err := Open(path, "consistent-read")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	task := mustCreateTask(t, reader, CreateSpec{Title: "Before commit"})
	writer, err := OpenExisting(path, "consistent-read")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	err = reader.read(func(q Queryer) error {
		var before, after string
		if err := q.QueryRow("SELECT title FROM tasks WHERE id=1").Scan(&before); err != nil {
			return err
		}
		if _, err := writer.DB.Exec("UPDATE tasks SET title='After commit' WHERE id=1"); err != nil {
			return err
		}
		if err := q.QueryRow("SELECT title FROM tasks WHERE id=1").Scan(&after); err != nil {
			return err
		}
		if before != task.Title || after != before {
			return fmt.Errorf("inconsistent read: %q -> %q", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := reader.Get(task.ID)
	if err != nil || after.Title != "After commit" {
		t.Fatalf("next read did not observe committed update: %+v %v", after, err)
	}
}
