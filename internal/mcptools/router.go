// Package mcptools defines usher-owned MCP tools. Router tools talk to usher's
// control API; the MCP stdio command is only protocol framing and transport
// wiring.
package mcptools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
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

// RouterTools returns the session-management tool catalog. Descriptions hold
// the routing boundary: substantive work goes to a coding-agent session, never
// to the router model itself.
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
			Description: "Resolve a loose session reference — full id, the first eight or more characters of " +
				"one, or a unique title substring — to an exact session id, without sending it anything. " +
				"Use it when the user turns to a session (\"focus X\", \"switch to X\"): it becomes the " +
				"current session. Never invent message text just to satisfy send_to_session.",
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
			Name: "read_session_transcript",
			Description: "Read a page of a session transcript, oldest turn first. With neither cursor, the latest page. " +
				"before pages backward: pass the returned before value while more is true. " +
				"from starts at a turn found by search_sessions: pass a hit's cursor, and pass the returned from value to read on.",
			InputSchema: object(map[string]any{
				"session_id": map[string]any{"type": "string"},
				"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
				"before":     map[string]any{"type": "string"},
				"from":       map[string]any{"type": "string"},
			}, "session_id"),
		},
		{
			Name: "search_sessions",
			Description: "Search transcript text. With session_id, search that session; without it, find matching sessions. " +
				"Each hit carries a cursor for read_session_transcript's from.",
			InputSchema: object(map[string]any{
				"session_id":    map[string]any{"type": "string"},
				"query":         map[string]any{"type": "string"},
				"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"context_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 500},
			}, "query"),
		},
		{
			Name: "create_session",
			Description: "Create a coding-agent session and send its initial instruction. " +
				"Use when the work fits no existing session. Pass cwd when the user names a directory or the work " +
				"belongs to a project — never a path you made up; leave it out for work tied to no project, which then " +
				"runs in the system temp directory. Returns the new session id immediately. " + deliver,
			InputSchema: object(map[string]any{
				"cwd":             map[string]any{"type": "string"},
				"initial_message": map[string]any{"type": "string"},
				"backend":         map[string]any{"type": "string", "enum": []string{"claude", "codex", "pi"}},
				"model":           map[string]any{"type": "string"},
			}, "initial_message"),
		},
		{
			Name:        "list_pending_interactions",
			Description: "List pending permission and user-question interactions across usher sessions.",
			InputSchema: object(map[string]any{}),
		},
		{
			Name: "respond_to_interaction",
			Description: "Allow or deny one pending interaction, when the user asks you to — never on your own " +
				"to unblock work you sent. This handles permission decisions only; question-answer payloads remain in the web UI.",
			InputSchema: object(map[string]any{
				"id":       map[string]any{"type": "string"},
				"behavior": map[string]any{"type": "string", "enum": []string{"allow", "deny"}},
				"reason":   map[string]any{"type": "string"},
			}, "id", "behavior"),
		},
		{
			Name:        "set_auto_approve",
			Description: "Enable or disable usher's auto-approve setting for a session, when the user asks you to.",
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
			Before    string `json:"before"`
			From      string `json:"from"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.SessionID == "" {
			return nil, errors.New("session_id is required")
		}
		sess, err := resolveSession(api, args.SessionID)
		if err != nil {
			return nil, err
		}
		limit := bounded(args.Limit, 20, 200)
		page, err := api.ReadTurns(sess.ID, args.Before, args.From, limit)
		if err != nil {
			return nil, err
		}
		// from has no server-side limit: it returns everything after the cursor.
		next := ""
		if len(page.Turns) > limit {
			next = page.Turns[limit].Cursor
			page.Turns = page.Turns[:limit]
		}
		out := transcriptPage{Turns: make([]transcriptTurn, len(page.Turns)), More: page.More, From: next}
		for i, t := range page.Turns {
			out.Turns[i] = transcriptTurn{Role: t.Role, Content: turnText(t), Time: t.DisplayTime()}
		}
		if page.More && len(page.Turns) > 0 {
			out.Before = page.Turns[0].Cursor
		}
		return out, nil

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
		if args.SessionID != "" {
			sess, err := resolveSession(api, args.SessionID)
			if err != nil {
				return nil, err
			}
			args.SessionID = sess.ID
		}
		// The server clamps further for cross-session searches; no need to
		// mirror its ceilings here.
		return api.SearchSessions(args.SessionID, args.Query,
			bounded(args.Limit, 20, 100), bounded(args.ContextChars, 120, 500))

	case "create_session":
		// A local struct, not pluginapi.CreateRequest: the wire type also
		// carries agent, which must stay out of the model's reach — the input
		// schema is only enforced client-side.
		var args struct {
			Cwd            string `json:"cwd"`
			InitialMessage string `json:"initial_message"`
			Backend        string `json:"backend"`
			Model          string `json:"model"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.InitialMessage == "" {
			return nil, errors.New("initial_message is required")
		}
		out, err := api.RouteNewSession(pluginapi.CreateRequest{
			Cwd: args.Cwd, InitialMessage: args.InitialMessage,
			Backend: args.Backend, Model: args.Model,
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
		pending, err := api.ListPendingInteractions()
		if err != nil {
			return nil, err
		}
		for _, p := range pending {
			if p.ID != args.ID {
				continue
			}
			if err := notRouter(api, p.SessionID); err != nil {
				return nil, err
			}
		}
		err = api.RespondInteraction(args.ID, interaction.Response{
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
		sess, err := resolveSession(api, args.SessionID)
		if err != nil {
			return nil, err
		}
		args.SessionID = sess.ID
		if err := notRouter(api, args.SessionID); err != nil {
			return nil, err
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
		sess, err := resolveSession(api, args.SessionID)
		if err != nil {
			return nil, err
		}
		args.SessionID = sess.ID
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

// notRouter refuses to let the router session — the one calling these tools —
// approve its own requests: those are the user's to answer.
func notRouter(api *pluginapi.Client, sessionID string) error {
	all, err := api.ListSessions()
	if err != nil {
		return err
	}
	for _, sess := range all {
		if sess.Router && sess.ID == sessionID {
			return errors.New("the router session's own permissions are the user's to grant")
		}
	}
	return nil
}

// transcriptPage is a transcript page as the model reads it: each turn
// flattened to text, tool calls reduced to a marker.
type transcriptPage struct {
	Turns []transcriptTurn `json:"turns"`
	More  bool             `json:"more"`
	// Before is the cursor for the page of older turns, set while More.
	Before string `json:"before,omitempty"`
	// From is the cursor to continue from when a read from a cursor was cut
	// short at limit.
	From string `json:"from,omitempty"`
}

type transcriptTurn struct {
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Time    time.Time `json:"ts"`
}

// turnText renders a turn as plain text. User turns carry their text in
// Content; assistant turns carry it in Parts, where a tool call becomes a
// `[tool: Name target]` marker.
func turnText(t core.Turn) string {
	if t.Role != "assistant" {
		return t.Content
	}
	var b strings.Builder
	for _, p := range t.Parts {
		var line string
		switch p.Type {
		case "text":
			line = p.Content
		case "tool":
			line = "[tool: " + strings.TrimSpace(p.ToolName+" "+p.ToolTarget) + "]"
		}
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line)
	}
	return b.String()
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
	// An id, whole or as a prefix, wins over a title that happens to contain
	// it — a session asked to "message 6304903c" mentions that id in its title.
	var byID, matches []pluginapi.SessionView
	lower := strings.ToLower(query)
	for _, sess := range all {
		if sess.ID == query {
			return sess, nil
		}
		if len(query) >= 8 && strings.HasPrefix(sess.ID, query) {
			byID = append(byID, sess)
		} else if strings.Contains(strings.ToLower(sess.Title), lower) {
			matches = append(matches, sess)
		}
	}
	if len(byID) > 0 {
		matches = byID
	}
	if len(matches) == 0 {
		return pluginapi.SessionView{}, fmt.Errorf("session not found: %s", query)
	}
	if len(matches) > 1 {
		return pluginapi.SessionView{}, fmt.Errorf("ambiguous session: %s", query)
	}
	return matches[0], nil
}
