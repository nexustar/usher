package pluginapi

import (
	"context"
	"encoding/json"
	"errors"

	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// fakeRouter implements RouterAPI backed by a broker and hand-fed pendings.
type fakeRouter struct {
	broker *broker.Broker

	mu        sync.Mutex
	sessions  map[string]core.Session
	sent      map[string][]string
	pending   []hook.Pending
	pendingCh chan hook.Pending
	responses map[string]hook.Response
}

func newFakeRouter() *fakeRouter {
	return &fakeRouter{
		broker:    broker.New(),
		sessions:  map[string]core.Session{},
		sent:      map[string][]string{},
		pendingCh: make(chan hook.Pending, 8),
		responses: map[string]hook.Response{},
	}
}

func (f *fakeRouter) GetSession(id string) (core.Session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	return s, ok
}

func (f *fakeRouter) SubscribeAllSessions() (<-chan broker.Event, func()) {
	return f.broker.SubscribeAll()
}

func (f *fakeRouter) SendToSession(id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return errors.New("no such session")
	}
	f.sent[id] = append(f.sent[id], text)
	return nil
}

func (f *fakeRouter) ListPendingInteractions() []hook.Pending {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]hook.Pending(nil), f.pending...)
}

func (f *fakeRouter) SubscribePendingInteractions() (<-chan hook.Pending, func()) {
	return f.pendingCh, func() {}
}

func (f *fakeRouter) RespondInteraction(id string, resp hook.Response) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, done := f.responses[id]; done {
		return errors.New("already resolved")
	}
	f.responses[id] = resp
	return nil
}

// startServer runs a Server on a temp socket and returns a connected Client.
func startServer(t *testing.T, f *fakeRouter) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "pa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "plugin.sock")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := NewServer(f, nil).Run(ctx, sock); err != nil {
			t.Errorf("server: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })

	c := NewClient(sock, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
		err := c.Ping(pingCtx)
		pingCancel()
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSessionRoundTrip(t *testing.T) {
	f := newFakeRouter()
	f.sessions["s1"] = core.Session{ID: "s1", Title: "hello", Cwd: "/w"}
	c := startServer(t, f)

	sess, ok := c.GetSession("s1")
	if !ok || sess.Title != "hello" || sess.Cwd != "/w" {
		t.Fatalf("GetSession = %+v, %v", sess, ok)
	}
	if _, ok := c.GetSession("nope"); ok {
		t.Fatal("missing session should report !ok")
	}

	if err := c.SendToSession("s1", "do it"); err != nil {
		t.Fatalf("SendToSession: %v", err)
	}
	if err := c.SendToSession("nope", "x"); err == nil || err.Error() != "no such session" {
		t.Fatalf("send to missing session: err = %v, want server message", err)
	}
	f.mu.Lock()
	got := f.sent["s1"]
	f.mu.Unlock()
	if len(got) != 1 || got[0] != "do it" {
		t.Fatalf("routed sends = %v", got)
	}
}

func TestEventStream(t *testing.T) {
	f := newFakeRouter()
	c := startServer(t, f)

	events, cancel := c.SubscribeAllSessions()
	defer cancel()

	// The SSE connect races the publish; retry until the subscriber is seen.
	raw := json.RawMessage(`{"message":{"content":[{"type":"text","text":"hi"}]}}`)
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev := <-events:
			if ev.SessionID != "s1" || ev.Type != "assistant" || string(ev.Raw) != string(raw) {
				t.Fatalf("event = %+v", ev)
			}
			return
		case <-tick.C:
			f.broker.Publish(broker.Event{SessionID: "s1", Type: "assistant", Raw: raw})
		case <-deadline:
			t.Fatal("no event received")
		}
	}
}

func TestInteractionsSnapshotAndLive(t *testing.T) {
	f := newFakeRouter()
	f.pending = []hook.Pending{{ID: "p1", SessionID: "s1", ToolName: "Bash"}}
	c := startServer(t, f)

	pending, cancel := c.SubscribePendingInteractions()
	defer cancel()

	first := recvPending(t, pending)
	if first.ID != "p1" {
		t.Fatalf("snapshot pending = %+v", first)
	}

	f.pendingCh <- hook.Pending{ID: "p2", SessionID: "s1", ToolName: "Edit"}
	second := recvPending(t, pending)
	if second.ID != "p2" {
		t.Fatalf("live pending = %+v", second)
	}

	if err := c.RespondInteraction("p1", hook.Response{Behavior: "allow"}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	if err := c.RespondInteraction("p1", hook.Response{Behavior: "allow"}); err == nil {
		t.Fatal("second respond should surface the server error")
	}
	f.mu.Lock()
	resp := f.responses["p1"]
	f.mu.Unlock()
	if resp.Behavior != "allow" {
		t.Fatalf("recorded response = %+v", resp)
	}
}

func recvPending(t *testing.T, ch <-chan hook.Pending) hook.Pending {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pending")
		return hook.Pending{}
	}
}

// TestReconnect kills the server mid-subscription and verifies the client
// heals onto a fresh server on the same socket path.
func TestReconnect(t *testing.T) {
	f := newFakeRouter()
	dir, err := os.MkdirTemp("", "pa")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "plugin.sock")

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { defer close(done1); _ = NewServer(f, nil).Run(ctx1, sock) }()

	c := NewClient(sock, nil)
	waitPing(t, c)
	events, cancel := c.SubscribeAllSessions()
	defer cancel()

	// Prove the first connection works.
	publishUntilReceived(t, f, events, "before")

	cancel1()
	<-done1

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); _ = NewServer(f, nil).Run(ctx2, sock) }()
	t.Cleanup(func() { cancel2(); <-done2 })
	waitPing(t, c)

	// The same subscription channel keeps delivering after the restart.
	publishUntilReceived(t, f, events, "after")
}

func waitPing(t *testing.T, c *Client) {
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

// publishUntilReceived repeatedly publishes a marker event until it arrives on
// events (the subscribe/reconnect races the publish), draining anything else.
func publishUntilReceived(t *testing.T, f *fakeRouter, events <-chan broker.Event, marker string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev := <-events:
			if ev.Type == marker {
				return
			}
		case <-tick.C:
			f.broker.Publish(broker.Event{SessionID: "s", Type: marker})
		case <-deadline:
			t.Fatalf("marker %q never received", marker)
		}
	}
}

// TestEventTypeFilter: a client with EventTypes set must not receive other
// event types (the server filters before marshaling).
func TestEventTypeFilter(t *testing.T) {
	f := newFakeRouter()
	c := startServer(t, f)
	c.EventTypes = []string{"assistant"}

	events, cancel := c.SubscribeAllSessions()
	defer cancel()

	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev := <-events:
			if ev.Type != "assistant" {
				t.Fatalf("filtered stream delivered %q", ev.Type)
			}
			return
		case <-tick.C:
			// The noise event is published first each round; receiving the
			// assistant event implies the earlier noise was filtered out.
			f.broker.Publish(broker.Event{SessionID: "s", Type: "user"})
			f.broker.Publish(broker.Event{SessionID: "s", Type: "assistant"})
		case <-deadline:
			t.Fatal("no event received")
		}
	}
}
