package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/core"
)

// Starter creates a session and returns its backend-assigned id.
// *router.Router implements it.
type Starter interface {
	StartSession(core.CreateOptions) (string, error)
}

// Resolver turns a task's target fields into create options.
// *agentprofile.Store implements it.
type Resolver interface {
	Resolve(agent, cwd, backend, model string) (core.CreateOptions, error)
}

// Cron resolution is one minute; this only bounds how late a match is noticed.
const checkInterval = 20 * time.Second

// Runner fires due tasks. Its only scheduling state is the instant of the last
// check: a task is due when it matches somewhere in (lastCheck, now]. Editing
// a task therefore needs nothing invalidated, a laptop that slept through
// several matches fires once on wake, and a fresh start never backfires.
//
// It never writes to the store.
type Runner struct {
	store    *Store
	starter  Starter
	resolver Resolver
	logger   *slog.Logger

	// Test seams; production uses time.Now and checkInterval.
	now      func() time.Time
	interval time.Duration

	// Held across a whole run, so two never overlap. See fire.
	fireMu sync.Mutex
}

func NewRunner(store *Store, starter Starter, resolver Resolver, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		store:    store,
		starter:  starter,
		resolver: resolver,
		logger:   logger,
		now:      time.Now,
		interval: checkInterval,
	}
}

// Store is the API layer's handle. The runner caches nothing from it, so
// writes need no coordination with the tick loop.
func (r *Runner) Store() *Store { return r.store }

func (r *Runner) Run(ctx context.Context) {
	last := r.now()
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := r.now()
		r.check(last, now)
		last = now
	}
}

// check fires every task with a match in (since, now]. It starts them one at a
// time and waits: `now` was read before any of them ran, so however long they
// take, the next window resumes exactly where this one ended.
func (r *Runner) check(since, now time.Time) {
	for _, task := range r.store.List() {
		if next := task.NextRun(since); next.IsZero() || next.After(now) {
			continue
		}
		r.fire(task)
	}
}

// RunNow starts a task's session immediately, whatever its schedule says — the
// only way to find out that a task's cwd or model is wrong without waiting for
// its next match.
func (r *Runner) RunNow(id string) (string, error) {
	task, ok := r.store.Get(id)
	if !ok {
		return "", fmt.Errorf("schedule %q not found", id)
	}
	return r.fire(task)
}

// fire starts one run, and only one at a time: a backend that takes minutes to
// come up would otherwise have several of these overlapping. Nothing piles up
// behind the lock — a check window that a slow run outlasts collapses every
// match it spans into the single run the next window asks for.
func (r *Runner) fire(task Task) (string, error) {
	r.fireMu.Lock()
	defer r.fireMu.Unlock()
	sessionID, err := r.start(task)
	if err != nil {
		r.logger.Warn("schedule run failed", "id", task.ID, "name", task.Name, "err", err)
	} else {
		r.logger.Info("schedule started session", "id", task.ID, "name", task.Name, "session", sessionID)
	}
	return sessionID, err
}

func (r *Runner) start(task Task) (string, error) {
	opts, err := r.resolver.Resolve(task.Agent, task.Cwd, task.Backend, task.Model)
	if err != nil {
		return "", err
	}
	opts.InitialMessage = task.Prompt
	return r.starter.StartSession(opts)
}
