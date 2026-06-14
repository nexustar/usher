// Package router glues discovery, sender, broker, and hook together. It is
// the central coordinator that the web layer and the Usher Agent both go
// through, and serves as the type that satisfies the agent's AgentAPI
// contract — keeping the agent's surface a strict subset of usher's
// internals.
package router

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"usher/internal/archive"
	"usher/internal/broker"
	"usher/internal/codexrollout"
	"usher/internal/core"
	"usher/internal/discovery"
	"usher/internal/hook"
	"usher/internal/jsonl"
	"usher/internal/sender"
)

// ErrSessionNotFound is returned when an operation targets a session with no
// log on disk (so its path/backend can't be resolved).
var ErrSessionNotFound = errors.New("session not found")

type Router struct {
	discovery *discovery.Discovery
	// senders holds one Sender per backend ("claude", "codex"); usher manages
	// both at once. A send is routed by the session's Backend tag (existing
	// sessions) or the chosen model (new sessions). defaultBackend is the
	// fallback when a backend is unknown/empty.
	senders        map[string]*sender.Sender
	defaultBackend string
	broker         *broker.Broker
	hooks          *hook.Manager
	archive        *archive.Store

	sendMu     sync.Mutex
	activeSend map[string]*sendToken   // sessionID -> latest send's cancel handle
	creating   map[string]core.Session // sessions usher is spawning, not yet on disk
}

// sendToken pairs a cancel function with a unique pointer identity so that a
// finishing goroutine only deletes its own entry — never the entry of a
// later send that replaced it.
type sendToken struct {
	cancel context.CancelFunc
}

// New builds a Router over the given backends (at least one). senders maps a
// backend name ("claude"/"codex") to its Sender; defaultBackend names the one to
// fall back to for unknown/empty backends and is the new-session default — it
// must be a key in senders.
func New(d *discovery.Discovery, senders map[string]*sender.Sender, defaultBackend string, b *broker.Broker, h *hook.Manager, archiveStore *archive.Store) *Router {
	return &Router{
		discovery:      d,
		senders:        senders,
		defaultBackend: defaultBackend,
		broker:         b,
		hooks:          h,
		archive:        archiveStore,
		activeSend:     map[string]*sendToken{},
		creating:       map[string]core.Session{},
	}
}

// Backends returns the enabled backend names, sorted ("claude" before "codex").
// The web layer uses it to show only available backends in the model picker.
func (r *Router) Backends() []string {
	out := make([]string, 0, len(r.senders))
	for b := range r.senders {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// senderForBackend returns the Sender for a backend, falling back to the
// default when the backend is empty or unregistered.
func (r *Router) senderForBackend(backend string) *sender.Sender {
	if s, ok := r.senders[backend]; ok {
		return s
	}
	return r.senders[r.defaultBackend]
}

// senderFor returns the Sender owning an existing session, by its Backend tag.
func (r *Router) senderFor(id string) *sender.Sender {
	if s, ok := r.discovery.Get(id); ok {
		return r.senderForBackend(s.Backend)
	}
	return r.senders[r.defaultBackend]
}

// anyHas reports whether any backend's sender holds a live process for id —
// used for hook ownership, where the session's backend may not be resolved yet.
func (r *Router) anyHas(id string) bool {
	for _, s := range r.senders {
		if s.Has(id) {
			return true
		}
	}
	return false
}

// liveSet unions the live-session ids across every backend's sender.
func (r *Router) liveSet() map[string]bool {
	set := map[string]bool{}
	for _, s := range r.senders {
		for _, id := range s.LiveSessions() {
			set[id] = true
		}
	}
	return set
}

// backendOf returns the backend a session belongs to, or the default if the
// session isn't known to discovery yet.
func (r *Router) backendOf(id string) string {
	if s, ok := r.discovery.Get(id); ok && s.Backend != "" {
		return s.Backend
	}
	return r.defaultBackend
}

// readTurnsForBackend parses a session log into display turns using the parser
// for its backend: Codex rollouts vs Claude jsonl. Both return the same shape.
func readTurnsForBackend(path, backend string, limit int) ([]jsonl.Turn, int, error) {
	if backend == "codex" {
		return codexrollout.ReadTurns(path, limit)
	}
	return jsonl.ReadTurns(path, limit)
}

// ReadTurns resolves a session's log path and backend and returns its grouped
// display turns (and the pre-trim total). Returns ErrSessionNotFound when the
// session has no log on disk.
func (r *Router) ReadTurns(id string, limit int) ([]jsonl.Turn, int, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, 0, ErrSessionNotFound
	}
	return readTurnsForBackend(path, r.backendOf(id), limit)
}

// BackendForModel exposes backendForModel to other packages (the web layer's
// model gate) — which backend a chosen model routes to.
func BackendForModel(model string) string { return backendForModel(model) }

// backendForModel maps a new-session model choice to its backend. Model names
// are unique across backends except the literal "default" (the UI resolves that
// to an explicit backend); gpt-*/o-series/codex are Codex, everything else
// (claude-*, opus, sonnet, haiku, fable) is Claude.
func backendForModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "gpt"), strings.HasPrefix(m, "o1"),
		strings.HasPrefix(m, "o3"), strings.HasPrefix(m, "o4"),
		strings.Contains(m, "codex"):
		return "codex"
	default:
		return "claude"
	}
}

// --- session reads -------------------------------------------------------

// ListSessions returns sessions decorated with their current run state: a
// turn in flight is "running"; otherwise a warm pooled process is "live".
func (r *Router) ListSessions() []core.Session {
	sessions := r.discovery.List()
	live := r.liveSet()
	r.sendMu.Lock()
	known := make(map[string]bool, len(sessions))
	for i := range sessions {
		known[sessions[i].ID] = true
		if _, running := r.activeSend[sessions[i].ID]; running {
			sessions[i].Status = core.StatusRunning
		} else if live[sessions[i].ID] {
			sessions[i].Status = core.StatusLive
		}
	}
	// Prepend sessions still being created (newest, not yet on disk) so a
	// just-created session shows in the list before its first jsonl write.
	var pending []core.Session
	for id, s := range r.creating {
		if !known[id] {
			pending = append(pending, s)
		}
	}
	r.sendMu.Unlock()
	return append(pending, sessions...)
}

func (r *Router) GetSession(id string) (core.Session, bool) {
	sess, ok := r.discovery.Get(id)
	if !ok {
		// Not on disk yet — fall back to the creating-overlay so a just-spawned
		// session's detail view opens instead of 404ing.
		r.sendMu.Lock()
		sess, ok = r.creating[id]
		r.sendMu.Unlock()
		return sess, ok
	}
	r.sendMu.Lock()
	_, running := r.activeSend[id]
	r.sendMu.Unlock()
	if running {
		sess.Status = core.StatusRunning
	} else if r.senderForBackend(sess.Backend).Has(id) {
		sess.Status = core.StatusLive
	}
	return sess, true
}

func (r *Router) SessionPath(id string) (string, bool) {
	return r.discovery.Path(id)
}

// ForkSession branches the conversation of srcID into a brand-new session:
// a prefix copy of its jsonl through the turn containing afterUUID (see
// jsonl.ForkCopy), under a fresh id in the same project dir. Pure file
// operation — no process is spawned; the fork is resumed lazily by the pool
// on its first send, like any idle session. Returns the new session id.
func (r *Router) ForkSession(srcID, afterUUID string) (string, error) {
	path, ok := r.discovery.Path(srcID)
	if !ok {
		return "", ErrSessionNotFound
	}
	newID := newUUIDv4()
	dir := filepath.Dir(path)

	var dstPath string
	var err error
	if r.backendOf(srcID) == "codex" {
		// Codex rollout: name the fork rollout-<ts>-<id>.jsonl (the shape discovery
		// matches) and truncate at the turn whose task_complete is afterUUID.
		dstPath = filepath.Join(dir, codexrollout.RolloutFilename(newID, time.Now()))
		err = codexrollout.ForkCopy(path, dstPath, afterUUID, newID, srcID)
	} else {
		dstPath = filepath.Join(dir, newID+".jsonl")
		err = jsonl.ForkCopy(path, dstPath, afterUUID, newID)
	}
	if err != nil {
		return "", err
	}
	// Ingest synchronously so the id resolves the moment the client navigates
	// to it, instead of racing the fsnotify watcher.
	r.discovery.Upsert(dstPath)
	return newID, nil
}

// IsArchived reports whether sessionID is archived in the default
// sidebar view. Wraps archive.Store.IsArchived with the session's
// last_event_at from discovery; returns false for unknown ids.
func (r *Router) IsArchived(sessionID string) bool {
	if r.archive == nil {
		return false
	}
	sess, ok := r.discovery.Get(sessionID)
	if !ok {
		return false
	}
	return r.archive.IsArchived(sessionID, sess.LastEventAt, time.Now())
}

// Archive marks sessionID as manually archived.
func (r *Router) Archive(sessionID string) {
	if r.archive != nil {
		r.archive.Archive(sessionID)
	}
}

// Unarchive removes the archive decision for sessionID. Looks up the
// session's last_event_at from discovery so the store can pick between
// "delete entry" (fresh — let auto-archive resume later) and "write DecisionShown"
// (stale — would otherwise re-archive on next IsArchived call). A missing
// session leaves lastEventAt zero, which the store treats as stale.
func (r *Router) Unarchive(sessionID string) {
	if r.archive == nil {
		return
	}
	sess, _ := r.discovery.Get(sessionID)
	r.archive.Unarchive(sessionID, sess.LastEventAt, time.Now())
}

// DeleteSession permanently removes a session: it cancels any in-flight turn,
// kills usher's live window for it (if any), deletes the session jsonl from
// disk, and forgets all per-session state (archive decision, auto-approve,
// remembered permission rules). Irreversible — the conversation is gone with
// the file. Errors if the session is unknown or the file delete fails; the
// live-process teardown is best-effort. Unlike Archive (a reversible sidebar
// hide), this is destructive.
func (r *Router) DeleteSession(id string) error {
	path, ok := r.discovery.Path(id)
	if !ok {
		return errors.New("session not found")
	}
	// Release any in-flight turn first so its tail goroutine stops before the
	// file is pulled out from under it.
	r.sendMu.Lock()
	tok := r.activeSend[id]
	r.sendMu.Unlock()
	if tok != nil {
		tok.cancel()
	}
	if err := r.senderFor(id).Kill(id); err != nil {
		slog.Warn("kill session window on delete", "session", id, "err", err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete session file: %w", err)
	}
	// Forget it synchronously so the id stops resolving immediately instead of
	// racing the fsnotify Remove event (mirror of fork's Upsert).
	r.discovery.Remove(id)
	if r.archive != nil {
		r.archive.Forget(id)
	}
	r.hooks.SetAutoApprove(id, false)
	r.hooks.ForgetSessionRules(id)
	return nil
}

// --- session writes ------------------------------------------------------

// SendToSession spawns a fire-and-forget subprocess for the session. Stream
// events go to broker subscribers. Returns immediately once the subprocess
// is started, or with an error if the session is unknown.
func (r *Router) SendToSession(id, text string) error {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return errors.New("session not found")
	}
	// A '!'-prefixed message is not a model turn: Claude Code runs it as a TUI
	// bash command. That is a feature claude already has — usher is not adding
	// command execution, only keeping such a message (which bracketed paste
	// can't neutralize, unlike a leading '/' or '@') from wedging the turn
	// tailer, since bash mode logs no turn_duration for it to wait on.
	if strings.HasPrefix(text, "!") {
		go r.injectDirect(sess.ID, text, sess.Cwd)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	tok := &sendToken{cancel: cancel}

	r.sendMu.Lock()
	r.activeSend[id] = tok
	r.sendMu.Unlock()

	go r.runSend(ctx, sess.ID, text, sess.Cwd, tok)
	return nil
}

// injectDirect pastes text without turn tracking (see the '!' note in
// SendToSession), then emits the turn.user + subprocess.exit events a normal
// turn would, so the web client adopts the echo and returns to idle with no
// special-casing. No activeSend: nothing to cancel, and the session stays
// "live" rather than "running". The 45s budget covers a cold window's resume.
func (r *Router) injectDirect(sessionID, text, cwd string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := r.senderFor(sessionID).Inject(ctx, sessionID, text, cwd); err != nil {
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
		return
	}
	uraw, _ := json.Marshal(map[string]any{"role": "user", "content": text, "ts": time.Now().UTC()})
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: "turn.user", Raw: uraw})
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
}

func (r *Router) runSend(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken) {
	defer r.releaseSend(sessionID, tok)

	ch, err := r.senderFor(sessionID).Send(ctx, sessionID, prompt, cwd)
	if err != nil {
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		return
	}
	asm := newStreamAssembler(r.backendOf(sessionID))
	for ev := range ch {
		r.publishStream(sessionID, asm, ev)
	}
}

// publishStream forwards one tail event to broker subscribers, deriving the
// display-ready turn events alongside the raw line:
//
//   - the raw event keeps flowing under its jsonl type ("user", "assistant",
//     "system", …) for non-web consumers (other frontends consume these); the
//     web SSE layer filters them out in favour of the derived events, which
//     are far smaller on the wire (no thinking blocks, usage stats, or file
//     snapshots).
//   - "part": one TurnPart appended to the in-progress assistant turn,
//     grouped/rendered server-side by jsonl.Assembler — the same engine
//     behind ReadTurns, so a part streamed live and the same turn fetched
//     later from /transcript can never disagree.
//   - "turn.user": a canonical user turn hit the jsonl (the prompt usher just
//     injected, or one queued from another frontend mid-turn). Carries the
//     persisted timestamp, so clients can stamp their optimistic echo.
//
// Returns the appended part (nil otherwise) so callers that accumulate the
// turn's text (CreateSession) don't re-parse the payload.
// streamAssembler is the cross-backend turn-grouping engine: both
// jsonl.Assembler (Claude) and codexrollout.Assembler (Codex) implement it, so
// publishStream derives the same turn.user/part events from either backend's
// log lines.
type streamAssembler interface {
	FeedLine(raw []byte) (completed []jsonl.Turn, part *jsonl.TurnPart)
	Model() string
}

var (
	_ streamAssembler = (*jsonl.Assembler)(nil)
	_ streamAssembler = (*codexrollout.Assembler)(nil)
)

// newStreamAssembler returns the assembler for a backend's log shape.
func newStreamAssembler(backend string) streamAssembler {
	if backend == "codex" {
		return codexrollout.NewAssembler()
	}
	return jsonl.NewAssembler()
}

// isControlEvent reports whether a StreamEvent is a synthesized control signal
// (not a backend log line) and so must not be fed to the assembler.
func isControlEvent(t string) bool {
	return t == "subprocess.started" || t == "subprocess.exit" || t == "error"
}

// lineTimestamp pulls the top-level "timestamp" from a log line (present on both
// Claude and Codex lines); zero time if absent.
func lineTimestamp(raw json.RawMessage) time.Time {
	var o struct {
		Timestamp time.Time `json:"timestamp"`
	}
	_ = json.Unmarshal(raw, &o)
	return o.Timestamp
}

func (r *Router) publishStream(sessionID string, asm streamAssembler, ev sender.StreamEvent) *jsonl.TurnPart {
	if ev.Type == "subprocess.exit" {
		ev.Raw = r.enrichExitWithTurnTimestamps(sessionID, ev.Raw)
	}
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: ev.Type, Raw: ev.Raw})

	if isControlEvent(ev.Type) {
		return nil
	}
	// Feed every log line; the assembler ignores non-conversational ones
	// (Claude's system events, Codex's session_meta/token_count, etc.).
	completed, part := asm.FeedLine(ev.Raw)
	for _, t := range completed {
		if t.Role != "user" {
			// Assistant turns are finalized client-side by the turn-end
			// transcript truth-up; no event needed.
			continue
		}
		raw, mErr := json.Marshal(map[string]any{"role": "user", "content": t.Content, "ts": t.Time})
		if mErr == nil {
			r.broker.Publish(broker.Event{SessionID: sessionID, Type: "turn.user", Raw: raw})
		}
	}
	if part != nil {
		raw, mErr := json.Marshal(map[string]any{
			"role": "assistant", "ts": lineTimestamp(ev.Raw), "model": asm.Model(), "part": part,
		})
		if mErr == nil {
			r.broker.Publish(broker.Event{SessionID: sessionID, Type: "part", Raw: raw})
		}
	}
	return part
}

// enrichExitWithTurnTimestamps reads the last two user/assistant turns from
// the session jsonl and injects their timestamps into the exit event so
// the web UI can replace its optimistically-stamped chat messages with the
// canonical server timestamps. Best-effort: any read failure leaves the
// payload untouched.
func (r *Router) enrichExitWithTurnTimestamps(sessionID string, raw json.RawMessage) json.RawMessage {
	path, ok := r.discovery.Path(sessionID)
	if !ok {
		return raw
	}
	turns, _, err := readTurnsForBackend(path, r.backendOf(sessionID), 2)
	if err != nil || len(turns) == 0 {
		return raw
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		payload = map[string]any{}
	}
	if len(turns) >= 2 && turns[len(turns)-2].Role == "user" {
		payload["user_ts"] = turns[len(turns)-2].Time
	}
	if turns[len(turns)-1].Role == "assistant" {
		payload["assistant_ts"] = turns[len(turns)-1].Time
		// Fork point of the turn that just finished, so the client can arm the
		// fork control on the promoted-in-place bubble without a refetch.
		payload["assistant_uuid"] = turns[len(turns)-1].UUID
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return b
}

func (r *Router) releaseSend(sessionID string, tok *sendToken) {
	r.sendMu.Lock()
	if cur, ok := r.activeSend[sessionID]; ok && cur == tok {
		delete(r.activeSend, sessionID)
	}
	delete(r.creating, sessionID) // discovery owns it once the turn hit disk
	r.sendMu.Unlock()
	tok.cancel()
}

// CancelSend stops the in-flight turn for sessionID. It both cancels usher's
// tail goroutine (tok.cancel) and interrupts the live interactive claude with
// Ctrl-C — the process is persistent now, so cancelling the listener alone
// would leave claude generating into the void.
func (r *Router) CancelSend(sessionID string) error {
	r.sendMu.Lock()
	tok, ok := r.activeSend[sessionID]
	r.sendMu.Unlock()
	if !ok {
		return errors.New("no active send")
	}
	if err := r.senderFor(sessionID).Interrupt(sessionID); err != nil {
		slog.Warn("interrupt session turn", "session", sessionID, "err", err)
	}
	tok.cancel()
	return nil
}

func (r *Router) SubscribeSession(id string) (<-chan broker.Event, func()) {
	return r.broker.Subscribe(id)
}

// --- terminal mirror -----------------------------------------------------

// CaptureScreen returns the current rendered pane contents (with colour
// escapes) for a session usher holds a live process for — the read-only
// terminal mirror's frame source. Ownership is required: there's no pane to
// mirror unless usher has a live window (sender.Has), and we must not reach
// into the user's own terminal/IDE claude on a shared socket.
func (r *Router) CaptureScreen(id string) (string, error) {
	snd := r.senderFor(id)
	if !snd.Has(id) {
		return "", errors.New("session not live")
	}
	return snd.CapturePane(id)
}

// SendKeys forwards navigation keys to a live session's pane, powering the
// terminal mirror's soft keys. Same ownership gate as CaptureScreen. The web
// layer restricts which key names reach here; this only enforces ownership.
func (r *Router) SendKeys(id string, keys ...string) error {
	snd := r.senderFor(id)
	if !snd.Has(id) {
		return errors.New("session not live")
	}
	if err := snd.SendKeys(id, keys...); err != nil {
		return err
	}
	// Esc while a turn is running interrupts claude in the pane, but an
	// interrupted turn never logs the turn_duration our tailer waits on — so the
	// turn would stick as "running" forever. Release it the same way the cancel
	// button does (cancel the tail ctx); the tailer then emits subprocess.exit
	// and clients recover live. No-op when no turn is in flight.
	for _, k := range keys {
		if k == "Escape" {
			r.sendMu.Lock()
			tok := r.activeSend[id]
			r.sendMu.Unlock()
			if tok != nil {
				tok.cancel()
			}
			break
		}
	}
	return nil
}

// ResizeCanvas sets the mirror's pane to cols×rows (and repairs any
// manual-attach drift). Called when a /screen stream opens, with cols and rows
// derived client-side from the viewer. Same ownership gate; a no-op error for
// unowned sessions is ignored by the caller.
func (r *Router) ResizeCanvas(id string, cols, rows int) error {
	snd := r.senderFor(id)
	if !snd.Has(id) {
		return errors.New("session not live")
	}
	return snd.ResizeCanvas(id, cols, rows)
}

// --- hook / interactions -------------------------------------------------

func (r *Router) ListPendingInteractions() []hook.Pending { return r.hooks.List() }

func (r *Router) RespondInteraction(id string, resp hook.Response) error {
	return r.hooks.Respond(id, resp)
}

// HandleHook applies a remembered per-session decision first, then blocks for
// the web UI when usher owns the session. Ownership = the session has a live
// window in usher's process pool (sender.Has) — a simple membership test, NOT
// whether a turn is currently executing. The old activeSend (turn-in-flight)
// gate raced the send/inject/turn lifecycle and bounced mid-turn tool prompts
// back to the pane; pool membership is the stable signal. It also keeps usher
// from intercepting the user's own terminal/IDE claude (not in our pool), which
// on a shared default socket would otherwise reach this same hook server.
func (r *Router) HandleHook(ctx context.Context, ev hook.Event) (hook.Response, error) {
	if resp, ok := r.hooks.QuickDecide(ev); ok {
		return resp, nil
	}
	if !r.anyHas(ev.SessionID) {
		return hook.Response{}, errors.New("session not owned by usher")
	}
	return r.hooks.Submit(ctx, ev)
}

func (r *Router) SetAutoApprove(sessionID string, enabled bool) {
	r.hooks.SetAutoApprove(sessionID, enabled)
}

func (r *Router) IsAutoApprove(sessionID string) bool {
	return r.hooks.IsAutoApprove(sessionID)
}

// --- transcript / blocking send (v0.2 LLM agent helpers) ----------------

// ReadSessionTranscript projects the most recent N user/assistant turns of
// a session's jsonl into core.TranscriptTurn. limit ≤ 0 returns everything.
func (r *Router) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, ErrSessionNotFound
	}
	turns, _, err := readTurnsForBackend(path, r.backendOf(id), limit)
	if err != nil {
		return nil, err
	}
	out := make([]core.TranscriptTurn, len(turns))
	for i, t := range turns {
		out[i] = core.TranscriptTurn{Role: t.Role, Content: t.Content, Time: t.Time}
	}
	return out, nil
}

// SendToSessionAndWait spawns the same fire-and-forget send as
// SendToSession but blocks until subprocess.exit (or timeout / ctx cancel),
// returning the accumulated assistant text streamed during this turn.
//
// We subscribe to the broker BEFORE issuing the send so no events are
// missed in the window between SendToSession returning and the subscriber
// being attached.
func (r *Router) SendToSessionAndWait(ctx context.Context, id, text string, timeout time.Duration) (string, error) {
	if _, ok := r.discovery.Get(id); !ok {
		return "", errors.New("session not found")
	}
	ch, cancel := r.broker.Subscribe(id)
	defer cancel()

	if err := r.SendToSession(id, text); err != nil {
		return "", err
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
	defer waitCancel()

	var buf strings.Builder
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return buf.String(), nil
			}
			switch ev.Type {
			case "part":
				// Message granularity (interactive claude emits no stream-json
				// token deltas): each text part carries one assistant message's
				// text blocks. Accumulate them across the turn.
				if t := partText(ev.Raw); t != "" {
					if buf.Len() > 0 {
						buf.WriteString("\n")
					}
					buf.WriteString(t)
				}
			case "subprocess.exit":
				return buf.String(), nil
			case "error":
				return buf.String(), errors.New(extractErrorMessage(ev.Raw))
			}
		case <-waitCtx.Done():
			if buf.Len() == 0 {
				return "", errors.New("timeout (no response received)")
			}
			return buf.String(), fmt.Errorf("timeout after %d chars (partial response retained)", buf.Len())
		}
	}
}

// partText returns the text content of a "part" broker event — assistant
// text parts only; tool parts and malformed payloads yield "".
func partText(raw json.RawMessage) string {
	var o struct {
		Role string `json:"role"`
		Part struct {
			Type    string `json:"type"`
			Content string `json:"content"`
		} `json:"part"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return ""
	}
	if o.Role != "assistant" || o.Part.Type != "text" {
		return ""
	}
	return o.Part.Content
}

func extractErrorMessage(raw json.RawMessage) string {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &e)
	if e.Message == "" {
		return "unknown error"
	}
	return e.Message
}

// StartSession spawns a brand-new Claude Code session in cwd and returns
// immediately with the generated session id. Stream events flow to broker
// subscribers — callers that want the first response inline should use
// CreateSession instead. Registered in activeSend so CancelSend works.
func (r *Router) StartSession(cwd, initialMsg, model string) (string, error) {
	if err := validateCreateInputs(cwd, initialMsg); err != nil {
		return "", err
	}
	backend := backendForModel(model)
	snd := r.senderForBackend(backend)
	if !snd.PreAssignsID() {
		// Codex assigns its own id; spawn, discover it, register under it.
		return r.startDiscoveredSession(cwd, initialMsg, model, backend, snd)
	}

	sessionID := newUUIDv4()
	ctx, cancel := context.WithCancel(context.Background())
	tok := &sendToken{cancel: cancel}

	now := time.Now()
	r.sendMu.Lock()
	r.activeSend[sessionID] = tok
	// Surface it now: discovery won't see it until claude writes the first
	// jsonl line, so without this the detail view 404s. Dropped in releaseSend.
	r.creating[sessionID] = core.Session{
		ID:          sessionID,
		Cwd:         cwd,
		Status:      core.StatusRunning,
		StartedAt:   now,
		LastEventAt: now,
		Backend:     backend,
	}
	r.sendMu.Unlock()

	go r.runStart(ctx, sessionID, initialMsg, cwd, model, tok)
	return sessionID, nil
}

// codexDiscoverTimeout bounds how long StartSession blocks waiting for Codex to
// write its new session log (and so reveal the id it assigned itself).
const codexDiscoverTimeout = 20 * time.Second

// startDiscoveredSession creates a new session for a backend that assigns its
// own id (Codex). It spawns under a temporary handle, blocks until the real id
// is discovered, then registers creating/activeSend/broker under that id — first
// and only id, no placeholder, no re-keying. The turn then streams in the
// background like a Claude start.
func (r *Router) startDiscoveredSession(cwd, initialMsg, model, backend string, snd *sender.Sender) (string, error) {
	tempID := newUUIDv4()
	ctx, cancel := context.WithCancel(context.Background())
	realID, ch, err := snd.StartCodexSession(ctx, tempID, initialMsg, cwd, model, codexDiscoverTimeout)
	if err != nil {
		cancel()
		return "", err
	}

	tok := &sendToken{cancel: cancel}
	now := time.Now()
	r.sendMu.Lock()
	r.activeSend[realID] = tok
	r.creating[realID] = core.Session{
		ID:          realID,
		Cwd:         cwd,
		Status:      core.StatusRunning,
		StartedAt:   now,
		LastEventAt: now,
		Backend:     backend,
	}
	r.sendMu.Unlock()

	go func() {
		defer r.releaseSend(realID, tok)
		asm := newStreamAssembler(backend)
		for ev := range ch {
			r.publishStream(realID, asm, ev)
		}
	}()
	return realID, nil
}

func (r *Router) runStart(ctx context.Context, sessionID, prompt, cwd, model string, tok *sendToken) {
	defer r.releaseSend(sessionID, tok)
	ch, err := r.senderForBackend(backendForModel(model)).SendNew(ctx, sessionID, prompt, cwd, model)
	if err != nil {
		errMsg, _ := json.Marshal(map[string]string{"message": err.Error()})
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "error", Raw: errMsg})
		return
	}
	asm := newStreamAssembler(backendForModel(model))
	for ev := range ch {
		r.publishStream(sessionID, asm, ev)
	}
}

func validateCreateInputs(cwd, initialMsg string) error {
	if cwd == "" {
		return errors.New("cwd is required")
	}
	if info, err := os.Stat(cwd); err != nil {
		return fmt.Errorf("cwd %q: %w", cwd, err)
	} else if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", cwd)
	}
	if initialMsg == "" {
		return errors.New("initial_message is required")
	}
	return nil
}

// CreateSession spawns a brand-new Claude Code session in cwd and waits for
// the assistant's first response (subject to timeout). Returns the
// generated session id and the accumulated assistant text. The session
// will appear in discovery via fsnotify shortly after the subprocess
// starts writing its jsonl.
func (r *Router) CreateSession(ctx context.Context, cwd, initialMsg string, timeout time.Duration) (string, string, error) {
	if err := validateCreateInputs(cwd, initialMsg); err != nil {
		return "", "", err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Create in the default backend with its default model (empty model → the
	// CLI's own default). Claude lets usher pick the id up front; Codex assigns
	// its own, so discover it first (mirrors StartSession's preAssignsID split).
	snd := r.senderForBackend(r.defaultBackend)
	var sessionID string
	var ch <-chan sender.StreamEvent
	var err error
	if snd.PreAssignsID() {
		sessionID = newUUIDv4()
		ch, err = snd.SendNew(waitCtx, sessionID, initialMsg, cwd, "")
	} else {
		sessionID, ch, err = snd.StartCodexSession(waitCtx, newUUIDv4(), initialMsg, cwd, "", codexDiscoverTimeout)
	}
	if err != nil {
		return "", "", err
	}

	var buf strings.Builder
	asm := newStreamAssembler(r.defaultBackend)
	for ev := range ch {
		// Forward to broker (raw + derived part/turn.user events) so any
		// session-detail subscriber that opens the new tab sees the live
		// stream too.
		if p := r.publishStream(sessionID, asm, ev); p != nil && p.Type == "text" {
			if buf.Len() > 0 {
				buf.WriteString("\n")
			}
			buf.WriteString(p.Content)
		}
	}

	if waitCtx.Err() != nil && buf.Len() == 0 {
		return sessionID, "", fmt.Errorf("create_session timeout (no response received within %s)", timeout)
	}
	return sessionID, buf.String(), nil
}

// newUUIDv4 produces a randomly-generated UUIDv4 string. We avoid the
// google/uuid dep — Claude Code accepts any RFC 4122 v4 string.
func newUUIDv4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
