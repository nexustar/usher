package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// binding ties a session to its Lark thread: the root message (the reply
// target that anchors the thread) and, once known, the thread id inbound
// events carry.
type binding struct {
	Root   string `json:"root"`             // root message id (om_...)
	Thread string `json:"thread,omitempty"` // thread id (omt_...), learned from the first reply
	// Title is the session title the root card was last rendered with, so
	// the hub only patches the card when the title actually changed.
	Title string `json:"title,omitempty"`
}

// threadStore persists the session↔thread map so threads are re-adopted
// across restarts. Same temp+rename discipline as telegram's topic store.
type threadStore struct {
	mu       sync.Mutex
	path     string // "" = in-memory (tests)
	m        map[string]binding
	byRoot   map[string]string
	byThread map[string]string
}

func newThreadStore(path string) (*threadStore, error) {
	s := &threadStore{
		path:     path,
		m:        map[string]binding{},
		byRoot:   map[string]string{},
		byThread: map[string]string{},
	}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.m); err != nil {
		return nil, err
	}
	for id, b := range s.m {
		s.byRoot[b.Root] = id
		if b.Thread != "" {
			s.byThread[b.Thread] = id
		}
	}
	return s, nil
}

// root returns the root message id bound to sessionID.
func (s *threadStore) root(sessionID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[sessionID]
	return b.Root, ok
}

// put binds a session to a freshly created root message.
func (s *threadStore) put(sessionID, rootID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[sessionID] = binding{Root: rootID}
	s.byRoot[rootID] = sessionID
	return s.persistLocked()
}

// setThread records the thread id once a reply reveals it. No-op when already
// known or the session is unbound.
func (s *threadStore) setThread(sessionID, threadID string) error {
	if threadID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[sessionID]
	if !ok || b.Thread == threadID {
		return nil
	}
	if b.Thread != "" {
		delete(s.byThread, b.Thread)
	}
	b.Thread = threadID
	s.m[sessionID] = b
	s.byThread[threadID] = sessionID
	return s.persistLocked()
}

// title returns the title the session's root card was last rendered with.
func (s *threadStore) title(sessionID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[sessionID]
	return b.Title, ok
}

// setTitle records the title the root card now shows.
func (s *threadStore) setTitle(sessionID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[sessionID]
	if !ok || b.Title == title {
		return nil
	}
	b.Title = title
	s.m[sessionID] = b
	return s.persistLocked()
}

// session resolves an inbound message's thread or root id to a session.
func (s *threadStore) session(threadID, rootID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if threadID != "" {
		if id, ok := s.byThread[threadID]; ok {
			return id, true
		}
	}
	if rootID != "" {
		if id, ok := s.byRoot[rootID]; ok {
			return id, true
		}
	}
	return "", false
}

func (s *threadStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
