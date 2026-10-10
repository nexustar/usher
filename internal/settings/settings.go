// Package settings persists instance-level settings that are changed from the
// UI, in settings.json under <data-dir>/.
package settings

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Router is usher's one router session: the session that holds the
// session-routing tools and receives routed replies.
type Router struct {
	// Agent names the agent new router sessions are created from. Empty uses
	// the built-in router agent.
	Agent string `json:"agent,omitempty"`
	// SessionID is the current router session. It may name a session that no
	// longer exists; readers check.
	SessionID string `json:"session_id,omitempty"`
}

type fileFormat struct {
	Router Router `json:"router,omitzero"`
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

func (s *Store) SetRouterAgent(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Router.Agent = name
	s.persist()
}

func (s *Store) SetRouterSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Router.SessionID = id
	s.persist()
}

func (s *Store) persist() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.f)
	if err != nil {
		slog.Warn("settings: encode", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		slog.Warn("settings: mkdir", "err", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("settings: write", "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("settings: rename", "err", err)
	}
}
