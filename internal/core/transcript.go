package core

import (
	"strings"
	"time"

	"github.com/nexustar/usher/internal/textutil"
)

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
	// Activity carries what the transcript records: goals and schedules.
	Activity Activity
}

// TurnPart is one segment within a grouped assistant turn.
type TurnPart struct {
	Type     string `json:"type"`
	Content  string `json:"content"`
	ToolName string `json:"toolName,omitempty"`
	// ToolTarget is the card title; for show_image, the path clients load.
	ToolTarget string `json:"toolTarget,omitempty"`
	// ToolInput is the full command or JSON arguments, empty when the title
	// already shows it all. Content holds only the output.
	ToolInput string `json:"toolInput,omitempty"`
	// ToolError marks a call its backend reported as failed.
	ToolError bool `json:"toolError,omitempty"`

	// ToolUseID is parser bookkeeping used to join metadata follow-ups to the
	// tool part they enrich. It is never part of the public transcript shape.
	ToolUseID string `json:"-"`
}

// ToolTitle is a shell tool's card title: target (a description) when set,
// else the first line of command.
func ToolTitle(target, command string) string {
	if target == "" {
		return textutil.FirstLine(command)
	}
	return target
}

// NewToolPart builds a tool part from its title, full input and rendered
// output. The input is dropped when the title already shows all of it.
func NewToolPart(name, title, input, content string) TurnPart {
	if strings.TrimSpace(input) == title {
		input = ""
	}
	return TurnPart{
		Type:       "tool",
		Content:    content,
		ToolName:   name,
		ToolTarget: title,
		ToolInput:  textutil.ClampBody(input),
	}
}

// Turn is a grouped, display-ready timeline entry shared by every backend.
type Turn struct {
	Role    string     `json:"role"`
	Content string     `json:"content,omitempty"`
	Parts   []TurnPart `json:"parts,omitempty"`
	Time    time.Time  `json:"ts"`
	Model   string     `json:"model,omitempty"`
	UUID    string     `json:"uuid,omitempty"`
	// Cursor names a turn read back from a log; opaque, stable as the log grows.
	Cursor  string    `json:"cursor,omitempty"`
	EndTime time.Time `json:"-"`
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
