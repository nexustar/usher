package pi

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

type Transcript struct{}

func (Transcript) ReadTurns(path string, limit int) ([]core.Turn, int, error) {
	entries, err := activeEntries(path)
	if err != nil {
		return nil, 0, err
	}
	a := NewAssembler()
	var turns []core.Turn
	for _, raw := range entries {
		completed, _ := a.FeedLine(raw)
		turns = append(turns, completed...)
	}
	if t := a.Flush(); t != nil {
		turns = append(turns, *t)
	}
	total := len(turns)
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, total, nil
}
func (Transcript) NewAssembler() backend.Assembler { return NewAssembler() }
func (Transcript) IsTurnComplete([]byte) bool      { return false }
func (Transcript) IsTurnAborted([]byte) bool       { return false }

// activeEntries selects the branch ending at the last entry. Pi appends the
// current branch, so the final entry is the active leaf in persisted sessions.
func activeEntries(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byID := map[string][]byte{}
	parent := map[string]string{}
	last := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.ID == "" {
			continue
		}
		byID[e.ID] = append([]byte(nil), sc.Bytes()...)
		if e.ParentID != nil {
			parent[e.ID] = *e.ParentID
		}
		last = e.ID
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var rev [][]byte
	seen := map[string]bool{}
	for last != "" && !seen[last] {
		seen[last] = true
		if raw := byID[last]; raw != nil {
			rev = append(rev, raw)
		}
		last = parent[last]
	}
	out := make([][]byte, len(rev))
	for i := range rev {
		out[len(rev)-1-i] = rev[i]
	}
	return out, nil
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
		if a.cur != nil {
			done = append(done, *a.cur)
			a.cur = nil
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
			case "thinking":
				p.Type, p.Content = "thinking", b.Thinking
			case "toolCall":
				p.Type, p.ToolName, p.ToolUseID = "tool", b.Name, b.ID
				p.ToolTarget = toolTarget(b.Name, b.Arguments)
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
			a.cur.Parts[i].Content = renderToolResult(a.cur.Parts[i].ToolName, content)
			p := a.cur.Parts[i]
			a.cur.Touch(ts)
			return nil, []*core.TurnPart{&p}
		}
		// Preserve orphaned results as a tool part rather than leaking raw tool
		// output into the assistant prose stream.
		p := core.TurnPart{Type: "tool", Content: renderToolResult(m.ToolName, content), ToolName: m.ToolName, ToolUseID: m.ToolCallID}
		a.cur.Parts = append(a.cur.Parts, p)
		a.cur.Touch(ts)
		return nil, []*core.TurnPart{&p}
	}
	return nil, nil
}

// renderToolResult fences terminal-style output before shared Markdown rendering.
func renderToolResult(name, body string) string {
	if body == "" || !terminalOutputTool(name) {
		return body
	}
	return fence(clampBody(body))
}

func terminalOutputTool(name string) bool {
	switch strings.ToLower(name) {
	case "read", "bash", "grep", "find", "ls":
		return true
	default:
		return false
	}
}

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
