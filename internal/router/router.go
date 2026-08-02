// Package router glues discovery, sender, broker, and hook together. It is
// the central coordinator that the web layer and the Usher Agent both go
// through, and serves as the type that satisfies the agent's AgentAPI
// contract — keeping the agent's surface a strict subset of usher's
// internals.
package router

import (
	"context"
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
	"unicode"

	backendpkg "github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/sender"
	"github.com/nexustar/usher/internal/sessionmeta"
	"github.com/nexustar/usher/internal/terminal"
	"github.com/nexustar/usher/internal/textutil"
)

// ErrSessionNotFound is returned when an operation targets a session with no
// log on disk (so its path/backend can't be resolved).
var ErrSessionNotFound = errors.New("session not found")
var ErrBackendUnavailable = errors.New("backend capability unavailable")
var ErrInvalidTitle = errors.New("session title must not be empty")

type Router struct {
	discovery *discovery.Discovery
	// backends explicitly compose each agent's runtime and optional capabilities.
	// A send is routed by the session's Backend tag (existing
	// sessions) or the chosen model (new sessions). defaultBackend is the
	// fallback when a backend is unknown/empty.
	backends       map[string]backendpkg.Backend
	defaultBackend string
	broker         *broker.Broker
	interactions   *interaction.Manager
	meta           *sessionmeta.Store
	terminal       *terminal.Manager
	composerMu     sync.Mutex
	composerCache  map[composerCacheKey][]backendpkg.ComposerItem

	sendMu     sync.Mutex
	activeSend map[string]*sendToken    // sessionID -> in-flight turn's cancel handle
	sendQueue  map[string][]pendingSend // sessionID -> sends waiting for the turn to end
	creating   map[string]core.Session  // sessions usher is spawning, not yet on disk

	// Foreign-turn watcher state (see foreignwatch.go): per-session log-size
	// baseline (guarded by sendMu) and the completion handler.
	foreignBase   map[string]int64
	onForeignTurn ForeignTurnHandler

	// runTurn overrides the turn executor ((*Router).runSend) in tests, so
	// queue mechanics are testable without a live tmux sender. nil in
	// production.
	runTurn func(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken)
}

// startTurn launches one turn's executor goroutine (the seam runTurn tests
// override).
func (r *Router) startTurn(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken) {
	run := r.runTurn
	if run == nil {
		run = r.runSend
	}
	go run(ctx, sessionID, prompt, cwd, tok)
}

// sendToken pairs a cancel function with a unique pointer identity so that a
// finishing goroutine only deletes its own entry — never the entry of a
// later send that replaced it.
type sendToken struct {
	cancel context.CancelFunc
}

// pendingSend is one send waiting in a session's FIFO queue behind an
// in-flight turn. pre (optional) runs just before the turn starts — after
// every event of the previous turn has been published — so a relay collector
// can subscribe with correct turn attribution. abort (optional) runs instead
// if the queued send is dropped (session deleted, turn cancelled).
type pendingSend struct {
	text  string
	pre   func()
	abort func(err error)
}

// maxQueuedSends bounds one session's send queue — a backstop against a
// looping agent, far above anything a human produces.
const maxQueuedSends = 32

// New builds a Router over explicitly assembled backends (at least one).
func New(d *discovery.Discovery, backends map[string]backendpkg.Backend, defaultBackend string, b *broker.Broker, h *interaction.Manager, meta *sessionmeta.Store, term *terminal.Manager) *Router {
	r := &Router{
		discovery:      d,
		backends:       backends,
		defaultBackend: defaultBackend,
		broker:         b,
		interactions:   h,
		meta:           meta,
		terminal:       term,
		activeSend:     map[string]*sendToken{},
		sendQueue:      map[string][]pendingSend{},
		creating:       map[string]core.Session{},
		composerCache:  map[composerCacheKey][]backendpkg.ComposerItem{},
	}
	if meta != nil {
		for _, backend := range backends {
			if setter, ok := backend.Runtime.(backendpkg.SystemPrompter); ok {
				setter.SetSystemPromptLookup(meta.AppendSystemPrompt)
			}
		}
	}
	return r
}

type composerCacheKey struct {
	backend string
	cwd     string
}

// Backends returns the enabled backend names, sorted ("claude" before "codex").
// The web layer uses it to show only available backends in the model picker.
func (r *Router) Backends() []string {
	out := make([]string, 0, len(r.backends))
	for b := range r.backends {
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}

// ComposerItems returns an authoritative cwd-aware catalog. A cold backend
// falls back to the last catalog observed for the same backend and cwd.
func (r *Router) ComposerItems(ctx context.Context, id, cwd string) (backendpkg.ComposerCatalog, error) {
	key := composerCacheKey{backend: r.backendOf(id), cwd: cwd}
	provider, ok := r.senderFor(id).(backendpkg.ComposerProvider)
	if !ok {
		cached, _ := r.cachedComposerItems(key)
		return cached, nil
	}
	catalog, err := provider.ComposerItems(ctx, id, cwd)
	if err != nil {
		cached, ok := r.cachedComposerItems(key)
		if ok {
			return cached, nil
		}
		return backendpkg.ComposerCatalog{}, err
	}
	if !catalog.Available {
		cached, _ := r.cachedComposerItems(key)
		return cached, nil
	}

	items := append([]backendpkg.ComposerItem(nil), catalog.Items...)
	sort.SliceStable(items, func(i, j int) bool {
		if a, b := composerKindRank(items[i].Kind), composerKindRank(items[j].Kind); a != b {
			return a < b
		}
		ai, aj := strings.ToLower(items[i].Name), strings.ToLower(items[j].Name)
		if ai != aj {
			return ai < aj
		}
		return items[i].Name < items[j].Name
	})
	r.composerMu.Lock()
	r.composerCache[key] = items
	r.composerMu.Unlock()
	catalog.Items = append([]backendpkg.ComposerItem(nil), items...)
	return catalog, nil
}

func composerKindRank(kind string) int {
	switch kind {
	case "command":
		return 0
	case "extension":
		return 1
	case "prompt":
		return 2
	case "skill":
		return 3
	default:
		return 4
	}
}

func (r *Router) cachedComposerItems(key composerCacheKey) (backendpkg.ComposerCatalog, bool) {
	r.composerMu.Lock()
	items, ok := r.composerCache[key]
	items = append([]backendpkg.ComposerItem(nil), items...)
	r.composerMu.Unlock()
	return backendpkg.ComposerCatalog{Items: items, Available: ok}, ok
}

// Models returns the selectable model catalog owned by backendName.
func (r *Router) Models(ctx context.Context, backendName string) ([]backendpkg.Model, error) {
	b, ok := r.backends[backendName]
	if !ok {
		return nil, fmt.Errorf("backend %q is not enabled", backendName)
	}
	if b.Models == nil {
		return nil, fmt.Errorf("%w: backend %q has no model catalog", ErrBackendUnavailable, backendName)
	}
	return b.Models.Models(ctx)
}

// ValidateModel applies a fail-closed model policy. Empty/default selects the
// CLI's own default; every explicit model must appear in the backend catalog.
func (r *Router) ValidateModel(ctx context.Context, backendName, model string) error {
	if model == "" || model == "default" {
		if _, ok := r.backends[backendName]; !ok {
			return fmt.Errorf("backend %q is not enabled", backendName)
		}
		return nil
	}
	b, ok := r.backends[backendName]
	if !ok {
		return fmt.Errorf("backend %q is not enabled", backendName)
	}
	if b.Models == nil {
		return fmt.Errorf("%w: backend %q has no model catalog", ErrBackendUnavailable, backendName)
	}
	if err := b.Models.ValidateModel(ctx, model); err != nil {
		return fmt.Errorf("invalid model %q for backend %q: %w", model, backendName, err)
	}
	return nil
}

func (r *Router) DefaultEffort(ctx context.Context, backendName, model string) string {
	b, ok := r.backends[backendName]
	if !ok || b.Models == nil {
		return ""
	}
	effort, err := b.Models.DefaultEffort(ctx, model)
	if err != nil {
		return ""
	}
	return effort
}

// senderForBackend returns the Sender for a backend, falling back to the
// default when the backend is empty or unregistered.
func (r *Router) senderForBackend(backend string) backendpkg.Runtime {
	if b, ok := r.backends[backend]; ok {
		return b.Runtime
	}
	return r.backends[r.defaultBackend].Runtime
}

// senderFor returns the Sender owning an existing session, by its Backend tag.
func (r *Router) senderFor(id string) backendpkg.Runtime {
	if s, ok := r.discovery.Get(id); ok {
		return r.senderForBackend(s.Backend)
	}
	return r.backends[r.defaultBackend].Runtime
}

// anyHas reports whether any backend's sender holds a live process for id —
// used for hook ownership, where the session's backend may not be resolved yet.
func (r *Router) anyHas(id string) bool {
	for _, b := range r.backends {
		s := b.Runtime
		if s.Has(id) {
			return true
		}
	}
	return false
}

// liveSet unions the live-session ids across every backend's sender.
func (r *Router) liveSet() map[string]bool {
	set := map[string]bool{}
	for _, b := range r.backends {
		s := b.Runtime
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

func (r *Router) transcriptForBackend(name string) (backendpkg.Transcript, error) {
	if b, ok := r.backends[name]; ok && b.Transcript != nil {
		return b.Transcript, nil
	}
	return nil, fmt.Errorf("%w: backend %q has no transcript", ErrBackendUnavailable, name)
}

func (r *Router) readTurnsForBackend(path, name string, limit int) ([]core.Turn, int, error) {
	format, err := r.transcriptForBackend(name)
	if err != nil {
		return nil, 0, err
	}
	return format.ReadTurns(path, limit)
}

// ReadTurns returns a window of a session's grouped display turns, the
// absolute index of the window's first turn, and the full turn count. Indices
// count from the oldest turn, so they stay stable as the transcript grows.
// before is the exclusive upper bound of the window; negative asks for the
// newest turns. Returns ErrSessionNotFound when the session has no log on
// disk.
func (r *Router) ReadTurns(id string, before, limit int) ([]core.Turn, int, int, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, 0, 0, ErrSessionNotFound
	}
	// Backends parse the whole log regardless of limit, so windowing here
	// costs nothing extra.
	turns, total, err := r.readTurnsForBackend(path, r.backendOf(id), 0)
	if err != nil {
		return nil, 0, 0, err
	}
	end := total
	if before >= 0 && before < end {
		end = before
	}
	start := 0
	if limit > 0 && end-limit > start {
		start = end - limit
	}
	return turns[start:end], start, total, nil
}

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
	return r.listSessions(false)
}

// ListSessionsWithSubagents includes read-only child transcripts for the web
// sidebar's per-parent disclosure. Normal callers intentionally see roots only.
func (r *Router) ListSessionsWithSubagents() []core.Session {
	return r.listSessions(true)
}

func (r *Router) listSessions(includeSubagents bool) []core.Session {
	sessions := r.discovery.List()
	if includeSubagents {
		sessions = r.discovery.ListAll()
	}
	live := r.liveSet()
	r.sendMu.Lock()
	known := make(map[string]bool, len(sessions))
	out := sessions[:0]
	for i := range sessions {
		sess := &sessions[i]
		known[sess.ID] = true
		if sess.IsSubagent && !includeSubagents {
			continue
		}
		if _, running := r.activeSend[sess.ID]; running {
			sess.Status = core.StatusRunning
		} else if live[sess.ID] {
			sess.Status = core.StatusLive
		}
		if !sess.IsSubagent {
			r.applyCustomTitle(sess)
		}
		out = append(out, *sess)
	}
	var pending []core.Session
	for id, s := range r.creating {
		if !known[id] {
			r.applyCustomTitle(&s)
			pending = append(pending, s)
		}
	}
	r.sendMu.Unlock()
	return append(pending, out...)
}

func (r *Router) GetSession(id string) (core.Session, bool) {
	sess, ok := r.discovery.Get(id)
	if !ok {
		// Not on disk yet — fall back to the creating-overlay so a just-spawned
		// session's detail view opens instead of 404ing.
		r.sendMu.Lock()
		sess, ok = r.creating[id]
		r.sendMu.Unlock()
		if ok {
			r.applyCustomTitle(&sess)
		}
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
	r.applyCustomTitle(&sess)
	return sess, true
}

func (r *Router) SessionPath(id string) (string, bool) {
	return r.discovery.Path(id)
}

// ForkSession asks the source backend to branch the conversation after the
// turn containing afterUUID and returns the new session id.
func (r *Router) ForkSession(srcID, afterUUID string) (string, error) {
	if sess, ok := r.discovery.Get(srcID); ok && sess.IsSubagent {
		return "", errors.New("subagent transcripts are read-only")
	}
	path, ok := r.discovery.Path(srcID)
	if !ok {
		return "", ErrSessionNotFound
	}
	b := r.backends[r.backendOf(srcID)]
	if b.Forker == nil {
		return "", errors.New("backend does not support session forks")
	}
	newID, dstPath, err := b.Forker.Fork(context.Background(), srcID, path, afterUUID)
	if err != nil {
		return "", err
	}
	if r.meta != nil {
		r.meta.SetAppendSystemPrompt(newID, r.meta.AppendSystemPrompt(srcID))
	}
	// Auto-approve is deliberately not inherited: a branch is a new conversation.
	// Ingest synchronously so the id resolves the moment the client navigates
	// to it, instead of racing the fsnotify watcher.
	r.discovery.Upsert(dstPath)
	return newID, nil
}

// staleClock is what auto-archive measures inactivity from: last user input,
// falling back to last event. Keying on input (not mtime) means pause/kill and
// streaming don't reset the countdown — mirrors discovery's sort.
func staleClock(s core.Session) time.Time {
	if !s.LastInputAt.IsZero() {
		return s.LastInputAt
	}
	return s.LastEventAt
}

func (r *Router) IsArchived(sessionID string) bool {
	sess, ok := r.discovery.Get(sessionID)
	if !ok {
		return false
	}
	return r.meta.IsArchived(sessionID, staleClock(sess), time.Now())
}

func (r *Router) Archive(sessionID string)       { r.meta.Archive(sessionID) }
func (r *Router) IsPinned(sessionID string) bool { return r.meta.IsPinned(sessionID) }
func (r *Router) Pin(sessionID string)           { r.meta.Pin(sessionID) }
func (r *Router) Unpin(sessionID string)         { r.meta.Unpin(sessionID) }
func (r *Router) Rename(ctx context.Context, sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrInvalidTitle
	}
	sess, ok := r.discovery.Get(sessionID)
	if !ok {
		return ErrSessionNotFound
	}
	path, ok := r.discovery.Path(sessionID)
	if !ok {
		return ErrSessionNotFound
	}
	b := r.backends[sess.Backend]
	if b.Renamer == nil {
		// Compatibility path for backends without native rename.
		r.meta.Rename(sessionID, title)
		return nil
	}
	if err := b.Renamer.Rename(ctx, sessionID, path, title); err != nil {
		return err
	}
	// Native metadata supersedes the legacy usher override.
	r.meta.Rename(sessionID, "")
	r.discovery.SetTitle(sessionID, title)
	return nil
}

func (r *Router) applyCustomTitle(s *core.Session) {
	if t := r.meta.CustomTitle(s.ID); t != "" {
		s.Title = t
	}
}

func (r *Router) Unarchive(sessionID string) {
	sess, _ := r.discovery.Get(sessionID)
	r.meta.Unarchive(sessionID, staleClock(sess), time.Now())
}

// DeleteSession permanently removes a session: it cancels any in-flight turn,
// kills usher's live window for it (if any), deletes the session jsonl from
// disk, and forgets all per-session state (archive decision and auto-approve).
// Irreversible — the conversation is gone with
// the file. Errors if the session is unknown or the file delete fails; the
// live-process teardown is best-effort. Unlike Archive (a reversible sidebar
// hide), this is destructive.
func (r *Router) DeleteSession(id string) error {
	if sess, ok := r.discovery.Get(id); ok && sess.IsSubagent {
		return errors.New("subagent transcripts are read-only")
	}
	path, ok := r.discovery.Path(id)
	if !ok {
		return errors.New("session not found")
	}
	// Release any in-flight turn first so its tail goroutine stops before the
	// file is pulled out from under it.
	r.stopLive(id)
	if r.terminal != nil {
		_ = r.terminal.Close(id)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete session file: %w", err)
	}
	// Cascade to this session's read-only subagent transcripts so they don't
	// orphan on disk (and in discovery) with a now-dangling parent.
	r.deleteSubagentTranscripts(id, path)
	// Forget it synchronously so the id stops resolving immediately instead of
	// racing the fsnotify Remove event (mirror of fork's Upsert).
	r.discovery.Remove(id)
	r.meta.Forget(id)
	r.interactions.SetAutoApprove(id, false)
	return nil
}

// deleteSubagentTranscripts removes a parent's read-only subagent transcripts —
// their files and their discovery entries — when the parent is deleted. Walking
// discovery covers both layouts: Claude nests them under <cwd>/<id>/, Codex
// scatters them by parent_thread_id. It then drops Claude's now-empty <cwd>/<id>/
// subtree beside rootPath (a no-op for Codex, whose derived path isn't a dir).
// All disk removal is best-effort; the goal is not to leave orphans behind.
func (r *Router) deleteSubagentTranscripts(parentID, rootPath string) {
	parent, _ := r.discovery.Get(parentID)
	children := make(map[string][]core.Session)
	for _, sub := range r.discovery.ListAll() {
		if sub.IsSubagent && (parent.Backend == "" || sub.Backend == parent.Backend) {
			children[sub.ParentID] = append(children[sub.ParentID], sub)
		}
	}
	var descendants []core.Session
	seen := map[string]bool{}
	var collect func(string)
	collect = func(id string) {
		for _, child := range children[id] {
			if seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			collect(child.ID)
			descendants = append(descendants, child)
		}
	}
	collect(parentID)
	for _, sub := range descendants {
		if subPath, ok := r.discovery.Path(sub.ID); ok {
			if err := os.Remove(subPath); err != nil && !os.IsNotExist(err) {
				slog.Warn("delete subagent transcript", "session", parentID, "subagent", sub.ID, "err", err)
			}
		}
		r.discovery.Remove(sub.ID)
	}
	_ = os.RemoveAll(strings.TrimSuffix(rootPath, ".jsonl"))
}

// PauseSession stops usher's live backend worker without touching its
// transcript or metadata. The conversation cold-resumes on the next Send.
// It is the manual equivalent of LRU eviction.
func (r *Router) PauseSession(id string) error {
	if sess, ok := r.discovery.Get(id); !ok {
		return errors.New("session not found")
	} else if sess.IsSubagent {
		return errors.New("subagent transcripts are read-only")
	}
	r.stopLive(id)
	return nil
}

// ResumeSession brings an idle backend worker online without adding a turn to
// the transcript. It is idempotent for sessions that are already live.
func (r *Router) ResumeSession(ctx context.Context, id string) error {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	if sess.IsSubagent {
		return errors.New("subagent transcripts are read-only")
	}
	return r.senderFor(id).Resume(ctx, id, sess.Cwd)
}

// stopLive drops a session to idle: cancels any in-flight turn, drops queued
// sends, and stops its live worker. Best-effort; idempotent. Shared by
// PauseSession/DeleteSession.
func (r *Router) stopLive(id string) {
	r.flushSendQueue(id, errors.New("session stopped"))
	r.sendMu.Lock()
	tok := r.activeSend[id]
	r.sendMu.Unlock()
	if tok != nil {
		tok.cancel()
	}
	if err := r.senderFor(id).Kill(id); err != nil {
		slog.Warn("kill session window", "session", id, "err", err)
	}
}

// --- session writes ------------------------------------------------------

// SendToSession delivers text to the session as the next turn. If the session
// is idle the turn starts immediately (fire-and-forget; stream events go to
// broker subscribers); if a turn is in flight the send waits in the session's
// FIFO queue and is injected when that turn ends. Returns an error only if
// the session is unknown or the queue is full.
func (r *Router) SendToSession(id, text string) error {
	return r.enqueueSend(id, text, nil, nil)
}

// enqueueSend is the single entry point for turn-tracked sends: run now if
// the session is idle, queue otherwise. Serializing usher-injected turns per
// session is what makes "subscribe, then collect until subprocess.exit" a
// sound way to capture one turn's reply — a send injected mid-turn would
// tail the PREVIOUS turn's remainder instead. Typing directly into an
// attached tmux pane still bypasses this (the manual-attach corner).
func (r *Router) enqueueSend(id, text string, pre func(), abort func(error)) error {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return errors.New("session not found")
	}
	if sess.IsSubagent {
		return errors.New("subagent transcripts are read-only")
	}
	// Reorder the sidebar the instant the user sends, without waiting for the
	// prompt to land in the jsonl (see discovery.MarkInput).
	r.discovery.MarkInput(id, time.Now().UTC())

	r.sendMu.Lock()
	if _, busy := r.activeSend[id]; busy || len(r.sendQueue[id]) > 0 {
		if len(r.sendQueue[id]) >= maxQueuedSends {
			r.sendMu.Unlock()
			return errors.New("send queue full for session")
		}
		r.sendQueue[id] = append(r.sendQueue[id], pendingSend{text: text, pre: pre, abort: abort})
		r.sendMu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	tok := &sendToken{cancel: cancel}
	r.activeSend[id] = tok
	r.sendMu.Unlock()

	if pre != nil {
		pre()
	}
	r.startTurn(ctx, sess.ID, text, sess.Cwd, tok)
	return nil
}

// injectDirect pastes text without turn tracking (see the '!' note in
// SendToSession), then emits the turn.user + subprocess.exit events a normal
// turn would, so the web client adopts the echo and returns to idle with no
// special-casing. No activeSend: nothing to cancel, and the session stays
// "live" rather than "running". The 45s budget covers a cold window's resume.
func (r *Router) runSend(ctx context.Context, sessionID, prompt, cwd string, tok *sendToken) {
	defer r.releaseSend(sessionID, tok)

	format, err := r.transcriptForBackend(r.backendOf(sessionID))
	if err != nil {
		r.failSend(sessionID, tok, err)
		return
	}
	started := time.Now()
	ch, err := r.senderFor(sessionID).Send(ctx, sessionID, prompt, cwd)
	if err != nil {
		r.failSend(sessionID, tok, err)
		return
	}
	asm := format.NewAssembler()
	for ev := range ch {
		// Publish BEFORE clearing the running bit: once activeSend is empty a
		// new send's collector may subscribe, and it must not see this
		// turn's exit.
		r.publishStream(sessionID, asm, ev, started)
		if ev.Type == backendpkg.EventProcessExit {
			r.markSendIdle(sessionID, tok)
		}
	}
}

// failSend terminates a turn that failed before the backend returned an event
// stream. Consumers use process exit as the lifecycle boundary, so an error
// without its matching exit would leave relays and queued sends waiting.
func (r *Router) failSend(sessionID string, tok *sendToken, err error) {
	r.markSendIdle(sessionID, tok)
	errRaw, _ := json.Marshal(backendpkg.ErrorPayload{Message: err.Error()})
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: backendpkg.EventError, Raw: errRaw})
	exitRaw, _ := json.Marshal(map[string]string{"reason": "error"})
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: backendpkg.EventProcessExit, Raw: exitRaw})
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
// isControlEvent reports whether a StreamEvent is a synthesized control signal
// (not a backend log line) and so must not be fed to the assembler.
func isControlEvent(t string) bool {
	return backendpkg.IsControlEvent(t)
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

func (r *Router) publishStream(sessionID string, asm backendpkg.Assembler, ev sender.StreamEvent, started time.Time) []*core.TurnPart {
	if ev.Type == backendpkg.EventRuntime {
		var runtime core.SessionRuntime
		if json.Unmarshal(ev.Raw, &runtime) == nil {
			r.discovery.UpdateRuntime(sessionID, runtime)
		}
	}
	if ev.Type == backendpkg.EventProcessExit {
		// The web refreshes cached session metadata as soon as it receives this
		// event. Ingest the final jsonl state synchronously first instead of
		// racing the asynchronous fsnotify Write event.
		if path, ok := r.discovery.Path(sessionID); ok {
			r.discovery.Upsert(path)
		}
		ev.Raw = r.enrichExitWithTurnTimestamps(sessionID, ev.Raw, started)
	}
	r.broker.Publish(broker.Event{SessionID: sessionID, Type: ev.Type, Raw: ev.Raw})

	if isControlEvent(ev.Type) {
		return nil
	}
	// Feed every log line; the assembler ignores non-conversational ones
	// (Claude's system events, Codex's session_meta/token_count, etc.).
	var completed []core.Turn
	var parts []*core.TurnPart
	if multi, ok := asm.(backendpkg.MultiPartAssembler); ok {
		completed, parts = multi.FeedLineParts(ev.Raw)
	} else {
		var part *core.TurnPart
		completed, part = asm.FeedLine(ev.Raw)
		if part != nil {
			parts = []*core.TurnPart{part}
		}
	}
	for _, t := range completed {
		if t.Role != "user" {
			// Assistant turns are finalized client-side by the turn-end
			// transcript truth-up; no event needed.
			continue
		}
		raw, mErr := json.Marshal(map[string]any{"role": "user", "content": t.Content, "ts": t.Time})
		if mErr == nil {
			r.broker.Publish(broker.Event{SessionID: sessionID, Type: backendpkg.EventTurnUser, Raw: raw})
		}
	}
	for _, part := range parts {
		raw, mErr := json.Marshal(map[string]any{
			"role": "assistant", "ts": lineTimestamp(ev.Raw), "model": asm.Model(), "part": part,
		})
		if mErr == nil {
			r.broker.Publish(broker.Event{SessionID: sessionID, Type: backendpkg.EventPart, Raw: raw})
		}
	}
	return parts
}

// enrichExitWithTurnTimestamps reads the last two user/assistant turns from
// the session jsonl and injects their timestamps into the exit event so
// the web UI can replace its optimistically-stamped chat messages with the
// canonical server timestamps. Best-effort: any read failure leaves the
// payload untouched.
//
// started, when non-zero, is when the send began: a trailing assistant turn
// older than that is the PREVIOUS exchange (this turn logged nothing — idle
// fallback for TUI-local commands, or a cancel before first output) and must
// not be stamped. Zero disables the check (first-turn paths have no previous
// exchange to mistake).
func (r *Router) enrichExitWithTurnTimestamps(sessionID string, raw json.RawMessage, started time.Time) json.RawMessage {
	path, ok := r.discovery.Path(sessionID)
	if !ok {
		return raw
	}
	turns, _, err := r.readTurnsForBackend(path, r.backendOf(sessionID), 2)
	if err != nil || len(turns) == 0 {
		return raw
	}
	if last := turns[len(turns)-1]; !started.IsZero() {
		// A normal completed send must end in an assistant turn. A trailing user
		// means this send was cancelled before output; do not borrow an earlier
		// user's timestamp from a [userA,userB] suffix.
		if last.Role != "assistant" {
			return raw
		}
		end := last.EndTime
		if end.IsZero() {
			end = last.Time
		}
		if end.IsZero() || end.Before(started) {
			return raw
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil || payload == nil {
		payload = map[string]any{}
	}
	applyTurnTimestamps(payload, turns)
	b, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return b
}

// applyTurnTimestamps stamps payload with the trailing user→assistant
// exchange's timestamps plus the assistant uuid (the fork anchor). Shared by
// the live exit enrichment above and the foreign watcher's synthetic exit.
func applyTurnTimestamps(payload map[string]any, turns []core.Turn) {
	last := len(turns) - 1
	if last < 0 {
		return
	}
	if turns[last].Role == "assistant" {
		payload["assistant_ts"] = turns[last].Time
		payload["assistant_uuid"] = turns[last].UUID
		// For turn timing; assistant_ts stays the turn start (web keys on it).
		if !turns[last].EndTime.IsZero() {
			payload["assistant_end_ts"] = turns[last].EndTime
		}
	}
	if last >= 1 && turns[last-1].Role == "user" {
		payload["user_ts"] = turns[last-1].Time
	}
}

func (r *Router) releaseSend(sessionID string, tok *sendToken) {
	r.sendMu.Lock()
	if cur, ok := r.activeSend[sessionID]; ok && cur == tok {
		delete(r.activeSend, sessionID)
	}
	delete(r.creating, sessionID) // discovery owns it once the turn hit disk
	// Belt over markSendIdle's bump — and the ONLY bump for create paths,
	// which end their first turn here without ever calling markSendIdle.
	r.bumpForeignBaseLocked(sessionID)

	// Promote the next queued send. runSend's event loop has ended, so the
	// finished turn is fully published — the promoted send's pre() (relay
	// subscription) can't see stale events.
	var next *pendingSend
	var nextTok *sendToken
	var nextCtx context.Context
	if _, busy := r.activeSend[sessionID]; !busy {
		if q := r.sendQueue[sessionID]; len(q) > 0 {
			n := q[0]
			next = &n
			if len(q) == 1 {
				delete(r.sendQueue, sessionID)
			} else {
				r.sendQueue[sessionID] = q[1:]
			}
			ctx, cancel := context.WithCancel(context.Background())
			nextTok = &sendToken{cancel: cancel}
			nextCtx = ctx
			r.activeSend[sessionID] = nextTok
		}
	}
	r.sendMu.Unlock()
	tok.cancel()

	if next == nil {
		return
	}
	sess, ok := r.discovery.Get(sessionID)
	if !ok {
		// Session vanished while this send was queued.
		if next.abort != nil {
			next.abort(errors.New("session no longer exists"))
		}
		r.releaseSend(sessionID, nextTok) // drain the rest of the queue
		return
	}
	if next.pre != nil {
		next.pre()
	}
	r.startTurn(nextCtx, sess.ID, next.text, sess.Cwd, nextTok)
}

// flushSendQueue drops every queued send for sessionID, calling each abort.
// Used when the user cancels the in-flight turn or the session goes away —
// continuing to inject queued messages after an explicit stop would surprise.
func (r *Router) flushSendQueue(sessionID string, reason error) {
	r.sendMu.Lock()
	q := r.sendQueue[sessionID]
	delete(r.sendQueue, sessionID)
	r.sendMu.Unlock()
	for _, p := range q {
		if p.abort != nil {
			p.abort(reason)
		}
	}
}

// markSendIdle clears the running-state bit before publishing a terminal
// event. The creating overlay stays in place until releaseSend, so a just-born
// session remains addressable while the browser receives error/exit.
func (r *Router) markSendIdle(sessionID string, tok *sendToken) {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	cur, ok := r.activeSend[sessionID]
	if ok && cur != tok {
		return
	}
	if ok {
		delete(r.activeSend, sessionID)
	}
	// This turn's log lines are usher's own (already relayed by its
	// collector) — move the foreign watcher's baseline past them.
	r.bumpForeignBaseLocked(sessionID)
	if sess, ok := r.creating[sessionID]; ok {
		sess.Status = core.StatusIdle
		sess.LastEventAt = time.Now()
		r.creating[sessionID] = sess
	}
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
	// Cancel means stop: drop queued follow-ups too, before cancelling the
	// turn, so releaseSend finds nothing to promote.
	r.flushSendQueue(sessionID, errors.New("cancelled"))
	// Stop local collection before the best-effort interrupt RPC.
	tok.cancel()
	if err := r.senderFor(sessionID).Interrupt(sessionID); err != nil {
		slog.Warn("interrupt session turn", "session", sessionID, "err", err)
	}
	return nil
}

func (r *Router) SubscribeSession(id string) (<-chan broker.Event, func()) {
	return r.broker.Subscribe(id)
}

// SubscribeAllSessions returns a stream of events across every session, for
// frontends (the Telegram hub) that mirror all active sessions rather than one
// open one. Counterpart to SubscribeSession for the SSE-per-session web path.
func (r *Router) SubscribeAllSessions() (<-chan broker.Event, func()) {
	return r.broker.SubscribeAll()
}

// SubscribePendingInteractions returns a stream of newly-submitted permission
// requests, so the Telegram hub can push inline allow/deny buttons without
// polling ListPendingInteractions.
func (r *Router) SubscribePendingInteractions() (<-chan interaction.Pending, func()) {
	return r.interactions.SubscribePending()
}

// --- session terminal ----------------------------------------------------

func (r *Router) TerminalAvailable() bool {
	return r.terminal != nil && r.terminal.Available()
}

func (r *Router) HasTerminal(id string) bool {
	return r.terminal != nil && r.terminal.Has(id)
}

// OpenTerminal starts or reuses the session's shell window.
func (r *Router) OpenTerminal(id string, cols, rows int) error {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	if sess.IsSubagent {
		return errors.New("subagent transcripts are read-only")
	}
	if r.terminal == nil {
		return terminal.ErrUnavailable
	}
	return r.terminal.Open(id, sess.Cwd, cols, rows)
}

func (r *Router) CloseTerminal(id string) error {
	if _, ok := r.discovery.Get(id); !ok {
		return ErrSessionNotFound
	}
	if r.terminal == nil {
		return terminal.ErrUnavailable
	}
	return r.terminal.Close(id)
}

func (r *Router) CaptureTerminal(id string) (string, error) {
	if r.terminal == nil {
		return "", terminal.ErrUnavailable
	}
	return r.terminal.Capture(id)
}

func (r *Router) SubmitTerminal(id, requestID, text string) error {
	sess, ok := r.discovery.Get(id)
	if !ok {
		return ErrSessionNotFound
	}
	if sess.IsSubagent {
		return errors.New("subagent transcripts are read-only")
	}
	if r.terminal == nil {
		return terminal.ErrUnavailable
	}
	return r.terminal.Submit(id, requestID, text)
}

func (r *Router) SendTerminalControl(id string, keys ...string) error {
	if _, ok := r.discovery.Get(id); !ok {
		return ErrSessionNotFound
	}
	if r.terminal == nil {
		return terminal.ErrUnavailable
	}
	return r.terminal.SendControl(id, keys...)
}

func (r *Router) ResizeTerminal(id string, cols, rows int) error {
	if r.terminal == nil {
		return terminal.ErrUnavailable
	}
	return r.terminal.Resize(id, cols, rows)
}

// --- hook / interactions -------------------------------------------------

func (r *Router) ListPendingInteractions() []interaction.Pending { return r.interactions.List() }

func (r *Router) RespondInteraction(id string, resp interaction.Response) error {
	return r.interactions.Respond(id, resp)
}

// HandleHook applies blanket auto-approve first, then blocks for the web UI
// when usher owns the session. Ownership = the session has a live
// window in usher's process pool (sender.Has) — a simple membership test, NOT
// whether a turn is currently executing. The old activeSend (turn-in-flight)
// gate raced the send/inject/turn lifecycle and bounced mid-turn tool prompts
// back to the pane; pool membership is the stable signal. It also keeps usher
// from intercepting the user's own terminal/IDE claude (not in our pool), which
// on a shared default socket would otherwise reach this same hook server.
func (r *Router) HandleHook(ctx context.Context, ev interaction.Request) (interaction.Response, error) {
	if resp, ok := r.interactions.QuickDecide(ev); ok {
		return resp, nil
	}
	if !r.anyHas(ev.SessionID) {
		return interaction.Response{}, errors.New("session not owned by usher")
	}
	return r.interactions.Submit(ctx, ev)
}

func (r *Router) SetAutoApprove(sessionID string, enabled bool) {
	r.interactions.SetAutoApprove(sessionID, enabled)
}

func (r *Router) IsAutoApprove(sessionID string) bool {
	return r.interactions.IsAutoApprove(sessionID)
}

// --- transcript / blocking send (v0.2 LLM agent helpers) ----------------

// ReadSessionTranscript projects the most recent N user/assistant turns of
// a session's jsonl into core.TranscriptTurn. limit ≤ 0 returns everything.
func (r *Router) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, ErrSessionNotFound
	}
	turns, _, err := r.readTurnsForBackend(path, r.backendOf(id), limit)
	if err != nil {
		return nil, err
	}
	out := make([]core.TranscriptTurn, len(turns))
	for i, t := range turns {
		out[i] = core.TranscriptTurn{Role: t.Role, Content: flattenTurnText(t, true), Time: t.Time}
	}
	return out, nil
}

// ReadSessionTranscriptPage returns one page of the transcript: up to limit
// turns starting at absolute index offset (0-based from the start of the
// session). A negative offset means "the most recent page". It also returns
// the resolved start offset and the total turn count, so a caller can page in
// either direction and know when it has reached an end. Because the jsonl is
// append-only, an absolute index is a stable cursor even as the session grows.
//
// This is the primitive behind read_session_transcript's paging: the per-call
// limit bounds how much enters the agent's context, while offset + total keep
// any depth reachable — there is no hard wall, only a page boundary.
func (r *Router) ReadSessionTranscriptPage(id string, offset, limit int) ([]core.TranscriptTurn, int, int, error) {
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, 0, 0, ErrSessionNotFound
	}
	turns, _, err := r.readTurnsForBackend(path, r.backendOf(id), 0)
	if err != nil {
		return nil, 0, 0, err
	}
	total := len(turns)
	start, end := pageBounds(offset, limit, total)
	out := make([]core.TranscriptTurn, end-start)
	for i := start; i < end; i++ {
		t := turns[i]
		out[i-start] = core.TranscriptTurn{Role: t.Role, Content: flattenTurnText(t, true), Time: t.Time}
	}
	return out, start, total, nil
}

// pageBounds resolves [start, end) into a slice of length total for a page of
// up to limit items beginning at offset. A negative offset selects the last
// page (start = total-limit). Everything is clamped to [0, total], so an
// offset past the end yields an empty page rather than a panic.
func pageBounds(offset, limit, total int) (start, end int) {
	if limit <= 0 {
		limit = 1
	}
	start = offset
	if start < 0 {
		start = total - limit
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end = start + limit
	if end > total {
		end = total
	}
	return start, end
}

// flattenTurnText renders a core.Turn to plain text. User turns carry their
// text in Content; assistant turns carry it in Parts (text blocks interleaved
// with tool annotations). When includeTools is set, tool parts are inlined as
// `[tool: Name target]` markers — matching what read_session_transcript
// advertises. Search passes includeTools=false so it only matches the
// user/assistant prose, not tool names or command/file targets.
func flattenTurnText(t core.Turn, includeTools bool) string {
	if t.Role != "assistant" {
		return t.Content
	}
	var b strings.Builder
	for _, p := range t.Parts {
		switch p.Type {
		case "text":
			if p.Content == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Content)
		case "tool":
			if !includeTools {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("[tool: ")
			b.WriteString(p.ToolName)
			if p.ToolTarget != "" {
				b.WriteString(" ")
				b.WriteString(p.ToolTarget)
			}
			b.WriteString("]")
		}
	}
	return b.String()
}

// SearchSessionTranscript scans the whole transcript for a case-insensitive
// substring of query in the user/assistant text (tool annotations excluded),
// returning at most maxHits matching turns with a bounded snippet around each
// first occurrence. The bool reports whether more turns matched than were
// returned. limit-style truncation of read_session_transcript is exactly what
// this avoids: it looks at every turn but returns only small located snippets.
func (r *Router) SearchSessionTranscript(id, query string, maxHits, contextChars int) ([]core.TranscriptSearchHit, bool, error) {
	if strings.TrimSpace(query) == "" {
		return nil, false, errors.New("query is required")
	}
	path, ok := r.discovery.Path(id)
	if !ok {
		return nil, false, ErrSessionNotFound
	}
	turns, _, err := r.readTurnsForBackend(path, r.backendOf(id), 0)
	if err != nil {
		return nil, false, err
	}
	hits, matched := scanTurnsForQuery(turns, []rune(query), maxHits, contextChars)
	return hits, matched > len(hits), nil
}

// SearchAllSessions runs the same substring search across every discovered
// session and returns one compact result per session that has a match: its
// total hit count and a snippet at the first hit. It is the routing primitive
// for "which session mentioned X?" — the alternative, calling
// SearchSessionTranscript per session, costs a tool round-trip each. Sessions
// are ranked by hit count (most matches first); the bool reports whether more
// matched than maxSessions returned. Every session's jsonl is read once, so
// this is heavier than a single-session search — meant for user-driven lookup.
func (r *Router) SearchAllSessions(query string, maxSessions, contextChars int) ([]core.SessionSearchResult, bool, error) {
	if strings.TrimSpace(query) == "" {
		return nil, false, errors.New("query is required")
	}
	q := []rune(query)
	var results []core.SessionSearchResult
	for _, s := range r.ListSessions() {
		path, ok := r.discovery.Path(s.ID)
		if !ok {
			continue
		}
		turns, _, err := r.readTurnsForBackend(path, r.backendOf(s.ID), 0)
		if err != nil {
			continue
		}
		hits, matched := scanTurnsForQuery(turns, q, 1, contextChars)
		if matched == 0 {
			continue
		}
		res := core.SessionSearchResult{
			SessionID: s.ID,
			Title:     s.Title,
			Cwd:       s.Cwd,
			HitCount:  matched,
		}
		if len(hits) > 0 {
			res.TurnIndex = hits[0].TurnIndex
			res.Snippet = hits[0].Snippet
		}
		results = append(results, res)
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].HitCount > results[j].HitCount
	})
	truncated := false
	if maxSessions > 0 && len(results) > maxSessions {
		results = results[:maxSessions]
		truncated = true
	}
	return results, truncated, nil
}

// scanTurnsForQuery scans decoded turns for a case-insensitive substring of q
// in the user/assistant prose (tool annotations excluded), returning up to
// maxHits located snippets and the total count of matching turns. Shared by
// the single-session and cross-session searches.
func scanTurnsForQuery(turns []core.Turn, q []rune, maxHits, contextChars int) (hits []core.TranscriptSearchHit, matched int) {
	for i, t := range turns {
		text := []rune(flattenTurnText(t, false))
		first, count := foldFindAll(text, q)
		if first < 0 {
			continue
		}
		matched++
		if len(hits) >= maxHits {
			continue
		}
		hits = append(hits, core.TranscriptSearchHit{
			Role:        t.Role,
			Time:        t.Time,
			TurnIndex:   i,
			Occurrences: count,
			Snippet:     snippetAround(text, first, len(q), contextChars),
		})
	}
	return hits, matched
}

// foldFindAll returns the index of the first case-insensitive occurrence of
// needle in hay (rune slices) and the total non-overlapping occurrence count.
// Rune-based to stay correct with multibyte (e.g. CJK) content, where mixing
// byte offsets from strings.ToLower would risk splitting a character.
func foldFindAll(hay, needle []rune) (first, count int) {
	first = -1
	if len(needle) == 0 {
		return -1, 0
	}
	for i := 0; i+len(needle) <= len(hay); {
		if foldEqualAt(hay, needle, i) {
			if first < 0 {
				first = i
			}
			count++
			i += len(needle)
		} else {
			i++
		}
	}
	return first, count
}

func foldEqualAt(hay, needle []rune, at int) bool {
	for j := range needle {
		if unicode.ToLower(hay[at+j]) != unicode.ToLower(needle[j]) {
			return false
		}
	}
	return true
}

// snippetAround returns matchLen runes at start plus up to ctx runes of
// context on each side, with ellipses marking truncation and newlines
// collapsed to spaces so the result stays a single compact line.
func snippetAround(text []rune, start, matchLen, ctx int) string {
	if ctx < 0 {
		ctx = 0
	}
	lo := start - ctx
	if lo < 0 {
		lo = 0
	}
	hi := start + matchLen + ctx
	if hi > len(text) {
		hi = len(text)
	}
	snip := strings.ReplaceAll(string(text[lo:hi]), "\n", " ")
	if lo > 0 {
		snip = "…" + snip
	}
	if hi < len(text) {
		snip = snip + "…"
	}
	return snip
}

// relayWaitCeiling bounds how long a relayed send's collector goroutine may
// outlive its turn. It is not a UX timeout — relays are event-driven and fire
// whenever subprocess.exit lands — only a leak backstop for a session whose
// turn never terminates.
const relayWaitCeiling = 24 * time.Hour

// errNoResponse marks a collect that ended with no assistant text at all
// (ceiling/timeout expiry before any part arrived).
var errNoResponse = errors.New("timeout (no response received)")

// SendToSessionRelayed delivers text like SendToSession and collects the
// turn's assistant text in the background, handing it to onDone (at most
// once, own goroutine; reply may be partial alongside a non-nil error). If
// the ceiling passes with no response at all — killed window, exit never
// observed — onDone is NOT called: a day-late "(relay: timeout)" message is
// noise, and the reply, if any, is in the transcript.
func (r *Router) SendToSessionRelayed(id, text string, onDone func(sessionID, reply string, err error)) error {
	return r.enqueueSend(id, text,
		func() {
			ch, cancel := r.broker.Subscribe(id)
			go func() {
				defer cancel()
				waitCtx, waitCancel := context.WithTimeout(context.Background(), relayWaitCeiling)
				defer waitCancel()
				reply, err := collectTurnText(waitCtx, ch)
				if errors.Is(err, errNoResponse) {
					slog.Warn("relay collector expired with no response; dropping", "session", id)
					return
				}
				onDone(id, reply, err)
			}()
		},
		func(err error) { onDone(id, "", err) },
	)
}

// collectTurnText accumulates one turn's assistant text from a broker
// subscription until subprocess.exit, an error event, channel close, or ctx
// expiry (partial text is returned alongside the timeout error).
func collectTurnText(ctx context.Context, ch <-chan broker.Event) (string, error) {
	var buf strings.Builder
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return buf.String(), nil
			}
			switch ev.Type {
			case backendpkg.EventPart:
				// Message granularity (interactive claude emits no stream-json
				// token deltas): each text part carries one assistant message's
				// text blocks. Accumulate them across the turn.
				if t := partText(ev.Raw); t != "" {
					if buf.Len() > 0 {
						buf.WriteString("\n")
					}
					buf.WriteString(t)
				}
			case backendpkg.EventProcessExit:
				return buf.String(), nil
			case backendpkg.EventError:
				return buf.String(), errors.New(extractErrorMessage(ev.Raw))
			}
		case <-ctx.Done():
			if buf.Len() == 0 {
				return "", errNoResponse
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

// prepare validates the caller's inputs and pins the backend, so the three
// create paths below differ only in how they wait for the first reply.
func (r *Router) prepare(ctx context.Context, o core.CreateOptions) (core.CreateOptions, error) {
	cwd, err := validateCreateInputs(o.Cwd, o.InitialMessage)
	if err != nil {
		return o, err
	}
	o.Cwd = cwd
	o.Backend, err = r.resolveCreateBackend(ctx, o.Backend, o.Model)
	return o, err
}

// StartSession creates a new session and returns once its backend-assigned ID
// is known. The first turn continues in the background and streams to broker.
func (r *Router) StartSession(o core.CreateOptions) (string, error) {
	o, err := r.prepare(context.Background(), o)
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithCancel(context.Background())
	id, ch, tok, err := r.beginNewSession(ctx, cancel, o)
	if err != nil {
		cancel()
		return "", err
	}
	format, _ := r.transcriptForBackend(o.Backend) // beginNewSession validated it
	go func() {
		defer r.releaseSend(id, tok)
		asm := format.NewAssembler()
		for ev := range ch {
			r.publishStream(id, asm, ev, time.Time{})
			if ev.Type == backendpkg.EventProcessExit {
				r.markSendIdle(id, tok)
			}
		}
	}()
	return id, nil
}

func (r *Router) resolveCreateBackend(ctx context.Context, backend, model string) (string, error) {
	explicitBackend := backend != ""
	if !explicitBackend {
		backend = backendForModel(model)
	}
	_, ok := r.backends[backend]
	if !ok {
		if explicitBackend {
			return "", fmt.Errorf("backend %q is not enabled", backend)
		}
		backend = r.defaultBackend
	}
	if err := r.ValidateModel(ctx, backend, model); err != nil {
		return "", err
	}
	return backend, nil
}

func (r *Router) beginNewSession(ctx context.Context, cancel context.CancelFunc, o core.CreateOptions) (string, <-chan sender.StreamEvent, *sendToken, error) {
	if _, err := r.transcriptForBackend(o.Backend); err != nil {
		return "", nil, nil, err
	}
	id, ch, err := r.senderForBackend(o.Backend).Start(ctx, backendpkg.StartRequest{
		Cwd: o.Cwd, Prompt: o.InitialMessage, Model: o.Model, AppendSystemPrompt: o.AppendSystemPrompt,
		AutoApprove: o.AutoApprove,
	})
	if err != nil {
		return "", nil, nil, err
	}
	if r.meta != nil {
		r.meta.SetAppendSystemPrompt(id, o.AppendSystemPrompt)
	}
	tok := &sendToken{cancel: cancel}
	now := time.Now()
	r.sendMu.Lock()
	r.activeSend[id] = tok
	r.creating[id] = core.Session{
		ID: id, Title: textutil.Truncate(o.InitialMessage, 60), Cwd: o.Cwd,
		Status: core.StatusRunning, StartedAt: now, LastEventAt: now,
		LastInputAt: now, Backend: o.Backend,
	}
	r.sendMu.Unlock()
	return id, ch, tok, nil
}

// validateCreateInputs checks the new-session inputs and returns the resolved
// cwd: ~ is expanded, the path must otherwise be absolute, and it is created
// if missing.
func validateCreateInputs(cwd, initialMsg string) (string, error) {
	if cwd == "" {
		return "", errors.New("cwd is required")
	}
	if initialMsg == "" {
		return "", errors.New("initial_message is required")
	}
	if cwd == "~" || strings.HasPrefix(cwd, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		cwd = filepath.Join(home, cwd[1:])
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd %q must be an absolute path or start with ~", cwd)
	}
	// MkdirAll no-ops if cwd exists and errors on a non-dir, so it also is-dir-checks.
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return "", fmt.Errorf("cwd %q: %w", cwd, err)
	}
	// Resolve symlinks: backends record the RESOLVED cwd in their logs (macOS
	// /tmp → /private/tmp), and Codex id discovery matches rollouts to cwd by
	// string equality — an unresolved alias here never matches, so the create
	// times out while the session runs on unbound.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	return cwd, nil
}

// CreateSession spawns a brand-new session on the default backend and waits for
// the assistant's first response (subject to timeout). Returns the
// generated session id and the accumulated assistant text. The session
// will appear in discovery via fsnotify shortly after the subprocess
// starts writing its jsonl.
func (r *Router) CreateSession(ctx context.Context, o core.CreateOptions, timeout time.Duration) (string, string, error) {
	o, err := r.prepare(ctx, o)
	if err != nil {
		return "", "", err
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sessionID, ch, tok, err := r.beginNewSession(waitCtx, cancel, o)
	if err != nil {
		return "", "", err
	}

	reply := r.collectNewSessionText(sessionID, o.Backend, ch)
	expired := waitCtx.Err() != nil // before releaseSend, which cancels waitCtx
	r.releaseSend(sessionID, tok)

	if expired && reply == "" {
		return sessionID, "", fmt.Errorf("create_session timeout (no response received within %s)", timeout)
	}
	return sessionID, reply, nil
}

// CreateSessionRelayed starts a new session like CreateSession but returns as
// soon as the session id is known; the first assistant reply is collected in
// the background and handed to onDone (same contract as SendToSessionRelayed;
// onDone also receives the new session id so callers don't have to close over
// the not-yet-assigned return value). For Claude the id is pre-assigned so
// this returns almost immediately; for Codex it returns once the rollout file
// is discovered.
func (r *Router) CreateSessionRelayed(o core.CreateOptions, onDone func(sessionID, reply string, err error)) (string, error) {
	o, err := r.prepare(context.Background(), o)
	if err != nil {
		return "", err
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), relayWaitCeiling)

	sessionID, ch, tok, err := r.beginNewSession(waitCtx, cancel, o)
	if err != nil {
		cancel()
		return "", err
	}

	go func() {
		reply := r.collectNewSessionText(sessionID, o.Backend, ch)
		expired := waitCtx.Err() != nil // before releaseSend, which cancels waitCtx
		r.releaseSend(sessionID, tok)
		if expired && reply == "" {
			onDone(sessionID, "", errors.New("no response received (send expired)"))
			return
		}
		onDone(sessionID, reply, nil)
	}()
	return sessionID, nil
}

// collectNewSessionText drains a new session's first-turn stream, forwarding
// every event to broker subscribers (raw + derived part/turn.user events, so a
// session-detail tab opened on the new id sees the live stream too) while
// accumulating the assistant text parts.
func (r *Router) collectNewSessionText(sessionID, backendName string, ch <-chan sender.StreamEvent) string {
	var buf strings.Builder
	format, err := r.transcriptForBackend(backendName)
	if err != nil {
		for range ch {
		}
		return ""
	}
	asm := format.NewAssembler()
	for ev := range ch {
		for _, p := range r.publishStream(sessionID, asm, ev, time.Time{}) {
			if p.Type == "text" {
				if buf.Len() > 0 {
					buf.WriteString("\n")
				}
				buf.WriteString(p.Content)
			}
		}
	}
	return buf.String()
}
