package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/imutil"
	"github.com/nexustar/usher/internal/pathutil"
	"github.com/nexustar/usher/internal/pluginapi"

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

// larkCardMax caps one markdown-card chunk. Cards hold ~30K characters, so
// splitting is rare; a fence unlucky enough to straddle a split renders
// broken in both halves, and the high threshold keeps that theoretical.
const larkCardMax = 20000

// promptCaption labels an echoed prompt mirrored from another frontend.
const promptCaption = "↑ mirrored user input"

// ackEmoji is the reaction usher adds to an inbound message once it has been
// handed to the session — a no-extra-message "received, working" marker
// (Feishu's "Get" sticker).
const ackEmoji = "Get"

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
	// mentionIDs is the whitelist in stable order, for card @-mentions.
	mentionIDs []string
	logger     *slog.Logger

	createMu sync.Mutex // serializes lazy root-message creation (see rootFor)

	askMu         sync.Mutex
	asks          map[string]askEntry
	asksBySession map[string]string

	// posted holds the prompts currently shown as live cards, by pending id.
	// It dedupes the snapshot replays the plugin-socket subscription sends on
	// every reconnect, and keeps the prompt body so a decided card can be
	// re-rendered without buttons. Entries leave on resolution via Lark;
	// prompts resolved elsewhere (web UI) linger — bounded by usage, not time.
	postedMu sync.Mutex
	posted   map[string]hook.Pending

	// recentSent: last prompt forwarded FROM Lark per session, so the
	// prompt-echo skips it (else the user's own message mirrors back twice).
	recentMu   sync.Mutex
	recentSent map[string]string

	// seen dedupes inbound pushes by message id: Feishu delivers events at
	// least once and the ws SDK does no event dedup of its own, so a slow
	// ack or a reconnect redelivers — without this, one typed message
	// reaches the session twice.
	seenMu sync.Mutex
	seen   map[string]time.Time

	// spawn runs accepted-inbound routing off the websocket handler
	// goroutine (see HandleMessage). Tests override it to run synchronously.
	spawn func(func())
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
	mentionIDs := slices.Clone(cfg.AllowedUserIDs)
	slices.Sort(mentionIDs)
	return &Hub{
		lark:          client,
		router:        router,
		store:         store,
		chat:          cfg.ChatID,
		allowed:       allowed,
		mentionIDs:    slices.Compact(mentionIDs),
		logger:        logger,
		asks:          map[string]askEntry{},
		asksBySession: map[string]string{},
		posted:        map[string]hook.Pending{},
		recentSent:    map[string]string{},
		seen:          map[string]time.Time{},
		spawn:         func(f func()) { go f() },
	}, nil
}

// seenTTL bounds how long a handled message id is remembered; Feishu retries
// span seconds-to-minutes, so ten minutes is generous.
const seenTTL = 10 * time.Minute

// alreadyHandled records a message id, reporting whether it was seen before.
// Expired entries are pruned on insert, keeping the map bounded by the
// message rate of the last ten minutes.
func (h *Hub) alreadyHandled(id string) bool {
	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	now := time.Now()
	if len(h.seen) > 256 {
		for k, t := range h.seen {
			if now.Sub(t) > seenTTL {
				delete(h.seen, k)
			}
		}
	}
	if _, ok := h.seen[id]; ok {
		return true
	}
	h.seen[id] = now
	return false
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
			if !h.claimPending(p) {
				continue // a snapshot replay of a card already posted
			}
			if !h.postPermission(ctx, p) {
				// The card never reached Lark; unclaim so the next snapshot
				// replay (reconnect) retries instead of dropping the prompt.
				h.unclaimPending(p.ID)
			}
		}
	}
}

// claimPending records a prompt as posted, returning false when it already
// was (a snapshot replay after reconnect).
func (h *Hub) claimPending(p hook.Pending) bool {
	h.postedMu.Lock()
	defer h.postedMu.Unlock()
	if _, ok := h.posted[p.ID]; ok {
		return false
	}
	h.posted[p.ID] = p
	return true
}

func (h *Hub) unclaimPending(id string) {
	h.postedMu.Lock()
	delete(h.posted, id)
	h.postedMu.Unlock()
}

// takePosted removes and returns the prompt behind a live card, for the
// resolved re-render. !ok after a plugin restart (the map is in-memory).
func (h *Hub) takePosted(id string) (hook.Pending, bool) {
	h.postedMu.Lock()
	defer h.postedMu.Unlock()
	p, ok := h.posted[id]
	if ok {
		delete(h.posted, id)
	}
	return p, ok
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
		h.refreshTitle(ctx, ev.SessionID)
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
	// Quote block + italic caption, so the echo reads as a citation rather
	// than more plain text in the stream.
	for _, chunk := range imutil.SplitMessage(text, larkCardMax) {
		if chunk == "" {
			continue
		}
		if !h.replyMarkdown(ctx, ev.SessionID, root, quoteMD(chunk)+"\n\n"+promptCaption) {
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
	for _, chunk := range imutil.SplitMessage(text, larkCardMax) {
		if chunk == "" {
			continue
		}
		if !h.replyMarkdown(ctx, ev.SessionID, root, chunk) {
			// Give up on the remaining text, but still mirror any images —
			// they're independent of a text-send failure.
			break
		}
	}
	for _, ref := range images {
		h.mirrorImage(ctx, ev.SessionID, root, ref)
	}
}

// replyMarkdown posts one assistant-text chunk as a post-message md
// paragraph, so bold / lists / fences render in a plain bubble (a "text"
// message shows markdown literally; a card adds a frame). If Lark rejects
// the post, the chunk degrades to plain text messages rather than dropping
// — a render edge case must not lose content.
func (h *Hub) replyMarkdown(ctx context.Context, sessionID, root, md string) bool {
	thread, err := h.lark.ReplyPost(ctx, root, postMD(sanitizeMarkdown(md)))
	if err == nil {
		h.recordThread(sessionID, thread)
		return true
	}
	h.logger.Warn("lark: markdown post rejected, sending plain", "session", sessionID, "err", err)
	for _, chunk := range imutil.SplitMessage(md, larkMaxMessage) {
		if !h.replyText(ctx, sessionID, root, chunk) {
			return false
		}
	}
	return true
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
func (h *Hub) postPermission(ctx context.Context, p hook.Pending) bool {
	root, err := h.rootFor(ctx, p.SessionID)
	if err != nil {
		h.logger.Warn("lark: permission thread", "session", p.SessionID, "err", err)
		return false
	}
	var c obj
	if p.ToolName == "AskUserQuestion" {
		c = h.registerAsk(p)
	} else {
		c = permissionCard(p, h.mentionIDs, "")
	}
	thread, err := h.lark.ReplyCard(ctx, root, cardJSON(c))
	if err != nil {
		h.logger.Warn("lark: post permission", "session", p.SessionID, "err", err)
		h.takeAsk(p.ID) // don't strand a typed reply on a card that never posted
		return false
	}
	h.recordThread(p.SessionID, thread)
	return true
}

// registerAsk renders the card for an AskUserQuestion and registers it for
// tap / typed-reply answering. Multi-question prompts can't be mapped to one
// typed reply, so those fall back to the web UI (Ignore-only card).
func (h *Hub) registerAsk(p hook.Pending) obj {
	qs := imutil.ParseQuestions(p.ToolInput)
	if len(qs) != 1 {
		return multiStepCard(p.ID, h.mentionIDs, "")
	}
	q := qs[0]
	labels := make([]string, len(q.Options))
	for i, o := range q.Options {
		labels[i] = o.Label
	}
	h.putAsk(p.ID, askEntry{question: q.Question, labels: labels, session: p.SessionID})
	return askCard(q, p.ID, h.mentionIDs, "")
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
	title, cwd, meta := h.sessionCardInfo(sessionID)
	root, err := h.lark.SendCard(ctx, h.chat, cardJSON(rootCard(title, cwd, meta)))
	if err != nil {
		return "", err
	}
	if err := h.store.put(sessionID, root); err != nil {
		h.logger.Warn("lark: persist thread map", "session", sessionID, "err", err)
	}
	if err := h.store.setTitle(sessionID, title); err != nil {
		h.logger.Warn("lark: persist thread title", "session", sessionID, "err", err)
	}
	h.logger.Info("lark: created thread", "session", sessionID, "root", root)
	return root, nil
}

// refreshTitle re-renders the root card when the session's title changed —
// renames and AI titles usually land after the thread already exists. Runs
// at turn end, so it costs one GetSession per turn, not per event. A legacy
// text root (pre-card threads) can't be patched; the title is recorded
// anyway so the failure isn't retried every turn.
func (h *Hub) refreshTitle(ctx context.Context, sessionID string) {
	root, ok := h.store.root(sessionID)
	if !ok {
		return
	}
	title, cwd, meta := h.sessionCardInfo(sessionID)
	if last, _ := h.store.title(sessionID); last == title {
		return
	}
	if err := h.lark.UpdateCard(ctx, root, cardJSON(rootCard(title, cwd, meta))); err != nil {
		h.logger.Warn("lark: retitle thread", "session", sessionID, "err", err)
	}
	if err := h.store.setTitle(sessionID, title); err != nil {
		h.logger.Warn("lark: persist thread title", "session", sessionID, "err", err)
	}
}

// sessionCardInfo resolves the root card's fields: the session's display
// title (short id fallback), its cwd, and a backend/short-id metadata line.
func (h *Hub) sessionCardInfo(sessionID string) (title, cwd, meta string) {
	title = imutil.ShortID(sessionID)
	meta = imutil.ShortID(sessionID)
	if sess, ok := h.router.GetSession(sessionID); ok {
		if strings.TrimSpace(sess.Title) != "" {
			title = sess.Title
		}
		cwd = sess.Cwd
		if sess.Backend != "" {
			meta = sess.Backend + " · " + meta
		}
	}
	return title, cwd, meta
}

// --- inbound (wired to the websocket event dispatcher) ---------------------

// mentionPlaceholder strips the @_user_N placeholders Lark substitutes for
// @-mentions in text content.
var mentionPlaceholder = regexp.MustCompile(`@_user_\d+\s*`)

// HandleMessage routes a message typed in a session's thread straight to
// that session (Mode A passthrough). Messages outside the configured chat,
// from unauthorized users, outside any bound thread, or without text are
// ignored — session lifecycle control stays in the web UI.
func (h *Hub) HandleMessage(_ context.Context, event *larkim.P2MessageReceiveV1) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	msg := event.Event.Message
	if deref(msg.ChatId) != h.chat || !h.authorizedSender(event.Event.Sender) {
		return
	}
	if id := deref(msg.MessageId); id != "" && h.alreadyHandled(id) {
		h.logger.Info("lark: duplicate inbound push ignored", "message", id)
		return
	}
	text := inboundText(msg)
	if text == "" {
		return
	}
	sessionID, ok := h.store.session(deref(msg.ThreadId), cmp.Or(deref(msg.RootId), deref(msg.ParentId)))
	if !ok {
		return // not (yet) bound to a session
	}
	h.recordThread(sessionID, deref(msg.ThreadId))
	// Route off the websocket handler goroutine: the push's ack frame is only
	// sent once this returns, and a late ack makes Feishu redeliver. The
	// detached context outlives the connection the push arrived on — an
	// accepted message must not die to a socket flap mid-delivery.
	messageID := deref(msg.MessageId)
	h.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		h.routeInbound(ctx, sessionID, messageID, text)
	})
}

// routeInbound delivers one accepted inbound message: as the answer to a
// pending AskUserQuestion if one is waiting, otherwise as a prompt.
func (h *Hub) routeInbound(ctx context.Context, sessionID, messageID, text string) {
	if h.answerByText(ctx, sessionID, messageID, text) {
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
	h.ack(ctx, messageID)
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
	msg := "✅ allowed"
	switch {
	case v.Kind == "i":
		msg = "🚫 ignored"
	case behavior == "deny":
		msg = "⛔ denied"
	case scope == "session":
		msg = "✅ allowed for session"
	}
	err := h.router.RespondInteraction(v.ID, hook.Response{Behavior: behavior, Scope: scope, Reason: "via lark"})
	if err != nil && !isServerReject(err) {
		// Transport failure: usher may never have seen it. Keep the card and
		// the ask entry live so the tap can be retried.
		h.logger.Warn("lark: respond interaction", "id", v.ID, "err", err)
		return toast("usher unreachable — try again")
	}
	if err != nil {
		msg = "already resolved"
	}
	h.takeAsk(v.ID) // an Ignore on a question also clears its typed-reply entry
	return h.resolved(v.ID, msg)
}

// resolved builds the card-callback response for a decided prompt: the
// outcome toast plus the buttonless re-render (when this process posted the
// card and still knows its body).
func (h *Hub) resolved(pendingID, outcome string) *callback.CardActionTriggerResponse {
	resp := toast(outcome)
	if p, ok := h.takePosted(pendingID); ok {
		resp.Card = &callback.Card{Type: "raw", Data: resolvedCard(p, outcome)}
	}
	return resp
}

// isServerReject reports whether err is usher refusing the call (already
// resolved / unknown id) rather than the socket failing.
func isServerReject(err error) bool {
	var apiErr *pluginapi.APIError
	return errors.As(err, &apiErr)
}

// handleAskAction resolves an AskUserQuestion option tap into an allow +
// answer response.
func (h *Hub) handleAskAction(v decisionValue) *callback.CardActionTriggerResponse {
	idx, err := strconv.Atoi(v.Opt)
	entry, ok := h.peekAsk(v.ID)
	if err != nil || !ok || idx < 0 || idx >= len(entry.labels) {
		// Don't consume the entry on a malformed tap: a valid tap or a typed
		// reply must still be able to answer the question.
		return toast("expired")
	}
	label := entry.labels[idx]
	respErr := h.router.RespondInteraction(v.ID, hook.Response{
		Behavior: "allow",
		Reason:   "via lark",
		Answers:  map[string]string{entry.question: label},
	})
	if respErr != nil && !isServerReject(respErr) {
		h.logger.Warn("lark: answer ask", "id", v.ID, "err", respErr)
		return toast("usher unreachable — try again")
	}
	msg := "✅ " + imutil.Truncate(label, 100)
	if respErr != nil {
		msg = "already resolved"
	}
	h.takeAsk(v.ID)
	return h.resolved(v.ID, msg)
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
// reply, returning true if it consumed the message (acking on success). A
// stale entry — the question was answered in the web UI meanwhile — returns
// false so the text is routed to the session as a normal prompt instead of
// being swallowed; a transport failure keeps the entry so retyping retries.
func (h *Hub) answerByText(ctx context.Context, sessionID, messageID, text string) bool {
	id, entry, ok := h.takeAskBySession(sessionID)
	if !ok {
		return false
	}
	err := h.router.RespondInteraction(id, hook.Response{
		Behavior: "allow",
		Reason:   "via lark",
		Answers:  map[string]string{entry.question: strings.TrimSpace(text)},
	})
	switch {
	case err == nil:
		h.unclaimPending(id)
		h.ack(ctx, messageID)
		return true
	case isServerReject(err):
		h.logger.Debug("lark: stale ask entry, routing reply as prompt", "id", id, "err", err)
		return false
	default:
		h.putAsk(id, entry) // transport failure: keep the question answerable
		h.logger.Warn("lark: answer ask by text", "id", id, "err", err)
		if root, ok := h.store.root(sessionID); ok {
			h.replyText(ctx, sessionID, root, "⚠️ couldn't deliver the answer (usher unreachable) — please retype it")
		}
		return true
	}
}

func (h *Hub) putAsk(id string, e askEntry) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	h.asks[id] = e
	h.asksBySession[e.session] = id
}

// peekAsk reads a pending question without consuming it.
func (h *Hub) peekAsk(id string) (askEntry, bool) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	e, ok := h.asks[id]
	return e, ok
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
