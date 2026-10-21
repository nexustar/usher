package codex

import (
	"context"
	"strings"
	"testing"
)

func TestSteerNeedsARunningTurn(t *testing.T) {
	c := New("unused", nil, nil, nil, nil, nil, nil)
	if err := c.Steer(context.Background(), "thread-1", "Yes"); err == nil {
		t.Fatal("steer without a running turn succeeded")
	}
}

// Steer names the turn app-server announced, so a turn that ended in between
// is rejected rather than steering its successor.
func TestSteerNamesTheRunningTurn(t *testing.T) {
	out := new(testWriteCloser)
	c := New("unused", nil, nil, nil, nil, nil, nil)
	c.in = out
	c.active["thread-1"] = "turn-7"
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the request is written before the reply is awaited
	if err := c.Steer(ctx, "thread-1", "Yes"); err == nil {
		t.Fatal("steer returned without a reply")
	}
	got := out.String()
	for _, want := range []string{`"method":"turn/steer"`, `"expectedTurnId":"turn-7"`, `"threadId":"thread-1"`, `"text":"Yes"`} {
		if !strings.Contains(got, want) {
			t.Errorf("request %s lacks %s", got, want)
		}
	}
}
