package pluginapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/agentprofile"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/pluginapi"
	"github.com/nexustar/usher/internal/pluginapi/pluginapitest"
)

func TestSessionRoundTrip(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Title: "hello", Cwd: "/w"})
	c := pluginapitest.Start(t, f)

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
	if got := f.Sent("s1"); len(got) != 1 || got[0] != "do it" {
		t.Fatalf("routed sends = %v", got)
	}

	path, err := c.UploadAttachment("s1", "note.txt", strings.NewReader("attachment"))
	if err != nil {
		t.Fatalf("UploadAttachment: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "attachment" {
		t.Fatalf("uploaded attachment = %q, %v", data, err)
	}
	if _, err := c.UploadAttachment("nope", "note.txt", strings.NewReader("x")); err == nil {
		t.Fatal("upload to missing session succeeded")
	}
}

func TestCreateSessionRoundTrip(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	c := pluginapitest.Start(t, f)

	got, err := c.CreateSession(pluginapi.CreateRequest{
		Backend: "codex", Cwd: "/tmp/work", InitialMessage: "hello", Model: "codex-test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if got.SessionID != "s_new" {
		t.Fatalf("id = %q", got.SessionID)
	}
	started := f.Started()
	f.SetStartErr(errors.New("bad cwd"))
	want := core.CreateOptions{
		Backend: "codex", Cwd: "/tmp/work", InitialMessage: "hello", Model: "codex-test",
	}
	if len(started) != 1 || !reflect.DeepEqual(started[0], want) {
		t.Fatalf("started = %+v, want %+v", started, want)
	}

	_, err = c.CreateSession(pluginapi.CreateRequest{Cwd: "/nope", InitialMessage: "hello"})
	if err == nil || err.Error() != "bad cwd" {
		t.Fatalf("CreateSession error = %v", err)
	}
}

func TestCreateSessionWithAgent(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	c := pluginapitest.StartWithAgents(t, f, agentprofile.New([]agentprofile.Profile{{
		Name: "dev", Cwd: "/work/dev",
		Backend: "codex", Model: "codex-default", AppendSystemPrompt: "Be careful.",
	}}))

	got, err := c.CreateSession(pluginapi.CreateRequest{
		Agent: "dev", InitialMessage: "hello", Model: "codex-override",
	})
	if err != nil || got.SessionID != "s_new" {
		t.Fatalf("CreateSession = %+v, %v", got, err)
	}
	started := f.Started()
	want := core.CreateOptions{
		Backend: "codex", Cwd: "/work/dev", InitialMessage: "hello",
		Model: "codex-override", AppendSystemPrompt: "Be careful.",
	}
	if len(started) != 1 || !reflect.DeepEqual(started[0], want) {
		t.Fatalf("started = %+v, want %+v", started, want)
	}
}

// "default" must beat a configured profile model on its way to the backend's
// own default, and an unknown agent must fail rather than silently spawn.
func TestCreateSessionAgentModelAndUnknown(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	c := pluginapitest.StartWithAgents(t, f, agentprofile.New([]agentprofile.Profile{{
		Name: "dev", Cwd: "/work/dev", Model: "profile-model",
	}}))

	if _, err := c.CreateSession(pluginapi.CreateRequest{
		Agent: "dev", InitialMessage: "hello", Model: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if started := f.Started(); len(started) != 1 || started[0].Model != "" {
		t.Fatalf("started = %+v, want the backend default model", started)
	}

	_, err := c.CreateSession(pluginapi.CreateRequest{Agent: "nope", InitialMessage: "hello"})
	if err == nil || !strings.Contains(err.Error(), "unknown agent") {
		t.Fatalf("unknown agent error = %v", err)
	}
}

func TestSessionControlRoundTrip(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Title: "work", Cwd: "/w"})
	f.SetPending(interaction.Pending{ID: "p1", SessionID: "s1"})
	c := pluginapitest.Start(t, f)

	sessions, err := c.ListSessions()
	if err != nil || len(sessions) != 1 || sessions[0].ID != "s1" {
		t.Fatalf("ListSessions = %+v, %v", sessions, err)
	}
	page, err := c.ReadTurns("s1", "", "", 20)
	if err != nil || len(page.Turns) != 1 || !page.More || page.Turns[0].Cursor != "c1" {
		t.Fatalf("ReadTurns = %+v, %v", page, err)
	}
	if older, err := c.ReadTurns("s1", "c1", "", 20); err != nil || older.More || older.Turns[0].Content != "head" {
		t.Fatalf("ReadTurns before c1 = %+v, %v", older, err)
	}
	found, err := c.SearchSessions("s1", "needle", 20, 120)
	if err != nil || found.Scope != "session" || len(found.Hits) != 1 || found.Hits[0].Snippet != "needle" {
		t.Fatalf("SearchSessions = %+v, %v", found, err)
	}
	global, err := c.SearchSessions("", "needle", 20, 120)
	if err != nil || global.Scope != "all" || len(global.Results) != 1 {
		t.Fatalf("SearchSessions (global) = %+v, %v", global, err)
	}
	created, err := c.CreateSession(pluginapi.CreateRequest{
		Backend: "codex", Cwd: "/tmp/new", InitialMessage: "build", Model: "m",
	})
	if err != nil || created.SessionID != "s_new" {
		t.Fatalf("CreateSession = %+v, %v", created, err)
	}

	if err := c.SetArchived("s1", true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	if err := c.SetAutoApprove("s1", true); err != nil {
		t.Fatalf("SetAutoApprove: %v", err)
	}
	sessions, err = c.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	for _, sess := range sessions {
		if sess.ID == "s1" && (!sess.Archived || !sess.AutoApprove) {
			t.Fatalf("flags did not round-trip: %+v", sess)
		}
	}

	pending, err := c.ListPendingInteractions()
	if err != nil || len(pending) != 1 || pending[0].ID != "p1" {
		t.Fatalf("ListPendingInteractions = %+v, %v", pending, err)
	}
	if err := c.RespondInteraction("p1", interaction.Response{Behavior: "deny"}); err != nil {
		t.Fatalf("RespondInteraction: %v", err)
	}
	if f.Response("p1").Behavior != "deny" {
		t.Fatalf("recorded response = %+v", f.Response("p1"))
	}
}

// TestSendReturnsBeforeTheTurnFinishes is the whole point of the asynchronous
// shape: the call must not be coupled to how long the session takes, and the
// finished turn must reach the router as a queued note rather than a prompt.
func TestSendReturnsBeforeTheTurnFinishes(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	f.AddSession(core.Session{ID: "router"})
	f.SetRouterSession("router")
	f.HoldRelay()
	c := pluginapitest.Start(t, f)

	got0, err := c.SendToSessionRelayed("s1", "go")
	if err != nil || !got0.Relayed {
		t.Fatalf("SendToSessionRelayed = %+v, %v", got0, err)
	}
	if got := f.Sent("router"); len(got) != 0 {
		t.Fatalf("inbox got %v before the turn completed", got)
	}

	// Now let the turn land: the note is recorded for the router, and nothing
	// is sent to it — a prompt would force a turn whose only product is the
	// model restating a reply the user can already read.
	f.ReleaseRelay()
	got := f.WaitForRelay(t, "router", 1)
	if got[0].SessionID != "s1" || got[0].Reply != "reply: go" {
		t.Fatalf("relayed note = %+v", got[0])
	}
	if sent := f.Sent("router"); len(sent) != 0 {
		t.Fatalf("relay prompted the router: %v", sent)
	}
}

// TestRelayReportsFailure: a turn that errors must still produce something in
// the inbox, or the router agent waits forever for a reply that never comes.
func TestRelayReportsFailure(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	f.AddSession(core.Session{ID: "router"})
	f.SetRouterSession("router")
	f.SetRelayErr(errors.New("backend died"))
	c := pluginapitest.Start(t, f)

	if _, err := c.SendToSessionRelayed("s1", "go"); err != nil {
		t.Fatalf("SendToSessionRelayed: %v", err)
	}
	got := f.WaitForRelay(t, "router", 1)
	if got[0].SessionID != "s1" || got[0].Err == nil || got[0].Err.Error() != "backend died" {
		t.Fatalf("relayed failure = %+v", got[0])
	}
}

// TestSendWithoutInboxStaysFireAndForget keeps the IM sidecars' contract: no
// A plain send relays nothing anywhere.
func TestSendWithoutInboxStaysFireAndForget(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	f.AddSession(core.Session{ID: "router"})
	c := pluginapitest.Start(t, f)

	if err := c.SendToSession("s1", "go"); err != nil {
		t.Fatalf("SendToSession: %v", err)
	}
	if got := f.Sent("s1"); len(got) != 1 || got[0] != "go" {
		t.Fatalf("target got %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if got := f.Sent("router"); len(got) != 0 {
		t.Fatalf("fire-and-forget send relayed to %v", got)
	}
}

// TestCreateRelaysFirstReply: the new session's opening turn comes back the
// same way a send's does.
func TestCreateRelaysFirstReply(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "router"})
	c := pluginapitest.Start(t, f)

	f.SetRouterSession("router")
	got, err := c.RouteNewSession(pluginapi.CreateRequest{Cwd: "/tmp/new", InitialMessage: "build"})
	if err != nil || got.SessionID != "s_new" || !got.Relayed {
		t.Fatalf("RouteNewSession = %+v, %v", got, err)
	}
	notes := f.WaitForRelay(t, "router", 1)
	if notes[0].SessionID != "s_new" || notes[0].Reply != "created: build" {
		t.Fatalf("relayed note = %+v", notes[0])
	}
}

// Asking for a relay when usher has no router session must still deliver, and
// must say so rather than silently promising a reply.
func TestRelayWithoutRouterSession(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	c := pluginapitest.Start(t, f)

	got, err := c.SendToSessionRelayed("s1", "go")
	if err != nil || got.Relayed {
		t.Fatalf("SendToSessionRelayed = %+v, %v", got, err)
	}
	if sent := f.Sent("s1"); len(sent) != 1 || sent[0] != "go" {
		t.Fatalf("prompt not delivered: %v", sent)
	}
}

func TestEventStream(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	c := pluginapitest.Start(t, f)

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
			f.Publish(broker.Event{SessionID: "s1", Type: "assistant", Raw: raw})
		case <-deadline:
			t.Fatal("no event received")
		}
	}
}

func TestInteractionsSnapshotAndLive(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.SetPending(interaction.Pending{ID: "p1", SessionID: "s1", ToolName: "Bash"})
	c := pluginapitest.Start(t, f)

	pending, cancel := c.SubscribePendingInteractions()
	defer cancel()

	first := recvPending(t, pending)
	if first.ID != "p1" {
		t.Fatalf("snapshot pending = %+v", first)
	}

	f.PushPending(interaction.Pending{ID: "p2", SessionID: "s1", ToolName: "Edit"})
	second := recvPending(t, pending)
	if second.ID != "p2" {
		t.Fatalf("live pending = %+v", second)
	}

	if err := c.RespondInteraction("p1", interaction.Response{Behavior: "allow"}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	if err := c.RespondInteraction("p1", interaction.Response{Behavior: "allow"}); err == nil {
		t.Fatal("second respond should surface the server error")
	}
	if resp := f.Response("p1"); resp.Behavior != "allow" {
		t.Fatalf("recorded response = %+v", resp)
	}
}

func recvPending(t *testing.T, ch <-chan interaction.Pending) interaction.Pending {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for pending")
		return interaction.Pending{}
	}
}

// TestReconnect kills the server mid-subscription and verifies the client
// heals onto a fresh server on the same socket path.
func TestReconnect(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	sock := pluginapitest.TempSocketPath(t)

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan struct{})
	go func() { defer close(done1); _ = pluginapi.NewServer(f, t.TempDir(), nil, nil).Run(ctx1, sock) }()

	c := pluginapi.NewClient(sock, nil)
	pluginapitest.WaitPing(t, c)
	events, cancel := c.SubscribeAllSessions()
	defer cancel()

	// Prove the first connection works.
	publishUntilReceived(t, f, events, "before")

	cancel1()
	<-done1

	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() { defer close(done2); _ = pluginapi.NewServer(f, t.TempDir(), nil, nil).Run(ctx2, sock) }()
	t.Cleanup(func() { cancel2(); <-done2 })
	pluginapitest.WaitPing(t, c)

	// The same subscription channel keeps delivering after the restart.
	publishUntilReceived(t, f, events, "after")
}

// publishUntilReceived repeatedly publishes a marker event until it arrives on
// events (the subscribe/reconnect races the publish), draining anything else.
func publishUntilReceived(t *testing.T, f *pluginapitest.FakeRouter, events <-chan broker.Event, marker string) {
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
			f.Publish(broker.Event{SessionID: "s", Type: marker})
		case <-deadline:
			t.Fatalf("marker %q never received", marker)
		}
	}
}

// TestEventTypeFilter: a client with EventTypes set must not receive other
// event types (the server filters before marshaling).
func TestEventTypeFilter(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	c := pluginapitest.Start(t, f)
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
			f.Publish(broker.Event{SessionID: "s", Type: "user"})
			f.Publish(broker.Event{SessionID: "s", Type: "assistant"})
		case <-deadline:
			t.Fatal("no event received")
		}
	}
}
