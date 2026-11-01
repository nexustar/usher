package codex

import (
	"encoding/json"
	"testing"
	"time"
)

func finishLimited(t *testing.T, c *Client, notes ...string) TurnResult {
	t.Helper()
	stream := &turnStream{done: make(chan TurnResult, 1), deltas: make(chan Delta, 1)}
	c.turns["thread-1"] = stream
	for _, n := range notes {
		var m rpcMessage
		if err := json.Unmarshal([]byte(n), &m); err != nil {
			t.Fatal(err)
		}
		c.dispatch(m)
	}
	select {
	case r := <-stream.done:
		return r
	default:
		t.Fatal("turn did not finish")
		return TurnResult{}
	}
}

func TestUsageLimitResetFromRateLimitSnapshot(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Unix()
	c := New("codex", nil, nil, nil, nil, nil, nil)
	snap, _ := json.Marshal(map[string]any{"rateLimits": map[string]any{
		"limitId":   "codex",
		"primary":   map[string]any{"usedPercent": 100, "windowDurationMins": 300, "resetsAt": reset},
		"secondary": map[string]any{"usedPercent": 40, "windowDurationMins": 10080, "resetsAt": reset + 86400},
	}})
	r := finishLimited(t, c,
		`{"method":"account/rateLimits/updated","params":`+string(snap)+`}`,
		`{"method":"error","params":{"threadId":"thread-1","turnId":"t","willRetry":false,"error":{"message":"You’ve hit your usage limit. Try again at 3:04 PM.","codexErrorInfo":"usageLimitExceeded"}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"t","status":"failed"}}}`,
	)
	if r.LimitResetsAt != reset {
		t.Fatalf("LimitResetsAt = %d, want %d", r.LimitResetsAt, reset)
	}
}

func TestUsageLimitResetFromMessageText(t *testing.T) {
	c := New("codex", nil, nil, nil, nil, nil, nil)
	r := finishLimited(t, c,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"t","status":"failed","error":{"message":"You’ve hit your usage limit. Try again at Jan 2nd, 2099 3:04 PM.","codexErrorInfo":"usageLimitExceeded"}}}}`,
	)
	want := time.Date(2099, 1, 2, 15, 5, 0, 0, time.Local).Unix()
	if r.LimitResetsAt != want {
		t.Fatalf("LimitResetsAt = %d, want %d", r.LimitResetsAt, want)
	}
}

func TestOtherErrorsCarryNoLimit(t *testing.T) {
	c := New("codex", nil, nil, nil, nil, nil, nil)
	r := finishLimited(t, c,
		`{"method":"error","params":{"threadId":"thread-1","willRetry":false,"error":{"message":"Try again at 3:04 PM.","codexErrorInfo":{"httpConnectionFailed":{"httpStatusCode":502}}}}}`,
		`{"method":"turn/completed","params":{"threadId":"thread-1","turn":{"id":"t","status":"failed"}}}`,
	)
	if r.LimitResetsAt != 0 {
		t.Fatalf("LimitResetsAt = %d", r.LimitResetsAt)
	}
}

func TestParseRetryAt(t *testing.T) {
	loc := time.FixedZone("X", 8*3600)
	now := time.Date(2026, 10, 24, 9, 0, 0, 0, loc)
	for msg, want := range map[string]time.Time{
		"You’ve hit your usage limit. Try again at 3:04 PM.":                    time.Date(2026, 10, 24, 15, 4, 0, 0, loc),
		"Switch to another model now, or try again at Oct 25th, 2026 11:30 AM.": time.Date(2026, 10, 25, 11, 30, 0, 0, loc),
		"You’ve hit your usage limit. Try again at Nov 1st, 2026 12:00 AM.":     time.Date(2026, 11, 1, 0, 0, 0, 0, loc),
	} {
		got, ok := parseRetryAt(msg, now)
		if !ok || !got.Equal(want) {
			t.Errorf("parseRetryAt(%q) = %v %v, want %v", msg, got, ok, want)
		}
	}
	if _, ok := parseRetryAt("You’ve hit your usage limit. Try again later.", now); ok {
		t.Error("later parsed as a time")
	}
}
