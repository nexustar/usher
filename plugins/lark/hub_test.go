package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
	"github.com/nexustar/usher/internal/imutil"
	"github.com/nexustar/usher/internal/pluginapi"

	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// fakeLark records outbound Lark traffic.
type sentMsg struct {
	kind string // root | text | card | image
	to   string // chat id (root) or root message id (replies)
	body string // text / card json / image key
}

type fakeLark struct {
	mu        sync.Mutex
	sent      []sentMsg
	reacted   []string
	nextRoot  int
	failSend  bool
	failCards bool
	failPosts bool
}

func (f *fakeLark) SendCard(_ context.Context, chatID, cardJSON string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSend {
		return "", errors.New("boom")
	}
	f.nextRoot++
	f.sent = append(f.sent, sentMsg{kind: "root", to: chatID, body: cardJSON})
	return "om_root_" + itoa(f.nextRoot), nil
}

func (f *fakeLark) UpdateCard(_ context.Context, messageID, cardJSON string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{kind: "update", to: messageID, body: cardJSON})
	return nil
}

func (f *fakeLark) ReplyText(_ context.Context, rootID, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{kind: "text", to: rootID, body: text})
	return "omt_" + rootID, nil
}

func (f *fakeLark) ReplyCard(_ context.Context, rootID, cardJSON string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCards {
		return "", errors.New("boom")
	}
	f.sent = append(f.sent, sentMsg{kind: "card", to: rootID, body: cardJSON})
	return "omt_" + rootID, nil
}

func (f *fakeLark) ReplyPost(_ context.Context, rootID, postJSON string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPosts {
		return "", errors.New("boom")
	}
	f.sent = append(f.sent, sentMsg{kind: "post", to: rootID, body: postJSON})
	return "omt_" + rootID, nil
}

func (f *fakeLark) UploadImage(_ context.Context, data []byte) (string, error) {
	return "img_key_1", nil
}

func (f *fakeLark) ReplyImage(_ context.Context, rootID, imageKey string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{kind: "image", to: rootID, body: imageKey})
	return "omt_" + rootID, nil
}

func (f *fakeLark) React(_ context.Context, messageID, emojiType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reacted = append(f.reacted, messageID)
	return nil
}

func (f *fakeLark) messages() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentMsg(nil), f.sent...)
}

func itoa(i int) string { data, _ := json.Marshal(i); return string(data) }

// fakeRouter implements RouterAPI.
type fakeRouter struct {
	broker *broker.Broker

	mu         sync.Mutex
	sessions   map[string]core.Session
	sent       map[string][]string
	pendingCh  chan hook.Pending
	responses  map[string]hook.Response
	respondErr error // forced RespondInteraction failure (transport-style)
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

func (f *fakeRouter) SubscribePendingInteractions() (<-chan hook.Pending, func()) {
	return f.pendingCh, func() {}
}

func (f *fakeRouter) RespondInteraction(id string, resp hook.Response) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.respondErr != nil {
		return f.respondErr
	}
	if _, done := f.responses[id]; done {
		// The real hub sees server rejections as pluginapi.APIError.
		return &pluginapi.APIError{Status: 409, Msg: "already resolved"}
	}
	f.responses[id] = resp
	return nil
}

func (f *fakeRouter) response(id string) (hook.Response, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.responses[id]
	return r, ok
}

const (
	testChat = "oc_test_chat"
	testUser = "ou_alice"
)

func newTestHub(t *testing.T, f *fakeLark, r *fakeRouter, allowed ...string) *Hub {
	t.Helper()
	h, err := NewHub(f, r, Config{ChatID: testChat, AllowedUserIDs: allowed}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.spawn = func(f func()) { f() } // synchronous routing keeps tests deterministic
	return h
}

func runHub(t *testing.T, h *Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var inboundSeq atomic.Int64

func inboundMessage(chat, sender, thread, root, text string) *larkim.P2MessageReceiveV1 {
	content, _ := json.Marshal(map[string]string{"text": text})
	contentStr := string(content)
	msgID := "om_inbound_" + strconv.FormatInt(inboundSeq.Add(1), 10)
	msgType := "text"
	senderType := "user"
	ev := &larkim.P2MessageReceiveV1{Event: &larkim.P2MessageReceiveV1Data{
		Sender: &larkim.EventSender{
			SenderType: &senderType,
			SenderId:   &larkim.UserId{OpenId: &sender},
		},
		Message: &larkim.EventMessage{
			MessageId:   &msgID,
			ChatId:      &chat,
			MessageType: &msgType,
			Content:     &contentStr,
		},
	}}
	if thread != "" {
		ev.Event.Message.ThreadId = &thread
	}
	if root != "" {
		ev.Event.Message.RootId = &root
	}
	return ev
}

func TestMirrorsAssistantAndLazilyCreatesThread(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1", Title: "fix the bug", Cwd: "/w"}
	h := newTestHub(t, f, r)
	runHub(t, h)

	raw := json.RawMessage(`{"message":{"content":[{"type":"text","text":"**hello** from <claude>"}]}}`)
	waitFor(t, "assistant mirror", func() bool {
		r.broker.Publish(broker.Event{SessionID: "s1", Type: "assistant", Raw: raw})
		msgs := f.messages()
		return len(msgs) >= 2 && msgs[0].kind == "root" && msgs[1].kind == "post"
	})

	msgs := f.messages()
	if msgs[0].to != testChat || !strings.Contains(msgs[0].body, "fix the bug") {
		t.Fatalf("root message = %+v", msgs[0])
	}
	if !strings.Contains(msgs[0].body, "/w") {
		t.Errorf("root message should carry the cwd: %q", msgs[0].body)
	}
	// Assistant text rides a post md paragraph: formatting passes through,
	// tag markup is escaped, and the bubble form means no card frame.
	var mirrored struct {
		ZhCN struct {
			Content [][]struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"zh_cn"`
	}
	if err := json.Unmarshal([]byte(msgs[1].body), &mirrored); err != nil {
		t.Fatal(err)
	}
	if msgs[1].to != "om_root_1" || mirrored.ZhCN.Content[0][0].Tag != "md" {
		t.Fatalf("mirrored post = %+v", msgs[1])
	}
	if got := mirrored.ZhCN.Content[0][0].Text; got != "**hello** from &#60;claude>" {
		t.Fatalf("post md content = %q", got)
	}
	// The reply's thread id is recorded for inbound routing.
	if id, ok := h.store.session("omt_om_root_1", ""); !ok || id != "s1" {
		t.Fatalf("thread mapping not recorded, got %q %v", id, ok)
	}
}

func TestTurnCompleteOnlyWithExistingThread(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r)
	runHub(t, h)

	// No thread yet → exit event mirrors nothing.
	r.broker.Publish(broker.Event{SessionID: "s1", Type: "subprocess.exit"})
	time.Sleep(50 * time.Millisecond)
	if len(f.messages()) != 0 {
		t.Fatalf("no thread: want no messages, got %v", f.messages())
	}

	raw := json.RawMessage(`{"message":{"content":[{"type":"text","text":"work"}]}}`)
	waitFor(t, "mirror", func() bool {
		r.broker.Publish(broker.Event{SessionID: "s1", Type: "assistant", Raw: raw})
		return len(f.messages()) >= 2
	})
	exit := json.RawMessage(`{"user_ts":"2026-06-25T03:00:00Z","assistant_ts":"2026-06-25T03:00:08Z"}`)
	r.broker.Publish(broker.Event{SessionID: "s1", Type: "subprocess.exit", Raw: exit})
	waitFor(t, "turn ping", func() bool {
		msgs := f.messages()
		last := msgs[len(msgs)-1]
		return last.kind == "text" && strings.Contains(last.body, "✅ responded in 8s")
	})
}

func TestInboundRoutingAndAuth(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r, testUser)
	if err := h.store.put("s1", "om_root_1"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.setThread("s1", "omt_1"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	// Authorized user in the bound thread → routed + acked.
	h.HandleMessage(ctx, inboundMessage(testChat, testUser, "omt_1", "", "run the tests"))
	if got := r.sent["s1"]; len(got) != 1 || got[0] != "run the tests" {
		t.Fatalf("routed = %v", got)
	}
	if len(f.reacted) != 1 {
		t.Fatalf("want ack reaction, got %v", f.reacted)
	}

	// Root-id fallback when the event carries no thread id.
	h.HandleMessage(ctx, inboundMessage(testChat, testUser, "", "om_root_1", "again"))
	if got := r.sent["s1"]; len(got) != 2 || got[1] != "again" {
		t.Fatalf("root-routed = %v", got)
	}

	// Wrong chat / unknown thread / other user → ignored.
	h.HandleMessage(ctx, inboundMessage("oc_other", testUser, "omt_1", "", "nope"))
	h.HandleMessage(ctx, inboundMessage(testChat, testUser, "omt_unknown", "", "nope"))
	h.HandleMessage(ctx, inboundMessage(testChat, "ou_mallory", "omt_1", "", "nope"))
	if got := r.sent["s1"]; len(got) != 2 {
		t.Fatalf("unauthorized messages leaked through: %v", got)
	}

	// The routed prompt's own echo is deduped; a different prompt echoes.
	raw, _ := json.Marshal(map[string]any{"message": map[string]any{"content": "again"}})
	h.mirrorPrompt(ctx, broker.Event{SessionID: "s1", Type: "user", Raw: raw})
	for _, m := range f.messages() {
		if strings.Contains(m.body, promptCaption) {
			t.Fatalf("own prompt should not echo back: %+v", m)
		}
	}
	rawWeb, _ := json.Marshal(map[string]any{"message": map[string]any{"content": "from the web"}})
	h.mirrorPrompt(ctx, broker.Event{SessionID: "s1", Type: "user", Raw: rawWeb})
	msgs := f.messages()
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.body, "from the web") || !strings.Contains(last.body, promptCaption) {
		t.Fatalf("web prompt should echo, got %+v", last)
	}
}

func TestPermissionCardRoundTrip(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r, testUser)
	runHub(t, h)

	p := hook.Pending{ID: "p1", SessionID: "s1", ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"rm -rf build"}`)}
	r.pendingCh <- p
	waitFor(t, "permission card", func() bool {
		msgs := f.messages()
		return len(msgs) >= 2 && msgs[len(msgs)-1].kind == "card"
	})
	card := f.messages()[len(f.messages())-1].body
	if !strings.Contains(card, "rm -rf build") || !strings.Contains(card, `"k":"a"`) {
		t.Fatalf("card json missing pieces: %s", card)
	}
	// Replay of the same pending (snapshot after reconnect) is deduped.
	r.pendingCh <- p
	time.Sleep(50 * time.Millisecond)
	if n := len(f.messages()); n != 2 {
		t.Fatalf("replayed pending reposted: %d messages", n)
	}

	// Tap "allow for session".
	resp := h.HandleCardAction(context.Background(), cardTap(testChat, testUser, obj{"k": "s", "id": "p1"}))
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "allowed for session") {
		t.Fatalf("toast = %+v", resp.Toast)
	}
	if got, ok := r.response("p1"); !ok || got.Behavior != "allow" || got.Scope != "session" {
		t.Fatalf("response = %+v %v", got, ok)
	}

	// Second tap → already resolved.
	resp = h.HandleCardAction(context.Background(), cardTap(testChat, testUser, obj{"k": "a", "id": "p1"}))
	if resp.Toast == nil || resp.Toast.Content != "already resolved" {
		t.Fatalf("second tap toast = %+v", resp.Toast)
	}

	// Unauthorized tapper.
	resp = h.HandleCardAction(context.Background(), cardTap(testChat, "ou_mallory", obj{"k": "a", "id": "p1"}))
	if resp.Toast == nil || resp.Toast.Content != "not authorized" {
		t.Fatalf("unauthorized toast = %+v", resp.Toast)
	}
}

func cardTap(chat, operator string, value obj) *callback.CardActionTriggerEvent {
	return &callback.CardActionTriggerEvent{Event: &callback.CardActionTriggerRequest{
		Operator: &callback.Operator{OpenID: operator},
		Context:  &callback.Context{OpenChatID: chat},
		Action:   &callback.CallBackAction{Value: value},
	}}
}

func TestAskQuestionTapAndTypedAnswer(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r, testUser)
	runHub(t, h)

	ask := func(id string) hook.Pending {
		return hook.Pending{ID: id, SessionID: "s1", ToolName: "AskUserQuestion",
			ToolInput: json.RawMessage(`{"questions":[{"question":"Deploy now?","header":"Deploy","options":[{"label":"Yes"},{"label":"No"}]}]}`)}
	}

	// Tap an option.
	r.pendingCh <- ask("q1")
	waitFor(t, "ask card", func() bool {
		msgs := f.messages()
		return len(msgs) >= 2 && msgs[len(msgs)-1].kind == "card"
	})
	card := f.messages()[len(f.messages())-1].body
	if !strings.Contains(card, "Deploy now?") || !strings.Contains(card, `"k":"q"`) {
		t.Fatalf("ask card json: %s", card)
	}
	resp := h.HandleCardAction(context.Background(), cardTap(testChat, testUser, obj{"k": "q", "id": "q1", "opt": "1"}))
	if resp.Toast == nil || !strings.Contains(resp.Toast.Content, "No") {
		t.Fatalf("toast = %+v", resp.Toast)
	}
	if got, _ := r.response("q1"); got.Answers["Deploy now?"] != "No" {
		t.Fatalf("answer = %+v", got.Answers)
	}

	// Typed reply answers the next question instead of becoming a prompt.
	r.pendingCh <- ask("q2")
	waitFor(t, "second ask card", func() bool {
		return len(f.messages()) >= 3
	})
	h.HandleMessage(context.Background(), inboundMessage(testChat, testUser, "omt_om_root_1", "", "ship it tomorrow"))
	if got, _ := r.response("q2"); got.Answers["Deploy now?"] != "ship it tomorrow" {
		t.Fatalf("typed answer = %+v", got.Answers)
	}
	if len(r.sent["s1"]) != 0 {
		t.Fatalf("typed answer must not become a prompt: %v", r.sent["s1"])
	}
}

func TestMultiQuestionFallsBackToWeb(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r)
	runHub(t, h)

	r.pendingCh <- hook.Pending{ID: "m1", SessionID: "s1", ToolName: "AskUserQuestion",
		ToolInput: json.RawMessage(`{"questions":[{"question":"a?"},{"question":"b?"}]}`)}
	waitFor(t, "multi-step card", func() bool {
		msgs := f.messages()
		return len(msgs) >= 2 && strings.Contains(msgs[len(msgs)-1].body, "Multi-step question")
	})
	// No ask entry registered: a typed reply is a normal prompt.
	if _, _, ok := h.takeAskBySession("s1"); ok {
		t.Fatal("multi-question prompt must not register a typed-reply entry")
	}
}

func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "threads.json")
	s, err := newThreadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.put("s1", "om_1"); err != nil {
		t.Fatal(err)
	}
	if err := s.setThread("s1", "omt_1"); err != nil {
		t.Fatal(err)
	}

	s2, err := newThreadStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if root, ok := s2.root("s1"); !ok || root != "om_1" {
		t.Fatalf("root = %q %v", root, ok)
	}
	if id, ok := s2.session("omt_1", ""); !ok || id != "s1" {
		t.Fatalf("byThread = %q %v", id, ok)
	}
	if id, ok := s2.session("", "om_1"); !ok || id != "s1" {
		t.Fatalf("byRoot = %q %v", id, ok)
	}
}

func TestDecisionCodec(t *testing.T) {
	cases := []struct {
		value    obj
		behavior string
		scope    string
		ok       bool
	}{
		{obj{"k": "a", "id": "x"}, "allow", "", true},
		{obj{"k": "s", "id": "x"}, "allow", "session", true},
		{obj{"k": "d", "id": "x"}, "deny", "", true},
		{obj{"k": "i", "id": "x"}, "deny", "", true},
		{obj{"k": "zz", "id": "x"}, "", "", false},
		{obj{"id": "x"}, "", "", false},
		{obj{"k": "a"}, "", "", false},
	}
	for _, c := range cases {
		v, ok := parseActionValue(c.value)
		if !ok {
			if c.ok {
				t.Errorf("parseActionValue(%v) failed", c.value)
			}
			continue
		}
		behavior, scope, ok := decodeDecision(v)
		if behavior != c.behavior || scope != c.scope || ok != c.ok {
			t.Errorf("decode(%v) = %q %q %v, want %q %q %v", c.value, behavior, scope, ok, c.behavior, c.scope, c.ok)
		}
	}
}

// TestFailedPermissionPostIsRetriedOnReplay: a card that never reached Lark
// must not be suppressed by the posted-dedupe when the snapshot replays it.
func TestFailedPermissionPostIsRetriedOnReplay(t *testing.T) {
	f, r := &fakeLark{failSend: true, failCards: true}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r)
	runHub(t, h)

	p := hook.Pending{ID: "p1", SessionID: "s1", ToolName: "Bash"}
	r.pendingCh <- p
	waitFor(t, "failed post unclaimed", func() bool {
		h.postedMu.Lock()
		defer h.postedMu.Unlock()
		return len(h.posted) == 0
	})

	f.mu.Lock()
	f.failSend, f.failCards = false, false
	f.mu.Unlock()
	r.pendingCh <- p // the snapshot replay after a reconnect
	waitFor(t, "replayed card", func() bool {
		msgs := f.messages()
		return len(msgs) >= 2 && msgs[len(msgs)-1].kind == "card"
	})
}

// TestStaleAskDoesNotSwallowPrompt: a question answered in the web UI leaves
// a stale ask entry; the next typed message must reach the session as a
// prompt, not vanish as an "answer".
func TestStaleAskDoesNotSwallowPrompt(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r, testUser)
	runHub(t, h)

	r.pendingCh <- hook.Pending{ID: "q1", SessionID: "s1", ToolName: "AskUserQuestion",
		ToolInput: json.RawMessage(`{"questions":[{"question":"Deploy?","options":[{"label":"Yes"}]}]}`)}
	waitFor(t, "ask card", func() bool { return len(f.messages()) >= 2 })

	// Resolve it "in the web UI" (directly on the router).
	if err := r.RespondInteraction("q1", hook.Response{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}

	h.HandleMessage(context.Background(), inboundMessage(testChat, testUser, "omt_om_root_1", "", "new prompt"))
	if got := r.sent["s1"]; len(got) != 1 || got[0] != "new prompt" {
		t.Fatalf("typed message after stale ask should route as a prompt, got %v", got)
	}
}

// TestTransportFailureKeepsAskAnswerable: a socket failure while answering
// must keep the entry so retyping retries, and must not ack.
func TestTransportFailureKeepsAskAnswerable(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r, testUser)
	runHub(t, h)

	r.pendingCh <- hook.Pending{ID: "q1", SessionID: "s1", ToolName: "AskUserQuestion",
		ToolInput: json.RawMessage(`{"questions":[{"question":"Deploy?","options":[{"label":"Yes"}]}]}`)}
	waitFor(t, "ask card", func() bool { return len(f.messages()) >= 2 })

	r.mu.Lock()
	r.respondErr = errors.New("dial unix: connection refused")
	r.mu.Unlock()
	h.HandleMessage(context.Background(), inboundMessage(testChat, testUser, "omt_om_root_1", "", "Yes"))
	if len(f.reacted) != 0 {
		t.Fatal("failed answer delivery must not ack")
	}
	if len(r.sent["s1"]) != 0 {
		t.Fatalf("failed answer must not become a prompt: %v", r.sent["s1"])
	}

	r.mu.Lock()
	r.respondErr = nil
	r.mu.Unlock()
	h.HandleMessage(context.Background(), inboundMessage(testChat, testUser, "omt_om_root_1", "", "Yes"))
	if got, _ := r.response("q1"); got.Answers["Deploy?"] != "Yes" {
		t.Fatalf("retyped answer should resolve the ask, got %+v", got.Answers)
	}
}

// TestResolvedCardReplacesButtons: a decided card comes back buttonless with
// the outcome, and a malformed ask tap does not consume the entry.
func TestResolvedCardReplacesButtons(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r, testUser)
	runHub(t, h)

	r.pendingCh <- hook.Pending{ID: "p1", SessionID: "s1", ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"make build"}`)}
	waitFor(t, "card", func() bool { return len(f.messages()) >= 2 })

	resp := h.HandleCardAction(context.Background(), cardTap(testChat, testUser, obj{"k": "a", "id": "p1"}))
	if resp.Card == nil || resp.Card.Type != "raw" {
		t.Fatalf("decided card should be re-rendered, got %+v", resp.Card)
	}
	rendered := cardJSON(resp.Card.Data.(obj))
	if strings.Contains(rendered, `"tag":"button"`) || !strings.Contains(rendered, "make build") {
		t.Fatalf("resolved card should keep the body and drop buttons: %s", rendered)
	}

	// Ask flow: malformed opt keeps the entry; the valid tap still works.
	r.pendingCh <- hook.Pending{ID: "q1", SessionID: "s1", ToolName: "AskUserQuestion",
		ToolInput: json.RawMessage(`{"questions":[{"question":"Go?","options":[{"label":"Yes"}]}]}`)}
	waitFor(t, "ask card", func() bool { return len(f.messages()) >= 3 })
	if resp := h.HandleCardAction(context.Background(), cardTap(testChat, testUser, obj{"k": "q", "id": "q1", "opt": "7"})); resp.Toast == nil || resp.Toast.Content != "expired" {
		t.Fatalf("out-of-range opt should toast expired, got %+v", resp.Toast)
	}
	if resp := h.HandleCardAction(context.Background(), cardTap(testChat, testUser, obj{"k": "q", "id": "q1", "opt": "0"})); resp.Card == nil {
		t.Fatalf("valid tap after malformed one should still resolve, got %+v", resp)
	}
	if got, _ := r.response("q1"); got.Answers["Go?"] != "Yes" {
		t.Fatalf("answer = %+v", got.Answers)
	}
}

// TestCardFenceInjectionDefanged: a tool input containing ``` cannot close
// the code fence and smuggle card markup into the card.
func TestCardFenceInjectionDefanged(t *testing.T) {
	p := hook.Pending{ID: "p1", ToolName: "Bash",
		ToolInput: json.RawMessage("{\"command\":\"echo hi\\n```\\n<at id=all></at> harmless\"}")}
	rendered := cardJSON(permissionCard(p, nil, ""))
	if strings.Contains(rendered, "<at id=all>") && !strings.Contains(rendered, "'''") {
		t.Fatalf("fence not defanged: %s", rendered)
	}
	var c struct {
		Schema string `json:"schema"`
		Body   struct {
			Elements []struct {
				Tag     string `json:"tag"`
				Content string `json:"content"`
			} `json:"elements"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(rendered), &c); err != nil {
		t.Fatal(err)
	}
	if c.Schema != "2.0" {
		t.Fatalf("card schema = %q, want 2.0", c.Schema)
	}
	if c.Body.Elements[0].Tag != "markdown" || !strings.Contains(c.Body.Elements[0].Content, "'''") {
		t.Fatalf("``` should be rewritten inside a markdown fence: %+v", c.Body.Elements[0])
	}
}

// TestCardBodyElementTags: 2.0 rejects bare plain_text (and other nested-only
// tags) as body elements — Lark parses cards server-side and errors, so every
// builder's top-level elements must stick to standalone-legal components.
func TestCardBodyElementTags(t *testing.T) {
	legal := map[string]bool{"div": true, "markdown": true, "column_set": true, "button": true}
	ask := imutil.AskQuestion{Header: "Deploy", Question: "Deploy now?", Options: []struct {
		Label string `json:"label"`
	}{{Label: "Yes"}, {Label: "No"}}}
	free := imutil.AskQuestion{Question: "Name?", MultiSelect: true, Options: []struct {
		Label string `json:"label"`
	}{{Label: "a"}}}
	p := hook.Pending{ID: "p1", ToolName: "Bash", ToolInput: json.RawMessage(`{"command":"ls"}`)}
	cards := map[string]obj{
		"root":                rootCard("fix the bug", "/w", "claude · 3f2a1b9c"),
		"permission":          permissionCard(p, []string{"ou_x"}, ""),
		"permission-resolved": permissionCard(p, nil, "allowed"),
		"ask":                 askCard(ask, "q1", []string{"ou_x"}, ""),
		"ask-resolved":        askCard(ask, "q1", nil, "answered"),
		"ask-freeform":        askCard(free, "q2", nil, ""),
		"multi-step":          multiStepCard("m1", []string{"ou_x"}, ""),
		"multi-step-resolved": multiStepCard("m1", nil, "ignored"),
	}
	for name, c := range cards {
		var parsed struct {
			Body struct {
				Elements []struct {
					Tag string `json:"tag"`
				} `json:"elements"`
			} `json:"body"`
		}
		if err := json.Unmarshal([]byte(cardJSON(c)), &parsed); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(parsed.Body.Elements) == 0 {
			t.Errorf("%s: no body elements", name)
		}
		for i, el := range parsed.Body.Elements {
			if !legal[el.Tag] {
				t.Errorf("%s: elements[%d] tag %q is not a standalone 2.0 component", name, i, el.Tag)
			}
		}
	}
}

// TestDuplicateInboundPushIgnored: Feishu delivers events at least once; a
// redelivered push (same message id) must not reach the session twice.
func TestDuplicateInboundPushIgnored(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r, testUser)
	if err := h.store.put("s1", "om_root_1"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.setThread("s1", "omt_1"); err != nil {
		t.Fatal(err)
	}

	ev := inboundMessage(testChat, testUser, "omt_1", "", "run the tests")
	h.HandleMessage(context.Background(), ev)
	h.HandleMessage(context.Background(), ev) // redelivery of the same push
	if got := r.sent["s1"]; len(got) != 1 || got[0] != "run the tests" {
		t.Fatalf("redelivered push must route once, got %v", got)
	}
	if len(f.reacted) != 1 {
		t.Fatalf("want a single ack, got %v", f.reacted)
	}
}

// TestMarkdownCardFallsBackToText: a rejected markdown card degrades to
// plain text messages instead of dropping the content.
func TestMarkdownCardFallsBackToText(t *testing.T) {
	f, r := &fakeLark{failPosts: true}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1"}
	h := newTestHub(t, f, r)
	runHub(t, h)

	raw := json.RawMessage(`{"message":{"content":[{"type":"text","text":"**still here**"}]}}`)
	waitFor(t, "plain fallback", func() bool {
		r.broker.Publish(broker.Event{SessionID: "s1", Type: "assistant", Raw: raw})
		msgs := f.messages()
		if len(msgs) == 0 {
			return false
		}
		last := msgs[len(msgs)-1]
		return last.kind == "text" && last.body == "**still here**"
	})
}

// TestRootCardRetitledOnTurnEnd: the AI title usually lands after the thread
// exists; the root card is patched once at turn end, and only on change.
func TestRootCardRetitledOnTurnEnd(t *testing.T) {
	f, r := &fakeLark{}, newFakeRouter()
	r.sessions["s1"] = core.Session{ID: "s1", Cwd: "/w"} // no title yet
	h := newTestHub(t, f, r)
	runHub(t, h)

	raw := json.RawMessage(`{"message":{"content":[{"type":"text","text":"working"}]}}`)
	waitFor(t, "thread created", func() bool {
		r.broker.Publish(broker.Event{SessionID: "s1", Type: "assistant", Raw: raw})
		return len(f.messages()) >= 2
	})

	// Turn ends with an unchanged title: no patch.
	r.broker.Publish(broker.Event{SessionID: "s1", Type: "subprocess.exit"})
	waitFor(t, "turn ping", func() bool {
		msgs := f.messages()
		return msgs[len(msgs)-1].kind == "text"
	})
	for _, m := range f.messages() {
		if m.kind == "update" {
			t.Fatalf("unchanged title must not patch the root card: %+v", m)
		}
	}

	// The AI title lands; the next turn end patches the root card once.
	r.mu.Lock()
	r.sessions["s1"] = core.Session{ID: "s1", Title: "fix the flaky test", Cwd: "/w"}
	r.mu.Unlock()
	r.broker.Publish(broker.Event{SessionID: "s1", Type: "subprocess.exit"})
	waitFor(t, "root card patched", func() bool {
		for _, m := range f.messages() {
			if m.kind == "update" && m.to == "om_root_1" && strings.Contains(m.body, "fix the flaky test") {
				return true
			}
		}
		return false
	})

	// A further turn end with the same title patches nothing new.
	r.broker.Publish(broker.Event{SessionID: "s1", Type: "subprocess.exit"})
	time.Sleep(50 * time.Millisecond)
	updates := 0
	for _, m := range f.messages() {
		if m.kind == "update" {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("want exactly one root-card patch, got %d", updates)
	}
}
