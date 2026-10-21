package router

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	backendpkg "github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/interaction"
)

// Questions a text part asks without waiting (core.TurnPart.Questions) are
// raised as one choice interaction. The answers go back as the backend's reply
// prompt, steered into the turn while it runs, else as the next turn. Any
// other prompt to the session withdraws them.

// askToken identifies one open question for withdrawal.
type askToken struct{ cancel context.CancelFunc }

// askInPlace raises questions a live turn of sessionID just asked.
func (r *Router) askInPlace(sessionID string, questions []core.Question) {
	if r.interactions == nil || len(questions) == 0 {
		return
	}
	b, ok := r.backends[r.backendOf(sessionID)]
	if !ok || b.Replier == nil {
		return
	}
	// The shape AskUserQuestion already puts in ToolInput.
	type option struct {
		Label string `json:"label"`
	}
	type question struct {
		Question string   `json:"question"`
		Options  []option `json:"options,omitempty"`
	}
	asked := make([]question, 0, len(questions))
	for _, q := range questions {
		uq := question{Question: q.Title}
		for _, o := range q.Options {
			uq.Options = append(uq.Options, option{Label: o})
		}
		asked = append(asked, uq)
	}
	input, err := json.Marshal(map[string]any{"questions": asked})
	if err != nil {
		return
	}
	sess, _ := r.discovery.Get(sessionID)
	ctx, cancel := context.WithCancel(context.Background())
	tok := &askToken{cancel: cancel}
	r.askMu.Lock()
	if r.asks == nil {
		r.asks = map[string]map[*askToken]struct{}{}
	}
	if r.asks[sessionID] == nil {
		r.asks[sessionID] = map[*askToken]struct{}{}
	}
	r.asks[sessionID][tok] = struct{}{}
	r.askMu.Unlock()
	go func() {
		defer r.forgetAsk(sessionID, tok)
		resp, err := r.interactions.Submit(ctx, interaction.Request{
			SessionID: sessionID,
			ToolUseID: questions[0].ID,
			Kind:      interaction.KindChoice,
			ToolName:  "request_user_input_async",
			ToolInput: input,
			Cwd:       sess.Cwd,
		})
		if err != nil || resp.Behavior != "allow" {
			return
		}
		if text := b.Replier.QuestionReply(questions, resp.Answers); text != "" {
			r.replyInPlace(sessionID, text)
		}
	}()
}

func (r *Router) forgetAsk(sessionID string, tok *askToken) {
	r.askMu.Lock()
	delete(r.asks[sessionID], tok)
	if len(r.asks[sessionID]) == 0 {
		delete(r.asks, sessionID)
	}
	r.askMu.Unlock()
	tok.cancel()
}

// withdrawQuestions closes the session's open questions.
func (r *Router) withdrawQuestions(sessionID string) {
	r.askMu.Lock()
	toks := r.asks[sessionID]
	delete(r.asks, sessionID)
	r.askMu.Unlock()
	for tok := range toks {
		tok.cancel()
	}
}

// replyInPlace delivers an answer into the running turn when the backend can
// steer one, else as the next turn.
func (r *Router) replyInPlace(sessionID, text string) {
	r.sendMu.Lock()
	_, busy := r.activeSend[sessionID]
	r.sendMu.Unlock()
	if busy {
		if s, ok := r.senderFor(sessionID).(backendpkg.Steerer); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := s.Steer(ctx, sessionID, text)
			cancel()
			if err == nil {
				slog.Debug("question answered into the running turn", "session", sessionID)
				return
			}
			slog.Debug("steer declined; answering as a turn", "session", sessionID, "err", err)
		}
	}
	if err := r.enqueueSend(sessionID, text, nil, nil); err != nil {
		slog.Warn("question answer not sent", "session", sessionID, "err", err)
	}
}
