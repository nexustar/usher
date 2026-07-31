package pi

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/interaction"
)

func questionsOf(t *testing.T, ev interaction.Request) []uiQuestion {
	t.Helper()
	var payload struct {
		Questions []uiQuestion `json:"questions"`
	}
	if err := json.Unmarshal(ev.ToolInput, &payload); err != nil {
		t.Fatalf("tool input %s: %v", ev.ToolInput, err)
	}
	return payload.Questions
}

func TestDialogEventMapsEveryAnswerableMethod(t *testing.T) {
	cases := []struct {
		name      string
		req       extensionUIRequest
		kind      string
		question  string
		options   []string
		multiline bool
	}{
		{
			name:     "select offers its options",
			req:      extensionUIRequest{Method: "select", Title: "Pick one", Options: []string{"A", "B"}},
			kind:     interaction.KindChoice,
			question: "Pick one",
			options:  []string{"A", "B"},
		},
		{
			// confirm carries a separate message; both halves have to reach the
			// user or the question reads as bare title.
			name:     "confirm becomes a two-option question",
			req:      extensionUIRequest{Method: "confirm", Title: "Deploy?", Message: "to production"},
			kind:     interaction.KindChoice,
			question: "Deploy?\nto production",
			options:  []string{confirmYes, confirmNo},
		},
		{
			name:     "input is free text",
			req:      extensionUIRequest{Method: "input", Title: "Name?", Placeholder: "your name"},
			kind:     interaction.KindText,
			question: "Name?",
		},
		{
			name:      "editor is multiline free text",
			req:       extensionUIRequest{Method: "editor", Title: "Edit", Prefill: "draft"},
			kind:      interaction.KindText,
			question:  "Edit",
			multiline: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.ID = "d1"
			ev, ok := dialogEvent("s1", "/work", tc.req)
			if !ok {
				t.Fatal("method not mapped")
			}
			if ev.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", ev.Kind, tc.kind)
			}
			// A blanket auto-approve must never settle these: it carries no
			// answer to send back.
			if _, quick := (&interaction.Manager{}).QuickDecide(interaction.Request{Kind: ev.Kind}); quick {
				t.Error("auto-approve short-circuited a question")
			}
			qs := questionsOf(t, ev)
			if len(qs) != 1 {
				t.Fatalf("questions = %+v, want 1", qs)
			}
			if qs[0].Question != tc.question {
				t.Errorf("question = %q, want %q", qs[0].Question, tc.question)
			}
			if qs[0].Multiline != tc.multiline {
				t.Errorf("multiline = %v, want %v", qs[0].Multiline, tc.multiline)
			}
			var got []string
			for _, o := range qs[0].Options {
				got = append(got, o.Label)
			}
			if len(got) != len(tc.options) {
				t.Fatalf("options = %v, want %v", got, tc.options)
			}
			for i := range got {
				if got[i] != tc.options[i] {
					t.Fatalf("options = %v, want %v", got, tc.options)
				}
			}
		})
	}

	if _, ok := dialogEvent("s1", "/work", extensionUIRequest{ID: "d1", Method: "hologram"}); ok {
		t.Error("an unknown method must not map to an interaction")
	}
}

// Pi reads a cancel as undefined for select/input/editor but as false for
// confirm, so a denied dialog must not come back as an empty value.
func TestDialogReplyShapes(t *testing.T) {
	allowA := interaction.Response{Behavior: "allow", Answers: map[string]string{"q": "A"}}
	deny := interaction.Response{Behavior: "deny"}

	if got := dialogReply("select", allowA); got["value"] != "A" {
		t.Errorf("select allow = %v", got)
	}
	if got := dialogReply("select", deny); got["cancelled"] != true {
		t.Errorf("select deny = %v", got)
	}
	yes := interaction.Response{Behavior: "allow", Answers: map[string]string{"q": confirmYes}}
	no := interaction.Response{Behavior: "allow", Answers: map[string]string{"q": confirmNo}}
	if got := dialogReply("confirm", yes); got["confirmed"] != true {
		t.Errorf("confirm yes = %v", got)
	}
	if got := dialogReply("confirm", no); got["confirmed"] != false {
		t.Errorf("confirm no = %v", got)
	}
	if got := dialogReply("confirm", deny); got["cancelled"] != true {
		t.Errorf("confirm deny = %v", got)
	}
	if got := dialogReply("input", allowA); got["value"] != "A" {
		t.Errorf("input allow = %v", got)
	}
}

// The permission fast path stays keyed to the exact shape npm:pi-permission-system
// emits: anything looser would capture ordinary questions and hand them to
// blanket auto-approve.
func TestPermissionFastPathIsExact(t *testing.T) {
	exact := extensionUIRequest{
		Method: "select", Title: "Permission Required: bash", Options: piPermissionSystemOptions,
	}
	if !isPiPermissionSystemRequest(exact) {
		t.Fatal("the permission-system shape was not recognized")
	}
	// The official permission-gate example: same intent, different shape. It
	// must fall through to the generic choice route, not be auto-approved.
	gate := extensionUIRequest{
		Method: "select", Title: "⚠️ Dangerous command:\n\n  rm -rf /\n\nAllow?", Options: []string{"Yes", "No"},
	}
	if isPiPermissionSystemRequest(gate) {
		t.Error("a two-option dialog was mistaken for the permission system")
	}
	ev, ok := dialogEvent("s1", "/work", gate)
	if !ok || ev.Kind != interaction.KindChoice {
		t.Errorf("gate dialog = %+v, ok=%v", ev, ok)
	}
}

// An idle worker must still answer dialogs. Before the pump owned the event
// stream nothing read it between turns, so an extension that asked a question
// outside a turn waited forever.
func TestIdleWorkerAnswersDialog(t *testing.T) {
	interactions := interaction.New("")
	r, w := fakePiRuntime(t, `{"sessionId":"s1"}`, `:`)
	r.interactions = interactions

	// Push a dialog through the worker's stdout with no turn in flight.
	w.c.events <- json.RawMessage(
		`{"type":"extension_ui_request","id":"d9","method":"select","title":"Pick","options":["A","B"]}`)

	var pending interaction.Pending
	deadline := time.Now().Add(3 * time.Second)
	for {
		if list := interactions.List(); len(list) > 0 {
			pending = list[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle worker never surfaced the dialog")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pending.Kind != interaction.KindChoice || pending.SessionID != "s1" {
		t.Fatalf("pending = %+v", pending)
	}
	if err := interactions.Respond(pending.ID, interaction.Response{
		Behavior: "allow", Answers: map[string]string{"Pick": "B"},
	}); err != nil {
		t.Fatal(err)
	}
	// The answer goes back to pi as an extension_ui_response naming the choice.
	waitFor(t, filepath.Join(w.cwd, "rpc.log"), `"value":"B"`)
}

// waitFor polls path until it contains want.
func waitFor(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(path)
			t.Fatalf("never saw %s in RPC log:\n%s", want, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An extension command runs inside pi and returns without an agent loop, so no
// agent_settled ever arrives. Waiting for one would leave the turn running
// forever; the RPC response is the completion signal instead.
func TestExtensionCommandEndsWithoutAgentSettled(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-pi")
	// get_commands files /demo under "extension"; prompt answers but, like pi,
	// never emits agent_settled for it.
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  case "$line" in
    *'"type":"get_commands"'*)
      printf '{"type":"response","id":"%s","success":true,"data":{"commands":[{"name":"demo","description":"d","source":"extension"}]}}\n' "$id"
      ;;
    *'"type":"get_state"'*)
      printf '{"type":"response","id":"%s","success":true,"data":{"sessionId":"s1"}}\n' "$id"
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
	r := &Runtime{workers: map[string]*worker{}, max: 1, logger: slog.New(slog.NewTextHandler(os.Stderr, nil))}
	w := &worker{c: c, cwd: dir, path: path, last: time.Now()}
	if err := r.add("s1", w); err != nil {
		t.Fatal(err)
	}

	ch, err := r.Send(context.Background(), "s1", "/demo", dir)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range collectPi(t, ch, 5*time.Second) {
		types = append(types, ev.Type)
	}
	if len(types) == 0 || types[len(types)-1] != backend.EventProcessExit {
		t.Fatalf("extension command did not finish: %v", types)
	}
	r.mu.Lock()
	busy := w.busy
	r.mu.Unlock()
	if busy {
		t.Error("worker left busy after an extension command")
	}
}
