package telegram

import (
	"context"
	"encoding/json"
	"html"
	"strconv"
	"strings"

	"github.com/nexustar/usher/internal/hook"
)

// askEntry remembers a posted AskUserQuestion awaiting an answer: the question
// text (to key the answer), the option labels (so a tapped index → label), and
// the topic thread (so a typed reply in that topic can answer it too).
type askEntry struct {
	question string
	labels   []string
	thread   int64
}

// askQuestion is the subset of an AskUserQuestion question we render.
type askQuestion struct {
	Header      string `json:"header"`
	Question    string `json:"question"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label string `json:"label"`
	} `json:"options"`
}

func parseQuestions(raw json.RawMessage) []askQuestion {
	var in struct {
		Questions []askQuestion `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil
	}
	return in.Questions
}

// postAskQuestion surfaces an AskUserQuestion in its topic. A single-select
// question with options gets tappable buttons; any single question can also be
// answered by just typing a reply in the topic (covering multiSelect and
// free-form "other"). Only a multi-question prompt can't be mapped to one typed
// reply, so that alone falls back to the web UI.
func (h *Hub) postAskQuestion(ctx context.Context, thread int64, p hook.Pending) {
	qs := parseQuestions(p.ToolInput)
	if len(qs) != 1 {
		text := "🔢 Multi-step question — please answer in the web UI."
		h.sendAskPrompt(ctx, thread, p.ID, text, ignoreOnly(p.ID))
		return
	}

	q := qs[0]
	labels := make([]string, len(q.Options))
	for i, o := range q.Options {
		labels[i] = o.Label
	}
	// Register first so a typed reply in this topic resolves the question.
	h.putAsk(p.ID, askEntry{question: q.Question, labels: labels, thread: thread})

	head := "❓ " + html.EscapeString(truncate(q.Question, 800))
	if q.Header != "" {
		head = "❓ " + html.EscapeString(truncate(q.Header, 200)) + "\n" + html.EscapeString(truncate(q.Question, 800))
	}

	if !q.MultiSelect && len(q.Options) > 0 {
		rows := make([][]InlineKeyboardButton, 0, len(q.Options))
		for i, o := range q.Options {
			rows = append(rows, []InlineKeyboardButton{{
				Text:         truncate(o.Label, 60),
				CallbackData: "q:" + p.ID + ":" + strconv.Itoa(i),
			}})
		}
		h.sendAskPrompt(ctx, thread, p.ID, head+"\n<i>tap an option, or type your own answer</i>",
			&InlineKeyboardMarkup{InlineKeyboard: rows})
		return
	}

	// multiSelect or free-form: answer by typing. List options for reference.
	hint := "reply with your answer"
	if q.MultiSelect {
		hint = "reply with your answer (comma-separated for multiple)"
	}
	text := head + "\n<i>" + hint + "</i>"
	if len(q.Options) > 0 {
		var opts []string
		for _, o := range q.Options {
			opts = append(opts, html.EscapeString(o.Label))
		}
		text += "\noptions: " + strings.Join(opts, ", ")
	}
	h.sendAskPrompt(ctx, thread, p.ID, text, ignoreOnly(p.ID))
}

// sendAskPrompt posts an AskUserQuestion message (HTML, @-mentioned). On send
// failure it drops the registered entry so a dead prompt can't strand a typed
// reply.
func (h *Hub) sendAskPrompt(ctx context.Context, thread int64, id, text string, markup *InlineKeyboardMarkup) {
	if _, err := h.client.SendMessage(ctx, SendMessageParams{
		ChatID:          h.group,
		MessageThreadID: thread,
		Text:            text + h.mentionSuffix(),
		ParseMode:       "HTML",
		ReplyMarkup:     markup,
	}); err != nil {
		h.logger.Warn("telegram: post ask", "id", id, "err", err)
		h.takeAsk(id)
	}
}

// ignoreOnly is the keyboard for a question that can't be answered with a tap:
// a single "Ignore" button (a deny under the hood — claude skips the question),
// matching the web UI's wording.
func ignoreOnly(id string) *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{
		{Text: "🚫 Ignore", CallbackData: encodeDecision("i", id)},
	}}}
}

// answerByText resolves a pending AskUserQuestion in a topic from a typed reply,
// returning true if it consumed the message (so it isn't also sent as a prompt).
func (h *Hub) answerByText(ctx context.Context, thread int64, text string) bool {
	id, entry, ok := h.takeAskByThread(thread)
	if !ok {
		return false
	}
	resp := hook.Response{
		Behavior: "allow",
		Reason:   "via telegram",
		Answers:  map[string]string{entry.question: strings.TrimSpace(text)},
	}
	if err := h.router.RespondInteraction(id, resp); err != nil {
		h.logger.Debug("telegram: answer ask by text", "id", id, "err", err)
	}
	return true
}

// handleAskCallback resolves an AskUserQuestion option tap (callback_data
// "q:<pendingID>:<optionIndex>") into an allow + answer response.
func (h *Hub) handleAskCallback(ctx context.Context, cb *CallbackQuery) {
	parts := strings.SplitN(cb.Data, ":", 3)
	if len(parts) != 3 {
		_ = h.client.AnswerCallbackQuery(ctx, cb.ID, "")
		return
	}
	id := parts[1]
	idx, err := strconv.Atoi(parts[2])
	entry, ok := h.takeAsk(id)
	if err != nil || !ok || idx < 0 || idx >= len(entry.labels) {
		_ = h.client.AnswerCallbackQuery(ctx, cb.ID, "expired")
		h.clearKeyboard(ctx, cb)
		return
	}
	label := entry.labels[idx]
	resp := hook.Response{
		Behavior: "allow",
		Reason:   "via telegram",
		Answers:  map[string]string{entry.question: label},
	}
	toast := "✅ " + label
	if err := h.router.RespondInteraction(id, resp); err != nil {
		toast = "already resolved"
	}
	_ = h.client.AnswerCallbackQuery(ctx, cb.ID, truncate(toast, 200))
	h.clearKeyboard(ctx, cb)
}

// putAsk registers a pending question under both its id and its topic thread.
func (h *Hub) putAsk(id string, e askEntry) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	h.asks[id] = e
	h.asksByThread[e.thread] = id
}

// takeAsk removes a pending question by id (and its thread index), returning it.
func (h *Hub) takeAsk(id string) (askEntry, bool) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	e, ok := h.asks[id]
	if ok {
		delete(h.asks, id)
		delete(h.asksByThread, e.thread)
	}
	return e, ok
}

// takeAskByThread removes the pending question bound to a topic thread, if any.
func (h *Hub) takeAskByThread(thread int64) (string, askEntry, bool) {
	h.askMu.Lock()
	defer h.askMu.Unlock()
	id, ok := h.asksByThread[thread]
	if !ok {
		return "", askEntry{}, false
	}
	e := h.asks[id]
	delete(h.asks, id)
	delete(h.asksByThread, thread)
	return id, e, true
}

func (h *Hub) clearKeyboard(ctx context.Context, cb *CallbackQuery) {
	if cb.Message == nil {
		return
	}
	if err := h.client.EditMessageReplyMarkup(ctx, cb.Message.Chat.ID, cb.Message.MessageID, nil); err != nil {
		h.logger.Debug("telegram: clear keyboard", "err", err)
	}
}
