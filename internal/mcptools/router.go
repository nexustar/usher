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
	"github.com/nexustar/usher/internal/pluginapi"
)

// Tool is the MCP tools/list representation used by the stdio server.
type Tool struct {
	Meta        map[string]any `json:"_meta,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Router dispatches usher's routing tools.
type Router struct {
	// API is the control socket. Nil disables every routing tool.
	API *pluginapi.Client
}

// RouterTools returns the session-management tool catalog.
func RouterTools() []Tool {
	deliver := " Returns as soon as the prompt is accepted; the turn runs on without you, however long it takes. " +
		"usher shows the session's reply to the user itself, verbatim, and tells you it came in a " +
		"[session <id> replied] note before your next message. Never wait or poll for it."
	userWords := map[string]any{"type": "string",
		"description": "The user's message in their own words, reworded only as far as it needs to stand on its own."}
	sessionRef := map[string]any{"type": "string",
		"description": "A session id, its first 8 or more characters, or a unique part of its title."}
	onRequest := "Only when the user asks for one; otherwise usher's default."

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
			Description: "List sessions with id, title, cwd, backend and status; use it to pick a session for routing. " +
				"The default omits archived sessions; archived=true lists only them.",
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
			Description: "Resolve a session reference to its exact id and title without sending it anything; " +
				"it becomes the current session. Use it when the user turns to a session " +
				"(\"focus X\", \"switch to X\") with nothing to send.",
			InputSchema: object(map[string]any{
				"session_id": sessionRef,
			}, "session_id"),
		},
		{
			Name:        "send_to_session",
			Description: "Send the user's message to an existing session as its next turn." + deliver,
			InputSchema: object(map[string]any{
				"session_id": sessionRef,
				"text":       userWords,
			}, "session_id", "text"),
		},
		{
			Name: "read_session_transcript",
			Description: "Read a page of a session's transcript, oldest turn first: user and assistant text, " +
				"tool calls as [tool: name target] markers. With neither cursor, the latest page. " +
				"before pages backward: pass the returned before while more is true. " +
				"from starts at a search hit's cursor: pass the returned from to read on.",
			InputSchema: object(map[string]any{
				"session_id": sessionRef,
				"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 200},
				"before":     map[string]any{"type": "string"},
				"from":       map[string]any{"type": "string"},
			}, "session_id"),
		},
		{
			Name: "search_sessions",
			Description: "Search transcript text. With session_id, search that session; without it, find the sessions that match. " +
				"Each hit carries a cursor for read_session_transcript's from.",
			InputSchema: object(map[string]any{
				"session_id":    sessionRef,
				"query":         map[string]any{"type": "string"},
				"limit":         map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
				"context_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": 500},
			}, "query"),
		},
		{
			Name: "create_session",
			Description: "Create a session and send it its first message, for work no existing session fits. " +
				"Returns the new session id." + deliver,
			InputSchema: object(map[string]any{
				"cwd": map[string]any{"type": "string",
					"description": "The directory the user named, or the project the work belongs to. " +
						"Omit it for work tied to no project (it then runs in the temp directory); never a path you made up."},
				"initial_message": userWords,
				"agent": map[string]any{"type": "string",
					"description": "A named set of session defaults the user configured: backend, model, cwd and more. " +
						"Only when the user names one; an unknown name fails with the names there are. " +
						"cwd, backend, model and effort given alongside override the agent's."},
				"backend": map[string]any{"type": "string", "enum": []string{"claude", "codex", "pi"}, "description": onRequest},
				"model":   map[string]any{"type": "string", "description": onRequest},
				"effort":  map[string]any{"type": "string", "description": "Reasoning effort, such as low or high. " + onRequest},
			}, "initial_message"),
		},
		{
			Name: "list_pending_interactions",
			Description: "List the permission requests and questions sessions are waiting on. " +
				"Read-only: the user answers them in usher's UI.",
			InputSchema: object(map[string]any{}),
		},
		{
			Name:        "set_archived",
			Description: "Archive or unarchive a session.",
			InputSchema: object(map[string]any{
				"session_id": sessionRef,
				"archived":   map[string]any{"type": "boolean"},
			}, "session_id", "archived"),
		},
	}
	// Exempt every routing tool from Tool Search deferral.
	for i := range tools {
		tools[i].Meta = map[string]any{"anthropic/alwaysLoad": true}
	}
	return tools
}

// Call validates and dispatches one router tool call against usher's control
// socket. The returned value is JSON-compatible.
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
		out := transcriptPage{Turns: make([]transcriptTurn, len(page.Turns)), More: page.HasMore, From: next}
		for i, t := range page.Turns {
			out.Turns[i] = transcriptTurn{Role: t.Role, Content: turnText(t), Time: t.DisplayTime()}
		}
		if page.HasMore && len(page.Turns) > 0 {
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
		return api.SearchSessions(args.SessionID, args.Query,
			bounded(args.Limit, 20, 100), bounded(args.ContextChars, 120, 500))

	case "create_session":
		var args struct {
			Agent          string `json:"agent"`
			Cwd            string `json:"cwd"`
			InitialMessage string `json:"initial_message"`
			Backend        string `json:"backend"`
			Model          string `json:"model"`
			Effort         string `json:"effort"`
		}
		if err := decode(raw, &args); err != nil {
			return nil, err
		}
		if args.InitialMessage == "" {
			return nil, errors.New("initial_message is required")
		}
		out, err := api.RouteNewSession(pluginapi.CreateRequest{
			Agent: args.Agent, Cwd: args.Cwd, InitialMessage: args.InitialMessage,
			Backend: args.Backend, Model: args.Model, Effort: args.Effort,
		})
		if err != nil {
			return nil, err
		}
		return deliveredResult(out), nil

	case "list_pending_interactions":
		return api.ListPendingInteractions()

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

// deliveredResult is the ack for a send or create.
func deliveredResult(d pluginapi.Delivery) map[string]any {
	out := map[string]any{
		"status": "delivered", "session_id": d.SessionID, "relayed": d.Relayed,
	}
	if d.Relayed {
		out["note"] = "usher shows the session's reply to the user when it lands and tells you in a " +
			"[session <id> replied] note before your next message. Nothing more is needed from you: " +
			"do not wait, poll, or restate it."
	} else {
		out["note"] = "no router session is designated, so nothing will be reported back to you; " +
			"read_session_transcript is the only way to see the reply"
	}
	return out
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
		return pluginapi.SessionView{}, fmt.Errorf("no session matches %q: give the first 8 or more characters of its id, or a unique part of its title", query)
	}
	if len(matches) > 1 {
		return pluginapi.SessionView{}, fmt.Errorf("ambiguous: %d sessions match %q; give more of the id or title", len(matches), query)
	}
	return matches[0], nil
}
