package core

import "time"

// SessionMeta is the backend-neutral descriptor discovery needs to list a
// persisted agent session without loading its full transcript.
type SessionMeta struct {
	ID          string
	ParentID    string
	IsSubagent  bool
	AgentName   string
	Cwd         string
	Title       string
	Prompt      string
	StartedAt   time.Time
	LastEventAt time.Time
	LastInputAt time.Time
	Runtime     SessionRuntime
}

// TurnPart is one segment within a grouped assistant turn.
type TurnPart struct {
	Type       string `json:"type"`
	Content    string `json:"content"`
	ToolName   string `json:"toolName,omitempty"`
	ToolTarget string `json:"toolTarget,omitempty"`

	// ToolUseID is parser bookkeeping used to join metadata follow-ups to the
	// tool part they enrich. It is never part of the public transcript shape.
	ToolUseID string `json:"-"`
}

// Turn is a grouped, display-ready timeline entry shared by every backend.
type Turn struct {
	Role    string     `json:"role"`
	Content string     `json:"content,omitempty"`
	Parts   []TurnPart `json:"parts,omitempty"`
	Time    time.Time  `json:"ts"`
	Model   string     `json:"model,omitempty"`
	UUID    string     `json:"uuid,omitempty"`
	EndTime time.Time  `json:"-"`
}

// DisplayTime is the timestamp a client shows for the turn: an assistant turn
// reads as when it finished, every other role as its single event.
func (t Turn) DisplayTime() time.Time {
	if t.Role == "assistant" && !t.EndTime.IsZero() {
		return t.EndTime
	}
	return t.Time
}

// Touch advances the server-side end timestamp when ts is usable.
func (t *Turn) Touch(ts time.Time) {
	if t != nil && !ts.IsZero() {
		t.EndTime = ts
	}
}
