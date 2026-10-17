// Package pluginapitest provides an in-memory RouterAPI and a socket-backed
// Server/Client pair. Both the pluginapi tests and the tests of packages that
// consume the control socket (internal/mcptools) drive the real HTTP path
// against it, so nobody has to hand-fake the client.
package pluginapitest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/api"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/pluginapi"
)

// FakeRouter implements pluginapi.RouterAPI backed by a broker and hand-fed
// state. Mutate it through the accessors — the server reads it concurrently.
type FakeRouter struct {
	broker *broker.Broker

	mu            sync.Mutex
	sessions      map[string]core.Session
	sent          map[string][]string
	pending       []interaction.Pending
	pendingCh     chan interaction.Pending
	responses     map[string]interaction.Response
	startErr      error
	started       []core.CreateOptions
	archived      map[string]bool
	autoAppr      map[string]bool
	relayGate     chan struct{}
	relayErr      error
	routerSession string
	queued        map[string][]QueuedRelay
}

func NewFakeRouter() *FakeRouter {
	return &FakeRouter{
		broker:    broker.New(),
		sessions:  map[string]core.Session{},
		sent:      map[string][]string{},
		pendingCh: make(chan interaction.Pending, 8),
		responses: map[string]interaction.Response{},
		archived:  map[string]bool{},
		autoAppr:  map[string]bool{},
		queued:    map[string][]QueuedRelay{},
	}
}

// AddSession makes a session visible to Get/List.
func (f *FakeRouter) AddSession(s core.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = s
}

// Sent returns the prompts routed to a session, in order.
func (f *FakeRouter) Sent(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[id]...)
}

// Started returns the recorded spawn requests, as the router resolved them.
func (f *FakeRouter) Started() []core.CreateOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.CreateOptions(nil), f.started...)
}

// SetStartErr makes subsequent spawns fail.
func (f *FakeRouter) SetStartErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startErr = err
}

// SetPending replaces the pending-interaction snapshot.
func (f *FakeRouter) SetPending(p ...interaction.Pending) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = p
}

// PushPending delivers one prompt on the live subscription channel.
func (f *FakeRouter) PushPending(p interaction.Pending) { f.pendingCh <- p }

// Response returns the recorded answer to a pending interaction.
func (f *FakeRouter) Response(id string) interaction.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.responses[id]
}

// Publish emits a session event to every subscriber.
func (f *FakeRouter) Publish(ev broker.Event) { f.broker.Publish(ev) }

func (f *FakeRouter) GetSession(id string) (core.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	return s, ok
}

func (f *FakeRouter) RouterSession() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.routerSession
}

func (f *FakeRouter) ListSessions() []core.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]core.Session, 0, len(f.sessions))
	for _, sess := range f.sessions {
		out = append(out, sess)
	}
	return out
}

func (f *FakeRouter) SubscribeAllSessions() (<-chan broker.Event, func()) {
	return f.broker.SubscribeAll()
}

func (f *FakeRouter) SendToSession(id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return errors.New("no such session")
	}
	f.sent[id] = append(f.sent[id], text)
	return nil
}

// HoldRelay parks every subsequent relay callback until ReleaseRelay, so a
// test can assert that a send returns before its turn finishes.
func (f *FakeRouter) HoldRelay() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relayGate = make(chan struct{})
}

// ReleaseRelay lets held callbacks run. Safe to call when nothing is held.
func (f *FakeRouter) ReleaseRelay() {
	f.mu.Lock()
	gate := f.relayGate
	f.relayGate = nil
	f.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

// SetRouterSession designates the session that receives relayed replies.
func (f *FakeRouter) SetRouterSession(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routerSession = id
}

// SetRelayErr makes subsequent relayed turns fail instead of replying.
func (f *FakeRouter) SetRelayErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.relayErr = err
}

// relay queues one note the way the real Router does: on its own goroutine,
// after the turn, exactly once. Returns whether an inbox was designated.
func (f *FakeRouter) relay(id, reply string) bool {
	f.mu.Lock()
	inbox, gate, relayErr := f.routerSession, f.relayGate, f.relayErr
	f.mu.Unlock()
	if inbox == "" {
		return false
	}
	go func() {
		if gate != nil {
			<-gate
		}
		note := QueuedRelay{SessionID: id, Reply: reply}
		if relayErr != nil {
			note = QueuedRelay{SessionID: id, Err: relayErr}
		}
		f.mu.Lock()
		f.queued[inbox] = append(f.queued[inbox], note)
		f.mu.Unlock()
	}()
	return true
}

// QueuedRelay is one note a relayed turn recorded, for assertions.
type QueuedRelay struct {
	SessionID string
	Reply     string
	Err       error
}

// WaitForRelay blocks until want notes are queued for routerID and returns
// them. The collector runs on its own goroutine, so a test cannot read them
// straight after the call that triggered one.
func (f *FakeRouter) WaitForRelay(t *testing.T, routerID string, want int) []QueuedRelay {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		got := append([]QueuedRelay(nil), f.queued[routerID]...)
		f.mu.Unlock()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("router %s got %d relays, want %d", routerID, len(got), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *FakeRouter) RouteToSession(id, text string) (bool, error) {
	if err := f.SendToSession(id, text); err != nil {
		return false, err
	}
	return f.relay(id, "reply: "+text), nil
}

func (f *FakeRouter) StartSession(o core.CreateOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	id := "s_new"
	f.started = append(f.started, o)
	f.sessions[id] = core.Session{ID: id, Cwd: o.Cwd}
	return id, nil
}

func (f *FakeRouter) RouteNewSession(o core.CreateOptions) (string, bool, error) {
	id, err := f.StartSession(o)
	if err != nil {
		return "", false, err
	}
	return id, f.relay(id, "created: "+o.InitialMessage), nil
}

// ReadTurns serves one fixed assistant turn made of a text part and a tool
// call, with an older page behind it.
func (f *FakeRouter) ReadTurns(id, before string, _ int) ([]core.Turn, bool, error) {
	if _, ok := f.GetSession(id); !ok {
		return nil, false, core.ErrSessionNotFound
	}
	if before != "" {
		return []core.Turn{{Role: "user", Content: "head", Cursor: "c0"}}, false, nil
	}
	return []core.Turn{{Role: "assistant", Cursor: "c1", Parts: []core.TurnPart{
		{Type: "text", Content: "tail"},
		{Type: "tool", ToolName: "Bash", ToolTarget: "ls"},
	}}}, true, nil
}

func (f *FakeRouter) ReadTurnsFrom(id, from string) ([]core.Turn, error) {
	if _, ok := f.GetSession(id); !ok {
		return nil, core.ErrSessionNotFound
	}
	return []core.Turn{
		{Role: "user", Content: "from " + from, Cursor: from},
		{Role: "assistant", Cursor: "c9", Parts: []core.TurnPart{{Type: "text", Content: "after"}}},
	}, nil
}

func (f *FakeRouter) SearchSessionTranscript(id, query string, _, _ int) ([]core.TranscriptSearchHit, bool, error) {
	if _, ok := f.GetSession(id); !ok {
		return nil, false, errors.New("no such session")
	}
	return []core.TranscriptSearchHit{{Snippet: query}}, false, nil
}

func (f *FakeRouter) SearchAllSessions(query string, _, _ int) ([]core.SessionSearchResult, bool, error) {
	return []core.SessionSearchResult{{SessionID: "s1", Snippet: query}}, false, nil
}

func (f *FakeRouter) Archive(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived[id] = true
}

func (f *FakeRouter) Unarchive(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived[id] = false
}

func (f *FakeRouter) IsArchived(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.archived[id]
}

func (f *FakeRouter) SetAutoApprove(id string, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.autoAppr[id] = enabled
}

func (f *FakeRouter) IsAutoApprove(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.autoAppr[id]
}

func (f *FakeRouter) ListPendingInteractions() []interaction.Pending {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]interaction.Pending(nil), f.pending...)
}

func (f *FakeRouter) SubscribePendingInteractions() (<-chan interaction.Pending, func()) {
	return f.pendingCh, func() {}
}

func (f *FakeRouter) RespondInteraction(id string, resp interaction.Response) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, done := f.responses[id]; done {
		return fmt.Errorf("%w: already resolved", interaction.ErrNotFound)
	}
	f.responses[id] = resp
	return nil
}

// Start runs a Server on a temp socket and returns a Client connected to it.
// Both are torn down when the test ends.
func Start(t *testing.T, f *FakeRouter) *pluginapi.Client {
	return StartWithAgents(t, f, nil)
}

// StartWithAgents is Start with a configured agent-profile store, for tests
// that create sessions through a named agent.
func StartWithAgents(t *testing.T, f *FakeRouter, agents *agentprofile.Store) *pluginapi.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "plugin.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := pluginapi.NewServer(f, api.Config{Agents: agents, AttachmentsDir: t.TempDir()}, nil).Run(ctx, sock); err != nil {
			t.Errorf("server: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })

	c := pluginapi.NewClient(sock, nil)
	WaitPing(t, c)
	return c
}

// WaitForSent blocks until the session has received want messages and returns
// them. Relay delivery is asynchronous by construction, so a test cannot read
// the inbox straight after the call that triggered it.
func (f *FakeRouter) WaitForSent(t *testing.T, id string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := f.Sent(id); len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s received %d messages, want %d", id, len(f.Sent(id)), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// WaitPing blocks until the socket answers, so a test never races startup.
func WaitPing(t *testing.T, c *pluginapi.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := c.Ping(ctx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TempSocketPath returns a socket path in a directory that outlives a server
// restart — TestReconnect rebinds the same path with a second Server.
func TempSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "plugin.sock")
}

// The rest of api.Router: operations these tests never drive.

func (f *FakeRouter) ListSessionsWithSubagents() []core.Session { return f.ListSessions() }

func (f *FakeRouter) OpenSession(id string, _ time.Duration) (core.Session, bool) {
	return f.GetSession(id)
}

func (f *FakeRouter) DefaultEffort(context.Context, string, string) string     { return "" }
func (f *FakeRouter) SetRuntime(context.Context, string, string, string) error { return nil }
func (f *FakeRouter) DeleteSession(string) error                               { return nil }
func (f *FakeRouter) ForkSession(id, _ string) (string, error)                 { return id + "_fork", nil }
func (f *FakeRouter) CancelSend(string) error                                  { return nil }
func (f *FakeRouter) Rename(_ context.Context, _, title string) error {
	if title == "" {
		return core.ErrInvalidTitle
	}
	return nil
}
func (f *FakeRouter) Pin(string)              {}
func (f *FakeRouter) Unpin(string)            {}
func (f *FakeRouter) IsPinned(string) bool    { return false }
func (f *FakeRouter) HasTerminal(string) bool { return false }
func (f *FakeRouter) TerminalAvailable() bool { return false }

func (f *FakeRouter) SubscribeSession(id string) (<-chan broker.Event, func()) {
	return f.broker.Subscribe(id)
}

func (f *FakeRouter) PauseSession(id string) error {
	if _, ok := f.GetSession(id); !ok {
		return core.ErrSessionNotFound
	}
	return nil
}

func (f *FakeRouter) ResumeSession(_ context.Context, id string) error { return f.PauseSession(id) }

func (f *FakeRouter) ComposerItems(context.Context, string, string) (backend.ComposerCatalog, error) {
	return backend.ComposerCatalog{}, nil
}

func (f *FakeRouter) Backends() []string { return []string{"claude"} }

func (f *FakeRouter) Models(context.Context, string) ([]backend.Model, error) { return nil, nil }
