package usheragent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// RuleAgent is the v0.1 main-chat agent: a small dispatcher over slash
// commands. v0.2 introduced LLMAgent (provider-agnostic OpenAI-compatible)
// behind the same Agent interface.
type RuleAgent struct {
	api AgentAPI
}

func NewRule(api AgentAPI) *RuleAgent { return &RuleAgent{api: api} }

const helpText = `commands:
  /list                          list all coding-agent sessions
  /focus <prefix>                switch focus without sending or reading
  /send <prefix> <text>          send <text>; the session's reply is relayed here when done
  /read <prefix> [n] [offset]    show n turns of a session (default 20), from offset (default: most recent)
  /search <query>                find sessions whose transcripts contain query
  /search --session <prefix> <query>
                                 find matching turns inside one session
  /new <cwd> <text>              start a new session in <cwd> with an initial message
  /pending                       list permission requests waiting for a decision
  /approve <interaction-prefix>  allow a pending permission request
  /deny <interaction-prefix>     deny a pending permission request
  /archive <prefix>              hide a session from the default list
  /unarchive <prefix>            restore an archived session
  /auto-approve <prefix> on|off  toggle auto-approving the session's prompts
  /help                          show this help`

// Handle implements Agent. The rule agent ignores history and currentFocus —
// every command is parsed in isolation. /send sets FocusSession in the
// returned result so the server can carry it forward, matching the contract
// LLMAgent uses.
func (a *RuleAgent) Handle(ctx context.Context, _ []HistoryMessage, _ string, userMsg string, relay RelaySink) (AgentResult, error) {
	msg := strings.TrimSpace(userMsg)
	if msg == "" {
		return AgentResult{}, nil
	}

	cmd, rest := splitOnce(msg)
	var reply, focus string

	switch cmd {
	case "/help":
		reply = helpText
	case "/list":
		reply = a.list()
	case "/focus":
		reply, focus = a.focus(rest)
	case "/send":
		reply, focus = a.send(rest, relay)
	case "/read":
		reply, focus = a.read(rest)
	case "/search":
		reply, focus = a.search(rest)
	case "/new":
		reply, focus = a.create(ctx, rest)
	case "/pending":
		reply = a.pending()
	case "/approve":
		reply = a.respond(rest, "allow")
	case "/deny":
		reply = a.respond(rest, "deny")
	case "/archive":
		reply = a.setArchived(rest, true)
	case "/unarchive":
		reply = a.setArchived(rest, false)
	case "/auto-approve", "/autoapprove":
		reply = a.autoApprove(rest)
	default:
		if strings.HasPrefix(cmd, "/") {
			reply = "unknown command: " + cmd + ". Try /help."
		} else {
			reply = "natural-language routing isn't implemented in v0.1; try /help for available commands."
		}
	}
	return AgentResult{Reply: reply, FocusSession: focus}, nil
}

func (a *RuleAgent) list() string {
	sessions := a.api.ListSessions()
	if len(sessions) == 0 {
		return "no sessions"
	}
	var lines []string
	for _, s := range sessions {
		var flags []string
		if a.api.IsAutoApprove(s.ID) {
			flags = append(flags, "auto-approve")
		}
		if a.api.IsArchived(s.ID) {
			flags = append(flags, "archived")
		}
		suffix := ""
		if len(flags) > 0 {
			suffix = "  [" + strings.Join(flags, ",") + "]"
		}
		lines = append(lines, fmt.Sprintf("%s  %s  %s%s",
			shortID(s.ID), padRight(truncate(titleOr(s), 40), 40), s.Cwd, suffix))
	}
	return strings.Join(lines, "\n")
}

// resolveSession matches sessions by id-prefix or title substring and
// requires exactly one hit. The second return is a user-facing error
// message (empty on success); callers bail when it's non-empty.
func (a *RuleAgent) resolveSession(prefix string) (core.Session, string) {
	matches := matchSessions(a.api.ListSessions(), prefix)
	if len(matches) == 0 {
		return core.Session{}, "no sessions match: " + prefix
	}
	if len(matches) > 1 {
		var lines []string
		for _, m := range matches {
			lines = append(lines, fmt.Sprintf("  %s  %s", shortID(m.ID), titleOr(m)))
		}
		return core.Session{}, "ambiguous; matches:\n" + strings.Join(lines, "\n")
	}
	return matches[0], ""
}

func (a *RuleAgent) focus(prefix string) (string, string) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "usage: /focus <session-prefix>", ""
	}
	sess, errMsg := a.resolveSession(prefix)
	if errMsg != "" {
		return errMsg, ""
	}
	return fmt.Sprintf("focused %s (%s)", shortID(sess.ID), titleOr(sess)), sess.ID
}

func (a *RuleAgent) send(args string, relay RelaySink) (string, string) {
	prefix, text := splitOnce(args)
	text = strings.TrimSpace(text)
	if prefix == "" || text == "" {
		return "usage: /send <session-prefix> <text>", ""
	}
	sess, errMsg := a.resolveSession(prefix)
	if errMsg != "" {
		return errMsg, ""
	}
	if relay == nil {
		if err := a.api.SendToSession(sess.ID, text); err != nil {
			return "send failed: " + err.Error(), ""
		}
		return fmt.Sprintf("sent to %s (%s)", shortID(sess.ID), titleOr(sess)), sess.ID
	}
	if err := a.api.SendToSessionRelayed(sess.ID, text, relay); err != nil {
		return "send failed: " + err.Error(), ""
	}
	return fmt.Sprintf("sent to %s (%s)", shortID(sess.ID), titleOr(sess)), sess.ID
}

func (a *RuleAgent) read(args string) (string, string) {
	prefix, rest := splitOnce(args)
	if prefix == "" {
		return "usage: /read <session-prefix> [n] [offset]", ""
	}
	limit := defaultReadTurns
	offset := -1 // most recent page
	if rest != "" {
		nStr, offStr := splitOnce(rest)
		if n, err := strconv.Atoi(strings.TrimSpace(nStr)); err == nil && n > 0 {
			limit = n
			if limit > maxReadTurns {
				limit = maxReadTurns
			}
		}
		if offStr != "" {
			if o, err := strconv.Atoi(strings.TrimSpace(offStr)); err == nil && o >= 0 {
				offset = o
			}
		}
	}
	sess, errMsg := a.resolveSession(prefix)
	if errMsg != "" {
		return errMsg, ""
	}
	turns, start, total, err := a.api.ReadSessionTranscriptPage(sess.ID, offset, limit)
	if err != nil {
		return "read failed: " + err.Error(), ""
	}
	if len(turns) == 0 {
		return "(empty transcript)", sess.ID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "turns %d-%d of %d\n", start, start+len(turns)-1, total)
	for _, tn := range turns {
		fmt.Fprintf(&b, "%s: %s\n", tn.Role, truncate(strings.TrimSpace(tn.Content), 500))
	}
	return strings.TrimRight(b.String(), "\n"), sess.ID
}

func (a *RuleAgent) search(args string) (string, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return "usage: /search <query> | /search --session <session-prefix> <query>", ""
	}
	if args == "--session" || strings.HasPrefix(args, "--session ") {
		prefix, query := splitOnce(strings.TrimSpace(strings.TrimPrefix(args, "--session")))
		query = strings.TrimSpace(query)
		if prefix == "" || query == "" {
			return "usage: /search --session <session-prefix> <query>", ""
		}
		sess, errMsg := a.resolveSession(prefix)
		if errMsg != "" {
			return errMsg, ""
		}
		hits, truncated, err := a.api.SearchSessionTranscript(sess.ID, query, defaultSearchHits, defaultSearchContext)
		if err != nil {
			return "search failed: " + err.Error(), ""
		}
		if len(hits) == 0 {
			return "no matches", sess.ID
		}
		var b strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&b, "#%d %s: %s\n", h.TurnIndex, h.Role, truncate(strings.TrimSpace(h.Snippet), 500))
		}
		if truncated {
			b.WriteString("(more matches not shown)")
		}
		return strings.TrimRight(b.String(), "\n"), sess.ID
	}
	results, truncated, err := a.api.SearchAllSessions(args, defaultSearchSessions, defaultSearchContext)
	if err != nil {
		return "search failed: " + err.Error(), ""
	}
	if len(results) == 0 {
		return "no matches", ""
	}
	var b strings.Builder
	for _, result := range results {
		fmt.Fprintf(&b, "%s  %s  (%d hits)\n", shortID(result.SessionID), truncate(result.Title, 50), result.HitCount)
	}
	if truncated {
		b.WriteString("(more matches not shown)")
	}
	return strings.TrimRight(b.String(), "\n"), ""
}

func (a *RuleAgent) create(ctx context.Context, args string) (string, string) {
	cwd, initial := splitOnce(args)
	initial = strings.TrimSpace(initial)
	if cwd == "" || initial == "" {
		return "usage: /new <cwd> <initial message>", ""
	}
	newID, reply, err := a.api.CreateSession(ctx,
		core.CreateOptions{Cwd: cwd, InitialMessage: initial}, defaultCreateTimeout)
	if err != nil {
		if newID != "" {
			return fmt.Sprintf("created %s but: %s", shortID(newID), err.Error()), newID
		}
		return "create failed: " + err.Error(), ""
	}
	if strings.TrimSpace(reply) == "" {
		reply = "(no text response)"
	}
	return fmt.Sprintf("created session %s\n%s", shortID(newID), reply), newID
}

func (a *RuleAgent) setArchived(prefix string, archived bool) string {
	prefix = strings.TrimSpace(prefix)
	verb := "archive"
	if !archived {
		verb = "unarchive"
	}
	if prefix == "" {
		return "usage: /" + verb + " <session-prefix>"
	}
	sess, errMsg := a.resolveSession(prefix)
	if errMsg != "" {
		return errMsg
	}
	if archived {
		a.api.Archive(sess.ID)
		return fmt.Sprintf("archived %s (%s)", shortID(sess.ID), titleOr(sess))
	}
	a.api.Unarchive(sess.ID)
	return fmt.Sprintf("unarchived %s (%s)", shortID(sess.ID), titleOr(sess))
}

func (a *RuleAgent) autoApprove(args string) string {
	prefix, mode := splitOnce(args)
	mode = strings.ToLower(strings.TrimSpace(mode))
	if prefix == "" || (mode != "on" && mode != "off") {
		return "usage: /auto-approve <session-prefix> on|off"
	}
	sess, errMsg := a.resolveSession(prefix)
	if errMsg != "" {
		return errMsg
	}
	enabled := mode == "on"
	a.api.SetAutoApprove(sess.ID, enabled)
	return fmt.Sprintf("auto-approve %s for %s (%s)", mode, shortID(sess.ID), titleOr(sess))
}

func (a *RuleAgent) pending() string {
	list := a.api.ListPendingInteractions()
	if len(list) == 0 {
		return "no pending interactions"
	}
	var lines []string
	for _, p := range list {
		lines = append(lines, fmt.Sprintf("%s  %s  in session %s",
			shortID(p.ID), p.ToolName, shortID(p.SessionID)))
	}
	return strings.Join(lines, "\n")
}

func (a *RuleAgent) respond(prefix, behavior string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return fmt.Sprintf("usage: /%s <interaction-id-prefix>", behavior)
	}
	list := a.api.ListPendingInteractions()
	var matches []hook.Pending
	for _, p := range list {
		if strings.HasPrefix(p.ID, prefix) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		return "no pending interaction matches: " + prefix
	}
	if len(matches) > 1 {
		return "ambiguous; multiple matches"
	}
	err := a.api.RespondInteraction(matches[0].ID, hook.Response{Behavior: behavior, Reason: "via main chat"})
	if err != nil {
		return "failed: " + err.Error()
	}
	verb := "allowed"
	if behavior == "deny" {
		verb = "denied"
	}
	return fmt.Sprintf("%s %s", verb, shortID(matches[0].ID))
}

// --- helpers -------------------------------------------------------------

func splitOnce(s string) (string, string) {
	parts := strings.SplitN(s, " ", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.TrimSpace(parts[1])
}

func matchSessions(all []core.Session, q string) []core.Session {
	qLower := strings.ToLower(q)
	var out []core.Session
	for _, s := range all {
		if strings.HasPrefix(s.ID, q) || strings.Contains(strings.ToLower(s.Title), qLower) {
			out = append(out, s)
		}
	}
	return out
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func titleOr(s core.Session) string {
	if s.Title == "" {
		return "(untitled)"
	}
	return s.Title
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func padRight(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(r))
}
