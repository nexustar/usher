// Package usheragent is the main-chat agent that routes user messages to
// Claude Code sessions and resolves permission requests.
//
// AgentAPI is intentionally a strict subset of router.Router's surface: the
// agent can read sessions, peek at transcripts, send to a session, and
// respond to a pending interaction — but it cannot subscribe to event
// streams, receive raw hook payloads, or talk to broker / discovery / hook
// managers directly. This boundary is what prevents future LLM agents from
// looping on themselves or escalating their own privileges.
package usheragent

import (
	"context"
	"fmt"
	"time"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

type AgentAPI interface {
	ListSessions() []core.Session
	SendToSession(id, text string) error

	// SendToSessionRelayed is SendToSession plus a background collector:
	// when the turn completes, onDone (shape-compatible with RelaySink)
	// receives the session id and the assistant text. It runs at most once
	// on its own goroutine; a collector that expires with nothing at all is
	// dropped without calling it. This is the delivery primitive behind the
	// main chat's relay channel — the session's reply reaches the user
	// verbatim without the agent holding its turn open.
	SendToSessionRelayed(id, text string, onDone func(sessionID, reply string, err error)) error

	ListPendingInteractions() []hook.Pending
	RespondInteraction(id string, resp hook.Response) error

	// Session housekeeping — the same per-session controls the web sidebar
	// has: archive a session, or auto-approve its permission prompts. The
	// read methods let the agent report current state.
	Archive(id string)
	Unarchive(id string)
	IsArchived(id string) bool
	SetAutoApprove(id string, enabled bool)
	IsAutoApprove(id string) bool

	// ReadSessionTranscript returns the most recent N user/assistant turns
	// from the session's jsonl. limit ≤ 0 means "no cap"; callers should
	// pass a sane default (the LLM agent uses 20 with a 200 ceiling).
	ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error)

	// ReadSessionTranscriptPage returns one page of the transcript: up to
	// limit turns starting at absolute index offset (negative offset = the
	// most recent page), plus the resolved start offset and the total turn
	// count. This is how a caller pages past ReadSessionTranscript's last-N
	// window to reach a deep search hit — no hard depth wall, just page size.
	ReadSessionTranscriptPage(id string, offset, limit int) ([]core.TranscriptTurn, int, int, error)

	// SearchSessionTranscript scans the entire transcript for a
	// case-insensitive substring of query in the user/assistant text and
	// returns at most maxHits matching turns, each with a bounded snippet of
	// contextChars runes of context on either side of the first occurrence.
	// The bool reports whether more turns matched than were returned. This is
	// the locate primitive that avoids read_session_transcript's fixed window
	// truncating away the match.
	SearchSessionTranscript(id, query string, maxHits, contextChars int) ([]core.TranscriptSearchHit, bool, error)

	// SearchAllSessions runs the same substring search across every session
	// and returns one compact result per matching session (hit count + a
	// snippet at the first hit), ranked by hit count. The bool reports whether
	// more sessions matched than maxSessions returned. This is the "which
	// session mentioned X?" primitive — one call instead of per-session fan-out.
	SearchAllSessions(query string, maxSessions, contextChars int) ([]core.SessionSearchResult, bool, error)

	// CreateSession starts a brand-new session in o.Cwd with o.InitialMessage
	// and waits for the first assistant response. Returns the new session id
	// and the assistant text.
	CreateSession(ctx context.Context, o core.CreateOptions, timeout time.Duration) (string, string, error)

	// CreateSessionRelayed is CreateSession without the in-turn wait: it
	// returns once the new session id is known and hands the first assistant
	// reply to onDone in the background (same contract as
	// SendToSessionRelayed; onDone also receives the new session id).
	CreateSessionRelayed(o core.CreateOptions, onDone func(sessionID, reply string, err error)) (string, error)
}

// RelayTag is the marker prepended to a relayed session reply when it is fed
// back to the model as a user-role history observation. The system prompt
// teaches the model this exact shape ("[session <id> replied]") — a test
// pins the two together, so change both or neither.
func RelayTag(sessionID string) string {
	return fmt.Sprintf("[session %s replied]\n", sessionID)
}

// SummaryTag likewise marks a compaction summary in the history; the system
// prompt documents it and a test pins the pair.
const SummaryTag = "[summary of earlier conversation]\n"

// RelaySink delivers a session's completed reply back to the main chat for
// verbatim display. The server (which knows the chat id) supplies it per
// Handle call; agents forward it into the relayed-send primitives. sessionID
// is the session that produced reply; err is non-nil when the turn errored or
// the collector gave up (reply may still carry partial text). A nil sink
// means the caller has no relay channel — agents fall back to plain
// fire-and-forget sends.
type RelaySink func(sessionID, reply string, err error)

// HistoryMessage is one prior turn handed to Agent.Handle. The Agent is
// responsible for converting these into its own backend's message shape
// (e.g. the LLM agent maps Role="agent" to OpenAI's "assistant").
type HistoryMessage struct {
	Role    string // "user" | "agent" | "tool"
	Content string
	Tool    *ToolEvent
}

// ToolEvent is the durable evidence for one main-chat tool invocation. The
// web layer stores it as one atomic record; LLMAgent expands it back into the
// assistant tool_call + tool result pair required by chat-completions APIs.
type ToolEvent struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
}

// AgentResult is what Agent.Handle returns. FocusSession is the session id
// the agent ended up working with this turn — empty when no session-targeted
// tool was called. The server merges this with the previous focus and stores
// it on the persisted agent message.
type AgentResult struct {
	Reply        string
	FocusSession string
	ToolEvents   []ToolEvent
}

// HistorySummarizer is the optional compaction hook: an Agent that also
// implements it can compress the older portion of a chat's history into a
// standing summary. The server type-asserts for it after each turn; agents
// without it (RuleAgent, which ignores history anyway) simply never compact
// and the derivation falls back to trimming.
type HistorySummarizer interface {
	SummarizeHistory(ctx context.Context, history []HistoryMessage) (string, error)
}

// Agent processes a user message in the main chat and returns a reply.
// history is the recent persisted user/agent turns (newest last; the current
// user message is NOT included). currentFocus is the most recent
// FocusSession from prior agent messages, or "" if none. relay (may be nil)
// is where session replies triggered this turn land once complete — the
// agent's own Reply is only routing talk, never a restatement of them.
type Agent interface {
	Handle(ctx context.Context, history []HistoryMessage, currentFocus, userMsg string, relay RelaySink) (AgentResult, error)
}

// strictModeAddendum is appended to the system prompt when LLMConfig.Strict
// is set. Designed to harden small / mid-tier models (Haiku, Flash, mini,
// Qwen-7B-class) against two failure modes: (1) metadata hallucination —
// answering session trivia from memory instead of the injected state;
// (2) role drift — doing substantive intellectual work in the router
// instead of forwarding it to the Claude Code session that has the real
// model and context.
const strictModeAddendum = `

## Strict mode (small-model enforcement)

Every user message ends with a <current_state> block: the current time
(now), a status legend, a useful subset of sessions (full id, cwd, status,
last_input, last_event, title), and the current focus with cwd + title. This block is
the ground truth. last_input is how long ago the user last talked to that
session and last_event how long ago its transcript changed — both already
computed against now; read recency straight off them, never do timestamp
math. Status reflects the PROCESS, not the task: "live" does not mean a
background task finished or is still running — read the transcript tail
to answer that.

### Your role: router, not assistant

usher's value comes from the Claude Code sessions running behind you —
each runs a strong model (Sonnet/Opus) on top of full project context.
Your job is to put the user's message in front of the right session and
return that session's reply, NOT to answer substantive questions
yourself.

For ANY question that is substantive on a session's domain — evaluation,
code review, design critique, analysis, "is this good?", "what should
we do?", "explain how X works", "summarize the design" — route it to the
relevant session with send_to_session. The session's reply is delivered
to the user verbatim by the server when it completes; you never see or
restate it. Do NOT answer from your own reasoning, even when you have
just read the transcript and feel you "know enough". Your reasoning is
small-model reasoning; the session's is not. Reading a transcript is for
figuring out *which session to route to and how to phrase the forwarded
message*, not for answering the user yourself.

You only answer locally when ALL of the following hold:
- The question is trivia readable from <current_state> (count / cwd /
  title / status / focus / pending count / current time / how recently a
  session was active), OR
- It is a meta question about usher itself ("what tools do you have?",
  "/help"), OR
- It is a routing decision ("which session is best for X?"), OR
- No session is relevant to the question.

### Grounding rules

- Trivia (count / cwd / title / status / focus / current time /
  last_active recency) → answer straight from <current_state>. Do NOT
  call list_sessions; the answer is already in the message.
- Never narrate a tool outcome you did not invoke this turn. If you
  say "I created session X", "I sent your message to Y", "I switched
  to Z", "I read the transcript of W", the corresponding tool
  (create_session, focus_session, send_to_session,
  read_session_transcript) MUST
  have actually been called this turn.
  Do NOT manufacture session ids, fabricate tool results, or describe
  outputs that "would have" happened. When the user confirms a
  previously-offered action ("yes", "go ahead", "要", "do it"),
  execute the tool — do NOT skip straight to narrating the result.
- If you find yourself writing "as I mentioned earlier", "based on my
  summary", "based on the previous analysis", or "you have N sessions"
  without a verifying tool call this turn, stop and either consult
  <current_state> or call a tool first.`
