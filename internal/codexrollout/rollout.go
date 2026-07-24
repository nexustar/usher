// Package codexrollout parses OpenAI Codex CLI "rollout" session logs into the
// the backend-neutral core display model consumed by router, web, and agents.
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
// Shared display and metadata types live in package core; this package owns
// only the Codex wire format and its projection into that contract.
package codexrollout

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
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

// envelope is the minimal rollout record shape for the line predicates below —
// unlike `line` it skips the timestamp, so a malformed timestamp can't fail an
// unrelated check.
type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// IsTurnComplete reports whether a rollout line is the end-of-turn marker. The
// sender/tail layer uses it the same way it uses Claude's system/turn_duration:
// the signal that the model has truly finished, not merely emitted a message.
func IsTurnComplete(raw []byte) bool {
	var l envelope
	if err := json.Unmarshal(raw, &l); err != nil || l.Type != "event_msg" {
		return false
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return false
	}
	// task_complete is the v1 wire name; turn_complete its announced v2 rename.
	return isTurnCompleteType(p.Type)
}

func isTurnCompleteType(t string) bool {
	return t == "task_complete" || t == "turn_complete"
}

// IsTurnAborted reports an explicit abort marker: the turn is over but no
// completion marker will follow. Error events are deliberately NOT matched —
// codex may retry and continue after one, and cutting a live turn short is
// worse than the wait it would save.
func IsTurnAborted(raw []byte) bool {
	var l envelope
	if err := json.Unmarshal(raw, &l); err != nil || l.Type != "event_msg" {
		return false
	}
	var p struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return false
	}
	switch p.Type {
	case "task_aborted", "turn_aborted", "task_cancelled", "turn_cancelled":
		return true
	default:
		return false
	}
}

// IsTurnActivity reports whether a rollout line is model output — proof a real
// turn is in flight. The records codex logs at submit time (turn_context, the
// user's own message) don't count: they appear whether or not the model runs.
func IsTurnActivity(raw []byte) bool {
	var l envelope
	if err := json.Unmarshal(raw, &l); err != nil {
		return false
	}
	switch l.Type {
	case "event_msg":
		var p struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			return false
		}
		// task_started (v1 wire name; turn_started its announced v2 rename) is
		// persisted at submit time, so the latch arms as soon as a turn begins.
		return p.Type == "task_started" || p.Type == "turn_started" ||
			strings.HasPrefix(p.Type, "agent_")
	case "response_item":
		var p struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(l.Payload, &p); err != nil {
			return false
		}
		switch p.Type {
		case "message":
			return p.Role == "assistant"
		case "reasoning", "function_call", "function_call_output", "local_shell_call", "web_search_call", "custom_tool_call":
			return true
		}
	}
	return false
}

// ReadSessionMeta reads the lightweight descriptor: id/cwd/start from the
// session_meta header, last-activity from the final timestamped line, and a
// title from the first real user prompt.
func ReadSessionMeta(path string) (core.SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return core.SessionMeta{}, err
	}
	defer f.Close()

	meta := core.SessionMeta{ID: SessionIDFromPath(path)}
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
				ID             string `json:"id"`
				Cwd            string `json:"cwd"`
				ParentThreadID string `json:"parent_thread_id"`
				ThreadSource   string `json:"thread_source"`
				AgentNickname  string `json:"agent_nickname"`
				AgentPath      string `json:"agent_path"`
			}
			if err := json.Unmarshal(l.Payload, &p); err == nil {
				if p.ID != "" {
					meta.ID = p.ID
				}
				meta.Cwd = p.Cwd
				meta.IsSubagent = p.ThreadSource == "subagent"
				if meta.IsSubagent {
					meta.ParentID = p.ParentThreadID
				}
				meta.AgentName = p.AgentNickname
				if meta.AgentName == "" {
					meta.AgentName = p.AgentPath
				}
			}
		case "event_msg":
			var usage struct {
				Type string `json:"type"`
				Info *struct {
					Last struct {
						Total int64 `json:"total_tokens"`
					} `json:"last_token_usage"`
					ContextWindow int64 `json:"model_context_window"`
				} `json:"info"`
			}
			if json.Unmarshal(l.Payload, &usage) == nil && usage.Type == "token_count" && usage.Info != nil {
				meta.Runtime.ContextTokens = usage.Info.Last.Total
				meta.Runtime.ContextWindow = usage.Info.ContextWindow
			}
			if msg, ok := userMessage(l.Payload); ok {
				if firstPrompt == "" {
					firstPrompt = msg
				}
				// user_message is codex's clean typed prompt — the sort key
				// (core.SessionMeta.LastInputAt).
				if !l.Timestamp.IsZero() {
					meta.LastInputAt = l.Timestamp
				}
			}
		case "turn_context":
			var p struct {
				Model  string `json:"model"`
				Effort string `json:"effort"`
			}
			if json.Unmarshal(l.Payload, &p) == nil {
				if p.Model != "" {
					meta.Runtime.Model = p.Model
				}
				if p.Effort != "" {
					meta.Runtime.Effort = p.Effort
				}
			}
		}
	}
	if firstPrompt != "" {
		meta.Prompt = truncate(firstPrompt, 60)
	}
	return meta, sc.Err()
}

// ReadThreadName reads the latest name from Codex's append-only index.
func ReadThreadName(indexPath, id string) (string, error) {
	names, err := ReadThreadNames(indexPath)
	return names[id], err
}

// ReadThreadNames reads the latest name for every indexed thread.
func ReadThreadNames(indexPath string) (map[string]string, error) {
	f, err := os.Open(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := newScanner(f)
	names := make(map[string]string)
	for sc.Scan() {
		var record struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if json.Unmarshal(sc.Bytes(), &record) == nil && record.ID != "" {
			names[record.ID] = record.ThreadName
		}
	}
	return names, sc.Err()
}

// RenameSession appends a Codex thread-name record.
func RenameSession(indexPath, id, title string) error {
	record := struct {
		ID         string `json:"id"`
		ThreadName string `json:"thread_name"`
		UpdatedAt  string `json:"updated_at"`
	}{
		ID:         id,
		ThreadName: title,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(indexPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// ReadTurns returns the grouped user/assistant turns of the rollout at path,
// matching jsonl.ReadTurns' contract (limit>0 keeps the most recent N; total is
// the count before trimming).
func ReadTurns(path string, limit int) (turns []core.Turn, total int, err error) {
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
	cur     *core.Turn
	pending map[string]toolStash // call_id -> tool call awaiting its output
	seenMCP map[string]struct{}  // canonical/legacy/response-item deduplication
	model   string               // last model seen on a turn_context line (sticky)
}

type toolStash struct {
	name   string
	target string
	skip   bool
	mcp    bool
}

func NewAssembler() *Assembler {
	return &Assembler{pending: map[string]toolStash{}, seenMCP: map[string]struct{}{}}
}

// Feed consumes one rollout line. completed holds turns this line finished (a
// real user prompt flushes the in-progress assistant turn, then commits itself);
// part is set when the line appended a part to the in-progress assistant turn
// (the per-event increment a live stream publishes — a copy, not mutated later).
func (a *Assembler) Feed(raw []byte) (completed []core.Turn, part *core.TurnPart) {
	// Parse the timestamp independently. A new or malformed timestamp format
	// must not make us discard the event type/payload (especially turn_complete).
	var wire struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, nil
	}
	l := line{Type: wire.Type, Payload: wire.Payload}
	if len(wire.Timestamp) != 0 {
		_ = json.Unmarshal(wire.Timestamp, &l.Timestamp)
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

func (a *Assembler) feedEvent(l line) (completed []core.Turn, part *core.TurnPart) {
	var p struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		TurnID  string `json:"turn_id"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return nil, nil
	}
	switch p.Type {
	case "context_compacted":
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		return append(completed, core.Turn{
			Role:    "system",
			Content: "Context compacted",
			Time:    l.Timestamp,
		}), nil
	case "user_message":
		// Real user prompt — flush any in-progress assistant turn, then commit.
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		if p.Message != "" {
			completed = append(completed, core.Turn{
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
		tp := core.TurnPart{Type: "text", Content: p.Message}
		a.cur.Parts = append(a.cur.Parts, tp)
		return nil, &tp
	case "mcp_tool_call_end":
		// app-server records MCP calls as lifecycle events rather than the
		// function_call/function_call_output pair emitted by the old TUI path.
		// Normalize both wires into the same tool TurnPart consumed by web/IM.
		return nil, a.mcpToolPart(l)
	case "item_completed":
		return nil, a.completedMCPToolPart(l)
	case "patch_apply_end":
		return nil, a.patchApplyPart(l)
	case "exec_command_end":
		return nil, a.execCommandPart(l)
	case "web_search_end":
		return nil, a.simpleEventToolPart(l, "WebSearch")
	case "image_generation_end":
		return nil, a.imageGenerationPart(l)
	case "view_image_tool_call":
		return nil, a.simpleEventToolPart(l, "ViewImage")
	case "dynamic_tool_call_response":
		return nil, a.dynamicToolPart(l)
	case "task_complete", "turn_complete": // kept explicit for the switch; predicate is shared elsewhere
		// End-of-turn marker: stamp the turn with its turn_id (the fork point a
		// client passes back to ForkCopy) and flush the assistant turn it closes.
		if a.cur != nil {
			a.cur.UUID = p.TurnID
			a.cur.Touch(l.Timestamp)
		}
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		return completed, nil
	}
	return nil, nil
}

func (a *Assembler) appendTool(ts time.Time, name, target, body string) *core.TurnPart {
	a.ensureTurn(ts)
	tp := core.TurnPart{Type: "tool", ToolName: name, ToolTarget: target}
	if body != "" {
		tp.Content = fence(clampBody(body))
	}
	a.cur.Parts = append(a.cur.Parts, tp)
	return &tp
}

func (a *Assembler) patchApplyPart(l line) *core.TurnPart {
	var p struct {
		Stdout  string `json:"stdout"`
		Stderr  string `json:"stderr"`
		Success bool   `json:"success"`
		Status  string `json:"status"`
		Changes map[string]struct {
			UnifiedDiff string `json:"unified_diff"`
		} `json:"changes"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	paths := make([]string, 0, len(p.Changes))
	var body []string
	for path, change := range p.Changes {
		paths = append(paths, path)
		if change.UnifiedDiff != "" {
			body = append(body, change.UnifiedDiff)
		}
	}
	sort.Strings(paths)
	if p.Stdout != "" {
		body = append(body, p.Stdout)
	}
	if p.Stderr != "" {
		body = append(body, p.Stderr)
	}
	if len(body) == 0 && (!p.Success || p.Status != "") {
		body = append(body, p.Status)
	}
	return a.appendTool(l.Timestamp, "Edit", strings.Join(paths, ", "), strings.Join(body, "\n"))
}

func (a *Assembler) execCommandPart(l line) *core.TurnPart {
	var p struct {
		Command          []string `json:"command"`
		AggregatedOutput string   `json:"aggregated_output"`
		Stdout           string   `json:"stdout"`
		Stderr           string   `json:"stderr"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	body := p.AggregatedOutput
	if body == "" {
		body = strings.TrimSpace(p.Stdout + "\n" + p.Stderr)
	}
	return a.appendTool(l.Timestamp, "Shell", strings.Join(p.Command, " "), body)
}

func (a *Assembler) simpleEventToolPart(l line, name string) *core.TurnPart {
	var p struct {
		Query  string          `json:"query"`
		Path   string          `json:"path"`
		Action json.RawMessage `json:"action"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	target := p.Query
	if target == "" {
		target = p.Path
	}
	body := ""
	if len(p.Action) > 0 && string(p.Action) != "null" {
		body = string(p.Action)
	}
	return a.appendTool(l.Timestamp, name, target, body)
}

func (a *Assembler) imageGenerationPart(l line) *core.TurnPart {
	var p struct {
		Status        string `json:"status"`
		RevisedPrompt string `json:"revised_prompt"`
		SavedPath     string `json:"saved_path"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	body := p.Status
	if p.RevisedPrompt != "" {
		body = strings.TrimSpace(body + "\n" + p.RevisedPrompt)
	}
	return a.appendTool(l.Timestamp, "ImageGeneration", p.SavedPath, body)
}

func (a *Assembler) dynamicToolPart(l line) *core.TurnPart {
	var p struct {
		Namespace    string                     `json:"namespace"`
		Tool         string                     `json:"tool"`
		Arguments    map[string]json.RawMessage `json:"arguments"`
		ContentItems []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content_items"`
		Error string `json:"error"`
	}
	if json.Unmarshal(l.Payload, &p) != nil || p.Tool == "" {
		return nil
	}
	name := p.Tool
	if p.Namespace != "" {
		name = p.Namespace + "__" + name
	}
	var body []string
	for _, item := range p.ContentItems {
		if item.Text != "" {
			body = append(body, item.Text)
		}
	}
	if p.Error != "" {
		body = append(body, p.Error)
	}
	return a.appendTool(l.Timestamp, name, toolTargetMap(p.Arguments), strings.Join(body, "\n"))
}

func (a *Assembler) mcpToolPart(l line) *core.TurnPart {
	var p struct {
		CallID     string `json:"call_id"`
		Invocation struct {
			Server    string                     `json:"server"`
			Tool      string                     `json:"tool"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"invocation"`
		Result struct {
			OK *struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"Ok"`
		} `json:"result"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil || p.Invocation.Tool == "" {
		return nil
	}
	if !a.markMCP(p.CallID) {
		return nil
	}
	name := p.Invocation.Tool
	if p.Invocation.Server != "" {
		name = "mcp__" + p.Invocation.Server + "__" + name
	}
	target := toolTargetMap(p.Invocation.Arguments)
	var texts []string
	if p.Result.OK != nil {
		for _, c := range p.Result.OK.Content {
			if c.Type == "text" && c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
	}
	a.ensureTurn(l.Timestamp)
	tp := core.TurnPart{Type: "tool", ToolName: name, ToolTarget: target}
	if len(texts) > 0 {
		tp.Content = fence(clampBody(strings.Join(texts, "\n")))
	}
	a.cur.Parts = append(a.cur.Parts, tp)
	return &tp
}

func (a *Assembler) completedMCPToolPart(l line) *core.TurnPart {
	var p struct {
		Item struct {
			Type      string                     `json:"type"`
			ID        string                     `json:"id"`
			Server    string                     `json:"server"`
			Tool      string                     `json:"tool"`
			Arguments map[string]json.RawMessage `json:"arguments"`
			Result    *struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"item"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil || (p.Item.Type != "mcp_tool_call" && p.Item.Type != "McpToolCall") || p.Item.Tool == "" {
		return nil
	}
	if !a.markMCP(p.Item.ID) {
		return nil
	}
	var texts []string
	if p.Item.Result != nil {
		for _, c := range p.Item.Result.Content {
			if c.Type == "text" && c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
	}
	if p.Item.Error != nil && p.Item.Error.Message != "" {
		texts = append(texts, p.Item.Error.Message)
	}
	return a.appendMCPPart(l.Timestamp, p.Item.Server, p.Item.Tool, p.Item.Arguments, texts)
}

func (a *Assembler) markMCP(callID string) bool {
	if callID == "" {
		return true
	}
	if _, ok := a.seenMCP[callID]; ok {
		return false
	}
	a.seenMCP[callID] = struct{}{}
	return true
}

func (a *Assembler) appendMCPPart(ts time.Time, server, tool string, arguments map[string]json.RawMessage, texts []string) *core.TurnPart {
	name := tool
	if server != "" {
		name = "mcp__" + server + "__" + tool
	}
	a.ensureTurn(ts)
	tp := core.TurnPart{Type: "tool", ToolName: name, ToolTarget: toolTargetMap(arguments)}
	if len(texts) > 0 {
		tp.Content = fence(clampBody(strings.Join(texts, "\n")))
	}
	a.cur.Parts = append(a.cur.Parts, tp)
	return &tp
}

// feedResponseItem handles the model-item stream. Only tool calls/outputs are
// taken from here; message text is sourced from the cleaner event_msg stream.
func (a *Assembler) feedResponseItem(l line) (part *core.TurnPart) {
	var p struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Namespace string          `json:"namespace"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
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
		name := prettyToolName(p.Name)
		isMCP := strings.HasPrefix(p.Namespace, "mcp__")
		if p.Namespace != "" {
			name = p.Namespace + "__" + p.Name
		}
		a.pending[p.CallID] = toolStash{
			name:   name,
			target: toolTarget(p.Arguments),
			mcp:    isMCP,
		}
		return nil
	case "function_call_output":
		stash := a.pending[p.CallID]
		delete(a.pending, p.CallID)
		if stash.mcp && !a.markMCP(p.CallID) {
			return nil
		}
		a.ensureTurn(l.Timestamp)
		tp := core.TurnPart{
			Type:       "tool",
			Content:    renderOutput(p.Output),
			ToolName:   stash.name,
			ToolTarget: stash.target,
		}
		a.cur.Parts = append(a.cur.Parts, tp)
		return &tp
	case "custom_tool_call":
		target := customExecTarget(p.Input)
		a.pending[p.CallID] = toolStash{
			name: prettyToolName(p.Name), target: target,
			skip: customCallHasCanonicalEvent(p.Name, p.Input),
		}
		return nil
	case "custom_tool_call_output":
		stash, ok := a.pending[p.CallID]
		delete(a.pending, p.CallID)
		if !ok || stash.skip {
			return nil
		}
		return a.appendTool(l.Timestamp, stash.name, stash.target, renderOutputBody(p.Output))
	}
	return nil
}

// FeedLine is Feed under the name the cross-backend assembler interface expects
// (jsonl.Assembler exposes the same method), so the router can drive either.
func (a *Assembler) FeedLine(raw []byte) (completed []core.Turn, part *core.TurnPart) {
	return a.Feed(raw)
}

// Model returns the most recent model seen on a turn_context line, or "" before
// the first one (the session_meta header carries only a provider, not the model).
func (a *Assembler) Model() string { return a.model }

func (a *Assembler) ensureTurn(ts time.Time) {
	if a.cur == nil {
		a.cur = &core.Turn{Role: "assistant", Time: ts, Model: a.model}
	}
	a.cur.Touch(ts)
}

// Flush commits and returns the in-progress assistant turn, or nil when there is
// none (or it gathered no parts). Call at end-of-input; a user prompt or
// task_complete flushes implicitly via Feed.
func (a *Assembler) Flush() *core.Turn {
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
	return toolTargetMap(m)
}

func toolTargetMap(m map[string]json.RawMessage) string {
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

func renderOutputBody(output json.RawMessage) string {
	if len(output) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(output, &s) == nil {
		return s
	}
	var items []struct{ Type, Text string }
	if json.Unmarshal(output, &items) == nil {
		var texts []string
		for _, item := range items {
			if item.Text != "" {
				texts = append(texts, item.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	return string(output)
}

var customCmdRe = regexp.MustCompile(`(?s)\b(?:cmd|command)\s*:\s*("(?:\\.|[^"\\])*")`)

func customExecTarget(input string) string {
	m := customCmdRe.FindStringSubmatch(input)
	if len(m) != 2 {
		return ""
	}
	var command string
	if json.Unmarshal([]byte(m[1]), &command) != nil {
		return ""
	}
	return firstLine(command)
}

func customCallHasCanonicalEvent(name, input string) bool {
	if name != "exec" {
		return false
	}
	for _, marker := range []string{"tools.apply_patch", "tools.mcp__", "tools.image_gen__", "tools.web__"} {
		if strings.Contains(input, marker) {
			return true
		}
	}
	return false
}

// prettyToolName maps Codex's internal tool names to friendlier labels; unknown
// names pass through unchanged (low-maintenance, honest about new tools).
func prettyToolName(name string) string {
	switch name {
	case "exec", "exec_command", "shell", "local_shell":
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
