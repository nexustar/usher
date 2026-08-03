// Package discovery scans Claude Code's projects directory and watches it for
// changes, exposing the set of known sessions.
//
// Sessions are not "owned" by usher: discovery is purely observational. A
// session jsonl file appearing on disk is enough; usher does not need to have
// launched the Claude Code process.
package discovery

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/nexustar/usher/internal/core"
)

type Discovery struct {
	sources []Source
	logger  *slog.Logger
	watcher *fsnotify.Watcher

	mu       sync.RWMutex
	sessions map[string]core.Session // by id
	paths    map[string]string       // id -> path
}

// NewMulti builds a Discovery that scans and watches several backend layouts at
// once (Claude Code and Codex), merging them into one session view. Each session
// is tagged with the Backend of the Source that found it; the Source decides
// which files are sessions and how to read them, everything else — scanning,
// watching, caching — is backend-agnostic.
func NewMulti(logger *slog.Logger, sources ...Source) (*Discovery, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Discovery{
		sources:  sources,
		logger:   logger,
		watcher:  w,
		sessions: map[string]core.Session{},
		paths:    map[string]string{},
	}, nil
}

// sourceFor returns the Source that owns path — the one whose Root contains it
// and whose IsSessionFile accepts it — or nil if path is not a session log.
func (d *Discovery) sourceFor(path string) Source {
	for _, s := range d.sources {
		root := s.Root()
		if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
			if s.IsSessionFile(path) {
				return s
			}
		}
	}
	return nil
}

// Start performs an initial scan, registers fsnotify watches on the root and
// every existing project subdirectory, and spawns the watch goroutine.
func (d *Discovery) Start(ctx context.Context) error {
	if err := d.scan(); err != nil {
		return err
	}
	if err := d.addWatches(); err != nil {
		return err
	}
	go d.run(ctx)
	return nil
}

// scan walks every source's root once and upserts each session file found.
func (d *Discovery) scan() error {
	for _, s := range d.sources {
		_ = filepath.Walk(s.Root(), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				// Best-effort: skip unreadable subtrees.
				return nil
			}
			if info.IsDir() {
				return nil
			}
			if s.IsSessionFile(path) {
				d.upsert(path)
			}
			return nil
		})
	}
	return nil
}

// addWatches registers watches on each source's root and every existing subdir,
// so new files are seen no matter which project (or, for Codex, which date
// partition) they appear under. A missing root (e.g. Codex never used) is
// skipped, not fatal.
func (d *Discovery) addWatches() error {
	for _, s := range d.sources {
		root := s.Root()
		if err := d.watcher.Add(root); err != nil {
			d.logger.Warn("watch root", "path", root, "err", err)
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() && path != root {
				if err := d.watcher.Add(path); err != nil {
					d.logger.Warn("watch subdir", "path", path, "err", err)
				}
			}
			return nil
		})
		if ms, ok := s.(MetadataSource); ok {
			for _, path := range ms.MetadataFiles() {
				if err := d.watcher.Add(filepath.Dir(path)); err != nil {
					d.logger.Warn("watch metadata dir", "path", filepath.Dir(path), "err", err)
				}
			}
		}
	}
	return nil
}

// Upsert synchronously ingests the session file at path. fsnotify would pick
// it up anyway; callers that are about to hand out the session id (fork) call
// this so the id resolves immediately instead of racing the watcher.
func (d *Discovery) Upsert(path string) { d.upsert(path) }

// upsert reads and caches a jsonl file's metadata. Known sessions are re-read
// when the file changes so cumulative usage stays current in the projection.
func (d *Discovery) upsert(path string) {
	src := d.sourceFor(path)
	if src == nil {
		return
	}
	id := src.SessionID(path)
	if id == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	d.mu.RLock()
	existing, known := d.sessions[id]
	d.mu.RUnlock()

	if known {
		existing.LastEventAt = info.ModTime()
		// cwd/prompt/title can land after the session file appears. Native
		// rename commands also change title after it was first populated.
		needTitle := existing.Title == ""
		if meta, err := src.ReadMeta(path); err == nil {
			// Claude's live result event is the authoritative source because it
			// includes the effective max window. Transcript usage is only a
			// fallback; never let a later fsnotify scan erase a captured window.
			if existing.Backend == "pi" {
				// Effort is persisted, but only once changed, so an empty one
				// means "unknown here", never "cleared". Transcript usage is
				// likewise a cold fallback: pi's own snapshot estimates the
				// whole branch where the file only counts the last message.
				// Neither reports anything after a compaction, so filling a
				// zero cannot resurrect a stale total.
				if existing.Runtime.Model == "" {
					existing.Runtime.Model = meta.Runtime.Model
				}
				if meta.Runtime.Effort != "" {
					existing.Runtime.Effort = meta.Runtime.Effort
				}
				if existing.Runtime.ContextTokens == 0 {
					existing.Runtime.ContextTokens = meta.Runtime.ContextTokens
				}
			} else if existing.Backend != "claude" || existing.Runtime.ContextWindow == 0 {
				existing.Runtime = meta.Runtime
			}
			if existing.Cwd == "" || existing.Prompt == "" || existing.LastInputAt.IsZero() || needTitle {
				applySubagentMeta(&existing, meta)
				if existing.Cwd == "" {
					existing.Cwd = meta.Cwd
				}
				if existing.Title == "" {
					existing.Title = meta.Title
				}
				if existing.Prompt == "" {
					existing.Prompt = meta.Prompt
				}
				if existing.StartedAt.IsZero() {
					existing.StartedAt = meta.StartedAt
				}
				if existing.LastInputAt.IsZero() {
					existing.LastInputAt = meta.LastInputAt
				}
			}
			if meta.Title != "" {
				existing.Title = meta.Title
			}
		}
		d.mu.Lock()
		d.sessions[id] = existing
		d.mu.Unlock()
		return
	}

	meta, err := src.ReadMeta(path)
	if err != nil {
		d.logger.Warn("read session meta", "path", path, "err", err)
		return
	}
	sess := core.Session{
		ID:          id,
		ParentID:    meta.ParentID,
		IsSubagent:  meta.IsSubagent,
		AgentName:   meta.AgentName,
		Cwd:         meta.Cwd,
		Title:       meta.Title,
		Prompt:      meta.Prompt,
		Status:      core.StatusIdle,
		StartedAt:   meta.StartedAt,
		LastEventAt: info.ModTime(),
		LastInputAt: meta.LastInputAt,
		Backend:     src.Backend(),
		Runtime:     meta.Runtime,
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = info.ModTime()
	}
	applySubagentMeta(&sess, meta)

	d.mu.Lock()
	d.sessions[id] = sess
	d.paths[id] = path
	d.mu.Unlock()
}

// applySubagentMeta fills structural metadata that may arrive after the file's
// Create event. Never clear an already-known relationship on a partial read.
func applySubagentMeta(sess *core.Session, meta core.SessionMeta) {
	if !meta.IsSubagent {
		return
	}
	sess.IsSubagent = true
	if meta.ParentID != "" {
		sess.ParentID = meta.ParentID
	}
	if meta.AgentName != "" {
		sess.AgentName = meta.AgentName
	}
	// Human names (Codex nicknames, Claude attributionAgent) beat the task
	// prompt. Opaque agent-<hash> ids deliberately fall through to Prompt.
	if sess.AgentName != "" && !strings.HasPrefix(sess.AgentName, "agent-") {
		sess.Title = sess.AgentName
	}
}

func (d *Discovery) remove(path string) {
	if src := d.sourceFor(path); src != nil {
		d.Remove(src.SessionID(path))
	}
}

// Remove forgets a session by id. fsnotify would pick a file deletion up
// anyway; callers that delete the jsonl themselves call this so the id stops
// resolving immediately instead of racing the watcher (mirror of Upsert).
func (d *Discovery) Remove(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	delete(d.sessions, id)
	delete(d.paths, id)
	d.mu.Unlock()
}

// MarkInput advances a session's input clock to t without reading the file, so
// ordering reflects "last talked to" the instant usher sends. No-op for an
// unknown id (seeded from content at first-sight) and never moves backwards.
func (d *Discovery) MarkInput(id string, t time.Time) {
	if id == "" || t.IsZero() {
		return
	}
	d.mu.Lock()
	if s, ok := d.sessions[id]; ok && t.After(s.LastInputAt) {
		s.LastInputAt = t
		d.sessions[id] = s
	}
	d.mu.Unlock()
}

// UpdateRuntime merges newly observed model/context fields into the cached
// snapshot. It returns false when discovery has not seen the session yet.
func (d *Discovery) UpdateRuntime(id string, runtime core.SessionRuntime) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[id]
	if !ok {
		return false
	}
	if runtime.Model != "" {
		s.Runtime.Model = runtime.Model
	}
	if runtime.Effort != "" {
		s.Runtime.Effort = runtime.Effort
	}
	if runtime.ContextWindow > 0 {
		s.Runtime.ContextWindow = runtime.ContextWindow
	}
	if runtime.ContextTokens > 0 || (s.Backend == "pi" && runtime.ContextWindow > 0) {
		// Pi reports zero after compaction to clear stale usage.
		s.Runtime.ContextTokens = runtime.ContextTokens
	}
	d.sessions[id] = s
	return true
}

func (d *Discovery) run(ctx context.Context) {
	defer d.watcher.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-d.watcher.Events:
			if !ok {
				return
			}
			d.handle(ev)
		case err, ok := <-d.watcher.Errors:
			if !ok {
				return
			}
			d.logger.Warn("fsnotify error", "err", err)
		}
	}
}

func (d *Discovery) handle(ev fsnotify.Event) {
	if d.refreshMetadataFile(ev.Name) {
		return
	}
	switch {
	case ev.Op.Has(fsnotify.Create):
		info, err := os.Stat(ev.Name)
		if err != nil {
			return
		}
		if info.IsDir() {
			// Walk the new tree: MkdirAll may have created nested dirs
			// (e.g. 2026/07/01/) before fsnotify delivered this event,
			// so we must watch subdirs and ingest files that already exist.
			_ = filepath.Walk(ev.Name, func(path string, fi os.FileInfo, err error) error {
				if err != nil {
					return nil
				}
				if fi.IsDir() {
					if err := d.watcher.Add(path); err != nil {
						d.logger.Warn("watch new subdir", "path", path, "err", err)
					}
				} else {
					d.upsert(path)
				}
				return nil
			})
			return
		}
		d.upsert(ev.Name) // upsert resolves the owning source, no-ops if none
	case ev.Op.Has(fsnotify.Write):
		d.upsert(ev.Name)
	case ev.Op.Has(fsnotify.Remove), ev.Op.Has(fsnotify.Rename):
		d.remove(ev.Name)
	}
}

func (d *Discovery) refreshMetadataFile(path string) bool {
	for _, src := range d.sources {
		ms, ok := src.(MetadataSource)
		if !ok {
			continue
		}
		for _, metadataPath := range ms.MetadataFiles() {
			if filepath.Clean(path) != filepath.Clean(metadataPath) {
				continue
			}
			titles, err := ms.RefreshMetadata(path)
			if err != nil {
				d.logger.Warn("refresh metadata", "path", path, "err", err)
				return true
			}
			d.mu.Lock()
			for id, session := range d.sessions {
				if session.Backend == src.Backend() {
					session.Title = titles[id]
					resolveTitle(&session)
					d.sessions[id] = session
				}
			}
			d.mu.Unlock()
			return true
		}
	}
	return false
}

// List returns root sessions sorted by most recent user input. Subagents are
// discoverable by id but hidden from default listings.
func (d *Discovery) List() []core.Session {
	all := d.ListAll()
	out := all[:0]
	for _, s := range all {
		if !s.IsSubagent {
			out = append(out, s)
		}
	}
	return out
}

// ListAll includes read-only subagent transcripts.
func (d *Discovery) ListAll() []core.Session {
	d.mu.RLock()
	out := make([]core.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		resolveTitle(&s)
		out = append(out, s)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return sortKey(out[i]).After(sortKey(out[j]))
	})
	return out
}

// sortKey orders by last user input, falling back to last event for sessions
// with no recorded input yet (only system lines).
func sortKey(s core.Session) time.Time {
	if !s.LastInputAt.IsZero() {
		return s.LastInputAt
	}
	return s.LastEventAt
}

// Get returns a session by ID. The bool is false if not found.
func (d *Discovery) Get(id string) (core.Session, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.sessions[id]
	if ok {
		resolveTitle(&s)
	}
	return s, ok
}

// SetTitle updates the cache after a native rename.
func (d *Discovery) SetTitle(id, title string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sessions[id]
	if !ok {
		return
	}
	s.Title = title
	d.sessions[id] = s
}

// resolveTitle fills Title from Prompt when no ai-title has been seen yet.
func resolveTitle(s *core.Session) {
	if s.Title == "" && s.Prompt != "" {
		s.Title = s.Prompt
	}
}

// Path returns the on-disk jsonl path for a session ID.
func (d *Discovery) Path(id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.paths[id]
	return p, ok
}
