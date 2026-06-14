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

	"github.com/fsnotify/fsnotify"

	"usher/internal/core"
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
	}
	return nil
}

// Upsert synchronously ingests the session file at path. fsnotify would pick
// it up anyway; callers that are about to hand out the session id (fork) call
// this so the id resolves immediately instead of racing the watcher.
func (d *Discovery) Upsert(path string) { d.upsert(path) }

// upsert reads a jsonl file's metadata if unknown, or just bumps the
// last-event timestamp from file mtime if already known. The full jsonl is
// scanned once at first sight; subsequent writes during streaming only touch
// mtime, avoiding repeated full-file reads.
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
		// cwd/title come from jsonl content written after the file appears, so
		// the first read can miss them; re-read while either is still empty
		// (self-limiting once both are set — no re-parse on every later write).
		if existing.Cwd == "" || existing.Title == "" {
			if meta, err := src.ReadMeta(path); err == nil {
				if existing.Cwd == "" {
					existing.Cwd = meta.Cwd
				}
				if existing.Title == "" {
					existing.Title = meta.Title
				}
				if existing.StartedAt.IsZero() {
					existing.StartedAt = meta.StartedAt
				}
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
		Cwd:         meta.Cwd,
		Title:       meta.Title,
		Status:      core.StatusIdle,
		StartedAt:   meta.StartedAt,
		LastEventAt: info.ModTime(),
		Backend:     src.Backend(),
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = info.ModTime()
	}

	d.mu.Lock()
	d.sessions[id] = sess
	d.paths[id] = path
	d.mu.Unlock()
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
	switch {
	case ev.Op.Has(fsnotify.Create):
		info, err := os.Stat(ev.Name)
		if err != nil {
			return
		}
		if info.IsDir() {
			if err := d.watcher.Add(ev.Name); err != nil {
				d.logger.Warn("watch new subdir", "path", ev.Name, "err", err)
			}
			return
		}
		d.upsert(ev.Name) // upsert resolves the owning source, no-ops if none
	case ev.Op.Has(fsnotify.Write):
		d.upsert(ev.Name)
	case ev.Op.Has(fsnotify.Remove), ev.Op.Has(fsnotify.Rename):
		d.remove(ev.Name)
	}
}

// List returns sessions sorted by most recent activity first.
func (d *Discovery) List() []core.Session {
	d.mu.RLock()
	out := make([]core.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		out = append(out, s)
	}
	d.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastEventAt.After(out[j].LastEventAt)
	})
	return out
}

// Get returns a session by ID. The bool is false if not found.
func (d *Discovery) Get(id string) (core.Session, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.sessions[id]
	return s, ok
}

// Path returns the on-disk jsonl path for a session ID.
func (d *Discovery) Path(id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	p, ok := d.paths[id]
	return p, ok
}
