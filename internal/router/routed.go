package router

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
)

// routingTool matches the tools that hand work to another session, under any
// backend's MCP naming.
var routingTool = regexp.MustCompile(`(^|__)(send_to_session|create_session)$`)

// RoutedRepliesFrom resolves the replies to the work session id routed
// elsewhere, in its turns from cursor from on (its newest page without one).
func (r *Router) RoutedRepliesFrom(id, from string) ([]core.RoutedReply, error) {
	format, path, err := r.transcriptOf(id)
	if err != nil {
		return nil, err
	}
	// Read unwired: a turn's Time must be when it began, which is when it
	// could first have dispatched anything — not when it ended.
	var turns []core.Turn
	if from != "" {
		turns, err = format.ReadFrom(path, from)
	} else {
		turns, _, err = format.ReadBefore(path, "", 50)
	}
	if err != nil {
		return nil, err
	}
	return r.RoutedReplies(turns), nil
}

// RoutedReplies resolves, for each routing tool call in turns, the reply it
// led to. Nothing records that link: the target's transcript holds the call's
// prompt as a user turn, and the reply is the turn that answers it. A turn's
// Time is taken as when it began.
func (r *Router) RoutedReplies(turns []core.Turn) []core.RoutedReply {
	out := []core.RoutedReply{}
	for _, t := range turns {
		if t.Role != "assistant" {
			continue
		}
		for i, p := range t.Parts {
			if p.Type != "tool" || !routingTool.MatchString(p.ToolName) {
				continue
			}
			target := jsonField(p.Content, "session_id")
			if target == "" {
				continue // a rejected call routed nothing
			}
			rr := core.RoutedReply{Turn: t.Cursor, Part: i, SessionID: target}
			if sess, ok := r.GetSession(target); ok {
				rr.Title, rr.Backend, rr.Status = sess.Title, sess.Backend, sess.Status
			}
			prompt := jsonField(p.ToolInput, "text")
			if prompt == "" {
				prompt = jsonField(p.ToolInput, "initial_message")
			}
			if prompt != "" {
				rr.Reply, rr.Time, rr.Done = r.replyTo(target, prompt, t.Time)
			}
			out = append(out, rr)
		}
	}
	return out
}

// routedSearchPages bounds how far back in the target a reply is looked for.
const routedSearchPages = 8

// replyTo finds the turn that answered prompt in session id: the one after the
// first user turn carrying that text since the dispatching turn began — the
// first, because the same text may have been sent again later. done is false
// while an answer is still to come or still being written.
func (r *Router) replyTo(id, prompt string, since time.Time) (reply string, at time.Time, done bool) {
	prompt = strings.TrimSpace(prompt)
	// Allow for a turn stamped as it was queued.
	since = since.Add(-5 * time.Second)
	var all []core.Turn // the target's turns back to since, oldest first
	before := ""
	for page := 0; page < routedSearchPages; page++ {
		turns, more, err := r.ReadTurns(id, before, 50)
		if err != nil || len(turns) == 0 {
			break
		}
		all = append(turns, all...)
		if !more || turns[0].Time.Before(since) {
			break
		}
		before = turns[0].Cursor
	}
	for i, t := range all {
		if t.Role != "user" || t.Time.Before(since) || strings.TrimSpace(t.Content) != prompt {
			continue
		}
		if i+1 == len(all) {
			break
		}
		switch next := all[i+1]; next.Role {
		case "assistant":
			return turnText(next), next.DisplayTime(), i+1 < len(all)-1 || !r.running(id)
		case "error":
			return next.Content, next.Time, true
		}
		break
	}
	// No answer yet; once the session has stopped there never will be one.
	return "", time.Time{}, !r.running(id)
}

func (r *Router) running(id string) bool {
	sess, ok := r.GetSession(id)
	return ok && (sess.Status == core.StatusRunning || sess.Status == core.StatusAwaitingPermission)
}

// turnText is an assistant turn's prose, tool calls left out.
func turnText(t core.Turn) string {
	var parts []string
	for _, p := range t.Parts {
		if p.Type == "text" && strings.TrimSpace(p.Content) != "" {
			parts = append(parts, p.Content)
		}
	}
	return strings.Join(parts, "\n\n")
}

// jsonField reads a string field from the first JSON object in s; tool input
// and output are JSON, but a backend may wrap them in other text.
func jsonField(s, key string) string {
	a, b := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if a < 0 || b <= a {
		return ""
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(s[a:b+1]), &m) != nil {
		return ""
	}
	var v string
	_ = json.Unmarshal(m[key], &v)
	return v
}
