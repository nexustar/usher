package push

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// store is the persisted set of browser subscriptions, keyed by endpoint.
// Layout mirrors archive.Store / hook.autoApprove: a single JSON document
// written via tmp-file + rename under <data-dir>/push-subscriptions.json.
type store struct {
	path string

	mu   sync.Mutex
	subs map[string]Subscription // endpoint -> subscription
}

func newStore(path string) *store {
	s := &store{path: path, subs: map[string]Subscription{}}
	if path != "" {
		s.load()
	}
	return s
}

func (s *store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("push: read subscriptions", "path", s.path, "err", err)
		}
		return
	}
	var loaded []Subscription
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("push: decode subscriptions", "path", s.path, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range loaded {
		if sub.Endpoint != "" {
			s.subs[sub.Endpoint] = sub
		}
	}
}

// persist writes the current set atomically. Caller must hold s.mu.
func (s *store) persist() {
	if s.path == "" {
		return
	}
	list := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		list = append(list, sub)
	}
	data, err := json.Marshal(list)
	if err != nil {
		slog.Warn("push: encode subscriptions", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		slog.Warn("push: mkdir", "err", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("push: write tmp", "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("push: rename", "err", err)
	}
}

func (s *store) add(sub Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.subs[sub.Endpoint]; ok && existing == sub {
		return // unchanged; skip the disk write
	}
	s.subs[sub.Endpoint] = sub
	s.persist()
}

func (s *store) remove(endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[endpoint]; !ok {
		return
	}
	delete(s.subs, endpoint)
	s.persist()
}

func (s *store) all() []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Subscription, 0, len(s.subs))
	for _, sub := range s.subs {
		out = append(out, sub)
	}
	return out
}
