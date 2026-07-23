package claudestream

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/hook"
)

func fakeClaude(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_CLAUDE_LOG"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "${FAKE_CLAUDE_LOG}.input"
  case "$line" in
    *control_request*) ;;
    *)
      uuid=$(printf '%s\n' "$line" | sed -n 's/.*"uuid":"\([^"]*\)".*/\1/p')
      printf '{"type":"command_lifecycle","command_uuid":"%s","state":"completed"}\n' "$uuid"
      ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script, log
}

func TestLongRunningProcessServesMultipleTurns(t *testing.T) {
	bin, log := fakeClaude(t)
	m := New(bin, `{"hooks":{}}`, "/tmp/h.sock", nil, 4, nil, nil)
	m.processes = map[string]*process{}
	os.Setenv("FAKE_CLAUDE_LOG", log)
	t.Cleanup(func() { os.Unsetenv("FAKE_CLAUDE_LOG"); m.Shutdown() })
	for i := 0; i < 2; i++ {
		ch, _, fresh, _, err := m.Send(context.Background(), "sid", "hello", "/tmp", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if fresh != (i == 0) {
			t.Fatalf("turn %d fresh=%v", i, fresh)
		}
		select {
		case r := <-ch:
			if r.IsError {
				t.Fatalf("result=%+v", r)
			}
		case <-time.After(time.Second):
			t.Fatal("result timeout")
		}
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; lines != 1 {
		t.Fatalf("spawn count=%d args=%s", lines, b)
	}
	if !strings.Contains(string(b), "--session-id sid") || !strings.Contains(string(b), "--input-format stream-json") || !strings.Contains(string(b), "--include-partial-messages") {
		t.Fatalf("args=%s", b)
	}
	in, _ := os.ReadFile(log + ".input")
	if !strings.Contains(string(in), `"content":[{"text":"hello","type":"text"}]`) {
		t.Fatalf("user message is not content-block array: %s", in)
	}
	if !strings.Contains(string(in), `"uuid":"`) {
		t.Fatalf("user message has no lifecycle uuid: %s", in)
	}
}

func TestCommandLifecycleCompletesMatchingTurnAndIgnoresResult(t *testing.T) {
	req1 := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta), uuid: "u1"}
	req2 := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta), uuid: "u2"}
	p := &process{turns: []*turnRequest{req1, req2}}
	m := &Manager{}
	input := strings.NewReader(
		"{\"type\":\"command_lifecycle\",\"command_uuid\":\"u1\",\"state\":\"completed\"}\n" +
			"{\"type\":\"result\",\"subtype\":\"success\"}\n" +
			"{\"type\":\"command_lifecycle\",\"command_uuid\":\"u2\",\"state\":\"completed\"}\n")

	m.readLoop(p, input)
	if got := <-req1.done; got.IsError || got.Subtype != "completed" {
		t.Fatalf("first lifecycle result = %+v", got)
	}
	if got := <-req2.done; got.IsError || got.Subtype != "completed" {
		t.Fatalf("second lifecycle result = %+v", got)
	}
	if len(p.turns) != 0 {
		t.Fatalf("turn bookkeeping = turns:%d", len(p.turns))
	}
}

func TestCanUseToolControlRequest(t *testing.T) {
	h := hook.New("")
	m := New("", "", "", nil, 2, h, nil)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	p := &process{id: "sid", cwd: "/work", in: w}
	m.handleControlRequest(p, []byte(`{
		"type":"control_request","request_id":"req-1","request":{
			"subtype":"can_use_tool","tool_name":"Edit","tool_use_id":"tool-1",
			"input":{"file_path":"/work/a"},
			"permission_suggestions":[{"type":"addRules","behavior":"allow","destination":"session","rules":[{"toolName":"Edit"}]}]
		}}`))
	deadline := time.Now().Add(time.Second)
	for len(h.List()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pending := h.List()
	if len(pending) != 1 || !pending[0].AllowAlways || pending[0].ToolName != "Edit" {
		t.Fatalf("pending permission = %+v", pending)
	}
	if err := h.Respond(pending[0].ID, hook.Response{Behavior: "allow", Scope: "session"}); err != nil {
		t.Fatal(err)
	}

	line := make(chan []byte, 1)
	go func() {
		b, _ := bufio.NewReader(r).ReadBytes('\n')
		line <- b
	}()
	select {
	case b := <-line:
		var got struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string         `json:"subtype"`
				RequestID string         `json:"request_id"`
				Response  map[string]any `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.Type != "control_response" || got.Response.Subtype != "success" || got.Response.RequestID != "req-1" {
			t.Fatalf("response envelope = %s", b)
		}
		if got.Response.Response["behavior"] != "allow" {
			t.Fatalf("decision = %#v", got.Response.Response)
		}
		input, ok := got.Response.Response["updatedInput"].(map[string]any)
		if !ok || input["file_path"] != "/work/a" {
			t.Fatalf("updatedInput = %#v", got.Response.Response["updatedInput"])
		}
		permissions, ok := got.Response.Response["updatedPermissions"].([]any)
		if !ok || len(permissions) != 1 {
			t.Fatalf("updatedPermissions = %#v", got.Response.Response["updatedPermissions"])
		}
	case <-time.After(time.Second):
		t.Fatal("control response timeout")
	}
}

func TestPermissionPromptToolFlagWhenHandlerConfigured(t *testing.T) {
	bin, log := fakeClaude(t)
	os.Setenv("FAKE_CLAUDE_LOG", log)
	defer os.Unsetenv("FAKE_CLAUDE_LOG")
	m := New(bin, "", "", nil, 4, hook.New(""), nil)
	defer m.Shutdown()
	ch, _, _, _, err := m.Send(context.Background(), "sid", "hello", "/tmp", "", false)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--permission-prompt-tool stdio") {
		t.Fatalf("args=%s", b)
	}
}

func TestClaudePermissionControlE2E(t *testing.T) {
	if os.Getenv("USHER_CLAUDE_E2E") == "" {
		t.Skip("set USHER_CLAUDE_E2E=1 to run against the installed Claude CLI")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		t.Fatal(err)
	}
	idBytes[6] = (idBytes[6] & 0x0f) | 0x40
	idBytes[8] = (idBytes[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		idBytes[0:4], idBytes[4:6], idBytes[6:8], idBytes[8:10], idBytes[10:16])

	h := hook.New("")
	m := New("claude", "", "", []string{"--permission-mode", "default"}, 1, h, nil)
	defer m.Shutdown()
	result, _, _, _, err := m.Send(context.Background(), id,
		"Use Edit to replace the exact text before with after in "+path+", then reply only done.", dir, "", false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(h.List()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	pending := h.List()
	if len(pending) != 1 || pending[0].ToolName != "Edit" {
		t.Fatalf("pending permission = %+v", pending)
	}
	if err := h.Respond(pending[0].ID, hook.Response{Behavior: "allow", Scope: "once"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.IsError {
			t.Fatalf("Claude result = %+v", got)
		}
		if got.ContextWindow <= 0 {
			t.Fatalf("Claude lifecycle lost result runtime metadata: %+v", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Claude result timeout")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "after\n" {
		t.Fatalf("edited file = %q", b)
	}
}

func TestResumeUsesResumeFlag(t *testing.T) {
	bin, log := fakeClaude(t)
	os.Setenv("FAKE_CLAUDE_LOG", log)
	defer os.Unsetenv("FAKE_CLAUDE_LOG")
	m := New(bin, "", "", nil, 4, nil, nil)
	defer m.Shutdown()
	ch, _, _, _, err := m.Send(context.Background(), "sid", "hello", "/tmp", "", true)
	if err != nil {
		t.Fatal(err)
	}
	<-ch
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "--resume sid") {
		t.Fatalf("args=%s", b)
	}
}

func TestSpontaneousTurnTailEventsDoNotStickOrStealNextResult(t *testing.T) {
	m := New("", "", "", nil, 2, nil, nil)
	p := &process{id: "s", lastUsed: time.Now()}
	m.processes["s"] = p
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, "{\"type\":\"assistant\"}\n")
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := len(p.turns)
		p.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spontaneous marker not queued")
		}
		time.Sleep(time.Millisecond)
	}
	user := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 1), uuid: "user-1"}
	p.mu.Lock()
	p.turns = append(p.turns, user)
	p.mu.Unlock()
	_, _ = io.WriteString(w, "{\"type\":\"result\",\"subtype\":\"success\"}\n{\"type\":\"command_lifecycle\",\"command_uuid\":\"foreign\",\"state\":\"completed\"}\n{\"type\":\"rate_limit_event\"}\n")
	select {
	case <-user.done:
		t.Fatal("spontaneous result was delivered to user turn")
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = io.WriteString(w, "{\"type\":\"command_lifecycle\",\"command_uuid\":\"user-1\",\"state\":\"completed\"}\n")
	select {
	case <-user.done:
	case <-time.After(time.Second):
		t.Fatal("user result not delivered")
	}
	_ = w.Close()
	<-done
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.turns) != 0 {
		t.Fatalf("turn queue stuck: %d", len(p.turns))
	}
}

func TestMessageStartMarksSpontaneousTurnButDeltasDoNot(t *testing.T) {
	m := New("", "", "", nil, 2, nil, nil)
	p := &process{id: "s", lastUsed: time.Now()}
	m.processes["s"] = p
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, `{"type":"stream_event","event":{"type":"message_start"}}`+"\n")
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		n := len(p.turns)
		p.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("message_start did not mark a spontaneous turn")
		}
		time.Sleep(time.Millisecond)
	}
	user := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 1), uuid: "user-1"}
	p.mu.Lock()
	if p.turns[0] != nil {
		p.mu.Unlock()
		t.Fatal("spontaneous marker is not nil")
	}
	p.turns = append(p.turns, user)
	p.mu.Unlock()
	// The spontaneous turn's deltas must not leak into the queued user turn.
	_, _ = io.WriteString(w, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"spontaneous"}}}`+"\n")
	_, _ = io.WriteString(w, `{"type":"command_lifecycle","command_uuid":"foreign","state":"completed"}`+"\n")
	select {
	case <-user.done:
		t.Fatal("spontaneous result was delivered to user turn")
	case d := <-user.deltas:
		t.Fatalf("spontaneous delta leaked to user turn: %+v", d)
	case <-time.After(20 * time.Millisecond):
	}
	_, _ = io.WriteString(w, `{"type":"command_lifecycle","command_uuid":"user-1","state":"completed"}`+"\n")
	select {
	case <-user.done:
	case <-time.After(time.Second):
		t.Fatal("user result not delivered")
	}
	_ = w.Close()
	<-done
}

func TestInitAdvertisesSlashCommands(t *testing.T) {
	m := New("", "", "", nil, 2, nil, nil)
	p := &process{id: "s", lastUsed: time.Now()}
	m.processes["s"] = p
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	if commands, complete := m.CommandsIfLive("s"); commands != nil || complete {
		t.Fatalf("catalog before init = (%#v, %v), want (nil, false)", commands, complete)
	}

	_, _ = io.WriteString(w, `{"type":"system","subtype":"init","slash_commands":["compact","/review","simplify","compact",""],"skills":["simplify"]}`+"\n")
	deadline := time.Now().Add(time.Second)
	for len(m.Commands("s")) != 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	commands := m.Commands("s")
	want := []Command{
		{Name: "compact", Kind: "command"},
		{Name: "review", Kind: "command"},
		{Name: "simplify", Kind: "skill"},
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("Commands = %#v, want %#v", commands, want)
	}
	if _, complete := m.CommandsIfLive("s"); !complete {
		t.Fatal("catalog remained incomplete after system/init")
	}

	// Callers receive a copy and cannot corrupt the live catalog.
	commands[0].Name = "changed"
	if got := m.Commands("s")[0].Name; got != "compact" {
		t.Fatalf("Commands returned shared storage: %q", got)
	}
	_ = w.Close()
	<-done
}

func TestLeadingSlashCommandValidation(t *testing.T) {
	commands := []Command{{Name: "compact"}, {Name: "my-skill"}}
	tests := []struct {
		prompt  string
		command string
		known   bool
		parsed  bool
	}{
		{prompt: "/compact", command: "/compact", known: true, parsed: true},
		{prompt: "/my-skill args", command: "/my-skill", known: true, parsed: true},
		{prompt: "/wtf", command: "/wtf", parsed: true},
		// Not parsed as a command, so Send forwards it instead of failing the
		// turn with "unknown command: /home/dev/x.go".
		{prompt: "/home/dev/x.go is broken"},
		{prompt: "//not-a-command"},
		{prompt: "normal prompt"},
		{prompt: " /compact"},
	}
	for _, tt := range tests {
		command, _, parsed := backend.ParseSlashCommand(tt.prompt)
		if command != tt.command || parsed != tt.parsed {
			t.Errorf("ParseSlashCommand(%q) = (%q, %v), want (%q, %v)", tt.prompt, command, parsed, tt.command, tt.parsed)
			continue
		}
		gotKnown := hasCommand(commands, strings.TrimPrefix(command, "/"))
		if parsed && gotKnown != tt.known {
			t.Errorf("known command %q = %v, want %v", command, gotKnown, tt.known)
		}
	}
}

func TestPartialTextDeltaRoutesToCurrentTurn(t *testing.T) {
	m := New("", "", "", nil, 2, nil, nil)
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 2), uuid: "u1"}
	p := &process{id: "s", lastUsed: time.Now(), turns: []*turnRequest{req}}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`+"\n")
	select {
	case d := <-req.deltas:
		if d.Text != "hello" {
			t.Fatalf("delta = %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("delta timeout")
	}
	_, _ = io.WriteString(w, `{"type":"command_lifecycle","command_uuid":"u1","state":"completed"}`+"\n")
	<-req.done
	_ = w.Close()
	<-done
}

func TestLifecycleCarriesResultRuntimeMetadata(t *testing.T) {
	m := New("", "", "", nil, 2, nil, nil)
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 1), uuid: "u1"}
	p := &process{id: "s", lastUsed: time.Now(), turns: []*turnRequest{req}}
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() { m.readLoop(p, r); close(done) }()
	_, _ = io.WriteString(w, `{"type":"command_lifecycle","command_uuid":"u1","state":"started"}`+"\n")
	_, _ = io.WriteString(w, `{"type":"assistant","message":{"model":"claude-opus-4-7"}}`+"\n")
	_, _ = io.WriteString(w, `{"type":"result","subtype":"success","modelUsage":{"claude-sonnet-4-6":{"contextWindow":200000},"claude-opus-4-7":{"contextWindow":1000000}}}`+"\n")
	_, _ = io.WriteString(w, `{"type":"command_lifecycle","command_uuid":"u1","state":"completed"}`+"\n")
	result := <-req.done
	if result.Model != "claude-opus-4-7" || result.ContextWindow != 1000000 {
		t.Fatalf("result runtime = %+v", result)
	}
	_ = w.Close()
	<-done
}

func TestLateResultDoesNotContaminateUnstartedNextTurn(t *testing.T) {
	req1 := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta), uuid: "u1", started: true}
	req2 := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta), uuid: "u2"}
	p := &process{turns: []*turnRequest{req1, req2}}
	m := &Manager{}
	input := strings.NewReader(
		"{\"type\":\"command_lifecycle\",\"command_uuid\":\"u1\",\"state\":\"completed\"}\n" +
			"{\"type\":\"result\",\"modelUsage\":{\"stale\":{\"contextWindow\":123}}}\n" +
			"{\"type\":\"command_lifecycle\",\"command_uuid\":\"u2\",\"state\":\"completed\"}\n")

	m.readLoop(p, input)
	<-req1.done
	if got := <-req2.done; got.ContextWindow != 0 || got.Model == "stale" {
		t.Fatalf("late result contaminated next turn: %+v", got)
	}
}

func TestMaxLiveDoesNotGrowWhenAllProcessesBusy(t *testing.T) {
	m := New("missing", "", "", nil, 1, nil, nil)
	m.processes["busy"] = &process{id: "busy", turns: []*turnRequest{nil}}
	if _, _, err := m.ensure(context.Background(), "new", "/tmp", "", true); err == nil {
		t.Fatal("expected max-live busy error")
	}
	if len(m.processes) != 1 {
		t.Fatalf("process count=%d, want hard cap 1", len(m.processes))
	}
}

func TestMaxLiveDoesNotEvictLeasedProcess(t *testing.T) {
	m := New("missing", "", "", nil, 1, nil, nil)
	p := &process{id: "leased"}
	m.processes[p.id] = p
	got, fresh, err := m.leaseProcess(context.Background(), p.id, "/tmp", "", true)
	if err != nil || got != p || fresh || p.leases != 1 {
		t.Fatalf("leased ensure = (%p, %v, %v), leases=%d; want existing process with one lease", got, fresh, err, p.leases)
	}
	// Assert the reason: spawning the missing binary would also error, which
	// would let this pass even if the lease guard had failed.
	_, _, err = m.ensure(context.Background(), "new", "/tmp", "", true)
	if err == nil || !strings.Contains(err.Error(), "all busy") {
		t.Fatalf("ensure alongside a leased process = %v, want all busy", err)
	}
	if m.processes[p.id] != p {
		t.Fatal("leased process was evicted")
	}
	releaseProcess(p)
	if p.leases != 0 {
		t.Fatalf("leases after release = %d, want 0", p.leases)
	}
}
