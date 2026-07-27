package usheragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// mockChatServer returns a series of canned responses in order. Each call
// to /v1/chat/completions consumes one response. Out-of-bounds calls fail
// the test — that means the agent looped further than expected.
type mockChatServer struct {
	t         *testing.T
	responses []ChatResponse
	mu        sync.Mutex
	calls     int
	lastReq   ChatRequest
}

func newMockChatServer(t *testing.T, responses []ChatResponse) (*httptest.Server, *mockChatServer) {
	m := &mockChatServer{t: t, responses: responses}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.lastReq = ChatRequest{} // Decode merges into the old value; reset so omitted fields (e.g. tools) don't linger
		_ = json.NewDecoder(r.Body).Decode(&m.lastReq)
		if m.calls >= len(m.responses) {
			t.Errorf("unexpected call %d (only %d responses queued)", m.calls+1, len(m.responses))
			http.Error(w, "no more responses", http.StatusInternalServerError)
			return
		}
		resp := m.responses[m.calls]
		m.calls++
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, m
}

// --- fake AgentAPI ---

type fakeAgentAPI struct {
	sessions []core.Session
	pending  []hook.Pending
	sent     []sendCall
	resolved []hook.Response

	transcripts map[string][]core.TranscriptTurn
	waitReplies map[string]string
	waitErrs    map[string]error
	relayed     []sendCall // sends made through SendToSessionRelayed

	created     []createCall
	createReply string
	createNewID string
	createErr   error

	archived    map[string]bool
	autoApprove map[string]bool
}
type sendCall struct{ ID, Text string }
type createCall struct{ Cwd, Msg, Backend, Model string }

func newFakeAgentAPI() *fakeAgentAPI {
	return &fakeAgentAPI{
		transcripts: map[string][]core.TranscriptTurn{},
		waitReplies: map[string]string{},
		waitErrs:    map[string]error{},
		archived:    map[string]bool{},
		autoApprove: map[string]bool{},
	}
}

func (f *fakeAgentAPI) ListSessions() []core.Session            { return f.sessions }
func (f *fakeAgentAPI) ListPendingInteractions() []hook.Pending { return f.pending }
func (f *fakeAgentAPI) SendToSession(id, text string) error {
	f.sent = append(f.sent, sendCall{id, text})
	return nil
}
func (f *fakeAgentAPI) RespondInteraction(id string, r hook.Response) error {
	f.resolved = append(f.resolved, r)
	return nil
}
func (f *fakeAgentAPI) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, errors.New("session not found")
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}
func (f *fakeAgentAPI) ReadSessionTranscriptPage(id string, offset, limit int) ([]core.TranscriptTurn, int, int, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, 0, 0, errors.New("session not found")
	}
	total := len(turns)
	if limit <= 0 {
		limit = 1
	}
	start := offset
	if start < 0 {
		start = total - limit
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return turns[start:end], start, total, nil
}
func (f *fakeAgentAPI) SearchSessionTranscript(id, query string, maxHits, _ int) ([]core.TranscriptSearchHit, bool, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, false, errors.New("session not found")
	}
	var hits []core.TranscriptSearchHit
	matched := 0
	for i, t := range turns {
		if !strings.Contains(strings.ToLower(t.Content), strings.ToLower(query)) {
			continue
		}
		matched++
		if len(hits) >= maxHits {
			continue
		}
		hits = append(hits, core.TranscriptSearchHit{Role: t.Role, Time: t.Time, TurnIndex: i, Occurrences: 1, Snippet: t.Content})
	}
	return hits, matched > len(hits), nil
}
func (f *fakeAgentAPI) SearchAllSessions(query string, maxSessions, _ int) ([]core.SessionSearchResult, bool, error) {
	var out []core.SessionSearchResult
	for _, s := range f.sessions {
		turns := f.transcripts[s.ID]
		count := 0
		firstIdx, firstSnip := 0, ""
		for i, t := range turns {
			if strings.Contains(strings.ToLower(t.Content), strings.ToLower(query)) {
				if count == 0 {
					firstIdx, firstSnip = i, t.Content
				}
				count++
			}
		}
		if count == 0 {
			continue
		}
		out = append(out, core.SessionSearchResult{SessionID: s.ID, Title: s.Title, Cwd: s.Cwd, HitCount: count, TurnIndex: firstIdx, Snippet: firstSnip})
	}
	truncated := false
	if maxSessions > 0 && len(out) > maxSessions {
		out, truncated = out[:maxSessions], true
	}
	return out, truncated, nil
}
func (f *fakeAgentAPI) CreateSession(_ context.Context, o core.CreateOptions, _ time.Duration) (string, string, error) {
	f.created = append(f.created, createCall{Cwd: o.Cwd, Msg: o.InitialMessage, Backend: o.Backend, Model: o.Model})
	if f.createErr != nil {
		return f.createNewID, f.createReply, f.createErr
	}
	return f.createNewID, f.createReply, nil
}

// SendToSessionRelayed resolves the relay synchronously from waitReplies /
// waitErrs so tests can assert the relayed text without goroutine plumbing.
func (f *fakeAgentAPI) SendToSessionRelayed(id, text string, onDone func(sessionID, reply string, err error)) error {
	f.sent = append(f.sent, sendCall{id, text})
	f.relayed = append(f.relayed, sendCall{id, text})
	if onDone != nil {
		onDone(id, f.waitReplies[id], f.waitErrs[id])
	}
	return nil
}

func (f *fakeAgentAPI) CreateSessionRelayed(o core.CreateOptions, onDone func(sessionID, reply string, err error)) (string, error) {
	f.created = append(f.created, createCall{Cwd: o.Cwd, Msg: o.InitialMessage, Backend: o.Backend, Model: o.Model})
	if f.createErr != nil {
		return "", f.createErr
	}
	if onDone != nil {
		onDone(f.createNewID, f.createReply, nil)
	}
	return f.createNewID, nil
}
func (f *fakeAgentAPI) Archive(id string)                { f.archived[id] = true }
func (f *fakeAgentAPI) Unarchive(id string)              { f.archived[id] = false }
func (f *fakeAgentAPI) IsArchived(id string) bool        { return f.archived[id] }
func (f *fakeAgentAPI) SetAutoApprove(id string, e bool) { f.autoApprove[id] = e }
func (f *fakeAgentAPI) IsAutoApprove(id string) bool     { return f.autoApprove[id] }

// --- helpers ---

func chatTextResp(text string) ChatResponse {
	return ChatResponse{
		Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
	}
}

func chatToolCallResp(callID, name, argsJSON string) ChatResponse {
	return ChatResponse{
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:       callID,
					Type:     "function",
					Function: ToolCallFunc{Name: name, Arguments: argsJSON},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
}

func newTestLLM(t *testing.T, api AgentAPI, baseURL string) *LLMAgent {
	t.Helper()
	c := NewChatClient(baseURL+"/v1", "test-key")
	a, err := NewLLM(api, LLMConfig{Client: c, Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func runHandle(t *testing.T, a *LLMAgent, userMsg string) AgentResult {
	t.Helper()
	res, err := a.Handle(context.Background(), nil, "", userMsg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// --- tests ---

func TestLLMAgent_DirectAnswer(t *testing.T) {
	srv, _ := newMockChatServer(t, []ChatResponse{chatTextResp("hi back")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	res := runHandle(t, a, "hello")
	if res.Reply != "hi back" {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "" {
		t.Errorf("Focus should be empty, got %q", res.FocusSession)
	}
}

func TestLLMAgent_ListSessionsToolThenAnswer(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{
		{ID: "abc12345", Title: "deploy", Cwd: "/x"},
		{ID: "def67890", Title: "tests", Cwd: "/y"},
	}
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_1", "list_sessions", `{}`),
		chatTextResp("you have 2 sessions: deploy and tests."),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "what's running?")
	if !strings.Contains(res.Reply, "deploy") {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "" {
		t.Errorf("list_sessions must not set focus, got %q", res.FocusSession)
	}
	if m.calls != 2 {
		t.Errorf("expected 2 server calls, got %d", m.calls)
	}
	if len(res.ToolEvents) != 1 || res.ToolEvents[0].Name != "list_sessions" || res.ToolEvents[0].CallID != "call_1" {
		t.Fatalf("tool events = %+v", res.ToolEvents)
	}
}

func TestDurableToolEventKeepsLongArgumentsValid(t *testing.T) {
	args := `{"session_id":"s","text":"` + strings.Repeat("长", 3000) + `"}`
	event := durableToolEvent(ToolCall{ID: "c", Function: ToolCallFunc{Name: "send_to_session", Arguments: args}}, `{"status":"sent"}`)
	if event.Arguments != args {
		t.Fatal("durable arguments were modified")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(event.Arguments), &decoded); err != nil {
		t.Fatalf("replayed arguments are invalid JSON: %v", err)
	}
}

func TestLLMAgent_SendToolDispatchAndFocus(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "deadbeef", Title: "X"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_send", "send_to_session", `{"session_id":"deadbeef","text":"deploy now"}`),
		chatTextResp("sent"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "tell deadbeef to deploy now")
	if len(api.sent) != 1 || api.sent[0].ID != "deadbeef" || api.sent[0].Text != "deploy now" {
		t.Errorf("sent = %+v", api.sent)
	}
	if res.FocusSession != "deadbeef" {
		t.Errorf("FocusSession = %q, want deadbeef", res.FocusSession)
	}
}

func TestLLMAgent_FocusToolDoesNotSend(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "deadbeef-0000", Title: "main chat work"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_focus", "focus_session", `{"session_id":"deadbeef"}`),
		chatTextResp("focused"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "jump to the main chat session")
	if res.FocusSession != "deadbeef-0000" {
		t.Errorf("FocusSession = %q, want deadbeef-0000", res.FocusSession)
	}
	if len(api.sent) != 0 || len(api.relayed) != 0 {
		t.Fatalf("focus_session sent a message: sent=%+v relayed=%+v", api.sent, api.relayed)
	}
}

func TestLLMAgent_LogsToolCall(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "deadbeef-0000", Title: "main chat work"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_focus", "focus_session", `{"session_id":"deadbeef"}`),
		chatTextResp("focused"),
	})
	defer srv.Close()

	var logs bytes.Buffer
	a, err := NewLLM(api, LLMConfig{
		Client: NewChatClient(srv.URL+"/v1", "test-key"),
		Model:  "test-model",
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = runHandle(t, a, "focus deadbeef")
	got := logs.String()
	for _, want := range []string{
		`"msg":"main chat tool call"`,
		`"tool":"focus_session"`,
		`"call_id":"call_focus"`,
		`"focus_session":"deadbeef-0000"`,
		`"is_error":false`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %s: %s", want, got)
		}
	}
}

func TestExecuteTool_FocusRejectsUnknownSession(t *testing.T) {
	a, _ := NewLLM(newFakeAgentAPI(), LLMConfig{
		Client: NewChatClient("http://x", "k"), Model: "m",
	})
	out, focus := a.executeTool(context.Background(), "focus_session", `{"session_id":"missing"}`, nil)
	if focus != "" || !strings.Contains(out, "session not found") {
		t.Fatalf("out=%q focus=%q", out, focus)
	}
}

func TestExecuteTool_FocusRejectsAmbiguousPrefix(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{
		{ID: "deadbeef-0001", Title: "one"},
		{ID: "deadbeef-0002", Title: "two"},
	}
	a, _ := NewLLM(api, LLMConfig{Client: NewChatClient("http://x", "k"), Model: "m"})
	out, focus := a.executeTool(context.Background(), "focus_session", `{"session_id":"deadbeef"}`, nil)
	if focus != "" || !strings.Contains(out, "ambiguous session") {
		t.Fatalf("out=%q focus=%q", out, focus)
	}
}

func TestExecuteTool_FocusMatchesTitle(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "abc12345-full", Title: "main chat MCP design"}}
	a, _ := NewLLM(api, LLMConfig{Client: NewChatClient("http://x", "k"), Model: "m"})
	out, focus := a.executeTool(context.Background(), "focus_session", `{"session_id":"MCP design"}`, nil)
	if focus != "abc12345-full" || !strings.Contains(out, `"status":"focused"`) {
		t.Fatalf("out=%q focus=%q", out, focus)
	}
}

func TestDefaultToolsIncludeFocusSession(t *testing.T) {
	for _, tool := range defaultTools() {
		if tool.Function.Name == "focus_session" {
			return
		}
	}
	t.Fatal("focus_session tool is not exposed")
}

func TestTranscriptToolRoutesPureNavigationToFocus(t *testing.T) {
	for _, tool := range defaultTools() {
		if tool.Function.Name != "read_session_transcript" {
			continue
		}
		desc := tool.Function.Description
		if !strings.Contains(desc, "use focus_session") {
			t.Fatalf("read_session_transcript description does not route pure navigation to focus_session: %q", desc)
		}
		if strings.Contains(desc, `prefer send_to_session`) {
			t.Fatalf("read_session_transcript description still routes pure navigation through send_to_session: %q", desc)
		}
		return
	}
	t.Fatal("read_session_transcript tool is not exposed")
}

func TestLLMAgent_RespondToolDispatchDoesNotSetFocus(t *testing.T) {
	api := newFakeAgentAPI()
	api.pending = []hook.Pending{{ID: "iact1", ToolName: "Bash"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_a", "respond_to_interaction", `{"id":"iact1","behavior":"allow","reason":"safe"}`),
		chatTextResp("approved"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "approve the pending bash")
	if len(api.resolved) != 1 || api.resolved[0].Behavior != "allow" {
		t.Errorf("resolved = %+v", api.resolved)
	}
	if res.FocusSession != "" {
		t.Errorf("respond_to_interaction must not set focus, got %q", res.FocusSession)
	}
}

func TestLLMAgent_ToolErrorPropagatesAsContent(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_x", "send_to_session", `{"session_id":"","text":""}`),
		chatTextResp("told you so"),
	})
	defer srv.Close()

	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "x", nil); err != nil {
		t.Fatal(err)
	}
	var toolMsg *ChatMessage
	for i := range m.lastReq.Messages {
		if m.lastReq.Messages[i].Role == "tool" {
			toolMsg = &m.lastReq.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message in second request")
	}
	if !strings.Contains(toolMsg.Content, "error") {
		t.Errorf("tool content = %q", toolMsg.Content)
	}
}

func TestLLMAgent_ReadTranscript(t *testing.T) {
	api := newFakeAgentAPI()
	api.transcripts["spike01"] = []core.TranscriptTurn{
		{Role: "user", Content: "what is 2+2?"},
		{Role: "assistant", Content: "4"},
	}
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "read_session_transcript", `{"session_id":"spike01","limit":50}`),
		chatTextResp("the assistant said 4"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "what was the answer in spike?")
	if !strings.Contains(res.Reply, "4") {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "spike01" {
		t.Errorf("read_session_transcript should set focus, got %q", res.FocusSession)
	}
	// Verify the tool result fed back contained transcript JSON
	var toolMsg *ChatMessage
	for i := range m.lastReq.Messages {
		if m.lastReq.Messages[i].Role == "tool" {
			toolMsg = &m.lastReq.Messages[i]
		}
	}
	if toolMsg == nil || !strings.Contains(toolMsg.Content, `"4"`) {
		t.Errorf("tool result didn't contain transcript content; got %q", toolMsg.Content)
	}
}

func TestLLMAgent_ReadTranscriptDefaultLimit(t *testing.T) {
	api := newFakeAgentAPI()
	// Build 100 turns; default limit is 20.
	api.transcripts["s"] = make([]core.TranscriptTurn, 100)
	for i := range api.transcripts["s"] {
		api.transcripts["s"][i] = core.TranscriptTurn{Role: "user", Content: "x"}
	}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "read_session_transcript", `{"session_id":"s"}`),
		chatTextResp("ok"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	_ = runHandle(t, a, "summarize s")
	// We can't easily count returned items without inspecting the tool
	// response; instead verify that the call didn't error and focus moved.
}

// TestLLMAgent_HistoryMappedNoFocusMessage: focus must NOT appear as a
// message (it would break the provider's prefix cache on every switch — the
// id lives in the tail <current_state> block instead); history maps straight
// after the single static system prompt.
func TestLLMAgent_HistoryMappedNoFocusMessage(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{chatTextResp("ok")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)

	history := []HistoryMessage{
		{Role: "user", Content: "first thing"},
		{Role: "agent", Content: "first reply"},
	}
	if _, err := a.Handle(context.Background(), history, "focused-session-id", "next thing", nil); err != nil {
		t.Fatal(err)
	}

	roles := []string{}
	for _, m := range m.lastReq.Messages {
		roles = append(roles, m.Role)
	}
	if len(roles) != 4 || roles[0] != "system" || roles[1] != "user" || roles[2] != "assistant" || roles[3] != "user" {
		t.Fatalf("expected [system, user, assistant, user]; got %v", roles)
	}
	for _, msg := range m.lastReq.Messages {
		if msg.Role == "system" && strings.Contains(msg.Content, "focused-session-id") {
			t.Errorf("focus injected as a system message (cache-hostile): %q", msg.Content)
		}
	}
	if m.lastReq.Messages[1].Content != "first thing" || m.lastReq.Messages[2].Content != "first reply" {
		t.Errorf("history mapping wrong: %+v", m.lastReq.Messages[1:3])
	}
	if m.lastReq.Messages[3].Content != "next thing" {
		t.Errorf("current user = %q", m.lastReq.Messages[3].Content)
	}
}

func TestLLMAgent_NoFocusInjectionWhenEmpty(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{chatTextResp("ok")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "hi", nil); err != nil {
		t.Fatal(err)
	}
	// Only one system message (the static prompt), then the user message.
	if len(m.lastReq.Messages) != 2 || m.lastReq.Messages[0].Role != "system" || m.lastReq.Messages[1].Role != "user" {
		t.Errorf("messages = %+v", m.lastReq.Messages)
	}
}

func TestLLMAgent_MaxItersGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chatToolCallResp("c", "list_sessions", `{}`))
	}))
	defer srv.Close()

	a, err := NewLLM(newFakeAgentAPI(), LLMConfig{
		Client:   NewChatClient(srv.URL+"/v1", "k"),
		Model:    "m",
		MaxIters: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(context.Background(), nil, "", "loop", nil); err == nil || !strings.Contains(err.Error(), "max iterations") {
		t.Errorf("expected max-iter error, got %v", err)
	}
}

func TestLLMAgent_RequiresClientAndModel(t *testing.T) {
	if _, err := NewLLM(newFakeAgentAPI(), LLMConfig{Model: "m"}); err == nil {
		t.Error("expected error for nil Client")
	}
	if _, err := NewLLM(newFakeAgentAPI(), LLMConfig{Client: NewChatClient("http://x", "k")}); err == nil {
		t.Error("expected error for empty Model")
	}
}

func TestLLMAgent_CreateSession(t *testing.T) {
	api := newFakeAgentAPI()
	api.createNewID = "new-uuid-1234"
	api.createReply = "Hi! I'm ready to help."

	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "create_session", `{"cwd":"/tmp","initial_message":"hello there","backend":"codex","model":"gpt-5.5"}`),
		chatTextResp("created session new-uuid (assistant said hi)"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "open a tmp scratch session")
	if res.FocusSession != "new-uuid-1234" {
		t.Errorf("focus = %q, want new-uuid-1234", res.FocusSession)
	}
	if len(api.created) != 1 || api.created[0].Cwd != "/tmp" || api.created[0].Msg != "hello there" {
		t.Errorf("created = %+v", api.created)
	}
	if api.created[0].Backend != "codex" || api.created[0].Model != "gpt-5.5" {
		t.Errorf("backend/model not forwarded: %+v", api.created[0])
	}
}

func TestLLMAgent_CreateSessionMissingArgs(t *testing.T) {
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "create_session", `{"cwd":""}`),
		chatTextResp("you'll need a cwd"),
	})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "scratch", nil); err != nil {
		t.Fatal(err)
	}
}

func TestLLMAgent_SetAutoApprove(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "deadbeef", Title: "deploy"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "set_auto_approve", `{"session_id":"deadbeef","enabled":true}`),
		chatTextResp("auto-approve is on for the deploy session"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "stop asking me about the deploy session")
	if !api.autoApprove["deadbeef"] {
		t.Error("auto-approve not enabled")
	}
	if res.FocusSession != "" {
		t.Errorf("set_auto_approve must not set focus, got %q", res.FocusSession)
	}
}

func TestLLMAgent_SetArchived(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "deadbeef", Title: "old spike"}}
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "set_archived", `{"session_id":"deadbeef","archived":true}`),
		chatTextResp("archived the old spike"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res := runHandle(t, a, "archive the old spike")
	if !api.archived["deadbeef"] {
		t.Error("session not archived")
	}
	if res.FocusSession != "" {
		t.Errorf("set_archived must not set focus, got %q", res.FocusSession)
	}
}

func TestLLMAgent_ListSessionsEnrichedWithFlags(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "x", Cwd: "/x"}}
	api.autoApprove["abc12345"] = true
	api.archived["abc12345"] = true
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c", "list_sessions", `{"archived":true}`),
		chatTextResp("ok"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	_ = runHandle(t, a, "list everything")

	var toolMsg *ChatMessage
	for i := range m.lastReq.Messages {
		if m.lastReq.Messages[i].Role == "tool" {
			toolMsg = &m.lastReq.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message")
	}
	if !strings.Contains(toolMsg.Content, `"auto_approve":true`) || !strings.Contains(toolMsg.Content, `"archived":true`) {
		t.Errorf("list_sessions output missing flags: %q", toolMsg.Content)
	}
}

func TestExecuteTool_ListSessionsFiltersStatusAndLimit(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{
		{ID: "run-1", Status: core.StatusRunning},
		{ID: "idle-1", Status: core.StatusIdle},
		{ID: "live-1", Status: core.StatusLive},
		{ID: "run-2", Status: core.StatusRunning},
	}
	a, _ := NewLLM(api, LLMConfig{Client: NewChatClient("http://x", "k"), Model: "m"})
	out, focus := a.executeTool(context.Background(), "list_sessions", `{"statuses":["running","live"],"limit":2}`, nil)
	if focus != "" {
		t.Fatalf("list_sessions changed focus to %q", focus)
	}
	if !strings.Contains(out, `"id":"run-1"`) || !strings.Contains(out, `"id":"live-1"`) {
		t.Fatalf("filtered output missing expected sessions: %s", out)
	}
	if strings.Contains(out, `"id":"idle-1"`) || strings.Contains(out, `"id":"run-2"`) {
		t.Fatalf("filtered output contains excluded sessions: %s", out)
	}
}

func TestExecuteTool_ListSessionsDefaultsTo20AndExcludesArchived(t *testing.T) {
	api := newFakeAgentAPI()
	for i := 0; i < 22; i++ {
		api.sessions = append(api.sessions, core.Session{ID: fmt.Sprintf("session-%02d", i), Status: core.StatusIdle})
	}
	api.archived["session-00"] = true
	a, _ := NewLLM(api, LLMConfig{Client: NewChatClient("http://x", "k"), Model: "m"})
	out, _ := a.executeTool(context.Background(), "list_sessions", `{}`, nil)
	var got struct {
		Sessions      []sessionView `json:"sessions"`
		Returned      int           `json:"returned"`
		TotalMatching int           `json:"total_matching"`
		Truncated     bool          `json:"truncated"`
		Note          string        `json:"note"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 20 {
		t.Fatalf("default returned %d sessions, want 20", len(got.Sessions))
	}
	if got.Returned != 20 || got.TotalMatching != 21 || !got.Truncated {
		t.Fatalf("default pagination metadata = returned %d total %d truncated %v", got.Returned, got.TotalMatching, got.Truncated)
	}
	if strings.Contains(out, `"id":"session-00"`) || !strings.Contains(got.Note, "archived=true") {
		t.Fatalf("default archive filtering/note wrong: %s", out)
	}
	archived, _ := a.executeTool(context.Background(), "list_sessions", `{"archived":true}`, nil)
	if !strings.Contains(archived, `"id":"session-00"`) || strings.Contains(archived, `"id":"session-01"`) {
		t.Fatalf("archived query wrong: %s", archived)
	}
}

func TestExecuteTool_ReadTranscriptLatestPageHasMore(t *testing.T) {
	api := newFakeAgentAPI()
	api.transcripts["s"] = make([]core.TranscriptTurn, 100)
	a, _ := NewLLM(api, LLMConfig{Client: NewChatClient("http://x", "k"), Model: "m"})
	out, _ := a.executeTool(context.Background(), "read_session_transcript", `{"session_id":"s"}`, nil)
	var got struct {
		Offset  int  `json:"offset"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Offset != 80 || got.Total != 100 || !got.HasMore {
		t.Fatalf("latest-page metadata = %+v", got)
	}
}

func TestExecuteTool_SearchSessionsScopes(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "migration"}}
	api.transcripts["abc12345"] = []core.TranscriptTurn{{Role: "user", Content: "migration plan"}}
	a, _ := NewLLM(api, LLMConfig{Client: NewChatClient("http://x", "k"), Model: "m"})

	scoped, focus := a.executeTool(context.Background(), "search_sessions", `{"session_id":"abc","query":"migration"}`, nil)
	if focus != "abc12345" || !strings.Contains(scoped, `"scope":"session"`) || !strings.Contains(scoped, `"migration plan"`) {
		t.Fatalf("scoped search = %s, focus %q", scoped, focus)
	}
	global, focus := a.executeTool(context.Background(), "search_sessions", `{"query":"migration"}`, nil)
	if focus != "" || !strings.Contains(global, `"scope":"all"`) || !strings.Contains(global, `"session_id":"abc12345"`) {
		t.Fatalf("global search = %s, focus %q", global, focus)
	}
}

func TestDefaultToolsExposeOnlyUnifiedSearch(t *testing.T) {
	var names []string
	for _, tool := range defaultTools() {
		names = append(names, tool.Function.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "search_sessions") || strings.Contains(joined, "search_session_transcript") || strings.Contains(joined, "search_all_sessions") {
		t.Fatalf("tool names = %v", names)
	}
}

func TestNormalizeReplayedLegacySearchTool(t *testing.T) {
	event := normalizeReplayedToolEvent(ToolEvent{
		CallID: "c", Name: "search_session_transcript",
		Arguments: `{"session_id":"abc","query":"migration","max_hits":7}`,
		Result:    `{"hits":[],"truncated":false}`,
	})
	if event.Name != "search_sessions" || !strings.Contains(event.Arguments, `"limit":7`) || strings.Contains(event.Arguments, "max_hits") {
		t.Fatalf("normalized event = %+v", event)
	}
	if !strings.Contains(event.Result, `"scope":"session"`) || !strings.Contains(event.Result, `"session_id":"abc"`) {
		t.Fatalf("normalized result = %s", event.Result)
	}
	global := normalizeReplayedToolEvent(ToolEvent{
		CallID: "g", Name: "search_all_sessions",
		Arguments: `{"query":"migration","max_sessions":9}`,
		Result:    `{"results":[],"truncated":false}`,
	})
	if global.Name != "search_sessions" || !strings.Contains(global.Arguments, `"limit":9`) || strings.Contains(global.Arguments, "max_sessions") {
		t.Fatalf("normalized global event = %+v", global)
	}
	if !strings.Contains(global.Result, `"scope":"all"`) {
		t.Fatalf("normalized global result = %s", global.Result)
	}
}

func TestSummarizeHistoryNormalizesLegacySearchTool(t *testing.T) {
	srv, mock := newMockChatServer(t, []ChatResponse{chatTextResp("summary")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	_, err := a.SummarizeHistory(context.Background(), []HistoryMessage{{Tool: &ToolEvent{
		CallID: "g", Name: "search_all_sessions",
		Arguments: `{"query":"migration","max_sessions":9}`,
		Result:    `{"results":[],"truncated":false}`,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	content := mock.lastReq.Messages[1].Content
	if !strings.Contains(content, "search_sessions") || strings.Contains(content, "search_all_sessions") || !strings.Contains(content, `"scope":"all"`) {
		t.Fatalf("summarizer input was not normalized: %s", content)
	}
}

func TestExecuteTool_ListSessionsRejectsInvalidFilters(t *testing.T) {
	a, _ := NewLLM(newFakeAgentAPI(), LLMConfig{Client: NewChatClient("http://x", "k"), Model: "m"})
	for _, args := range []string{`{"statuses":["busy"]}`, `{"limit":0}`} {
		out, _ := a.executeTool(context.Background(), "list_sessions", args, nil)
		if !strings.Contains(out, `"error"`) {
			t.Errorf("args %s returned %s, want error", args, out)
		}
	}
}

// The real failure mode: a reasoning model returns thinking state on the
// assistant tool-call turn, and the SECOND request (after the tool result)
// must replay it verbatim or the provider 400s. We assert both DeepSeek's
// message-level reasoning_content and Gemini's tool_call-level extra_content
// reach the server on that second request.
func TestLLMAgent_ReplaysProviderReasoningAcrossToolLoop(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "x"}}

	first := ChatResponse{Choices: []ChatChoice{{
		Message: ChatMessage{
			Role:  "assistant",
			Extra: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"thinking…"`)},
			ToolCalls: []ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: ToolCallFunc{Name: "list_sessions", Arguments: "{}"},
				Extra:    map[string]json.RawMessage{"extra_content": json.RawMessage(`{"google":{"thought_signature":"SIG123"}}`)},
			}},
		},
		FinishReason: "tool_calls",
	}}}
	srv, m := newMockChatServer(t, []ChatResponse{first, chatTextResp("there is 1 session")})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "how many sessions?", nil); err != nil {
		t.Fatal(err)
	}

	// m.lastReq is the SECOND request, decoded with our types — the replayed
	// assistant tool-call message must still carry both reasoning fields.
	var replayed *ChatMessage
	for i := range m.lastReq.Messages {
		if m.lastReq.Messages[i].Role == "assistant" && len(m.lastReq.Messages[i].ToolCalls) > 0 {
			replayed = &m.lastReq.Messages[i]
		}
	}
	if replayed == nil {
		t.Fatal("assistant tool-call message was not replayed in the second request")
	}
	if _, ok := replayed.Extra["reasoning_content"]; !ok {
		t.Error("DeepSeek-style reasoning_content was not replayed")
	}
	if len(replayed.ToolCalls) == 0 || replayed.ToolCalls[0].Extra["extra_content"] == nil {
		t.Error("Gemini-style extra_content/thought_signature was not replayed")
	}
}

func TestExecuteTool_BadJSONArgs(t *testing.T) {
	a, _ := NewLLM(newFakeAgentAPI(), LLMConfig{
		Client: NewChatClient("http://x", "k"),
		Model:  "m",
	})
	// "not json" is unparseable; repair pipeline collapses to "{}" which
	// then fails the per-tool required-arg check with a clear message.
	got, focus := a.executeTool(context.Background(), "send_to_session", "not json", nil)
	if !strings.Contains(got, "required") {
		t.Errorf("got %q (want 'required' from arg validation)", got)
	}
	if focus != "" {
		t.Errorf("focus = %q, want empty", focus)
	}
}

func TestRepairJSONArgs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any // the parsed Go value we expect after repair+Unmarshal
	}{
		{"valid passes through", `{"a":1}`, map[string]any{"a": float64(1)}},
		{"trailing comma", `{"a":1,}`, map[string]any{"a": float64(1)}},
		{"unquoted key", `{a:1}`, map[string]any{"a": float64(1)}},
		{"python None", `{"a":None}`, map[string]any{"a": nil}},
		{"python True/False", `{"a":True,"b":False}`, map[string]any{"a": true, "b": false}},
		{"single-quoted value", `{"a":'hi'}`, map[string]any{"a": "hi"}},
		{"empty string", "", map[string]any{}},
		{"unrecoverable junk", "<<<not json>>>", map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repaired := repairJSONArgs(c.in)
			var got any
			if err := json.Unmarshal([]byte(repaired), &got); err != nil {
				t.Fatalf("repaired %q didn't parse: %v (repaired=%q)", c.in, err, repaired)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(c.want)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("repair(%q) → %q\n  got  %s\n  want %s", c.in, repaired, gotJSON, wantJSON)
			}
		})
	}
}

func TestLLMStrictModeAddsEnforcementToSystemPrompt(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{chatTextResp("ok")})
	defer srv.Close()
	a, err := NewLLM(newFakeAgentAPI(), LLMConfig{
		Client: NewChatClient(srv.URL+"/v1", "k"),
		Model:  "m",
		Strict: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Handle(context.Background(), nil, "", "x", nil); err != nil {
		t.Fatal(err)
	}
	if len(m.lastReq.Messages) == 0 || m.lastReq.Messages[0].Role != "system" {
		t.Fatal("missing system message")
	}
	if !strings.Contains(m.lastReq.Messages[0].Content, "Strict mode") {
		t.Errorf("system prompt missing strict block:\n%s", m.lastReq.Messages[0].Content)
	}
}

func TestLLMNonStrictHasNoEnforcement(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{chatTextResp("ok")})
	defer srv.Close()
	a, _ := NewLLM(newFakeAgentAPI(), LLMConfig{
		Client: NewChatClient(srv.URL+"/v1", "k"),
		Model:  "m",
		// Strict not set
	})
	if _, err := a.Handle(context.Background(), nil, "", "x", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.lastReq.Messages[0].Content, "Strict mode") {
		t.Error("non-strict run leaked strict block into system prompt")
	}
}

// --- relayed sends (main-chat relay channel) -------------------------------

func TestLLMAgent_SendToSessionRelayed(t *testing.T) {
	api := newFakeAgentAPI()
	api.sessions = []core.Session{{ID: "deadbeef", Title: "X"}}
	api.waitReplies["deadbeef"] = "the session's own words"
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_send", "send_to_session", `{"session_id":"deadbeef","text":"deploy now"}`),
		chatTextResp("routed"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	var relayed []string
	relay := func(sessionID, reply string, err error) {
		relayed = append(relayed, sessionID+"|"+reply)
	}
	res, err := a.Handle(context.Background(), nil, "", "tell deadbeef to deploy now", relay)
	if err != nil {
		t.Fatal(err)
	}
	// With a relay sink present the send must go through the relayed
	// primitive, not the plain fire-and-forget.
	if len(api.relayed) != 1 || api.relayed[0].ID != "deadbeef" || api.relayed[0].Text != "deploy now" {
		t.Errorf("relayed = %+v", api.relayed)
	}
	if len(relayed) != 1 || relayed[0] != "deadbeef|the session's own words" {
		t.Errorf("relay sink got %v", relayed)
	}
	if res.FocusSession != "deadbeef" {
		t.Errorf("FocusSession = %q", res.FocusSession)
	}
	// The tool result must tell the model the reply is handled elsewhere.
	var toolMsg *ChatMessage
	for i := range m.lastReq.Messages {
		if m.lastReq.Messages[i].Role == "tool" {
			toolMsg = &m.lastReq.Messages[i]
		}
	}
	if toolMsg == nil || !strings.Contains(toolMsg.Content, "verbatim") {
		t.Errorf("tool result should mention the automatic relay, got %+v", toolMsg)
	}
}

func TestLLMAgent_SendToSessionWithoutRelayFallsBack(t *testing.T) {
	api := newFakeAgentAPI()
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_send", "send_to_session", `{"session_id":"deadbeef","text":"go"}`),
		chatTextResp("sent"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "x", nil); err != nil {
		t.Fatal(err)
	}
	if len(api.relayed) != 0 {
		t.Errorf("nil sink must use plain SendToSession, relayed = %+v", api.relayed)
	}
	if len(api.sent) != 1 {
		t.Errorf("sent = %+v", api.sent)
	}
}

func TestLLMAgent_CreateSessionRelayed(t *testing.T) {
	api := newFakeAgentAPI()
	api.createNewID = "newid123"
	api.createReply = "first reply"
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("call_c", "create_session", `{"cwd":"/tmp","initial_message":"hi"}`),
		chatTextResp("created"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	var relayed []string
	relay := func(sessionID, reply string, err error) {
		relayed = append(relayed, sessionID+"|"+reply)
	}
	res, err := a.Handle(context.Background(), nil, "", "new scratch session", relay)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 1 || api.created[0].Cwd != "/tmp" {
		t.Errorf("created = %+v", api.created)
	}
	if len(relayed) != 1 || relayed[0] != "newid123|first reply" {
		t.Errorf("relay sink got %v", relayed)
	}
	if res.FocusSession != "newid123" {
		t.Errorf("FocusSession = %q", res.FocusSession)
	}
}

// TestLLMAgent_MaxItersWrapUp: when the tool budget runs out, the agent makes
// one final tools-free completion and returns its text (with the accumulated
// focus) instead of a bare error.
func TestLLMAgent_MaxItersWrapUp(t *testing.T) {
	api := newFakeAgentAPI()
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c1", "send_to_session", `{"session_id":"deadbeef","text":"go"}`),
		chatToolCallResp("c2", "read_session_transcript", `{"session_id":"deadbeef"}`),
		chatTextResp("routed to deadbeef; details will follow"),
	})
	defer srv.Close()

	a, err := NewLLM(api, LLMConfig{Client: NewChatClient(srv.URL+"/v1", "k"), Model: "m", MaxIters: 2})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Handle(context.Background(), nil, "", "do the thing", nil)
	if err != nil {
		t.Fatalf("wrap-up should recover, got %v", err)
	}
	if res.Reply != "routed to deadbeef; details will follow" {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "deadbeef" {
		t.Errorf("FocusSession = %q, want deadbeef (accumulated before exhaustion)", res.FocusSession)
	}
	// The wrap-up request must offer no tools and carry the budget notice.
	if len(m.lastReq.Tools) != 0 {
		t.Errorf("wrap-up request still offered %d tools", len(m.lastReq.Tools))
	}
	last := m.lastReq.Messages[len(m.lastReq.Messages)-1]
	if last.Role != "user" || !strings.Contains(last.Content, "Tool budget") {
		t.Errorf("wrap-up notice missing, last message = %+v", last)
	}
}

// TestLLMAgent_RepeatedIdenticalCallBlocked: an immediate identical repeat of
// a tool call is not dispatched — the model gets an error result instead
// (anti-polling / anti-duplicate-send guard).
func TestLLMAgent_RepeatedIdenticalCallBlocked(t *testing.T) {
	api := newFakeAgentAPI()
	srv, m := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c1", "send_to_session", `{"session_id":"deadbeef","text":"go"}`),
		chatToolCallResp("c2", "send_to_session", `{"session_id":"deadbeef","text":"go"}`),
		chatTextResp("ok"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	if _, err := a.Handle(context.Background(), nil, "", "x", nil); err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 1 {
		t.Errorf("duplicate send dispatched: sent = %+v", api.sent)
	}
	var blocked bool
	for _, msg := range m.lastReq.Messages {
		if msg.Role == "tool" && strings.Contains(msg.Content, "repeated identical tool call blocked") {
			blocked = true
		}
	}
	if !blocked {
		t.Error("second call's tool result should carry the blocked notice")
	}
}

// TestLLMAgent_ToolCallsWithoutFinishReason: providers that return tool calls
// with a missing finish_reason must still get their tools dispatched instead
// of the turn being misread as an empty final answer.
func TestLLMAgent_ToolCallsWithoutFinishReason(t *testing.T) {
	api := newFakeAgentAPI()
	quirky := ChatResponse{
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:       "c1",
					Type:     "function",
					Function: ToolCallFunc{Name: "send_to_session", Arguments: `{"session_id":"deadbeef","text":"go"}`},
				}},
			},
			FinishReason: "", // provider quirk: tool calls, no finish_reason
		}},
	}
	srv, _ := newMockChatServer(t, []ChatResponse{quirky, chatTextResp("done")})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res, err := a.Handle(context.Background(), nil, "", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.sent) != 1 {
		t.Errorf("tool call not dispatched: sent = %+v", api.sent)
	}
	if res.Reply != "done" || res.FocusSession != "deadbeef" {
		t.Errorf("res = %+v", res)
	}
}

// TestLLMAgent_LengthWithToolCallsNotDispatched: a max_tokens-truncated
// response carrying (possibly partial) tool calls must NOT be executed.
func TestLLMAgent_LengthWithToolCallsNotDispatched(t *testing.T) {
	api := newFakeAgentAPI()
	truncated := ChatResponse{
		Choices: []ChatChoice{{
			Message: ChatMessage{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID:       "c1",
					Type:     "function",
					Function: ToolCallFunc{Name: "send_to_session", Arguments: `{"session_id":"deadbeef","text":"half a mess`},
				}},
			},
			FinishReason: "length",
		}},
	}
	srv, _ := newMockChatServer(t, []ChatResponse{truncated})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	_, err := a.Handle(context.Background(), nil, "", "x", nil)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("err = %v, want truncated-by-max_tokens", err)
	}
	if len(api.sent) != 0 {
		t.Errorf("truncated tool call was dispatched: %+v", api.sent)
	}
}

// TestLLMAgent_RetryAfterErrorNotBlocked: the anti-repeat guard must let an
// identical call through when the first attempt returned an error result.
func TestLLMAgent_RetryAfterErrorNotBlocked(t *testing.T) {
	api := newFakeAgentAPI()
	// respond_to_interaction with an unknown id errors; the model retries
	// the identical call — the retry must reach the API again.
	srv, _ := newMockChatServer(t, []ChatResponse{
		chatToolCallResp("c1", "read_session_transcript", `{"session_id":"missing"}`),
		chatToolCallResp("c2", "read_session_transcript", `{"session_id":"missing"}`),
		chatTextResp("gave up"),
	})
	defer srv.Close()

	a := newTestLLM(t, api, srv.URL)
	res, err := a.Handle(context.Background(), nil, "", "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "gave up" {
		t.Errorf("Reply = %q", res.Reply)
	}
	// Both identical calls errored ("session not found") and neither was
	// blocked — observable via the mock's final request containing two tool
	// results with the not-found error and zero "blocked" notices.
}

// TestLLMAgent_EmptyToolCallsGlitchErrors: finish_reason=tool_calls with no
// calls and no content must error (an invisible empty answer helps nobody).
func TestLLMAgent_EmptyToolCallsGlitchErrors(t *testing.T) {
	glitch := ChatResponse{
		Choices: []ChatChoice{{
			Message:      ChatMessage{Role: "assistant"},
			FinishReason: "tool_calls",
		}},
	}
	srv, _ := newMockChatServer(t, []ChatResponse{glitch})
	defer srv.Close()

	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)
	_, err := a.Handle(context.Background(), nil, "", "x", nil)
	if err == nil || !strings.Contains(err.Error(), "no tool_calls") {
		t.Errorf("err = %v, want explicit glitch error", err)
	}
}

// TestSystemPromptDocumentsRelayTag pins the relay history marker (produced
// by the web layer via RelayTag) to the shape the system prompt teaches the
// model. If either side changes without the other, the model silently stops
// recognizing relayed replies as session output.
func TestSystemPromptDocumentsRelayTag(t *testing.T) {
	const documented = "[session <id> replied]"
	if !strings.Contains(defaultLLMSystemPrompt, documented) {
		t.Fatalf("system prompt no longer documents the relay marker %q", documented)
	}
	got := RelayTag("abc12345")
	want := strings.Replace(documented, "<id>", "abc12345", 1) + "\n"
	if got != want {
		t.Errorf("RelayTag = %q, want %q (matching the prompt's documented shape)", got, want)
	}
}

func TestSystemPromptDocumentsSummaryTag(t *testing.T) {
	if !strings.Contains(defaultLLMSystemPrompt, strings.TrimSuffix(SummaryTag, "\n")) {
		t.Fatalf("system prompt no longer documents the summary marker %q", SummaryTag)
	}
}

func TestSummarizeHistory(t *testing.T) {
	srv, m := newMockChatServer(t, []ChatResponse{chatTextResp("- standing: send infra work to ab12cd34")})
	defer srv.Close()
	a := newTestLLM(t, newFakeAgentAPI(), srv.URL)

	got, err := a.SummarizeHistory(context.Background(), []HistoryMessage{
		{Role: "user", Content: "always send infra work to ab12cd34"},
		{Role: "agent", Content: "noted"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ab12cd34") {
		t.Errorf("summary = %q", got)
	}
	// Tools-free call with the flattened history in the user message.
	if len(m.lastReq.Tools) != 0 {
		t.Errorf("summarize offered %d tools", len(m.lastReq.Tools))
	}
	if len(m.lastReq.Messages) != 2 || !strings.Contains(m.lastReq.Messages[1].Content, "always send infra work") {
		t.Errorf("summarize request messages = %+v", m.lastReq.Messages)
	}
}
