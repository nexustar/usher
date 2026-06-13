// Package core holds shared types referenced by multiple internal packages.
package core

import "time"

// Status describes a session's relationship to usher's interactive process
// pool. "live" means usher holds a warm claude process for it (idle, ready to
// answer instantly); "running" means a turn is actively executing in that
// process. A plain discovered session usher hasn't loaded is "idle".
type Status string

const (
	StatusIdle               Status = "idle"
	StatusLive               Status = "live"
	StatusRunning            Status = "running"
	StatusAwaitingPermission Status = "awaiting_permission"
)

// Session is the projection of a Claude Code session that usher manages.
// In v0.1 it is a derived view of a jsonl file at
// ~/.claude/projects/<sanitized-cwd>/<id>.jsonl.
type Session struct {
	ID          string    `json:"id"`
	Cwd         string    `json:"cwd"`
	Title       string    `json:"title"`
	Status      Status    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	LastEventAt time.Time `json:"last_event_at"`

	// Backend names the agent CLI this session belongs to ("claude" or "codex").
	// usher manages both at once; a session belongs to one for its life. Set by
	// discovery from the Source that found the session's log.
	Backend string `json:"backend"`
}

// TranscriptTurn is a single user/assistant turn extracted from a session
// jsonl. Shared across packages so the router and the LLM agent can pass
// transcripts through without copying types.
type TranscriptTurn struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Time    time.Time `json:"ts"`
}
