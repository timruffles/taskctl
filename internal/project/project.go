package project

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Database  string `json:"database"`
	Scratch   string `json:"scratch"`
}
type Marker struct {
	ProjectID string `json:"project_id"`
}
type Config struct {
	Inactivity   string `json:"inactivity"`
	ScratchGrace string `json:"scratch_grace"`
	ListLimit    int    `json:"list_limit"`
}

var validID = regexp.MustCompile(`^P[0-9a-f]{16}$`)

func Base(kind string) string {
	defaults := map[string]string{"DATA": filepath.Join(".local", "share"), "CONFIG": ".config", "STATE": filepath.Join(".local", "state")}
	if p := os.Getenv("XDG_" + kind + "_HOME"); p != "" && filepath.IsAbs(p) {
		return filepath.Join(p, "taskctl")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return filepath.Join(home, defaults[kind], "taskctl")
}
func Dir(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("invalid project ID %q", id)
	}
	return filepath.Join(Base("DATA"), "projects", id), nil
}
func Load(id string) (Project, error) {
	dir, err := Dir(id)
	if err != nil {
		return Project{}, err
	}
	b, err := readConfigFile(filepath.Join(dir, "project.json"))
	if err != nil {
		return Project{}, fmt.Errorf("project %s: %w", id, err)
	}
	var p Project
	if err = json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.ID != id {
		return p, fmt.Errorf("project manifest identity mismatch")
	}
	p.Database = filepath.Join(dir, "tasks.sqlite")
	p.Scratch = filepath.Join(Base("STATE"), "scratch", id)
	return p, nil
}
func Create(name string) (Project, error) {
	if strings.TrimSpace(name) == "" {
		return Project{}, fmt.Errorf("project name is required")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Project{}, err
	}
	id := "P" + hex.EncodeToString(b[:])
	dir, _ := Dir(id)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return Project{}, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return Project{}, err
	}
	p := Project{ID: id, Name: name, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	data, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "project.json"), append(data, '\n'), 0600); err != nil {
		return Project{}, err
	}
	return Load(id)
}
func Resolve(explicit, cwd string) (Project, error) {
	if explicit != "" {
		return Load(explicit)
	}
	if id := os.Getenv("TASKCTL_PROJECT"); id != "" {
		return Load(id)
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return Project{}, err
	}
	for {
		b, err := readConfigFile(filepath.Join(dir, ".taskctl.json"))
		if err == nil {
			var m Marker
			if err = json.Unmarshal(b, &m); err != nil {
				return Project{}, fmt.Errorf("invalid .taskctl.json: %w", err)
			}
			return Load(m.ProjectID)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Project{}, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return Project{}, fmt.Errorf("no project selected; use --project ID or projects attach --project ID --path DIR")
}
func Attach(p Project, path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("attachment path must be a directory")
	}
	marker := filepath.Join(abs, ".taskctl.json")
	data, _ := json.MarshalIndent(Marker{p.ID}, "", "  ")
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		var m Marker
		old, e := readConfigFile(marker)
		if e == nil && json.Unmarshal(old, &m) == nil && m.ProjectID == p.ID {
			return nil
		}
		return fmt.Errorf("%s already selects a different project", marker)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
func Defaults() (Config, error) {
	c := Config{Inactivity: "30m", ScratchGrace: "168h", ListLimit: 20}
	data, err := readConfigFile(filepath.Join(Base("CONFIG"), "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one JSON object")
	}
	for _, v := range []string{c.Inactivity, c.ScratchGrace} {
		x, err := time.ParseDuration(v)
		if err != nil || x <= 0 {
			return c, fmt.Errorf("configuration durations must be positive")
		}
	}
	if c.ListLimit < 1 || c.ListLimit > 200 {
		return c, fmt.Errorf("list_limit must be between 1 and 200")
	}
	return c, nil
}

func readConfigFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const limit = 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("configuration file exceeds 1 MiB: %s", path)
	}
	return data, nil
}
