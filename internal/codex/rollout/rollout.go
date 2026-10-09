// Package rollout parses OpenAI Codex CLI "rollout" session logs into the
// the backend-neutral core display model consumed by router, web, and agents.
//
// A rollout lives at ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl,
// one JSON object per line: {"timestamp","type","payload":{...}}. The first
// line is a session_meta carrying the session id, cwd, and start time. A turn
// is finished by an event_msg task_complete, the analog of Claude Code's
// system/turn_duration marker.
//
// Codex has two persistence modes (session_meta.history_mode). Legacy rollouts
// carry the clean conversation in the event_msg UI stream (user_message /
// agent_message) with tools as per-event markers (patch_apply_end,
// web_search_end, …) and shells as response_item custom_tool_call. Paginated
// rollouts (cli 0.153+, now the default) instead deliver everything as
// event_msg item_completed TurnItems (UserMessage / AgentMessage /
// CommandExecution / FileChange / …) and no longer persist the legacy per-tool
// events; there the batched `exec` custom_tool_call is a duplicate of the
// per-op items and is dropped (see Assembler.toolItems). A command left running
// in the background has no item until it exits; its card is built from the
// wrapper's output meanwhile (see backgroundOutput). Both modes are
// reconstructed into the same core turns.
//
// Shared display and metadata types live in package core; this package owns
// only the Codex wire format and its projection into that contract.
package rollout

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/textutil"
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
	return isTurnAbortedType(p.Type)
}

func isTurnAbortedType(t string) bool {
	switch t {
	case "task_aborted", "turn_aborted", "task_cancelled", "turn_cancelled":
		return true
	default:
		return false
	}
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
			if msg, ok := cleanUserPrompt(l.Payload); ok {
				if firstPrompt == "" {
					firstPrompt = msg
				}
				// The clean typed prompt — the sort key
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
		meta.Prompt = textutil.Truncate(strings.TrimSpace(firstPrompt), 60)
	}
	return meta, sc.Err()
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

// Assembler groups rollout lines into turns, mirroring jsonl.Assembler so a part
// streamed live and the same turn re-read from /transcript never disagree. Feed
// it raw lines in file order.
type Assembler struct {
	cur     *core.Turn
	pending map[string]toolStash // call_id -> tool call awaiting its output
	seenMCP map[string]struct{}  // canonical/legacy/response-item deduplication
	model   string               // last model seen on a turn_context line (sticky)
	// toolItems counts per-op tool items rendered from the paginated
	// item_completed stream (CommandExecution/FileChange/…, but not MCP). A
	// custom_tool_call snapshots it; if it grew before the call's output, the
	// items already rendered the same ops and the batched `exec` wrapper is
	// dropped as their duplicate. A wrapper that produced no item (e.g. a pure
	// text() script) still renders, and legacy sessions never increment it.
	toolItems  int
	background map[string]*bgCommand // unified-exec session id -> command still running
	missed     bool                  // see MissedContext
}

type toolStash struct {
	name        string
	target      string // card title
	input       string // shell: the full command; MCP: the indented JSON arguments
	script      string // a batched `exec` wrapper's source
	skip        bool
	mcp         bool
	itemsAtCall int // toolItems when the call was seen (paginated dedup)
}

func NewAssembler() *Assembler {
	return &Assembler{
		pending:    map[string]toolStash{},
		seenMCP:    map[string]struct{}{},
		background: map[string]*bgCommand{},
	}
}

// Feed consumes one rollout line. completed holds turns this line finished (a
// real user prompt flushes the in-progress assistant turn, then commits itself);
// part is set when the line appended a part to the in-progress assistant turn
// (the per-event increment a live stream publishes — a copy, not mutated later).
// For a background command it is the increment alone: the turn keeps one card
// per command, which later output and the final item update in place.
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
		Reason  string `json:"reason"`
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
		return a.feedItemCompleted(l)
	case "patch_apply_end":
		return nil, a.patchApplyPart(l)
	case "exec_command_end":
		return nil, a.commandPart(l)
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
	default:
		if !isTurnAbortedType(p.Type) {
			return nil, nil
		}
		// No completion marker follows an abort, so the assistant turn it cut
		// short is flushed here. Without the marker an interrupted turn is
		// indistinguishable from a finished one: codex persists whatever text
		// streamed before the interrupt and nothing else.
		//
		// reason distinguishes a deliberate interrupt from budget_limited or
		// review_ended, which the user did not ask for. It is carried through
		// verbatim rather than reworded — the older task_* spellings predate
		// the field and leave it empty.
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		message := backend.AbortedTurnMessage
		if p.Reason != "" {
			message += " (" + p.Reason + ")"
		}
		return append(completed, core.Turn{
			Role:    "error",
			Content: message,
			Time:    l.Timestamp,
		}), nil
	}
}

// appendTool appends a tool part: title and input as given, body fenced as the
// tool's output. The returned part is the turn's own, so a caller may still
// flag it.
func (a *Assembler) appendTool(ts time.Time, name, title, input, body string) *core.TurnPart {
	a.ensureTurn(ts)
	a.cur.Parts = append(a.cur.Parts, core.NewToolPart(name, title, input, fenceBody(body)))
	return &a.cur.Parts[len(a.cur.Parts)-1]
}

func fenceBody(body string) string {
	if body == "" {
		return ""
	}
	return textutil.Fence("", textutil.ClampBody(body))
}

// appendArgsTool appends an MCP/dynamic tool part, whose input is its arguments.
func (a *Assembler) appendArgsTool(ts time.Time, name string, arguments map[string]json.RawMessage, texts []string, failed bool) *core.TurnPart {
	part := a.appendTool(ts, name, toolTargetMap(arguments), textutil.IndentJSON(arguments), strings.Join(texts, "\n"))
	part.ToolError = failed
	return part
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
	for path := range p.Changes {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var body []string
	for _, path := range paths {
		if diff := p.Changes[path].UnifiedDiff; diff != "" {
			body = append(body, diff)
		}
	}
	if p.Stdout != "" {
		body = append(body, p.Stdout)
	}
	if p.Stderr != "" {
		body = append(body, p.Stderr)
	}
	if len(body) == 0 && (!p.Success || p.Status != "") {
		body = append(body, p.Status)
	}
	part := a.appendTool(l.Timestamp, "Edit", strings.Join(paths, ", "), "", strings.Join(body, "\n"))
	// The legacy event carries success, the paginated item a status.
	part.ToolError = p.Status == "failed" || p.Status == "declined" || (p.Status == "" && !p.Success)
	return part
}

// commandPart renders a finished command. One that ran in the background
// already has a card in this turn: the finished command, which carries the
// complete output, takes that card over instead of adding a second.
func (a *Assembler) commandPart(l line) *core.TurnPart {
	var p struct {
		Command          []string `json:"command"`
		ProcessID        string   `json:"process_id"`
		ExitCode         *int     `json:"exit_code"`
		Status           string   `json:"status"`
		AggregatedOutput string   `json:"aggregated_output"`
		FormattedOutput  string   `json:"formatted_output"`
		Stdout           string   `json:"stdout"`
		Stderr           string   `json:"stderr"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	command := shellScript(p.Command)
	body := p.AggregatedOutput
	if body == "" {
		body = p.FormattedOutput
	}
	if body == "" {
		body = strings.TrimSpace(p.Stdout + "\n" + p.Stderr)
	}
	bg := a.background[p.ProcessID]
	if bg != nil {
		bg.done = true
	}
	var part *core.TurnPart
	if bg != nil && bg.turn != nil && bg.turn == a.cur {
		a.ensureTurn(l.Timestamp)
		a.cur.Parts[bg.idx] = core.NewToolPart("Shell", core.ToolTitle("", command), command, fenceBody(body))
		part = &a.cur.Parts[bg.idx]
	} else {
		part = a.appendTool(l.Timestamp, "Shell", core.ToolTitle("", command), command, body)
	}
	part.ToolError = p.Status == "failed" || p.Status == "declined" || (p.ExitCode != nil && *p.ExitCode != 0)
	return part
}

// shellScript unwraps codex's `bash -lc <script>` argv. The item's parsed_cmd
// is no substitute: it is a lossy summary that drops operators and filters.
func shellScript(argv []string) string {
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		switch filepath.Base(argv[0]) {
		case "bash", "zsh", "sh":
			return argv[2]
		}
	}
	return strings.Join(argv, " ")
}

func (a *Assembler) simpleEventToolPart(l line, name string) *core.TurnPart {
	var p struct {
		Query   string          `json:"query"`
		Path    string          `json:"path"`
		Action  json.RawMessage `json:"action"`
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"results"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	target := firstNonEmpty(p.Query, p.Path)
	if target == "" && len(p.Results) > 0 {
		// A follow-up on an open page carries no query: name it by its first
		// hit. The results themselves are huge and stay out.
		target = textutil.FirstLine(firstNonEmpty(p.Results[0].Title, p.Results[0].URL))
	}
	body := ""
	if len(p.Action) > 0 && string(p.Action) != "null" {
		body = string(p.Action)
	}
	return a.appendTool(l.Timestamp, name, target, "", body)
}

func (a *Assembler) imageGenerationPart(l line) *core.TurnPart {
	// The legacy event and core item use snake_case; the host-extension variant
	// (kind image_gen.generation) uses camelCase. result holds the base64 image
	// and is deliberately never read.
	var p struct {
		Status             string `json:"status"`
		RevisedPrompt      string `json:"revised_prompt"`
		RevisedPromptCamel string `json:"revisedPrompt"`
		SavedPath          string `json:"saved_path"`
		SavedPathCamel     string `json:"savedPath"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	revised := firstNonEmpty(p.RevisedPrompt, p.RevisedPromptCamel)
	body := p.Status
	if revised != "" {
		body = strings.TrimSpace(body + "\n" + revised)
	}
	return a.appendTool(l.Timestamp, "ImageGeneration", firstNonEmpty(p.SavedPath, p.SavedPathCamel), "", body)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	return a.appendArgsTool(l.Timestamp, name, p.Arguments, body, p.Error != "")
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
				IsError bool `json:"isError"`
			} `json:"Ok"`
			Err json.RawMessage `json:"Err"`
		} `json:"result"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil || p.Invocation.Tool == "" {
		return nil
	}
	if !a.markMCP(p.CallID) {
		return nil
	}
	var texts []string
	if p.Result.OK != nil {
		for _, c := range p.Result.OK.Content {
			if c.Type == "text" && c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
	}
	failed := len(p.Result.Err) > 0 || (p.Result.OK != nil && p.Result.OK.IsError)
	return a.appendArgsTool(l.Timestamp, mcpToolName(p.Invocation.Server, p.Invocation.Tool), p.Invocation.Arguments, texts, failed)
}

// feedItemCompleted projects a paginated item_completed TurnItem into turns.
// A tool item carries the same field names as its legacy per-tool event, so
// exposing the item as the line payload (il) lets the legacy handlers parse it
// unchanged.
func (a *Assembler) feedItemCompleted(l line) (completed []core.Turn, part *core.TurnPart) {
	var p struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return nil, nil
	}
	var it struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(p.Item, &it); err != nil {
		return nil, nil
	}
	il := line{Timestamp: l.Timestamp, Payload: p.Item}
	switch it.Type {
	case "UserMessage":
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		if msg := itemMessageText(p.Item); msg != "" {
			completed = append(completed, core.Turn{Role: "user", Content: msg, Time: l.Timestamp})
		}
		return completed, nil
	case "AgentMessage":
		msg := itemMessageText(p.Item)
		if msg == "" {
			return nil, nil
		}
		a.ensureTurn(l.Timestamp)
		tp := core.TurnPart{Type: "text", Content: msg}
		a.cur.Parts = append(a.cur.Parts, tp)
		return nil, &tp
	case "CommandExecution":
		return nil, a.toolItem(a.commandPart(il))
	case "FileChange":
		return nil, a.toolItem(a.patchApplyPart(il))
	case "WebSearch":
		return nil, a.toolItem(a.simpleEventToolPart(il, "WebSearch"))
	case "ImageGeneration":
		return nil, a.toolItem(a.imageGenerationPart(il))
	case "ImageView":
		return nil, a.toolItem(a.simpleEventToolPart(il, "ViewImage"))
	case "DynamicToolCall":
		return nil, a.toolItem(a.dynamicToolPart(il))
	case "Extension":
		return nil, a.toolItem(a.extensionItem(il))
	case "ContextCompaction":
		if t := a.Flush(); t != nil {
			completed = append(completed, *t)
		}
		return append(completed, core.Turn{Role: "system", Content: "Context compacted", Time: l.Timestamp}), nil
	default:
		// McpToolCall (and unknown items). completedMCPToolPart reads the item
		// off the original line and dedups MCP itself, so it never feeds the
		// custom_tool_call counter (an MCP call inside an exec wrapper is already
		// skipped by customCallHasCanonicalEvent).
		return nil, a.completedMCPToolPart(l)
	}
}

// toolItem counts a rendered per-op tool item so a later custom_tool_call that
// batched the same ops is dropped as a duplicate (see the custom_tool_call_output
// case). MCP items are excluded — they never need to gate the wrapper.
func (a *Assembler) toolItem(part *core.TurnPart) *core.TurnPart {
	if part != nil {
		a.toolItems++
	}
	return part
}

// extensionItem handles the "Extension" item, keyed by a dotted kind. Real
// paginated sessions deliver hosted web search and image generation this way,
// not as the core WebSearch / ImageGeneration item types.
func (a *Assembler) extensionItem(l line) *core.TurnPart {
	var p struct {
		Kind string `json:"kind"`
	}
	if json.Unmarshal(l.Payload, &p) != nil {
		return nil
	}
	switch p.Kind {
	case "web.search":
		return a.simpleEventToolPart(l, "WebSearch")
	case "image_gen.generation":
		return a.imageGenerationPart(l)
	default:
		// clock.sleep and other host extensions have no useful transcript form.
		return nil
	}
}

func (a *Assembler) completedMCPToolPart(l line) *core.TurnPart {
	var p struct {
		Item struct {
			Type      string                     `json:"type"`
			ID        string                     `json:"id"`
			Server    string                     `json:"server"`
			Tool      string                     `json:"tool"`
			Arguments map[string]json.RawMessage `json:"arguments"`
			Status    string                     `json:"status"`
			Result    *struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
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
	failed := p.Item.Status == "failed" || p.Item.Error != nil || (p.Item.Result != nil && p.Item.Result.IsError)
	return a.appendArgsTool(l.Timestamp, mcpToolName(p.Item.Server, p.Item.Tool), p.Item.Arguments, texts, failed)
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

func mcpToolName(server, tool string) string {
	if server == "" {
		return tool
	}
	return "mcp__" + server + "__" + tool
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
		stash := toolStash{name: name, mcp: isMCP}
		args := parseArgs(p.Arguments)
		if isMCP {
			stash.target, stash.input = toolTargetMap(args), textutil.IndentJSON(args)
		} else {
			target, command := splitToolTarget(args)
			stash.target, stash.input = core.ToolTitle(target, command), command
		}
		a.pending[p.CallID] = stash
		return nil
	case "custom_tool_call":
		command := customExecCommand(p.Input)
		a.pending[p.CallID] = toolStash{
			name:        prettyToolName(p.Name),
			target:      core.ToolTitle("", command),
			input:       command,
			script:      p.Input,
			skip:        customCallHasCanonicalEvent(p.Name, p.Input),
			itemsAtCall: a.toolItems,
		}
		return nil
	case "function_call_output":
		stash := a.pending[p.CallID]
		delete(a.pending, p.CallID)
		if stash.mcp && !a.markMCP(p.CallID) {
			return nil
		}
		return a.appendTool(l.Timestamp, stash.name, stash.target, stash.input, renderOutputBody(p.Output))
	case "custom_tool_call_output":
		stash, ok := a.pending[p.CallID]
		delete(a.pending, p.CallID)
		if !ok {
			return nil
		}
		part, oneShot, handled := a.backgroundOutput(l.Timestamp, stash, p.Output)
		// Drop the batched exec wrapper when its ops already rendered as items
		// (see Assembler.toolItems).
		if stash.skip || a.toolItems > stash.itemsAtCall {
			return part
		}
		if !handled {
			return a.appendTool(l.Timestamp, stash.name, stash.target, stash.input, renderOutputBody(p.Output))
		}
		if oneShot != "" {
			return a.appendTool(l.Timestamp, stash.name, stash.target, stash.input, oneShot)
		}
		return part
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

// MissedContext covers the sticky model and background commands already
// running.
func (a *Assembler) MissedContext() bool {
	missed := a.missed
	a.missed = false
	return missed
}

func (a *Assembler) ensureTurn(ts time.Time) {
	if a.cur == nil {
		a.cur = &core.Turn{Role: "assistant", Time: ts, Model: a.model}
		a.missed = a.missed || a.model == ""
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

// cleanUserPrompt returns the user's typed prompt from an event_msg payload,
// handling both the older user_message event and the newer item_completed
// UserMessage item (cli 0.153+). Used by ReadSessionMeta for the title and the
// LastInputAt sort key.
func cleanUserPrompt(payload json.RawMessage) (string, bool) {
	var p struct {
		Type string          `json:"type"`
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", false
	}
	if p.Type == "user_message" {
		return userMessage(payload)
	}
	if p.Type == "item_completed" {
		var it struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(p.Item, &it) == nil && it.Type == "UserMessage" {
			msg := itemMessageText(p.Item)
			return msg, msg != ""
		}
	}
	return "", false
}

// itemMessageText joins the text of a message item's content array. The
// content-item type tag varies in case across roles (user "text", agent
// "Text"), but the text lives in a lowercase "text" field in both.
func itemMessageText(item json.RawMessage) string {
	var p struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(item, &p); err != nil {
		return ""
	}
	var texts []string
	for _, c := range p.Content {
		if c.Text != "" {
			texts = append(texts, c.Text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

// parseArgs decodes a function_call's JSON-encoded arguments string; nil when
// empty or malformed.
func parseArgs(arguments string) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if arguments == "" || json.Unmarshal([]byte(arguments), &m) != nil {
		return nil
	}
	return m
}

// argString returns the first of keys that m holds as a non-empty string.
func argString(m map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var s string
		if raw, ok := m[key]; ok && json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// splitToolTarget separates a function_call's shell command from its display
// target (file path).
func splitToolTarget(m map[string]json.RawMessage) (target, command string) {
	if cmd := argString(m, "cmd", "command"); cmd != "" {
		return "", cmd
	}
	return argString(m, "file_path", "path"), ""
}

// toolTargetMap picks the display target of an MCP/dynamic tool, whose command
// keys are plain values rather than shell commands.
func toolTargetMap(m map[string]json.RawMessage) string {
	return textutil.FirstLine(argString(m, "cmd", "command", "file_path", "path"))
}

// renderOutputBody normalizes a tool call output: either a JSON string (older
// shape) or an array of {type,text} content items (newer shape).
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

// bgCommand is a command still running after its exec_command call returned.
type bgCommand struct {
	command string
	body    string     // output read so far
	turn    *core.Turn // the turn holding its card; a later turn opens its own
	idx     int
	done    bool // its item arrived, with the complete output
}

// execChunk is one tools.* result in a batched `exec` call's output.
// session_id marks a command still running; the chunk that reports its
// exit_code carries none, so chunks pair to the script's calls by position.
type execChunk struct {
	ChunkID   string `json:"chunk_id"`
	SessionID *int64 `json:"session_id"`
	Output    string `json:"output"`
}

// execCall is one tools.* call of a batched `exec` script: an exec_command
// with its cmd, or a write_stdin with the session it addresses and what it
// sends. A write_stdin that sends nothing only reads.
type execCall struct {
	stdin   bool
	session string
	chars   string
	command string
}

var (
	execCallRe   = regexp.MustCompile(`\btools\.(exec_command|write_stdin)\b(\s*\(\s*\{)?`)
	sessionArgRe = regexp.MustCompile(`\bsession_id\s*:\s*(\d+)`)
	charsArgRe   = regexp.MustCompile(`(?s)\bchars\s*:\s*("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*')`)
)

func parseExecCalls(script string) []execCall {
	var calls []execCall
	for _, loc := range execCallRe.FindAllStringSubmatchIndex(script, -1) {
		call := execCall{stdin: script[loc[2]:loc[3]] == "write_stdin"}
		args := ""
		if loc[4] >= 0 {
			args = objectLiteral(script[loc[5]-1:])
		}
		if !call.stdin {
			call.command = customExecCommand(args)
		} else {
			if m := sessionArgRe.FindStringSubmatch(args); m != nil {
				call.session = m[1]
			}
			if m := charsArgRe.FindStringSubmatch(args); m != nil {
				call.chars = jsString(m[1])
			}
		}
		calls = append(calls, call)
	}
	return calls
}

// objectLiteral returns the JS object literal s opens with, its braces
// balanced outside string literals.
func objectLiteral(s string) string {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}

// jsString decodes a double- or single-quoted JS string literal.
func jsString(lit string) string {
	if strings.HasPrefix(lit, "'") {
		body := strings.ReplaceAll(lit[1:len(lit)-1], `\'`, `'`)
		lit = `"` + strings.ReplaceAll(body, `"`, `\"`) + `"`
	}
	var out string
	_ = json.Unmarshal([]byte(lit), &out)
	return out
}

// backgroundOutput takes the chunks of a wrapper's output that belong to
// background commands: the chunk that leaves a command running opens its card,
// a later read adds to it, and input sent to it gets a card of its own. part
// is what the live stream shows for that. oneShot is the output of the
// wrapper's other commands. handled is false when no chunk concerns a
// background command.
func (a *Assembler) backgroundOutput(ts time.Time, stash toolStash, output json.RawMessage) (part *core.TurnPart, oneShot string, handled bool) {
	var items []struct{ Text string }
	if json.Unmarshal(output, &items) != nil {
		return nil, "", false
	}
	var chunks []execChunk
	for _, item := range items {
		var c execChunk
		// chunk_id is required: a command may print a JSON object of its own.
		if json.Unmarshal([]byte(item.Text), &c) == nil && c.ChunkID != "" {
			chunks = append(chunks, c)
		}
	}
	calls := parseExecCalls(stash.script)
	if len(calls) != len(chunks) {
		calls = make([]execCall, len(chunks)) // unreadable script: go by the chunks alone
	}
	for i, c := range chunks {
		call := calls[i]
		session := call.session
		if c.SessionID != nil {
			session = strconv.FormatInt(*c.SessionID, 10)
		}
		if session == "" {
			oneShot = joinOutput(oneShot, c.Output)
			continue
		}
		handled = true
		bg := a.background[session]
		a.missed = a.missed || (bg == nil && (call.chars != "" || call.stdin))
		if call.chars != "" {
			part = a.appendTool(ts, "Stdin", bg.title(session), call.chars, strings.TrimRight(c.Output, "\n"))
			continue
		}
		if bg != nil && bg.done {
			if c.SessionID == nil {
				continue // the read that saw it exit, after its item
			}
			bg = nil // the id now names a new command
		}
		spawned := bg == nil && !call.stdin
		if bg == nil {
			bg = &bgCommand{}
			if spawned {
				bg.command = firstNonEmpty(call.command, stash.input)
			}
			a.background[session] = bg
		}
		if p := a.backgroundProgress(ts, stash.name, session, bg, c.Output, spawned); p != nil {
			part = p
		}
	}
	return part, oneShot, handled
}

func (bg *bgCommand) title(session string) string {
	if bg != nil {
		if title := core.ToolTitle("", bg.command); title != "" {
			return title
		}
	}
	return "session " + session
}

// backgroundProgress adds output to the command's card in this turn, opening
// one if the turn has none. A read that found nothing new changes nothing.
func (a *Assembler) backgroundProgress(ts time.Time, name, session string, bg *bgCommand, output string, spawned bool) *core.TurnPart {
	output = strings.TrimRight(output, "\n")
	title := bg.title(session)
	if bg.turn != nil && bg.turn == a.cur {
		if output == "" {
			return nil
		}
		a.ensureTurn(ts)
		bg.body = joinOutput(bg.body, output)
		a.cur.Parts[bg.idx].Content = fenceBody(bg.body)
		increment := core.NewToolPart(name, title, "", fenceBody(output))
		return &increment
	}
	if output == "" && !spawned {
		return nil
	}
	part := a.appendTool(ts, name, title, bg.command, output)
	bg.turn, bg.idx, bg.body = a.cur, len(a.cur.Parts)-1, output
	return part
}

func joinOutput(body, more string) string {
	more = strings.TrimRight(more, "\n")
	if body == "" || more == "" {
		return body + more
	}
	return body + "\n" + more
}

var customCmdRe = regexp.MustCompile(`(?s)\b(?:cmd|command)\s*:\s*("(?:\\.|[^"\\])*")`)

func customExecCommand(input string) string {
	m := customCmdRe.FindStringSubmatch(input)
	if len(m) != 2 {
		return ""
	}
	var command string
	if json.Unmarshal([]byte(m[1]), &command) != nil {
		return ""
	}
	return command
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
