package codexrollout

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// a two-turn rollout: header + (user/agent/task_complete t1) + (user/agent/task_complete t2).
const twoTurnRollout = `{"timestamp":"2026-06-14T00:00:00Z","type":"session_meta","payload":{"id":"src-id","cwd":"/proj","cli_version":"0.139.0","timestamp":"2026-06-14T00:00:00Z"}}
{"timestamp":"2026-06-14T00:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"first question"}}
{"timestamp":"2026-06-14T00:00:02Z","type":"event_msg","payload":{"type":"agent_message","message":"first answer"}}
{"timestamp":"2026-06-14T00:00:03Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}
{"timestamp":"2026-06-14T00:00:04Z","type":"event_msg","payload":{"type":"user_message","message":"second question"}}
{"timestamp":"2026-06-14T00:00:05Z","type":"event_msg","payload":{"type":"agent_message","message":"second answer"}}
{"timestamp":"2026-06-14T00:00:06Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t2"}}
`

func TestTurnUUIDIsTurnID(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.jsonl")
	if err := os.WriteFile(p, []byte(twoTurnRollout), 0o644); err != nil {
		t.Fatal(err)
	}
	turns, _, err := ReadTurns(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	// turns: user, assistant(t1), user, assistant(t2)
	if len(turns) != 4 {
		t.Fatalf("got %d turns, want 4", len(turns))
	}
	if turns[1].Role != "assistant" || turns[1].UUID != "t1" {
		t.Errorf("turn[1] UUID = %q, want t1", turns[1].UUID)
	}
	if turns[3].UUID != "t2" {
		t.Errorf("turn[3] UUID = %q, want t2", turns[3].UUID)
	}
}

func TestForkCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")
	if err := os.WriteFile(src, []byte(twoTurnRollout), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, RolloutFilename("aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", time.Unix(0, 0)))

	if err := ForkCopy(src, dst, "t1", "aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", "src-id"); err != nil {
		t.Fatal(err)
	}

	// The fork keeps only turn 1.
	turns, _, err := ReadTurns(dst, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("forked turns = %d, want 2 (only the first exchange); %+v", len(turns), turns)
	}
	if turns[0].Content != "first question" {
		t.Errorf("turn0 = %q", turns[0].Content)
	}
	for _, tn := range turns {
		for _, p := range tn.Parts {
			if strings.Contains(p.Content, "second") {
				t.Errorf("turn 2 content leaked into the fork: %q", p.Content)
			}
		}
	}

	// Header rewritten: new id + forked_from_id, cwd preserved.
	first, _ := os.ReadFile(dst)
	header := strings.SplitN(string(first), "\n", 2)[0]
	var top struct {
		Type    string `json:"type"`
		Payload struct {
			ID           string `json:"id"`
			ForkedFromID string `json:"forked_from_id"`
			Cwd          string `json:"cwd"`
			CliVersion   string `json:"cli_version"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(header), &top); err != nil {
		t.Fatal(err)
	}
	if top.Type != "session_meta" {
		t.Errorf("header type = %q", top.Type)
	}
	if top.Payload.ID != "aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("forked id = %q, want new-id", top.Payload.ID)
	}
	if top.Payload.ForkedFromID != "src-id" {
		t.Errorf("forked_from_id = %q, want src-id", top.Payload.ForkedFromID)
	}
	if top.Payload.Cwd != "/proj" || top.Payload.CliVersion != "0.139.0" {
		t.Errorf("header fields not preserved: cwd=%q cli=%q", top.Payload.Cwd, top.Payload.CliVersion)
	}

	// SessionIDFromPath finds the new id from the fork filename (discovery relies
	// on this), and the fork's own meta agrees.
	if got := SessionIDFromPath(dst); got != "aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("SessionIDFromPath(fork) = %q, want new-id", got)
	}
}

func TestForkCopy_UnknownTurnErrors(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")
	os.WriteFile(src, []byte(twoTurnRollout), 0o644)
	dst := filepath.Join(dir, "out.jsonl")
	if err := ForkCopy(src, dst, "nope", "aaaa1111-bbbb-cccc-dddd-eeeeeeeeeeee", "src-id"); err == nil {
		t.Error("expected an error forking at an unknown turn id")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("no fork file should be written when the turn isn't found")
	}
}
