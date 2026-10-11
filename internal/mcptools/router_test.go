package mcptools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/pluginapi"
	"github.com/nexustar/usher/internal/pluginapi/pluginapitest"
)

// start brings up a real control socket and returns a Router wired to it, so
// the tests cover the wire shape as well as the dispatch logic.
func start(t *testing.T, f *pluginapitest.FakeRouter) *Router {
	t.Helper()
	return &Router{API: pluginapitest.Start(t, f)}
}

func call(t *testing.T, r *Router, name, args string) any {
	t.Helper()
	got, err := r.Call(name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

func TestRouterToolsCatalog(t *testing.T) {
	tools := RouterTools()
	if len(tools) != 8 {
		t.Fatalf("tool count = %d, want 8", len(tools))
	}
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name] {
			t.Fatalf("duplicate tool %q", tool.Name)
		}
		seen[tool.Name] = true
		if tool.InputSchema["additionalProperties"] != false {
			t.Fatalf("%s schema permits unknown properties", tool.Name)
		}
		if tool.Meta["anthropic/alwaysLoad"] != true {
			t.Fatalf("%s is not exempt from Tool Search deferral", tool.Name)
		}
	}
}

func TestCallListFiltersAndCaps(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "active", Status: core.StatusLive})
	f.AddSession(core.Session{ID: "idle", Status: core.StatusIdle})
	f.AddSession(core.Session{ID: "old", Status: core.StatusIdle})
	f.Archive("old")
	r := start(t, f)

	payload := call(t, r, "list_sessions", `{"statuses":["live"],"limit":999}`).(map[string]any)
	if payload["returned"] != 1 || payload["total_matching"] != 1 {
		t.Fatalf("live filter = %#v", payload)
	}

	// Archived sessions are a separate query, never mixed into the default.
	payload = call(t, r, "list_sessions", `{}`).(map[string]any)
	if payload["returned"] != 2 {
		t.Fatalf("default list = %#v", payload)
	}
	payload = call(t, r, "list_sessions", `{"archived":true}`).(map[string]any)
	if payload["returned"] != 1 {
		t.Fatalf("archived list = %#v", payload)
	}

	if _, err := r.Call("list_sessions",
		json.RawMessage(`{"statuses":["bogus"]}`)); err == nil {
		t.Fatal("invalid status accepted")
	}
}

func TestCallResolveAndSendAck(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "abc12345-full", Title: "deploy work"})
	r := start(t, f)

	resolved := call(t, r, "resolve_session", `{"session_id":"deploy"}`).(map[string]any)
	if resolved["session_id"] != "abc12345-full" {
		t.Fatalf("resolve by title = %#v", resolved)
	}
	resolved = call(t, r, "resolve_session", `{"session_id":"abc12345"}`).(map[string]any)
	if resolved["session_id"] != "abc12345-full" {
		t.Fatalf("resolve by id prefix = %#v", resolved)
	}
	if _, err := r.Call("resolve_session",
		json.RawMessage(`{"session_id":"nope"}`)); err == nil {
		t.Fatal("unknown session resolved")
	}
	// Shorter than the ids usher shows, a reference is a title, not an id:
	// "abc" must not land on whichever session's id happens to start so.
	if _, err := r.Call("resolve_session", json.RawMessage(`{"session_id":"abc"}`)); err == nil {
		t.Fatal("a three-character id prefix resolved")
	}

	// send accepts the same loose reference resolve does, and acks without
	// waiting for the turn.
	f.AddSession(core.Session{ID: "router"})
	f.SetRouterSession("router")
	ack := call(t, r, "send_to_session", `{"session_id":"deploy","text":"ship it"}`).(map[string]any)
	if ack["status"] != "delivered" || ack["session_id"] != "abc12345-full" || ack["relayed"] != true {
		t.Fatalf("send ack = %#v", ack)
	}
	if sent := f.Sent("abc12345-full"); len(sent) != 1 || sent[0] != "ship it" {
		t.Fatalf("routed sends = %v", sent)
	}
	// The reply is reported to the router without prompting it: the note waits
	// for the router's next turn, and the user reads the reply in the
	// transcript rather than in a restatement.
	got := f.WaitForRelay(t, "router", 1)
	if got[0].SessionID != "abc12345-full" || got[0].Reply != "reply: ship it" || got[0].Err != nil {
		t.Fatalf("relayed reply = %+v", got[0])
	}
	if sent := f.Sent("router"); len(sent) != 0 {
		t.Fatalf("relay prompted the router: %v", sent)
	}
}

// With no router session designated, the send must still deliver, and the
// result must say so rather than implying a reply is coming.
func TestCallSendWithoutInbox(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	r := start(t, f)

	ack := call(t, r, "send_to_session", `{"session_id":"s1","text":"go"}`).(map[string]any)
	if ack["status"] != "delivered" || ack["relayed"] != false {
		t.Fatalf("send ack = %#v", ack)
	}
	if note, _ := ack["note"].(string); !strings.Contains(note, "no reply will be reported back") {
		t.Fatalf("note = %q, want the no-inbox warning", note)
	}
	if sent := f.Sent("s1"); len(sent) != 1 {
		t.Fatalf("routed sends = %v", sent)
	}
}

// The delivery contract has to reach the model twice: in the description it
// plans from, and in the result it reacts to.
func TestRouterToolsDescribeTheRelay(t *testing.T) {
	for _, tool := range RouterTools() {
		if tool.Name != "send_to_session" && tool.Name != "create_session" {
			continue
		}
		if !strings.Contains(tool.Description, "never poll") {
			t.Errorf("%s does not forbid polling: %q", tool.Name, tool.Description)
		}
		if !strings.Contains(tool.Description, "relayed=false") {
			t.Errorf("%s does not cover the no-relay case: %q", tool.Name, tool.Description)
		}
	}
}

func TestCallAmbiguousSessionRejected(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1", Title: "deploy api"})
	f.AddSession(core.Session{ID: "s2", Title: "deploy web"})
	r := start(t, f)

	_, err := r.Call("resolve_session", json.RawMessage(`{"session_id":"deploy"}`))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguous", err)
	}
}

func TestCallFlagsAndInteractions(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	f.SetPending(interaction.Pending{ID: "p1", SessionID: "s1", ToolName: "Bash"})
	r := start(t, f)

	call(t, r, "set_archived", `{"session_id":"s1","archived":true}`)
	if !f.IsArchived("s1") {
		t.Fatal("flags did not reach the router")
	}

	pending := call(t, r, "list_pending_interactions", `{}`).([]interaction.Pending)
	if len(pending) != 1 || pending[0].ID != "p1" {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestCallSearchAndTranscript(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "s1"})
	r := start(t, f)

	// A turn reaches the model as text, its tool calls as markers, and the
	// cursor for the older page only while there is one.
	page := call(t, r, "read_session_transcript", `{"session_id":"s1"}`).(transcriptPage)
	if len(page.Turns) != 1 || page.Turns[0].Content != "tail\n[tool: Bash ls]" || !page.More || page.Before != "c1" {
		t.Fatalf("transcript = %+v", page)
	}
	older := call(t, r, "read_session_transcript", `{"session_id":"s1","before":"c1"}`).(transcriptPage)
	if len(older.Turns) != 1 || older.Turns[0].Content != "head" || older.More || older.Before != "" {
		t.Fatalf("older page = %+v", older)
	}
	// from starts at a search hit and is capped client-side.
	from := call(t, r, "read_session_transcript", `{"session_id":"s1","from":"hit","limit":1}`).(transcriptPage)
	if len(from.Turns) != 1 || from.Turns[0].Content != "from hit" {
		t.Fatalf("from page = %+v", from)
	}
	scoped := call(t, r, "search_sessions", `{"session_id":"s1","query":"needle"}`).(pluginapi.SearchResult)
	if scoped.Scope != "session" || len(scoped.Hits) != 1 {
		t.Fatalf("scoped search = %+v", scoped)
	}
	global := call(t, r, "search_sessions", `{"query":"needle"}`).(pluginapi.SearchResult)
	if global.Scope != "all" || len(global.Results) != 1 {
		t.Fatalf("global search = %+v", global)
	}
	if _, err := r.Call("search_sessions",
		json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Fatal("blank query accepted")
	}
}

func TestCallCreateRelaysToTheRouterInbox(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "router"})
	f.SetRouterSession("router")
	r := start(t, f)

	ack := call(t, r, "create_session",
		`{"cwd":"/tmp/new","initial_message":"build","backend":"codex"}`).(map[string]any)
	if ack["status"] != "delivered" || ack["session_id"] != "s_new" {
		t.Fatalf("create ack = %#v", ack)
	}
	// The inbox is the router's own, never something the model chooses.
	if got := f.WaitForRelay(t, "router", 1); got[0].SessionID != "s_new" || got[0].Reply != "created: build" {
		t.Fatalf("relayed first reply = %+v", got[0])
	}
	if _, err := r.Call("create_session",
		json.RawMessage(`{"cwd":"/tmp/new"}`)); err == nil {
		t.Fatal("missing initial_message accepted")
	}
}

// The model must not be able to opt routed work out of reporting back, nor
// smuggle wire-only fields (agent) past the client-side input schema.
func TestCallCreateIgnoresWireOnlyFields(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "router"})
	f.SetRouterSession("router")
	r := start(t, f)

	ack := call(t, r, "create_session",
		`{"cwd":"/tmp/new","initial_message":"build","relay":false,"agent":"sneaky"}`).(map[string]any)
	if ack["relayed"] != true {
		t.Fatalf("model turned the relay off: %#v", ack)
	}
	f.WaitForRelay(t, "router", 1)
}

func TestCallRejectsUnknownToolAndNilAPI(t *testing.T) {
	if _, err := (&Router{}).Call("list_sessions", nil); err == nil {
		t.Fatal("router with no control API accepted a call")
	}
	r := start(t, pluginapitest.NewFakeRouter())
	if _, err := r.Call("rm_rf", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown tool accepted")
	}
}

// A session whose title mentions another session's id must not make that id
// ambiguous.
func TestResolvePrefersIDOverTitle(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "6304903c-aaaa", Title: "worker"})
	f.AddSession(core.Session{ID: "a692bb2c-bbbb", Title: "Ask session 6304903c to reply"})
	r := start(t, f)
	got := call(t, r, "resolve_session", `{"session_id":"6304903c"}`).(map[string]any)
	if got["session_id"] != "6304903c-aaaa" {
		t.Fatalf("resolved %v", got)
	}
}

// Every tool that names a session takes what the user types: usually the
// first characters of its id.
func TestCallsAcceptAnIDPrefix(t *testing.T) {
	f := pluginapitest.NewFakeRouter()
	f.AddSession(core.Session{ID: "5b6434b5-aaaa", Title: "deploy notes"})
	r := start(t, f)

	if page := call(t, r, "read_session_transcript", `{"session_id":"5b6434b5"}`).(transcriptPage); len(page.Turns) != 1 {
		t.Fatalf("transcript by prefix = %+v", page)
	}
	if found := call(t, r, "search_sessions", `{"session_id":"5b6434b5","query":"deploy"}`).(pluginapi.SearchResult); found.SessionID != "5b6434b5-aaaa" {
		t.Fatalf("search by prefix = %+v", found)
	}
	call(t, r, "set_archived", `{"session_id":"5b6434b5","archived":true}`)
	if !f.IsArchived("5b6434b5-aaaa") {
		t.Fatal("flags set by prefix did not reach the session")
	}
}
