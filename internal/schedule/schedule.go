// Package schedule starts sessions on a clock: a prompt plus the session
// defaults to create it with, run on a cron expression or once at a fixed
// instant. It only ever creates a session, never sends into an existing one —
// a recurring prompt would grow one session's context without bound.
//
// It keeps no history.
package schedule

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Task is one saved schedule: a cron expression and the prompt to start a
// session with. Agent, Cwd, Backend and Model are the composer's
// create-session inputs, resolved through the agent store at fire time — so
// editing an agent changes what its tasks do next.
type Task struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Cron    string `json:"cron"`

	Agent   string `json:"agent,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	Prompt  string `json:"prompt"`
}

// NextRun reports when the task is next due, or the zero time if it is not:
// disabled, or a spec no calendar reaches.
func (t Task) NextRun(after time.Time) time.Time {
	if !t.Enabled {
		return time.Time{}
	}
	spec, err := ParseSpec(t.Cron)
	if err != nil {
		return time.Time{} // rejected on save; only a hand-edited file gets here
	}
	return spec.NextAfter(after)
}

type fileFormat struct {
	Schedules []Task `json:"schedules"`
}

// Store holds the tasks in creation order and owns their file. There are a
// handful of them, so a slice is the whole index.
type Store struct {
	mu    sync.RWMutex
	save  func([]Task) error
	tasks []Task
}

// Open builds a store from data, the tasks as JSON (empty for none), that
// keeps itself through save.
func Open(data json.RawMessage, save func([]Task) error) (*Store, error) {
	var tasks []Task
	if len(data) > 0 {
		if err := json.Unmarshal(data, &tasks); err != nil {
			return nil, fmt.Errorf("parse schedules: %w", err)
		}
	}
	s := New(nil)
	s.save = save
	// Same rules as the write path, so a hand-edited file cannot hold a spec
	// that would fail silently at 3am.
	for i := range tasks {
		task, err := validate(tasks[i])
		if err != nil {
			return nil, fmt.Errorf("schedules[%d]: %w", i, err)
		}
		if task.ID == "" {
			task.ID = newID()
		}
		if indexOf(s.tasks, task.ID) >= 0 {
			return nil, fmt.Errorf("duplicate schedule id %q", task.ID)
		}
		s.tasks = append(s.tasks, task)
	}
	return s, nil
}

// Load opens the tasks of a file of their own, {"schedules": [...]}. A
// missing file is no tasks yet.
func Load(path string) (*Store, error) {
	var file fileFormat
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return nil, fmt.Errorf("read schedules file: %w", err)
	default:
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("parse schedules file: %w", err)
		}
	}
	raw, _ := json.Marshal(file.Schedules)
	return Open(raw, func(tasks []Task) error { return writeFile(path, tasks) })
}

func New(tasks []Task) *Store {
	return &Store{tasks: append([]Task(nil), tasks...)}
}

func (s *Store) List() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.copyLocked()
}

func (s *Store) Get(id string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := indexOf(s.tasks, id); i >= 0 {
		return s.tasks[i], true
	}
	return Task{}, false
}

func indexOf(tasks []Task, id string) int {
	for i := range tasks {
		if tasks[i].ID == id {
			return i
		}
	}
	return -1
}

// validate checks the fields the store owns. Whether the task can actually
// create a session — the agent exists, cwd resolves — is the caller's to
// check, since only it knows the agent store.
func validate(t Task) (Task, error) {
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" {
		return Task{}, errors.New("name is required")
	}
	t.Prompt = strings.TrimSpace(t.Prompt)
	if t.Prompt == "" {
		return Task{}, errors.New("prompt is required")
	}
	t.Cron = strings.TrimSpace(t.Cron)
	if _, err := ParseSpec(t.Cron); err != nil {
		return Task{}, err
	}
	return t, nil
}

func (s *Store) Create(t Task) (Task, error) {
	t, err := validate(t)
	if err != nil {
		return Task{}, err
	}
	t.ID = newID()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.commitLocked(append(s.copyLocked(), t)); err != nil {
		return Task{}, err
	}
	return t, nil
}

// Update replaces the task with the given id, keeping the id from the stored
// one, so a client may PUT back the object it was given.
func (s *Store) Update(id string, t Task) (Task, error) {
	t, err := validate(t)
	if err != nil {
		return Task{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.copyLocked()
	i := indexOf(next, id)
	if i < 0 {
		return Task{}, fmt.Errorf("schedule %q not found", id)
	}
	t.ID = id
	next[i] = t
	if err := s.commitLocked(next); err != nil {
		return Task{}, err
	}
	return t, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.copyLocked()
	i := indexOf(next, id)
	if i < 0 {
		return fmt.Errorf("schedule %q not found", id)
	}
	return s.commitLocked(append(next[:i], next[i+1:]...))
}

// copyLocked returns tasks the caller may rearrange without disturbing the
// store's own slice.
func (s *Store) copyLocked() []Task {
	return append([]Task(nil), s.tasks...)
}

// commitLocked saves tasks and adopts them only once that lands, so a store
// that failed to save is a store that did not change.
func (s *Store) commitLocked(tasks []Task) error {
	if s.save == nil {
		return errors.New("schedule store is not writable")
	}
	if tasks == nil {
		tasks = []Task{}
	}
	if err := s.save(tasks); err != nil {
		return err
	}
	s.tasks = tasks
	return nil
}

func writeFile(path string, tasks []Task) error {
	data, err := json.MarshalIndent(fileFormat{Schedules: tasks}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode schedules file: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create schedules directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write schedules file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace schedules file: %w", err)
	}
	return nil
}

// newID is random rather than the name: names are prose the user rewrites, and
// renaming must not silently point a task's file entry at something else.
func newID() string {
	var b [8]byte
	rand.Read(b[:]) // documented never to fail
	return fmt.Sprintf("%x", b[:])
}
