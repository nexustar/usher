// Main-chat orchestration: the queue/worker pipeline behind POST /send,
// the model's derived history view (relay birth forms, summary anchoring,
// compaction), relay delivery (usher-initiated and foreign turns), the
// per-chat SSE stream, and the ground-truth state block. The mainchat store
// stays the display-layer truth; everything the model sees is derived here.
//
// Split out of server.go — same package, no behavior of its own beyond what
// the function comments state.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nexustar/usher/internal/agent/usheragent"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/mainchat"
	"github.com/nexustar/usher/internal/textutil"
)

// chatFrame is one SSE frame on /api/mainchats/{id}/events. Event is the SSE
// event name: "message" (Data carries a persisted message + optional focus)
// or "turn.done" (an agent turn finished — even one that displayed nothing —
// so the client can clear its thinking placeholder; Data is zero).
type chatFrame struct {
	Event string
	Data  chatEvent
}

// chatEvent is the "message" frame payload: a persisted message, plus the
// resolved focus when this message moved it.
type chatEvent struct {
	Message mainchat.Message `json:"message"`
	Focus   *focusDetail     `json:"focus,omitempty"`
}

func (s *Server) handleListMainChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.main.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if chats == nil {
		chats = []mainchat.Chat{}
	}
	writeJSON(w, http.StatusOK, chats)
}

func (s *Server) handleListMainChatMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	// Apply the public limit after internal tool events are filtered, so a
	// tool-heavy turn cannot make visible messages disappear from the page.
	msgs, err := s.main.Read(id, 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if msgs == nil {
		msgs = []mainchat.Message{}
	}
	visible := msgs[:0]
	for _, m := range msgs {
		if m.Role != "tool" {
			visible = append(visible, m)
		}
	}
	if limit > 0 && len(visible) > limit {
		visible = visible[len(visible)-limit:]
	}
	writeJSON(w, http.StatusOK, visible)
}

type focusDetail struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd,omitempty"`
	Title     string `json:"title,omitempty"`
}

type mainChatInfo struct {
	ID    string       `json:"id"`
	Focus *focusDetail `json:"focus,omitempty"`
}

const stateBlockRecentIdle = 10

// renderStateBlock produces a compact ground-truth dump of the router's
// current view of sessions + the active focus. We append it to the user's
// message every turn so trivia questions ("how many sessions?", "what's
// the focused cwd?", "is X running?") can be answered straight from
// context instead of hallucinated. Patterned after Hermes-Agent's
// per-turn state injection — kept off the system prompt so cache hits
// still happen on the static prefix.
func (s *Server) renderStateBlock(focusID string) string {
	allSessions := s.router.ListSessions()
	sessions := make([]core.Session, 0, len(allSessions))
	for _, sess := range allSessions {
		if sess.ID != focusID && s.router.IsArchived(sess.ID) {
			continue
		}
		sessions = append(sessions, sess)
	}
	pending := s.router.ListPendingInteractions()

	now := time.Now().UTC()

	var b strings.Builder
	b.WriteString("<current_state>\n")
	fmt.Fprintf(&b, "now: %s\n", now.Format(time.RFC3339))
	// The status legend heads off a costly misreading: "live" looks like
	// "still working" but only means the process is warm — background work
	// (workflows, subagents) runs invisibly under it, so status can never
	// answer "is the task done?". The transcript can.
	b.WriteString("status legend: running = a turn is executing | live = idle, accepts input (background work may still be in flight) | idle = no process. Status cannot tell whether a task finished — read the transcript tail for that.\n")
	fmt.Fprintf(&b, "session_count: %d\n", len(sessions))
	fmt.Fprintf(&b, "pending_permission_requests: %d\n", len(pending))
	if focusID != "" {
		if sess, ok := s.router.GetSession(focusID); ok {
			fmt.Fprintf(&b, "focus: %s (cwd %s, title %q)\n",
				focusID, sess.Cwd, textutil.Truncate(sess.Title, 60))
		} else {
			fmt.Fprintf(&b, "focus: %s (no longer in discovery)\n", focusID)
		}
	} else {
		b.WriteString("focus: (none yet)\n")
	}
	rows := stateBlockSessions(sessions, focusID)
	if len(sessions) > len(rows) {
		fmt.Fprintf(&b, "sessions shown: focus + all running/live/awaiting_permission + %d recent idle; unshown sessions remain available via tools\n", stateBlockRecentIdle)
	}
	b.WriteString("sessions (id  cwd  status  last_input  last_event  title):\n")
	for _, sess := range rows {
		mark := ""
		if sess.ID == focusID {
			mark = "  [FOCUS]"
		}
		// last_input = the user last talked to it; last_event = the
		// transcript last changed. Transcript movement after the last input
		// is the tell that background work produced something.
		fmt.Fprintf(&b, "  %s  %-30s  %-7s  %-9s  %-9s  %s%s\n",
			sess.ID,
			textutil.Truncate(sess.Cwd, 30),
			string(sess.Status),
			humanizeAge(now, sess.LastInputAt),
			humanizeAge(now, sess.LastEventAt),
			textutil.Truncate(sess.Title, 50),
			mark)
	}
	if len(sessions) > len(rows) {
		fmt.Fprintf(&b, "  … %d more not shown; absence above does not mean missing\n", len(sessions)-len(rows))
	}
	b.WriteString("</current_state>")
	return b.String()
}

// stateBlockSessions keeps the state preamble useful without feeding every
// historical session to the model: active sessions and the current focus are
// always present, plus a small recent-idle window. Input order is preserved
// (Router.ListSessions orders by latest user input).
func stateBlockSessions(sessions []core.Session, focusID string) []core.Session {
	rows := make([]core.Session, 0, stateBlockRecentIdle+1)
	idle := 0
	for _, sess := range sessions {
		active := sess.Status == core.StatusRunning ||
			sess.Status == core.StatusLive ||
			sess.Status == core.StatusAwaitingPermission
		focused := sess.ID == focusID
		if !active && !focused {
			if idle >= stateBlockRecentIdle {
				continue
			}
			idle++
		}
		rows = append(rows, sess)
	}
	return rows
}

// humanizeAge renders how long ago last happened relative to now as a compact
// phrase ("just now", "5m ago", "3h ago", "2d ago") for the state block, so
// the router can reason about recency (e.g. picking the most recently active
// session) without doing timestamp math on a small model. A zero or future
// last reads as "just now".
func humanizeAge(now, last time.Time) string {
	if last.IsZero() {
		return "unknown"
	}
	d := now.Sub(last)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// focusSwitchBanner returns a one-line clickable banner when `touched` (the
// session this turn acted on) differs from prevFocus, or "" if focus didn't
// change. The link is the SPA's session route, rendered in the chat bubble.
func focusSwitchBanner(prevFocus, touched, title string) string {
	if touched == "" || touched == prevFocus {
		return ""
	}
	verb := "Switching to"
	if prevFocus == "" {
		verb = "Routing to"
	}
	label := title
	if label == "" {
		label = textutil.ShortID(touched)
	}
	return fmt.Sprintf("↪ %s [%s](#/s/%s)\n\n", verb, label, touched)
}

func (s *Server) handleGetMainChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	msgs, err := s.main.Read(id, 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	info := mainChatInfo{ID: id, Focus: s.lastFocus(msgs)}
	writeJSON(w, http.StatusOK, info)
}

// lastFocus walks msgs newest-first and returns the most recent non-empty
// FocusSession decorated with current session metadata (cwd, title) if
// the session is still discoverable.
func (s *Server) lastFocus(msgs []mainchat.Message) *focusDetail {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].FocusSession == "" {
			continue
		}
		return s.focusDetailFor(msgs[i].FocusSession)
	}
	return nil
}

// agentTurnTimeout bounds one detached agent turn (the LLM tool-call loop).
// Session sends inside the turn no longer block (replies arrive via relay),
// so this only guards against a hung LLM backend.
const agentTurnTimeout = 10 * time.Minute

// maxQueuedChatTurns bounds one chat's turn queue. A full queue means the
// agent is far behind (or a client is retry-looping) — reject with 429 rather
// than stack unbounded turns that fire long after the user gave up.
const maxQueuedChatTurns = 8

// handleMainChatSend persists the user message (a failed persist is the
// client's 500, not a log line), queues the agent turn, and returns 202. The
// turn runs detached from the request — a locked phone or dropped tunnel
// can't kill it — and all resulting messages arrive over the SSE stream.
func (s *Server) handleMainChatSend(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	if err := mainchat.ValidateID(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Reserve the queue slot BEFORE persisting: a message rejected with 429
	// must leave no trace, or it would sit in the history as a ghost no
	// worker will ever process.
	if !s.tryReserveTurn(id) {
		writeErr(w, http.StatusTooManyRequests, "chat is busy (turn queue full); try again shortly")
		return
	}
	userMsg := mainchat.Message{Role: "user", Content: req.Text, Time: time.Now().UTC()}
	if err := s.appendChat(id, userMsg, nil); err != nil {
		s.releaseTurn(id)
		writeErr(w, http.StatusInternalServerError, "persist message: "+err.Error())
		return
	}
	s.chatQueue(id) <- userMsg // can't block: reservations gate the buffer
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// tryReserveTurn claims one of the chat's maxQueuedChatTurns queue slots;
// the claim guarantees the subsequent channel send cannot block or overflow.
// Released by the worker on dequeue.
func (s *Server) tryReserveTurn(chatID string) bool {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if s.chatPending[chatID] >= maxQueuedChatTurns {
		return false
	}
	s.chatPending[chatID]++
	return true
}

func (s *Server) releaseTurn(chatID string) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	if s.chatPending[chatID] <= 1 {
		delete(s.chatPending, chatID)
	} else {
		s.chatPending[chatID]--
	}
}

// chatQueue returns the chat's turn queue, lazily starting its single worker
// goroutine. One worker per chat = strict arrival-order turns; workers are
// tiny and live for the process (the set of chats is small).
func (s *Server) chatQueue(chatID string) chan mainchat.Message {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	q := s.chatQueues[chatID]
	if q == nil {
		q = make(chan mainchat.Message, maxQueuedChatTurns)
		s.chatQueues[chatID] = q
		go func() {
			for msg := range q {
				s.releaseTurn(chatID) // slot freed on dequeue, not turn end
				s.runMainChatTurn(chatID, msg)
				// Compaction runs between turns on this same worker: no
				// races with turns, and no user-visible latency unless the
				// user sends again within the summarization window.
				s.maybeCompactChat(chatID)
			}
		}()
	}
	return q
}

// runMainChatTurn executes one agent turn for an already-persisted user
// message. Turns for a chat run one at a time in arrival order, so each
// turn's history contains the previous turn's messages. Relayed session
// replies are the exception: they append whenever their session finishes.
func (s *Server) runMainChatTurn(chatID string, userMsg mainchat.Message) {
	// Always signal turn end — even a turn that displayed nothing — so the
	// client can clear its thinking placeholder.
	defer s.broadcastChat(chatID, chatFrame{Event: "turn.done"})

	prior, err := s.main.Read(chatID, 0)
	if err != nil {
		s.logger.Warn("main chat read", "chat", chatID, "err", err)
		return
	}
	// The store already holds userMsg (persisted at POST time). History is
	// everything EXCEPT this turn's own user message, which is passed
	// separately as the current message.
	for i := len(prior) - 1; i >= 0; i-- {
		m := prior[i]
		if m.Role == "user" && m.Content == userMsg.Content && m.Time.Equal(userMsg.Time) {
			prior = append(prior[:i:i], prior[i+1:]...)
			break
		}
	}
	prevFocus := ""
	if fd := s.lastFocus(prior); fd != nil {
		prevFocus = fd.SessionID
	}

	// Append a compact ground-truth block to the user message. The agent
	// (especially small models like Haiku / Flash / mini) uses it to answer
	// metadata trivia without hallucinating, and to verify focus before
	// claiming a switch.
	enrichedUserMsg := userMsg.Content + "\n\n" + s.renderStateBlock(prevFocus)

	type pendingRelay struct {
		sessionID string
		reply     string
		err       error
	}
	var relayMu sync.Mutex
	var queuedRelays []pendingRelay
	relaysReleased := false
	relay := func(sessionID, reply string, err error) {
		relayMu.Lock()
		if !relaysReleased {
			queuedRelays = append(queuedRelays, pendingRelay{sessionID: sessionID, reply: reply, err: err})
			relayMu.Unlock()
			return
		}
		relayMu.Unlock()
		s.relaySessionReply(chatID, sessionID, reply, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), agentTurnTimeout)
	defer cancel()
	res, err := s.agent.Handle(ctx, deriveChatHistory(prior), prevFocus, enrichedUserMsg, relay)
	if err != nil {
		s.logger.Warn("agent handle", "err", err)
		// Keep res.FocusSession: Handle returns the focus accumulated before
		// the failure, so routing that already happened isn't forgotten.
		res.Reply = "agent error: " + err.Error()
	}
	// Persist tool evidence before the final agent message, preserving the
	// order in which the model observed this turn. These records are internal
	// history and are filtered from the messages API.
	for _, event := range res.ToolEvents {
		if appendErr := s.main.Append(chatID, mainchat.Message{
			Role: "tool",
			Tool: &mainchat.ToolEvent{CallID: event.CallID, Name: event.Name, Arguments: event.Arguments, Result: event.Result},
		}); appendErr != nil {
			s.logger.Warn("main chat append tool", "chat", chatID, "tool", event.Name, "err", appendErr)
		}
	}
	// A very fast session can reply before Handle returns. Release relays only
	// after their triggering tool events are durable, so JSONL chronology
	// matches the causal order.
	relayMu.Lock()
	relaysReleased = true
	pending := queuedRelays
	queuedRelays = nil
	relayMu.Unlock()
	for _, completed := range pending {
		s.relaySessionReply(chatID, completed.sessionID, completed.reply, completed.err)
	}
	// Carry forward focus when this turn didn't touch any session.
	newFocus := res.FocusSession
	if newFocus == "" {
		newFocus = prevFocus
	}
	// Announce a focus change server-side (the model can't reliably detect a
	// switch itself): prepend a linked banner when this turn routed to a
	// session different from the prior focus.
	title := ""
	if sess, ok := s.router.GetSession(res.FocusSession); ok {
		title = sess.Title
	}
	content := focusSwitchBanner(prevFocus, res.FocusSession, title) + res.Reply
	if strings.TrimSpace(content) == "" {
		// Pure-passthrough turn: the agent said nothing and focus didn't
		// switch (a switch always yields a banner) — nothing to display.
		// The deferred turn.done still tells the client the turn is over.
		return
	}
	if err := s.appendChat(chatID, mainchat.Message{
		Role:         "agent",
		Content:      content,
		FocusSession: newFocus,
	}, s.focusDetailFor(newFocus)); err != nil {
		s.logger.Warn("main chat append agent", "chat", chatID, "err", err)
	}
}

// History derivation. The model's view of a chat is computed from the store
// on every turn, and its shape is designed around provider prefix caches:
// content, once derived at a position, never changes — relays get their form
// at birth, and the only rewrite point is a compaction (which pays one
// deliberate cache miss and then stays stable until the next one).
const (
	// Relay birth-form slider: a session reply ≤ relayVerbatimMax runes
	// enters the history verbatim; a larger one enters as a head+tail
	// excerpt with a transcript pointer. 0 = excerpt everything; the
	// display/store always keeps the full text either way.
	relayVerbatimMax = 2048
	relayExcerptHead = 800
	relayExcerptTail = 400

	// historyBudgetRunes triggers compaction after a turn; the hard cap
	// bounds the derivation by front-trimming when compaction is
	// unavailable or failing (cache-hostile, correctness backstop only).
	historyBudgetRunes  = 16 * 1024
	historyHardCapRunes = 24 * 1024

	// compactKeepRunes of recent history stay verbatim through a
	// compaction; everything older folds into the summary.
	compactKeepRunes = 6 * 1024

	summarizeTimeout = 2 * time.Minute
)

// deriveChatHistory maps persisted messages to the model's history: anchored
// at the last summary (which stands in for everything it covered), each
// message in its immutable derived form, front-trimmed only at the hard cap.
func deriveChatHistory(msgs []mainchat.Message) []usheragent.HistoryMessage {
	msgs = sinceLastSummary(msgs)
	out := make([]usheragent.HistoryMessage, 0, len(msgs))
	total := 0
	for _, m := range msgs {
		h := deriveHistoryMessage(m)
		if h.Role == "" && h.Tool == nil {
			continue
		}
		out = append(out, h)
		total += historyMessageRunes(h)
	}
	for len(out) > 1 && total > historyHardCapRunes {
		total -= historyMessageRunes(out[0])
		out = out[1:]
	}
	return out
}

func historyMessageRunes(h usheragent.HistoryMessage) int {
	if h.Tool != nil {
		return utf8.RuneCountInString(h.Tool.Name) + utf8.RuneCountInString(h.Tool.Arguments) + utf8.RuneCountInString(h.Tool.Result) + 16
	}
	return utf8.RuneCountInString(h.Content)
}

// sinceLastSummary returns the summary (first) plus every message it does
// not cover. The kept-verbatim tail of a compaction predates the summary in
// the append-only store, so coverage is by CoveredThrough timestamp, not
// position.
func sinceLastSummary(msgs []mainchat.Message) []mainchat.Message {
	si := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "summary" {
			si = i
			break
		}
	}
	if si < 0 {
		return msgs
	}
	covered := msgs[si].CoveredThrough
	out := []mainchat.Message{msgs[si]}
	for i, m := range msgs {
		if i == si || m.Role == "summary" {
			continue // older summaries are themselves folded into the newest
		}
		if !m.Time.After(covered) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// deriveHistoryMessage is the immutable model-view form of one message.
// Relays and summaries become user-role observations tagged with their
// nature — information shown to the user, not the agent's own words.
func deriveHistoryMessage(m mainchat.Message) usheragent.HistoryMessage {
	switch m.Role {
	case "tool":
		if m.Tool == nil {
			return usheragent.HistoryMessage{}
		}
		return usheragent.HistoryMessage{Role: "tool", Tool: &usheragent.ToolEvent{
			CallID: m.Tool.CallID, Name: m.Tool.Name, Arguments: m.Tool.Arguments, Result: m.Tool.Result,
		}}
	case "relay":
		sid := textutil.ShortID(m.SourceSession)
		return usheragent.HistoryMessage{Role: "user", Content: usheragent.RelayTag(sid) + relayBirthForm(m.Content, sid)}
	case "summary":
		return usheragent.HistoryMessage{Role: "user", Content: usheragent.SummaryTag + m.Content}
	default:
		return usheragent.HistoryMessage{Role: m.Role, Content: m.Content}
	}
}

// relayBirthForm renders a session reply for the model: verbatim when small,
// head+tail excerpt with a recovery pointer when large. Long replies usually
// carry their TLDR up front and their conclusion at the end; the middle is
// one read_session_transcript call away, at full fidelity.
func relayBirthForm(reply, sid string) string {
	r := []rune(reply)
	if len(r) <= relayVerbatimMax {
		return reply
	}
	omitted := len(r) - relayExcerptHead - relayExcerptTail
	return string(r[:relayExcerptHead]) +
		fmt.Sprintf("\n[… %d chars omitted — read_session_transcript(%s) for the full reply]\n", omitted, sid) +
		string(r[len(r)-relayExcerptTail:])
}

// maybeCompactChat folds the chat's older history into a summary message
// once the derived view exceeds its budget. Only agents implementing
// HistorySummarizer compact (the rule agent ignores history anyway); failure
// is silent — the derivation's hard cap keeps prompts bounded until the next
// attempt. The summary is appended (the store stays append-only) with
// CoveredThrough marking the fold point, and is itself a broadcast message,
// so clients render the compaction marker live.
func (s *Server) maybeCompactChat(chatID string) {
	summarizer, ok := s.agent.(usheragent.HistorySummarizer)
	if !ok {
		return
	}
	msgs, err := s.main.Read(chatID, 0)
	if err != nil {
		return
	}
	msgs = sinceLastSummary(msgs)
	valid := msgs[:0]
	for _, m := range msgs {
		if m.Role != "tool" || m.Tool != nil {
			valid = append(valid, m)
		}
	}
	msgs = valid

	derived := make([]usheragent.HistoryMessage, len(msgs))
	sizes := make([]int, len(msgs))
	total := 0
	for i, m := range msgs {
		derived[i] = deriveHistoryMessage(m)
		sizes[i] = historyMessageRunes(derived[i])
		total += sizes[i]
	}
	if total <= historyBudgetRunes {
		return
	}

	// Fold everything except a recent tail of ~compactKeepRunes.
	cut := len(msgs)
	kept := 0
	for cut > 0 && kept < compactKeepRunes {
		cut--
		kept += sizes[cut]
	}
	if cut < 2 {
		return // one giant message; folding it buys nothing
	}

	ctx, cancel := context.WithTimeout(context.Background(), summarizeTimeout)
	defer cancel()
	text, err := summarizer.SummarizeHistory(ctx, derived[:cut])
	if err != nil {
		s.logger.Warn("chat compaction", "chat", chatID, "err", err)
		return
	}
	if err := s.appendChat(chatID, mainchat.Message{
		Role:           "summary",
		Content:        text,
		CoveredThrough: msgs[cut-1].Time,
	}, nil); err != nil {
		s.logger.Warn("chat compaction append", "chat", chatID, "err", err)
	}
}

// followScanWindow bounds how far back a chat's history is scanned when
// deciding whether it follows a session: reference the session within the
// last N messages and foreign turns keep flowing in; go quiet for long
// enough and the mirror stops.
const followScanWindow = 100

// RelayForeignTurn delivers a turn usher did NOT initiate — a background
// workflow continuation, a prompt typed straight into the tmux pane — to
// every chat that recently routed to the session. Follows are derived from
// the chat histories themselves (FocusSession/SourceSession references), so
// there is no registry to persist and restarts lose nothing. Wired as the
// router's ForeignTurnHandler.
func (s *Server) RelayForeignTurn(sessionID, text string) {
	chats, err := s.main.List()
	if err != nil {
		return
	}
	for _, c := range chats {
		msgs, err := s.main.Read(c.ID, followScanWindow)
		if err != nil {
			continue
		}
		if !referencesSession(msgs, sessionID) {
			continue
		}
		s.relaySessionReply(c.ID, sessionID, text, nil)
	}
}

func referencesSession(msgs []mainchat.Message, sessionID string) bool {
	for _, m := range msgs {
		if m.FocusSession == sessionID || m.SourceSession == sessionID {
			return true
		}
	}
	return false
}

// relaySessionReply appends a session's completed reply to the chat verbatim.
// This is the display path for session output — the agent never restates it.
func (s *Server) relaySessionReply(chatID, sessionID, reply string, err error) {
	// The reply is stored verbatim — surrounding whitespace can be meaning
	// (code fences, terminal output); trimming is only for the empty check.
	content := reply
	if strings.TrimSpace(content) == "" {
		content = "(no text response)"
	}
	if err != nil {
		content += "\n\n(relay: " + err.Error() + ")"
	}
	if aerr := s.appendChat(chatID, mainchat.Message{
		Role:          "relay",
		Content:       content,
		SourceSession: sessionID,
	}, nil); aerr != nil {
		s.logger.Warn("main chat append relay", "chat", chatID, "session", sessionID, "err", aerr)
	}
}

func (s *Server) focusDetailFor(sessionID string) *focusDetail {
	if sessionID == "" {
		return nil
	}
	fd := &focusDetail{SessionID: sessionID}
	if sess, ok := s.router.GetSession(sessionID); ok {
		fd.Cwd = sess.Cwd
		fd.Title = sess.Title
	}
	return fd
}

// appendChat persists msg (the returned error is the caller's to surface),
// then broadcasts it (with the focus it moved, if any) to the chat's SSE
// subscribers. A failed persist is NOT broadcast — showing a message that
// isn't stored would desync the UI from the next turn's history.
func (s *Server) appendChat(chatID string, msg mainchat.Message, focus *focusDetail) error {
	if msg.Time.IsZero() {
		msg.Time = time.Now().UTC()
	}
	if err := s.main.Append(chatID, msg); err != nil {
		return err
	}
	s.broadcastChat(chatID, chatFrame{Event: "message", Data: chatEvent{Message: msg, Focus: focus}})
	return nil
}

// broadcastChat fans one frame out to the chat's SSE subscribers. A full
// subscriber is force-closed instead of skipped: a silent drop would leave
// the stream healthy-looking but missing messages, with nothing ever
// triggering the client's reconnect-refetch recovery.
func (s *Server) broadcastChat(chatID string, frame chatFrame) {
	var evict []func()
	s.chatMu.Lock()
	for ch, cancel := range s.chatSubs[chatID] {
		select {
		case ch <- frame:
		default:
			evict = append(evict, cancel)
		}
	}
	s.chatMu.Unlock()
	// cancel takes chatMu; run evictions after unlocking.
	for _, cancel := range evict {
		cancel()
	}
}

func (s *Server) subscribeChat(chatID string) (<-chan chatFrame, func()) {
	ch := make(chan chatFrame, 16)
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.chatMu.Lock()
			delete(s.chatSubs[chatID], ch)
			if len(s.chatSubs[chatID]) == 0 {
				delete(s.chatSubs, chatID)
			}
			s.chatMu.Unlock()
			close(ch)
		})
	}
	s.chatMu.Lock()
	if s.chatSubs[chatID] == nil {
		s.chatSubs[chatID] = map[chan chatFrame]func(){}
	}
	s.chatSubs[chatID][ch] = cancel
	s.chatMu.Unlock()
	return ch, cancel
}

// handleMainChatEvents streams a chat's frames as SSE: "message" events and
// "turn.done" markers. No replay; instead the subscription registers BEFORE
// the response headers flush and the client refetches history on every open —
// so anything after that fetch arrives either in it or on this stream.
func (s *Server) handleMainChatEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := mainchat.ValidateID(id); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Subscribe BEFORE the preamble flushes: once the client sees the stream
	// open, everything after its history fetch is guaranteed to arrive.
	ch, cancel := s.subscribeChat(id)
	defer cancel()

	flusher, ok := sseStart(w)
	if !ok {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case frame, ok := <-ch:
			if !ok {
				// Force-closed as a slow subscriber: end the response so the
				// EventSource reconnects and refetches.
				return
			}
			payload := []byte("{}")
			if frame.Event == "message" {
				b, err := json.Marshal(frame.Data)
				if err != nil {
					continue
				}
				payload = b
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.Event, payload); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
