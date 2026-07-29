package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nexustar/usher/internal/agent/usheragent"
	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/mainchat"
	"github.com/nexustar/usher/internal/router"
	"github.com/nexustar/usher/internal/sessionmeta"
)

// scriptedAgent is a minimal Agent for main-chat flow tests: it records the
// history each turn saw, optionally exercises the relay sink, and returns a
// fixed reply/focus. With echo set, the reply is "re: <first line of the
// user message>" so ordering across turns is observable.
type scriptedAgent struct {
	mu         sync.Mutex
	reply      string
	echo       bool
	focus      string
	delay      time.Duration
	useRelay   func(relay usheragent.RelaySink)
	toolEvents []usheragent.ToolEvent
	histories  [][]usheragent.HistoryMessage
}

func (f *scriptedAgent) Handle(_ context.Context, history []usheragent.HistoryMessage, _, userMsg string, relay usheragent.RelaySink) (usheragent.AgentResult, error) {
	f.mu.Lock()
	f.histories = append(f.histories, history)
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.useRelay != nil {
		f.useRelay(relay)
	}
	reply := f.reply
	if f.echo {
		text, _, _ := strings.Cut(userMsg, "\n\n<current_state>")
		reply = "re: " + text
	}
	return usheragent.AgentResult{Reply: reply, FocusSession: f.focus, ToolEvents: f.toolEvents}, nil
}

func newChatTestServer(t *testing.T, agent usheragent.Agent) *Server {
	t.Helper()
	dir := t.TempDir()
	store, err := mainchat.NewStore(filepath.Join(dir, "mainchats"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(slog.Default(), discovery.NewClaudeSource(filepath.Join(dir, "projects")))
	if err != nil {
		t.Fatal(err)
	}
	r := router.New(d, map[string]backend.Backend{}, "claude", broker.New(),
		hook.New(filepath.Join(dir, "auto.json")), sessionmeta.New(filepath.Join(dir, "sessions.json"), 0), nil)
	return NewServer("", "", "", nil, r, store, agent, nil, "", "", "", nil, nil, slog.Default())
}

func chatMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/mainchats/{id}/send", s.handleMainChatSend)
	mux.HandleFunc("GET /api/mainchats/{id}/events", s.handleMainChatEvents)
	return mux
}

func postChat(t *testing.T, base, chatID, text string) *http.Response {
	t.Helper()
	res, err := http.Post(base+"/api/mainchats/"+chatID+"/send", "application/json",
		strings.NewReader(`{"text":`+strconv.Quote(text)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

// waitForMessages polls the store until the chat holds at least n messages.
func waitForMessages(t *testing.T, s *Server, chatID string, n int) []mainchat.Message {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := s.main.Read(chatID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) >= n {
			return msgs
		}
		time.Sleep(5 * time.Millisecond)
	}
	msgs, _ := s.main.Read(chatID, 0)
	t.Fatalf("timed out waiting for %d messages, have %d: %+v", n, len(msgs), msgs)
	return nil
}

// TestMainChatSendAsyncRelayAndHistory drives the full detached-turn flow:
// POST returns 202 immediately, the user message + relayed session reply +
// agent reply all land in the store in order, and the NEXT turn's history
// carries the relay as a tagged user-role observation.
func TestMainChatSendAsyncRelayAndHistory(t *testing.T) {
	agent := &scriptedAgent{
		reply: "routed",
		focus: "sess-1",
		useRelay: func(relay usheragent.RelaySink) {
			relay("sess-1", "verbatim session reply", nil)
		},
	}
	s := newChatTestServer(t, agent)
	srv := httptest.NewServer(chatMux(s))
	defer srv.Close()

	if res := postChat(t, srv.URL, "chat1", "hi"); res.StatusCode != http.StatusAccepted {
		t.Fatalf("POST status = %d, want 202", res.StatusCode)
	}
	msgs := waitForMessages(t, s, "chat1", 3)
	if msgs[0].Role != "user" || msgs[0].Content != "hi" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	// The relay sink fired inside the turn, so the relay precedes the agent
	// reply; both orders reach the user either way.
	if msgs[1].Role != "relay" || msgs[1].Content != "verbatim session reply" || msgs[1].SourceSession != "sess-1" {
		t.Errorf("msg1 = %+v", msgs[1])
	}
	if msgs[2].Role != "agent" || !strings.Contains(msgs[2].Content, "routed") {
		t.Errorf("msg2 = %+v", msgs[2])
	}
	if msgs[2].FocusSession != "sess-1" {
		t.Errorf("agent FocusSession = %q", msgs[2].FocusSession)
	}

	// Second turn: its history must include the relayed reply as a tagged
	// user-role observation, not an agent message.
	agent.useRelay = nil
	postChat(t, srv.URL, "chat1", "again")
	waitForMessages(t, s, "chat1", 5)

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.histories) != 2 {
		t.Fatalf("agent ran %d turns, want 2", len(agent.histories))
	}
	var sawRelay bool
	for _, h := range agent.histories[1] {
		if strings.Contains(h.Content, "[session sess-1 replied]") && strings.Contains(h.Content, "verbatim session reply") {
			sawRelay = true
			if h.Role != "user" {
				t.Errorf("relay history role = %q, want user", h.Role)
			}
		}
	}
	if !sawRelay {
		t.Errorf("second turn's history missing relayed reply: %+v", agent.histories[1])
	}
}

func TestMainChatToolEventPrecedesFastRelay(t *testing.T) {
	agent := &scriptedAgent{
		reply:      "routed",
		focus:      "sess-1",
		toolEvents: []usheragent.ToolEvent{{CallID: "c1", Name: "send_to_session", Arguments: `{"session_id":"sess-1","text":"hi"}`, Result: `{"status":"sent"}`}},
		useRelay: func(relay usheragent.RelaySink) {
			relay("sess-1", "fast reply", nil)
		},
	}
	s := newChatTestServer(t, agent)
	user := mainchat.Message{Role: "user", Content: "send hi", Time: time.Now().UTC()}
	if err := s.main.Append("chat1", user); err != nil {
		t.Fatal(err)
	}
	s.runMainChatTurn("chat1", user)
	msgs, err := s.main.Read("chat1", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user", "tool", "relay", "agent"}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %+v", msgs)
	}
	for i, role := range want {
		if msgs[i].Role != role {
			t.Fatalf("role[%d] = %q, want %q; messages=%+v", i, msgs[i].Role, role, msgs)
		}
	}
}

// TestMainChatTurnSerialization proves the queue contract under rapid sends
// with a slow agent: user messages persist immediately in arrival order
// (durable at 202 time), turns then run one at a time in that same order,
// and the second turn's history already contains the first turn's reply.
func TestMainChatTurnSerialization(t *testing.T) {
	agent := &scriptedAgent{echo: true, delay: 60 * time.Millisecond}
	s := newChatTestServer(t, agent)
	srv := httptest.NewServer(chatMux(s))
	defer srv.Close()

	postChat(t, srv.URL, "chat1", "one")
	postChat(t, srv.URL, "chat1", "two")

	// Both user messages are persisted before either turn completes; the
	// agent replies then land in turn order.
	msgs := waitForMessages(t, s, "chat1", 4)
	wantRoles := []string{"user", "user", "agent", "agent"}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Fatalf("msg%d role = %q, want %q: %+v", i, msgs[i].Role, want, msgs)
		}
	}
	if msgs[0].Content != "one" || msgs[1].Content != "two" {
		t.Errorf("user order = %q, %q", msgs[0].Content, msgs[1].Content)
	}
	if msgs[2].Content != "re: one" || msgs[3].Content != "re: two" {
		t.Errorf("turns ran out of order: %q, %q", msgs[2].Content, msgs[3].Content)
	}

	// Turn 2's history: contains user "one" AND turn 1's reply (read fresh
	// after turn 1 finished), but NOT its own user message.
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.histories) != 2 {
		t.Fatalf("ran %d turns, want 2", len(agent.histories))
	}
	h2 := agent.histories[1]
	var sawOne, sawReplyOne, sawTwo bool
	for _, h := range h2 {
		switch h.Content {
		case "one":
			sawOne = true
		case "re: one":
			sawReplyOne = true
		case "two":
			sawTwo = true
		}
	}
	if !sawOne || !sawReplyOne {
		t.Errorf("turn 2 history missing prior turn: %+v", h2)
	}
	if sawTwo {
		t.Errorf("turn 2 history contains its own user message: %+v", h2)
	}
}

// TestMainChatEmptyAgentReplySkipped: a pure-passthrough turn (empty reply,
// no focus switch) must not append an empty agent bubble.
func TestMainChatEmptyAgentReplySkipped(t *testing.T) {
	agent := &scriptedAgent{reply: "", focus: ""}
	s := newChatTestServer(t, agent)
	srv := httptest.NewServer(chatMux(s))
	defer srv.Close()

	postChat(t, srv.URL, "chat1", "hi")
	waitForMessages(t, s, "chat1", 1)
	time.Sleep(100 * time.Millisecond) // give a would-be agent append time to land
	msgs, _ := s.main.Read("chat1", 0)
	if len(msgs) != 1 {
		t.Errorf("expected only the user message, got %+v", msgs)
	}
}

// TestMainChatEventsSSE: messages appended while a client is subscribed
// arrive as SSE "message" frames in order, and the turn closes with a
// "turn.done" frame (the client's placeholder-clearing signal).
func TestMainChatEventsSSE(t *testing.T) {
	agent := &scriptedAgent{reply: "hello back"}
	s := newChatTestServer(t, agent)
	srv := httptest.NewServer(chatMux(s))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/mainchats/chat1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// The subscription registers before the response headers flush, so a
	// received header block means we're subscribed; no polling needed.
	if res.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", res.StatusCode)
	}

	postChat(t, srv.URL, "chat1", "hi")

	type frame struct {
		name string
		ev   chatEvent
		err  error
	}
	frames := make(chan frame, 8)
	go func() {
		sc := bufio.NewScanner(res.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev chatEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				frames <- frame{err: err}
				return
			}
			frames <- frame{name: name, ev: ev}
		}
	}()

	var got []frame
	timeout := time.After(3 * time.Second)
	for len(got) < 3 {
		select {
		case f := <-frames:
			if f.err != nil {
				t.Fatal(f.err)
			}
			got = append(got, f)
		case <-timeout:
			t.Fatalf("timed out; frames so far: %+v", got)
		}
	}
	if got[0].name != "message" || got[0].ev.Message.Role != "user" || got[0].ev.Message.Content != "hi" {
		t.Errorf("frame0 = %+v", got[0])
	}
	if got[1].name != "message" || got[1].ev.Message.Role != "agent" || !strings.Contains(got[1].ev.Message.Content, "hello back") {
		t.Errorf("frame1 = %+v", got[1])
	}
	if got[2].name != "turn.done" {
		t.Errorf("frame2 = %+v, want turn.done", got[2])
	}
}

// TestBroadcastEvictsSlowSubscriber: a subscriber that stops reading is
// force-closed on buffer overflow (its EventSource would reconnect and
// refetch) instead of silently losing frames on a healthy-looking stream.
func TestBroadcastEvictsSlowSubscriber(t *testing.T) {
	s := newChatTestServer(t, &scriptedAgent{})
	ch, cancel := s.subscribeChat("chat1")
	defer cancel()

	// Fill the buffer (cap 16) and overflow it without reading.
	for i := 0; i < 20; i++ {
		s.broadcastChat("chat1", chatFrame{Event: "turn.done"})
	}

	// The subscriber must have been evicted: channel closed after its
	// buffered frames drain, and the registry no longer holds it.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				s.chatMu.Lock()
				n := len(s.chatSubs["chat1"])
				s.chatMu.Unlock()
				if n != 0 {
					t.Errorf("evicted subscriber still registered (%d)", n)
				}
				return
			}
		case <-deadline:
			t.Fatal("subscriber channel never closed after overflow")
		}
	}
}

func TestDeriveChatHistoryRelayForms(t *testing.T) {
	// Small relay: verbatim. Large relay (CJK to prove rune-safe cuts):
	// head+tail excerpt with a recovery pointer, decided at derivation and
	// stable forever.
	big := strings.Repeat("中", relayVerbatimMax+500)
	in := []mainchat.Message{
		{Role: "user", Content: "hi"},
		{Role: "agent", Content: "routed"},
		{Role: "relay", Content: "full reply", SourceSession: "0af0c1d2-3e4f-5678-9abc-def012345678"},
		{Role: "relay", Content: big, SourceSession: "0af0c1d2-3e4f-5678-9abc-def012345678"},
	}
	out := deriveChatHistory(in)
	if len(out) != 4 {
		t.Fatalf("len = %d", len(out))
	}
	if out[0].Role != "user" || out[1].Role != "agent" {
		t.Errorf("passthrough roles wrong: %+v", out[:2])
	}
	if out[2].Role != "user" ||
		!strings.HasPrefix(out[2].Content, "[session 0af0c1d2 replied]\n") ||
		!strings.HasSuffix(out[2].Content, "full reply") {
		t.Errorf("small relay must be verbatim: %q", out[2].Content)
	}
	ex := out[3].Content
	if !strings.Contains(ex, "chars omitted — read_session_transcript(0af0c1d2)") {
		t.Errorf("large relay missing pointer: %q", ex)
	}
	if !utf8.ValidString(ex) {
		t.Error("excerpt cut through a rune")
	}
	if got := utf8.RuneCountInString(ex); got > relayExcerptHead+relayExcerptTail+200 {
		t.Errorf("excerpt too large: %d runes", got)
	}
}

func TestDeriveChatHistoryToolEvent(t *testing.T) {
	in := []mainchat.Message{{Role: "tool", Tool: &mainchat.ToolEvent{
		CallID: "call_1", Name: "focus_session", Arguments: `{"session_id":"abc"}`, Result: `{"focused_session":"abc-full"}`,
	}}}
	out := deriveChatHistory(in)
	if len(out) != 1 || out[0].Tool == nil {
		t.Fatalf("derived tool history = %+v", out)
	}
	if out[0].Tool.Name != "focus_session" || out[0].Tool.Result != `{"focused_session":"abc-full"}` {
		t.Fatalf("derived event = %+v", out[0].Tool)
	}
}

func TestDeriveChatHistorySkipsMalformedToolEvent(t *testing.T) {
	out := deriveChatHistory([]mainchat.Message{
		{Role: "user", Content: "before"},
		{Role: "tool"},
		{Role: "agent", Content: "after"},
	})
	if len(out) != 2 || out[0].Content != "before" || out[1].Content != "after" {
		t.Fatalf("malformed tool record was not skipped: %+v", out)
	}
}

func TestDeriveChatHistorySummaryAnchoring(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	in := []mainchat.Message{
		{Role: "user", Content: "old question", Time: t0},
		{Role: "agent", Content: "old answer", Time: t0.Add(1 * time.Minute)},
		{Role: "user", Content: "recent tail msg", Time: t0.Add(2 * time.Minute)},
		// Compaction folded everything through t0+1m; the tail above was
		// kept verbatim and predates the summary in the append-only store.
		{Role: "summary", Content: "standing stuff", Time: t0.Add(3 * time.Minute), CoveredThrough: t0.Add(1 * time.Minute)},
		{Role: "user", Content: "new msg", Time: t0.Add(4 * time.Minute)},
	}
	out := deriveChatHistory(in)
	want := []string{"[summary of earlier conversation]\nstanding stuff", "recent tail msg", "new msg"}
	if len(out) != len(want) {
		t.Fatalf("derived %d messages: %+v", len(out), out)
	}
	for i, w := range want {
		if out[i].Content != w {
			t.Errorf("out[%d] = %q, want %q", i, out[i].Content, w)
		}
	}
	if out[0].Role != "user" {
		t.Errorf("summary must map to user role, got %q", out[0].Role)
	}
}

func TestDeriveChatHistoryHardCap(t *testing.T) {
	// No summary available (e.g. rule agent): the hard cap front-trims.
	var in []mainchat.Message
	for i := 0; i < 40; i++ {
		in = append(in, mainchat.Message{Role: "user", Content: strings.Repeat("x", 1024)})
	}
	out := deriveChatHistory(in)
	total := 0
	for _, h := range out {
		total += utf8.RuneCountInString(h.Content)
	}
	if total > historyHardCapRunes {
		t.Errorf("derived history %d runes exceeds hard cap %d", total, historyHardCapRunes)
	}
	if len(out) == 0 || out[len(out)-1].Content != in[len(in)-1].Content {
		t.Error("trim must drop from the FRONT, keeping the newest tail")
	}
}

func TestRelaySessionReplyErrorAndEmpty(t *testing.T) {
	s := newChatTestServer(t, &scriptedAgent{})
	s.relaySessionReply("chat1", "sess-9", "", errors.New("turn exploded"))
	msgs, err := s.main.Read("chat1", 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("msgs = %+v, err = %v", msgs, err)
	}
	m := msgs[0]
	if m.Role != "relay" || m.SourceSession != "sess-9" {
		t.Errorf("msg = %+v", m)
	}
	if !strings.Contains(m.Content, "(no text response)") || !strings.Contains(m.Content, "turn exploded") {
		t.Errorf("content = %q", m.Content)
	}
}

// TestQueueFullLeavesNoGhostMessage: a send rejected with 429 must leave
// nothing in the store — the invariant is store-user-count == accepted-count,
// no persisted message without a worker turn to process it.
func TestQueueFullLeavesNoGhostMessage(t *testing.T) {
	agent := &scriptedAgent{reply: "ok", delay: 300 * time.Millisecond}
	s := newChatTestServer(t, agent)
	srv := httptest.NewServer(chatMux(s))
	defer srv.Close()

	accepted, rejected := 0, 0
	for i := 0; i < maxQueuedChatTurns+4; i++ {
		res := postChat(t, srv.URL, "chat1", "m"+strconv.Itoa(i))
		switch res.StatusCode {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			rejected++
		default:
			t.Fatalf("unexpected status %d", res.StatusCode)
		}
	}
	if rejected == 0 {
		t.Fatal("burst never hit the queue cap")
	}
	users := 0
	msgs, err := s.main.Read("chat1", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Role == "user" {
			users++
		}
	}
	if users != accepted {
		t.Errorf("store holds %d user messages, %d were accepted — ghost messages", users, accepted)
	}
}

// TestRelayPreservesWhitespace: relayed replies are stored verbatim; leading/
// trailing whitespace (code fences, terminal output) survives.
func TestRelayPreservesWhitespace(t *testing.T) {
	s := newChatTestServer(t, &scriptedAgent{})
	raw := "\n```sh\n$ make test\n```\n\n"
	s.relaySessionReply("chat1", "sess-1", raw, nil)
	msgs, err := s.main.Read("chat1", 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("msgs = %+v, err = %v", msgs, err)
	}
	if msgs[0].Content != raw {
		t.Errorf("relay content altered: %q", msgs[0].Content)
	}
}

// summarizingAgent adds the compaction hook to scriptedAgent.
type summarizingAgent struct {
	scriptedAgent
	summarized [][]usheragent.HistoryMessage
}

func (f *summarizingAgent) SummarizeHistory(_ context.Context, history []usheragent.HistoryMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.summarized = append(f.summarized, history)
	return "standing summary (" + strconv.Itoa(len(history)) + " messages folded)", nil
}

// TestChatCompaction: once the derived history exceeds its budget, the turn
// worker folds the older portion into an appended summary message, and the
// next derivation anchors on it — under budget, summary first, recent tail
// kept.
func TestChatCompaction(t *testing.T) {
	agent := &summarizingAgent{scriptedAgent: scriptedAgent{reply: "ok"}}
	s := newChatTestServer(t, agent)

	// Seed past the budget: 20 relays × 1000 runes ≈ 20k derived > 16k.
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 20; i++ {
		if err := s.main.Append("chat1", mainchat.Message{
			Role: "relay", SourceSession: "sess-1",
			Content: strings.Repeat("x", 1000),
			Time:    base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(chatMux(s))
	defer srv.Close()
	postChat(t, srv.URL, "chat1", "hi")

	var summary *mainchat.Message
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && summary == nil {
		msgs, err := s.main.Read("chat1", 0)
		if err != nil {
			t.Fatal(err)
		}
		for i := range msgs {
			if msgs[i].Role == "summary" {
				summary = &msgs[i]
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if summary == nil {
		t.Fatal("no summary message appended")
	}
	if summary.CoveredThrough.IsZero() {
		t.Error("summary missing CoveredThrough")
	}
	agent.mu.Lock()
	if len(agent.summarized) == 0 || len(agent.summarized[0]) < 2 {
		t.Errorf("summarizer input = %+v", agent.summarized)
	}
	agent.mu.Unlock()

	// The next derivation must anchor on the summary and fit the budget.
	msgs, _ := s.main.Read("chat1", 0)
	hist := deriveChatHistory(msgs)
	if !strings.HasPrefix(hist[0].Content, usheragent.SummaryTag) {
		t.Errorf("derived history must start with the summary, got %q", hist[0].Content[:60])
	}
	total := 0
	for _, h := range hist {
		total += utf8.RuneCountInString(h.Content)
	}
	if total > historyBudgetRunes {
		t.Errorf("post-compaction derived history still %d runes (> %d)", total, historyBudgetRunes)
	}
	// The kept tail must still be present after the summary.
	var tailKept bool
	for _, h := range hist[1:] {
		if strings.Contains(h.Content, "xxxx") {
			tailKept = true
		}
	}
	if !tailKept {
		t.Error("recent tail vanished after compaction")
	}
}

// TestRelayForeignTurnFollowsReferencingChats: a foreign turn is relayed
// only to chats whose recent history references the session.
func TestRelayForeignTurnFollowsReferencingChats(t *testing.T) {
	s := newChatTestServer(t, &scriptedAgent{})
	if err := s.main.Append("chat1", mainchat.Message{Role: "agent", Content: "routed", FocusSession: "sess-X"}); err != nil {
		t.Fatal(err)
	}
	if err := s.main.Append("chat2", mainchat.Message{Role: "user", Content: "unrelated"}); err != nil {
		t.Fatal(err)
	}

	s.RelayForeignTurn("sess-X", "late background report")

	msgs1, _ := s.main.Read("chat1", 0)
	last := msgs1[len(msgs1)-1]
	if last.Role != "relay" || last.Content != "late background report" || last.SourceSession != "sess-X" {
		t.Errorf("chat1 last = %+v", last)
	}
	msgs2, _ := s.main.Read("chat2", 0)
	for _, m := range msgs2 {
		if m.Role == "relay" {
			t.Errorf("chat2 must not receive the relay: %+v", m)
		}
	}
}

// TestStateBlockLegendAndLastEvent: the ground-truth block carries the
// status legend and the last_event column header the prompt relies on.
func TestStateBlockLegendAndLastEvent(t *testing.T) {
	s := newChatTestServer(t, &scriptedAgent{})
	block := s.renderStateBlock("")
	if !strings.Contains(block, "status legend:") || !strings.Contains(block, "background work") {
		t.Errorf("missing status legend: %q", block)
	}
	if !strings.Contains(block, "last_event") {
		t.Errorf("missing last_event column: %q", block)
	}
}

func TestStateBlockSessionsIncludesFocusActiveAndRecentIdle(t *testing.T) {
	var sessions []core.Session
	for i := 0; i < 12; i++ {
		sessions = append(sessions, core.Session{ID: fmt.Sprintf("idle-%02d", i), Status: core.StatusIdle})
	}
	sessions = append(sessions,
		core.Session{ID: "running", Status: core.StatusRunning},
		core.Session{ID: "live", Status: core.StatusLive},
		core.Session{ID: "waiting", Status: core.StatusAwaitingPermission},
	)
	rows := stateBlockSessions(sessions, "idle-11")
	got := make(map[string]bool, len(rows))
	for _, row := range rows {
		got[row.ID] = true
	}
	for _, id := range []string{"idle-00", "idle-09", "idle-11", "running", "live", "waiting"} {
		if !got[id] {
			t.Errorf("missing %s from selected rows: %+v", id, rows)
		}
	}
	if got["idle-10"] {
		t.Errorf("included idle session outside recent window: %+v", rows)
	}
}
