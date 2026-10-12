package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/sender"
	"github.com/nexustar/usher/internal/transcript"
)

const askedLine = `{"timestamp":"2026-10-12T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"call-1","content":[{"type":"Text","text":"Deploy?\n- Yes\n- No"}],"delivery":"async","questions":[{"title":"Deploy?","options":["Yes","No"]}]}}}`

// steerRuntime records steered text, or refuses like app-server does once
// the turn has ended.
type steerRuntime struct {
	backend.Runtime
	mu      sync.Mutex
	refuse  bool
	steered []string
}

func (s *steerRuntime) Steer(_ context.Context, _ string, text string) error {
	if s.refuse {
		return errors.New("turn ended")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steered = append(s.steered, text)
	return nil
}

func (s *steerRuntime) texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.steered...)
}

func newQuestionTestRouter(t *testing.T, rt *steerRuntime) (*Router, string) {
	t.Helper()
	root := t.TempDir()
	id := "01a10b67-aaaa-7000-8000-000000000001"
	dir := filepath.Join(root, "2026", "10", "12")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-12T10-00-00-"+id+".jsonl")
	meta := `{"timestamp":"2026-10-12T10:00:00Z","type":"session_meta","payload":{"id":"` + id + `","timestamp":"2026-10-12T10:00:00Z","cwd":"/tmp/x"}}` + "\n"
	if err := os.WriteFile(path, []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewCodexSource(root))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	d.Upsert(path)
	r := &Router{
		discovery:      d,
		backends:       map[string]backend.Backend{"codex": {Transcript: transcript.Codex{}, Replier: transcript.Codex{}, Runtime: rt}},
		defaultBackend: "codex",
		broker:         broker.New(),
		interactions:   interaction.New(""),
		activeSend:     map[string]*sendToken{},
		sendQueue:      map[string][]pendingSend{},
		creating:       map[string]core.Session{},
	}
	return r, id
}

func publishAsked(r *Router, id string) {
	asm := transcript.Codex{}.NewAssembler()
	r.publishStream(id, asm, sender.StreamEvent{Type: "event_msg", Raw: json.RawMessage(askedLine)}, time.Time{})
}

func awaitQuestions(t *testing.T, r *Router, n int) []interaction.Pending {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if list := r.interactions.List(); len(list) == n {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending interactions = %+v, want %d", r.interactions.List(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A question asked by a live turn becomes a choice interaction; its answer is
// steered into the still-running turn as the backend's reply envelope.
func TestInPlaceQuestionIsAskedAndSteered(t *testing.T) {
	rt := &steerRuntime{}
	r, id := newQuestionTestRouter(t, rt)
	r.activeSend[id] = &sendToken{cancel: func() {}}
	publishAsked(r, id)
	pending := awaitQuestions(t, r, 1)[0]
	if pending.Kind != interaction.KindChoice || pending.SessionID != id || pending.Cwd != "/tmp/x" {
		t.Fatalf("pending = %+v", pending)
	}
	if in := string(pending.ToolInput); !strings.Contains(in, `"question":"Deploy?"`) || !strings.Contains(in, `{"label":"Yes"}`) {
		t.Fatalf("tool input = %s", in)
	}
	if err := r.interactions.Respond(pending.ID, interaction.Response{Behavior: "allow", Answers: map[string]string{"Deploy?": "Yes"}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(rt.texts()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("answer was not steered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := rt.texts()[0]
	for _, want := range []string{"<send_user_message_question_reply>", `"answer":"Yes"`, `"question":"Deploy?"`, `"questionItemId":"[\"request_user_input_async\",\"call-1\",0]"`} {
		if !strings.Contains(got, want) {
			t.Errorf("steered %q lacks %s", got, want)
		}
	}
	awaitQuestions(t, r, 0)
	if _, queued := r.sendQueue[id]; queued {
		t.Error("a steered answer was also queued as a turn")
	}
}

// Without a steerable turn the answer is the next turn; ignoring sends nothing.
func TestInPlaceQuestionAnswerStartsATurnWhenIdle(t *testing.T) {
	r, id := newQuestionTestRouter(t, &steerRuntime{refuse: true})
	prompts := make(chan string, 2)
	r.runTurn = func(_ context.Context, sessionID, prompt, _ string, tok *sendToken) {
		prompts <- prompt
		r.releaseSend(sessionID, tok)
	}
	publishAsked(r, id)
	pending := awaitQuestions(t, r, 1)[0]
	if err := r.interactions.Respond(pending.ID, interaction.Response{Behavior: "deny"}); err != nil {
		t.Fatal(err)
	}
	awaitQuestions(t, r, 0)
	select {
	case p := <-prompts:
		t.Fatalf("ignoring a question sent %q", p)
	case <-time.After(50 * time.Millisecond):
	}

	// The manager shares one decision per question id for a minute; a fresh
	// manager asks again.
	r.interactions = interaction.New("")
	publishAsked(r, id)
	pending = awaitQuestions(t, r, 1)[0]
	if err := r.interactions.Respond(pending.ID, interaction.Response{Behavior: "allow", Answers: map[string]string{"Deploy?": "No"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-prompts:
		if !strings.Contains(p, `"answer":"No"`) {
			t.Fatalf("turn prompt = %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("answer did not start a turn")
	}
}

// Any other prompt to the session withdraws its open questions.
func TestInPlaceQuestionWithdrawnByPrompt(t *testing.T) {
	r, id := newQuestionTestRouter(t, &steerRuntime{})
	r.runTurn = func(_ context.Context, sessionID, _, _ string, tok *sendToken) {
		r.releaseSend(sessionID, tok)
	}
	publishAsked(r, id)
	awaitQuestions(t, r, 1)
	if err := r.SendToSession(id, "never mind"); err != nil {
		t.Fatal(err)
	}
	awaitQuestions(t, r, 0)
}
