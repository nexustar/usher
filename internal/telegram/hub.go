package telegram

import (
	"context"
	"errors"
	"html"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/pathutil"
)

// RouterAPI is the strict subset of router.Router the hub consumes — the same
// boundary discipline as the main-chat agent's AgentAPI.
type RouterAPI interface {
	GetSession(id string) (core.Session, bool)
	SubscribeAllSessions() (<-chan broker.Event, func())
	SendToSession(id, text string) error
	SubscribePendingInteractions() (<-chan hook.Pending, func())
	RespondInteraction(id string, resp hook.Response) error
}

// Config configures a Hub. The bot token is baked into the client passed to
// NewHub, so it isn't here.
type Config struct {
	GroupID   int64  // the forum supergroup chat id usher mirrors into
	StatePath string // session→topic map file; "" = in-memory (tests)
	// AllowedUserIDs whitelists user ids that may drive sessions; empty = any
	// member of GroupID (the private group is then the trust boundary).
	AllowedUserIDs []int64
}

// longPollTimeout is the server-side hold (seconds) for getUpdates.
const longPollTimeout = 50

// promptCaption labels an echoed prompt; the "↑" points at the quoted block.
const promptCaption = "↑ mirrored user input"

// ackReaction is the emoji usher reacts with on an inbound message once it has
// been handed to the session — a no-extra-message "received, working" marker.
const ackReaction = "👀"

// maxPhotoBytes is Telegram's sendPhoto size cap (~10 MB); larger files are
// rejected, so usher doesn't bother uploading them.
const maxPhotoBytes = 10 << 20

// Hub mirrors usher's Claude Code sessions into a Telegram forum supergroup,
// one topic per session. It is a peer frontend to the web server: it consumes
// the Router and the global event stream, owning no Claude processes itself.
type Hub struct {
	client  *Client
	router  RouterAPI
	store   *topicStore
	group   int64
	allowed map[int64]bool // empty = any group member allowed
	logger  *slog.Logger

	createMu sync.Mutex // serializes lazy topic creation (see topicFor)

	// AskUserQuestion prompts awaiting an answer — by pending id, and by topic
	// thread so a typed reply can resolve one.
	askMu        sync.Mutex
	asks         map[string]askEntry
	asksByThread map[int64]string

	// recentSent: last prompt forwarded FROM Telegram per session, so the
	// prompt-echo skips it (else the user's own message mirrors back twice).
	recentMu   sync.Mutex
	recentSent map[string]string
}

// NewHub builds a Hub. The topic-mapping store is loaded from cfg.StatePath
// (re-adopting existing topics across restarts).
func NewHub(client *Client, router RouterAPI, cfg Config, logger *slog.Logger) (*Hub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store, err := newTopicStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	allowed := make(map[int64]bool, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allowed[id] = true
	}
	return &Hub{
		client:       client,
		router:       router,
		store:        store,
		group:        cfg.GroupID,
		allowed:      allowed,
		logger:       logger,
		asks:         map[string]askEntry{},
		asksByThread: map[int64]string{},
		recentSent:   map[string]string{},
	}, nil
}

// Run validates the token and runs the hub's loops (dispatch, poll, permission,
// reconcile) until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) error {
	me, err := h.client.GetMe(ctx)
	if err != nil {
		return err
	}
	h.logger.Info("telegram hub started", "bot", me.Username, "group", h.group)
	if len(h.allowed) == 0 {
		h.logger.Warn("telegram: no --telegram-allowed-user-ids set; any member of the group can drive sessions")
	}

	go h.pollLoop(ctx)
	go h.permissionLoop(ctx)
	go h.reconcileLoop(ctx)

	return h.dispatchLoop(ctx)
}

// sessionQueueSize bounds each session's mirror backlog. A turn produces only a
// handful of events, so this absorbs normal bursts; overflow (a worker wedged
// in a long 429 backoff) is dropped for that session alone.
const sessionQueueSize = 64

// dispatchLoop fans the global event stream out to one worker goroutine per
// session. Network I/O (and any 429 backoff) happens in the per-session worker,
// so a slow or rate-limited topic only backs up its own queue — it never blocks
// this consumer, which would otherwise let the broker drop events for EVERY
// session. The workers map is owned solely by this goroutine (no lock).
func (h *Hub) dispatchLoop(ctx context.Context) error {
	events, cancel := h.router.SubscribeAllSessions()
	defer cancel()

	workers := map[string]chan broker.Event{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			ch := workers[ev.SessionID]
			if ch == nil {
				ch = make(chan broker.Event, sessionQueueSize)
				workers[ev.SessionID] = ch
				go h.sessionWorker(ctx, ch)
			}
			select {
			case ch <- ev:
			default:
				h.logger.Warn("telegram: mirror queue full, dropping event",
					"session", ev.SessionID, "type", ev.Type)
			}
		}
	}
}

// sessionWorker mirrors one session's events serially (preserving order within
// the topic) until ctx is done. One goroutine lives per session seen this run —
// cheap for a single-user tool; not reaped on idle.
func (h *Hub) sessionWorker(ctx context.Context, ch <-chan broker.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			h.handleEvent(ctx, ev)
		}
	}
}

// pollLoop long-polls getUpdates and dispatches each update until ctx is done.
// Transient errors back off briefly and retry; the offset advances past every
// update returned so none is reprocessed.
func (h *Hub) pollLoop(ctx context.Context) {
	var offset int64
	for {
		if ctx.Err() != nil {
			return
		}
		pollCtx, cancel := PollContext(ctx, longPollTimeout)
		updates, err := h.client.GetUpdates(pollCtx, offset, longPollTimeout)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			h.logger.Warn("telegram: getUpdates", "err", err)
			if !sleepCtx(ctx, 3*time.Second) {
				return
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			h.handleUpdate(ctx, u)
		}
	}
}

const reconcileInterval = 5 * time.Minute

// reconcileLoop closes the topic of any session that no longer exists on disk
// (at startup and on a ticker). Archived sessions keep their topic — a revived
// one reuses it.
func (h *Hub) reconcileLoop(ctx context.Context) {
	h.reconcile(ctx)
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.reconcile(ctx)
		}
	}
}

func (h *Hub) reconcile(ctx context.Context) {
	for sessionID, thread := range h.store.all() {
		if ctx.Err() != nil {
			return
		}
		// Keep the topic while the session exists (even if archived); only a
		// deleted session closes its topic.
		if _, exists := h.router.GetSession(sessionID); exists {
			continue
		}
		if err := h.client.CloseForumTopic(ctx, h.group, thread); err != nil {
			h.logger.Warn("telegram: close topic", "session", sessionID, "thread", thread, "err", err)
		}
		// Drop the mapping regardless: if the close failed because the topic was
		// already closed/deleted, retrying forever helps no one.
		if err := h.store.delete(sessionID); err != nil {
			h.logger.Warn("telegram: drop topic mapping", "session", sessionID, "err", err)
		}
		h.logger.Info("telegram: closed topic for deleted session", "session", sessionID, "thread", thread)
	}
}

// handleUpdate dispatches one update to the message or callback-query path.
func (h *Hub) handleUpdate(ctx context.Context, u Update) {
	switch {
	case u.Message != nil:
		h.handleInbound(ctx, u.Message)
	case u.CallbackQuery != nil:
		h.handleCallback(ctx, u.CallbackQuery)
	}
}

// permissionLoop posts each new permission request as an allow/deny prompt in
// the originating session's topic, until ctx is done.
func (h *Hub) permissionLoop(ctx context.Context) {
	pending, cancel := h.router.SubscribePendingInteractions()
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-pending:
			if !ok {
				return
			}
			h.postPermission(ctx, p)
		}
	}
}

// postPermission posts a pending interaction into its session's topic as
// allow / deny / allow-for-session buttons (lazily creating the topic).
// AskUserQuestion gets its own option prompt instead.
func (h *Hub) postPermission(ctx context.Context, p hook.Pending) {
	thread, err := h.topicFor(ctx, p.SessionID)
	if err != nil {
		h.logger.Warn("telegram: permission topic", "session", p.SessionID, "err", err)
		return
	}
	// AskUserQuestion needs its options as buttons; a plain "allow" without a
	// chosen answer would just block on the pane TUI selector.
	if p.ToolName == "AskUserQuestion" {
		h.postAskQuestion(ctx, thread, p)
		return
	}
	if _, err := h.client.SendMessage(ctx, SendMessageParams{
		ChatID:          h.group,
		MessageThreadID: thread,
		Text:            permissionHTML(p) + h.mentionSuffix(),
		ParseMode:       "HTML",
		ReplyMarkup: &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
			{Text: "✅ Allow", CallbackData: encodeDecision("a", p.ID)},
			{Text: "⛔ Deny", CallbackData: encodeDecision("d", p.ID)},
		}, {
			{Text: "✅ Allow for session", CallbackData: encodeDecision("s", p.ID)},
		}}},
	}); err != nil {
		h.logger.Warn("telegram: post permission", "session", p.SessionID, "err", err)
	}
}

// handleCallback resolves a permission button tap: it authorizes the tapper,
// maps the button to a hook.Response, strips the keyboard so it can't be
// re-tapped, and toasts the outcome.
func (h *Hub) handleCallback(ctx context.Context, cb *CallbackQuery) {
	if cb.Message == nil || !h.authorizedChat(cb.Message.Chat.ID) || !h.authorizedSender(&cb.From) {
		_ = h.client.AnswerCallbackQuery(ctx, cb.ID, "not authorized")
		return
	}
	// AskUserQuestion option taps carry an option index, resolved separately.
	if strings.HasPrefix(cb.Data, "q:") {
		h.handleAskCallback(ctx, cb)
		return
	}
	behavior, scope, id, ok := decodeDecision(cb.Data)
	if !ok {
		_ = h.client.AnswerCallbackQuery(ctx, cb.ID, "")
		return
	}
	resp := hook.Response{Behavior: behavior, Scope: scope, Reason: "via telegram"}
	toast := "✅ allowed"
	switch {
	case strings.HasPrefix(cb.Data, "i:"):
		toast = "🚫 ignored"
	case behavior == "deny":
		toast = "⛔ denied"
	case scope == "session":
		toast = "✅ allowed for session"
	}
	if err := h.router.RespondInteraction(id, resp); err != nil {
		toast = "already resolved"
	}
	_ = h.client.AnswerCallbackQuery(ctx, cb.ID, toast)
	// Strip the buttons regardless, so a stale prompt can't be tapped again.
	if err := h.client.EditMessageReplyMarkup(ctx, cb.Message.Chat.ID, cb.Message.MessageID, nil); err != nil {
		h.logger.Debug("telegram: clear keyboard", "err", err)
	}
}

// handleInbound routes a message typed in a session's topic straight to that
// session (Mode A passthrough). Messages outside the configured group, from
// unauthorized users, in the General topic (no thread), or in topics not bound
// to a session are ignored — control of session lifecycle stays in the web UI.
func (h *Hub) handleInbound(ctx context.Context, m *Message) {
	if !h.authorized(m) {
		return
	}
	if m.MessageThreadID == 0 || strings.TrimSpace(m.Text) == "" {
		return
	}
	// A pending AskUserQuestion in this topic claims the reply as its answer
	// (the session is blocked waiting), rather than starting a new prompt.
	if h.answerByText(ctx, m.MessageThreadID, m.Text) {
		if err := h.client.SetMessageReaction(ctx, m.Chat.ID, m.MessageID, ackReaction); err != nil {
			h.logger.Debug("telegram: ack reaction", "err", err)
		}
		return
	}
	sessionID, ok := h.store.session(m.MessageThreadID)
	if !ok {
		return // topic not (yet) bound to a session
	}
	// Record before sending so the prompt-echo skips this message's own "user"
	// event (the user already sees what they typed here).
	h.recordSent(sessionID, m.Text)
	if err := h.router.SendToSession(sessionID, m.Text); err != nil {
		h.logger.Warn("telegram: send to session", "session", sessionID, "err", err)
		h.notifyTopic(m.MessageThreadID, "⚠️ couldn't deliver: "+err.Error())
		return
	}
	// React 👀 to confirm the message reached the session (no extra message).
	if err := h.client.SetMessageReaction(ctx, m.Chat.ID, m.MessageID, ackReaction); err != nil {
		h.logger.Debug("telegram: ack reaction", "session", sessionID, "err", err)
	}
}

// authorized gates an inbound message: right group and an allowed sender.
func (h *Hub) authorized(m *Message) bool {
	return h.authorizedChat(m.Chat.ID) && h.authorizedSender(m.From)
}

// authorizedChat reports whether chatID is the configured forum group.
func (h *Hub) authorizedChat(chatID int64) bool { return chatID == h.group }

// authorizedSender reports whether u may drive sessions: a non-bot user, and
// on the whitelist when one is configured (empty whitelist = any member). A
// nil sender (channel post / anonymous admin) is rejected.
func (h *Hub) authorizedSender(u *User) bool {
	if u == nil || u.IsBot {
		return false
	}
	if len(h.allowed) > 0 && !h.allowed[u.ID] {
		return false
	}
	return true
}

// notifyTopic posts a plain status line into a topic, best-effort.
func (h *Hub) notifyTopic(thread int64, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.client.SendMessage(ctx, SendMessageParams{
		ChatID:          h.group,
		MessageThreadID: thread,
		Text:            text,
	}); err != nil {
		h.logger.Warn("telegram: notify topic", "thread", thread, "err", err)
	}
}

// handleEvent mirrors a single session event into its topic. The streamed
// assistant text is sent silently (no phone buzz on every chunk); only turn
// completion — and, elsewhere, permission prompts — notify audibly.
func (h *Hub) handleEvent(ctx context.Context, ev broker.Event) {
	switch ev.Type {
	case "user":
		h.mirrorPrompt(ctx, ev)
	case "assistant":
		h.mirrorAssistant(ctx, ev)
	case "subprocess.exit":
		h.notifyTurnComplete(ctx, ev)
	}
}

// mirrorPrompt echoes a web/main-chat-originated prompt into its topic (so the
// topic shows the question). Prompts typed in Telegram are recorded by
// handleInbound and skipped; tool_result "user" events have no prompt text.
func (h *Hub) mirrorPrompt(ctx context.Context, ev broker.Event) {
	text := strings.TrimSpace(extractUserText(ev.Raw))
	if text == "" || h.consumeRecentSent(ev.SessionID, text) {
		return
	}
	thread, err := h.topicFor(ctx, ev.SessionID)
	if err != nil {
		h.logger.Warn("telegram: prompt topic", "session", ev.SessionID, "err", err)
		return
	}
	// Expandable blockquote (long prompts collapse) + caption. Over the length
	// cap, fall back to plain chunks.
	quoted := "<blockquote expandable>" + html.EscapeString(text) + "</blockquote>\n" + promptCaption
	if len([]rune(quoted)) <= telegramMaxMessage {
		if err := h.sendSilentHTML(ctx, thread, quoted, text+"\n"+promptCaption); err != nil {
			h.logger.Warn("telegram: echo prompt", "session", ev.SessionID, "err", err)
		}
		return
	}
	for _, chunk := range splitMessage(text) {
		if _, err := h.client.SendMessage(ctx, SendMessageParams{
			ChatID:              h.group,
			MessageThreadID:     thread,
			Text:                chunk,
			DisableNotification: true,
		}); err != nil {
			h.logger.Warn("telegram: echo prompt", "session", ev.SessionID, "err", err)
			return
		}
	}
}

// mirrorAssistant posts the assistant text and any show_image attachments of an
// event into its topic as silent messages.
func (h *Hub) mirrorAssistant(ctx context.Context, ev broker.Event) {
	text := assistantText(ev.Raw)
	images := imageRefs(ev.Raw)
	if text == "" && len(images) == 0 {
		return
	}
	thread, err := h.topicFor(ctx, ev.SessionID)
	if err != nil {
		h.logger.Warn("telegram: ensure topic", "session", ev.SessionID, "err", err)
		return
	}
	if text != "" {
		for _, chunk := range splitMessage(text) {
			if err := h.sendProse(ctx, thread, chunk); err != nil {
				// Give up on the remaining text, but still mirror any images —
				// they're independent of a text-send failure.
				h.logger.Warn("telegram: send", "session", ev.SessionID, "err", err)
				break
			}
		}
	}
	for _, ref := range images {
		h.mirrorImage(ctx, ev.SessionID, thread, ref)
	}
}

// sendProse posts one assistant-text chunk, rendered as Telegram HTML (plain on
// fallback).
func (h *Hub) sendProse(ctx context.Context, thread int64, chunk string) error {
	return h.sendSilentHTML(ctx, thread, toTelegramHTML(chunk), chunk)
}

// sendSilentHTML posts a silent HTML message, falling back to a plain-text
// version if the HTML is over the length cap or Telegram rejects it (400 from a
// best-effort conversion) — so a render edge case degrades to raw rather than
// dropping the message. plain must already fit the length cap.
func (h *Hub) sendSilentHTML(ctx context.Context, thread int64, htmlText, plain string) error {
	if len([]rune(htmlText)) <= telegramMaxMessage {
		_, err := h.client.SendMessage(ctx, SendMessageParams{
			ChatID:              h.group,
			MessageThreadID:     thread,
			Text:                htmlText,
			ParseMode:           "HTML",
			DisableNotification: true,
		})
		if err == nil {
			return nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != 400 {
			return err // a real failure (network, etc.), not an HTML parse error
		}
		h.logger.Debug("telegram: HTML rejected, retrying plain", "err", err)
	}
	_, err := h.client.SendMessage(ctx, SendMessageParams{
		ChatID:              h.group,
		MessageThreadID:     thread,
		Text:                plain,
		DisableNotification: true,
	})
	return err
}

// mirrorImage uploads a show_image attachment (silently). The path is resolved
// strictly inside the session cwd (pathutil), never a general file read.
func (h *Hub) mirrorImage(ctx context.Context, sessionID string, thread int64, ref string) {
	sess, ok := h.router.GetSession(sessionID)
	if !ok {
		return
	}
	full, ok := pathutil.ResolveWithinDir(sess.Cwd, ref)
	if !ok {
		h.logger.Warn("telegram: image outside session cwd", "session", sessionID, "path", ref)
		return
	}
	if !imageExts[strings.ToLower(filepath.Ext(full))] {
		return
	}
	name := filepath.Base(full)
	// Pre-check size: sendPhoto caps at ~10 MB, so don't read a huge file into
	// memory only for Telegram to reject it.
	if info, err := os.Stat(full); err == nil && info.Size() > maxPhotoBytes {
		h.imageFailNotice(ctx, thread, name, "larger than 10 MB")
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		h.logger.Warn("telegram: read image", "session", sessionID, "path", full, "err", err)
		return
	}
	if _, err := h.client.SendPhoto(ctx, SendPhotoParams{
		ChatID:              h.group,
		MessageThreadID:     thread,
		DisableNotification: true,
	}, name, data); err != nil {
		h.logger.Warn("telegram: send photo", "session", sessionID, "path", full, "err", err)
		h.imageFailNotice(ctx, thread, name, err.Error())
	}
}

// imageFailNotice leaves a silent note in the topic when an image can't be
// uploaded, so the image isn't silently missing.
func (h *Hub) imageFailNotice(ctx context.Context, thread int64, name, reason string) {
	if _, err := h.client.SendMessage(ctx, SendMessageParams{
		ChatID:              h.group,
		MessageThreadID:     thread,
		Text:                "🖼️ couldn't send image " + name + " (" + reason + ")",
		DisableNotification: true,
	}); err != nil {
		h.logger.Warn("telegram: image-fail notice", "thread", thread, "err", err)
	}
}

// notifyTurnComplete posts an audible turn-done ping into the session's topic —
// the "come look" signal after the silent stream. It does not create a topic:
// a turn that mirrored nothing (no assistant text) gets no ping.
func (h *Hub) notifyTurnComplete(ctx context.Context, ev broker.Event) {
	thread, ok := h.store.thread(ev.SessionID)
	if !ok {
		return
	}
	text := "✅ responded"
	if d, ok := turnDuration(ev.Raw); ok {
		text += " in " + humanizeDuration(d)
	}
	// No @mention here: turn completion fires every turn, so it stays audible
	// (notifies when the group isn't muted) but does NOT pierce a mute. Only the
	// blocking prompts (permission / question) mention the user.
	if _, err := h.client.SendMessage(ctx, SendMessageParams{
		ChatID:          h.group,
		MessageThreadID: thread,
		Text:            text,
	}); err != nil {
		h.logger.Warn("telegram: turn-complete ping", "session", ev.SessionID, "err", err)
	}
}

// mentionSuffix returns inline tg://user @-mentions of the whitelisted users. A
// mention pierces a muted group, so "come look" messages still notify when the
// stream is muted. Empty if no whitelist (no id to mention).
func (h *Hub) mentionSuffix() string {
	if len(h.allowed) == 0 {
		return ""
	}
	ids := make([]int64, 0, len(h.allowed))
	for id := range h.allowed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(` <a href="tg://user?id=`)
		b.WriteString(strconv.FormatInt(id, 10))
		b.WriteString(`">🔔</a>`)
	}
	return b.String()
}

// topicFor returns the forum topic bound to sessionID, lazily creating one on
// first need. The mapping is persisted so the topic is re-adopted on restart.
func (h *Hub) topicFor(ctx context.Context, sessionID string) (int64, error) {
	if id, ok := h.store.thread(sessionID); ok {
		return id, nil
	}
	// Serialize creation so the Run loop and permissionLoop can't both create a
	// topic for the same session's first activity (which would orphan one). The
	// hit path above stays lock-free; this only gates the rare create.
	h.createMu.Lock()
	defer h.createMu.Unlock()
	if id, ok := h.store.thread(sessionID); ok {
		return id, nil // another goroutine created it while we waited
	}
	topic, err := h.client.CreateForumTopic(ctx, h.group, h.topicName(sessionID))
	if err != nil {
		return 0, err
	}
	if err := h.store.put(sessionID, topic.MessageThreadID); err != nil {
		h.logger.Warn("telegram: persist topic map", "session", sessionID, "err", err)
	}
	h.logger.Info("telegram: created topic", "session", sessionID, "thread", topic.MessageThreadID)
	return topic.MessageThreadID, nil
}

// topicName derives a human-friendly topic title from the session's title,
// falling back to a short id. Telegram caps topic names at 128 chars.
func (h *Hub) topicName(sessionID string) string {
	name := shortID(sessionID)
	if sess, ok := h.router.GetSession(sessionID); ok && strings.TrimSpace(sess.Title) != "" {
		name = sess.Title
	}
	return truncate(name, 128)
}

// recordSent remembers the prompt usher just forwarded from Telegram, for the
// prompt-echo dedup.
func (h *Hub) recordSent(sessionID, text string) {
	h.recentMu.Lock()
	h.recentSent[sessionID] = strings.TrimSpace(text)
	h.recentMu.Unlock()
}

// consumeRecentSent reports whether text matches the prompt last forwarded from
// Telegram for sessionID, clearing it on a match (one prompt → one user event).
func (h *Hub) consumeRecentSent(sessionID, text string) bool {
	h.recentMu.Lock()
	defer h.recentMu.Unlock()
	if h.recentSent[sessionID] == text {
		delete(h.recentSent, sessionID)
		return true
	}
	return false
}
