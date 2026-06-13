package discovery

import (
	"os"
	"path/filepath"
	"strings"

	"usher/internal/codexrollout"
	"usher/internal/jsonl"
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
	// IsSessionFile reports whether path is a top-level session log, as opposed
	// to a subagent transcript, tool-result blob, or other nested artifact that
	// must not show up as a fake session.
	IsSessionFile(path string) bool
	// SessionID extracts the session id from a session file path, or "" if none.
	SessionID(path string) string
	// ReadMeta reads the lightweight descriptor used for listing.
	ReadMeta(path string) (jsonl.SessionMeta, error)
}

// ClaudeSource scans Claude Code's projects tree:
// <root>/<sanitized-cwd>/<id>.jsonl, where the id is the bare filename.
type ClaudeSource struct{ root string }

func NewClaudeSource(root string) ClaudeSource { return ClaudeSource{root: root} }

func (s ClaudeSource) Backend() string { return "claude" }
func (s ClaudeSource) Root() string    { return s.root }

// IsSessionFile accepts only top-level project files (<root>/<cwd>/<id>.jsonl,
// i.e. exactly one separator below root). Subagent transcripts
// (<cwd>/<id>/subagents/agent-*.jsonl), tool-result blobs, and Claude's
// auto-memory .md files all live deeper and are filtered out — subagents in
// particular carry non-UUID ids that `claude --resume` refuses.
func (s ClaudeSource) IsSessionFile(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return false
	}
	return strings.Count(rel, string(os.PathSeparator)) == 1
}

func (s ClaudeSource) SessionID(path string) string {
	name := filepath.Base(path)
	if !strings.HasSuffix(name, ".jsonl") {
		return ""
	}
	return strings.TrimSuffix(name, ".jsonl")
}

func (s ClaudeSource) ReadMeta(path string) (jsonl.SessionMeta, error) {
	return jsonl.ReadSessionMeta(path)
}

// CodexSource scans Codex CLI's rollout tree:
// <root>/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl, where the id is the UUID embedded
// in the filename. The date partitioning is handled by Discovery's recursive
// watch, so a session is recognized purely by its filename shape rather than by
// depth — robust to Codex reorganizing the tree.
type CodexSource struct{ root string }

func NewCodexSource(root string) CodexSource { return CodexSource{root: root} }

func (s CodexSource) Backend() string { return "codex" }
func (s CodexSource) Root() string    { return s.root }

// IsSessionFile accepts rollout files by their name (rollout-…-<uuid>.jsonl).
// Codex keeps archived sessions under a sibling ~/.codex/archived_sessions, a
// different root that is simply not scanned, so every rollout under Root is a
// live session.
func (s CodexSource) IsSessionFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") &&
		strings.HasSuffix(base, ".jsonl") &&
		codexrollout.SessionIDFromPath(base) != ""
}

func (s CodexSource) SessionID(path string) string {
	return codexrollout.SessionIDFromPath(path)
}

func (s CodexSource) ReadMeta(path string) (jsonl.SessionMeta, error) {
	return codexrollout.ReadSessionMeta(path)
}
