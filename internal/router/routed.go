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

// RoutedReplies resolves, for each routing tool call in turns, the reply it
// led to in the target session. Nothing records that link: the call's prompt is
// in the caller's transcript and the target's transcript holds the same text
// as a user turn, so the reply is the turn that answers it.
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
				rr.Title = sess.Title
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
// first user turn carrying that text since the dispatching turn began. done is
// false while that reply is still being written.
func (r *Router) replyTo(id, prompt string, since time.Time) (reply string, at time.Time, done bool) {
	prompt = strings.TrimSpace(prompt)
	// The dispatching turn's clock and the target's are the same machine's, but
	// allow for a turn stamped as it was queued.
	since = since.Add(-5 * time.Second)
	var newer []core.Turn // turns after the page being scanned, oldest first
	before := ""
	for page := 0; page < routedSearchPages; page++ {
		turns, more, err := r.ReadTurns(id, before, 50)
		if err != nil || len(turns) == 0 {
			return "", time.Time{}, false
		}
		all := append(turns, newer...)
		match := -1
		for i := range turns {
			t := turns[i]
			if t.Role == "user" && !t.Time.Before(since) && strings.TrimSpace(t.Content) == prompt {
				match = i
				break
			}
		}
		if match >= 0 && match+1 < len(all) {
			next := all[match+1]
			last := match+1 == len(all)-1
			switch next.Role {
			case "assistant":
				done = !last || !r.running(id)
				return turnText(next), next.DisplayTime(), done
			case "error":
				return next.Content, next.Time, true
			}
			return "", time.Time{}, false
		}
		if match >= 0 || !more || turns[0].Time.Before(since) {
			return "", time.Time{}, false
		}
		newer, before = all, turns[0].Cursor
	}
	return "", time.Time{}, false
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
