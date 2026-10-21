package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/transcript"
)

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

const foreignTurn = `{"type":"user","message":{"role":"user","content":"<task-notification>workflow done</task-notification>"},"timestamp":"2026-06-25T03:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"the research report"}]},"uuid":"uu-fork","timestamp":"2026-06-25T03:00:08Z"}
`

// TestForeignTurnDetectedAndRelayedOnce: a turn the session ran on its own
// (background continuation) is detected at its end-of-turn marker, relayed
// exactly once with the assistant's text, and announced on the broker so an
// open detail view refetches.
func TestForeignTurnDetectedAndRelayedOnce(t *testing.T) {
	r := newQueueTestRouter(t)
	var got []string
	r.SetForeignTurnHandler(func(id, text string) { got = append(got, id+"|"+text) })

	// First sight baselines the pre-existing log; history is never relayed.
	r.scanForeignTurns()
	if len(got) != 0 {
		t.Fatalf("history relayed: %v", got)
	}

	path, ok := r.discovery.Path("abc12345")
	if !ok {
		t.Fatal("no path for session")
	}
	appendFile(t, path, foreignTurn)
	r.scanForeignTurns() // no marker yet: turn still open
	if len(got) != 0 {
		t.Fatalf("mid-turn content relayed: %v", got)
	}

	ch, unsub := r.broker.Subscribe("abc12345")
	defer unsub()
	appendFile(t, path, `{"type":"system","subtype":"turn_duration"}`+"\n")
	r.scanForeignTurns()
	if len(got) != 1 || got[0] != "abc12345|the research report" {
		t.Fatalf("relay = %v", got)
	}
	select {
	case ev := <-ch:
		if ev.Type != "turn.prompt" {
			t.Errorf("broker event 1 = %q, want turn.prompt", ev.Type)
		}
	default:
		t.Fatal("no foreign user event published")
	}
	select {
	case ev := <-ch:
		if ev.Type != "turn.part" {
			t.Errorf("broker event 2 = %q, want part", ev.Type)
		}
	default:
		t.Fatal("no foreign assistant part event published")
	}
	select {
	case ev := <-ch:
		if ev.Type != "turn.ended" {
			t.Errorf("broker event 3 = %q, want turn.ended", ev.Type)
		}
		var p struct {
			UserTS        time.Time `json:"user_ts"`
			AssistantTS   time.Time `json:"assistant_ts"`
			AssistantUUID string    `json:"assistant_uuid"`
		}
		if err := json.Unmarshal(ev.Raw, &p); err != nil {
			t.Fatalf("exit payload: %v", err)
		}
		if p.UserTS.IsZero() || p.AssistantTS.IsZero() || p.AssistantUUID != "uu-fork" {
			t.Errorf("exit payload missing turn stamps: %s", ev.Raw)
		}
	default:
		t.Error("no synthetic exit published for the detail view")
	}

	r.scanForeignTurns() // baseline advanced: no re-relay
	if len(got) != 1 {
		t.Errorf("double relay: %v", got)
	}
}

func TestSubagentCompletionOnlyWakesDetailView(t *testing.T) {
	r := newQueueTestRouter(t)
	var relayed []string
	r.SetForeignTurnHandler(func(id, text string) { relayed = append(relayed, id+"|"+text) })

	rootPath, _ := r.discovery.Path("abc12345")
	childPath := filepath.Join(filepath.Dir(rootPath), "abc12345", "subagents", "agent-child.jsonl")
	if err := os.MkdirAll(filepath.Dir(childPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(`{"type":"user","message":{"role":"user","content":"child task"}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.discovery.Upsert(childPath)
	r.scanForeignTurns() // baseline the in-progress child transcript

	const childID = "abc12345::agent-child"
	ch, unsub := r.broker.Subscribe(childID)
	defer unsub()
	appendFile(t, childPath, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"child result"}]}}
{"type":"system","subtype":"turn_duration"}
`)
	r.scanForeignTurns()

	select {
	case ev := <-ch:
		if ev.Type != "turn.ended" {
			t.Fatalf("subagent event = %q, want only turn.ended", ev.Type)
		}
	default:
		t.Fatal("subagent completion did not wake detail view")
	}
	select {
	case ev := <-ch:
		t.Fatalf("unexpected extra subagent event: %q", ev.Type)
	default:
	}
	if len(relayed) != 0 {
		t.Fatalf("subagent result relayed as foreign turn: %v", relayed)
	}
}

// TestForeignWatchIgnoresUsherTurns: turns usher itself ran (activeSend +
// baseline bump at turn end) never reach the foreign handler, while a real
// foreign turn afterwards still does.
func TestForeignWatchIgnoresUsherTurns(t *testing.T) {
	r := newQueueTestRouter(t)
	var got []string
	r.SetForeignTurnHandler(func(id, text string) { got = append(got, text) })
	r.scanForeignTurns() // baseline

	path, _ := r.discovery.Path("abc12345")
	tok := &sendToken{cancel: func() {}}
	r.sendMu.Lock()
	r.activeSend["abc12345"] = tok
	r.sendMu.Unlock()

	appendFile(t, path, `{"type":"user","message":{"role":"user","content":"usher prompt"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"usher-relayed answer"}]}}
{"type":"system","subtype":"turn_duration"}
`)
	r.scanForeignTurns() // in-flight usher turn: suppressed
	if len(got) != 0 {
		t.Fatalf("usher turn relayed as foreign: %v", got)
	}
	r.markSendIdle("abc12345", tok)
	r.releaseSend("abc12345", tok)
	r.scanForeignTurns() // baseline was bumped at turn end
	if len(got) != 0 {
		t.Fatalf("usher turn re-relayed after release: %v", got)
	}

	appendFile(t, path, foreignTurn+`{"type":"system","subtype":"turn_duration"}`+"\n")
	r.scanForeignTurns()
	if len(got) != 1 || got[0] != "the research report" {
		t.Errorf("foreign turn after usher turn: %v", got)
	}
}

func TestTurnCompleteMarker(t *testing.T) {
	cases := []struct {
		backend string
		line    string
		want    bool
	}{
		{"claude", `{"type":"system","subtype":"turn_duration","durationMs":12}`, true},
		{"claude", `{"type":"assistant","message":{}}`, false},
		{"claude", `{"type":"assistant","message":{"stop_reason":"end_turn"}}` + "\n" + `{"type":"system","subtype":"stop_hook_summary"}` + "\n" + `{"type":"cost-state"}`, true},
		{"claude", `{"type":"assistant","message":{"stop_reason":"tool_use"}}` + "\n" + `{"type":"last-prompt"}`, false},
		{"claude", `{"type":"assistant","message":{"stop_reason":"end_turn"}}` + "\n" + `{"type":"user","message":{"role":"user","content":"next"}}`, false},
		{"codex", `{"timestamp":"t","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`, true},
		{"codex", `{"type":"event_msg","payload":{"type":"turn_complete","turn_id":"t1"}}`, true}, // announced v2 rename
		{"codex", `{"type":"event_msg","payload":{"type":"agent_message"}}`, false},
		{"claude", `not json`, false},
	}
	for _, c := range cases {
		var format backend.Transcript = transcript.Claude{}
		if c.backend == "codex" {
			format = transcript.Codex{}
		}
		if got := format.TurnEnded([]byte(c.line)); got != c.want {
			t.Errorf("turnCompleteMarker(%s, %s) = %v, want %v", c.backend, c.line, got, c.want)
		}
	}
}

// TestForeignTurnsBatchedInOneIntervalAllRelayed: two foreign turns
// completing between polls (chained background continuations) must BOTH be
// relayed, in order — the baseline jump may not swallow the earlier one.
func TestForeignTurnsBatchedInOneIntervalAllRelayed(t *testing.T) {
	r := newQueueTestRouter(t)
	var got []string
	r.SetForeignTurnHandler(func(_, text string) { got = append(got, text) })
	r.scanForeignTurns() // baseline

	path, _ := r.discovery.Path("abc12345")
	appendFile(t, path, `{"type":"user","message":{"role":"user","content":"first notification"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"first result"}]}}
{"type":"system","subtype":"turn_duration"}
{"type":"user","message":{"role":"user","content":"second notification"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"second result"}]}}
{"type":"system","subtype":"turn_duration"}
`)
	r.scanForeignTurns()
	if len(got) != 2 || got[0] != "first result" || got[1] != "second result" {
		t.Fatalf("relayed %v, want both results in order", got)
	}
	r.scanForeignTurns()
	if len(got) != 2 {
		t.Errorf("re-relayed after baseline: %v", got)
	}
}
