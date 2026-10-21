package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// topicStore persists the session→forum-topic mapping so usher re-adopts
// existing topics across restarts instead of recreating them (mirroring the
// tmux "re-adopt by list-windows" approach). It is safe for concurrent use.
type topicStore struct {
	mu       sync.Mutex
	path     string           // empty = in-memory only (tests)
	threads  map[string]int64 // sessionID → message_thread_id (persisted)
	byThread map[int64]string // message_thread_id → sessionID (reverse, derived)
}

func newTopicStore(path string) (*topicStore, error) {
	s := &topicStore{path: path, threads: map[string]int64{}, byThread: map[int64]string{}}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // first run
		}
		return nil, err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &s.threads); err != nil {
			return nil, err
		}
	}
	for sess, thread := range s.threads {
		s.byThread[thread] = sess
	}
	return s, nil
}

// thread returns the topic id bound to sessionID, if any.
func (s *topicStore) thread(sessionID string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.threads[sessionID]
	return id, ok
}

// session returns the session id bound to a forum topic, if any — the reverse
// of thread, used to route an inbound topic message to its session.
func (s *topicStore) session(threadID int64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byThread[threadID]
	return id, ok
}

// all returns a snapshot copy of the session→topic map, for the reconcile
// loop to iterate without holding the lock.
func (s *topicStore) all() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.threads))
	for k, v := range s.threads {
		out[k] = v
	}
	return out
}

// put binds sessionID to threadID and persists the map.
func (s *topicStore) put(sessionID string, threadID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.threads[sessionID] = threadID
	s.byThread[threadID] = sessionID
	return s.persistLocked()
}

// delete drops sessionID's binding and persists. No-op if absent.
func (s *topicStore) delete(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	thread, ok := s.threads[sessionID]
	if !ok {
		return nil
	}
	delete(s.threads, sessionID)
	delete(s.byThread, thread)
	return s.persistLocked()
}

// persistLocked writes the map via temp-file + rename. Caller holds s.mu.
func (s *topicStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.threads)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
