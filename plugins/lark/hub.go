package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/imutil"
	"github.com/nexustar/usher/internal/pathutil"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// RouterAPI is the Router subset the hub consumes — identical to the
// in-process Telegram hub's interface; *pluginapi.Client satisfies it over
// the plugin socket.
type RouterAPI interface {
	GetSession(id string) (core.Session, bool)
	SubscribeAllSessions() (<-chan broker.Event, func())
	SendToSession(id, text string) error
	SubscribePendingInteractions() (<-chan hook.Pending, func())
	RespondInteraction(id string, resp hook.Response) error
}

// Config configures a Hub. App credentials are baked into the lark client.
type Config struct {
	ChatID    string // the Lark group chat usher mirrors into (oc_...)
	StatePath string // session→thread map file; "" = in-memory (tests)
	// AllowedUserIDs whitelists open ids (ou_...) that may drive sessions;
	// empty = any member of ChatID (the private group is the trust boundary).
	AllowedUserIDs []string
}

// larkMaxMessage caps one text message. Lark's real limit is ~150KB of
// content JSON; chunking well below that keeps messages readable.
const larkMaxMessage = 4000

// promptCaption labels an echoed prompt mirrored from another frontend.
const promptCaption = "↑ mirrored user input"

// ackEmoji is the reaction usher adds to an inbound message once it has been
// handed to the session — a no-extra-message "received, working" marker.
const ackEmoji = "THUMBSUP"

// maxImageBytes is Lark's image upload cap (10 MB).
const maxImageBytes = 10 << 20

// askEntry remembers a posted AskUserQuestion awaiting an answer: the question
// text (to key the answer) and the option labels (so a tapped index → label).
// It is indexed by pending id and by session (a typed reply in the session's
// thread answers it).
type askEntry struct {
	question string
	labels   []string
	session  string
}

// Hub mirrors usher's sessions into a Lark group chat, one thread per
// session. It is a peer frontend to the web server, consuming the Router
// through the plugin socket; it owns no Claude processes itself.
type Hub struct {
	lark    larkAPI
	router  RouterAPI
	store   *threadStore
	chat    string
	allowed map[string]bool // empty = any chat member allowed
	logger  *slog.Logger

	createMu sync.Mutex // serializes lazy root-message creation (see rootFor)

	askMu         sync.Mutex
	asks          map[string]askEntry
	asksBySession map[string]string

	// posted dedupes permission prompts: the plugin-socket subscription
	// replays the pending snapshot on every reconnect.
	postedMu sync.Mutex
	posted   map[string]bool

	// recentSent: last prompt forwarded FROM Lark per session, so the
	// prompt-echo skips it (else the user's own message mirrors back twice).
	recentMu   sync.Mutex
	recentSent map[string]string
}

// NewHub builds a Hub. The thread-mapping store is loaded from cfg.StatePath
// (re-adopting existing threads across restarts).
func NewHub(client larkAPI, router RouterAPI, cfg Config, logger *slog.Logger) (*Hub, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store, err := newThreadStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(cfg.AllowedUserIDs))
	for _, id := range cfg.AllowedUserIDs {
		allowed[id] = true
	}
	return &Hub{
		lark:          client,
		router:        router,
		store:         store,
		chat:          cfg.ChatID,
		allowed:       allowed,
		logger:        logger,
		asks:          map[string]askEntry{},
		asksBySession: map[string]string{},
		posted:        map[string]bool{},
		recentSent:    map[string]string{},
	}, nil
}

// mentions returns the whitelisted open ids in stable order, for card
// @-mentions.
func (h *Hub) mentions() []string {
	ids := make([]string, 0, len(h.allowed))
	for id := range h.allowed {
		ids = append(ids, id)
	}
	sortStrings(ids)
	return ids
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Run runs the hub's loops until ctx is cancelled. Inbound Lark traffic
// arrives separately via HandleMessage / HandleCardAction (wired to the
// websocket event dispatcher).
func (h *Hub) Run(ctx context.Context) error {
	if len(h.allowed) == 0 {
		h.logger.Warn("lark: no --allowed-user-ids set; any member of the chat can drive sessions")
	}
	go h.permissionLoop(ctx)
	return h.dispatchLoop(ctx)
}

// sessionQueueSize bounds each session's mirror backlog (see the telegram
// hub for rationale: a slow thread only backs up its own queue).
const sessionQueueSize = 64

// dispatchLoop fans the global event stream out to one worker per session.
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
				h.logger.Warn("lark: mirror queue full, dropping event",
					"session", ev.SessionID, "type", ev.Type)
			}
		}
	}
}

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

// permissionLoop posts each new permission request into the originating
// session's thread. The subscription replays the pending snapshot on every
// (re)connect, so prompts are deduped by pending id.
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
			if h.markPosted(p.ID) {
				h.postPermission(ctx, p)
			}
		}
	}
}

// markPosted records a pending id, returning false when it was already
// posted (a snapshot replay after reconnect).
func (h *Hub) markPosted(id string) bool {
	h.postedMu.Lock()
	defer h.postedMu.Unlock()
	if h.posted[id] {
		return false
	}
	h.posted[id] = true
	return true
}

// handleEvent mirrors a single session event into its thread.
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

// mirrorPrompt echoes a web/main-chat-originated prompt into its thread.
// Prompts typed in Lark are recorded by HandleMessage and skipped.
func (h *Hub) mirrorPrompt(ctx context.Context, ev broker.Event) {
	text := strings.TrimSpace(imutil.ExtractUserText(ev.Raw))
	if text == "" || h.consumeRecentSent(ev.SessionID, text) {
		return
	}
	root, err := h.rootFor(ctx, ev.SessionID)
	if err != nil {
		h.logger.Warn("lark: prompt thread", "session", ev.SessionID, "err", err)
		return
	}
	for _, chunk := range imutil.SplitMessage(text+"\n"+promptCaption, larkMaxMessage) {
		if !h.replyText(ctx, ev.SessionID, root, chunk) {
			return
		}
	}
}

// mirrorAssistant posts the assistant text and any show_image attachments of
// an event into its thread.
func (h *Hub) mirrorAssistant(ctx context.Context, ev broker.Event) {
	text := imutil.AssistantText(ev.Raw)
	images := imutil.ImageRefs(ev.Raw)
	if text == "" && len(images) == 0 {
		return
	}
	root, err := h.rootFor(ctx, ev.SessionID)
	if err != nil {
		h.logger.Warn("lark: ensure thread", "session", ev.SessionID, "err", err)
		return
	}
	for _, chunk := range imutil.SplitMessage(text, larkMaxMessage) {
		if chunk == "" {
			continue
		}
		if !h.replyText(ctx, ev.SessionID, root, chunk) {
			// Give up on the remaining text, but still mirror any images —
			// they're independent of a text-send failure.
			break
		}
	}
	for _, ref := range images {
		h.mirrorImage(ctx, ev.SessionID, root, ref)
	}
}

// replyText posts one threaded text reply, recording the thread id the reply
// reveals. Returns false on failure.
func (h *Hub) replyText(ctx context.Context, sessionID, root, text string) bool {
	thread, err := h.lark.ReplyText(ctx, root, text)
	if err != nil {
		h.logger.Warn("lark: send", "session", sessionID, "err", err)
		return false
	}
	h.recordThread(sessionID, thread)
	return true
}

func (h *Hub) recordThread(sessionID, thread string) {
	if err := h.store.setThread(sessionID, thread); err != nil {
		h.logger.Warn("lark: persist thread id", "session", sessionID, "err", err)
	}
}

// mirrorImage uploads a show_image attachment into the thread.
func (h *Hub) mirrorImage(ctx context.Context, sessionID, root, ref string) {
	sess, ok := h.router.GetSession(sessionID)
	if !ok {
		return
	}
	full, ok := pathutil.ResolveImagePath(sess.Cwd, ref)
	if !ok {
		h.logger.Warn("lark: image outside allowed dirs", "session", sessionID, "path", ref)
		return
	}
	if !imutil.ImageExts[strings.ToLower(filepath.Ext(full))] {
		return
	}
	name := filepath.Base(full)
	if info, err := os.Stat(full); err == nil && info.Size() > maxImageBytes {
		h.imageFailNotice(ctx, sessionID, root, name, "larger than 10 MB")
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		h.logger.Warn("lark: read image", "session", sessionID, "path", full, "err", err)
		return
	}
	key, err := h.lark.UploadImage(ctx, data)
	if err != nil {
		h.logger.Warn("lark: upload image", "session", sessionID, "path", full, "err", err)
		h.imageFailNotice(ctx, sessionID, root, name, err.Error())
		return
	}
	thread, err := h.lark.ReplyImage(ctx, root, key)
	if err != nil {
		h.logger.Warn("lark: send image", "session", sessionID, "path", full, "err", err)
		h.imageFailNotice(ctx, sessionID, root, name, err.Error())
		return
	}
	h.recordThread(sessionID, thread)
}

// imageFailNotice leaves a note in the thread when an image can't be sent,
// so it isn't silently missing.
func (h *Hub) imageFailNotice(ctx context.Context, sessionID, root, name, reason string) {
	h.replyText(ctx, sessionID, root, "🖼️ couldn't send image "+name+" ("+reason+")")
}

// notifyTurnComplete posts a turn-done ping into the session's thread — the
// "come look" signal. It does not create a thread: a turn that mirrored
// nothing gets no ping.
func (h *Hub) notifyTurnComplete(ctx context.Context, ev broker.Event) {
	root, ok := h.store.root(ev.SessionID)
	if !ok {
		return
	}
	text := "✅ responded"
	if d, ok := imutil.TurnDuration(ev.Raw); ok {
		text += " in " + imutil.HumanizeDuration(d)
	}
	h.replyText(ctx, ev.SessionID, root, text)
}

// postPermission posts a pending interaction into its session's thread as an
// interactive card (lazily creating the thread). AskUserQuestion gets its
// own option prompt instead.
func (h *Hub) postPermission(ctx context.Context, p hook.Pending) {
	root, err := h.rootFor(ctx, p.SessionID)
	if err != nil {
		h.logger.Warn("lark: permission thread", "session", p.SessionID, "err", err)
		return
	}
	card := ""
	if p.ToolName == "AskUserQuestion" {
		card = h.registerAsk(p)
	} else {
		card = permissionCard(p, h.mentions(), "")
	}
	thread, err := h.lark.ReplyCard(ctx, root, card)
	if err != nil {
		h.logger.Warn("lark: post permission", "session", p.SessionID, "err", err)
		h.takeAsk(p.ID) // don't strand a typed reply on a card that never posted
		return
	}
	h.recordThread(p.SessionID, thread)
}

// registerAsk renders the card for an AskUserQuestion and registers it for
// tap / typed-reply answering. Multi-question prompts can't be mapped to one
// typed reply, so those fall back to the web UI (Ignore-only card).
func (h *Hub) registerAsk(p hook.Pending) string {
	qs := parseQuestions(p.ToolInput)
	if len(qs) != 1 {
		return multiStepCard(p.ID, h.mentions(), "")
	}
	q := qs[0]
	labels := make([]string, len(q.Options))
	for i, o := range q.Options {
		labels[i] = o.Label
	}
	h.putAsk(p.ID, askEntry{question: q.Question, labels: labels, session: p.SessionID})
	return askCard(q, p.ID, h.mentions(), "")
}

// rootFor returns the thread-root message bound to sessionID, lazily posting
// one on first need. The mapping is persisted so the thread is re-adopted on
// restart.
func (h *Hub) rootFor(ctx context.Context, sessionID string) (string, error) {
	if id, ok := h.store.root(sessionID); ok {
		return id, nil
	}
	h.createMu.Lock()
	defer h.createMu.Unlock()
	if id, ok := h.store.root(sessionID); ok {
		return id, nil // another goroutine created it while we waited
	}
	root, err := h.lark.SendText(ctx, h.chat, h.threadTitle(sessionID))
	if err != nil {
		return "", err
	}
	if err := h.store.put(sessionID, root); err != nil {
		h.logger.Warn("lark: persist thread map", "session", sessionID, "err", err)
	}
	h.logger.Info("lark: created thread", "session", sessionID, "root", root)
	return root, nil
}

// threadTitle renders the root message anchoring a session's thread.
func (h *Hub) threadTitle(sessionID string) string {
	name := imutil.ShortID(sessionID)
	cwd := ""
	if sess, ok := h.router.GetSession(sessionID); ok {
		if strings.TrimSpace(sess.Title) != "" {
			name = sess.Title
		}
		cwd = sess.Cwd
	}
	title := "🧵 " + imutil.Truncate(name, 120)
	if cwd != "" {
		title += "\n📁 " + cwd
	}
	return title
}

// --- inbound (wired to the websocket event dispatcher) ---------------------

// mentionPlaceholder strips the @_user_N placeholders Lark substitutes for
// @-mentions in text content.
var mentionPlaceholder = regexp.MustCompile(`@_user_\d+\s*`)

// HandleMessage routes a message typed in a session's thread straight to
// that session (Mode A passthrough). Messages outside the configured chat,
// from unauthorized users, outside any bound thread, or without text are
// ignored — session lifecycle control stays in the web UI.
func (h *Hub) HandleMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	msg := event.Event.Message
	if deref(msg.ChatId) != h.chat || !h.authorizedSender(event.Event.Sender) {
		return
	}
	text := inboundText(msg)
	if text == "" {
		return
	}
	sessionID, ok := h.store.session(deref(msg.ThreadId), firstNonEmpty(deref(msg.RootId), deref(msg.ParentId)))
	if !ok {
		return // not (yet) bound to a session
	}
	h.recordThread(sessionID, deref(msg.ThreadId))
	// A pending AskUserQuestion for this session claims the reply as its
	// answer (the session is blocked waiting), rather than a new prompt.
	if h.answerByText(sessionID, text) {
		h.ack(ctx, deref(msg.MessageId))
		return
	}
	// Record before sending so the prompt-echo skips this message's own
	// "user" event (the user already sees what they typed here).
	h.recordSent(sessionID, text)
	if err := h.router.SendToSession(sessionID, text); err != nil {
		h.logger.Warn("lark: send to session", "session", sessionID, "err", err)
		if root, ok := h.store.root(sessionID); ok {
			h.replyText(ctx, sessionID, root, "⚠️ couldn't deliver: "+err.Error())
		}
		return
	}
	h.ack(ctx, deref(msg.MessageId))
}

// ack reacts to an inbound message to confirm it reached the session.
func (h *Hub) ack(ctx context.Context, messageID string) {
	if messageID == "" {
		return
	}
	if err := h.lark.React(ctx, messageID, ackEmoji); err != nil {
		h.logger.Debug("lark: ack reaction", "err", err)
	}
}

// inboundText extracts the typed text of an inbound message (text messages
// only; posts/files/etc. are ignored).
func inboundText(msg *larkim.EventMessage) string {
	if deref(msg.MessageType) != larkim.MsgTypeText {
		return ""
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(deref(msg.Content)), &content); err != nil {
		return ""
	}
	return strings.TrimSpace(mentionPlaceholder.ReplaceAllString(content.Text, ""))
}

// authorizedSender reports whether the sender may drive sessions: a user
// sender, on the whitelist when one is configured.
func (h *Hub) authorizedSender(s *larkim.EventSender) bool {
	if s == nil || deref(s.SenderType) != "user" || s.SenderId == nil {
		return false
	}
	openID := deref(s.SenderId.OpenId)
	if openID == "" {
		return false
	}
	if len(h.allowed) > 0 && !h.allowed[openID] {
		return false
	}
	return true
}

// HandleCardAction resolves a card button tap: it authorizes the tapper,
// maps the button to a hook.Response, and returns a toast plus the resolved
// card (buttons stripped so it can't be re-tapped).
func (h *Hub) HandleCardAction(ctx context.Context, event *callback.CardActionTriggerEvent) *callback.CardActionTriggerResponse {
	if event == nil || event.Event == nil || event.Event.Action == nil {
		return &callback.CardActionTriggerResponse{}
	}
	req := event.Event
	if !h.authorizedOperator(req) {
		return toast("not authorized")
	}
	v, ok := parseActionValue(req.Action.Value)
	if !ok {
		return &callback.CardActionTriggerResponse{}
	}
	if v.Kind == "q" {
		return h.handleAskAction(v)
	}
	behavior, scope, ok := decodeDecision(v)
	if !ok {
		return &callback.CardActionTriggerResponse{}
	}
	h.takeAsk(v.ID) // an Ignore on a question also clears its typed-reply entry
	resp := hook.Response{Behavior: behavior, Scope: scope, Reason: "via lark"}
	msg := "✅ allowed"
	switch {
	case v.Kind == "i":
		msg = "🚫 ignored"
	case behavior == "deny":
		msg = "⛔ denied"
	case scope == "session":
		msg = "✅ allowed for session"
	}
	if err := h.router.RespondInteraction(v.ID, resp); err != nil {
		msg = "already resolved"
	}
	return toast(msg)
}

// handleAskAction resolves an AskUserQuestion option tap into an allow +
// answer response.
func (h *Hub) handleAskAction(v decisionValue) *callback.CardActionTriggerResponse {
	idx, err := strconv.Atoi(v.Opt)
	entry, ok := h.takeAsk(v.ID)
	if err != nil || !ok || idx < 0 || idx >= len(entry.labels) {
		return toast("expired")
	}
	label := entry.labels[idx]
	resp := hook.Response{
		Behavior: "allow",
		Reason:   "via lark",
		Answers:  map[string]string{entry.question: label},
	}
	msg := "✅ " + imutil.Truncate(label, 100)
	if err := h.router.RespondInteraction(v.ID, resp); err != nil {
		msg = "already resolved"
	}
	return toast(msg)
}

// authorizedOperator gates a card tap: right chat and an allowed operator.
func (h *Hub) authorizedOperator(req *callback.CardActionTriggerRequest) bool {
	if req.Context == nil || req.Context.OpenChatID != h.chat {
		return false
	}
	if len(h.allowed) == 0 {
		return req.Operator != nil && req.Operator.OpenID != ""
	}
	return req.Operator != nil && h.allowed[req.Operator.OpenID]
}

func toast(msg string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{Type: "info", Content: msg},
	}
}

// answerByText resolves a pending AskUserQuestion for a session from a typed
// reply, returning true if it consumed the message.
func (h *Hub) answerByText(sessionID, text string) bool {
	id, entry, ok := h.takeAskBySession(sessionID)
	if !ok {
		return false
	}
	resp := hook.Response{
		Behavior: "allow",
		Reason:   "via lark",
		Answers:  map[string]string{entry.question: strings.TrimSpace(text)},
	}
	if err := h.router.RespondInteraction(id, resp); err != nil {
		h.logger.Debug("lark: answer ask by text", "id", id, "err", err)
	}
	return true
}

func (h *Hub) putAsk(id string, e askEntry) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	h.asks[id] = e
	h.asksBySession[e.session] = id
}

func (h *Hub) takeAsk(id string) (askEntry, bool) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	e, ok := h.asks[id]
	if ok {
		delete(h.asks, id)
		delete(h.asksBySession, e.session)
	}
	return e, ok
}

func (h *Hub) takeAskBySession(sessionID string) (string, askEntry, bool) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	id, ok := h.asksBySession[sessionID]
	if !ok {
		return "", askEntry{}, false
	}
	e := h.asks[id]
	delete(h.asks, id)
	delete(h.asksBySession, sessionID)
	return id, e, true
}

func (h *Hub) recordSent(sessionID, text string) {
	h.recentMu.Lock()
	h.recentSent[sessionID] = strings.TrimSpace(text)
	h.recentMu.Unlock()
}

func (h *Hub) consumeRecentSent(sessionID, text string) bool {
	h.recentMu.Lock()
	defer h.recentMu.Unlock()
	if h.recentSent[sessionID] == text {
		delete(h.recentSent, sessionID)
		return true
	}
	return false
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
