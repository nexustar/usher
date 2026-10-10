// Package core holds shared types referenced by multiple internal packages.
package core

import (
	"errors"
	"time"
)

// Status describes a session's relationship to usher's worker pool. "live"
// means usher holds a warm worker for it (idle, ready to answer instantly);
// "running" means that worker is mid-turn, usher's or the backend's own (a
// /loop tick). A plain discovered session usher hasn't loaded is "idle".
type Status string

const (
	StatusIdle               Status = "idle"
	StatusLive               Status = "live"
	StatusRunning            Status = "running"
	StatusAwaitingPermission Status = "awaiting_permission"
)

// Activity is what a session holds beyond a turn in flight: work that dies
// with its process, and standing instructions.
type Activity struct {
	// Agents are delegated agents still running: Claude background agent
	// tasks, Codex child threads mid-turn.
	Agents int `json:"agents,omitempty"`
	// Loops are recurring schedules (/loop 5m). The timers live in the
	// process; Claude re-creates them from the transcript on resume.
	Loops []Loop `json:"loops,omitempty"`
	// NextWake is a pending self-paced /loop tick.
	NextWake time.Time `json:"next_wake,omitzero"`
	// Goal is the active /goal. Claude records it in the transcript, so it
	// shows on cold sessions too; Codex reports it only while live.
	Goal *Goal `json:"goal,omitempty"`
}

// Loop is one recurring schedule; Schedule is the backend's own description.
type Loop struct {
	ID       string `json:"id"`
	Cron     string `json:"cron"`
	Schedule string `json:"schedule"`
	Prompt   string `json:"prompt,omitempty"`
}

// Goal is a condition the session keeps working toward.
type Goal struct {
	Condition string `json:"condition"`
	// Status is active, or one of Codex's: paused, blocked, usageLimited,
	// budgetLimited, complete. Claude's is always active while set.
	Status string `json:"status"`
	// Reason is Claude's last not-yet-met verdict.
	Reason string `json:"reason,omitempty"`
}

// The wake timer fires seconds late; the tick then pins the process itself.
const wakeGrace = 5 * time.Minute

// Pins reports whether stopping the process would lose something. A goal
// does not pin: nothing is lost once the process stops.
func (a Activity) Pins() bool {
	return a.Agents > 0 || len(a.Loops) > 0 ||
		(!a.NextWake.IsZero() && time.Now().Before(a.NextWake.Add(wakeGrace)))
}

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
	Activity    Activity  `json:"activity,omitzero"`
	StartedAt   time.Time `json:"started_at"`
	LastEventAt time.Time `json:"last_event_at"`

	// LastInputAt is the time of the most recent genuine user prompt. Unlike
	// LastEventAt (file mtime) it ignores assistant streaming, tool turns, and
	// the untimed metadata claude writes on pause/kill, so it is the sidebar
	// sort key and auto-archive clock: a session only reorders when the user
	// talks to it. Read from the jsonl, and stamped on usher sends ahead of it.
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

// TranscriptPage is one window of a transcript. Offset is the absolute index
// of Turns[0] (0-based from the start of the session); with Total it lets a
// caller page in either direction and know when it has reached an end.
type TranscriptPage struct {
	Turns  []TranscriptTurn `json:"turns"`
	Offset int              `json:"offset"`
	Total  int              `json:"total"`
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
	Role      string    `json:"role"`
	Time      time.Time `json:"ts"`
	TurnIndex int       `json:"turn_index"`
	// Cursor names the matching turn; a transcript read from it starts there.
	Cursor      string `json:"cursor,omitempty"`
	Occurrences int    `json:"occurrences"`
	Snippet     string `json:"snippet"`
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
	Cursor    string `json:"cursor,omitempty"`
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
	// Router designates the new session as usher's router: it spawns with the
	// session-routing MCP tools and becomes the inbox for relayed replies,
	// replacing any previous designation.
	Router bool
}

// Errors a caller can branch on, whichever layer raised them.
var (
	// ErrSessionNotFound: the operation targets a session with no log on disk.
	ErrSessionNotFound = errors.New("session not found")
	// ErrReadOnlySession: the operation would change a session usher only reads.
	ErrReadOnlySession = errors.New("subagent transcripts are read-only")
	// ErrSendQueueFull: a busy session cannot queue another prompt.
	ErrSendQueueFull = errors.New("send queue full for session")
	ErrInvalidTitle  = errors.New("session title must not be empty")
)

// EventRelayLanded is published on the router session when a session it routed
// work to has replied; its raw names that session.
const EventRelayLanded = "relay.landed"

// RoutedReply is what a routing tool call in a transcript led to: the reply of
// the session the work went to.
type RoutedReply struct {
	// Turn and Part locate the call: its turn's cursor and its index there.
	Turn string `json:"turn"`
	Part int    `json:"part"`
	// SessionID and Title are the session the work went to.
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	// Reply is empty until the session has answered; Done is false while that
	// answer is still being written.
	Reply string    `json:"reply,omitempty"`
	Time  time.Time `json:"ts,omitzero"`
	Done  bool      `json:"done"`
}
