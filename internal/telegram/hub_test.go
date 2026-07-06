package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// fakeRouter implements RouterAPI with a hand-fed event channel and records
// the messages routed to sessions.
type fakeRouter struct {
	events   chan broker.Event
	pending  chan hook.Pending
	sessions map[string]core.Session

	mu        sync.Mutex
	sent      []routedMsg
	responded map[string]hook.Response
	respErr   error // returned by RespondInteraction when set
	sendErr   error // returned by SendToSession when set
}

type routedMsg struct {
	session, text string
}

func (f *fakeRouter) GetSession(id string) (core.Session, bool) {
	s, ok := f.sessions[id]
	return s, ok
}
func (f *fakeRouter) SubscribeAllSessions() (<-chan broker.Event, func()) {
	return f.events, func() {}
}
func (f *fakeRouter) SendToSession(id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, routedMsg{id, text})
	return nil
}
func (f *fakeRouter) routed() []routedMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]routedMsg(nil), f.sent...)
}
func (f *fakeRouter) SubscribePendingInteractions() (<-chan hook.Pending, func()) {
	if f.pending == nil {
		f.pending = make(chan hook.Pending)
	}
	return f.pending, func() {}
}
func (f *fakeRouter) RespondInteraction(id string, resp hook.Response) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.respErr != nil {
		return f.respErr
	}
	if f.responded == nil {
		f.responded = map[string]hook.Response{}
	}
	f.responded[id] = resp
	return nil
}

// recordingServer captures createForumTopic / sendMessage calls.
type recordingServer struct {
	mu          sync.Mutex
	createdFor  []string // topic names created
	sentThreads []int64  // thread ids messages were sent to
	sentTexts   []string
	sentSilent  []bool // disable_notification per sent message
	nextThread  int64
}

func (rs *recordingServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		rs.mu.Lock()
		defer rs.mu.Unlock()
		var result any
		switch method {
		case "getMe":
			result = User{ID: 1, IsBot: true, Username: "usherbot"}
		case "getUpdates":
			// Emulate a (very short) long poll returning nothing, so the
			// inbound loop doesn't hot-spin against the test server.
			rs.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			rs.mu.Lock()
			result = []Update{}
		case "createForumTopic":
			rs.nextThread++
			rs.createdFor = append(rs.createdFor, body["name"].(string))
			result = ForumTopic{MessageThreadID: rs.nextThread, Name: body["name"].(string)}
		case "sendMessage":
			rs.sentThreads = append(rs.sentThreads, int64(body["message_thread_id"].(float64)))
			rs.sentTexts = append(rs.sentTexts, body["text"].(string))
			silent, _ := body["disable_notification"].(bool)
			rs.sentSilent = append(rs.sentSilent, silent)
			result = Message{MessageID: 1}
		default:
			t.Errorf("unexpected method %q", method)
		}
		resultJSON, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
	}
}

func TestHubMirrorsAndLazyCreatesTopic(t *testing.T) {
	rs := &recordingServer{}
	srv := httptest.NewServer(rs.handler(t))
	defer srv.Close()

	fr := &fakeRouter{
		events:   make(chan broker.Event, 8),
		sessions: map[string]core.Session{"sessXYZ": {ID: "sessXYZ", Title: "fix the bug"}},
	}
	hub, err := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: -100, StatePath: ""}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = hub.Run(ctx) }()

	asst := func(text string) broker.Event {
		raw, _ := json.Marshal(map[string]any{"message": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}})
		return broker.Event{SessionID: "sessXYZ", Type: "assistant", Raw: raw}
	}

	fr.events <- asst("first reply")
	fr.events <- broker.Event{SessionID: "sessXYZ", Type: "user", Raw: json.RawMessage(`{}`)} // ignored
	fr.events <- asst("second reply")
	fr.events <- broker.Event{SessionID: "sessXYZ", Type: "subprocess.exit", Raw: json.RawMessage(`{}`)}

	waitFor(t, func() bool {
		rs.mu.Lock()
		defer rs.mu.Unlock()
		return len(rs.sentTexts) == 3
	})

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.createdFor) != 1 {
		t.Fatalf("topic should be created exactly once, got %d: %v", len(rs.createdFor), rs.createdFor)
	}
	if rs.createdFor[0] != "fix the bug" {
		t.Errorf("topic name = %q, want session title", rs.createdFor[0])
	}
	if rs.sentThreads[0] != 1 || rs.sentThreads[1] != 1 || rs.sentThreads[2] != 1 {
		t.Errorf("all messages should go to thread 1, got %v", rs.sentThreads)
	}
	if rs.sentTexts[0] != "first reply" || rs.sentTexts[1] != "second reply" {
		t.Errorf("texts = %v", rs.sentTexts)
	}
	// Streamed assistant text is silent; the turn-complete ping is audible.
	if !rs.sentSilent[0] || !rs.sentSilent[1] {
		t.Errorf("assistant mirrors should be silent, got %v", rs.sentSilent)
	}
	if rs.sentTexts[2] != "✅ responded" || rs.sentSilent[2] {
		t.Errorf("turn-complete ping should be audible '✅ responded', got text=%q silent=%v", rs.sentTexts[2], rs.sentSilent[2])
	}
}

func TestHubMirrorsShowImage(t *testing.T) {
	cwd := t.TempDir()
	imgPath := filepath.Join(cwd, "chart.png")
	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-image-bytes")
	if err := os.WriteFile(imgPath, imgBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	type photoRec struct {
		thread   int64
		filename string
		size     int
		silent   bool
	}
	var mu sync.Mutex
	var photos []photoRec

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]
		var result any
		switch method {
		case "getMe":
			result = User{ID: 1, IsBot: true, Username: "usherbot"}
		case "getUpdates":
			time.Sleep(20 * time.Millisecond)
			result = []Update{}
		case "createForumTopic":
			result = ForumTopic{MessageThreadID: 7, Name: "t"}
		case "sendPhoto":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse multipart: %v", err)
			}
			thread, _ := strconv.ParseInt(r.FormValue("message_thread_id"), 10, 64)
			silent := r.FormValue("disable_notification") == "true"
			f, hdr, err := r.FormFile("photo")
			if err != nil {
				t.Errorf("form file: %v", err)
			} else {
				b, _ := io.ReadAll(f)
				mu.Lock()
				photos = append(photos, photoRec{thread, hdr.Filename, len(b), silent})
				mu.Unlock()
			}
			result = Message{MessageID: 1}
		default:
			t.Errorf("unexpected method %q", method)
		}
		resultJSON, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
	}))
	defer srv.Close()

	fr := &fakeRouter{
		events:   make(chan broker.Event, 4),
		sessions: map[string]core.Session{"s1": {ID: "s1", Cwd: cwd}},
	}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: -100}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()

	raw, _ := json.Marshal(map[string]any{"message": map[string]any{
		"content": []map[string]any{{
			"type": "tool_use", "name": "mcp__usher__show_image",
			"input": map[string]any{"file_path": "chart.png"},
		}},
	}})
	fr.events <- broker.Event{SessionID: "s1", Type: "assistant", Raw: raw}

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(photos) == 1 })

	mu.Lock()
	defer mu.Unlock()
	p := photos[0]
	if p.thread != 7 || p.filename != "chart.png" || p.size != len(imgBytes) || !p.silent {
		t.Fatalf("photo = %+v, want thread 7, chart.png, %d bytes, silent", p, len(imgBytes))
	}
}

func TestMirrorImageFailureNotice(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "big.png"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var noticeText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch parts[len(parts)-1] {
		case "sendPhoto":
			// Reject the upload, as Telegram would for an oversized/odd file.
			_ = json.NewEncoder(w).Encode(apiResponse{OK: false, ErrorCode: 400, Description: "PHOTO_INVALID_DIMENSIONS"})
			return
		case "sendMessage":
			var body struct {
				Text string `json:"text"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			noticeText = body.Text
			mu.Unlock()
			resultJSON, _ := json.Marshal(Message{MessageID: 1})
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
			return
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`true`)})
	}))
	defer srv.Close()

	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{"s1": {ID: "s1", Cwd: cwd}}}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: -100}, nil)
	hub.mirrorImage(context.Background(), "s1", 5, "big.png")

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(noticeText, "couldn't send image") || !strings.Contains(noticeText, "big.png") {
		t.Fatalf("expected a failure notice mentioning the file, got %q", noticeText)
	}
}

func TestPermissionHTML(t *testing.T) {
	p := hook.Pending{ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"grep -r \"x<y\" ."}`)}
	got := permissionHTML(p)
	if !strings.Contains(got, "<b>Permission requested</b>") || !strings.Contains(got, "Bash") {
		t.Errorf("missing header/tool: %q", got)
	}
	// Command goes in a <pre> block, HTML-escaped.
	if !strings.Contains(got, "<pre>grep -r &#34;x&lt;y&#34; .</pre>") {
		t.Errorf("command should be escaped inside <pre>, got %q", got)
	}
}

func TestPromptEchoAndDedup(t *testing.T) {
	const group = int64(-100200)
	var mu sync.Mutex
	var texts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch parts[len(parts)-1] {
		case "getMe":
			resultJSON, _ := json.Marshal(User{ID: 1, IsBot: true, Username: "usherbot"})
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
			return
		case "getUpdates":
			time.Sleep(20 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`[]`)})
			return
		case "sendMessage":
			var body struct {
				Text string `json:"text"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			texts = append(texts, body.Text)
			mu.Unlock()
			resultJSON, _ := json.Marshal(Message{MessageID: 1})
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
			return
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`true`)})
	}))
	defer srv.Close()

	fr := &fakeRouter{events: make(chan broker.Event, 4), sessions: map[string]core.Session{"s1": {ID: "s1"}}}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group}, nil)
	_ = hub.store.put("s1", 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = hub.Run(ctx) }()

	userEv := func(text string) broker.Event {
		raw, _ := json.Marshal(map[string]any{"message": map[string]any{"content": text}})
		return broker.Event{SessionID: "s1", Type: "user", Raw: raw}
	}

	// Web-originated prompt (not recorded) → echoed with the ▶ prefix.
	fr.events <- userEv("from the web")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(texts) == 1 })
	mu.Lock()
	if texts[0] != "<blockquote expandable>from the web</blockquote>\n↑ mirrored user input" {
		t.Errorf("web prompt echo = %q, want blockquote-wrapped", texts[0])
	}
	mu.Unlock()

	// Telegram-originated prompt: record (as handleInbound does), then its user
	// event must be skipped (no duplicate).
	hub.recordSent("s1", "typed in telegram")
	fr.events <- userEv("typed in telegram")
	fr.events <- userEv("a later web prompt") // ordering marker: this one echoes
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(texts) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	for _, tx := range texts {
		if strings.Contains(tx, "typed in telegram") {
			t.Errorf("telegram-originated prompt should not be echoed, saw %q", tx)
		}
	}
	if texts[1] != "<blockquote expandable>a later web prompt</blockquote>\n↑ mirrored user input" {
		t.Errorf("second echo = %q, want blockquote-wrapped", texts[1])
	}
}

func TestPerSessionIsolation(t *testing.T) {
	// Session s1's topic (thread 1) blocks mid-send; s2's (thread 2) must still
	// get mirrored — proving one wedged topic doesn't stall the others.
	block := make(chan struct{})
	var mu sync.Mutex
	var s2Sent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if parts[len(parts)-1] == "sendMessage" {
			var body struct {
				Thread int64 `json:"message_thread_id"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			if body.Thread == 1 {
				<-block // wedge s1's worker
			} else if body.Thread == 2 {
				mu.Lock()
				s2Sent = true
				mu.Unlock()
			}
		}
		resultJSON, _ := json.Marshal(Message{MessageID: 1})
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
	}))
	defer srv.Close()

	fr := &fakeRouter{events: make(chan broker.Event, 4), sessions: map[string]core.Session{"s1": {ID: "s1"}, "s2": {ID: "s2"}}}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: -100}, nil)
	_ = hub.store.put("s1", 1)
	_ = hub.store.put("s2", 2)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = hub.Run(ctx) }()

	asst := func(sess, text string) broker.Event {
		raw, _ := json.Marshal(map[string]any{"message": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
		}})
		return broker.Event{SessionID: sess, Type: "assistant", Raw: raw}
	}
	fr.events <- asst("s1", "blocks here")
	fr.events <- asst("s2", "should still send")

	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return s2Sent })

	close(block) // release s1
	cancel()
}

func TestHandleInboundRoutingAndAuth(t *testing.T) {
	const group = int64(-100200)
	srv := okServer()
	defer srv.Close()
	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{}}
	hub, err := NewHub(NewClient("T", srv.URL), fr, Config{
		GroupID:        group,
		AllowedUserIDs: []int64{777},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Bind topic 5 to a session.
	if err := hub.store.put("sessZ", 5); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	msg := func(chat, user, thread int64, text string) *Message {
		return &Message{
			Chat:            Chat{ID: chat},
			From:            &User{ID: user},
			MessageThreadID: thread,
			Text:            text,
		}
	}

	// Accepted: right group, allowed user, bound topic.
	hub.handleInbound(ctx, msg(group, 777, 5, "do the thing"))
	// Rejected: wrong group.
	hub.handleInbound(ctx, msg(-999, 777, 5, "nope"))
	// Rejected: unauthorized user.
	hub.handleInbound(ctx, msg(group, 42, 5, "nope"))
	// Rejected: General topic (no thread).
	hub.handleInbound(ctx, msg(group, 777, 0, "nope"))
	// Rejected: topic not bound to any session.
	hub.handleInbound(ctx, msg(group, 777, 99, "nope"))
	// Rejected: blank text.
	hub.handleInbound(ctx, msg(group, 777, 5, "   "))

	got := fr.routed()
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 routed message, got %d: %+v", len(got), got)
	}
	if got[0].session != "sessZ" || got[0].text != "do the thing" {
		t.Fatalf("routed = %+v", got[0])
	}
}

func TestHandleInboundEmptyWhitelistAllowsAnyMember(t *testing.T) {
	const group = int64(-100200)
	srv := okServer()
	defer srv.Close()
	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{}}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group}, nil)
	_ = hub.store.put("sessZ", 5)

	hub.handleInbound(context.Background(), &Message{Chat: Chat{ID: group}, From: &User{ID: 12345}, MessageThreadID: 5, Text: "hi"})
	if got := fr.routed(); len(got) != 1 || got[0].session != "sessZ" {
		t.Fatalf("empty whitelist should allow any member, got %+v", got)
	}
}

func TestDecodeDecision(t *testing.T) {
	cases := []struct {
		data                string
		behavior, scope, id string
		ok                  bool
	}{
		{"a:abc123", "allow", "", "abc123", true},
		{"s:abc123", "allow", "session", "abc123", true},
		{"d:abc123", "deny", "", "abc123", true},
		{"i:abc123", "deny", "", "abc123", true}, // ignore = deny under the hood

		{"x:abc123", "", "", "", false},
		{"a:", "", "", "", false},
		{"noColon", "", "", "", false},
	}
	for _, c := range cases {
		b, s, id, ok := decodeDecision(c.data)
		if b != c.behavior || s != c.scope || id != c.id || ok != c.ok {
			t.Errorf("decodeDecision(%q) = %q,%q,%q,%v want %q,%q,%q,%v",
				c.data, b, s, id, ok, c.behavior, c.scope, c.id, c.ok)
		}
	}
	// callback_data stays within Telegram's 64-byte limit for a 32-char id.
	if got := len(encodeDecision("s", strings.Repeat("f", 32))); got > 64 {
		t.Errorf("callback_data too long: %d", got)
	}
}

func TestAskAnswerByText(t *testing.T) {
	const group = int64(-100200)
	srv := okServer()
	defer srv.Close()
	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{"s1": {ID: "s1"}}}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group}, nil)
	_ = hub.store.put("s1", 5)
	ctx := context.Background()

	// A free-form (no options) question registers the topic for a typed reply.
	hub.postAskQuestion(ctx, 5, hook.Pending{
		ID: "pX", SessionID: "s1", ToolName: "AskUserQuestion",
		ToolInput: json.RawMessage(`{"questions":[{"question":"Name?","options":[]}]}`),
	})

	// Typing in the topic answers the question (not routed as a new prompt).
	hub.handleInbound(ctx, &Message{MessageID: 1, Chat: Chat{ID: group}, From: &User{ID: 1}, MessageThreadID: 5, Text: "Alice"})

	fr.mu.Lock()
	resp, ok := fr.responded["pX"]
	routedN := len(fr.sent)
	fr.mu.Unlock()
	if !ok || resp.Behavior != "allow" || resp.Answers["Name?"] != "Alice" {
		t.Fatalf("typed answer not resolved: %+v ok=%v", resp, ok)
	}
	if routedN != 0 {
		t.Errorf("answer should be consumed, not routed to the session (got %d sends)", routedN)
	}

	// With no pending question now, a later message routes normally.
	hub.handleInbound(ctx, &Message{MessageID: 2, Chat: Chat{ID: group}, From: &User{ID: 1}, MessageThreadID: 5, Text: "next"})
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if len(fr.sent) != 1 || fr.sent[0].text != "next" {
		t.Errorf("after the question is answered, a message should route to the session, got %+v", fr.sent)
	}
}

func TestHandleCallbackResolvesInteraction(t *testing.T) {
	const group = int64(-100200)
	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{}}

	var editCalled, answerCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch parts[len(parts)-1] {
		case "answerCallbackQuery":
			answerCalled = true
		case "editMessageReplyMarkup":
			editCalled = true
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`true`)})
	}))
	defer srv.Close()

	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group, AllowedUserIDs: []int64{777}}, nil)
	ctx := context.Background()

	cb := &CallbackQuery{
		ID:      "q1",
		From:    User{ID: 777},
		Data:    encodeDecision("s", "pend42"),
		Message: &Message{MessageID: 9, Chat: Chat{ID: group}, MessageThreadID: 5},
	}
	hub.handleCallback(ctx, cb)

	fr.mu.Lock()
	resp, ok := fr.responded["pend42"]
	fr.mu.Unlock()
	if !ok || resp.Behavior != "allow" || resp.Scope != "session" {
		t.Fatalf("interaction not resolved correctly: %+v ok=%v", resp, ok)
	}
	if !answerCalled || !editCalled {
		t.Errorf("answerCallbackQuery=%v editMessageReplyMarkup=%v, want both true", answerCalled, editCalled)
	}

	// Unauthorized tapper must not resolve anything.
	fr.responded = nil
	hub.handleCallback(ctx, &CallbackQuery{
		ID: "q2", From: User{ID: 42}, Data: encodeDecision("a", "pendX"),
		Message: &Message{MessageID: 1, Chat: Chat{ID: group}},
	})
	fr.mu.Lock()
	n := len(fr.responded)
	fr.mu.Unlock()
	if n != 0 {
		t.Errorf("unauthorized tap should not resolve, got %d responses", n)
	}
}

func TestReconcileClosesDeletedTopics(t *testing.T) {
	const group = int64(-100200)

	var closed []int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if parts[len(parts)-1] == "closeForumTopic" {
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			mu.Lock()
			closed = append(closed, int64(body["message_thread_id"].(float64)))
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`true`)})
	}))
	defer srv.Close()

	fr := &fakeRouter{
		events: make(chan broker.Event),
		sessions: map[string]core.Session{
			"live": {ID: "live"},
			"old":  {ID: "old"}, // still exists on disk (e.g. archived) → keep
			// "gone" intentionally absent → GetSession false → deleted
		},
	}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group}, nil)
	_ = hub.store.put("live", 1)
	_ = hub.store.put("old", 2)
	_ = hub.store.put("gone", 3)

	hub.reconcile(context.Background())

	mu.Lock()
	defer mu.Unlock()
	// Only the deleted session's topic closes; existing (incl. archived) keep theirs.
	if len(closed) != 1 || closed[0] != 3 {
		t.Fatalf("closed topics = %v, want [3] (deleted only)", closed)
	}
	if _, ok := hub.store.thread("live"); !ok {
		t.Error("live session's topic should remain")
	}
	if _, ok := hub.store.thread("old"); !ok {
		t.Error("existing (archived) session's topic should remain")
	}
	if _, ok := hub.store.thread("gone"); ok {
		t.Error("deleted session's mapping should be dropped")
	}
}

func TestAskQuestionRoundTrip(t *testing.T) {
	const group = int64(-100200)
	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{}}

	var sentButtons [][]InlineKeyboardButton
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if parts[len(parts)-1] == "sendMessage" {
			var body struct {
				ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			sentButtons = body.ReplyMarkup.InlineKeyboard
			resultJSON, _ := json.Marshal(Message{MessageID: 1})
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
			return
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`true`)})
	}))
	defer srv.Close()

	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group, AllowedUserIDs: []int64{1}}, nil)
	_ = hub.store.put("s1", 5)
	ctx := context.Background()

	input := json.RawMessage(`{"questions":[{"header":"Pick","question":"Which DB?","options":[{"label":"Postgres"},{"label":"SQLite"}]}]}`)
	hub.postAskQuestion(ctx, 5, hook.Pending{ID: "pend1", SessionID: "s1", ToolName: "AskUserQuestion", ToolInput: input})

	if len(sentButtons) != 2 {
		t.Fatalf("want 2 option buttons, got %d", len(sentButtons))
	}
	// Tap the second option (SQLite, index 1).
	hub.handleAskCallback(ctx, &CallbackQuery{
		ID: "q", From: User{ID: 1},
		Data:    sentButtons[1][0].CallbackData,
		Message: &Message{MessageID: 1, Chat: Chat{ID: group}},
	})

	fr.mu.Lock()
	resp, ok := fr.responded["pend1"]
	fr.mu.Unlock()
	if !ok || resp.Behavior != "allow" || resp.Answers["Which DB?"] != "SQLite" {
		t.Fatalf("ask answer not resolved: %+v ok=%v", resp, ok)
	}
}

func TestAskQuestionMultiFallsBackToWeb(t *testing.T) {
	const group = int64(-100200)
	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{}}
	var hadButtons bool
	var text string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if parts[len(parts)-1] == "sendMessage" {
			var body struct {
				Text        string               `json:"text"`
				ReplyMarkup InlineKeyboardMarkup `json:"reply_markup"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			text = body.Text
			hadButtons = len(body.ReplyMarkup.InlineKeyboard) > 0
			resultJSON, _ := json.Marshal(Message{MessageID: 1})
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
			return
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: json.RawMessage(`true`)})
	}))
	defer srv.Close()
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group}, nil)

	// Two questions → can't be answered with one tap → web fallback.
	input := json.RawMessage(`{"questions":[{"question":"A?","options":[{"label":"x"}]},{"question":"B?","options":[{"label":"y"}]}]}`)
	hub.postAskQuestion(context.Background(), 5, hook.Pending{ID: "p", ToolName: "AskUserQuestion", ToolInput: input})

	if !strings.Contains(text, "web UI") {
		t.Errorf("multi-question should fall back to web UI note, got %q", text)
	}
	if _, leaked := hub.takeAsk("p"); leaked {
		t.Error("multi-question fallback must not store an ask entry")
	}
	_ = hadButtons // a Deny button is offered; presence not asserted strictly
}

// okServer returns a Bot API stub that answers every method with a generic
// ok:true (Message result), for tests that only exercise routing/gate logic.
func okServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resultJSON, _ := json.Marshal(Message{MessageID: 1})
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
	}))
}

func TestHandleInboundReactsOnSuccess(t *testing.T) {
	const group = int64(-100200)
	var mu sync.Mutex
	var reactedMsg int64
	var reactedEmoji string
	sendCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		switch parts[len(parts)-1] {
		case "setMessageReaction":
			reactedMsg = int64(body["message_id"].(float64))
			if arr, ok := body["reaction"].([]any); ok && len(arr) > 0 {
				reactedEmoji = arr[0].(map[string]any)["emoji"].(string)
			}
		case "sendMessage":
			sendCount++
		}
		mu.Unlock()
		resultJSON, _ := json.Marshal(Message{MessageID: 1})
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
	}))
	defer srv.Close()

	fr := &fakeRouter{events: make(chan broker.Event), sessions: map[string]core.Session{}}
	hub, _ := NewHub(NewClient("T", srv.URL), fr, Config{GroupID: group}, nil)
	_ = hub.store.put("sessZ", 5)

	// Success → reacts 👀 on the message, no ⚠️ sendMessage.
	hub.handleInbound(context.Background(), &Message{
		MessageID: 42, Chat: Chat{ID: group}, From: &User{ID: 1}, MessageThreadID: 5, Text: "go",
	})
	mu.Lock()
	if reactedMsg != 42 || reactedEmoji != ackReaction {
		t.Errorf("want reaction %q on msg 42, got %q on %d", ackReaction, reactedEmoji, reactedMsg)
	}
	if sendCount != 0 {
		t.Errorf("success path should not post a message, got %d", sendCount)
	}
	mu.Unlock()

	// Failure → no reaction, a ⚠️ notice instead.
	mu.Lock()
	reactedMsg, sendCount = 0, 0
	mu.Unlock()
	fr.sendErr = errors.New("send failed")
	hub.handleInbound(context.Background(), &Message{
		MessageID: 43, Chat: Chat{ID: group}, From: &User{ID: 1}, MessageThreadID: 5, Text: "go",
	})
	mu.Lock()
	defer mu.Unlock()
	if reactedMsg != 0 {
		t.Errorf("delivery failure should not react, got reaction on %d", reactedMsg)
	}
	if sendCount != 1 {
		t.Errorf("delivery failure should post one ⚠️ notice, got %d", sendCount)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestSendProseFallsBackToPlainOn400(t *testing.T) {
	var mu sync.Mutex
	var attempts []struct{ text, mode string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text      string `json:"text"`
			ParseMode string `json:"parse_mode"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		attempts = append(attempts, struct{ text, mode string }{body.Text, body.ParseMode})
		n := len(attempts)
		mu.Unlock()
		if n == 1 {
			_ = json.NewEncoder(w).Encode(apiResponse{OK: false, ErrorCode: 400, Description: "can't parse entities"})
			return
		}
		resultJSON, _ := json.Marshal(Message{MessageID: 1})
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
	}))
	defer srv.Close()

	hub, _ := NewHub(NewClient("T", srv.URL), &fakeRouter{}, Config{GroupID: -1}, nil)
	if err := hub.sendProse(context.Background(), 5, "say **hi**"); err != nil {
		t.Fatalf("sendProse: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attempts) != 2 {
		t.Fatalf("want 2 attempts (HTML then plain), got %d", len(attempts))
	}
	if attempts[0].mode != "HTML" || !strings.Contains(attempts[0].text, "<b>hi</b>") {
		t.Errorf("first attempt should be HTML, got %+v", attempts[0])
	}
	if attempts[1].mode != "" || attempts[1].text != "say **hi**" {
		t.Errorf("fallback should be plain raw text, got %+v", attempts[1])
	}
}
