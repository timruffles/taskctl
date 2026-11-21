package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

var binaryPath string

func TestMain(m *testing.M) {
	if os.Getenv("TASKCTL_QA_CALLBACK") == "1" {
		if err := watchCallback(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if supplied := os.Getenv("TASKCTL_TEST_BINARY"); supplied != "" {
		absolute, err := filepath.Abs(supplied)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		binaryPath = absolute
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "taskctl-process-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binaryPath = filepath.Join(dir, "taskctl")
	if runtime.GOOS == "windows" {
		binaryPath += ".exe"
	}
	compiler := filepath.Join(runtime.GOROOT(), "bin", "go")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	cmd := exec.CommandContext(ctx, compiler, "build", "-buildvcs=false", "-o", binaryPath, "./cmd/taskctl")
	cmd.Dir = ".."
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err = cmd.Run()
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "build taskctl for process tests:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type fixture struct {
	t       *testing.T
	root    string
	workdir string
	env     []string
	project string
}

type commandResult struct {
	stdout string
	stderr string
	code   int
	err    error
}

type task struct {
	ID            string            `json:"id"`
	Title         string            `json:"title"`
	Brief         string            `json:"brief"`
	Status        string            `json:"status"`
	OriginID      string            `json:"origin_id"`
	ParentID      string            `json:"parent_id"`
	Revision      int64             `json:"revision"`
	Owner         string            `json:"owner"`
	ClaimID       string            `json:"claim_id"`
	Progress      string            `json:"progress"`
	Result        string            `json:"result"`
	Metadata      map[string]string `json:"metadata"`
	Prerequisites []string          `json:"prerequisites"`
}

type taskList struct {
	Tasks      []task `json:"tasks"`
	Total      int    `json:"total"`
	Omitted    int    `json:"omitted"`
	NextOffset int    `json:"next_offset"`
}

type event struct {
	ID     int64  `json:"id"`
	TaskID string `json:"task_id"`
	Kind   string `json:"kind"`
}

type delivery struct {
	Token    string          `json:"token"`
	Through  int64           `json:"through"`
	Events   []event         `json:"events"`
	Snapshot json.RawMessage `json:"snapshot"`
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	workdir := filepath.Join(root, "work")
	if err := os.MkdirAll(workdir, 0700); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "TASKCTL_") || name == "XDG_DATA_HOME" || name == "XDG_STATE_HOME" || name == "XDG_CONFIG_HOME" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"XDG_DATA_HOME="+filepath.Join(root, "data"),
		"XDG_STATE_HOME="+filepath.Join(root, "state"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"),
	)
	f := &fixture{t: t, root: root, workdir: workdir, env: env}
	var project struct {
		ID string `json:"id"`
	}
	f.read(f.must("projects", "create", "--name", "process-test", "--json"), &project)
	if project.ID == "" {
		t.Fatal("project creation returned no ID")
	}
	f.project = project.ID
	return f
}

func (f *fixture) run(input string, args ...string) commandResult {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if f.project != "" {
		args = append([]string{"--project", f.project}, args...)
	}
	cmd := exec.CommandContext(ctx, binaryPath, args...)
	cmd.Env, cmd.Dir = f.env, f.workdir
	cmd.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	if err != nil {
		result.code = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.code = exitErr.ExitCode()
		}
	}
	return result
}

func (f *fixture) must(args ...string) commandResult {
	f.t.Helper()
	result := f.run("", args...)
	if result.code != 0 {
		f.t.Fatalf("%v exited %d: stderr=%s stdout=%s err=%v", args, result.code, result.stderr, result.stdout, result.err)
	}
	return result
}

func (f *fixture) read(result commandResult, target any) {
	f.t.Helper()
	decoder := json.NewDecoder(strings.NewReader(result.stdout))
	if err := decoder.Decode(target); err != nil {
		f.t.Fatalf("decode JSON: %v; stdout=%q stderr=%q", err, result.stdout, result.stderr)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		f.t.Fatalf("unexpected extra stdout after JSON: %q", result.stdout)
	}
}

func (f *fixture) create(title string, extra ...string) task {
	f.t.Helper()
	args := []string{"tasks", "create", "--title", title, "--brief", "Deliver " + title + " with a recorded verification result.", "--json"}
	args = append(args, extra...)
	var created task
	f.read(f.must(args...), &created)
	if created.ID == "" {
		f.t.Fatal("created task has no ID")
	}
	return created
}

func (f *fixture) get(id string) task {
	f.t.Helper()
	var found task
	f.read(f.must("tasks", "show", id, "--json"), &found)
	return found
}

func (f *fixture) claim(id, session string) task {
	f.t.Helper()
	var claimed task
	f.read(f.must("tasks", "claim", id, "--session", session, "--json"), &claimed)
	if claimed.ClaimID == "" {
		f.t.Fatal("successful claim returned no fencing token")
	}
	return claimed
}

func (f *fixture) complete(claimed task) task {
	f.t.Helper()
	var completed task
	f.read(f.must("tasks", "complete", claimed.ID, "--claim", claimed.ClaimID, "--summary", "Verified the requested outcome.", "--json"), &completed)
	return completed
}

func (f *fixture) list(command string) taskList {
	f.t.Helper()
	var listed taskList
	f.read(f.must("tasks", command, "--limit", "100", "--json"), &listed)
	return listed
}

func (f *fixture) subscribe(taskID, session string) string {
	f.t.Helper()
	var subscription struct {
		ID string `json:"id"`
	}
	f.read(f.must("tasks", "subscribe", taskID, "--session", session, "--json"), &subscription)
	if subscription.ID == "" {
		f.t.Fatal("subscription has no ID")
	}
	return subscription.ID
}

func (f *fixture) offer(subscription, session string, extra ...string) delivery {
	f.t.Helper()
	args := []string{"subscriptions", "pending", subscription, "--session", session, "--limit", "100", "--json"}
	args = append(args, extra...)
	var offered delivery
	f.read(f.must(args...), &offered)
	if offered.Token == "" {
		f.t.Fatal("nonempty delivery has no token")
	}
	return offered
}

func (f *fixture) acknowledge(subscription, session string, offered delivery) {
	f.t.Helper()
	f.must("subscriptions", "acknowledge", subscription, "--session", session, "--token", offered.Token, "--through", fmt.Sprint(offered.Through), "--json")
}

func (f *fixture) callback(logPath string) string {
	f.t.Helper()
	helper, err := os.Executable()
	if err != nil {
		f.t.Fatal(err)
	}
	f.env = append(f.env,
		"TASKCTL_QA_CALLBACK=1",
		"TASKCTL_QA_EVENT_LOG="+logPath,
		"TASKCTL_QA_PROJECT="+f.project,
		"TASKCTL_QA_BINARY="+binaryPath,
	)
	return helper
}

func (f *fixture) requireConflict(result commandResult) {
	f.t.Helper()
	if result.code != 3 {
		f.t.Fatalf("expected conflict exit 3, got %d: stderr=%q stdout=%q err=%v", result.code, result.stderr, result.stdout, result.err)
	}
	if strings.TrimSpace(result.stdout) != "" {
		f.t.Fatalf("failed mutation wrote to stdout: %q", result.stdout)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &envelope); err != nil || envelope.Error.Code == "" || envelope.Error.Message == "" {
		f.t.Fatalf("conflict lacks a structured error: %q", result.stderr)
	}
}

func race(count int, invoke func(int) commandResult) []commandResult {
	results := make([]commandResult, count)
	start := make(chan struct{})
	var ready, finished sync.WaitGroup
	ready.Add(count)
	finished.Add(count)
	for i := range results {
		go func(i int) {
			defer finished.Done()
			ready.Done()
			<-start
			results[i] = invoke(i)
		}(i)
	}
	ready.Wait()
	close(start)
	finished.Wait()
	return results
}

func containsTask(tasks []task, id string) bool {
	for _, candidate := range tasks {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

func watchCallback() error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1024*1024))
	if err != nil {
		return err
	}
	var message struct {
		Type  string `json:"type"`
		Event event  `json:"event"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return err
	}
	log, err := os.OpenFile(os.Getenv("TASKCTL_QA_EVENT_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = log.Write(append(bytes.TrimSpace(data), '\n'))
	closeErr := log.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if path := os.Getenv("TASKCTL_QA_FAIL_ONCE"); path != "" {
		flag, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			flag.Close()
			return errors.New("deliberate first callback failure")
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if message.Type != "event" || message.Event.Kind != "claimed" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	invoke := func(args ...string) ([]byte, error) {
		args = append([]string{"--project", os.Getenv("TASKCTL_QA_PROJECT")}, args...)
		cmd := exec.CommandContext(ctx, os.Getenv("TASKCTL_QA_BINARY"), args...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("callback command %v: %w: %s", args, err, output)
		}
		return output, nil
	}
	output, err := invoke("tasks", "show", message.Event.TaskID, "--json")
	if err != nil {
		return err
	}
	var claimed task
	if err := json.Unmarshal(output, &claimed); err != nil {
		return err
	}
	_, err = invoke("tasks", "complete", claimed.ID, "--claim", claimed.ClaimID, "--summary", "Completed from an independent callback process.", "--json")
	return err
}
