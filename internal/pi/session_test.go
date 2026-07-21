package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
)

func TestPiPermissionSystemRequestRecognition(t *testing.T) {
	valid := extensionUIRequest{Method: "select", Title: "Permission Required\nbash: git push", Options: append([]string(nil), piPermissionSystemOptions...)}
	if !isPiPermissionSystemRequest(valid) {
		t.Fatal("pi-permission-system request not recognized")
	}
	for _, mutate := range []func(*extensionUIRequest){
		func(r *extensionUIRequest) { r.Method = "confirm" },
		func(r *extensionUIRequest) { r.Title = "Choose a model" },
		func(r *extensionUIRequest) { r.Options = []string{"Yes", "No"} },
	} {
		copy := valid
		mutate(&copy)
		if isPiPermissionSystemRequest(copy) {
			b, _ := json.Marshal(copy)
			t.Fatalf("unrelated request recognized: %s", b)
		}
	}
}

func TestRPCDataCancelled(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		{"cancelled", `{"cancelled":true}`, true},
		{"not cancelled", `{"cancelled":false}`, false},
		{"ordinary response", `{"sessionId":"new"}`, false},
		{"non-object response", `null`, false},
		{"malformed response", `{`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := rpcDataCancelled(json.RawMessage(tt.raw)); got != tt.want {
				t.Fatalf("rpcDataCancelled(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestClientReapsExitedProcess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := startClient(bin, t.TempDir(), "", t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not reap exited process")
	}
	if c.cmd.ProcessState == nil || !c.cmd.ProcessState.Exited() {
		t.Fatalf("process was not waited: %+v", c.cmd.ProcessState)
	}
}

func TestCleanupForkFailureDoesNotRestoreRemovedWorker(t *testing.T) {
	w := &worker{busy: true}
	r := &Runtime{workers: map[string]*worker{}}
	r.cleanupForkFailure("source", w, false, false)
	if r.workers["source"] != nil {
		t.Fatal("concurrently removed worker was restored")
	}

	r.workers["source"] = w
	r.cleanupForkFailure("source", w, false, false)
	if r.workers["source"] != w || w.busy {
		t.Fatalf("owned source worker was not released: worker=%p busy=%v", r.workers["source"], w.busy)
	}
}

func TestLeasedWorkerIsNotEvicted(t *testing.T) {
	source := &worker{last: time.Now()}
	r := &Runtime{max: 1, workers: map[string]*worker{"source": source}}
	leased := r.leaseWorkerIfLive("source")
	if leased != source || source.leases != 1 {
		t.Fatalf("leaseWorkerIfLive = %p, leases = %d; want source, 1", leased, source.leases)
	}

	err := r.add("other", &worker{last: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "all busy") {
		t.Fatalf("add with leased worker error = %v, want all busy", err)
	}
	if r.workers["source"] != source {
		t.Fatal("leased source worker was evicted")
	}
	r.releaseWorker(source)
	if source.leases != 0 {
		t.Fatalf("leases after release = %d, want 0", source.leases)
	}
}

func TestTailPiJSONLLeavesPartialRecord(t *testing.T) {
	path := writeFixture(t, "{\"type\":\"message\",\"id\":\"one\"}\n{\"type\":\"message\"")
	offset := int64(0)
	out := make(chan backend.Event, 2)
	grew, err := tailPiJSONL(context.Background(), path, &offset, out)
	if err != nil || !grew {
		t.Fatalf("first tail: grew=%v err=%v", grew, err)
	}
	if offset != int64(len("{\"type\":\"message\",\"id\":\"one\"}\n")) {
		t.Fatalf("offset=%d", offset)
	}
	if ev := <-out; ev.Type != "message" {
		t.Fatalf("event=%+v", ev)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(",\"id\":\"two\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	grew, err = tailPiJSONL(context.Background(), path, &offset, out)
	if err != nil || !grew {
		t.Fatalf("second tail: grew=%v err=%v", grew, err)
	}
	if ev := <-out; ev.Type != "message" {
		t.Fatalf("event=%+v", ev)
	}
}

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadSessionMeta(t *testing.T) {
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"hello pi","timestamp":1782900001000}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-07-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"provider":"anthropic","model":"claude-x"}}
`)
	meta, err := ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "sess-1" || meta.Cwd != "/work" || meta.Prompt != "hello pi" || meta.Runtime.Model != "claude-x" {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestTranscriptSelectsActiveBranch(t *testing.T) {
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"first"}}
{"type":"message","id":"old","parentId":"u1","timestamp":"2026-07-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"abandoned"}]}}
{"type":"message","id":"new","parentId":"u1","timestamp":"2026-07-01T10:00:03Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"active"},{"type":"toolCall","id":"tc1","name":"bash","arguments":{"command":"go test ./..."}}],"model":"model-1"}}
{"type":"message","id":"tr1","parentId":"new","timestamp":"2026-07-01T10:00:04Z","message":{"role":"toolResult","toolCallId":"tc1","toolName":"bash","content":[{"type":"text","text":"ok"}]}}
`)
	turns, total, err := (Transcript{}).ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(turns) != 2 {
		t.Fatalf("turns=%+v total=%d", turns, total)
	}
	if turns[1].Model != "model-1" || len(turns[1].Parts) != 3 {
		t.Fatalf("assistant=%+v", turns[1])
	}
	for _, p := range turns[1].Parts {
		if p.Content == "abandoned" {
			t.Fatal("inactive branch was rendered")
		}
	}
	if turns[1].Parts[2].ToolTarget != "go test ./..." || turns[1].Parts[2].Content != "```\nok\n```" {
		t.Fatalf("parts=%+v", turns[1].Parts)
	}
}

func TestForkRPCCommand(t *testing.T) {
	parent := func(s string) *string { return &s }
	user := func(id, parentID string) rpcEntry {
		e := rpcEntry{Type: "message", ID: id, ParentID: parent(parentID)}
		e.Message.Role = "user"
		return e
	}
	state := entriesState{LeafID: "a3", Entries: []rpcEntry{
		{Type: "message", ID: "u1"},
		{Type: "message", ID: "a1", ParentID: parent("u1")},
		{Type: "model_change", ID: "m2", ParentID: parent("a1")},
		user("u2", "m2"),
		{Type: "message", ID: "a2", ParentID: parent("u2")},
		user("u3", "a2"),
		{Type: "message", ID: "a3", ParentID: parent("u3")},
		user("u2x", "a1"), // abandoned sibling
	}}
	for _, tt := range []struct{ after, command, entry string }{
		{"a1", "fork", "u2"},
		{"a2", "fork", "u3"},
		{"a3", "clone", ""},
	} {
		command, entry, err := forkRPCCommand(state, tt.after)
		if err != nil {
			t.Fatalf("after %s: %v", tt.after, err)
		}
		if command != tt.command || entry != tt.entry {
			t.Errorf("after %s: got %s %s, want %s %s", tt.after, command, entry, tt.command, tt.entry)
		}
	}
	if _, _, err := forkRPCCommand(state, "u2x"); err == nil {
		t.Fatal("abandoned branch fork point accepted")
	}
}

func TestRenderTerminalToolResult(t *testing.T) {
	if got := renderToolResult("read", "package pi\n"); got != "```\npackage pi\n\n```" {
		t.Fatalf("plain read result = %q", got)
	}
	if got := renderToolResult("Read", "before\n```go\nafter\n```"); !strings.HasPrefix(got, "````\n") || !strings.HasSuffix(got, "\n````") {
		t.Fatalf("embedded fence was not widened: %q", got)
	}
	if got := renderToolResult("bash", "# output"); got != "```\n# output\n```" {
		t.Fatalf("bash result = %q", got)
	}
	if got := renderToolResult("grep", "README.md:1:# usher"); got != "```\nREADME.md:1:# usher\n```" {
		t.Fatalf("grep result = %q", got)
	}
	if got := renderToolResult("extension", "# markdown"); got != "# markdown" {
		t.Fatalf("non-terminal result changed: %q", got)
	}
}

func TestFeedLinePartsReturnsEveryAssistantBlock(t *testing.T) {
	a := NewAssembler()
	raw := []byte(`{"type":"message","id":"a1","timestamp":"2026-07-01T10:00:00Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"first"},{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"one"}},{"type":"toolCall","id":"t2","name":"read","arguments":{"path":"two.go"}}]}}`)
	_, parts := a.FeedLineParts(raw)
	if len(parts) != 4 {
		t.Fatalf("got %d live parts, want 4: %+v", len(parts), parts)
	}
	if parts[0].Type != "thinking" || parts[1].Content != "first" || parts[2].ToolTarget != "one" || parts[3].ToolTarget != "two.go" {
		t.Fatalf("live parts out of order or incomplete: %+v", parts)
	}
	if turn := a.Flush(); turn == nil || len(turn.Parts) != 4 {
		t.Fatalf("canonical turn does not match live parts: %+v", turn)
	}
}
