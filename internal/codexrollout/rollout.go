// Package codexrollout parses OpenAI Codex CLI "rollout" session logs into the
// same display model usher already uses for Claude Code sessions
// (jsonl.SessionMeta / jsonl.Turn), so the router, web, and agent layers can
// consume both backends through one contract.
//
// A rollout lives at ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl,
// one JSON object per line: {"timestamp","type","payload":{...}}. The first
// line is a session_meta carrying the session id, cwd, and start time. The
// conversation is reconstructed from the UI event stream (event_msg
// user_message / agent_message — the clean text the user saw) interleaved with
// the model-item stream's tool calls (response_item function_call /
// function_call_output, linked by call_id). A turn is finished by an event_msg
// task_complete, the analog of Claude Code's system/turn_duration marker.
//
// The shared display types (jsonl.Turn, jsonl.TurnPart, jsonl.SessionMeta) live
// in package jsonl today for historical reasons; they are backend-neutral and a
// later refactor may move them to package core. codexrollout imports them so the
// rendered transcript shape is byte-for-byte the contract router/web expect.
package codexrollout

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"usher/internal/jsonl"
)

// line is the uniform envelope of every rollout record.
type line struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// uuidRe matches a v4/v7-shaped UUID; the session id is the UUID embedded at the
// end of the rollout filename (the leading timestamp also contains hyphens, so a
// plain split is ambiguous — anchor on the UUID pattern instead).
var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// SessionIDFromPath extracts the session UUID from a rollout filename, or "" if
// the name carries none (so discovery can cheaply key off the path).
func SessionIDFromPath(path string) string {
	return uuidRe.FindString(filepath.Base(path))
}

// IsTurnComplete reports whether a rollout line is the end-of-turn marker. The
// sender/tail layer uses it the same way it uses Claude's system/turn_duration:
// the signal that the model has truly finished, not merely emitted a message.
func IsTurnComplete(raw []byte) bool {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil || l.Type != "event_msg" {
		return false
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return false
	}
	return p.Type == "task_complete"
}

// ReadSessionMeta reads the lightweight descriptor: id/cwd/start from the
// session_meta header, last-activity from the final timestamped line, and a
// title from the first real user prompt.
func ReadSessionMeta(path string) (jsonl.SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return jsonl.SessionMeta{}, err
	}
	defer f.Close()

	meta := jsonl.SessionMeta{ID: SessionIDFromPath(path)}
	sc := newScanner(f)
	var firstPrompt string
	for sc.Scan() {
		var l line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		if meta.StartedAt.IsZero() && !l.Timestamp.IsZero() {
			meta.StartedAt = l.Timestamp
		}
		if !l.Timestamp.IsZero() {
			meta.LastEventAt = l.Timestamp
		}
		switch l.Type {
		case "session_meta":
			var p struct {
				ID  string `json:"id"`
				Cwd string `json:"cwd"`
			}
			if err := json.Unmarshal(l.Payload, &p); err == nil {
				if p.ID != "" {
					meta.ID = p.ID
				}
				meta.Cwd = p.Cwd
			}
		case "event_msg":
			if firstPrompt == "" {
				if msg, ok := userMessage(l.Payload); ok {
					firstPrompt = msg
				}
			}
		}
	}
	if firstPrompt != "" {
		meta.Title = truncate(firstPrompt, 60)
	}
	return meta, sc.Err()
}

// ReadTurns returns the grouped user/assistant turns of the rollout at path,
// matching jsonl.ReadTurns' contract (limit>0 keeps the most recent N; total is
// the count before trimming).
func ReadTurns(path string, limit int) (turns []jsonl.Turn, total int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	asm := NewAssembler()
	sc := newScanner(f)
	for sc.Scan() {
		completed, _ := asm.Feed(sc.Bytes())
		turns = append(turns, completed...)
	}
	if t := asm.Flush(); t != nil {
		turns = append(turns, *t)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	total = len(turns)
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, total, nil
}

// Assembler groups rollout lines into turns, mirroring jsonl.Assembler so a part
// streamed live and the same turn re-read from /transcript never disagree. Feed
// it raw lines in file order.
type Assembler struct {
	cur     *jsonl.Turn
	pending map[string]toolStash // call_id -> tool call awaiting its output
	model   string               // last model seen on a turn_context line (sticky)
}

type toolStash struct {
	name   string
	target string
}

func NewAssembler() *Assembler {
	return &Assembler{pending: map[string]toolStash{}}
}

// Feed consumes one rollout line. completed holds turns this line finished (a
// real user prompt flushes the in-progress assistant turn, then commits itself);
// part is set when the line appended a part to the in-progress assistant turn
// (the per-event increment a live stream publishes — a copy, not mutated later).
func (a *Assembler) Feed(raw []byte) (completed []jsonl.Turn, part *jsonl.TurnPart) {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, nil
	}
	switch l.Type {
	case "event_msg":
		return a.feedEvent(l)
	case "response_item":
		return nil, a.feedResponseItem(l)
	case "turn_context":
		a.feedTurnContext(l)
	}
	return nil, nil
}

// feedTurnContext captures the per-turn model Codex records on each turn_context
// line (the session_meta header carries only a provider). It's sticky: the model
// holds for subsequent turns until a later turn_context changes it.
func (a *Assembler) feedTurnContext(l line) {
	var p struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil || p.Model == "" {
		return
	}
	a.model = p.Model
	if a.cur != nil && a.cur.Model == "" {
		a.cur.Model = p.Model
	}
}

func (a *Assembler) feedEvent(l line) (completed []jsonl.Turn, part *jsonl.TurnPart) {
	var p struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		TurnID  string `json:"turn_id"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return nil, nil
	}
	switch p.Type {
	case "user_message":
		// Real user prompt — flush any in-progress assistant turn, then commit.
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		if p.Message != "" {
			completed = append(completed, jsonl.Turn{
				Role:    "user",
				Content: p.Message,
				Time:    l.Timestamp,
			})
		}
		return completed, nil
	case "agent_message":
		if p.Message == "" {
			return nil, nil
		}
		a.ensureTurn(l.Timestamp)
		tp := jsonl.TurnPart{Type: "text", Content: p.Message}
		a.cur.Parts = append(a.cur.Parts, tp)
		return nil, &tp
	case "task_complete":
		// End-of-turn marker: stamp the turn with its turn_id (the fork point a
		// client passes back to ForkCopy) and flush the assistant turn it closes.
		if a.cur != nil {
			a.cur.UUID = p.TurnID
		}
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		return completed, nil
	}
	return nil, nil
}

// feedResponseItem handles the model-item stream. Only tool calls/outputs are
// taken from here; message text is sourced from the cleaner event_msg stream.
func (a *Assembler) feedResponseItem(l line) (part *jsonl.TurnPart) {
	var p struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		CallID    string          `json:"call_id"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return nil
	}
	switch p.Type {
	case "function_call":
		// Stash until the output arrives; emit one combined tool part then, so a
		// tool turn carries name + target + result like Claude's.
		a.pending[p.CallID] = toolStash{
			name:   prettyToolName(p.Name),
			target: toolTarget(p.Arguments),
		}
		return nil
	case "function_call_output":
		stash := a.pending[p.CallID]
		delete(a.pending, p.CallID)
		a.ensureTurn(l.Timestamp)
		tp := jsonl.TurnPart{
			Type:       "tool",
			Content:    renderOutput(p.Output),
			ToolName:   stash.name,
			ToolTarget: stash.target,
		}
		a.cur.Parts = append(a.cur.Parts, tp)
		return &tp
	}
	return nil
}

// FeedLine is Feed under the name the cross-backend assembler interface expects
// (jsonl.Assembler exposes the same method), so the router can drive either.
func (a *Assembler) FeedLine(raw []byte) (completed []jsonl.Turn, part *jsonl.TurnPart) {
	return a.Feed(raw)
}

// Model returns the most recent model seen on a turn_context line, or "" before
// the first one (the session_meta header carries only a provider, not the model).
func (a *Assembler) Model() string { return a.model }

func (a *Assembler) ensureTurn(ts time.Time) {
	if a.cur == nil {
		a.cur = &jsonl.Turn{Role: "assistant", Time: ts, Model: a.model}
	}
}

// Flush commits and returns the in-progress assistant turn, or nil when there is
// none (or it gathered no parts). Call at end-of-input; a user prompt or
// task_complete flushes implicitly via Feed.
func (a *Assembler) Flush() *jsonl.Turn {
	t := a.cur
	a.cur = nil
	if t == nil || len(t.Parts) == 0 {
		return nil
	}
	return t
}

// --- payload helpers ---

// userMessage returns the text of an event_msg user_message payload.
func userMessage(payload json.RawMessage) (string, bool) {
	var p struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.Type != "user_message" {
		return "", false
	}
	return p.Message, p.Message != ""
}

// toolTarget pulls the most informative argument out of a function_call's
// arguments (itself a JSON-encoded string): a shell command, else a file path.
func toolTarget(arguments string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return ""
	}
	for _, key := range []string{"cmd", "command", "file_path", "path"} {
		if raw, ok := m[key]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil && s != "" {
				return firstLine(s)
			}
		}
	}
	return ""
}

// renderOutput renders a function_call_output's output (a JSON string for the
// shapes seen so far) into a fenced block, clamped.
func renderOutput(output json.RawMessage) string {
	if len(output) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(output, &s); err == nil {
		return fence(clampBody(s))
	}
	return fence(clampBody(string(output)))
}

// prettyToolName maps Codex's internal tool names to friendlier labels; unknown
// names pass through unchanged (low-maintenance, honest about new tools).
func prettyToolName(name string) string {
	switch name {
	case "exec_command", "shell", "local_shell":
		return "Shell"
	case "apply_patch":
		return "Edit"
	case "":
		return ""
	default:
		return name
	}
}

func newScanner(f *os.File) *bufio.Scanner {
	sc := bufio.NewScanner(f)
	// session_meta (base_instructions) and large tool outputs blow past 64K.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return sc
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// fence wraps body in a markdown code fence widened past any backtick run inside
// body, so a payload containing ``` cannot close the block early.
func fence(body string) string {
	longest, run := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	ticks := strings.Repeat("`", max(3, longest+1))
	return ticks + "\n" + body + "\n" + ticks
}

// clampBody caps a tool body so one huge output cannot bloat the transcript.
func clampBody(s string) string {
	const maxBytes = 32 * 1024
	const maxLines = 400
	if len(s) > maxBytes {
		s = s[:maxBytes] + "\n… (truncated)"
	}
	if lines := strings.Split(s, "\n"); len(lines) > maxLines {
		s = strings.Join(append(lines[:maxLines], "… (truncated)"), "\n")
	}
	return s
}
