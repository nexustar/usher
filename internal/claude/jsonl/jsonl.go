// Package jsonl parses Claude Code's session log files.
//
// Each session lives at ~/.claude/projects/<sanitized-cwd>/<id>.jsonl, one
// JSON object per line. Lines have heterogeneous shape — type values seen so
// far include: queue-operation, user, assistant, last-prompt, attachment,
// ai-title, file-history-snapshot, permission-mode. We unmarshal common fields
// into Event and keep Raw for downstream typed projections.
package jsonl

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/textutil"
)

// Event is one line of a session jsonl. Common fields are extracted; the full
// raw payload is retained for type-specific decoding by callers.
type Event struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"` // e.g. "turn_duration" on type=system
	SessionID string          `json:"sessionId,omitempty"`
	Timestamp time.Time       `json:"timestamp,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	UUID      string          `json:"uuid,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`

	// ToolUseResult is a line-level sibling of Message that Claude Code attaches
	// to tool_result events. It carries the rich payload — Edit/Write diff
	// (structuredPatch), Read file content, Bash stdout/stderr — that the inline
	// message.content does not, so it is the source for rendering tool turns.
	ToolUseResult json.RawMessage `json:"toolUseResult,omitempty"`

	AITitle string `json:"aiTitle,omitempty"`
	// CustomTitle is Claude's display-only session title.
	CustomTitle *string `json:"customTitle,omitempty"`

	// AttributionAgent is the subagent type ("Explore", "general-purpose", …)
	// Claude Code stamps on the lines of a subagent (sidechain) transcript. It
	// makes a far better row label than the opaque agent-<hash> filename.
	AttributionAgent string `json:"attributionAgent,omitempty"`

	// IsMeta marks harness-injected context (e.g. skill content loaded after
	// a Skill tool call). These are user-role messages but not real user input.
	IsMeta          bool   `json:"isMeta,omitempty"`
	SourceToolUseID string `json:"sourceToolUseID,omitempty"`

	// Origin ("coordinator" — a message pushed into a running subagent) and
	// PromptSource ("system" — a queued prompt) mark meta that carries
	// injected input. Boilerplate meta has neither.
	Origin struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	PromptSource string `json:"promptSource,omitempty"`

	Raw json.RawMessage `json:"-"`
}

// ParseLine decodes one jsonl line into an Event.
func ParseLine(line []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		return ev, err
	}
	ev.Raw = append(json.RawMessage(nil), line...)
	return ev, nil
}

// Compatibility aliases keep parser callers source-compatible while the
// backend-neutral transcript contract lives in core.
type SessionMeta = core.SessionMeta

// MetaScanner folds a session log's lines, fed in file order, into its
// SessionMeta. Cwd, title and the first prompt can each appear anywhere.
type MetaScanner struct {
	meta                                  SessionMeta
	firstUserPrompt, aiTitle, customTitle string
	activity                              activityScan
}

func NewMetaScanner(path string) *MetaScanner {
	return &MetaScanner{meta: SessionMeta{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}}
}

// Feed takes one line; a malformed one is skipped.
func (s *MetaScanner) Feed(line []byte) {
	ev, err := ParseLine(line)
	if err != nil {
		return
	}
	meta := &s.meta
	s.activity.feed(ev)
	if meta.StartedAt.IsZero() && !ev.Timestamp.IsZero() {
		meta.StartedAt = ev.Timestamp
	}
	if !ev.Timestamp.IsZero() {
		meta.LastEventAt = ev.Timestamp
	}
	if meta.Cwd == "" && ev.Cwd != "" {
		meta.Cwd = ev.Cwd
	}
	if ev.Type == "ai-title" && ev.AITitle != "" {
		s.aiTitle = ev.AITitle
	}
	if ev.CustomTitle != nil {
		s.customTitle = *ev.CustomTitle
	}
	if meta.AgentName == "" && ev.AttributionAgent != "" {
		meta.AgentName = ev.AttributionAgent
	}
	if ev.Type == "assistant" && len(ev.Message) > 0 {
		updateClaudeRuntime(&meta.Runtime, ev.Message)
	}
	if ev.Type == "user" && len(ev.Message) > 0 {
		content := extractUserContent(ev.Message)
		if s.firstUserPrompt == "" && !ev.isBoilerplateMeta() {
			s.firstUserPrompt = content
		}
		// A genuine typed prompt — not a tool_result echo or the
		// "[Request interrupted ...]" marker claude writes on Ctrl-C.
		if !ev.Timestamp.IsZero() && !hasToolResult(ev.Message) &&
			!ev.IsMeta &&
			!strings.HasPrefix(content, "[Request interrupted") {
			meta.LastInputAt = ev.Timestamp
		}
	}
}

// Meta is the metadata of the lines fed so far.
func (s *MetaScanner) Meta() SessionMeta {
	meta := s.meta
	if s.firstUserPrompt != "" {
		meta.Prompt = textutil.Truncate(strings.TrimSpace(s.firstUserPrompt), 60)
	}
	if s.customTitle != "" {
		meta.Title = s.customTitle
	} else {
		meta.Title = s.aiTitle
	}
	meta.Activity = s.activity.activity()
	return meta
}

// ReadSessionMeta scans the whole file at path.
func ReadSessionMeta(path string) (SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer f.Close()

	s := NewMetaScanner(path)
	sc := bufio.NewScanner(f)
	// Some events (assistant message with usage stats, large attachments)
	// can exceed bufio's default 64K line limit.
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		s.Feed(sc.Bytes())
	}
	return s.Meta(), sc.Err()
}

// RenameSession directly appends Claude's native custom-title record. The
// Claude SDK supports this for live sessions; unlike /rename, it stays out of
// model context.
func RenameSession(path, sessionID, title string) error {
	record := struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{
		Type:        "custom-title",
		CustomTitle: title,
		SessionID:   sessionID,
	}
	return appendRecord(path, record)
}

func appendRecord(path string, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func updateClaudeRuntime(runtime *core.SessionRuntime, raw json.RawMessage) {
	var msg struct {
		Model string `json:"model"`
		Usage struct {
			Input         int64 `json:"input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			Output        int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	if msg.Model != "" {
		runtime.Model = msg.Model
	}
	cached := msg.Usage.CacheCreation + msg.Usage.CacheRead
	context := msg.Usage.Input + cached + msg.Usage.Output
	if context > 0 {
		runtime.ContextTokens = context
	}
}

// body is a decoded message: its content is a plain string or blocks.
type body struct {
	Model  string
	text   string
	blocks []bodyBlock
}

type bodyBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

func parseBody(msg json.RawMessage) body {
	var m struct {
		Model   string      `json:"model"`
		Content []bodyBlock `json:"content"`
	}
	err := json.Unmarshal(msg, &m)
	if err == nil {
		return body{Model: m.Model, blocks: m.Content}
	}
	// A type error fails only its own field: blocks that still decoded held
	// it, and none at all means content is a string.
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		return body{}
	}
	if len(m.Content) > 0 {
		return body{Model: m.Model, blocks: m.Content}
	}
	var t struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(msg, &t) != nil {
		return body{Model: m.Model}
	}
	return body{Model: m.Model, text: t.Content}
}

// joinedText is the plain string, or the text blocks joined.
func (b body) joinedText() string {
	if b.text != "" {
		return b.text
	}
	var parts []string
	for _, blk := range b.blocks {
		if blk.Type == "text" && blk.Text != "" {
			parts = append(parts, blk.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (b body) hasToolResult() bool {
	for _, blk := range b.blocks {
		if blk.Type == "tool_result" {
			return true
		}
	}
	return false
}

// collectToolUses records tool_use id→name+target from an assistant message.
func (b body) collectToolUses(dst map[string]toolInfo) {
	for _, blk := range b.blocks {
		if blk.Type == "tool_use" && blk.ID != "" && blk.Name != "" {
			ti := toolInfo{name: blk.Name, target: toolTarget(blk.Input)}
			switch {
			case blk.Name == "Bash":
				ti.input = inputString(blk.Input, "command")
				ti.target = core.ToolTitle(ti.target, ti.input)
			case strings.HasPrefix(blk.Name, "mcp__"):
				ti.input = textutil.IndentJSON(blk.Input)
			}
			dst[blk.ID] = ti
		}
	}
}

// matchToolInfo looks up the tool name+target for the first tool_result block.
// It also returns the tool_use_id for isMeta follow-up matching, and whether
// Claude flagged the result as an error.
func (b body) matchToolInfo(names map[string]toolInfo) (ti toolInfo, id string, failed bool) {
	for _, blk := range b.blocks {
		if blk.Type == "tool_result" && blk.ToolUseID != "" {
			return names[blk.ToolUseID], blk.ToolUseID, blk.IsError
		}
	}
	return toolInfo{}, "", false
}

func (b body) firstToolResultContent() json.RawMessage {
	for _, blk := range b.blocks {
		if blk.Type == "tool_result" {
			return blk.Content
		}
	}
	return nil
}

// extractUserContent pulls a representative text from a user message body. The
// body's content can be either a plain string or an array of content blocks.
func extractUserContent(msg json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// ---------- Turn assembly (grouped, display-ready projection) ----------

type toolInfo struct {
	name   string
	target string // card title
	input  string // Bash: the full command; MCP: the indented JSON arguments
}

// IsTurnComplete reports Claude Code's persisted end-of-turn marker.
func IsTurnComplete(raw []byte) bool {
	var o struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	return json.Unmarshal(raw, &o) == nil && o.Type == "system" && o.Subtype == "turn_duration"
}

// Assembler is the single grouping engine behind both transcript reads and
// the live event stream: feed it user/assistant events in file order and it
// yields the turns and parts a transcript read serves, so a part streamed live
// and the same turn fetched later from /transcript can never disagree on
// grouping or rendering.
type Assembler struct {
	toolMap map[string]toolInfo
	cur     *core.Turn
}

func NewAssembler() *Assembler {
	return &Assembler{toolMap: map[string]toolInfo{}}
}

// Feed consumes one session event. completed holds turns this event finished
// (a real user prompt first flushes the in-progress assistant turn, then
// commits itself as a user turn). part is set when the event appended a part
// to the in-progress assistant turn — the per-event increment a live stream
// publishes (it is a copy; later Feeds don't mutate it). Events that are not
// user/assistant lines are ignored.
func (a *Assembler) Feed(ev Event) (completed []core.Turn, part *core.TurnPart) {
	if ev.Type == "system" && ev.Subtype == "compact_boundary" {
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		return append(completed, core.Turn{
			Role:    "system",
			Content: "Context compacted",
			Time:    ev.Timestamp,
		}), nil
	}
	if ev.Type != "user" && ev.Type != "assistant" {
		return nil, nil
	}

	// Boilerplate meta: not a prompt, and it must not flush the assistant turn
	// it interrupts.
	if ev.Type == "user" && ev.isBoilerplateMeta() {
		return nil, nil
	}

	b := parseBody(ev.Message)
	if ev.Type == "user" && !b.hasToolResult() && !(ev.IsMeta && ev.SourceToolUseID != "") {
		// Real user prompt — flush any in-progress assistant turn.
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		if text := b.joinedText(); text != "" {
			completed = append(completed, core.Turn{
				Role:    "user",
				Content: compactTaskNotification(text),
				Time:    ev.Timestamp,
			})
		}
		return completed, nil
	}

	// isMeta user message with sourceToolUseID (e.g. skill content after a
	// Skill tool call): append text to the matching tool part.
	if ev.IsMeta && ev.SourceToolUseID != "" && ev.Type == "user" {
		text := b.joinedText()
		if text == "" {
			return nil, nil
		}
		if a.cur == nil {
			a.cur = &core.Turn{Role: "assistant", Time: ev.Timestamp}
		}
		if ev.UUID != "" {
			a.cur.UUID = ev.UUID
		}
		a.cur.Touch(ev.Timestamp)
		if ev.SourceToolUseID != "" {
			for i := len(a.cur.Parts) - 1; i >= 0; i-- {
				if a.cur.Parts[i].Type == "tool" && a.cur.Parts[i].ToolUseID == ev.SourceToolUseID {
					a.cur.Parts[i].Content += "\n" + text
					return nil, &a.cur.Parts[i]
				}
			}
		}
		ti := a.toolMap[ev.SourceToolUseID]
		p := core.TurnPart{
			Type:       "tool",
			Content:    text,
			ToolName:   ti.name,
			ToolTarget: ti.target,
			ToolUseID:  ev.SourceToolUseID,
		}
		a.cur.Parts = append(a.cur.Parts, p)
		return nil, &p
	}

	// Start a new assistant turn if needed (tool_result lines carry no model).
	if a.cur == nil {
		a.cur = &core.Turn{
			Role:  "assistant",
			Time:  ev.Timestamp,
			Model: b.Model,
		}
	} else if b.Model != "" && a.cur.Model == "" {
		a.cur.Model = b.Model
	}
	// Track the turn's last event — its fork point and end time — even when
	// the event contributes no visible part.
	if ev.UUID != "" {
		a.cur.UUID = ev.UUID
	}
	a.cur.Touch(ev.Timestamp)

	if ev.Type == "assistant" {
		// Collect tool_use id→info for later matching.
		b.collectToolUses(a.toolMap)
		// Append a text part (skip tool_use/thinking-only messages).
		if text := b.joinedText(); text != "" {
			p := core.TurnPart{Type: "text", Content: text}
			a.cur.Parts = append(a.cur.Parts, p)
			return nil, &p
		}
		return nil, nil
	}

	// user event carrying a tool_result: append as a "tool" part.
	ti, tuID, failed := b.matchToolInfo(a.toolMap)
	content, kind := renderToolResult(ev, b, ti.target)
	// A known tool that printed nothing (mkdir, git add) still gets its card.
	if content == "" && ti.name == "" {
		return nil, nil
	}
	p := core.NewToolPart(ti.name, ti.target, ti.input, content)
	p.ContentKind = kind
	p.ToolUseID, p.ToolError = tuID, failed
	a.cur.Parts = append(a.cur.Parts, p)
	return nil, &p
}

// FeedLine parses one raw jsonl line and feeds it, the uniform entry point
// shared with other backends' assemblers (a malformed line is ignored).
func (a *Assembler) FeedLine(raw []byte) (completed []core.Turn, part *core.TurnPart) {
	ev, err := ParseLine(raw)
	if err != nil {
		return nil, nil
	}
	return a.Feed(ev)
}

// Model returns the model id of the in-progress assistant turn ("" if none).
func (a *Assembler) Model() string {
	if a.cur == nil {
		return ""
	}
	return a.cur.Model
}

// Flush commits and returns the in-progress assistant turn, or nil when there
// is none (or it gathered no parts). Call at end-of-input; a real user prompt
// flushes implicitly via Feed.
func (a *Assembler) Flush() *core.Turn {
	t := a.cur
	a.cur = nil
	if t == nil || len(t.Parts) == 0 {
		return nil
	}
	return t
}

// compactTaskNotification rewrites Claude Code's self-injected
// <task-notification> prompt (background task completion — one summary line
// plus a machine payload that dwarfs it) to the short form the TUI itself
// displays. Any other text passes through unchanged. This is a
// display/transcript transform only; the raw line stays on disk untouched.
func compactTaskNotification(text string) string {
	if !strings.HasPrefix(strings.TrimSpace(text), "<task-notification>") {
		return text
	}
	if s := xmlTagContent(text, "summary"); s != "" {
		return "[task notification] " + s
	}
	if s := xmlTagContent(text, "status"); s != "" {
		return "[task notification] background task " + s
	}
	return "[task notification]"
}

// xmlTagContent returns the trimmed text between the first <tag>…</tag> pair,
// or "" — a narrow scan for Claude Code's notification markup, not an XML
// parser.
func xmlTagContent(s, tag string) string {
	open, end := "<"+tag+">", "</"+tag+">"
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(s[i+len(open) : i+j])
}

func hasToolResult(msg json.RawMessage) bool { return parseBody(msg).hasToolResult() }

// renderToolResult produces the output of a tool_result ("tool") turn and its
// content kind. It is built from the line-level toolUseResult, which carries
// the rich payload (Edit/Write diff, Read file content, Bash stdout/stderr)
// that the inline message.content does not; tools whose shape we do not
// special-case fall back to the inline tool_result text.
func renderToolResult(ev Event, b body, target string) (content, kind string) {
	var tur toolUseResultData
	if len(ev.ToolUseResult) > 0 {
		_ = json.Unmarshal(ev.ToolUseResult, &tur)
	}
	if body, kind := tur.render(); body != "" {
		return body, kind
	}
	inline := b.firstToolResultContent()
	// An image comes back as bytes only — no text to fall back on.
	if tur.Type == "image" || hasImageBlock(inline) {
		if target == "" {
			return "[image]", ""
		}
		return "", core.ContentImage
	}
	return flattenToolResult(inline), ""
}

// isBoilerplateMeta reports whether the event is meta Claude Code injects for
// its own bookkeeping: the scale notice after an image, the caveat before a
// local command's output, the nudge after a turn that printed nothing. Meta
// naming a tool, an origin, or a prompt source carries content and is kept.
func (ev Event) isBoilerplateMeta() bool {
	return ev.IsMeta && ev.SourceToolUseID == "" &&
		ev.Origin.Kind == "" && ev.PromptSource == ""
}

func hasImageBlock(raw json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "image" {
			return true
		}
	}
	return false
}

func flattenToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// patchHunk is one hunk of a Claude Code structuredPatch. Its Lines already
// carry the unified-diff prefix (' ', '+', '-'), so they drop straight into a
// diff fence.
type patchHunk struct {
	OldStart int      `json:"oldStart"`
	OldLines int      `json:"oldLines"`
	NewStart int      `json:"newStart"`
	NewLines int      `json:"newLines"`
	Lines    []string `json:"lines"`
}

// toolUseResultData decodes the shapes of toolUseResult we render richly. Edit
// and Write carry structuredPatch; Read carries File; Bash carries Stdout/
// Stderr. Unknown shapes leave every field zero and render() returns "".
type toolUseResultData struct {
	Type            string      `json:"type"`
	StructuredPatch []patchHunk `json:"structuredPatch"`
	File            *struct {
		Content string `json:"content"`
	} `json:"file"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// render turns the structured payload into output text and its content kind,
// or "" when the shape is not one we special-case.
func (t toolUseResultData) render() (body, kind string) {
	switch {
	case len(t.StructuredPatch) > 0:
		return patchBody(t.StructuredPatch), core.ContentDiff
	case t.File != nil && t.File.Content != "":
		return t.File.Content, ""
	case t.Stdout != "" || t.Stderr != "":
		out := t.Stdout
		if t.Stderr != "" {
			if out != "" {
				out += "\n"
			}
			out += t.Stderr
		}
		return out, ""
	}
	return "", ""
}

// patchBody renders structuredPatch hunks as unified-diff text.
func patchBody(hunks []patchHunk) string {
	var b strings.Builder
	for i, h := range hunks {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		for _, ln := range h.Lines {
			b.WriteByte('\n')
			b.WriteString(ln)
		}
	}
	return b.String()
}

// toolTarget picks the most informative non-command argument to show beside a
// tool name: a file path, a human description, or a search pattern. A Bash
// command travels separately as the part's ToolInput.
func toolTarget(input json.RawMessage) string {
	if p := inputString(input, "file_path"); p != "" {
		return p
	}
	if d := inputString(input, "description"); d != "" {
		return d
	}
	if pat := inputString(input, "pattern"); pat != "" {
		return pat
	}
	return ""
}

// inputString reads a string field from a tool_use input object, "" if absent.
func inputString(input json.RawMessage, key string) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m[key], &s); err != nil {
		return ""
	}
	return s
}
