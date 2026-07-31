package usheragent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
)

//go:embed prompts/system_prompt.md
var defaultLLMSystemPrompt string

// LLMConfig configures NewLLM. Client and Model are required.
type LLMConfig struct {
	Client       *ChatClient
	Model        string
	SystemPrompt string // optional override; defaults to embedded prompts/system_prompt.md
	MaxIters     int    // default 10; bounds runaway tool-call loops
	Strict       bool   // append small-model enforcement block to the system prompt
	Logger       *slog.Logger
}

// LLMAgent is a provider-agnostic main-chat agent built on the OpenAI
// Chat Completions wire format. It calls a small set of tools (mirroring
// AgentAPI) on the user's behalf and answers in natural language.
type LLMAgent struct {
	api       AgentAPI
	client    *ChatClient
	model     string
	sysPrompt string
	tools     []ChatTool
	maxIter   int
	logger    *slog.Logger
}

func NewLLM(api AgentAPI, cfg LLMConfig) (*LLMAgent, error) {
	if cfg.Client == nil {
		return nil, errors.New("usheragent: nil ChatClient")
	}
	if cfg.Model == "" {
		return nil, errors.New("usheragent: missing Model")
	}
	sys := cfg.SystemPrompt
	if sys == "" {
		sys = defaultLLMSystemPrompt
	}
	if cfg.Strict {
		sys = sys + strictModeAddendum
	}
	iters := cfg.MaxIters
	if iters <= 0 {
		iters = 10
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMAgent{
		api:       api,
		client:    cfg.Client,
		model:     cfg.Model,
		sysPrompt: sys,
		tools:     defaultTools(),
		maxIter:   iters,
		logger:    logger,
	}, nil
}

const (
	defaultReadTurns      = 20
	maxReadTurns          = 200
	defaultSearchHits     = 20
	maxSearchHits         = 100
	defaultSearchContext  = 120
	maxSearchContext      = 500
	defaultSearchSessions = 20
	maxSearchSessions     = 50
	defaultCreateTimeout  = 300 * time.Second // 5 min — initial-message turn
	maxCreateTimeout      = 900 * time.Second // 15 min — hard ceiling
)

// Handle drives the tool-call loop until the model returns finish_reason="stop"
// or maxIter is exhausted. history is mapped to OpenAI roles verbatim — the
// server owns its shape (summary anchoring, relay excerpts), Handle owns only
// the transport.
func (a *LLMAgent) Handle(ctx context.Context, history []HistoryMessage, currentFocus, userMsg string, relay RelaySink) (AgentResult, error) {
	// currentFocus is deliberately NOT injected as a message: a system
	// message after the static prompt would invalidate the provider's prefix
	// cache for the whole history every time focus changes. The focus id
	// lives in the <current_state> block at the message tail; the behavioral
	// rule ("default to the focus") lives in the static system prompt.
	_ = currentFocus
	msgs := []ChatMessage{{Role: "system", Content: a.sysPrompt}}
	for _, h := range history {
		if h.Tool != nil {
			event := normalizeReplayedToolEvent(*h.Tool)
			msgs = append(msgs,
				ChatMessage{Role: "assistant", ToolCalls: []ToolCall{{ID: event.CallID, Type: "function", Function: ToolCallFunc{Name: event.Name, Arguments: event.Arguments}}}},
				ChatMessage{Role: "tool", ToolCallID: event.CallID, Content: event.Result},
			)
			continue
		}
		role := "user"
		if h.Role == "agent" {
			role = "assistant"
		}
		msgs = append(msgs, ChatMessage{Role: role, Content: h.Content})
	}
	msgs = append(msgs, ChatMessage{Role: "user", Content: userMsg})

	focus := "" // session id touched this turn; carries across the loop's tool calls
	var toolEvents []ToolEvent
	result := func(reply string) AgentResult {
		return AgentResult{Reply: reply, FocusSession: focus, ToolEvents: toolEvents}
	}

	// Anti-poll guard: an immediately-repeated identical (name, args) tool
	// call is blocked — but only when the first attempt SUCCEEDED, so a
	// retry after an error result passes through. Weaker models "wait" for
	// async replies by re-reading the same transcript page or re-sending.
	// (Every error return below carries FocusSession so the server keeps
	// the turn's routing even when the loop fails.)
	lastCallSig := ""
	lastCallOK := false

	for i := 0; i < a.maxIter; i++ {
		resp, err := a.client.ChatCompletion(ctx, ChatRequest{
			Model:    a.model,
			Messages: msgs,
			Tools:    a.tools,
		})
		if err != nil {
			return result(""), err
		}
		if len(resp.Choices) == 0 {
			return result(""), errors.New("empty choices in chat response")
		}
		choice := resp.Choices[0]
		msgs = append(msgs, choice.Message)

		// Checked before the tool_calls dispatch: a max_tokens-truncated
		// response may carry PARTIAL tool calls — dispatching a cut-off
		// send_to_session would deliver a garbled half-message.
		if choice.FinishReason == "length" {
			return result(""), errors.New("response truncated by max_tokens")
		}

		// Some providers return tool calls with a missing/nonstandard
		// finish_reason — dispatch on the presence of tool_calls, so such a
		// turn isn't misread as an (empty) final answer.
		if len(choice.Message.ToolCalls) > 0 {
			for _, call := range choice.Message.ToolCalls {
				sig := call.Function.Name + "\x00" + call.Function.Arguments
				if sig == lastCallSig && lastCallOK {
					out := errResult("repeated identical tool call blocked — do not poll for a session's reply (it is relayed automatically) or resend the same message; answer the user now with what you have")
					a.logger.Warn("main chat tool call blocked",
						"tool", call.Function.Name,
						"call_id", call.ID,
						"arguments", boundedLogText(call.Function.Arguments, 2048),
						"reason", "repeated identical successful call")
					msgs = append(msgs, ChatMessage{
						Role:       "tool",
						ToolCallID: call.ID,
						Content:    out,
					})
					toolEvents = append(toolEvents, durableToolEvent(call, out))
					continue
				}
				started := time.Now()
				out, focusUpdate := a.executeTool(ctx, call.Function.Name, call.Function.Arguments, relay)
				a.logger.Info("main chat tool call",
					"tool", call.Function.Name,
					"call_id", call.ID,
					"arguments", boundedLogText(call.Function.Arguments, 2048),
					"result", boundedLogText(out, 4096),
					"is_error", strings.HasPrefix(out, `{"error":`),
					"focus_session", focusUpdate,
					"duration", time.Since(started))
				lastCallSig = sig
				lastCallOK = !strings.HasPrefix(out, `{"error":`)
				if focusUpdate != "" {
					focus = focusUpdate
				}
				msgs = append(msgs, ChatMessage{
					Role:       "tool",
					ToolCallID: call.ID,
					Content:    out,
				})
				toolEvents = append(toolEvents, durableToolEvent(call, out))
			}
			continue
		}

		switch choice.FinishReason {
		case "stop", "end_turn", "":
			return result(choice.Message.Content), nil
		case "tool_calls":
			// Glitch: finish_reason says tool_calls but none were present
			// (that case was dispatched above). Use the content if there is
			// any; otherwise surface the malformed turn instead of silently
			// returning an empty answer.
			if strings.TrimSpace(choice.Message.Content) != "" {
				return result(choice.Message.Content), nil
			}
			return result(""), errors.New("finish_reason=tool_calls but no tool_calls returned")
		default:
			return result(""), fmt.Errorf("unexpected finish_reason: %q", choice.FinishReason)
		}
	}

	// Tool budget exhausted with the model still calling tools. The user saw
	// none of that traffic, so a bare error wastes the whole turn — instead
	// force a text-only wrap-up: no tools offered, plus an explicit
	// instruction to answer from what it already learned.
	msgs = append(msgs, ChatMessage{
		Role:    "user",
		Content: "[system] Tool budget for this turn is exhausted. Stop calling tools and answer the user now, in plain text, from what you already learned. If you routed work to a session, just say so — its reply arrives automatically.",
	})
	resp, err := a.client.ChatCompletion(ctx, ChatRequest{Model: a.model, Messages: msgs})
	if err != nil {
		return result(""), fmt.Errorf("max iterations (%d) reached and wrap-up failed: %w", a.maxIter, err)
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return result(""), fmt.Errorf("max iterations (%d) reached without final answer", a.maxIter)
	}
	return result(resp.Choices[0].Message.Content), nil
}

// normalizeReplayedToolEvent keeps durable history compatible when exposed
// tool schemas are consolidated. The store remains append-only; migration is
// applied only to the model view.
func normalizeReplayedToolEvent(event ToolEvent) ToolEvent {
	scope := ""
	switch event.Name {
	case "search_session_transcript":
		event.Name = "search_sessions"
		scope = "session"
	case "search_all_sessions":
		event.Name = "search_sessions"
		scope = "all"
	default:
		return event
	}
	var args map[string]any
	if json.Unmarshal([]byte(event.Arguments), &args) == nil {
		if v, ok := args["max_hits"]; ok {
			args["limit"] = v
			delete(args, "max_hits")
		}
		if v, ok := args["max_sessions"]; ok {
			args["limit"] = v
			delete(args, "max_sessions")
		}
		if b, err := json.Marshal(args); err == nil {
			event.Arguments = string(b)
		}
	}
	var result map[string]any
	if json.Unmarshal([]byte(event.Result), &result) == nil {
		result["scope"] = scope
		if scope == "session" {
			if id, ok := args["session_id"]; ok {
				result["session_id"] = id
			}
		}
		if b, err := json.Marshal(result); err == nil {
			event.Result = string(b)
		}
	}
	return event
}

func durableToolEvent(call ToolCall, result string) ToolEvent {
	return ToolEvent{
		CallID: call.ID,
		Name:   call.Function.Name,
		// Arguments are replayed as the protocol-level function.arguments JSON
		// field on later turns, so they must remain complete and valid. Result
		// is replayed as free-text tool content and may be safely bounded.
		Arguments: call.Function.Arguments,
		Result:    boundedLogText(result, 4096),
	}
}

func boundedLogText(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + fmt.Sprintf("… [%d runes omitted]", len(r)-maxRunes)
}

// summarizePrompt drives history compaction. The main chat is a router, not
// a knowledge store — session transcripts remain fully recoverable via tools
// — so the summary keeps only what future ROUTING needs and compresses hard.
const summarizePrompt = `You compress the older part of a session-routing chat into a standing summary. Keep ONLY, as terse bullet points:

- standing user instructions and preferences that remain in force ("always send X-type work to session Y", tone/format asks);
- which sessions were used for what (short id + one-line purpose) and roughly when last touched;
- unresolved threads: work still running, questions the user never answered, promised follow-ups.

DROP: completed exchanges, pleasantries, session reply bodies (a pointer like "session ab12cd34 answered the migration question" is enough — full text stays recoverable from the session transcript). Do not invent anything not in the input. Answer with the summary only, no preamble.`

// SummarizeHistory implements HistorySummarizer: one tools-free completion
// over the flattened history.
func (a *LLMAgent) SummarizeHistory(ctx context.Context, history []HistoryMessage) (string, error) {
	var b strings.Builder
	for _, h := range history {
		if h.Tool != nil {
			event := normalizeReplayedToolEvent(*h.Tool)
			fmt.Fprintf(&b, "tool: %s(%s) -> %s\n\n", event.Name, event.Arguments, event.Result)
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n\n", h.Role, h.Content)
	}
	resp, err := a.client.ChatCompletion(ctx, ChatRequest{
		Model: a.model,
		Messages: []ChatMessage{
			{Role: "system", Content: summarizePrompt},
			{Role: "user", Content: b.String()},
		},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 || strings.TrimSpace(resp.Choices[0].Message.Content) == "" {
		return "", errors.New("empty summary")
	}
	return resp.Choices[0].Message.Content, nil
}

// executeTool dispatches a tool call to AgentAPI. Output is always a JSON
// string (or `{"error":"..."}`) — that's what OpenAI-protocol `role:"tool"`
// messages expect for `content`. The second return value is the session id
// this tool touched (empty for read-only / non-targeted tools); used by
// Handle to compute the turn's FocusSession. relay (may be nil) is where
// send/create replies land once the target session completes its turn.
func (a *LLMAgent) executeTool(ctx context.Context, name, argsJSON string, relay RelaySink) (string, string) {
	switch name {
	case "list_sessions":
		var args struct {
			Statuses []string `json:"statuses"`
			Limit    *int     `json:"limit"`
			Archived bool     `json:"archived"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		keep := make(map[core.Status]bool, len(args.Statuses))
		for _, raw := range args.Statuses {
			status := core.Status(raw)
			switch status {
			case core.StatusIdle, core.StatusLive, core.StatusRunning, core.StatusAwaitingPermission:
				keep[status] = true
			default:
				return errResult("invalid status: " + raw), ""
			}
		}
		limit := 20
		if args.Limit != nil {
			if *args.Limit <= 0 {
				return errResult("limit must be positive"), ""
			}
			limit = *args.Limit
			if limit > 200 {
				limit = 200
			}
		}
		note := "Archived sessions are excluded. Query again with archived=true to list archived sessions separately."
		if args.Archived {
			note = "Returning archived sessions only. Use archived=false (the default) for active sessions."
		}
		sessions, total := a.enrichedSessions(keep, limit, args.Archived)
		b, _ := json.Marshal(map[string]any{
			"sessions":       sessions,
			"returned":       len(sessions),
			"total_matching": total,
			"truncated":      len(sessions) < total,
			"note":           note,
		})
		return string(b), ""

	case "focus_session":
		var args struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" {
			return errResult("session_id is required"), ""
		}
		matches := matchSessions(a.api.ListSessions(), args.SessionID)
		if len(matches) == 0 {
			return errResult("session not found: " + args.SessionID), ""
		}
		if len(matches) > 1 {
			return errResult("ambiguous session: " + args.SessionID), ""
		}
		sess := matches[0]
		payload, _ := json.Marshal(map[string]any{
			"status":     "focused",
			"session_id": sess.ID,
			"title":      sess.Title,
		})
		return string(payload), sess.ID

	case "send_to_session":
		var args struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" || args.Text == "" {
			return errResult("session_id and text are required"), ""
		}
		if relay == nil {
			// No relay channel (caller can't display async replies) —
			// plain fire-and-forget.
			if err := a.api.SendToSession(args.SessionID, args.Text); err != nil {
				return errResult(err.Error()), ""
			}
			return `{"status":"sent"}`, args.SessionID
		}
		if err := a.api.SendToSessionRelayed(args.SessionID, args.Text, relay); err != nil {
			return errResult(err.Error()), ""
		}
		return `{"status":"sent","note":"the session's reply will be shown to the user verbatim when it completes — do not wait for it or restate it"}`, args.SessionID

	case "read_session_transcript":
		var args struct {
			SessionID string `json:"session_id"`
			Limit     int    `json:"limit"`
			Offset    *int   `json:"offset"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" {
			return errResult("session_id is required"), ""
		}
		limit := args.Limit
		if limit <= 0 {
			limit = defaultReadTurns
		}
		if limit > maxReadTurns {
			limit = maxReadTurns
		}
		// Negative offset (or omitted) = most recent page; a caller pages older
		// turns with offset = returned offset - limit, using total to stop.
		offset := -1
		if args.Offset != nil {
			offset = *args.Offset
		}
		turns, start, total, err := a.api.ReadSessionTranscriptPage(args.SessionID, offset, limit)
		if err != nil {
			return errResult(err.Error()), ""
		}
		payload, _ := json.Marshal(map[string]any{
			"turns":    turns,
			"offset":   start,
			"total":    total,
			"has_more": start > 0 || start+len(turns) < total,
		})
		return string(payload), args.SessionID

	case "search_sessions":
		var args struct {
			SessionID    string `json:"session_id"`
			Query        string `json:"query"`
			Limit        int    `json:"limit"`
			ContextChars int    `json:"context_chars"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if strings.TrimSpace(args.Query) == "" {
			return errResult("query is required"), ""
		}
		ctxChars := args.ContextChars
		if ctxChars <= 0 {
			ctxChars = defaultSearchContext
		}
		if ctxChars > maxSearchContext {
			ctxChars = maxSearchContext
		}
		if args.SessionID != "" {
			matches := matchSessions(a.api.ListSessions(), args.SessionID)
			if len(matches) == 0 {
				return errResult("session not found: " + args.SessionID), ""
			}
			if len(matches) > 1 {
				return errResult("ambiguous session: " + args.SessionID), ""
			}
			limit := args.Limit
			if limit <= 0 {
				limit = defaultSearchHits
			}
			if limit > maxSearchHits {
				limit = maxSearchHits
			}
			sess := matches[0]
			hits, truncated, err := a.api.SearchSessionTranscript(sess.ID, args.Query, limit, ctxChars)
			if err != nil {
				return errResult(err.Error()), ""
			}
			payload, _ := json.Marshal(map[string]any{"scope": "session", "session_id": sess.ID, "hits": hits, "truncated": truncated})
			return string(payload), sess.ID
		}
		limit := args.Limit
		if limit <= 0 {
			limit = defaultSearchSessions
		}
		if limit > maxSearchSessions {
			limit = maxSearchSessions
		}
		results, truncated, err := a.api.SearchAllSessions(args.Query, limit, ctxChars)
		if err != nil {
			return errResult(err.Error()), ""
		}
		payload, _ := json.Marshal(map[string]any{"scope": "all", "results": results, "truncated": truncated})
		// Cross-session search doesn't target one session, so it sets no focus.
		return string(payload), ""

	case "create_session":
		var args struct {
			Cwd            string `json:"cwd"`
			InitialMessage string `json:"initial_message"`
			Backend        string `json:"backend"`
			Model          string `json:"model"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.Cwd == "" || args.InitialMessage == "" {
			return errResult("cwd and initial_message are required"), ""
		}
		opts := core.CreateOptions{
			Backend: args.Backend, Cwd: args.Cwd,
			InitialMessage: args.InitialMessage, Model: args.Model,
		}
		if relay == nil {
			timeout := defaultCreateTimeout
			if args.TimeoutSeconds > 0 {
				t := time.Duration(args.TimeoutSeconds) * time.Second
				if t > maxCreateTimeout {
					t = maxCreateTimeout
				}
				timeout = t
			}
			newID, text, err := a.api.CreateSession(ctx, opts, timeout)
			if err != nil {
				payload, _ := json.Marshal(map[string]any{"session_id": newID, "response": text, "error": err.Error()})
				return string(payload), newID
			}
			payload, _ := json.Marshal(map[string]any{"session_id": newID, "response": text})
			return string(payload), newID
		}
		newID, err := a.api.CreateSessionRelayed(opts, relay)
		if err != nil {
			return errResult(err.Error()), ""
		}
		payload, _ := json.Marshal(map[string]any{
			"session_id": newID,
			"status":     "created",
			"note":       "the session's first reply will be shown to the user verbatim when it completes — do not wait for it or restate it",
		})
		return string(payload), newID

	case "list_pending_interactions":
		b, _ := json.Marshal(a.api.ListPendingInteractions())
		return string(b), ""

	case "respond_to_interaction":
		var args struct {
			ID       string `json:"id"`
			Behavior string `json:"behavior"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.ID == "" || (args.Behavior != "allow" && args.Behavior != "deny") {
			return errResult("id and behavior (allow|deny) are required"), ""
		}
		if err := a.api.RespondInteraction(args.ID, interaction.Response{
			Behavior: args.Behavior,
			Reason:   args.Reason,
			Scope:    "once",
		}); err != nil {
			return errResult(err.Error()), ""
		}
		// Don't update focus from respond_to_interaction; keep it tied to
		// send-class operations so "now show me" reliably means the session
		// the agent last sent to.
		return `{"status":"ok"}`, ""

	case "set_auto_approve":
		var args struct {
			SessionID string `json:"session_id"`
			Enabled   bool   `json:"enabled"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" {
			return errResult("session_id is required"), ""
		}
		a.api.SetAutoApprove(args.SessionID, args.Enabled)
		// Housekeeping, not a send — don't change focus (same as respond).
		payload, _ := json.Marshal(map[string]any{"status": "ok", "session_id": args.SessionID, "auto_approve": args.Enabled})
		return string(payload), ""

	case "set_archived":
		var args struct {
			SessionID string `json:"session_id"`
			Archived  bool   `json:"archived"`
		}
		if err := json.Unmarshal([]byte(repairJSONArgs(argsJSON)), &args); err != nil {
			return errResult("invalid arguments: " + err.Error()), ""
		}
		if args.SessionID == "" {
			return errResult("session_id is required"), ""
		}
		if args.Archived {
			a.api.Archive(args.SessionID)
		} else {
			a.api.Unarchive(args.SessionID)
		}
		payload, _ := json.Marshal(map[string]any{"status": "ok", "session_id": args.SessionID, "archived": args.Archived})
		return string(payload), ""

	default:
		return errResult("unknown tool: " + name), ""
	}
}

// sessionView adds the two web-sidebar flags (archived, auto_approve) to a
// session so the agent can see and report them.
type sessionView struct {
	core.Session
	Archived    bool `json:"archived"`
	AutoApprove bool `json:"auto_approve"`
}

func (a *LLMAgent) enrichedSessions(statuses map[core.Status]bool, limit int, archived bool) ([]sessionView, int) {
	sessions := a.api.ListSessions()
	out := make([]sessionView, 0, len(sessions))
	total := 0
	for _, s := range sessions {
		isArchived := a.api.IsArchived(s.ID)
		if isArchived != archived {
			continue
		}
		if len(statuses) > 0 && !statuses[s.Status] {
			continue
		}
		total++
		if limit <= 0 || len(out) < limit {
			out = append(out, sessionView{
				Session:     s,
				Archived:    isArchived,
				AutoApprove: a.api.IsAutoApprove(s.ID),
			})
		}
	}
	return out, total
}

func errResult(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}

// repairJSONArgs makes a best-effort fix to malformed tool-call argument
// JSON emitted by small models. Patterned after Hermes-Agent's 5-pass
// repair pipeline. Returns the original string if it parses cleanly, the
// repaired string if any pass succeeds, or "{}" if everything fails (a
// recoverable empty object beats a hard error mid-loop).
//
// We only touch arguments that already FAILED to parse — never modify
// already-valid JSON. The tool dispatch path calls Unmarshal on the
// returned string and reports its own errors as usual.
func repairJSONArgs(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	var probe any
	if err := json.Unmarshal([]byte(raw), &probe); err == nil {
		return raw
	}
	s := raw
	// 1. Strip trailing commas before } or ]
	s = jsonTrailingComma.ReplaceAllString(s, "$1")
	// 2. Quote unquoted keys ({key: 1} → {"key": 1}). Conservative — only
	//    matches positions clearly inside an object (after `{` or `,`).
	s = jsonUnquotedKey.ReplaceAllString(s, `$1"$2":`)
	// 3. Python literal sentinels → JSON
	s = jsonPyNone.ReplaceAllString(s, "null")
	s = jsonPyTrue.ReplaceAllString(s, "true")
	s = jsonPyFalse.ReplaceAllString(s, "false")
	// 4. Single-quoted string values → double-quoted (very narrow: covers
	//    {key: 'value'} mistake, not nested complex strings).
	s = jsonSingleQuoteVal.ReplaceAllStringFunc(s, func(m string) string {
		// Replace surrounding single quotes with double; escape any
		// embedded double quotes.
		inner := m[1 : len(m)-1]
		inner = strings.ReplaceAll(inner, `"`, `\"`)
		return `"` + inner + `"`
	})
	if err := json.Unmarshal([]byte(s), &probe); err == nil {
		return s
	}
	return "{}"
}

var (
	jsonTrailingComma  = regexp.MustCompile(`,(\s*[}\]])`)
	jsonUnquotedKey    = regexp.MustCompile(`([{,]\s*)([A-Za-z_][A-Za-z0-9_]*)\s*:`)
	jsonPyNone         = regexp.MustCompile(`\bNone\b`)
	jsonPyTrue         = regexp.MustCompile(`\bTrue\b`)
	jsonPyFalse        = regexp.MustCompile(`\bFalse\b`)
	jsonSingleQuoteVal = regexp.MustCompile(`'[^'\n]*'`)
)

func defaultTools() []ChatTool {
	return []ChatTool{
		{
			Type: "function",
			Function: ChatFunction{
				Name: "list_sessions",
				Description: `List coding-agent sessions discovered on this machine. Optionally filter by status and limit the result. Returns {sessions, returned, total_matching, truncated, note}; each session includes id, cwd, title, status, timestamps, runtime metadata, archived, and auto_approve. With no arguments, returns at most 20 non-archived sessions. Archived sessions require a separate archived=true query. If truncated is true, absence from sessions does NOT mean a session is missing.

USE FOR: questions you can't answer from <current_state> — exact timestamps, status that may have changed in the last few seconds.

DO NOT USE FOR: simple metadata trivia like "how many sessions", "which session is in /tmp", "what's the focused cwd" — that's already in the <current_state> block at the end of the user message.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"statuses": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string", "enum": []string{"idle", "live", "running", "awaiting_permission"}},
							"description": "Optional statuses to include. Omit for all statuses.",
						},
						"limit": map[string]any{
							"type":        "integer",
							"minimum":     1,
							"maximum":     200,
							"description": "Maximum sessions to return. Defaults to 20; maximum 200.",
						},
						"archived": map[string]any{
							"type":        "boolean",
							"description": "False/default returns only non-archived sessions. True returns only archived sessions in a separate query.",
						},
					},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "focus_session",
				Description: `Switch the main chat's dashboard focus to an existing session WITHOUT sending a message, starting a turn, or reading its transcript.

USE FOR: pure navigation requests such as "jump to X", "focus X", "switch to X", or "跳到 X" when the user supplied no instruction or question for that session.

DO NOT USE FOR: forwarding work or a question (use send_to_session). Never invent work to accompany a navigation request.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{
							"type":        "string",
							"description": "Session ID, unique ID prefix, or unique title substring.",
						},
					},
					"required":             []string{"session_id"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "send_to_session",
				Description: `Deliver a message to a session. Returns immediately; when the session finishes its turn, its reply is automatically shown to the user IN THIS CHAT, verbatim — you never see it and must not wait for it or restate it. Updates focus to the target session.

USE FOR: the default way to route ANY instruction or question to a session — quick questions, long tasks, "ask X to ...", "run Z", follow-ups. Task duration doesn't matter; the reply arrives whenever it's ready.

DO NOT USE FOR: pure navigation such as "jump/focus/switch to X" when the user supplied no work. Use focus_session instead. The text must faithfully represent an instruction or question the user actually provided; never invent one.

AFTER CALLING: reply with at most one short routing sentence (or nothing beyond what the dashboard already shows). Never say you will "report back" — the relay is automatic.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{
							"type":        "string",
							"description": "Full session ID, exactly as returned by list_sessions.",
						},
						"text": map[string]any{
							"type":        "string",
							"description": "The message text to send.",
						},
					},
					"required":             []string{"session_id", "text"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "read_session_transcript",
				Description: `Read one page of user/assistant turns from a session's transcript. Tool uses inside the session are inlined as ` + "`tool: Name`" + ` annotations. Returns {turns: [{role, content, ts}, ...], offset, total, has_more} — offset is the absolute index of the first turn returned, total the whole transcript's turn count.

USE FOR: any question about what was DONE or SAID inside a session — "what did session X say?", "summarize Y", "what's the latest output from Z?", "any update?", deeper dives. ALSO the recovery path for excerpted replies: a chat message showing "[… N chars omitted …]" has its full text here — recent replies are in the default page; older ones, locate via search_sessions with session_id and jump with offset.

PAGING: with no offset you get the most recent ` + "`limit`" + ` turns. total tells you how many exist; if has_more is true there are older turns you haven't seen. To reach a specific spot — e.g. a turn_index from search_sessions, or the page before this one — pass offset (absolute, 0-based from the start). There is no depth limit; limit only bounds one page.

DO NOT USE FOR: looking up session metadata (cwd, title, status, count) — <current_state> already has that. For pure navigation such as "switch to session X", use focus_session.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string"},
						"limit":      map[string]any{"type": "integer", "description": "Optional. Page size (turns per call). Default 20, max 200."},
						"offset":     map[string]any{"type": "integer", "description": "Optional. Absolute 0-based index of the first turn to return. Omit for the most recent page. Use a turn_index from search_sessions to jump to a hit."},
					},
					"required":             []string{"session_id"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "search_sessions",
				Description: `Search user/assistant prose using a case-insensitive literal substring. With session_id, searches that one session and returns {scope:"session", session_id, hits, truncated}; without session_id, searches every session and returns {scope:"all", results, truncated}, ranked by hit count.

USE WITH session_id: locate something inside a known session, then call read_session_transcript with offset=<turn_index> for full context.

USE WITHOUT session_id: find which sessions discussed a topic, then focus, route to, or drill into a result.

DO NOT USE FOR: reading latest activity (use read_session_transcript), pure navigation (use focus_session), or matching tool/command/file names. Search covers prose only.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id":    map[string]any{"type": "string", "description": "Optional session ID, unique ID prefix, or unique title substring. Omit to search all sessions."},
						"query":         map[string]any{"type": "string", "description": "Literal substring to find (case-insensitive)."},
						"limit":         map[string]any{"type": "integer", "description": "Optional. With session_id, max matching turns (default 20, max 100); without it, max matching sessions (default 20, max 50)."},
						"context_chars": map[string]any{"type": "integer", "description": "Optional. Characters of context on each side of the match. Default 120, max 500."},
					},
					"required":             []string{"query"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "create_session",
				Description: `Start a brand-new coding-agent session in cwd and send it an initial message. Returns {session_id, status} immediately; the session's first reply is automatically shown to the user in this chat, verbatim — do not wait for it or restate it. New id becomes focus.

USE FOR: the user wants fresh context that doesn't fit any session in <current_state> — scratch experiments, a new project, isolated debugging.

DO NOT USE FOR: routing into an existing session that matches the work — use send_to_session on that one. cwd must already exist; do not invent paths. /tmp is a safe default for ephemeral / scratch work when the user gives no hint. Leave backend and model empty unless the user requests one or the task needs a backend-specific capability.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"cwd":             map[string]any{"type": "string", "description": "Working directory for the new session. Must exist."},
						"initial_message": map[string]any{"type": "string", "description": "First message sent to the new session."},
						"backend":         map[string]any{"type": "string", "description": "Optional backend name. Empty uses model inference or the configured default."},
						"model":           map[string]any{"type": "string", "description": "Optional model from the selected backend's model catalog."},
					},
					"required":             []string{"cwd", "initial_message"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "list_pending_interactions",
				Description: `List permission requests across all sessions waiting for a user decision. Returns [{id, session_id, tool_name, tool_input, cwd, created_at}, ...].

USE FOR: "any pending approvals?", "what's waiting", "show me the queue".

DO NOT USE FOR: deciding to approve or deny — that's respond_to_interaction. The count alone is in <current_state>.pending_permission_requests.`,
				Parameters: map[string]any{
					"type":                 "object",
					"properties":           map[string]any{},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "respond_to_interaction",
				Description: `Approve or deny a single pending permission request by id.

USE FOR: explicit user authorization — "approve the bash one", "deny X", "let it run", "block the rm".

DO NOT blanket-approve anything dangerous-looking (mass deletes, sending sensitive data, broad access) without confirming with the user first.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":       map[string]any{"type": "string"},
						"behavior": map[string]any{"type": "string", "enum": []string{"allow", "deny"}},
						"reason":   map[string]any{"type": "string"},
					},
					"required":             []string{"id", "behavior"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "set_auto_approve",
				Description: `Turn a session's permission auto-approval on or off. When on, usher silently allows that session's permission prompts instead of queuing them for a decision.

USE FOR: "stop asking me about session X", "auto-approve the deploy session", "let X run unattended", and turning it back off ("ask me again for X").

DO NOT blanket-enable on a session doing dangerous work (mass deletes, prod changes) without confirming with the user — it suppresses every future prompt for that session.`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string", "description": "Full session ID, exactly as returned by list_sessions."},
						"enabled":    map[string]any{"type": "boolean", "description": "true to auto-approve this session's prompts, false to resume asking."},
					},
					"required":             []string{"session_id", "enabled"},
					"additionalProperties": false,
				},
			},
		},
		{
			Type: "function",
			Function: ChatFunction{
				Name: "set_archived",
				Description: `Archive or unarchive a session. Archived sessions are hidden from the default session list (they still exist and can be unarchived). Use to tidy finished / stale sessions.

USE FOR: "archive the old spike", "hide the finished sessions", "bring back session X", "unarchive Y".`,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"session_id": map[string]any{"type": "string", "description": "Full session ID, exactly as returned by list_sessions."},
						"archived":   map[string]any{"type": "boolean", "description": "true to archive (hide), false to unarchive (restore)."},
					},
					"required":             []string{"session_id", "archived"},
					"additionalProperties": false,
				},
			},
		},
	}
}
