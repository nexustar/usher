package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/pkg/interaction"
)

// Interrupt requests must include the running turn ID.
func TestInterruptCarriesTheRunningTurnID(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.log")
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> "` + logPath + `"
  case "$line" in
    *'"initialize"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake/1"}}' ;;
    *'"turn/interrupt"'*) printf '%s\n' '{"jsonrpc":"2.0","id":9,"result":{}}' ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(script, interaction.New(), nil, nil, nil, nil, nil)
	t.Cleanup(c.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	c.dispatch(rpcMessage{Method: "turn/started", Params: json.RawMessage(`{"threadId":"t1","turn":{"id":"turn-abc"}}`)})

	// The fake uses a fixed response ID, so call asynchronously.
	go func() { _ = c.Interrupt(ctx, "t1") }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(logPath)
		if strings.Contains(string(b), `"turn/interrupt"`) {
			if !strings.Contains(string(b), `"turnId":"turn-abc"`) {
				t.Fatalf("interrupt did not carry the running turn id:\n%s", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no turn/interrupt was sent:\n%s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Before turn/started, no active turn ID is known.
func TestInterruptBeforeTurnStartedSendsEmptyTurnID(t *testing.T) {
	c := New("unused", nil, nil, nil, nil, nil, nil)
	c.turns["t1"] = &turnStream{done: make(chan TurnResult, 1), deltas: make(chan Delta, 1)}
	if id, ok := c.active["t1"]; ok || id != "" {
		t.Fatalf("active turn id before turn/started = %q, %v; want empty", id, ok)
	}
}

// Completion must clear the tracked turn ID.
func TestTurnCompletedClearsTrackedTurnID(t *testing.T) {
	c := New("unused", nil, nil, nil, nil, nil, nil)
	c.dispatch(rpcMessage{Method: "turn/started", Params: json.RawMessage(`{"threadId":"t1","turn":{"id":"turn-abc"}}`)})
	if c.active["t1"] != "turn-abc" {
		t.Fatalf("tracked turn id = %q, want turn-abc", c.active["t1"])
	}
	c.dispatch(rpcMessage{Method: "turn/completed", Params: json.RawMessage(`{"threadId":"t1","turn":{"id":"turn-abc","status":"interrupted"}}`)})
	if _, ok := c.active["t1"]; ok {
		t.Fatal("turn/completed left a stale turn id behind")
	}
}

func TestResumeGoalSetsItActive(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "requests.log")
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> "` + logPath + `"
  case "$line" in
    *'"initialize"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"userAgent":"fake/1"}}' ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(script, interaction.New(), nil, nil, nil, nil, nil)
	t.Cleanup(c.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	go func() { _ = c.ResumeGoal(ctx, "t1") }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(logPath)
		if strings.Contains(string(b), `"thread/goal/set"`) {
			for _, want := range []string{`"status":"active"`, `"origin":"user"`} {
				if !strings.Contains(string(b), want) {
					t.Fatalf("missing %s:\n%s", want, b)
				}
			}
			if strings.Contains(string(b), `"objective"`) {
				t.Fatalf("resume changed the objective:\n%s", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no thread/goal/set was sent:\n%s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGoalStatusAsksAppServer(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codex")
	body := `#!/bin/sh
while IFS= read -r line; do
  id=$(printf '%s\n' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
  case "$line" in
    *'"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"fake/1"}}\n' "$id" ;;
    *'"threadId":"stopped"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"goal":{"objective":"x","status":"usageLimited"}}}\n' "$id" ;;
    *'"thread/goal/get"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"goal":null}}\n' "$id" ;;
  esac
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(script, interaction.New(), nil, nil, nil, nil, nil)
	t.Cleanup(c.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	for id, want := range map[string]string{"stopped": "usageLimited", "plain": ""} {
		if got, err := c.GoalStatus(ctx, id); err != nil || got != want {
			t.Errorf("GoalStatus(%s) = %q, %v; want %q", id, got, err, want)
		}
	}
}
