// Package interaction brokers every backend request that blocks on a user
// answer — Claude's permission callback and AskUserQuestion hook, Codex
// approvals, pi's extension dialogs — through usher's web UI. One-way
// notifications stay out: nothing waits on them.
package interaction

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/nexustar/usher/pkg/core"
)

// Interaction kinds. The frontend picks its control from these, never from a
// backend or tool name.
const (
	KindPermission = "permission" // allow/deny, answered by Response.Behavior
	KindChoice     = "choice"     // pick an option, answered by Response.Answers
	KindText       = "text"       // free text, answered by Response.Answers
	KindPlan       = "plan"       // approve a plan, answered by Response.Behavior
)

// Pending describes an interaction waiting for a user decision.
type Pending struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Kind      string          `json:"kind"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Cwd       string          `json:"cwd,omitempty"`
	// Preview shows a permission's tool call as a transcript tool part would;
	// clients fall back to ToolInput without it.
	Preview *core.TurnPart `json:"preview,omitempty"`
	// Reason is the backend's own words for why it asks.
	Reason string `json:"reason,omitempty"`
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
	// Scope is "once" (default) or "always"; the backend owns the exact scope
	// of "always".
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
	Preview     *core.TurnPart
	Reason      string
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

// Manager owns pending interactions and each session's approval.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*pendingEntry

	approvalMu sync.Mutex
	approvals  ApprovalStore

	subMu       sync.Mutex
	pendingSubs map[*pendingSub]struct{} // notified when a new interaction is submitted

	dedupMu sync.Mutex
	dedup   map[string]*dedupDecision
}

// ApprovalStore keeps each session's approval; "" is unset.
type ApprovalStore interface {
	Approval(id string) string
	SetApproval(id, approval string)
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

// New constructs a Manager that keeps approvals in memory until SetApprovalStore.
func New() *Manager {
	return &Manager{
		pending:     map[string]*pendingEntry{},
		approvals:   memApprovals{},
		pendingSubs: map[*pendingSub]struct{}{},
		dedup:       map[string]*dedupDecision{},
	}
}

type memApprovals map[string]string

func (m memApprovals) Approval(id string) string { return m[id] }

func (m memApprovals) SetApproval(id, approval string) {
	if approval == "" {
		delete(m, id)
	} else {
		m[id] = approval
	}
}

func (m *Manager) SetApprovalStore(s ApprovalStore) {
	m.approvalMu.Lock()
	m.approvals = s
	m.approvalMu.Unlock()
}

// Approval is id's approval; one never set asks.
func (m *Manager) Approval(id string) string {
	m.approvalMu.Lock()
	defer m.approvalMu.Unlock()
	if approval := m.approvals.Approval(id); approval != "" {
		return approval
	}
	return core.ApprovalAsk
}

// SetApproval records id's approval; "" returns it to ask. Entering
// full allows what the session is already waiting on.
func (m *Manager) SetApproval(id, approval string) {
	m.approvalMu.Lock()
	m.approvals.SetApproval(id, approval)
	m.approvalMu.Unlock()
	if m.Approval(id) != core.ApprovalFull {
		return
	}
	m.mu.Lock()
	var waiting []*pendingEntry
	for _, p := range m.pending {
		if p.SessionID == id && p.Kind == KindPermission {
			waiting = append(waiting, p)
		}
	}
	m.mu.Unlock()
	for _, p := range waiting {
		select {
		case p.response <- Response{Behavior: "allow", Reason: "approval: full"}:
		default:
		}
	}
}

// QuickDecide settles ev without UI in full; returns (zero, false) when
// input is needed.
func (m *Manager) QuickDecide(ev Request) (Response, bool) {
	// A bare allow carries no answer, so it can only settle a permission.
	if kindOf(ev) != KindPermission {
		return Response{}, false
	}
	if m.Approval(ev.SessionID) == core.ApprovalFull {
		slog.Info("interaction auto-approved", "session", ev.SessionID, "tool", ev.ToolName)
		return Response{
			Behavior: "allow",
			Reason:   "approval: full",
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
			Preview:     ev.Preview,
			Reason:      ev.Reason,
			AllowAlways: ev.AllowAlways,
			CreatedAt:   time.Now().UTC(),
		},
		response: make(chan Response, 1),
	}
	m.mu.Lock()
	m.pending[p.ID] = p
	m.mu.Unlock()
	m.notifyPending(p.Pending)
	slog.Info("interaction requested", "session", ev.SessionID, "kind", p.Kind, "tool", ev.ToolName)

	defer func() {
		m.mu.Lock()
		delete(m.pending, p.ID)
		m.mu.Unlock()
	}()

	select {
	case r := <-p.response:
		slog.Info("interaction resolved",
			"session", ev.SessionID, "kind", p.Kind, "tool", ev.ToolName,
			"behavior", r.Behavior, "scope", r.Scope,
			"waited", time.Since(p.CreatedAt).Round(time.Millisecond))
		return r, nil
	case <-ctx.Done():
		slog.Info("interaction abandoned", "session", ev.SessionID, "kind", p.Kind, "tool", ev.ToolName, "err", ctx.Err())
		return Response{}, ctx.Err()
	}
}

// List returns a snapshot of all currently-pending interactions, oldest first.
func (m *Manager) List() []Pending {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Pending, 0, len(m.pending))
	for _, p := range m.pending {
		out = append(out, p.Pending)
	}
	slices.SortFunc(out, func(a, b Pending) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
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

// ErrNotFound is a decision for an interaction that is not pending.
var ErrNotFound = errors.New("interaction not found")

// Respond delivers the user's decision to the matching pending interaction.
func (m *Manager) Respond(id string, r Response) error {
	m.mu.Lock()
	p, ok := m.pending[id]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	if r.Scope == "always" {
		if r.Behavior != "allow" {
			return errors.New("scope always is only valid for allow")
		}
		if !p.AllowAlways {
			return errors.New("always-allow is not available for this interaction")
		}
	}
	select {
	case p.response <- r:
		return nil
	default:
		return fmt.Errorf("%w: already resolved", ErrNotFound)
	}
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
