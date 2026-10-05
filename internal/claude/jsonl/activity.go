package jsonl

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
)

// Claude drops session crons older than this when it re-creates them on resume.
const cronMaxAge = 7 * 24 * time.Hour

// activityScan derives a session's goal and schedules from its transcript:
// goal_status attachments and the cron/wakeup tools' results. Claude
// re-creates session crons from the same records on resume. None of this
// reaches stream-json.
type activityScan struct {
	tools    map[string]toolUse // tool_use id → call, schedule tools only
	loops    map[string]core.Loop
	order    []string // cron ids, creation order
	nextWake time.Time
	goal     *core.Goal
}

// The result names the job; cron and prompt are only in the call.
type toolUse struct {
	name, cron, prompt string
}

// Every discovery scan runs this on every line: look before decoding.
func (s *activityScan) feed(ev Event) {
	switch ev.Type {
	case "attachment":
		if bytes.Contains(ev.Raw, []byte(`"goal_status"`)) {
			s.goalStatus(ev.Raw)
		}
	case "assistant":
		if bytes.Contains(ev.Message, []byte("Cron")) || bytes.Contains(ev.Message, []byte("ScheduleWakeup")) {
			s.toolUses(ev.Message)
		}
	case "user":
		s.toolResults(ev)
	}
}

func (s *activityScan) activity() core.Activity {
	a := core.Activity{NextWake: s.nextWake, Goal: s.goal}
	for _, id := range s.order {
		if loop, ok := s.loops[id]; ok {
			a.Loops = append(a.Loops, loop)
		}
	}
	return a
}

// goal_status records: set → met:false sentinel:true; each not-yet-met
// verdict → met:false with reason; met, /goal clear, and an unrecoverable
// error → met:true; judged impossible → met:false failed:true.
func (s *activityScan) goalStatus(raw json.RawMessage) {
	var r struct {
		Attachment struct {
			Type      string `json:"type"`
			Met       bool   `json:"met"`
			Failed    bool   `json:"failed"`
			Condition string `json:"condition"`
			Reason    string `json:"reason"`
		} `json:"attachment"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Attachment.Type != "goal_status" {
		return
	}
	if r.Attachment.Met || r.Attachment.Failed {
		s.goal = nil
		return
	}
	s.goal = &core.Goal{Condition: r.Attachment.Condition, Status: "active", Reason: r.Attachment.Reason}
}

func (s *activityScan) toolUses(msg json.RawMessage) {
	var m struct {
		Content []struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input struct {
				Cron   string `json:"cron"`
				Prompt string `json:"prompt"`
			} `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return
	}
	for _, b := range m.Content {
		if b.Type != "tool_use" {
			continue
		}
		switch b.Name {
		case "CronCreate", "CronDelete", "CronList", "ScheduleWakeup":
			if s.tools == nil {
				s.tools = map[string]toolUse{}
			}
			s.tools[b.ID] = toolUse{name: b.Name, cron: b.Input.Cron, prompt: b.Input.Prompt}
		}
	}
}

func (s *activityScan) toolResults(ev Event) {
	if len(s.tools) == 0 || len(ev.ToolUseResult) == 0 {
		return
	}
	var m struct {
		Content []struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			IsError   bool   `json:"is_error"`
		} `json:"content"`
	}
	if json.Unmarshal(ev.Message, &m) != nil {
		return
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" || b.IsError {
			continue
		}
		use, ok := s.tools[b.ToolUseID]
		if !ok {
			continue
		}
		delete(s.tools, b.ToolUseID)
		switch use.name {
		case "CronCreate":
			var r cronJob
			if json.Unmarshal(ev.ToolUseResult, &r) == nil {
				r.Cron, r.Prompt = use.cron, use.prompt
				s.addCron(r, ev.Timestamp)
			}
		case "CronDelete":
			var r struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(ev.ToolUseResult, &r) == nil {
				delete(s.loops, r.ID)
			}
		case "CronList":
			var r struct {
				Jobs []cronJob `json:"jobs"`
			}
			if json.Unmarshal(ev.ToolUseResult, &r) == nil {
				s.loops, s.order = nil, nil
				for _, job := range r.Jobs {
					s.addCron(job, ev.Timestamp)
				}
			}
		case "ScheduleWakeup":
			var r struct {
				ScheduledFor int64 `json:"scheduledFor"`
				Stopped      bool  `json:"stopped"`
			}
			if json.Unmarshal(ev.ToolUseResult, &r) != nil {
				continue
			}
			s.nextWake = time.Time{}
			if !r.Stopped && r.ScheduledFor > 0 {
				s.nextWake = time.UnixMilli(r.ScheduledFor).UTC()
			}
		}
	}
}

// Durable is a pointer: CronCreate's result always carries it, but CronList
// writes durable:false for session-only jobs and omits it for durable ones.
type cronJob struct {
	ID            string `json:"id"`
	Cron          string `json:"cron"`
	Prompt        string `json:"prompt"`
	HumanSchedule string `json:"humanSchedule"`
	Recurring     bool   `json:"recurring"`
	Durable       *bool  `json:"durable"`
}

// Session-scoped recurring jobs only: a durable job outlives the session; a
// one-shot leaves no record when it fires, and telling pending from spent
// takes a cron parser.
func (s *activityScan) addCron(job cronJob, at time.Time) {
	sessionOnly := job.Durable != nil && !*job.Durable
	if job.ID == "" || !job.Recurring || !sessionOnly {
		return
	}
	if !at.IsZero() && time.Since(at) > cronMaxAge {
		return
	}
	// A fixed-interval /loop with no prompt runs the harness's own sentinel.
	prompt := job.Prompt
	if strings.HasPrefix(prompt, "<<") && strings.HasSuffix(prompt, ">>") {
		prompt = ""
	}
	if s.loops == nil {
		s.loops = map[string]core.Loop{}
	}
	if _, seen := s.loops[job.ID]; !seen {
		s.order = append(s.order, job.ID)
	}
	s.loops[job.ID] = core.Loop{ID: job.ID, Cron: job.Cron, Schedule: job.HumanSchedule, Prompt: prompt}
}
