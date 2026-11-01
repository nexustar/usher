package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/sender"
	"github.com/nexustar/usher/pkg/settings"
)

func limitEvent(resetsAt time.Time) sender.StreamEvent {
	return sender.StreamEvent{Type: backend.EventTurnLimited, Raw: json.RawMessage(fmt.Sprintf(`{"resets_at":%d}`, resetsAt.Unix()))}
}

var turnEnded = sender.StreamEvent{Type: backend.EventTurnEnded, Raw: json.RawMessage(`{}`)}

func sessionLimit(t *testing.T, r *Router) *core.UsageLimit {
	t.Helper()
	sess := core.Session{ID: "abc12345"}
	r.applyUsageLimit(&sess)
	return sess.UsageLimit
}

func TestUsageLimitResumeSendsContinueAfterReset(t *testing.T) {
	r := newQueueTestRouter(t)
	r.limitJitter = func() time.Duration { return 0 }
	prompts := make(chan string, 4)
	limitAgain := true
	r.runTurn = func(_ context.Context, id, prompt, _ string, tok *sendToken) {
		prompts <- prompt
		if limitAgain {
			limitAgain = false
			r.publishStream(id, nil, limitEvent(time.Now().Add(20*time.Millisecond)), tok.started)
		}
		r.publishStream(id, nil, turnEnded, tok.started)
		r.releaseSend(id, tok)
	}

	if err := r.SetLimitResume("abc12345", true); !errors.Is(err, core.ErrNoUsageLimit) {
		t.Fatalf("arm without a limit: %v", err)
	}

	started := time.Now()
	reset := started.Add(50 * time.Millisecond)
	r.publishStream("abc12345", nil, limitEvent(reset), started)
	r.publishStream("abc12345", nil, turnEnded, started)
	l := sessionLimit(t, r)
	if l == nil || l.State != core.LimitOff || l.ResetsAt.Unix() != reset.Unix() {
		t.Fatalf("limit after refused turn = %+v", l)
	}

	if err := r.SetLimitResume("abc12345", true); err != nil {
		t.Fatal(err)
	}
	// The first resume hits the limit again and stays armed; the second
	// completes and clears it.
	for i := range 2 {
		select {
		case p := <-prompts:
			if p != limitResumePrompt {
				t.Fatalf("resume %d sent %q", i, p)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("resume %d never sent", i)
		}
	}
	deadline := time.Now().Add(time.Second)
	for sessionLimit(t, r) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("limit not cleared: %+v", sessionLimit(t, r))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestUsageLimitDisarmAndLaterTurnClear(t *testing.T) {
	r := newQueueTestRouter(t)
	r.limitJitter = func() time.Duration { return 0 }
	sent := make(chan string, 1)
	r.runTurn = func(_ context.Context, id, prompt, _ string, tok *sendToken) {
		sent <- prompt
		r.publishStream(id, nil, turnEnded, tok.started)
		r.releaseSend(id, tok)
	}

	started := time.Now()
	r.publishStream("abc12345", nil, limitEvent(started.Add(30*time.Millisecond)), started)
	r.publishStream("abc12345", nil, turnEnded, started)
	if err := r.SetLimitResume("abc12345", true); err != nil {
		t.Fatal(err)
	}
	if err := r.SetLimitResume("abc12345", false); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-sent:
		t.Fatalf("disarmed session was sent %q", p)
	case <-time.After(1500 * time.Millisecond):
	}
	if l := sessionLimit(t, r); l == nil || l.State != core.LimitOff {
		t.Fatalf("limit after disarm = %+v", l)
	}

	// The user's own turn getting through clears the limit.
	if err := r.SendToSession("abc12345", "hello"); err != nil {
		t.Fatal(err)
	}
	<-sent
	deadline := time.Now().Add(time.Second)
	for sessionLimit(t, r) != nil {
		if time.Now().After(deadline) {
			t.Fatal("limit not cleared by a clean turn")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Resumes that keep hitting the limit stop after maxLimitResumes.
func TestUsageLimitResumeGivesUp(t *testing.T) {
	r := newQueueTestRouter(t)
	r.limitJitter = func() time.Duration { return 0 }
	prompts := make(chan string, 8)
	r.runTurn = func(_ context.Context, id, prompt, _ string, tok *sendToken) {
		prompts <- prompt
		// A reset already in the past, as a stale report would give.
		r.publishStream(id, nil, limitEvent(time.Now().Add(-time.Minute)), tok.started)
		r.publishStream(id, nil, turnEnded, tok.started)
		r.releaseSend(id, tok)
	}
	started := time.Now()
	r.publishStream("abc12345", nil, limitEvent(started.Add(time.Millisecond)), started)
	r.publishStream("abc12345", nil, turnEnded, started)
	if err := r.SetLimitResume("abc12345", true); err != nil {
		t.Fatal(err)
	}
	for i := range maxLimitResumes {
		select {
		case <-prompts:
		case <-time.After(3 * time.Second):
			t.Fatalf("resume %d never sent", i)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		l := sessionLimit(t, r)
		if l != nil && l.State == core.LimitGaveUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still armed: %+v", l)
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case p := <-prompts:
		t.Fatalf("resumed past the cap: %q", p)
	case <-time.After(50 * time.Millisecond):
	}
	// Arming again is a fresh start.
	if err := r.SetLimitResume("abc12345", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-prompts:
	case <-time.After(3 * time.Second):
		t.Fatal("re-armed resume never sent")
	}
}

func TestUsageLimitTooFarIsNotArmed(t *testing.T) {
	r := newQueueTestRouter(t)
	started := time.Now()
	r.publishStream("abc12345", nil, limitEvent(started.Add(3*24*time.Hour)), started)
	r.publishStream("abc12345", nil, turnEnded, started)
	if err := r.SetLimitResume("abc12345", true); !errors.Is(err, core.ErrLimitTooFar) {
		t.Fatalf("arm = %v", err)
	}
	if l := sessionLimit(t, r); l == nil || l.State != core.LimitTooFar {
		t.Fatalf("limit = %+v", l)
	}
}

type goalRuntime struct {
	backend.Runtime
	resumed chan string
}

func (g *goalRuntime) ResumeGoal(_ context.Context, id, _ string) (bool, error) {
	g.resumed <- id
	return true, nil
}

// A goal the limit stopped is restarted, not prompted.
func TestUsageLimitResumesStoppedGoal(t *testing.T) {
	r := newQueueTestRouter(t)
	r.limitJitter = func() time.Duration { return 0 }
	rt := &goalRuntime{resumed: make(chan string, 1)}
	entry := r.backends["claude"]
	entry.Runtime = rt
	r.backends["claude"] = entry
	r.runTurn = func(_ context.Context, _, prompt, _ string, _ *sendToken) { t.Errorf("sent %q", prompt) }

	started := time.Now()
	r.publishStream("abc12345", nil, limitEvent(started.Add(time.Millisecond)), started)
	if err := r.SetLimitResume("abc12345", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rt.resumed:
	case <-time.After(3 * time.Second):
		t.Fatal("goal never resumed")
	}
	deadline := time.Now().Add(time.Second)
	for sessionLimit(t, r) != nil {
		if time.Now().After(deadline) {
			t.Fatal("limit not forgotten")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// With the setting on a limit waits by itself; a prompt or a pause ends the wait.
func TestContinueAtResetArmsAndPromptOrPauseCancels(t *testing.T) {
	for name, stop := range map[string]func(r *Router) error{
		"prompt": func(r *Router) error { return r.SendToSession("abc12345", "hello") },
		"pause":  func(r *Router) error { return r.PauseSession("abc12345") },
	} {
		t.Run(name, func(t *testing.T) {
			r := newQueueTestRouter(t)
			rt := &killRuntime{}
			entry := r.backends["claude"]
			entry.Runtime = rt
			r.backends["claude"] = entry
			r.settings = settings.New("")
			r.runTurn = func(_ context.Context, id, _, _ string, tok *sendToken) { r.releaseSend(id, tok) }
			started := time.Now()
			r.publishStream("abc12345", nil, limitEvent(started.Add(time.Hour)), started)
			if l := sessionLimit(t, r); l == nil || l.State != core.LimitWaiting {
				t.Fatalf("limit = %+v", l)
			}
			if err := stop(r); err != nil {
				t.Fatal(err)
			}
			if l := sessionLimit(t, r); l != nil && l.State != core.LimitOff {
				t.Fatalf("after stop = %+v", l)
			}
		})
	}
}

type killRuntime struct{ backend.Runtime }

func (*killRuntime) Kill(string) error { return nil }
