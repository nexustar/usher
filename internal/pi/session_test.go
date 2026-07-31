package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
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

func TestStartClientPassesAppendSystemPrompt(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-pi")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := startClientWithSystemPrompt(
		bin, t.TempDir(), "", t.TempDir(), "", "Be concise.", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	got := strings.Join(c.cmd.Args, "\x00")
	if !strings.Contains(got, "--append-system-prompt\x00Be concise.") {
		t.Fatalf("args = %q", c.cmd.Args)
	}
}

func TestSystemPromptLookup(t *testing.T) {
	r := NewRuntime("pi", t.TempDir(), nil, 1, Models{}, nil, nil)
	r.SetSystemPromptLookup(func(id string) string {
		if id == "saved" {
			return "Persisted prompt."
		}
		return ""
	})
	if got := r.promptFor("saved"); got != "Persisted prompt." {
		t.Fatalf("promptFor(saved) = %q", got)
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

func TestTranscriptProjectsCompaction(t *testing.T) {
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"hello"}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-07-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"before"}]}}
{"type":"compaction","id":"c1","parentId":"a1","timestamp":"2026-07-01T10:00:03Z","summary":"internal summary"}
{"type":"message","id":"u2","parentId":"c1","timestamp":"2026-07-01T10:00:04Z","message":{"role":"user","content":"continue"}}
`)
	turns, _, err := (Transcript{}).ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 || turns[1].Role != "assistant" || turns[2].Role != "system" ||
		turns[2].Content != "Context compacted" || turns[2].UUID != "c1" {
		t.Fatalf("turns = %+v", turns)
	}
}

// The thinking level is persisted on every change, so a cold session still
// knows its current level — the last record wins.
func TestReadSessionMetaThinkingLevel(t *testing.T) {
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"hello"}}
{"type":"thinking_level_change","id":"t1","parentId":"u1","timestamp":"2026-07-01T10:00:02Z","thinkingLevel":"high"}
{"type":"thinking_level_change","id":"t2","parentId":"t1","timestamp":"2026-07-01T10:00:03Z","thinkingLevel":"low"}
`)
	meta, err := ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Runtime.Effort != "low" {
		t.Fatalf("effort = %q, want low", meta.Runtime.Effort)
	}

	// A session that never changed level records nothing; only RPC state knows
	// the model default, so meta must not claim one.
	quiet := writeFixture(t, `{"type":"session","version":3,"id":"sess-2","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"hello"}}
`)
	meta, err = ReadSessionMeta(quiet)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Runtime.Effort != "" {
		t.Fatalf("effort = %q, want empty", meta.Runtime.Effort)
	}
}

func TestTranscriptProjectsCustomMessages(t *testing.T) {
	// Extension-injected entries: display false is context-only and must stay
	// out of the transcript; the rest render whether content is a plain string
	// or the block form.
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"hello"}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-07-01T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"before"}]}}
{"type":"custom_message","id":"c1","parentId":"a1","timestamp":"2026-07-01T10:00:03Z","customType":"ext:note","content":"visible note","display":true}
{"type":"custom_message","id":"c2","parentId":"c1","timestamp":"2026-07-01T10:00:04Z","customType":"ext:state","content":"hidden state","display":false}
{"type":"custom_message","id":"c3","parentId":"c2","timestamp":"2026-07-01T10:00:05Z","customType":"ext:blocks","content":[{"type":"text","text":"block note"}],"display":true}
{"type":"message","id":"u2","parentId":"c3","timestamp":"2026-07-01T10:00:06Z","message":{"role":"user","content":"continue"}}
`)
	turns, _, err := (Transcript{}).ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ role, content, uuid string }{
		{"user", "hello", "u1"},
		{"assistant", "", "a1"},
		{"system", "visible note", "c1"},
		{"system", "block note", "c3"},
		{"user", "continue", "u2"},
	}
	if len(turns) != len(want) {
		t.Fatalf("len(turns) = %d, want %d: %+v", len(turns), len(want), turns)
	}
	for i, w := range want {
		got := turns[i]
		if got.Role != w.role || got.UUID != w.uuid || (w.content != "" && got.Content != w.content) {
			t.Fatalf("turn %d = %+v, want role=%q content=%q uuid=%q", i, got, w.role, w.content, w.uuid)
		}
	}
}

func TestTranscriptSurfacesPersistedError(t *testing.T) {
	// Each failed model response is its own stopReason "error" record; the
	// transcript must show one error turn per record, not end silently.
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-25T08:57:42Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-25T08:57:43Z","message":{"role":"user","content":"run"}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-07-25T08:57:44Z","message":{"role":"assistant","content":[{"type":"text","text":"working"}]}}
{"type":"message","id":"a2","parentId":"a1","timestamp":"2026-07-25T09:03:10Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"terminated"}}
{"type":"message","id":"a3","parentId":"a2","timestamp":"2026-07-25T09:03:13Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Service Unavailable"}}
{"type":"message","id":"a4","parentId":"a3","timestamp":"2026-07-25T09:03:18Z","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Service Unavailable"}}
`)
	turns, _, err := (Transcript{}).ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 5 {
		t.Fatalf("len(turns) = %d, want 5: %+v", len(turns), turns)
	}
	if turns[1].Role != "assistant" || len(turns[1].Parts) != 1 || turns[1].Parts[0].Content != "working" {
		t.Fatalf("assistant turn = %+v", turns[1])
	}
	want := []struct{ content, uuid string }{
		{"terminated", "a2"},
		{"Service Unavailable", "a3"},
		{"Service Unavailable", "a4"},
	}
	for i, w := range want {
		got := turns[2+i]
		if got.Role != "error" || got.Content != w.content || got.UUID != w.uuid {
			t.Fatalf("error turn %d = %+v, want role=error content=%q uuid=%q", i, got, w.content, w.uuid)
		}
	}
}

func TestTranscriptSurfacesAbortedTurn(t *testing.T) {
	// An interrupt persists as stopReason "aborted", either with the text that
	// streamed before it or with no content at all. Both must be visible as an
	// interruption rather than passing for a finished answer.
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-23T15:46:13Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-23T15:46:14Z","message":{"role":"user","content":"write an essay"}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-07-23T15:46:20Z","message":{"role":"assistant","content":[{"type":"text","text":"Zero began as an absence"}],"stopReason":"aborted","errorMessage":"Request was aborted"}}
{"type":"message","id":"u2","parentId":"a1","timestamp":"2026-07-23T15:47:00Z","message":{"role":"user","content":"again"}}
{"type":"message","id":"a2","parentId":"u2","timestamp":"2026-07-23T15:47:05Z","message":{"role":"assistant","content":[],"stopReason":"aborted","errorMessage":"Request was aborted"}}
`)
	turns, _, err := (Transcript{}).ReadTurns(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 5 {
		t.Fatalf("len(turns) = %d, want 5: %+v", len(turns), turns)
	}
	// The partial text survives as its own turn, followed by the marker.
	if turns[1].Role != "assistant" || len(turns[1].Parts) != 1 ||
		turns[1].Parts[0].Content != "Zero began as an absence" {
		t.Fatalf("partial assistant turn = %+v", turns[1])
	}
	for _, i := range []int{2, 4} {
		if turns[i].Role != "error" || turns[i].Content != "Request was aborted" {
			t.Fatalf("turns[%d] = %+v, want an aborted marker", i, turns[i])
		}
	}
	// A content-free abort must not leave an empty assistant shell behind.
	if turns[4].UUID != "a2" {
		t.Fatalf("turns[4].UUID = %q, want a2", turns[4].UUID)
	}
}

func TestIsTurnComplete(t *testing.T) {
	// "toolUse" is the agent loop continuing, not the turn ending — treating it
	// as complete would relay a foreign turn mid-tool-call.
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"stop", `{"type":"message","id":"a1","message":{"role":"assistant","content":[],"stopReason":"stop"}}`, true},
		{"stop with text", `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop"}}`, true},
		// A truncated response whose tool calls pi will fail and retry is still
		// mid-turn: toolResult and further assistant records follow it.
		{"length with tool call", `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"read"}],"stopReason":"length"}}`, false},
		{"length without tool call", `{"type":"message","id":"a1","message":{"role":"assistant","content":[{"type":"text","text":"cut off"}],"stopReason":"length"}}`, true},
		{"length empty content", `{"type":"message","id":"a1","message":{"role":"assistant","content":[],"stopReason":"length"}}`, true},
		// Unparseable content must not be mistaken for a finished turn.
		{"length unparseable content", `{"type":"message","id":"a1","message":{"role":"assistant","content":"oops","stopReason":"length"}}`, false},
		{"tool use", `{"type":"message","id":"a1","message":{"role":"assistant","content":[],"stopReason":"toolUse"}}`, false},
		{"aborted", `{"type":"message","id":"a1","message":{"role":"assistant","content":[],"stopReason":"aborted"}}`, false},
		{"error", `{"type":"message","id":"a1","message":{"role":"assistant","content":[],"stopReason":"error"}}`, false},
		{"user", `{"type":"message","id":"u1","message":{"role":"user","content":"hi"}}`, false},
		{"tool result", `{"type":"message","id":"t1","message":{"role":"toolResult","content":"ok"}}`, false},
		{"compaction", `{"type":"compaction","id":"c1","summary":"s"}`, false},
		{"thinking level change", `{"type":"thinking_level_change","id":"k1","thinkingLevel":"high"}`, false},
		{"session header", `{"type":"session","id":"sess-1","cwd":"/work"}`, false},
		{"garbage", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Transcript{}).IsTurnComplete([]byte(tc.line)); got != tc.want {
				t.Errorf("IsTurnComplete = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRenameSessionUsesPiSessionInfo(t *testing.T) {
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"hello pi"}}
`)
	if err := RenameSession(path, "  Native\r\n\npi title  "); err != nil {
		t.Fatal(err)
	}
	meta, err := ReadSessionMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Title != "Native pi title" {
		t.Fatalf("Title = %q", meta.Title)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var got entry
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "session_info" || got.ParentID == nil || *got.ParentID != "u1" || len(got.ID) != 8 {
		t.Fatalf("session_info = %+v", got)
	}
}

func TestRuntimeRenameUsesRPCForLiveWorker(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rpc.log")
	bin := filepath.Join(dir, "fake-pi")
	body := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$FAKE_PI_LOG"
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"type":"get_state"'*)
      printf '{"type":"response","id":"%s","success":true,"data":{"model":{"provider":"anthropic","id":"claude-x"},"thinkingLevel":"high"}}\n' "$id"
      continue
      ;;
    *'"type":"get_session_stats"'*)
      printf '{"type":"response","id":"%s","success":true,"data":{"contextUsage":{"tokens":42000,"contextWindow":200000,"percent":21}}}\n' "$id"
      continue
      ;;
  esac
  printf '{"type":"response","id":"%s","success":true,"data":{}}\n' "$id"
done
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	path := writeFixture(t, `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-07-01T10:00:00Z","cwd":"/work"}`+"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_PI_LOG", logPath)
	c, err := startClient(bin, dir, path, dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.stop)
	w := &worker{c: c, cwd: dir, path: path, last: time.Now()}
	r := &Runtime{workers: map[string]*worker{"sess-1": w}, max: 1}
	if err := r.Rename(context.Background(), "sess-1", path, "Live title"); err != nil {
		t.Fatal(err)
	}
	events, err := r.Send(context.Background(), "sess-1", "/name Command title", dir)
	if err != nil {
		t.Fatal(err)
	}
	var eventTypes []string
	for event := range events {
		eventTypes = append(eventTypes, event.Type)
	}
	if !reflect.DeepEqual(eventTypes, []string{backend.EventProcessStarted, backend.EventProcessExit}) {
		t.Fatalf("command events = %v", eventTypes)
	}
	events, err = r.Send(context.Background(), "sess-1", "/compact Keep the API details", dir)
	if err != nil {
		t.Fatal(err)
	}
	eventTypes = eventTypes[:0]
	var runtimeEvent backend.Event
	for event := range events {
		eventTypes = append(eventTypes, event.Type)
		if event.Type == backend.EventRuntime {
			runtimeEvent = event
		}
	}
	if !reflect.DeepEqual(eventTypes, []string{
		backend.EventProcessStarted, backend.EventTurnStatus, backend.EventRuntime, backend.EventProcessExit,
	}) {
		t.Fatalf("compact events = %v", eventTypes)
	}
	var runtime core.SessionRuntime
	if err := json.Unmarshal(runtimeEvent.Raw, &runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.Model != "claude-x" || runtime.Effort != "high" ||
		runtime.ContextTokens != 42000 || runtime.ContextWindow != 200000 {
		t.Fatalf("runtime = %+v", runtime)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("live rename modified session JSONL directly")
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), `"type":"set_session_name"`) ||
		!strings.Contains(string(logged), `"name":"Live title"`) ||
		!strings.Contains(string(logged), `"name":"Command title"`) ||
		!strings.Contains(string(logged), `"type":"compact"`) ||
		!strings.Contains(string(logged), `"customInstructions":"Keep the API details"`) ||
		strings.Contains(string(logged), `"type":"prompt"`) {
		t.Fatalf("RPC log = %s", logged)
	}
	if w.leases != 0 {
		t.Fatalf("worker lease leaked: %d", w.leases)
	}
}

// A manual /compact must swallow the events pi streams before the RPC response
// so they cannot leak into the next turn's event consumption.
func TestCompactDrainsPreResponseEvents(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"type":"compact"'*)
      printf '{"type":"compaction_start","reason":"manual"}\n'
      printf '{"type":"compaction_end","reason":"manual"}\n'
      printf '{"type":"response","id":"%s","success":true,"data":{}}\n' "$id"
      ;;
    *)
      printf '{"type":"response","id":"%s","success":true,"data":{}}\n' "$id"
      ;;
  esac
done
`
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := startClient(bin, dir, "", dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.stop)
	w := &worker{c: c, cwd: dir, path: path, last: time.Now()}
	r := &Runtime{workers: map[string]*worker{"s1": w}, max: 1}

	ch, err := r.Send(context.Background(), "s1", "/compact", dir)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range collectPi(t, ch, 5*time.Second) {
		types = append(types, ev.Type)
	}
	if !reflect.DeepEqual(types, []string{
		backend.EventProcessStarted, backend.EventTurnStatus, backend.EventProcessExit,
	}) {
		t.Fatalf("compact events = %v", types)
	}
	if n := len(w.c.events); n != 0 {
		t.Errorf("%d pre-response events leaked into the next turn, want 0", n)
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
