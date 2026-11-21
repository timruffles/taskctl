package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"taskctl/internal/project"
	"taskctl/internal/store"
)

func queueTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "tasks.sqlite"), "P0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func queueTestTask(t *testing.T, s *store.Store) store.Task {
	t.Helper()
	task, err := s.Create(store.CreateSpec{Title: "Queue transport", Brief: "Finish remaining work"})
	if err != nil {
		t.Fatal(err)
	}
	task, err = s.Claim(task.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func fakeQueueExecutable(t *testing.T, exit string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path, log := filepath.Join(dir, "fake codex"), filepath.Join(dir, "queue.log")
	t.Setenv("TASKCTL_QUEUE_TEST_LOG", log)
	script := "#!/bin/sh\nprintf '%s\\0' CALL \"$@\" >> \"$TASKCTL_QUEUE_TEST_LOG\"\nexit " + exit + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func runQueueWatchOnce(t *testing.T, s *store.Store, sub, session, executable string) (string, error) {
	t.Helper()
	o, err := parse([]string{"subscriptions", "watch", sub, "--session", session, "--codex", "--codex-executable", executable,
		"--once", "--limit", "2", "--timeout", "2s", "--lease", "1m", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	a := app{o: o, s: s, p: project.Project{ID: s.ProjectID}, cfg: project.Config{Inactivity: "30m"}, out: &output}
	err = a.watchSubscription(sub, session)
	return output.String(), err
}

func queueLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func TestQueueWatchScansProgressThenQueuesCompletionWithoutAcknowledging(t *testing.T) {
	s := queueTestStore(t)
	task := queueTestTask(t, s)
	session := "watcher ' $(printf TASKCTL_TEST_EXPANSION)"
	sub, err := s.Subscribe(task.ID, session, true)
	if err != nil {
		t.Fatal(err)
	}
	executable, log := fakeQueueExecutable(t, "0")
	if _, err := runQueueWatchOnce(t, s, sub.ID, session, executable); err != nil {
		t.Fatal(err)
	}
	if got := queueLog(t, log); len(got) != 0 {
		t.Fatalf("in-progress initial snapshot woke agent: %q", got)
	}
	for i := 0; i < 7; i++ {
		if _, err := s.Progress(task.ID, task.ClaimID, "Useful partial work"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Complete(task.ID, task.ClaimID, "All acceptance criteria met"); err != nil {
		t.Fatal(err)
	}
	for page := 0; page < 4; page++ {
		if _, err := runQueueWatchOnce(t, s, sub.ID, session, executable); err != nil {
			t.Fatal(err)
		}
		if page < 3 && len(queueLog(t, log)) != 0 {
			t.Fatal("progress-only page woke agent")
		}
	}
	args := queueLog(t, log)
	if len(args) != 7 || args[0] != "CALL" || args[1] != "queue" || args[2] != "--thread" || args[3] != session || args[4] != "--message" || args[6] != "--resume" {
		t.Fatalf("unexpected fake queue arguments: %q", args)
	}
	message := args[5]
	if strings.Contains(message, "--token") || !strings.Contains(message, sub.ID) {
		t.Fatalf("wake-only message contains a delivery token or loses target: %s", message)
	}
	state, _, err := s.SubscriptionsPage(session, 200, 0)
	if err != nil || len(state) != 1 || state[0].QueuedThrough == 0 || state[0].Cursor != sub.Cursor || !state[0].SnapshotPending {
		t.Fatalf("queue acceptance acknowledged data: %+v %v", state, err)
	}
	if _, err := runQueueWatchOnce(t, s, sub.ID, session, executable); err != nil {
		t.Fatal(err)
	}
	if len(queueLog(t, log)) != 7 {
		t.Fatal("already accepted wake repeated without a new event")
	}
	delivery, err := s.Offer(sub.ID, session, 100, time.Minute)
	if err != nil || delivery.Snapshot == nil || len(delivery.Events) != 8 {
		t.Fatalf("queue acceptance consumed pending context: %+v %v", delivery, err)
	}

	commandStart := strings.Index(message, "Read them with ")
	commandEnd := strings.Index(message, ". Acknowledge ")
	if commandStart < 0 || commandEnd <= commandStart {
		t.Fatalf("wake message lacks a literal pending command: %s", message)
	}
	command := message[commandStart+len("Read them with ") : commandEnd]
	dir := t.TempDir()
	pendingLog := filepath.Join(dir, "pending.log")
	t.Setenv("TASKCTL_PENDING_TEST_LOG", pendingLog)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "taskctl"), []byte("#!/bin/sh\nprintf '%s\\0' \"$@\" > \"$TASKCTL_PENDING_TEST_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-c", command).CombinedOutput(); err != nil {
		t.Fatalf("suggested pending command is not safely executable: %s: %v %s", command, err, output)
	}
	data, err := os.ReadFile(pendingLog)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	want := []string{"--project", s.ProjectID, "subscriptions", "pending", sub.ID, "--session", session, "--json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("suggested pending command changed literal session: got %q want %q", got, want)
	}
}

func TestQueueWatchFailureLeavesReceiptAndDeliveryPending(t *testing.T) {
	s := queueTestStore(t)
	task := queueTestTask(t, s)
	if _, err := s.Complete(task.ID, task.ClaimID, "Already complete"); err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(task.ID, "watcher", true)
	if err != nil {
		t.Fatal(err)
	}
	executable, log := fakeQueueExecutable(t, "7")
	if _, err := runQueueWatchOnce(t, s, sub.ID, "watcher", executable); err == nil {
		t.Fatal("failed queue command reported success")
	}
	if len(queueLog(t, log)) != 7 {
		t.Fatal("fake queue was not invoked")
	}
	state, _, err := s.SubscriptionsPage("watcher", 200, 0)
	if err != nil || state[0].QueuedThrough != 0 || state[0].QueueCursor != sub.QueueCursor || state[0].Cursor != sub.Cursor || !state[0].SnapshotPending {
		t.Fatalf("failed queue advanced durable delivery: %+v %v", state, err)
	}
	delivery, err := s.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil || delivery.Token == "" || delivery.Snapshot == nil {
		t.Fatalf("failed queue left claim locked or consumed context: %+v %v", delivery, err)
	}
}

func TestQueueWatchRequiresOptInAndDoesNotHideWrongOwner(t *testing.T) {
	s := queueTestStore(t)
	task := queueTestTask(t, s)
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	executable, log := fakeQueueExecutable(t, "0")
	if _, err := runQueueWatchOnce(t, s, sub.ID, "wrong-owner", executable); store.ErrorCode(err) != "conflict" {
		t.Fatalf("wrong session was treated as a busy delivery: %v", err)
	}
	if _, err := runQueueWatchOnce(t, s, sub.ID, "watcher", executable); err == nil {
		t.Fatal("queue watch ignored missing resume opt-in")
	}
	if len(queueLog(t, log)) != 0 {
		t.Fatal("fake queue was invoked without opt-in")
	}
	if delivery, err := s.Offer(sub.ID, "watcher", 5, time.Minute); err != nil || delivery.Token == "" {
		t.Fatalf("rejected opt-in left a delivery lock: %+v %v", delivery, err)
	}
}

func TestQueueWatchIgnoresLargePayloadAndPreservesFullConsumption(t *testing.T) {
	s := queueTestStore(t)
	task := queueTestTask(t, s)
	sub, err := s.Subscribe(task.ID, "watcher", true)
	if err != nil {
		t.Fatal(err)
	}
	executable, log := fakeQueueExecutable(t, "0")
	if _, err := runQueueWatchOnce(t, s, sub.ID, "watcher", executable); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Progress(task.ID, task.ClaimID, "Large saved checkpoint"); err != nil {
		t.Fatal(err)
	}
	payload := `{"checkpoint":"` + strings.Repeat("x", 2*1024*1024) + `"}`
	if _, err := s.DB.Exec(`UPDATE events SET data=? WHERE kind='progress'`, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(task.ID, task.ClaimID, "Finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := runQueueWatchOnce(t, s, sub.ID, "watcher", executable); err != nil {
		t.Fatal(err)
	}
	args := queueLog(t, log)
	if len(args) != 7 || len(args[5]) > 2048 {
		t.Fatalf("metadata-only queue page failed to reach completion behind large progress payload: %d arguments", len(args))
	}
	delivery, err := s.Offer(sub.ID, "watcher", 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(delivery.Events) != 1 || delivery.Events[0].Kind != "progress" || string(delivery.Events[0].Data) != payload || !delivery.More {
		t.Fatal("queue summary consumed or changed the full pending event payload")
	}
}
