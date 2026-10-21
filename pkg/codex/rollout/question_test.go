package rollout

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/nexustar/usher/pkg/core"
)

const questionID = `["request_user_input_async","call-1",0]`

var deployQuestion = []core.Question{{ID: questionID, Title: "Deploy to prod?", Options: []string{"Yes", "No"}}}

// request_user_input_async asks mid-turn: its AgentMessage item carries the
// questions, the call that only acknowledges it renders nothing, and the
// user's reply envelope reads as quote and answer.
func TestAssemblerAsyncQuestions(t *testing.T) {
	reply, _ := json.Marshal(QuestionReply(deployQuestion, map[string]string{"Deploy to prod?": "Yes"}))
	a := NewAssembler()
	var done []core.Turn
	var streamed []*core.TurnPart
	for _, ln := range []string{
		`{"timestamp":"2026-10-12T10:00:00Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"ship it"}]}}}`,
		`{"timestamp":"2026-10-12T10:00:01Z","type":"response_item","payload":{"type":"function_call","name":"request_user_input_async","arguments":"{\"questions\":[{\"title\":\"Deploy to prod?\",\"options\":[\"Yes\",\"No\"]}]}","call_id":"call-1"}}`,
		`{"timestamp":"2026-10-12T10:00:01Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"call-1","content":[{"type":"Text","text":"Deploy to prod?\n- Yes\n- No"}],"phase":"final_answer","delivery":"async","questions":[{"title":"Deploy to prod?","options":["Yes","No"]}]}}}`,
		`{"timestamp":"2026-10-12T10:00:02Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"{\"accepted\":true}"}}`,
		`{"timestamp":"2026-10-12T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"a2","content":[{"type":"Text","text":"Working on it."}]}}}`,
		`{"timestamp":"2026-10-12T10:00:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
		`{"timestamp":"2026-10-12T10:00:05Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u2","content":[{"type":"text","text":` + string(reply) + `}]}}}`,
	} {
		completed, part := a.Feed([]byte(ln))
		done = append(done, completed...)
		if part != nil {
			streamed = append(streamed, part)
		}
	}
	if len(done) != 3 {
		t.Fatalf("got %d turns, want user, assistant, user: %+v", len(done), done)
	}
	parts := done[1].Parts
	if len(parts) != 2 || parts[0].Type != "text" || parts[1].Type != "text" {
		t.Fatalf("assistant parts = %+v, want the question and the follow-up text only", parts)
	}
	if !strings.Contains(parts[0].Content, "Deploy to prod?") {
		t.Errorf("question content = %q", parts[0].Content)
	}
	q := parts[0].Questions
	if len(q) != 1 || q[0].ID != questionID || q[0].Title != "Deploy to prod?" || strings.Join(q[0].Options, ",") != "Yes,No" {
		t.Errorf("questions = %+v", q)
	}
	if len(parts[1].Questions) != 0 {
		t.Errorf("plain text carries questions: %+v", parts[1].Questions)
	}
	if len(streamed) != 2 || len(streamed[0].Questions) != 1 {
		t.Errorf("streamed parts = %+v, want the question part to carry its questions", streamed)
	}
	if done[2].Role != "user" || done[2].Content != "> Deploy to prod?\n\nYes" {
		t.Errorf("reply turn = %+v", done[2])
	}
}

func TestQuestionReplyRoundTrip(t *testing.T) {
	long := strings.Repeat("é", 400) // 800 bytes
	questions := []core.Question{
		{ID: questionID, Title: "Line one\r\nline two " + long},
		{ID: `["request_user_input_async","call-1",1]`, Title: "Skipped?"},
		{ID: `["request_user_input_async","call-1",2]`, Title: "Branch?", Options: []string{"main"}},
	}
	text := QuestionReply(questions, map[string]string{
		questions[0].Title: "A \"quoted\" answer\nover two lines",
		"Branch?":          " main ",
	})
	shown, ok := questionReplyText(text)
	if !ok {
		t.Fatalf("reply not recognized: %q", text)
	}
	if !strings.HasPrefix(shown, "> Line one  line two ") || !strings.HasSuffix(shown, "\n\n> Branch?\n\nmain") {
		t.Errorf("shown = %q", shown)
	}
	quoted := strings.TrimPrefix(strings.SplitN(shown, "\n\n", 2)[0], "> ")
	if len(quoted) > replyTitleBytes || !utf8.ValidString(quoted) || strings.ContainsAny(quoted, "\r\n") {
		t.Errorf("quoted title not bounded to %d valid bytes on one line: %d bytes", replyTitleBytes, len(quoted))
	}
	var replies []questionReply
	body := strings.TrimSuffix(strings.TrimPrefix(text, replyOpen), replyClose)
	if err := json.Unmarshal([]byte(body), &replies); err != nil || len(replies) != 2 ||
		replies[0].QuestionItemID != questionID || replies[1].QuestionItemID != questions[2].ID {
		t.Fatalf("envelope body = %q (%v)", body, err)
	}
	if QuestionReply(questions, map[string]string{"Skipped?": "  "}) != "" {
		t.Error("blank answers made a reply")
	}

	// Older desktop replies hold one object rather than a list.
	one, ok := questionReplyText(replyOpen + `{"answer":"No","question":"Deploy?","questionItemId":"x"}` + replyClose)
	if !ok || one != "> Deploy?\n\nNo" {
		t.Errorf("single-object reply = %q, %v", one, ok)
	}
	for _, plain := range []string{"hello", replyOpen + "not json" + replyClose, replyOpen + "[]" + replyClose} {
		if got := userText(plain); got != plain {
			t.Errorf("userText(%q) = %q", plain, got)
		}
	}
}

// The legacy user_message event and the session title read a reply the same way.
func TestQuestionReplyInLegacyEventAndMeta(t *testing.T) {
	reply, _ := json.Marshal(QuestionReply(deployQuestion, map[string]string{"Deploy to prod?": "No"}))
	line := `{"timestamp":"2026-10-12T10:00:05Z","type":"event_msg","payload":{"type":"user_message","message":` + string(reply) + `}}`
	a := NewAssembler()
	done, _ := a.Feed([]byte(line))
	if len(done) != 1 || done[0].Content != "> Deploy to prod?\n\nNo" {
		t.Errorf("legacy reply turn = %+v", done)
	}
	s := NewMetaScanner("rollout-2026-10-12T10-00-00-01a10b67-3866-7a13-b481-5150393996b5.jsonl")
	s.Feed([]byte(line))
	if got := s.Meta().Prompt; !strings.HasPrefix(got, "> Deploy to prod?") {
		t.Errorf("title from a reply = %q", got)
	}
}
