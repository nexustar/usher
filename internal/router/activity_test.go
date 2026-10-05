package router

import (
	"testing"
	"time"

	backendpkg "github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

func TestApplyLiveStatusMergesWorkerOverTranscript(t *testing.T) {
	wake := time.Now().Add(time.Hour)
	transcript := core.Activity{
		Loops:    []core.Loop{{ID: "c1", Schedule: "Every 5 minutes"}},
		NextWake: wake,
		Goal:     &core.Goal{Condition: "tests pass", Status: "active", Reason: "two left"},
	}
	fresh := func() core.Session { return core.Session{ID: "s", Status: core.StatusIdle, Activity: transcript} }

	// No worker: transcript state stands, nothing runs.
	sess := fresh()
	applyLiveStatus(&sess, false, map[string]backendpkg.LiveSession{})
	if sess.Status != core.StatusIdle || sess.Activity.Agents != 0 || len(sess.Activity.Loops) != 1 ||
		!sess.Activity.NextWake.Equal(wake) || sess.Activity.Goal == nil || sess.Activity.Goal.Reason != "two left" {
		t.Fatalf("no worker: %+v %+v", sess.Status, sess.Activity)
	}

	// An idle worker with agents: live, open to input, transcript fields kept.
	sess = fresh()
	applyLiveStatus(&sess, false, map[string]backendpkg.LiveSession{"s": {ID: "s", Activity: core.Activity{Agents: 2}}})
	if sess.Status != core.StatusLive || sess.Activity.Agents != 2 || len(sess.Activity.Loops) != 1 || sess.Activity.Goal.Reason != "two left" {
		t.Fatalf("idle worker: %+v %+v", sess.Status, sess.Activity)
	}

	// A worker that knows the goal (Codex) overrides the transcript's.
	sess = fresh()
	applyLiveStatus(&sess, false, map[string]backendpkg.LiveSession{"s": {ID: "s", Running: true, Activity: core.Activity{Goal: &core.Goal{Condition: "ship", Status: "paused"}}}})
	if sess.Status != core.StatusRunning || sess.Activity.Goal.Status != "paused" || sess.Activity.Agents != 0 {
		t.Fatalf("worker goal: %+v %+v", sess.Status, sess.Activity)
	}

	// usher's own send marks running even before the worker reports a turn.
	sess = fresh()
	applyLiveStatus(&sess, true, map[string]backendpkg.LiveSession{"s": {ID: "s"}})
	if sess.Status != core.StatusRunning {
		t.Fatalf("active send: %v", sess.Status)
	}
}
