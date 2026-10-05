package jsonl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/internal/core"
)

// Record shapes are those Claude Code 2.1.270 writes for the cron/wakeup tools
// and the /goal Stop hook.
func TestReadSessionMetaDerivesActivity(t *testing.T) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)
	wake := now.Add(2 * time.Minute).Truncate(time.Millisecond)
	toolUse := func(id, name string, input ...string) string {
		in := "{}"
		if len(input) > 0 {
			in = input[0]
		}
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, ts, id, name, in)
	}
	toolResult := func(id, result string) string {
		return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]},"toolUseResult":%s}`, ts, id, result)
	}
	goal := func(body string) string {
		return fmt.Sprintf(`{"type":"attachment","timestamp":%q,"attachment":{"type":"goal_status",%s}}`, ts, body)
	}
	write := func(t *testing.T, lines ...string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "s.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	standing := []string{
		toolUse("t1", "CronCreate", `{"cron":"*/5 * * * *","prompt":"say hi"}`),
		toolResult("t1", `{"id":"e5844fe6","humanSchedule":"Every 5 minutes","recurring":true,"durable":false}`),
		toolUse("t2", "CronCreate"),
		toolResult("t2", `{"id":"once","humanSchedule":"Once at 09:00","recurring":false,"durable":false}`),
		toolUse("t3", "ScheduleWakeup"),
		toolResult("t3", fmt.Sprintf(`{"scheduledFor":%d,"clampedDelaySeconds":120,"wasClamped":false}`, wake.UnixMilli())),
		goal(`"met":false,"sentinel":true,"condition":"tests pass"`),
		goal(`"met":false,"condition":"tests pass","reason":"two failures left","iterations":2`),
	}
	meta, err := ReadSessionMeta(write(t, standing...))
	if err != nil {
		t.Fatal(err)
	}
	a := meta.Activity
	wantLoop := core.Loop{ID: "e5844fe6", Cron: "*/5 * * * *", Schedule: "Every 5 minutes", Prompt: "say hi"}
	if len(a.Loops) != 1 || a.Loops[0] != wantLoop {
		t.Fatalf("Loops = %+v, want the recurring job only: %+v", a.Loops, wantLoop)
	}
	if !a.NextWake.Equal(wake) {
		t.Fatalf("NextWake = %v, want %v", a.NextWake, wake)
	}
	if a.Goal == nil || a.Goal.Condition != "tests pass" || a.Goal.Status != "active" || a.Goal.Reason != "two failures left" {
		t.Fatalf("Goal = %+v", a.Goal)
	}
	if !a.Pins() {
		t.Fatal("loop timers and a pending wake must pin the process")
	}

	cleared := append(standing,
		goal(`"met":true,"condition":"tests pass","reason":"all green","iterations":3`),
		toolUse("t4", "CronDelete"),
		toolResult("t4", `{"id":"e5844fe6"}`),
		toolUse("t5", "ScheduleWakeup"),
		toolResult("t5", `{"scheduledFor":0,"clampedDelaySeconds":0,"wasClamped":false,"stopped":true,"cancelledWakeups":1}`),
	)
	meta, err = ReadSessionMeta(write(t, cleared...))
	if err != nil {
		t.Fatal(err)
	}
	a = meta.Activity
	if a.Goal != nil || len(a.Loops) != 0 || !a.NextWake.IsZero() {
		t.Fatalf("after clear/delete/stop: %+v", a)
	}
	if a.Pins() {
		t.Fatal("nothing left to pin")
	}

	// CronList is authoritative: jobs it omits are gone. It writes durable:false
	// for session-only jobs and leaves the field out for durable ones.
	listed := append(standing,
		toolUse("t6", "CronList"),
		toolResult("t6", `{"jobs":[{"id":"other","cron":"0 * * * *","humanSchedule":"Every hour","prompt":"x","recurring":true,"durable":false},`+
			`{"id":"disk","cron":"0 9 * * *","humanSchedule":"Daily at 09:00","prompt":"y","recurring":true}]}`),
	)
	meta, _ = ReadSessionMeta(write(t, listed...))
	if got := meta.Activity.Loops; len(got) != 1 || got[0].ID != "other" || got[0].Cron != "0 * * * *" || got[0].Schedule != "Every hour" || got[0].Prompt != "x" {
		t.Fatalf("Loops after CronList = %+v", got)
	}

	// A judged-impossible goal ends it like a met one; a failed CronCreate
	// records nothing; a prompt-less /loop runs the harness sentinel, which is
	// not a prompt.
	odd := []string{
		goal(`"met":false,"sentinel":true,"condition":"fly"`),
		goal(`"met":false,"failed":true,"condition":"fly","reason":"no wings"`),
		toolUse("t7", "CronCreate", `{"cron":"* * * * *","prompt":"z"}`),
		fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t7","is_error":true,"content":"bad cron"}]},"toolUseResult":"bad cron"}`, ts),
		toolUse("t8", "CronCreate", `{"cron":"*/5 * * * *","prompt":"<<autonomous-loop>>"}`),
		toolResult("t8", `{"id":"auto","humanSchedule":"Every 5 minutes","recurring":true,"durable":false}`),
	}
	meta, _ = ReadSessionMeta(write(t, odd...))
	a = meta.Activity
	if a.Goal != nil {
		t.Fatalf("impossible goal still active: %+v", a.Goal)
	}
	if len(a.Loops) != 1 || a.Loops[0].ID != "auto" || a.Loops[0].Prompt != "" {
		t.Fatalf("Loops = %+v, want only the sentinel loop with no prompt", a.Loops)
	}
}
