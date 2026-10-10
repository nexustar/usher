package discovery

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nexustar/usher/internal/claude/jsonl"
	"github.com/nexustar/usher/internal/codex/rollout"
	"github.com/nexustar/usher/internal/core"
	piagent "github.com/nexustar/usher/internal/pi"
)

// Source describes where one backend's session logs live on disk and how to
// read them, so Discovery can scan Claude Code and Codex side by side without
// knowing either layout. Discovery handles the watching/caching; a Source only
// answers "is this file a session, what's its id, and what's its metadata".
type Source interface {
	// Backend names the agent CLI this source's sessions belong to
	// ("claude" or "codex"); stamped onto each discovered core.Session.
	Backend() string
	// Root is the directory to scan and recursively watch.
	Root() string
	// IsSessionFile reports whether path is a root or subagent session log, as
	// opposed to a tool-result blob or unrelated nested artifact.
	IsSessionFile(path string) bool
	// SessionID extracts the session id from a session file path, or "" if none.
	SessionID(path string) string
	// NewMetaScanner starts a read of the log at path.
	NewMetaScanner(path string) MetaScanner
	// Sparse reports whether a log's first and last lines hold what a listing
	// needs, so the ones between can go unread.
	Sparse() bool
}

// MetaScanner folds a session log's lines, fed in file order, into the
// descriptor used for listing. Lines may be skipped, never reordered.
type MetaScanner interface {
	Feed(line []byte)
	Meta() core.SessionMeta
}

// MetadataSource lists metadata files that affect cached sessions.
type MetadataSource interface {
	MetadataFiles() []string
	RefreshMetadata(string) (map[string]string, error)
}

// ClaudeSource scans Claude Code's projects tree:
// <root>/<sanitized-cwd>/<id>.jsonl, where the id is the bare filename.
type ClaudeSource struct{ root string }

func NewClaudeSource(root string) ClaudeSource { return ClaudeSource{root: root} }

func (s ClaudeSource) Backend() string { return "claude" }
func (s ClaudeSource) Root() string    { return s.root }

// IsSessionFile accepts top-level sessions and their nested subagent
// transcripts. Other nested JSONL artifacts remain excluded.
func (s ClaudeSource) IsSessionFile(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) == 2 {
		return true
	}
	return len(parts) >= 4 && parts[2] == "subagents" && strings.HasPrefix(filepath.Base(path), "agent-")
}

// subagentParent returns the parent session id when path is a Claude subagent
// transcript (<cwd>/<parent-id>/subagents/agent-*.jsonl), else "", false.
func (s ClaudeSource) subagentParent(path string) (string, bool) {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) >= 4 && parts[2] == "subagents" {
		return parts[1], true
	}
	return "", false
}

func (s ClaudeSource) SessionID(path string) string {
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	id := strings.TrimSuffix(name, ".jsonl")
	if parent, ok := s.subagentParent(path); ok {
		return parent + "::" + id
	}
	return id
}

func (s ClaudeSource) Sparse() bool { return true }

func (s ClaudeSource) NewMetaScanner(path string) MetaScanner {
	return claudeScanner{jsonl.NewMetaScanner(path), s, path}
}

type claudeScanner struct {
	*jsonl.MetaScanner
	src  ClaudeSource
	path string
}

func (c claudeScanner) Meta() core.SessionMeta {
	meta := c.MetaScanner.Meta()
	if parent, ok := c.src.subagentParent(c.path); ok {
		meta.ParentID = parent
		meta.IsSubagent = true
		// The id must stay unique, so it's always the agent-<hash> filename;
		// AgentName is the human label (attributionAgent) and falls back to
		// that same hash only when absent.
		fileID := strings.TrimSuffix(filepath.Base(c.path), ".jsonl")
		meta.ID = meta.ParentID + "::" + fileID
		if meta.AgentName == "" {
			meta.AgentName = fileID
		}
	}
	return meta
}

// CodexSource scans Codex CLI's rollout tree:
// <root>/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl, where the id is the UUID embedded
// in the filename. The date partitioning is handled by Discovery's recursive
// watch, so a session is recognized purely by its filename shape rather than by
// depth — robust to Codex reorganizing the tree.
type CodexSource struct {
	root, indexPath string
	mu              sync.Mutex
	names           map[string]string
}

func NewCodexSource(root string) *CodexSource {
	return &CodexSource{root: root, indexPath: filepath.Join(filepath.Dir(root), "session_index.jsonl")}
}

func (s *CodexSource) Backend() string { return "codex" }
func (s *CodexSource) Root() string    { return s.root }

// IsSessionFile accepts rollout files by their name (rollout-…-<uuid>.jsonl).
// Codex keeps archived sessions under a sibling ~/.codex/archived_sessions, a
// different root that is simply not scanned, so every rollout under Root is a
// live session.
func (s *CodexSource) IsSessionFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") &&
		strings.HasSuffix(base, ".jsonl") &&
		rollout.SessionIDFromPath(base) != ""
}

func (s *CodexSource) SessionID(path string) string {
	return rollout.SessionIDFromPath(path)
}

func (s *CodexSource) Sparse() bool { return true }

func (s *CodexSource) NewMetaScanner(path string) MetaScanner {
	return codexScanner{rollout.NewMetaScanner(path), s}
}

type codexScanner struct {
	*rollout.MetaScanner
	src *CodexSource
}

func (c codexScanner) Meta() core.SessionMeta {
	meta := c.MetaScanner.Meta()
	names, _ := c.src.threadNames(false)
	meta.Title = names[meta.ID]
	return meta
}

func (s *CodexSource) MetadataFiles() []string { return []string{s.indexPath} }

func (s *CodexSource) RefreshMetadata(path string) (map[string]string, error) {
	if filepath.Clean(path) != filepath.Clean(s.indexPath) {
		return nil, os.ErrNotExist
	}
	return s.threadNames(true)
}

func (s *CodexSource) threadNames(refresh bool) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.names == nil || refresh {
		names, err := rollout.ReadThreadNames(s.indexPath)
		if err != nil {
			return nil, err
		}
		s.names = names
	}
	return s.names, nil
}

// PiSource scans pi's per-working-directory session tree. Directory names are
// an implementation detail; the stable session id and cwd live in the header.
type PiSource struct{ root string }

func NewPiSource(root string) PiSource { return PiSource{root: root} }
func (s PiSource) Backend() string     { return "pi" }
func (s PiSource) Root() string        { return s.root }
func (s PiSource) IsSessionFile(path string) bool {
	return strings.HasSuffix(filepath.Base(path), ".jsonl")
}
func (s PiSource) SessionID(path string) string { return piagent.SessionIDFromPath(path) }

// A rename is recorded once, wherever in the log it happened.
func (s PiSource) Sparse() bool                      { return false }
func (s PiSource) NewMetaScanner(string) MetaScanner { return piagent.NewMetaScanner() }
