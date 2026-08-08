package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/sender"
	"github.com/nexustar/usher/internal/sessionmeta"
	"github.com/nexustar/usher/internal/transcript"
)

// TestPublishStreamDerivesCodexTurns proves the live path: fed Codex rollout
// lines through the codex assembler, publishStream derives the backend-neutral
// turn.user / part broker events the web client renders (same as for Claude).
func TestPublishStreamDerivesCodexTurns(t *testing.T) {
	b := broker.New()
	r := &Router{broker: b}
	sub, unsub := b.Subscribe("s1")
	defer unsub()

	asm := transcript.Codex{}.NewAssembler()
	lines := []string{
		`{"timestamp":"2026-06-14T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}`,
		`{"timestamp":"2026-06-14T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"hi there"}}`,
		`{"timestamp":"2026-06-14T00:00:09Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`,
	}
	go func() {
		for _, ln := range lines {
			r.publishStream("s1", asm, sender.StreamEvent{Type: "event_msg", Raw: json.RawMessage(ln)}, time.Time{})
		}
		b.Publish(broker.Event{SessionID: "s1", Type: "done"})
	}()

	var sawUser, sawPart bool
	for ev := range sub {
		switch {
		case ev.Type == "turn.user" && strings.Contains(string(ev.Raw), "hello codex"):
			sawUser = true
		case ev.Type == "part" && strings.Contains(string(ev.Raw), "hi there"):
			sawPart = true
		case ev.Type == "done":
			if !sawUser {
				t.Error("no turn.user derived from codex user_message")
			}
			if !sawPart {
				t.Error("no part derived from codex agent_message")
			}
			return
		}
	}
}

func TestFailSendPublishesTerminalExit(t *testing.T) {
	b := broker.New()
	tok := &sendToken{}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	r := &Router{
		discovery:   d,
		broker:      b,
		activeSend:  map[string]*sendToken{"s1": tok},
		foreignBase: map[string]int64{},
	}
	sub, unsub := b.Subscribe("s1")
	defer unsub()

	r.failSend("s1", tok, errors.New("unknown command: /wtf"))
	first, second := <-sub, <-sub
	if first.Type != backend.EventError || !strings.Contains(string(first.Raw), "unknown command: /wtf") {
		t.Fatalf("first event = %s %s, want command error", first.Type, first.Raw)
	}
	if second.Type != backend.EventProcessExit || !strings.Contains(string(second.Raw), `"reason":"error"`) {
		t.Fatalf("second event = %s %s, want terminal error exit", second.Type, second.Raw)
	}
	if _, active := r.activeSend["s1"]; active {
		t.Fatal("send remained active after startup error")
	}
}

const codexLog = `{"timestamp":"2026-06-14T00:00:00Z","type":"session_meta","payload":{"id":"id1","cwd":"/c","timestamp":"2026-06-14T00:00:00Z"}}
{"timestamp":"2026-06-14T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello codex"}}
{"timestamp":"2026-06-14T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"hi"}}
{"timestamp":"2026-06-14T00:00:09Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
`

const claudeLog = `{"type":"user","message":{"role":"user","content":"hello claude"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}
{"type":"system","subtype":"turn_duration"}
`

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type recordingRenamer struct {
	id, path, title string
	err             error
}

type testForker struct {
	dstPath string
	newID   string
}

func (f testForker) Fork(_ context.Context, _, _, _ string) (string, string, error) {
	if err := os.MkdirAll(filepath.Dir(f.dstPath), 0o755); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(f.dstPath, []byte(claudeLog), 0o600); err != nil {
		return "", "", err
	}
	return f.newID, f.dstPath, nil
}

func TestForkSessionInheritsAppendSystemPrompt(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "-tmp")
	srcPath := filepath.Join(projectDir, "source.jsonl")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, []byte(claudeLog), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	d.Upsert(srcPath)
	metaPath := filepath.Join(t.TempDir(), "sessions.json")
	meta := sessionmeta.New(metaPath, 0)
	meta.SetAppendSystemPrompt("source", "Persisted prompt.")
	forker := testForker{
		dstPath: filepath.Join(projectDir, "branch.jsonl"),
		newID:   "branch",
	}
	r := New(d, map[string]backend.Backend{
		"claude": {Forker: forker},
	}, "claude", nil, nil, meta, nil)

	newID, err := r.ForkSession("source", "turn")
	if err != nil {
		t.Fatal(err)
	}
	if newID != "branch" {
		t.Fatalf("new id = %q", newID)
	}
	if got := meta.AppendSystemPrompt("branch"); got != "Persisted prompt." {
		t.Fatalf("fork prompt = %q", got)
	}
	reloaded := sessionmeta.New(metaPath, 0)
	if got := reloaded.AppendSystemPrompt("branch"); got != "Persisted prompt." {
		t.Fatalf("persisted fork prompt = %q", got)
	}
}

// A branch starts without auto-approve even though it inherits the system prompt.
func TestForkSessionDoesNotInheritAutoApprove(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "-tmp")
	srcPath := filepath.Join(projectDir, "source.jsonl")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, []byte(claudeLog), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	d.Upsert(srcPath)
	h := interaction.New("")
	h.SetAutoApprove("source", true)
	meta := sessionmeta.New(filepath.Join(t.TempDir(), "sessions.json"), 0)
	r := New(d, map[string]backend.Backend{
		"claude": {Forker: testForker{
			dstPath: filepath.Join(projectDir, "branch.jsonl"),
			newID:   "branch",
		}},
	}, "claude", nil, h, meta, nil)

	if _, err := r.ForkSession("source", "turn"); err != nil {
		t.Fatal(err)
	}
	if h.IsAutoApprove("branch") {
		t.Error("branch inherited auto-approve from its source")
	}
	if !h.IsAutoApprove("source") {
		t.Error("forking cleared the source's auto-approve")
	}
}

func (r *recordingRenamer) Rename(_ context.Context, id, path, title string) error {
	r.id, r.path, r.title = id, path, title
	return r.err
}

func TestRenameMigratesUsherOverrideToNativeMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-tmp", "session-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"first prompt"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	d.Upsert(path)
	meta := sessionmeta.New(filepath.Join(t.TempDir(), "sessions.json"), 0)
	meta.Rename("session-1", "legacy override")
	native := &recordingRenamer{}
	r := New(d, map[string]backend.Backend{
		"claude": {Renamer: native},
	}, "claude", nil, nil, meta, nil)

	if err := r.Rename(context.Background(), "session-1", "native title"); err != nil {
		t.Fatal(err)
	}
	if native.id != "session-1" || native.path != path || native.title != "native title" {
		t.Fatalf("native rename = %+v", native)
	}
	if got := meta.CustomTitle("session-1"); got != "" {
		t.Fatalf("legacy override remains: %q", got)
	}
	sess, ok := d.Get("session-1")
	if !ok || sess.Title != "native title" {
		t.Fatalf("session after rename = %+v, ok=%v", sess, ok)
	}
	if err := r.Rename(context.Background(), "session-1", " \n "); !errors.Is(err, ErrInvalidTitle) {
		t.Fatalf("empty rename error = %v", err)
	}
	if native.title != "native title" {
		t.Fatalf("empty rename reached backend: %+v", native)
	}
}

// TestReadTurnsForBackend proves the dispatch: each backend's parser
// understands its own log shape and yields nothing from the other's.
func TestReadTurnsForBackend(t *testing.T) {
	codexPath := writeTemp(t, "rollout.jsonl", codexLog)
	claudePath := writeTemp(t, "claude.jsonl", claudeLog)

	turns, _, err := (transcript.Codex{}).ReadTurns(codexPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Role != "user" || turns[0].Content != "hello codex" {
		t.Fatalf("codex parser: got %+v", turns)
	}
	// The Claude parser must not understand a Codex rollout (event_msg lines are
	// not user/assistant) — proving the dispatch matters.
	if wrong, _, _ := (transcript.Claude{}).ReadTurns(codexPath, 0); len(wrong) != 0 {
		t.Errorf("claude parser should yield nothing from a codex log; got %+v", wrong)
	}

	turns, _, err = (transcript.Claude{}).ReadTurns(claudePath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) == 0 || turns[0].Content != "hello claude" {
		t.Fatalf("claude parser: got %+v", turns)
	}
	if wrong, _, _ := (transcript.Codex{}).ReadTurns(claudePath, 0); len(wrong) != 0 {
		t.Errorf("codex parser should yield nothing from a claude log; got %+v", wrong)
	}
}

// TestReadTurnsWindow proves window indices stay stable as a session grows:
// transcripts only gain turns at the tail, so `before` keeps naming the same
// turns after more arrive.
func TestReadTurnsWindow(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "-tmp")
	logPath := filepath.Join(projectDir, "win.jsonl")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(i int) string {
		return fmt.Sprintf("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"t%d\"}}\n", i)
	}
	var log strings.Builder
	for i := 0; i < 6; i++ {
		log.WriteString(line(i))
	}
	if err := os.WriteFile(logPath, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	d.Upsert(logPath)
	r := New(d, map[string]backend.Backend{
		"claude": {Transcript: transcript.Claude{}},
	}, "claude", nil, nil, nil, nil)

	contents := func(turns []core.Turn) []string {
		out := make([]string, 0, len(turns))
		for _, turn := range turns {
			out = append(out, turn.Content)
		}
		return out
	}
	check := func(name string, before, limit int, want []string, wantOffset, wantTotal int) {
		t.Helper()
		turns, offset, total, err := r.ReadTurns("win", before, limit)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := contents(turns); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: turns = %v, want %v", name, got, want)
		}
		if offset != wantOffset {
			t.Errorf("%s: offset = %d, want %d", name, offset, wantOffset)
		}
		if total != wantTotal {
			t.Errorf("%s: total = %d, want %d", name, total, wantTotal)
		}
	}

	check("newest page", -1, 2, []string{"t4", "t5"}, 4, 6)
	check("page before it", 4, 2, []string{"t2", "t3"}, 2, 6)

	log.WriteString(line(6))
	log.WriteString(line(7))
	if err := os.WriteFile(logPath, []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	check("same page after growth", 4, 2, []string{"t2", "t3"}, 2, 8)
	check("newest page slid", -1, 2, []string{"t6", "t7"}, 6, 8)

	check("clamped at front", 1, 5, []string{"t0"}, 0, 8)
	check("empty window at front", 0, 5, []string{}, 0, 8)
	check("before past the end", 99, 2, []string{"t6", "t7"}, 6, 8)
}

// TestReadTurnsAssistantEndTime proves an assistant turn's wire timestamp is
// when it finished — the same value the exit payload's assistant_end_ts carries.
func TestReadTurnsAssistantEndTime(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "-tmp")
	logPath := filepath.Join(projectDir, "end.jsonl")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","timestamp":"2026-04-26T10:00:00.000Z","message":{"role":"user","content":"run ls"}}`,
		`{"type":"assistant","timestamp":"2026-04-26T10:00:01.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","timestamp":"2026-04-26T10:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"file1.txt"}]}}`,
		`{"type":"assistant","timestamp":"2026-04-26T10:00:03.000Z","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	d.Upsert(logPath)
	r := New(d, map[string]backend.Backend{
		"claude": {Transcript: transcript.Claude{}},
	}, "claude", nil, nil, nil, nil)

	turns, _, _, err := r.ReadTurns("end", -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("got %d turns, want 2: %+v", len(turns), turns)
	}
	if want := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC); !turns[0].Time.Equal(want) {
		t.Errorf("user ts = %v, want %v", turns[0].Time, want)
	}
	if want := time.Date(2026, 4, 26, 10, 0, 3, 0, time.UTC); !turns[1].Time.Equal(want) {
		t.Errorf("assistant ts = %v, want %v (turn end)", turns[1].Time, want)
	}

	// Same projection through the agent-facing transcript readers.
	tt, err := r.ReadSessionTranscript("end", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tt) != 2 || !tt[1].Time.Equal(turns[1].Time) {
		t.Errorf("ReadSessionTranscript ts = %v, want %v", tt, turns[1].Time)
	}
	page, _, _, err := r.ReadSessionTranscriptPage("end", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || !page[1].Time.Equal(turns[1].Time) {
		t.Errorf("ReadSessionTranscriptPage ts = %v, want %v", page, turns[1].Time)
	}
}

func TestBackendForModel(t *testing.T) {
	cases := map[string]string{
		"gpt-5.5":           "codex",
		"gpt-4.1":           "codex",
		"o3":                "codex",
		"o4-mini":           "codex",
		"codex-mini":        "codex",
		"claude-opus-4-8":   "claude",
		"opus":              "claude",
		"sonnet":            "claude",
		"haiku":             "claude",
		"claude-fable-5":    "claude",
		"":                  "claude", // unspecified → default backend
		"default":           "claude", // ambiguous name resolves to the default backend
		"GPT-5.5":           "codex",  // case-insensitive
		"  gpt-5.5  ":       "codex",  // trimmed
		"something-unknown": "claude",
	}
	for model, want := range cases {
		if got := backendForModel(model); got != want {
			t.Errorf("backendForModel(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestSenderForBackendFallsBackToDefault(t *testing.T) {
	r := &Router{
		backends:       map[string]backend.Backend{"claude": {Transcript: transcript.Claude{}}},
		defaultBackend: "claude",
	}
	// An unregistered backend falls back to the default (here the claude entry).
	if _, ok := r.backends["codex"]; ok {
		t.Fatal("precondition: codex should be unregistered")
	}
	// senderForBackend returns the default entry for an unknown backend; we only
	// assert it does not panic and returns the same (nil) default value.
	if got := r.senderForBackend("codex"); got != r.backends["claude"].Runtime {
		t.Errorf("unknown backend did not fall back to default")
	}
	if got := r.senderForBackend("claude"); got != r.backends["claude"].Runtime {
		t.Errorf("registered backend not returned")
	}
}

type staticModels struct {
	models []backend.Model
}

type scriptedComposer struct {
	backend.Runtime
	catalogs []backend.ComposerCatalog
	errs     []error
	calls    int
}

func (s *scriptedComposer) ComposerItems(context.Context, string, string) (backend.ComposerCatalog, error) {
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return backend.ComposerCatalog{}, s.errs[i]
	}
	if i < len(s.catalogs) {
		return s.catalogs[i], nil
	}
	return backend.ComposerCatalog{}, nil
}

func TestComposerItemsFallsBackToLastAvailableCatalog(t *testing.T) {
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedComposer{catalogs: []backend.ComposerCatalog{
		{Items: []backend.ComposerItem{{Name: "my-skill", Kind: "skill"}, {Name: "review", Kind: "command"}, {Name: "compact", Kind: "command"}}, Available: true},
		{Items: []backend.ComposerItem{{Name: "partial"}}, Available: false},
	}}
	r := New(d, map[string]backend.Backend{"codex": {Runtime: provider}}, "codex", nil, nil, nil, nil)

	first, err := r.ComposerItems(context.Background(), "session", "/work")
	if err != nil || !first.Available || len(first.Items) != 3 || first.Items[0].Name != "compact" || first.Items[2].Name != "my-skill" {
		t.Fatalf("first catalog = (%+v, %v), want commands sorted before skills", first, err)
	}
	second, err := r.ComposerItems(context.Background(), "session", "/work")
	if err != nil || !second.Available || len(second.Items) != 3 || second.Items[0].Name != "compact" {
		t.Fatalf("cold fallback = (%+v, %v), want cached complete catalog", second, err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (cache must not bypass live query)", provider.calls)
	}

	missing, err := r.ComposerItems(context.Background(), "session", "/other")
	if err != nil || missing.Available || missing.Items != nil {
		t.Fatalf("uncached cwd = (%+v, %v), want unavailable empty catalog", missing, err)
	}
}

func (s staticModels) Models(context.Context) ([]backend.Model, error) {
	return s.models, nil
}

func (s staticModels) ValidateModel(_ context.Context, model string) error {
	for _, candidate := range s.models {
		if candidate.ID == model {
			return nil
		}
	}
	return errors.New("unknown model")
}

func (staticModels) DefaultEffort(context.Context, string) (string, error) {
	return "", nil
}

func TestTranscriptForBackendReturnsCapabilityError(t *testing.T) {
	r := &Router{backends: map[string]backend.Backend{
		"incomplete": {},
	}}

	if _, err := r.transcriptForBackend("incomplete"); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("transcriptForBackend error = %v, want ErrBackendUnavailable", err)
	}
	if _, err := r.transcriptForBackend("missing"); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("unknown backend error = %v, want ErrBackendUnavailable", err)
	}
}

func TestValidateModelFailsClosed(t *testing.T) {
	r := &Router{backends: map[string]backend.Backend{
		"catalogued":   {Models: staticModels{models: []backend.Model{{ID: "known"}}}},
		"uncatalogued": {},
	}}

	tests := []struct {
		name        string
		backendName string
		model       string
		wantErr     bool
	}{
		{name: "known model", backendName: "catalogued", model: "known"},
		{name: "cli default", backendName: "catalogued", model: "default"},
		{name: "empty means cli default", backendName: "uncatalogued"},
		{name: "unknown model", backendName: "catalogued", model: "unknown", wantErr: true},
		{name: "missing catalog", backendName: "uncatalogued", model: "anything", wantErr: true},
		{name: "missing backend explicit model", backendName: "missing", model: "anything", wantErr: true},
		{name: "missing backend default", backendName: "missing", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.ValidateModel(context.Background(), tt.backendName, tt.model)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateModel(%q, %q) error = %v, wantErr %v", tt.backendName, tt.model, err, tt.wantErr)
			}
		})
	}
}

func TestResolveCreateBackend(t *testing.T) {
	r := &Router{
		defaultBackend: "claude",
		backends: map[string]backend.Backend{
			"claude": {Models: staticModels{models: []backend.Model{{ID: "sonnet"}}}},
			"codex":  {Models: staticModels{models: []backend.Model{{ID: "gpt-5.5"}}}},
		},
	}

	tests := []struct {
		name, backendName, model, want string
		wantErr                        bool
	}{
		{name: "both omitted", want: "claude"},
		{name: "model inference", model: "gpt-5.5", want: "codex"},
		{name: "explicit backend default model", backendName: "codex", want: "codex"},
		{name: "explicit matching pair", backendName: "claude", model: "sonnet", want: "claude"},
		{name: "explicit mismatch", backendName: "claude", model: "gpt-5.5", wantErr: true},
		{name: "unknown backend", backendName: "pi", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.resolveCreateBackend(context.Background(), tt.backendName, tt.model)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("resolveCreateBackend(%q, %q) = %q, %v; want %q, wantErr %v", tt.backendName, tt.model, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// --- collectTurnText (the relayed/waited send's accumulate loop) ----------

func partEvent(sid, role, typ, content string) broker.Event {
	raw, _ := json.Marshal(map[string]any{
		"role": role,
		"part": map[string]string{"type": typ, "content": content},
	})
	return broker.Event{SessionID: sid, Type: "part", Raw: raw}
}

func TestCollectTurnTextAccumulatesUntilExit(t *testing.T) {
	b := broker.New()
	ch, unsub := b.Subscribe("s1")
	defer unsub()

	go func() {
		b.Publish(partEvent("s1", "assistant", "text", "hello"))
		b.Publish(partEvent("s1", "assistant", "tool", "ignored tool part"))
		b.Publish(partEvent("s1", "user", "text", "ignored user echo"))
		b.Publish(partEvent("s1", "assistant", "text", "world"))
		b.Publish(broker.Event{SessionID: "s1", Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
	}()

	got, err := collectTurnText(context.Background(), ch)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello\nworld" {
		t.Errorf("collected %q, want %q", got, "hello\nworld")
	}
}

func TestCollectTurnTextErrorEvent(t *testing.T) {
	b := broker.New()
	ch, unsub := b.Subscribe("s1")
	defer unsub()

	go func() {
		b.Publish(partEvent("s1", "assistant", "text", "partial"))
		b.Publish(broker.Event{SessionID: "s1", Type: "error", Raw: json.RawMessage(`{"message":"boom"}`)})
	}()

	got, err := collectTurnText(context.Background(), ch)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want boom", err)
	}
	if got != "partial" {
		t.Errorf("partial text lost: %q", got)
	}
}

func TestCollectTurnTextTimeout(t *testing.T) {
	b := broker.New()
	ch, unsub := b.Subscribe("s1")
	defer unsub()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired: no events will ever arrive

	if _, err := collectTurnText(ctx, ch); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want timeout", err)
	}
}

func TestSendToSessionRelayedUnknownSession(t *testing.T) {
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	r := &Router{discovery: d, broker: broker.New()}
	if err := r.SendToSessionRelayed("nope", "hi", func(string, string, error) {}); err == nil {
		t.Error("expected error for unknown session")
	}
}

// TestDeleteSubagentTranscripts: deleting a parent cascades to its read-only
// subagents — their files, the <cwd>/<id>/ subtree, and their discovery entries
// all go, leaving no orphans with a dangling parent.
func TestDeleteSubagentTranscripts(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "-tmp-proj")
	subDir := filepath.Join(cwd, "p1", "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(cwd, "p1.jsonl")
	rootLine := `{"type":"user","sessionId":"p1","cwd":"/tmp/proj","timestamp":"2026-07-01T10:00:00.000Z","message":{"role":"user","content":"seed"},"uuid":"u1"}` + "\n"
	if err := os.WriteFile(rootPath, []byte(rootLine), 0o644); err != nil {
		t.Fatal(err)
	}
	subLine := `{"type":"user","attributionAgent":"Explore","timestamp":"2026-07-01T10:00:01.000Z","message":{"role":"user","content":"task"},"uuid":"su"}` + "\n"
	for _, name := range []string{"agent-aaa.jsonl", "agent-bbb.jsonl"} {
		if err := os.WriteFile(filepath.Join(subDir, name), []byte(subLine), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(d.ListAll()); got != 3 {
		t.Fatalf("ListAll = %d, want 3 (root + 2 subagents)", got)
	}

	r := &Router{discovery: d}
	r.deleteSubagentTranscripts("p1", rootPath)

	if _, err := os.Stat(filepath.Join(cwd, "p1")); !os.IsNotExist(err) {
		t.Errorf("subagent subtree still on disk: err=%v", err)
	}
	for _, id := range []string{"p1::agent-aaa", "p1::agent-bbb"} {
		if _, ok := d.Get(id); ok {
			t.Errorf("subagent %q still in discovery", id)
		}
	}
	if _, ok := d.Get("p1"); !ok {
		t.Error("root should remain (the helper deletes children only)")
	}
}

func TestDeleteSubagentTranscriptsRecursesCodexDescendants(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "07", "13")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rootID := "019f5000-0000-7000-8000-000000000001"
	childID := "019f5000-0000-7000-8000-000000000002"
	grandchildID := "019f5000-0000-7000-8000-000000000003"
	writeRollout := func(id, parent, source string) string {
		path := filepath.Join(dir, "rollout-2026-07-13T00-00-00-"+id+".jsonl")
		line := fmt.Sprintf(`{"timestamp":"2026-07-13T00:00:00Z","type":"session_meta","payload":{"id":%q,"cwd":"/tmp/project","thread_source":%q,"parent_thread_id":%q}}`, id, source, parent) + "\n"
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	rootPath := writeRollout(rootID, "", "user")
	childPath := writeRollout(childID, rootID, "subagent")
	grandchildPath := writeRollout(grandchildID, childID, "subagent")
	d, err := discovery.NewMulti(nil, discovery.NewCodexSource(root))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{discovery: d}
	r.deleteSubagentTranscripts(rootID, rootPath)
	for _, tc := range []struct{ id, path string }{{childID, childPath}, {grandchildID, grandchildPath}} {
		if _, err := os.Stat(tc.path); !os.IsNotExist(err) {
			t.Errorf("descendant %s still on disk: err=%v", tc.id, err)
		}
		if _, ok := d.Get(tc.id); ok {
			t.Errorf("descendant %s still in discovery", tc.id)
		}
	}
	if _, err := os.Stat(rootPath); err != nil {
		t.Fatalf("root transcript removed by child cleanup: %v", err)
	}
}

// --- send queue (per-session turn serialization) ---------------------------

// newQueueTestRouter builds a Router over a real discovery holding one session
// (id "abc12345"), with runTurn overridden to simulate each turn: publish one
// assistant part echoing the prompt, publish exit, release the send. That is
// exactly the event pattern a real turn produces, so the relay collectors'
// attribution can be asserted end to end without tmux.
func newQueueTestRouter(t *testing.T) *Router {
	t.Helper()
	tmp := t.TempDir()
	line := `{"type":"user","sessionId":"abc12345","cwd":"/tmp/x","timestamp":"2026-07-01T10:00:00.000Z","message":{"role":"user","content":"seed"},"uuid":"u1"}` + "\n"
	p := filepath.Join(tmp, "-tmp-x", "abc12345.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(tmp))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := d.Start(ctx); err != nil {
		t.Fatal(err)
	}
	r := &Router{
		discovery:      d,
		backends:       map[string]backend.Backend{"claude": {Transcript: transcript.Claude{}}},
		defaultBackend: "claude",
		broker:         broker.New(),
		activeSend:     map[string]*sendToken{},
		sendQueue:      map[string][]pendingSend{},
		creating:       map[string]core.Session{},
	}
	r.runTurn = func(_ context.Context, sessionID, prompt, _ string, tok *sendToken) {
		r.broker.Publish(partEvent(sessionID, "assistant", "text", "re: "+prompt))
		r.broker.Publish(broker.Event{SessionID: sessionID, Type: "subprocess.exit", Raw: json.RawMessage(`{}`)})
		r.releaseSend(sessionID, tok)
	}
	return r
}

// TestEnrichExitSkipsStaleExchange: a turn that logged nothing (idle-fallback
// exit for a TUI-local command, or a cancel before first output) must not pick
// up the PREVIOUS exchange's timestamps — IM mirrors would show its duration
// as the current turn's "responded in".
func TestEnrichExitSkipsStaleExchange(t *testing.T) {
	r := newQueueTestRouter(t)
	path, _ := r.discovery.Path("abc12345")
	appendFile(t, path, `{"type":"assistant","timestamp":"2026-07-01T10:00:08.000Z","message":{"role":"assistant","content":[{"type":"text","text":"prev reply"}]},"uuid":"uu-prev"}`+"\n")

	stamps := func(raw json.RawMessage) bool {
		var p struct {
			UserTS      time.Time `json:"user_ts"`
			AssistantTS time.Time `json:"assistant_ts"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatal(err)
		}
		return !p.UserTS.IsZero() && !p.AssistantTS.IsZero()
	}

	// Send began after the exchange completed → it is a previous turn's: skip.
	after := time.Date(2026, 7, 1, 10, 1, 0, 0, time.UTC)
	if stamps(r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), after)) {
		t.Error("stale exchange stamped onto an empty turn's exit")
	}
	// Send began before the exchange completed → it is this turn's: stamp.
	before := time.Date(2026, 7, 1, 10, 0, 1, 0, time.UTC)
	if !stamps(r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), before)) {
		t.Error("current turn's exchange not stamped")
	}
	// Zero started disables the check (first-turn paths).
	if !stamps(r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), time.Time{})) {
		t.Error("zero started should stamp unconditionally")
	}
}

func TestEnrichExitSkipsConsecutiveUsersAndZeroEndTime(t *testing.T) {
	r := newQueueTestRouter(t)
	path, _ := r.discovery.Path("abc12345")
	appendFile(t, path, `{"type":"user","timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"A"}}`+"\n")
	appendFile(t, path, `{"type":"user","timestamp":"2026-07-01T10:00:02Z","message":{"role":"user","content":"B"}}`+"\n")
	raw := r.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), time.Date(2026, 7, 1, 10, 0, 1, 500000000, time.UTC))
	if string(raw) != `{}` {
		t.Fatalf("consecutive-user exit was enriched: %s", raw)
	}

	r2 := newQueueTestRouter(t)
	path2, _ := r2.discovery.Path("abc12345")
	appendFile(t, path2, `{"type":"user","timestamp":"2026-07-01T10:00:01Z","message":{"role":"user","content":"current"}}`+"\n")
	appendFile(t, path2, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"undated"}]}}`+"\n")
	raw = r2.enrichExitWithTurnTimestamps("abc12345", json.RawMessage(`{}`), time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
	if string(raw) != `{}` {
		t.Fatalf("zero-time assistant exit was enriched: %s", raw)
	}
}

// TestSendQueueSerializesAndAttributesReplies: three rapid relayed sends into
// one session must run as three ordered turns, each collector receiving ITS
// OWN turn's reply — not the previous turn's tail (the busy-session
// misattribution bug).
func TestSendQueueSerializesAndAttributesReplies(t *testing.T) {
	r := newQueueTestRouter(t)

	type got struct{ prompt, reply string }
	results := make(chan got, 3)
	for _, prompt := range []string{"one", "two", "three"} {
		p := prompt
		err := r.SendToSessionRelayed("abc12345", p, func(_, reply string, err error) {
			if err != nil {
				t.Errorf("send %q relay err: %v", p, err)
			}
			results <- got{p, reply}
		})
		if err != nil {
			t.Fatalf("send %q: %v", p, err)
		}
	}

	for i := 0; i < 3; i++ {
		select {
		case g := <-results:
			if g.reply != "re: "+g.prompt {
				t.Errorf("prompt %q got reply %q — misattributed turn", g.prompt, g.reply)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for relayed replies")
		}
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if len(r.sendQueue["abc12345"]) != 0 || len(r.activeSend) != 0 {
		t.Errorf("queue not drained: %+v %+v", r.sendQueue, r.activeSend)
	}
}

func TestFlushSendQueueAborts(t *testing.T) {
	r := newQueueTestRouter(t)
	r.sendMu.Lock()
	r.activeSend["abc12345"] = &sendToken{cancel: func() {}}
	r.sendMu.Unlock()

	aborted := make(chan error, 1)
	if err := r.enqueueSend("abc12345", "queued", nil, func(err error) { aborted <- err }); err != nil {
		t.Fatal(err)
	}
	r.flushSendQueue("abc12345", errors.New("cancelled"))
	select {
	case err := <-aborted:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Errorf("abort err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort callback never ran")
	}
}

type blockingInterruptRuntime struct {
	backend.Runtime
	entered chan struct{}
	release chan struct{}
}

func (b *blockingInterruptRuntime) Interrupt(string) error {
	close(b.entered)
	<-b.release
	return nil
}

func TestCancelSendCancelsCollectorBeforeBackendInterruptReturns(t *testing.T) {
	r := newQueueTestRouter(t)
	rt := &blockingInterruptRuntime{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	entry := r.backends["claude"]
	entry.Runtime = rt
	r.backends["claude"] = entry

	ctx, cancel := context.WithCancel(context.Background())
	r.sendMu.Lock()
	r.activeSend["abc12345"] = &sendToken{cancel: cancel}
	r.sendMu.Unlock()

	done := make(chan error, 1)
	go func() { done <- r.CancelSend("abc12345") }()
	<-rt.entered
	select {
	case <-ctx.Done():
		// Local stream cancellation must not wait for the backend RPC.
	default:
		t.Fatal("collector still active while backend interrupt is blocked")
	}
	close(rt.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestValidateCreateInputsResolvesSymlinks: a cwd reached through a symlink
// (macOS /tmp → /private/tmp) must come back resolved, or Codex id discovery
// never matches the rollout's recorded cwd.
func TestValidateCreateInputsResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := validateCreateInputs(link, "hi")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("cwd = %q, want %q", got, want)
	}
}
