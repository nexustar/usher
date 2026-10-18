package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/interaction"
)

func requestUserInputParams() json.RawMessage {
	return json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":false,"questions":[` +
		`{"id":"approach","header":"Approach","question":"Which approach?","isOther":true,"isSecret":false,"options":[{"label":"Rewrite","description":"Start over."},{"label":"Patch","description":"Minimal fix."}]},` +
		`{"id":"branch","header":"Branch","question":"Branch name?","isOther":true,"isSecret":false,"options":[{"label":"main","description":"Default."}]}]}`)
}

// awaitPending waits for the one interaction the request raises.
func awaitPending(t *testing.T, h *interaction.Manager) interaction.Pending {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if list := h.List(); len(list) == 1 {
			return list[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("request_user_input raised no interaction")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// request_user_input reaches the UI as a choice in AskUserQuestion's shape and
// is answered by question id; a typed answer goes back as a note.
func TestRequestUserInputAnswersByQuestionID(t *testing.T) {
	interactions := interaction.New()
	out := new(testWriteCloser)
	c := New("unused", interactions, nil, nil, nil, nil, nil)
	c.in = out
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.requestUserInput(rpcMessage{ID: json.RawMessage(`12`), Method: "item/tool/requestUserInput", Params: requestUserInputParams()})
	}()
	pending := awaitPending(t, interactions)
	if pending.Kind != interaction.KindChoice || pending.SessionID != "thread-1" {
		t.Fatalf("pending = %+v", pending)
	}
	var input struct {
		Questions []struct {
			Header, Question string
			Options          []struct{ Label, Description string }
		}
	}
	if err := json.Unmarshal(pending.ToolInput, &input); err != nil || len(input.Questions) != 2 {
		t.Fatalf("tool input = %s (%v)", pending.ToolInput, err)
	}
	if q := input.Questions[0]; q.Header != "Approach" || q.Question != "Which approach?" ||
		len(q.Options) != 2 || q.Options[1].Label != "Patch" || q.Options[1].Description != "Minimal fix." {
		t.Fatalf("question = %+v", q)
	}
	if err := interactions.Respond(pending.ID, interaction.Response{
		Behavior: "allow",
		Answers:  map[string]string{"Which approach?": "Patch", "Branch name?": "feat/async"},
	}); err != nil {
		t.Fatal(err)
	}
	<-done
	var resp struct {
		Result struct {
			Answers map[string]struct{ Answers []string } `json:"answers"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("response %s: %v", out.Bytes(), err)
	}
	if a := resp.Result.Answers; len(a) != 2 ||
		strings.Join(a["approach"].Answers, "|") != "Patch" ||
		strings.Join(a["branch"].Answers, "|") != "user_note: feat/async" {
		t.Fatalf("answers = %+v", a)
	}
}

// An ignored request answers nothing, which Codex reads as skipped.
func TestRequestUserInputIgnoredAnswersNothing(t *testing.T) {
	interactions := interaction.New()
	out := new(testWriteCloser)
	c := New("unused", interactions, nil, nil, nil, nil, nil)
	c.in = out
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.requestUserInput(rpcMessage{ID: json.RawMessage(`13`), Method: "item/tool/requestUserInput", Params: requestUserInputParams()})
	}()
	pending := awaitPending(t, interactions)
	if err := interactions.Respond(pending.ID, interaction.Response{Behavior: "deny"}); err != nil {
		t.Fatal(err)
	}
	<-done
	if !strings.Contains(out.String(), `"answers":{}`) {
		t.Fatalf("response = %s", out.String())
	}
}
