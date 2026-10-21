// Package settings persists instance-level settings that are changed from the
// UI, in settings.json under <data-dir>/.
package settings

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Router is usher's one router session: the session that holds the
// session-routing tools and receives routed replies.
type Router struct {
	// SessionID is the current router session. It may name a session that no
	// longer exists; readers check.
	SessionID string `json:"session_id,omitempty"`
}

// fileFormat keeps agents and schedules raw: their packages own their shape.
type fileFormat struct {
	Router    Router          `json:"router,omitzero"`
	Agents    json.RawMessage `json:"agents,omitempty"`
	Schedules json.RawMessage `json:"schedules,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	path string
	f    fileFormat
}

// New loads the settings at path. An empty path keeps them in memory.
func New(path string) *Store {
	s := &Store{path: path}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("settings: read", "path", path, "err", err)
		}
		return s
	}
	if err := json.Unmarshal(data, &s.f); err != nil {
		slog.Warn("settings: decode", "path", path, "err", err)
	}
	return s
}

func (s *Store) Router() Router {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Router
}

func (s *Store) SetRouterSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Router.SessionID = id
	if err := s.persistLocked(); err != nil {
		slog.Warn("settings: save", "err", err)
	}
}

func (s *Store) Agents() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Agents
}

func (s *Store) SetAgents(v any) error { return s.setRaw(&s.f.Agents, v) }

func (s *Store) Schedules() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Schedules
}

func (s *Store) SetSchedules(v any) error { return s.setRaw(&s.f.Schedules, v) }

// setRaw saves v as field, keeping the old value if that fails.
func (s *Store) setRaw(field *json.RawMessage, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := *field
	*field = data
	if err := s.persistLocked(); err != nil {
		*field = prev
		return err
	}
	return nil
}

// ImportFile moves the agents or schedules of a file from before settings.json
// held them — the same key at its top level — into this store, then removes
// the file. A section already here wins.
func (s *Store) ImportFile(key, path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(data, &legacy); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var field *json.RawMessage
	switch key {
	case "agents":
		field = &s.f.Agents
	case "schedules":
		field = &s.f.Schedules
	default:
		return fmt.Errorf("settings has no %q section", key)
	}
	if len(*field) == 0 && len(legacy[key]) > 0 {
		*field = legacy[key]
		if err := s.persistLocked(); err != nil {
			*field = nil
			return err
		}
	}
	return os.Remove(path)
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create settings directory: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}
	return nil
}
