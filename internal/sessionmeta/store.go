// Package sessionmeta persists per-session user decisions (archive, pin)
// in sessions.json under <data-dir>/.
package sessionmeta

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const DefaultAutoArchiveAfter = 7 * 24 * time.Hour

type archiveDecision string

const (
	decisionArchived archiveDecision = "archived"
	decisionShown    archiveDecision = "shown"
)

type fileFormat struct {
	Archived           map[string]archiveDecision `json:"archived,omitempty"`
	Pinned             []string                   `json:"pinned,omitempty"`
	Titles             map[string]string          `json:"titles,omitempty"`
	AppendSystemPrompt map[string]string          `json:"append_system_prompt,omitempty"`
	ExtraArgs          map[string][]string        `json:"extra_args,omitempty"`
}

type Store struct {
	path      string
	autoAfter time.Duration

	mu                 sync.Mutex
	archived           map[string]archiveDecision
	pinned             map[string]bool
	titles             map[string]string
	appendSystemPrompt map[string]string
	extraArgs          map[string][]string
}

func New(path string, autoAfter time.Duration) *Store {
	if autoAfter < 0 {
		autoAfter = 0
	}
	s := &Store{
		path:               path,
		autoAfter:          autoAfter,
		archived:           map[string]archiveDecision{},
		pinned:             map[string]bool{},
		titles:             map[string]string{},
		appendSystemPrompt: map[string]string{},
		extraArgs:          map[string][]string{},
	}
	s.load()
	return s
}

func (s *Store) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("sessionmeta: read", "path", s.path, "err", err)
		}
		return
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		slog.Warn("sessionmeta: decode", "path", s.path, "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range f.Archived {
		if v == decisionArchived || v == decisionShown {
			s.archived[k] = v
		}
	}
	for _, id := range f.Pinned {
		s.pinned[id] = true
	}
	for id, t := range f.Titles {
		s.titles[id] = t
	}
	for id, prompt := range f.AppendSystemPrompt {
		s.appendSystemPrompt[id] = prompt
	}
	for id, args := range f.ExtraArgs {
		if len(args) > 0 {
			s.extraArgs[id] = args
		}
	}
}

func (s *Store) persist() {
	if s.path == "" {
		return
	}
	pinned := make([]string, 0, len(s.pinned))
	for id := range s.pinned {
		pinned = append(pinned, id)
	}
	var titles map[string]string
	if len(s.titles) > 0 {
		titles = s.titles
	}
	var prompts map[string]string
	if len(s.appendSystemPrompt) > 0 {
		prompts = s.appendSystemPrompt
	}
	var extraArgs map[string][]string
	if len(s.extraArgs) > 0 {
		extraArgs = s.extraArgs
	}
	data, err := json.Marshal(fileFormat{
		Archived: s.archived, Pinned: pinned, Titles: titles,
		AppendSystemPrompt: prompts, ExtraArgs: extraArgs,
	})
	if err != nil {
		slog.Warn("sessionmeta: encode", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		slog.Warn("sessionmeta: mkdir", "err", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("sessionmeta: write", "err", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		slog.Warn("sessionmeta: rename", "err", err)
	}
}

// Archive drops the pin — a pin overrides archiving, so keeping both would make
// this a no-op.
func (s *Store) Archive(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archived[id] == decisionArchived && !s.pinned[id] {
		return
	}
	delete(s.pinned, id)
	s.archived[id] = decisionArchived
	s.persist()
}

func (s *Store) Unarchive(id string, lastEventAt time.Time, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fresh := s.autoAfter == 0 ||
		(!lastEventAt.IsZero() && now.Sub(lastEventAt) <= s.autoAfter)
	if fresh {
		if _, ok := s.archived[id]; !ok {
			return
		}
		delete(s.archived, id)
	} else {
		if s.archived[id] == decisionShown {
			return
		}
		s.archived[id] = decisionShown
	}
	s.persist()
}

// A pin wins over the auto-archive timer and over an earlier explicit archive,
// as an overlay: unpinning restores the underlying state.
func (s *Store) IsArchived(id string, lastEventAt time.Time, now time.Time) bool {
	s.mu.Lock()
	d, ok := s.archived[id]
	pinned := s.pinned[id]
	s.mu.Unlock()
	if pinned {
		return false
	}
	if ok {
		return d == decisionArchived
	}
	if s.autoAfter == 0 || lastEventAt.IsZero() {
		return false
	}
	return now.Sub(lastEventAt) > s.autoAfter
}

func (s *Store) Pin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pinned[id] {
		return
	}
	s.pinned[id] = true
	s.persist()
}

func (s *Store) Unpin(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pinned[id] {
		return
	}
	delete(s.pinned, id)
	s.persist()
}

func (s *Store) IsPinned(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinned[id]
}

func (s *Store) Rename(id, title string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if title == "" {
		delete(s.titles, id)
	} else {
		s.titles[id] = title
	}
	s.persist()
}

func (s *Store) CustomTitle(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.titles[id]
}

func (s *Store) SetAppendSystemPrompt(id, prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prompt == "" {
		if _, ok := s.appendSystemPrompt[id]; !ok {
			return
		}
		delete(s.appendSystemPrompt, id)
	} else {
		if s.appendSystemPrompt[id] == prompt {
			return
		}
		s.appendSystemPrompt[id] = prompt
	}
	s.persist()
}

func (s *Store) AppendSystemPrompt(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendSystemPrompt[id]
}

func (s *Store) SetExtraArgs(id string, args []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(args) == 0 {
		if _, ok := s.extraArgs[id]; !ok {
			return
		}
		delete(s.extraArgs, id)
	} else {
		if slices.Equal(s.extraArgs[id], args) {
			return
		}
		s.extraArgs[id] = append([]string(nil), args...)
	}
	s.persist()
}

func (s *Store) ExtraArgs(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.extraArgs[id]...)
}

// Forget drops all state for id.
func (s *Store) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hasArchive := s.archived[id]
	hasPin := s.pinned[id]
	_, hasTitle := s.titles[id]
	_, hasPrompt := s.appendSystemPrompt[id]
	_, hasArgs := s.extraArgs[id]
	if !hasArchive && !hasPin && !hasTitle && !hasPrompt && !hasArgs {
		return
	}
	delete(s.archived, id)
	delete(s.pinned, id)
	delete(s.titles, id)
	delete(s.appendSystemPrompt, id)
	delete(s.extraArgs, id)
	s.persist()
}
