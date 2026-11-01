package router

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	backendpkg "github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
)

// limitResumePrompt is what an armed session is sent once its limit resets,
// worded as Claude Code's own auto-continue.
const limitResumePrompt = "Your usage limit has reset. Continue the task you were working on when the limit was reached; do not repeat work that is already complete."

// As Claude Code's own auto-continue does: give up after two resumes in a row,
// and do not sit out a reset more than a day away.
const (
	maxLimitResumes = 2
	maxLimitWait    = 24 * time.Hour
)

// usageLimit is kept in memory only: a restart forgets the limit and any
// armed resume.
type usageLimit struct {
	resetsAt time.Time
	noted    time.Time
	resume   bool
	resumes  int // sent since it was last armed
	gaveUp   bool
	resuming bool
	timer    *time.Timer
}

func (l *usageLimit) state(now time.Time) string {
	switch {
	case l.resuming:
		return core.LimitResuming
	case l.resume:
		return core.LimitWaiting
	case l.gaveUp:
		return core.LimitGaveUp
	case l.resetsAt.Sub(now) > maxLimitWait:
		return core.LimitTooFar
	}
	return core.LimitOff
}

// limitJitter spreads armed sessions past the reset instant, which the
// backend reports to the second or, for codex's message text, the minute.
func limitJitter() time.Duration {
	return 30*time.Second + rand.N(60*time.Second)
}

func (r *Router) noteUsageLimit(id string, raw json.RawMessage) {
	var p backendpkg.UsageLimitPayload
	if json.Unmarshal(raw, &p) != nil || p.ResetsAt <= 0 {
		return
	}
	resetsAt := time.Unix(p.ResetsAt, 0).UTC()
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if r.limits == nil {
		r.limits = map[string]*usageLimit{}
	}
	l := r.limits[id]
	if l == nil {
		l = &usageLimit{resume: r.ContinueAtReset()}
		r.limits[id] = l
	}
	l.resetsAt, l.noted, l.resuming = resetsAt, r.limitNow(), false
	// A resume that hit the limit again waits for the next reset, within bounds.
	if l.resume {
		switch {
		case l.resumes >= maxLimitResumes:
			l.resume, l.gaveUp = false, true
		case resetsAt.Sub(l.noted) > maxLimitWait:
			l.resume = false
		default:
			r.armLocked(id, l)
		}
	}
	slog.Info("usage limit reached", "session", id, "resets_at", resetsAt, "resume", l.resume)
}

// endUsageLimit forgets the limit once a turn begun after it ends without
// hitting it again.
func (r *Router) endUsageLimit(id string, started time.Time) {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if l := r.limits[id]; l != nil && l.noted.Before(started) {
		r.dropLimitLocked(id, l)
	}
}

func (r *Router) forgetUsageLimit(id string) {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if l := r.limits[id]; l != nil {
		r.dropLimitLocked(id, l)
	}
}

func (r *Router) dropLimitLocked(id string, l *usageLimit) {
	if l.timer != nil {
		l.timer.Stop()
	}
	delete(r.limits, id)
}

// SetLimitResume arms or disarms sending "continue" when the session's usage
// limit resets.
func (r *Router) SetLimitResume(id string, on bool) error {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	l := r.limits[id]
	if l == nil {
		return core.ErrNoUsageLimit
	}
	if l.resume == on {
		return nil
	}
	if on && l.resetsAt.Sub(r.limitNow()) > maxLimitWait {
		return core.ErrLimitTooFar
	}
	l.resume, l.resumes, l.gaveUp = on, 0, false
	if on {
		r.armLocked(id, l)
	} else if l.timer != nil {
		l.timer.Stop()
		l.timer = nil
	}
	slog.Info("usage limit resume", "session", id, "armed", on, "resets_at", l.resetsAt)
	return nil
}

// cancelLimitWait stops a pending resume: the session was paused, or a prompt
// took its place.
func (r *Router) cancelLimitWait(id string) {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if l := r.limits[id]; l != nil && l.resume {
		l.resume = false
		if l.timer != nil {
			l.timer.Stop()
			l.timer = nil
		}
	}
}

func (r *Router) armLocked(id string, l *usageLimit) {
	if l.timer != nil {
		l.timer.Stop()
	}
	jitter := limitJitter
	if r.limitJitter != nil {
		jitter = r.limitJitter
	}
	at := l.resetsAt
	wait := max(at.Sub(r.limitNow()), 0) + jitter()
	l.timer = time.AfterFunc(wait, func() { r.fireLimitResume(id, at) })
}

// fireLimitResume sends the resume prompt. The limit stays recorded: the
// turn either ends cleanly and clears it, or hits it again and re-arms.
func (r *Router) fireLimitResume(id string, at time.Time) {
	r.limitMu.Lock()
	l := r.limits[id]
	if l == nil || !l.resume || !l.resetsAt.Equal(at) {
		r.limitMu.Unlock()
		return
	}
	l.timer = nil
	l.resumes++
	l.resuming = true
	r.limitMu.Unlock()
	slog.Info("usage limit reset, resuming", "session", id)
	if r.resumeGoal(id) {
		r.forgetUsageLimit(id)
		return
	}
	r.withdrawQuestions(id)
	if err := r.enqueueSend(id, pendingSend{text: limitResumePrompt}); err != nil {
		slog.Warn("usage limit resume failed", "session", id, "err", err)
		r.limitMu.Lock()
		if cur := r.limits[id]; cur == l {
			r.dropLimitLocked(id, l)
		}
		r.limitMu.Unlock()
	}
}

// resumeGoal restarts a goal the limit stopped: a plain prompt would run one
// turn and leave the goal stopped. The goal's turn is the backend's own, so a
// limit it hits is not tracked here.
func (r *Router) resumeGoal(id string) bool {
	g, ok := r.senderFor(id).(backendpkg.GoalResumer)
	sess, found := r.discovery.Get(id)
	if !ok || !found {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resumed, err := g.ResumeGoal(ctx, id, sess.Cwd)
	if err != nil {
		slog.Warn("usage limit goal resume failed", "session", id, "err", err)
		return false
	}
	return resumed
}

// ContinueAtReset is the instance's setting; without settings it is off.
func (r *Router) ContinueAtReset() bool {
	return r.settings != nil && r.settings.ContinueAtReset()
}

// SetContinueAtReset applies to limits hit from now on.
func (r *Router) SetContinueAtReset(on bool) error {
	if r.settings == nil {
		return errors.New("settings are not configured")
	}
	return r.settings.SetContinueAtReset(on)
}

func (r *Router) limitNow() time.Time {
	if r.limitClock != nil {
		return r.limitClock()
	}
	return time.Now()
}

func (r *Router) applyUsageLimit(s *core.Session) {
	r.limitMu.Lock()
	defer r.limitMu.Unlock()
	if l := r.limits[s.ID]; l != nil {
		s.UsageLimit = &core.UsageLimit{ResetsAt: l.resetsAt, State: l.state(r.limitNow())}
	}
}
