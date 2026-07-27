package usheragent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

type fakeAPI struct {
	sessions   []core.Session
	pending    []hook.Pending
	sentTo     []string
	sentText   []string
	resolved   map[string]hook.Response
	sendErr    error
	respondErr error

	transcripts map[string][]core.TranscriptTurn
	waitReplies map[string]string
	waitErrs    map[string]error

	archived    map[string]bool
	autoApprove map[string]bool

	created     []createCall
	createReply string
	createNewID string
	createErr   error
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		resolved:    map[string]hook.Response{},
		transcripts: map[string][]core.TranscriptTurn{},
		waitReplies: map[string]string{},
		waitErrs:    map[string]error{},
		archived:    map[string]bool{},
		autoApprove: map[string]bool{},
	}
}

func (f *fakeAPI) ListSessions() []core.Session            { return f.sessions }
func (f *fakeAPI) ListPendingInteractions() []hook.Pending { return f.pending }
func (f *fakeAPI) SendToSession(id, text string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sentTo = append(f.sentTo, id)
	f.sentText = append(f.sentText, text)
	return nil
}
func (f *fakeAPI) RespondInteraction(id string, r hook.Response) error {
	if f.respondErr != nil {
		return f.respondErr
	}
	f.resolved[id] = r
	return nil
}
func (f *fakeAPI) ReadSessionTranscript(id string, limit int) ([]core.TranscriptTurn, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, fmt.Errorf("no transcript for %q", id)
	}
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, nil
}
func (f *fakeAPI) ReadSessionTranscriptPage(id string, offset, limit int) ([]core.TranscriptTurn, int, int, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, 0, 0, fmt.Errorf("no transcript for %q", id)
	}
	total := len(turns)
	if limit <= 0 {
		limit = 1
	}
	start := offset
	if start < 0 {
		start = total - limit
	}
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return turns[start:end], start, total, nil
}
func (f *fakeAPI) SearchSessionTranscript(id, query string, maxHits, _ int) ([]core.TranscriptSearchHit, bool, error) {
	turns, ok := f.transcripts[id]
	if !ok {
		return nil, false, fmt.Errorf("no transcript for %q", id)
	}
	var hits []core.TranscriptSearchHit
	matched := 0
	for i, t := range turns {
		if !strings.Contains(strings.ToLower(t.Content), strings.ToLower(query)) {
			continue
		}
		matched++
		if len(hits) >= maxHits {
			continue
		}
		hits = append(hits, core.TranscriptSearchHit{Role: t.Role, Time: t.Time, TurnIndex: i, Occurrences: 1, Snippet: t.Content})
	}
	return hits, matched > len(hits), nil
}
func (f *fakeAPI) SearchAllSessions(query string, maxSessions, _ int) ([]core.SessionSearchResult, bool, error) {
	var out []core.SessionSearchResult
	for _, s := range f.sessions {
		count := 0
		for _, t := range f.transcripts[s.ID] {
			if strings.Contains(strings.ToLower(t.Content), strings.ToLower(query)) {
				count++
			}
		}
		if count > 0 {
			out = append(out, core.SessionSearchResult{SessionID: s.ID, Title: s.Title, Cwd: s.Cwd, HitCount: count})
		}
	}
	truncated := false
	if maxSessions > 0 && len(out) > maxSessions {
		out, truncated = out[:maxSessions], true
	}
	return out, truncated, nil
}
func (f *fakeAPI) CreateSession(_ context.Context, o core.CreateOptions, _ time.Duration) (string, string, error) {
	f.created = append(f.created, createCall{Cwd: o.Cwd, Msg: o.InitialMessage})
	if f.createErr != nil {
		return f.createNewID, f.createReply, f.createErr
	}
	return f.createNewID, f.createReply, nil
}

// SendToSessionRelayed resolves the relay synchronously from waitReplies /
// waitErrs so tests can assert the relayed text without goroutine plumbing.
func (f *fakeAPI) SendToSessionRelayed(id, text string, onDone func(sessionID, reply string, err error)) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sentTo = append(f.sentTo, id)
	f.sentText = append(f.sentText, text)
	if onDone != nil {
		onDone(id, f.waitReplies[id], f.waitErrs[id])
	}
	return nil
}

func (f *fakeAPI) CreateSessionRelayed(o core.CreateOptions, onDone func(sessionID, reply string, err error)) (string, error) {
	f.created = append(f.created, createCall{Cwd: o.Cwd, Msg: o.InitialMessage})
	if f.createErr != nil {
		return "", f.createErr
	}
	if onDone != nil {
		onDone(f.createNewID, f.createReply, nil)
	}
	return f.createNewID, nil
}
func (f *fakeAPI) Archive(id string)                { f.archived[id] = true }
func (f *fakeAPI) Unarchive(id string)              { f.archived[id] = false }
func (f *fakeAPI) IsArchived(id string) bool        { return f.archived[id] }
func (f *fakeAPI) SetAutoApprove(id string, e bool) { f.autoApprove[id] = e }
func (f *fakeAPI) IsAutoApprove(id string) bool     { return f.autoApprove[id] }

func handle(t *testing.T, a *RuleAgent, msg string) string {
	t.Helper()
	res, err := a.Handle(context.Background(), nil, "", msg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Reply
}

func handleFull(t *testing.T, a *RuleAgent, msg string) AgentResult {
	t.Helper()
	res, err := a.Handle(context.Background(), nil, "", msg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRule_HelpAndUnknown(t *testing.T) {
	a := NewRule(newFakeAPI())
	if !strings.Contains(handle(t, a, "/help"), "/list") {
		t.Error("/help missing /list")
	}
	if got := handle(t, a, "/whatever"); !strings.Contains(got, "unknown") {
		t.Errorf("unknown got: %q", got)
	}
	if got := handle(t, a, "hello there"); !strings.Contains(got, "natural-language") {
		t.Errorf("nl got: %q", got)
	}
	if handle(t, a, "") != "" {
		t.Error("empty message should produce empty reply")
	}
}

func TestRule_List(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abcdef0123", Title: "first", Cwd: "/tmp/a", LastEventAt: time.Now()},
		{ID: "0123abcdef", Title: "second", Cwd: "/tmp/b", LastEventAt: time.Now()},
	}
	a := NewRule(api)
	out := handle(t, a, "/list")
	if !strings.Contains(out, "abcdef01") || !strings.Contains(out, "0123abcd") {
		t.Errorf("list missing ids: %q", out)
	}
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("list missing titles: %q", out)
	}
}

func TestRule_ListEmpty(t *testing.T) {
	if got := handle(t, NewRule(newFakeAPI()), "/list"); got != "no sessions" {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendByIDPrefix(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abc12345", Title: "one", Cwd: "/tmp"},
		{ID: "def67890", Title: "two", Cwd: "/tmp"},
	}
	a := NewRule(api)
	res := handleFull(t, a, "/send abc hello world")
	if !strings.Contains(res.Reply, "sent to abc12345") {
		t.Errorf("got %q", res.Reply)
	}
	if res.FocusSession != "abc12345" {
		t.Errorf("FocusSession = %q, want abc12345", res.FocusSession)
	}
	if len(api.sentTo) != 1 || api.sentTo[0] != "abc12345" {
		t.Errorf("sentTo = %v", api.sentTo)
	}
	if api.sentText[0] != "hello world" {
		t.Errorf("sentText = %q", api.sentText[0])
	}
}

func TestRule_NonSendCommandDoesNotChangeFocus(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "one"}}
	a := NewRule(api)
	if res := handleFull(t, a, "/list"); res.FocusSession != "" {
		t.Errorf("/list should not set focus, got %q", res.FocusSession)
	}
	if res := handleFull(t, a, "/help"); res.FocusSession != "" {
		t.Errorf("/help should not set focus, got %q", res.FocusSession)
	}
}

func TestRule_SendByTitleSubstring(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abc", Title: "deploy script"},
		{ID: "def", Title: "run tests"},
	}
	a := NewRule(api)
	if got := handle(t, a, "/send deploy ship it"); !strings.Contains(got, "sent to abc") {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendAmbiguous(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abc1", Title: "one"},
		{ID: "abc2", Title: "two"},
	}
	a := NewRule(api)
	out := handle(t, a, "/send abc hi")
	if !strings.Contains(out, "ambiguous") {
		t.Errorf("got %q", out)
	}
	if len(api.sentTo) != 0 {
		t.Error("send should not have happened")
	}
}

func TestRule_SendNoMatch(t *testing.T) {
	api := newFakeAPI()
	a := NewRule(api)
	if got := handle(t, a, "/send nope hi"); !strings.Contains(got, "no sessions match") {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendUsage(t *testing.T) {
	a := NewRule(newFakeAPI())
	if got := handle(t, a, "/send"); !strings.Contains(got, "usage") {
		t.Errorf("got %q", got)
	}
	if got := handle(t, a, "/send abc"); !strings.Contains(got, "usage") {
		t.Errorf("got %q", got)
	}
}

func TestRule_SendError(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc"}}
	api.sendErr = errors.New("boom")
	a := NewRule(api)
	if got := handle(t, a, "/send abc hi"); !strings.Contains(got, "boom") {
		t.Errorf("got %q", got)
	}
}

func TestRule_FocusDoesNotSendOrRead(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "main chat work"}}
	a := NewRule(api)

	res := handleFull(t, a, "/focus abc")
	if res.FocusSession != "abc12345" {
		t.Fatalf("FocusSession = %q, want abc12345", res.FocusSession)
	}
	if len(api.sentTo) != 0 || len(api.sentText) != 0 {
		t.Fatalf("focus sent a message: ids=%+v texts=%+v", api.sentTo, api.sentText)
	}
	if !strings.Contains(res.Reply, "focused abc12345") {
		t.Errorf("Reply = %q", res.Reply)
	}
}

func TestRule_FocusUsageAndResolutionErrors(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{
		{ID: "abc12345", Title: "one"},
		{ID: "abc67890", Title: "two"},
	}
	a := NewRule(api)
	if got := handle(t, a, "/focus"); !strings.Contains(got, "usage:") {
		t.Errorf("empty focus = %q", got)
	}
	if got := handle(t, a, "/focus missing"); !strings.Contains(got, "no sessions match") {
		t.Errorf("missing focus = %q", got)
	}
	if got := handle(t, a, "/focus abc"); !strings.Contains(got, "ambiguous") {
		t.Errorf("ambiguous focus = %q", got)
	}
}

func TestRule_PendingAndRespond(t *testing.T) {
	api := newFakeAPI()
	api.pending = []hook.Pending{
		{ID: "deadbeefcafef00d", SessionID: "sessabcd", ToolName: "Bash"},
	}
	a := NewRule(api)
	out := handle(t, a, "/pending")
	if !strings.Contains(out, "deadbeef") || !strings.Contains(out, "Bash") {
		t.Errorf("got %q", out)
	}
	out = handle(t, a, "/approve dead")
	if !strings.Contains(out, "allowed") {
		t.Errorf("got %q", out)
	}
	if got := api.resolved["deadbeefcafef00d"]; got.Behavior != "allow" {
		t.Errorf("behavior = %q", got.Behavior)
	}
}

func TestRule_DenyAndAmbiguous(t *testing.T) {
	api := newFakeAPI()
	api.pending = []hook.Pending{
		{ID: "abc11", ToolName: "Bash"},
		{ID: "abc22", ToolName: "Read"},
	}
	a := NewRule(api)
	if got := handle(t, a, "/deny abc"); !strings.Contains(got, "ambiguous") {
		t.Errorf("got %q", got)
	}
	if got := handle(t, a, "/deny abc11"); !strings.Contains(got, "denied") {
		t.Errorf("got %q", got)
	}
}

func TestRule_ApproveNoMatch(t *testing.T) {
	a := NewRule(newFakeAPI())
	if got := handle(t, a, "/approve missing"); !strings.Contains(got, "no pending") {
		t.Errorf("got %q", got)
	}
}

func TestRule_ArchiveAndUnarchive(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "spike"}}
	a := NewRule(api)

	res := handleFull(t, a, "/archive abc")
	if !strings.Contains(res.Reply, "archived abc12345") {
		t.Errorf("got %q", res.Reply)
	}
	if !api.archived["abc12345"] {
		t.Error("session not archived")
	}
	if res.FocusSession != "" {
		t.Errorf("archive should not set focus, got %q", res.FocusSession)
	}

	if got := handle(t, a, "/unarchive abc"); !strings.Contains(got, "unarchived abc12345") {
		t.Errorf("got %q", got)
	}
	if api.archived["abc12345"] {
		t.Error("session still archived")
	}
}

func TestRule_AutoApprove(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "deploy"}}
	a := NewRule(api)

	if got := handle(t, a, "/auto-approve abc on"); !strings.Contains(got, "auto-approve on") {
		t.Errorf("got %q", got)
	}
	if !api.autoApprove["abc12345"] {
		t.Error("auto-approve not enabled")
	}
	if got := handle(t, a, "/auto-approve abc off"); !strings.Contains(got, "auto-approve off") {
		t.Errorf("got %q", got)
	}
	if api.autoApprove["abc12345"] {
		t.Error("auto-approve not disabled")
	}
	if got := handle(t, a, "/auto-approve abc"); !strings.Contains(got, "usage") {
		t.Errorf("missing mode should show usage, got %q", got)
	}
}

func TestRule_ListShowsFlags(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "x", Cwd: "/tmp"}}
	api.autoApprove["abc12345"] = true
	api.archived["abc12345"] = true
	a := NewRule(api)
	out := handle(t, a, "/list")
	if !strings.Contains(out, "auto-approve") || !strings.Contains(out, "archived") {
		t.Errorf("list missing flags: %q", out)
	}
}

func TestRule_Read(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "x"}}
	api.transcripts["abc12345"] = []core.TranscriptTurn{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello back"},
	}
	a := NewRule(api)
	res := handleFull(t, a, "/read abc")
	if !strings.Contains(res.Reply, "hello back") {
		t.Errorf("got %q", res.Reply)
	}
	if res.FocusSession != "abc12345" {
		t.Errorf("read should set focus, got %q", res.FocusSession)
	}
}

func TestRule_ReadOffset(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "x"}}
	api.transcripts["abc12345"] = []core.TranscriptTurn{
		{Role: "user", Content: "turn0"},
		{Role: "assistant", Content: "turn1"},
		{Role: "user", Content: "turn2"},
	}
	a := NewRule(api)
	res := handleFull(t, a, "/read abc 1 0")
	if !strings.Contains(res.Reply, "turn0") || strings.Contains(res.Reply, "turn1") {
		t.Errorf("got %q, want only the turn at offset 0", res.Reply)
	}
}

func TestRule_Search(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345", Title: "x"}}
	api.transcripts["abc12345"] = []core.TranscriptTurn{
		{Role: "user", Content: "what's the migration status"},
		{Role: "assistant", Content: "unrelated"},
	}
	a := NewRule(api)
	res := handleFull(t, a, "/search --session abc migration")
	if !strings.Contains(res.Reply, "migration status") {
		t.Errorf("got %q", res.Reply)
	}
	if res.FocusSession != "abc12345" {
		t.Errorf("search should set focus, got %q", res.FocusSession)
	}
	if got := handle(t, a, "/search --session abc nomatch"); !strings.Contains(got, "no matches") {
		t.Errorf("got %q", got)
	}
	global := handleFull(t, a, "/search migration")
	if !strings.Contains(global.Reply, "abc12345") || global.FocusSession != "" {
		t.Errorf("global search = %+v", global)
	}
}

func TestRule_New(t *testing.T) {
	api := newFakeAPI()
	api.createNewID = "new-uuid-1234"
	api.createReply = "ready"
	a := NewRule(api)
	res := handleFull(t, a, "/new /tmp build me a thing")
	if !strings.Contains(res.Reply, "new-uuid") || !strings.Contains(res.Reply, "ready") {
		t.Errorf("got %q", res.Reply)
	}
	if res.FocusSession != "new-uuid-1234" {
		t.Errorf("focus = %q", res.FocusSession)
	}
	if len(api.created) != 1 || api.created[0].Cwd != "/tmp" || api.created[0].Msg != "build me a thing" {
		t.Errorf("created = %+v", api.created)
	}
	if got := handle(t, a, "/new /tmp"); !strings.Contains(got, "usage") {
		t.Errorf("missing message should show usage, got %q", got)
	}
}

func TestRuleSendRelaysReply(t *testing.T) {
	api := newFakeAPI()
	api.sessions = []core.Session{{ID: "abc12345-0000", Title: "deploy"}}
	api.waitReplies["abc12345-0000"] = "session reply text"
	a := NewRule(api)

	var relayed []string
	relay := func(sessionID, reply string, err error) {
		relayed = append(relayed, sessionID+"|"+reply)
	}
	res, err := a.Handle(context.Background(), nil, "", "/send abc build it", relay)
	if err != nil {
		t.Fatal(err)
	}
	if len(api.sentTo) != 1 || api.sentTo[0] != "abc12345-0000" {
		t.Errorf("sentTo = %v", api.sentTo)
	}
	if len(relayed) != 1 || relayed[0] != "abc12345-0000|session reply text" {
		t.Errorf("relay sink got %v", relayed)
	}
	if !strings.Contains(res.Reply, "sent to") {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.FocusSession != "abc12345-0000" {
		t.Errorf("FocusSession = %q", res.FocusSession)
	}
}
