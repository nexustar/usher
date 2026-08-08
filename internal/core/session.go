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

// Session is the backend-neutral projection of a discovered conversation
// transcript. Subagent sessions are read-only children of a root session.
type Session struct {
	ID          string    `json:"id"`
	ParentID    string    `json:"parent_id,omitempty"`
	IsSubagent  bool      `json:"is_subagent,omitempty"`
	AgentName   string    `json:"agent_name,omitempty"`
	Cwd         string    `json:"cwd"`
	Title       string    `json:"title"`
	Prompt      string    `json:"-"`
	Status      Status    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	LastEventAt time.Time `json:"last_event_at"`

	// LastInputAt is the time of the most recent genuine user prompt. Unlike
	// LastEventAt (file mtime) it ignores assistant streaming, tool turns, and
	// the untimed metadata claude writes on pause/kill, so it is the sidebar
	// sort key and auto-archive clock: a session only reorders when the user
	// talks to it. Seeded from jsonl at discovery, stamped on usher sends.
	LastInputAt time.Time `json:"last_input_at"`

	// Backend names the agent CLI this session belongs to ("claude" or "codex").
	// usher manages both at once; a session belongs to one for its life. Set by
	// discovery from the Source that found the session's log.
	Backend string         `json:"backend"`
	Runtime SessionRuntime `json:"runtime"`
}

// TranscriptTurn is a single user/assistant turn extracted from a session
// jsonl. Shared across packages so the router and the LLM agent can pass
// transcripts through without copying types.
type TranscriptTurn struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Time    time.Time `json:"ts"`
}

// SessionRuntime is the latest model configuration and active context recorded
// for a session. Account-wide limits deliberately do not belong here.
type SessionRuntime struct {
	Model         string `json:"model,omitempty"`
	Effort        string `json:"effort,omitempty"`
	ContextTokens int64  `json:"context_tokens,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
}

// TranscriptSearchHit is one matching turn from a transcript substring search.
// TurnIndex is the 0-based position of the turn within the full transcript
// (newest last), letting a caller locate the surrounding context with a
// follow-up read. Snippet is the matched text with limited context on either
// side of the first occurrence; Occurrences counts all matches in that turn.
type TranscriptSearchHit struct {
	Role        string    `json:"role"`
	Time        time.Time `json:"ts"`
	TurnIndex   int       `json:"turn_index"`
	Occurrences int       `json:"occurrences"`
	Snippet     string    `json:"snippet"`
}

// SessionSearchResult is one session's summary from a cross-session search:
// how many turns matched and a snippet at the first hit, enough to decide
// which session to open or route to. TurnIndex locates the first hit for a
// follow-up read_session_transcript.
type SessionSearchResult struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Cwd       string `json:"cwd"`
	HitCount  int    `json:"hit_count"`
	TurnIndex int    `json:"turn_index"`
	Snippet   string `json:"snippet"`
}

// CreateOptions describes a session to create. Every field is optional except
// Cwd and InitialMessage: an empty Model takes the backend's default, and an
// empty Backend preserves the legacy model-name inference for callers that
// don't know which agent owns the selected model.
type CreateOptions struct {
	Backend            string
	Cwd                string
	InitialMessage     string
	Model              string
	AppendSystemPrompt string
	ExtraArgs          []string
	AutoApprove        bool
}
