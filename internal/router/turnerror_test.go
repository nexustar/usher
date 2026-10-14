package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/discovery"
	"github.com/nexustar/usher/internal/transcript"
)

func TestTurnErrorEndsTranscript(t *testing.T) {
	root := t.TempDir()
	logPath := filepath.Join(root, "-tmp", "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	line := func(i int, ts time.Time) string {
		return fmt.Sprintf("{\"type\":\"user\",\"timestamp\":%q,\"message\":{\"role\":\"user\",\"content\":\"t%d\"}}\n",
			ts.UTC().Format(time.RFC3339Nano), i)
	}
	past := time.Now().Add(-time.Hour)
	log := line(0, past) + line(1, past.Add(time.Second))
	if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := discovery.NewMulti(nil, discovery.NewClaudeSource(root))
	if err != nil {
		t.Fatal(err)
	}
	d.Upsert(logPath)
	r := New(d, map[string]backend.Backend{"claude": {Transcript: transcript.Claude{}}}, "claude", broker.New(), nil, nil, nil)
	r.runTurn = func(context.Context, string, string, string, *sendToken) {}
	last := func() core.Turn {
		t.Helper()
		turns, _, err := r.ReadTurns("s1", "", 2)
		if err != nil || len(turns) != 2 {
			t.Fatalf("turns = %+v, err = %v", turns, err)
		}
		return turns[1]
	}

	r.noteTurnError("s1", "429", time.Time{})
	if got := last(); got.Role != "error" || got.Content != "429" {
		t.Fatalf("last turn = %+v, want the error", got)
	}
	if err := os.WriteFile(logPath, []byte(log+line(2, time.Now().Add(time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := last(); got.Role == "error" {
		t.Fatal("error kept after the log moved on")
	}
	if err := os.WriteFile(logPath, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	r.startTurn(context.Background(), "s1", "again", "/tmp", &sendToken{})
	if got := last(); got.Role == "error" {
		t.Fatal("error kept after the next send")
	}
}

func TestTurnErrorNotDoubled(t *testing.T) {
	r := &Router{}
	started := time.Now().Add(-time.Minute)
	r.noteTurnError("s1", "live", started)

	logged := []core.Turn{{Role: "error", Time: started.Add(time.Second)}}
	if got := r.withTurnError("s1", logged); len(got) != 1 {
		t.Errorf("this turn's logged error: turns = %+v, want it alone", got)
	}
	earlier := []core.Turn{{Role: "error", Time: started.Add(-time.Second)}}
	if got := r.withTurnError("s1", earlier); len(got) != 2 {
		t.Errorf("an earlier turn's logged error: turns = %+v, want the live error after it", got)
	}
}
