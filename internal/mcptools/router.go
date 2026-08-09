// Package mcptools defines usher-owned MCP tools independently of the
// main-chat agent. Router tools talk to usher's control API; the MCP stdio
// command is only protocol framing and transport wiring.
package mcptools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/pluginapi"
)

// Tool is the MCP tools/list representation used by the stdio server.
type Tool struct {
	Meta        map[string]any `json:"_meta,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Router dispatches usher's routing tools. Holding these tools is what makes
// a session the router, so it never has to name itself: usher resolves the
// relay inbox — its own router session — on the other side of the socket.
type Router struct {
	// API is the control socket. Nil disables every routing tool.
	API *pluginapi.Client
}

// RouterTools returns the opt-in session-management tool catalog. Descriptions
// preserve the old main-chat agent's routing boundary: substantive work goes
// to a coding-agent session, never to the router model itself.
//
// The send/create descriptions promise the report-back, because holding these
// tools is what makes a session the router. Each call's result confirms whether
// it was actually attached, for the case where usher has no router session
// designated and nothing will come back.
func RouterTools() []Tool {
	deliver := "The call returns as soon as the prompt is accepted — it does NOT wait for the turn. " +
		"usher puts the session's reply in front of the user itself, verbatim, the moment it lands: it is " +
		"NOT your job to deliver, quote, or summarize it. You are told what finished as a " +
		"[session <id> replied] line prefixed to your next message. Never wait for it, never poll for it " +
		"with list_sessions or read_session_transcript. Send, then end your turn. " +
		"If the result says relayed=false, nothing will be reported and you must collect it with " +
		"read_session_transcript instead."

	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{
			"type":                 "object",
			"properties":           properties,
			"additionalProperties": false,
		}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	tools := []Tool{
		{
			Name: "list_sessions",
			Description: "List usher coding-agent sessions. Returns session id, cwd, title, status, timestamps, backend, archived, and auto_approve. " +
				"Use this to choose a session before routing work. Archived sessions are a separate query: " +
				"archived=true lists only them, the default lists only live ones.",
			InputSchema: object(map[string]any{
				"statuses": map[string]any{"type": "array", "items": map[string]any{
					"type": "string", "enum": []string{"idle", "live", "running", "awaiting_permission"},
				}},
				"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
				"archived": map[string]any{"type": "boolean"},
			}),
		},
		{
			Name: "resolve_session",
			Description: "Resolve a loose session reference — full id, unique id prefix, or unique title " +
				"substring — to an exact session id, without sending it anything. Use it for a pure " +
				"navigation request such as \"jump to X\", and never invent message text just to satisfy " +
				"send_to_session. It cannot move the dashboard's focus, so do not tell the user the view " +
				"switched.",
			InputSchema: object(map[string]any{
				"session_id": map[string]any{"type": "string"},
			}, "session_id"),
		},
		{
			Name: "send_to_session",
			Description: "Deliver the user's substantive instruction or question to an existing coding-agent session. " +
				"This is THE delivery tool — task duration never matters, so never decline work for being long-running. " +
				deliver,
			InputSchema: object(map[string]any{
				"session_id": map[string]any{"type": "string", "description": "Full session id from list_sessions."},
				"text":       map[string]any{"type": "string"},
			}, "session_id", "text"),
		},
		{
			Name:        "read_session_transcript",
			Description: "Read one page of a session transcript. Omit offset for the latest page; use the returned absolute offset to page backward.",
			InputSchema: object(map[string]any{
				"session_id": map[string]any{"type": "string"},
				"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
				"offset":     map[string]any{"type": "integer", "minimum": 0},
			}, "session_id"),
		},
		{
			Name:        "search_sessions",
			Description: "Search transcript text. With session_id, search that session; without it, find matching sessions.",
			InputSchema: object(map[string]any{
				"session_id":    map[string]any{"type": "string"},
				"query":         map[string]any{"type": "string"},
				"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"context_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 500},
			}, "query"),
		},
		{
			Name: "create_session",
			Description: "Create a coding-agent session in an explicit cwd and send its initial instruction. " +
				"Use when the work fits no existing session. The cwd must already exist. " +
				"Returns the new session id immediately. " + deliver,
			InputSchema: object(map[string]any{
				"cwd":             map[string]any{"type": "string"},
				"initial_message": map[string]any{"type": "string"},
				"backend":         map[string]any{"type": "string", "enum": []string{"claude", "codex", "pi"}},
				"model":           map[string]any{"type": "string"},
			}, "cwd", "initial_message"),
		},
		{
			Name:        "list_pending_interactions",
			Description: "List pending permission and user-question interactions across usher sessions.",
			InputSchema: object(map[string]any{}),
		},
		{
			Name:        "respond_to_interaction",
			Description: "Allow or deny one pending interaction. This handles permission decisions only; question-answer payloads remain in the web UI.",
			InputSchema: object(map[string]any{
				"id":       map[string]any{"type": "string"},
				"behavior": map[string]any{"type": "string", "enum": []string{"allow", "deny"}},
				"reason":   map[string]any{"type": "string"},
			}, "id", "behavior"),
		},
		{
			Name:        "set_auto_approve",
			Description: "Enable or disable usher's auto-approve setting for a session.",
			InputSchema: object(map[string]any{
				"session_id": map[string]any{"type": "string"},
				"enabled":    map[string]any{"type": "boolean"},
			}, "session_id", "enabled"),
		},
		{
			Name:        "set_archived",
			Description: "Archive or unarchive an usher session.",
			InputSchema: object(map[string]any{
				"session_id": map[string]any{"type": "string"},
				"archived":   map[string]any{"type": "boolean"},
			}, "session_id", "archived"),
		},
	}
	// Exempt every routing tool from Tool Search deferral (paired with the
	// server-level alwaysLoad in mcp-router.json): these tools are the router
	// session's whole job, so none may hide behind a ToolSearch step.
	for i := range tools {
		tools[i].Meta = map[string]any{"anthropic/alwaysLoad": true}
	}
	return tools
}

// Call validates and dispatches one router tool call against usher's control
// socket. The returned value is JSON-compatible and becomes MCP
// structuredContent as well as text content.
//
// There is no context parameter because there is nothing to cancel: every
// call is a short control-socket round trip, and the client bounds each one
// itself. Delivering a prompt no longer blocks on the turn.
func (r *Router) Call(name string, raw json.RawMessage) (any, error) {
	if r == nil || r.API == nil {
		return nil, errors.New("router control API unavailable")
	}
	api := r.API
	switch name {
	case "list_sessions":
		var args struct {
			Statuses []string `json:"statuses"`
			Limit    int      `json:"limit"`
			Archived bool     `json:"archived"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		keep := map[string]bool{}
		for _, status := range args.Statuses {
			switch status {
			case "idle", "live", "running", "awaiting_permission":
				keep[status] = true
			default:
				return nil, fmt.Errorf("invalid status: %s", status)
			}
		}
		limit := bounded(args.Limit, 20, 200)
		all, err := api.ListSessions()
		if err != nil {
			return nil, err
		}
		filtered := make([]pluginapi.SessionView, 0, limit)
		total := 0
		for _, sess := range all {
			if sess.Archived != args.Archived || len(keep) > 0 && !keep[string(sess.Status)] {
				continue
			}
			total++
			if len(filtered) < limit {
				filtered = append(filtered, sess)
			}
		}
		return map[string]any{
			"sessions": filtered, "returned": len(filtered), "total_matching": total,
			"truncated": len(filtered) < total,
		}, nil

	case "resolve_session":
		var args struct {
			SessionID string `json:"session_id"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		sess, err := resolveSession(api, args.SessionID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "resolved", "session_id": sess.ID, "title": sess.Title}, nil

	case "send_to_session":
		var args struct {
			SessionID string `json:"session_id"`
			Text      string `json:"text"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.SessionID == "" || args.Text == "" {
			return nil, errors.New("session_id and text are required")
		}
		// Resolve first: a prompt delivered to the wrong session is not
		// something the model can take back once the turn starts.
		sess, err := resolveSession(api, args.SessionID)
		if err != nil {
			return nil, err
		}
		out, err := api.SendToSessionRelayed(sess.ID, args.Text)
		if err != nil {
			return nil, err
		}
		return deliveredResult(out), nil

	case "read_session_transcript":
		var args struct {
			SessionID string `json:"session_id"`
			Limit     int    `json:"limit"`
			Offset    *int   `json:"offset"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.SessionID == "" {
			return nil, errors.New("session_id is required")
		}
		offset := -1
		if args.Offset != nil {
			offset = *args.Offset
		}
		return api.ReadSessionTranscriptPage(args.SessionID, offset, bounded(args.Limit, 20, 200))

	case "search_sessions":
		var args struct {
			SessionID    string `json:"session_id"`
			Query        string `json:"query"`
			Limit        int    `json:"limit"`
			ContextChars int    `json:"context_chars"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if strings.TrimSpace(args.Query) == "" {
			return nil, errors.New("query is required")
		}
		// The server clamps further for cross-session searches; no need to
		// mirror its ceilings here.
		return api.SearchSessions(args.SessionID, args.Query,
			bounded(args.Limit, 20, 100), bounded(args.ContextChars, 120, 500))

	case "create_session":
		// A local struct, not pluginapi.CreateRequest: the wire type also
		// carries agent and relay, which must stay out of the model's reach —
		// the input schema is only enforced client-side.
		var args struct {
			Cwd            string `json:"cwd"`
			InitialMessage string `json:"initial_message"`
			Backend        string `json:"backend"`
			Model          string `json:"model"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.Cwd == "" || args.InitialMessage == "" {
			return nil, errors.New("cwd and initial_message are required")
		}
		out, err := api.CreateSession(pluginapi.CreateRequest{
			Cwd: args.Cwd, InitialMessage: args.InitialMessage,
			Backend: args.Backend, Model: args.Model,
			// Never the model's call: routed work always reports back.
			Relay: true,
		})
		if err != nil {
			return nil, err
		}
		return deliveredResult(out), nil

	case "list_pending_interactions":
		return api.ListPendingInteractions()

	case "respond_to_interaction":
		var args struct {
			ID       string `json:"id"`
			Behavior string `json:"behavior"`
			Reason   string `json:"reason"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.ID == "" || args.Behavior != "allow" && args.Behavior != "deny" {
			return nil, errors.New("id and behavior (allow|deny) are required")
		}
		err := api.RespondInteraction(args.ID, interaction.Response{
			Behavior: args.Behavior, Reason: args.Reason, Scope: "once",
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "id": args.ID}, nil

	case "set_auto_approve":
		var args struct {
			SessionID string `json:"session_id"`
			Enabled   bool   `json:"enabled"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.SessionID == "" {
			return nil, errors.New("session_id is required")
		}
		if err := api.SetAutoApprove(args.SessionID, args.Enabled); err != nil {
			return nil, err
		}
		return map[string]any{
			"status": "ok", "session_id": args.SessionID, "auto_approve": args.Enabled,
		}, nil

	case "set_archived":
		var args struct {
			SessionID string `json:"session_id"`
			Archived  bool   `json:"archived"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.SessionID == "" {
			return nil, errors.New("session_id is required")
		}
		if err := api.SetArchived(args.SessionID, args.Archived); err != nil {
			return nil, err
		}
		return map[string]any{
			"status": "ok", "session_id": args.SessionID, "archived": args.Archived,
		}, nil
	}
	return nil, fmt.Errorf("unknown router tool: %s", name)
}

// deliveredResult is the ack for a send or create. The note repeats the tool
// description's promise at the point of use, where a model that skimmed the
// catalog still sees it — the failure this guards against is a router that
// polls for a reply, or apologises for not having one yet.
func deliveredResult(d pluginapi.Delivery) map[string]any {
	out := map[string]any{
		"status": "delivered", "session_id": d.SessionID, "relayed": d.Relayed,
	}
	if d.Relayed {
		out["note"] = "usher shows the session's reply to the user verbatim when it completes, and tells " +
			"you what finished with your next message — do not wait for it, poll for it, or restate it"
	} else {
		out["note"] = "no reply will be reported back; read_session_transcript is the only way to collect it"
	}
	return out
}

func decode(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func bounded(value, fallback, max int) int {
	if value <= 0 {
		return fallback
	}
	if value > max {
		return max
	}
	return value
}

func resolveSession(api *pluginapi.Client, query string) (pluginapi.SessionView, error) {
	if query == "" {
		return pluginapi.SessionView{}, errors.New("session_id is required")
	}
	all, err := api.ListSessions()
	if err != nil {
		return pluginapi.SessionView{}, err
	}
	var matches []pluginapi.SessionView
	lower := strings.ToLower(query)
	for _, sess := range all {
		if sess.ID == query {
			return sess, nil
		}
		if strings.HasPrefix(sess.ID, query) || strings.Contains(strings.ToLower(sess.Title), lower) {
			matches = append(matches, sess)
		}
	}
	if len(matches) == 0 {
		return pluginapi.SessionView{}, fmt.Errorf("session not found: %s", query)
	}
	if len(matches) > 1 {
		return pluginapi.SessionView{}, fmt.Errorf("ambiguous session: %s", query)
	}
	return matches[0], nil
}
