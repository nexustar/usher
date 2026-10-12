package rollout

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/nexustar/usher/internal/core"
)

// request_user_input_async asks without waiting: the questions ride an
// AgentMessage item and the turn goes on. The answer is a user message in the
// envelope Codex's clients recognize; core reads it as plain text.
const (
	asyncQuestionTool = "request_user_input_async"
	asyncMessageTool  = "send_message_to_user_async"
	replyOpen         = "<send_user_message_question_reply>"
	replyClose        = "</send_user_message_question_reply>"
	// replyTitleBytes bounds the question a reply quotes, as Codex's clients do.
	replyTitleBytes = 512
)

// itemQuestions projects an AgentMessage item's questions; nil when it asks none.
func itemQuestions(item json.RawMessage) []core.Question {
	var p struct {
		ID        string `json:"id"`
		Questions []struct {
			Title   string   `json:"title"`
			Options []string `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(item, &p) != nil || len(p.Questions) == 0 {
		return nil
	}
	out := make([]core.Question, 0, len(p.Questions))
	for i, q := range p.Questions {
		// Codex's clients name a question by tool, message id and index.
		id, _ := json.Marshal([]any{asyncQuestionTool, p.ID, i})
		out = append(out, core.Question{ID: string(id), Title: q.Title, Options: q.Options})
	}
	return out
}

type questionReply struct {
	Answer         string `json:"answer"`
	Question       string `json:"question"`
	QuestionItemID string `json:"questionItemId"`
}

// QuestionReply is the user message answering questions with answers, keyed
// by title; "" when none is answered.
func QuestionReply(questions []core.Question, answers map[string]string) string {
	var replies []questionReply
	for _, q := range questions {
		answer := strings.TrimSpace(answers[q.Title])
		if answer == "" {
			continue
		}
		replies = append(replies, questionReply{Answer: answer, Question: replyTitle(q.Title), QuestionItemID: q.ID})
	}
	if len(replies) == 0 {
		return ""
	}
	body, _ := json.Marshal(replies)
	return replyOpen + "\n" + string(body) + "\n" + replyClose
}

// replyTitle is the question as a reply quotes it: one line, bounded.
func replyTitle(title string) string {
	title = strings.NewReplacer("\n", " ", "\r", " ").Replace(title)
	if len(title) > replyTitleBytes {
		cut := replyTitleBytes
		for cut > 0 && !utf8.RuneStart(title[cut]) {
			cut--
		}
		title = title[:cut]
	}
	return title
}

// questionReplyText renders a reply envelope as the user saw it: the question
// quoted, then the answer. ok is false for any other text.
func questionReplyText(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, replyOpen) || !strings.HasSuffix(t, replyClose) {
		return "", false
	}
	body := []byte(t[len(replyOpen) : len(t)-len(replyClose)])
	var replies []questionReply
	if json.Unmarshal(body, &replies) != nil {
		// Older desktop replies hold one object.
		var one questionReply
		if json.Unmarshal(body, &one) != nil {
			return "", false
		}
		replies = []questionReply{one}
	}
	if len(replies) == 0 {
		return "", false
	}
	shown := make([]string, 0, len(replies))
	for _, r := range replies {
		shown = append(shown, "> "+r.Question+"\n\n"+r.Answer)
	}
	return strings.Join(shown, "\n\n"), true
}

// userText is a user message as shown: a question reply reads as quote and answer.
func userText(msg string) string {
	if shown, ok := questionReplyText(msg); ok {
		return shown
	}
	return msg
}
