// Package interaction brokers every backend request that blocks on a user
// answer — Claude's permission callback and AskUserQuestion hook, Codex
// approvals, pi's extension dialogs — through usher's web UI. One-way
// notifications stay out: nothing waits on them.
package interaction

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Interaction kinds. The frontend picks its control from these, never from a
// backend or tool name.
const (
	KindPermission = "permission" // allow/deny, answered by Response.Behavior
	KindChoice     = "choice"     // pick an option, answered by Response.Answers
	KindText       = "text"       // free text, answered by Response.Answers
)

// Pending describes an interaction waiting for a user decision.
type Pending struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Kind      string          `json:"kind"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	// AllowAlways is true only when the originating backend advertised a
	// native rule or repeated-approval decision for this request.
	AllowAlways bool      `json:"allow_always"`
	CreatedAt   time.Time `json:"created_at"`
}

type pendingEntry struct {
	Pending
	response chan Response
}

// Response is the user's decision on a pending interaction.
type Response struct {
	Behavior string `json:"behavior"` // allow | deny
	Reason   string `json:"reason,omitempty"`
	// Scope is "once" (default) or "session". "session" is the historical API
	// spelling for the UI's "always" choice; the backend owns its exact scope.
	Scope string `json:"scope,omitempty"`
	// Answers maps each question, verbatim from the tool input, to what the
	// user picked or typed; Behavior is "allow" alongside it. Claude's
	// AskUserQuestion merges it into the tool's updatedInput so the tool never
	// falls back to its pane TUI selector.
	Answers map[string]string `json:"answers,omitempty"`
}

// Answer returns the single answer of a one-question interaction.
func (r Response) Answer() string {
	for _, v := range r.Answers {
		return v
	}
	return ""
}

// Request is one backend asking the user something.
type Request struct {
	SessionID string
	ToolUseID string
	// Kind is one of the Kind* constants; empty means KindPermission, except
	// for Claude's AskUserQuestion, which is a choice by tool name alone.
	Kind        string
	ToolName    string
	ToolInput   json.RawMessage
	Cwd         string
	AllowAlways bool
}

// AskUserQuestionTool arrives as an ordinary hook event, so nothing upstream
// can tag its kind; the name is the only signal.
const AskUserQuestionTool = "AskUserQuestion"

func kindOf(ev Request) string {
	if ev.Kind != "" {
		return ev.Kind
	}
	if ev.ToolName == AskUserQuestionTool {
		return KindChoice
	}
	return KindPermission
}

// Manager owns pending interactions and the per-session blanket auto-approve
// flag. Auto-approve is persisted to disk so the trust boundary survives
// restarts.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*pendingEntry

	autoMu      sync.Mutex
	autoApprove map[string]bool // sessionID → true when blanket-allow is on
	autoPath    string          // empty = no disk persistence (tests)

	subMu       sync.Mutex
	pendingSubs map[*pendingSub]struct{} // notified when a new interaction is submitted

	dedupMu sync.Mutex
	dedup   map[string]*dedupDecision
}

type dedupDecision struct {
	done     chan struct{}
	response Response
	err      error
}

// pendingSub subscribes to new-pending notifications. Buffered and drop-on-full,
// like the broker: a stalled consumer never blocks Submit.
type pendingSub struct {
	ch chan Pending
}

// New constructs a Manager. autoPath is the file backing the auto-approve
// flag map; pass "" to disable persistence (e.g. in tests). If the file
// exists at construction time, its state is loaded; subsequent calls to
// SetAutoApprove rewrite it atomically.
func New(autoPath string) *Manager {
	m := &Manager{
		pending:     map[string]*pendingEntry{},
		autoApprove: map[string]bool{},
		autoPath:    autoPath,
		pendingSubs: map[*pendingSub]struct{}{},
		dedup:       map[string]*dedupDecision{},
	}
	if autoPath != "" {
		m.loadAutoApprove()
	}
	return m
}

// loadAutoApprove reads autoPath into m.autoApprove. Best-effort: a missing
// file is normal on first run; a corrupt file is logged and treated as empty.
func (m *Manager) loadAutoApprove() {
	data, err := os.ReadFile(m.autoPath)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("auto-approve: read", "path", m.autoPath, "err", err)
		}
		return
	}
	var loaded map[string]bool
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("auto-approve: decode", "path", m.autoPath, "err", err)
		return
	}
	m.autoMu.Lock()
	defer m.autoMu.Unlock()
	for k, v := range loaded {
		if v {
			m.autoApprove[k] = true
		}
	}
}

// persistAutoApprove writes the current map to autoPath via temp-file +
// rename so a partial write can't corrupt the on-disk state. Caller must
// hold m.autoMu. Best-effort: failures are logged but don't surface.
func (m *Manager) persistAutoApprove() {
	if m.autoPath == "" {
		return
	}
	data, err := json.Marshal(m.autoApprove)
	if err != nil {
		slog.Warn("auto-approve: encode", "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.autoPath), 0o700); err != nil {
		slog.Warn("auto-approve: mkdir", "err", err)
		return
	}
	tmp := m.autoPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("auto-approve: write tmp", "err", err)
		return
	}
	if err := os.Rename(tmp, m.autoPath); err != nil {
		slog.Warn("auto-approve: rename", "err", err)
	}
}

// QuickDecide settles ev from blanket auto-approve without UI; returns
// (zero, false) when input is needed.
func (m *Manager) QuickDecide(ev Request) (Response, bool) {
	// A bare allow carries no answer, so it can only settle a permission.
	if kindOf(ev) != KindPermission {
		return Response{}, false
	}
	if m.IsAutoApprove(ev.SessionID) {
		return Response{
			Behavior: "allow",
			Reason:   "auto-approve",
		}, true
	}
	return Response{}, false
}

// Submit blocks until the user responds via Respond or ctx is cancelled.
// Short-circuits via QuickDecide first.
func (m *Manager) Submit(ctx context.Context, ev Request) (Response, error) {
	if ev.ToolUseID == "" {
		return m.submit(ctx, ev)
	}
	key := ev.SessionID + "\x00" + ev.ToolUseID
	m.dedupMu.Lock()
	d := m.dedup[key]
	if d == nil {
		d = &dedupDecision{done: make(chan struct{})}
		m.dedup[key] = d
		m.dedupMu.Unlock()
		d.response, d.err = m.submit(ctx, ev)
		close(d.done)
		time.AfterFunc(time.Minute, func() {
			m.dedupMu.Lock()
			if m.dedup[key] == d {
				delete(m.dedup, key)
			}
			m.dedupMu.Unlock()
		})
		return d.response, d.err
	}
	m.dedupMu.Unlock()
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-d.done:
		return d.response, d.err
	}
}

func (m *Manager) submit(ctx context.Context, ev Request) (Response, error) {
	if resp, ok := m.QuickDecide(ev); ok {
		return resp, nil
	}

	p := &pendingEntry{
		Pending: Pending{
			ID:          newID(),
			SessionID:   ev.SessionID,
			Kind:        kindOf(ev),
			ToolName:    ev.ToolName,
			ToolInput:   ev.ToolInput,
			Cwd:         ev.Cwd,
			AllowAlways: ev.AllowAlways,
			CreatedAt:   time.Now().UTC(),
		},
		response: make(chan Response, 1),
	}
	m.mu.Lock()
	m.pending[p.ID] = p
	m.mu.Unlock()
	m.notifyPending(p.Pending)

	defer func() {
		m.mu.Lock()
		delete(m.pending, p.ID)
		m.mu.Unlock()
	}()

	select {
	case r := <-p.response:
		return r, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// List returns a snapshot of all currently-pending interactions.
func (m *Manager) List() []Pending {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Pending, 0, len(m.pending))
	for _, p := range m.pending {
		out = append(out, p.Pending)
	}
	return out
}

// SubscribePending returns a buffered channel that receives each new pending
// interaction as it is submitted, plus a cancel function. The web UI polls
// List() instead; this push path lets the web-push dispatcher surface permission
// prompts as notifications without polling. Drop-on-full, so a slow consumer
// never blocks Submit.
func (m *Manager) SubscribePending() (<-chan Pending, func()) {
	sub := &pendingSub{ch: make(chan Pending, 64)}
	m.subMu.Lock()
	m.pendingSubs[sub] = struct{}{}
	m.subMu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			m.subMu.Lock()
			delete(m.pendingSubs, sub)
			m.subMu.Unlock()
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

func (m *Manager) notifyPending(p Pending) {
	m.subMu.Lock()
	chans := make([]chan Pending, 0, len(m.pendingSubs))
	for s := range m.pendingSubs {
		chans = append(chans, s.ch)
	}
	m.subMu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- p:
		default:
		}
	}
}

// Respond delivers the user's decision to the matching pending interaction.
// Returns an error if the ID is unknown or the entry has already been resolved.
func (m *Manager) Respond(id string, r Response) error {
	m.mu.Lock()
	p, ok := m.pending[id]
	m.mu.Unlock()
	if !ok {
		return errors.New("interaction not found")
	}
	if r.Scope == "session" {
		if r.Behavior != "allow" {
			return errors.New("session scope is only valid for allow")
		}
		if !p.AllowAlways {
			return errors.New("session approval is not available for this interaction")
		}
	}
	select {
	case p.response <- r:
		return nil
	default:
		return errors.New("already resolved")
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetAutoApprove flips the blanket "allow every tool call" flag for a
// session and persists the change to disk so it survives restarts.
func (m *Manager) SetAutoApprove(sessionID string, enabled bool) {
	m.autoMu.Lock()
	defer m.autoMu.Unlock()
	if enabled {
		m.autoApprove[sessionID] = true
	} else {
		delete(m.autoApprove, sessionID)
	}
	m.persistAutoApprove()
}

// IsAutoApprove reports whether sessionID is currently in blanket-allow mode.
func (m *Manager) IsAutoApprove(sessionID string) bool {
	m.autoMu.Lock()
	defer m.autoMu.Unlock()
	return m.autoApprove[sessionID]
}
