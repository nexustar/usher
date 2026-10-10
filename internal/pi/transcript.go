package pi

import (
	"encoding/json"
	"math"
	"os"
	"strings"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/textutil"
	"github.com/nexustar/usher/internal/window"
)

type Transcript struct{}

var sessionLog = window.Reader{
	NewAssembler: func() backend.Assembler { return NewAssembler() },
	Lines:        branchLines,
}

func (Transcript) ReadBefore(path, before string, limit int) ([]core.Turn, bool, error) {
	return sessionLog.Before(path, before, limit)
}
func (Transcript) ReadFrom(path, from string) ([]core.Turn, error) {
	return sessionLog.From(path, from)
}
func (Transcript) NewAssembler() backend.Assembler { return NewAssembler() }

// IsTurnComplete reports pi's end-of-turn marker. The agent loop keeps running
// through assistant records whose stopReason is "toolUse", so only a final stop
// ends the turn — plus a "length" cutoff that carries no tool call: pi fails
// truncated tool calls and keeps looping (agent-loop terminate:false), so a
// length record with tool calls is still mid-turn and more records follow.
func (Transcript) IsTurnComplete(raw []byte) bool {
	var e entry
	if json.Unmarshal(raw, &e) != nil || e.Type != "message" {
		return false
	}
	var m message
	if json.Unmarshal(e.Message, &m) != nil || m.Role != "assistant" {
		return false
	}
	switch m.StopReason {
	case "stop":
		return true
	case "length":
		return !hasToolCall(m.Content)
	default:
		return false
	}
}

// hasToolCall reports whether an assistant record carries a tool call. Content
// that does not parse counts as one: an unreadable record must not be mistaken
// for the end of a turn.
func hasToolCall(content json.RawMessage) bool {
	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return true
	}
	for _, b := range blocks {
		if b.Type == "toolCall" {
			return true
		}
	}
	return false
}

// branchLines yields the entries of one branch in [from, end): a pi log is a
// tree whose parents precede their children. The branch is the one the entry
// at end hangs off, or with last the one the span's last entry is on — at the
// end of the file, the active leaf.
func branchLines(f *os.File, from, end int64, last bool, yield func(off int64, line []byte) bool) error {
	type node struct {
		off    int64
		line   []byte
		parent string
	}
	ids := func(line []byte) (id, parent string) {
		var e entry
		if json.Unmarshal(line, &e) != nil || e.ParentID == nil {
			return e.ID, ""
		}
		return e.ID, *e.ParentID
	}
	byID := map[string]node{}
	leaf := ""
	err := window.FileLines(f, from, end, false, func(off int64, line []byte) bool {
		if id, parent := ids(line); id != "" {
			byID[id] = node{off, append([]byte(nil), line...), parent}
			leaf = id
		}
		return true
	})
	if err != nil {
		return err
	}
	if !last {
		err := window.FileLines(f, end, math.MaxInt64, false, func(_ int64, line []byte) bool {
			_, leaf = ids(line)
			return false
		})
		if err != nil {
			return err
		}
	}
	var branch []node
	for {
		n, ok := byID[leaf]
		if !ok {
			break
		}
		delete(byID, leaf) // a parent cycle must not loop
		branch = append(branch, n)
		leaf = n.parent
	}
	for i := len(branch) - 1; i >= 0; i-- {
		if !yield(branch[i].off, branch[i].line) {
			break
		}
	}
	return nil
}

type Assembler struct {
	cur   *core.Turn
	model string
}

func NewAssembler() *Assembler     { return &Assembler{} }
func (a *Assembler) Model() string { return a.model }

func (a *Assembler) FeedLine(raw []byte) ([]core.Turn, *core.TurnPart) {
	completed, parts := a.FeedLineParts(raw)
	if len(parts) == 0 {
		return completed, nil
	}
	return completed, parts[len(parts)-1]
}

// FeedLineParts exposes every block Pi stores together in one assistant record.
func (a *Assembler) FeedLineParts(raw []byte) ([]core.Turn, []*core.TurnPart) {
	var e entry
	if json.Unmarshal(raw, &e) != nil {
		return nil, nil
	}
	if e.Type == "compaction" {
		var completed []core.Turn
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		return append(completed, core.Turn{
			Role:    "system",
			Content: "Context compacted",
			Time:    e.Timestamp,
			UUID:    e.ID,
		}), nil
	}
	// Extension-injected content the model sees. display false keeps it out of
	// the user's view but not out of the context; the TUI's registered renderer
	// cannot cross the RPC boundary, so usher shows the stored content.
	if e.Type == "custom_message" {
		text := contentText(e.Content)
		if !e.Display || text == "" {
			return nil, nil
		}
		var completed []core.Turn
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		return append(completed, core.Turn{
			Role:    "system",
			Content: text,
			Time:    e.Timestamp,
			UUID:    e.ID,
		}), nil
	}
	if e.Type != "message" {
		return nil, nil
	}
	var m message
	if json.Unmarshal(e.Message, &m) != nil {
		return nil, nil
	}
	ts := entryTime(e, m)
	switch m.Role {
	case "user":
		var done []core.Turn
		if t := a.Flush(); t != nil {
			done = append(done, *t)
		}
		text := contentText(m.Content)
		if text != "" {
			done = append(done, core.Turn{Role: "user", Content: text, Time: ts, UUID: e.ID, EndTime: ts})
		}
		return done, nil
	case "assistant":
		if a.cur != nil { /* tool-loop assistant messages belong to one turn */
		} else {
			a.cur = &core.Turn{Role: "assistant", Time: ts, UUID: e.ID}
		}
		if m.Model != "" {
			a.model, a.cur.Model = m.Model, m.Model
		}
		var blocks []block
		if json.Unmarshal(m.Content, &blocks) != nil {
			return nil, nil
		}
		var parts []*core.TurnPart
		for _, b := range blocks {
			p := core.TurnPart{}
			switch b.Type {
			case "text":
				p.Type, p.Content = "text", b.Text
			// thinking is dropped: pi's provider is the only one that returns
			// reasoning in the clear — claude and codex persist it encrypted.
			case "toolCall":
				target, input := toolTarget(b.Name, b.Arguments), ""
				switch {
				case strings.EqualFold(b.Name, "bash"):
					// bash's target is its command: the first line titles the card.
					target, input = textutil.FirstLine(target), target
				case b.Name == "codemode":
					input = codemodeScript(b.Arguments)
					target = codemodeTitle(input)
				case strings.HasPrefix(b.Name, "mcp__"):
					input = textutil.IndentJSON(b.Arguments)
				}
				p = core.NewToolPart(b.Name, target, input, "")
				p.ToolUseID = b.ID
			default:
				continue
			}
			if p.Content == "" && p.ToolName == "" {
				continue
			}
			a.cur.Parts = append(a.cur.Parts, p)
			cp := p
			parts = append(parts, &cp)
		}
		// Failed and interrupted responses are persisted as an assistant record
		// with stopReason "error"/"aborted", often with no content at all. Emit
		// one error turn per record: otherwise the turn ends silently, or — when
		// the record does carry the text streamed before an interrupt — passes
		// for a finished answer.
		if (m.StopReason == "error" || m.StopReason == "aborted") && m.ErrorMessage != "" {
			var done []core.Turn
			if t := a.Flush(); t != nil {
				done = append(done, *t)
			}
			done = append(done, core.Turn{Role: "error", Content: m.ErrorMessage, Time: ts, UUID: e.ID, EndTime: ts})
			return done, parts
		}
		a.cur.Touch(ts)
		return nil, parts
	case "toolResult":
		if a.cur == nil {
			a.cur = &core.Turn{Role: "assistant", Time: ts}
		}
		content := contentText(m.Content)
		for i := len(a.cur.Parts) - 1; i >= 0; i-- {
			if a.cur.Parts[i].ToolUseID != m.ToolCallID {
				continue
			}
			if a.cur.Parts[i].ToolName == "" {
				a.cur.Parts[i].ToolName = m.ToolName
			}
			a.cur.Parts[i].Content = textutil.ClampBody(content)
			a.cur.Parts[i].ToolError = m.IsError
			p := a.cur.Parts[i]
			a.cur.Touch(ts)
			return nil, []*core.TurnPart{&p}
		}
		// Preserve orphaned results as a tool part rather than leaking raw tool
		// output into the assistant prose stream.
		p := core.TurnPart{Type: "tool", Content: textutil.ClampBody(content), ToolName: m.ToolName, ToolUseID: m.ToolCallID, ToolError: m.IsError}
		a.cur.Parts = append(a.cur.Parts, p)
		a.cur.Touch(ts)
		return nil, []*core.TurnPart{&p}
	}
	return nil, nil
}

func toolTarget(name string, args json.RawMessage) string {
	var v map[string]any
	if json.Unmarshal(args, &v) != nil {
		return ""
	}
	for _, key := range []string{"command", "path", "file_path", "query", "pattern"} {
		if s, ok := v[key].(string); ok {
			return s
		}
	}
	return ""
}

func codemodeScript(args json.RawMessage) string {
	var v struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(args, &v)
	return v.Code
}

// codemodeTitle is the script's first line of code: blank lines and comments,
// including the leading "// @options:" directive, say nothing about the call.
func codemodeTitle(script string) string {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "//") {
			return line
		}
	}
	return ""
}

func (a *Assembler) Flush() *core.Turn {
	if a.cur == nil {
		return nil
	}
	t := a.cur
	a.cur = nil
	// Avoid empty assistant shells produced by extension-only messages.
	if len(t.Parts) == 0 && strings.TrimSpace(t.Content) == "" {
		return nil
	}
	return t
}
