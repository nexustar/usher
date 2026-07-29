package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/hook"
)

type worker struct {
	client *Client
	ready  chan struct{}
	err    error
	busy   bool
	// leases protect short RPCs without making the session appear mid-turn.
	leases   int
	lastUsed time.Time
}

// Manager owns one app-server worker per live root Codex session.
type Manager struct {
	bin     string
	hooks   *hook.Manager
	sandbox map[string]any
	config  map[string]any
	env     []string
	logger  *slog.Logger
	maxLive int

	mu           sync.Mutex
	workers      map[string]*worker
	starting     int
	systemPrompt func(string) string
}

func NewManager(bin string, hooks *hook.Manager, sandbox, config map[string]any, env []string, maxLive int, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if maxLive <= 0 {
		maxLive = 8
	}
	return &Manager{bin: bin, hooks: hooks, sandbox: cloneMap(sandbox), config: cloneMap(config), env: append([]string(nil), env...), logger: logger, maxLive: maxLive, workers: map[string]*worker{}}
}

func (m *Manager) newClient(instructions string) *Client {
	var args []string
	if instructions != "" {
		quoted, _ := json.Marshal(instructions)
		args = []string{"-c", "developer_instructions=" + string(quoted)}
	}
	return New(m.bin, m.hooks, m.sandbox, m.config, m.env, args, m.logger)
}

func (m *Manager) SetSystemPromptLookup(lookup func(string) string) {
	m.mu.Lock()
	m.systemPrompt = lookup
	m.mu.Unlock()
}

func (m *Manager) promptFor(id string) string {
	m.mu.Lock()
	lookup := m.systemPrompt
	m.mu.Unlock()
	if lookup == nil {
		return ""
	}
	return lookup(id)
}

// reserve makes room for a new worker. The returned idle victim has already
// been removed from the live map and can be stopped without holding m.mu.
func (m *Manager) reserve() (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.workers)+m.starting < m.maxLive {
		m.starting++
		return nil, nil
	}
	var victimID string
	var victim *worker
	for id, w := range m.workers {
		if w.ready != nil || w.busy || w.leases > 0 || w.client.Busy(id) {
			continue
		}
		if victim != nil {
			wRunning, victimRunning := w.client.Running(), victim.client.Running()
			if (wRunning && !victimRunning) || (wRunning == victimRunning && !w.lastUsed.Before(victim.lastUsed)) {
				continue
			}
		}
		victimID, victim = id, w
	}
	if victim == nil {
		return nil, fmt.Errorf("maximum live Codex sessions (%d) are all busy", m.maxLive)
	}
	delete(m.workers, victimID)
	m.starting++
	return victim.client, nil
}

func (m *Manager) finishStart() {
	m.mu.Lock()
	m.starting--
	m.mu.Unlock()
}

func (m *Manager) StartThread(ctx context.Context, cwd, model, instructions string) (string, error) {
	victim, err := m.reserve()
	if err != nil {
		return "", err
	}
	if victim != nil {
		victim.Shutdown()
	}
	c := m.newClient(instructions)
	id, err := c.StartThread(ctx, cwd, model)
	if err != nil {
		m.finishStart()
		c.Shutdown()
		return "", err
	}
	m.mu.Lock()
	m.starting--
	m.workers[id] = &worker{client: c, lastUsed: time.Now()}
	m.mu.Unlock()
	return id, nil
}

// leaseWorker resumes a session and leases its worker against eviction. The
// lease is taken under the same lock that confirms the worker is still owned.
// Unlike pi's leaseWorkerIfLive it will start a cold worker, so it belongs only
// on paths that are about to drive a turn.
func (m *Manager) leaseWorker(ctx context.Context, id, cwd string) (*worker, error) {
	for {
		w, err := m.getOrResume(ctx, id, cwd)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		if m.workers[id] != w {
			// Evicted in the gap. Its client is already shutting down, so
			// resume again rather than talking to a dead process.
			m.mu.Unlock()
			continue
		}
		w.leases++
		w.lastUsed = time.Now()
		m.mu.Unlock()
		return w, nil
	}
}

func (m *Manager) releaseWorker(w *worker) {
	m.mu.Lock()
	w.leases--
	w.lastUsed = time.Now()
	m.mu.Unlock()
}

func (m *Manager) getOrResume(ctx context.Context, id, cwd string) (*worker, error) {
	for {
		m.mu.Lock()
		if w := m.workers[id]; w != nil {
			ready := w.ready
			if ready == nil {
				w.lastUsed = time.Now()
				m.mu.Unlock()
				return w, w.err
			}
			m.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
			}
			continue
		}
		m.mu.Unlock()

		victim, err := m.reserve()
		if err != nil {
			return nil, err
		}
		if victim != nil {
			victim.Shutdown()
		}
		ready := make(chan struct{})
		w := &worker{client: m.newClient(m.promptFor(id)), ready: ready, lastUsed: time.Now()}
		m.mu.Lock()
		m.starting--
		if existing := m.workers[id]; existing != nil {
			m.mu.Unlock()
			w.client.Shutdown()
			continue
		}
		m.workers[id] = w
		m.mu.Unlock()

		err = w.client.ResumeThread(ctx, id, cwd)
		m.mu.Lock()
		owned := m.workers[id] == w
		if !owned && err == nil {
			err = fmt.Errorf("Codex session %s stopped", id)
		}
		w.err = err
		w.ready = nil
		if err != nil && owned {
			delete(m.workers, id)
		}
		close(ready)
		m.mu.Unlock()
		if err != nil {
			w.client.Shutdown()
			return nil, err
		}
		return w, nil
	}
}

func (m *Manager) StartTurn(ctx context.Context, id, prompt, cwd string) (<-chan TurnResult, <-chan Delta, error) {
	return m.startOperation(ctx, id, cwd, func(c *Client) (<-chan TurnResult, <-chan Delta, error) {
		return c.StartTurn(ctx, id, prompt, cwd)
	})
}

func (m *Manager) Compact(ctx context.Context, id, cwd string) (<-chan TurnResult, <-chan Delta, error) {
	return m.startOperation(ctx, id, cwd, func(c *Client) (<-chan TurnResult, <-chan Delta, error) {
		return c.Compact(ctx, id)
	})
}

func (m *Manager) Review(ctx context.Context, id, cwd, instructions string) (<-chan TurnResult, <-chan Delta, error) {
	return m.startOperation(ctx, id, cwd, func(c *Client) (<-chan TurnResult, <-chan Delta, error) {
		return c.Review(ctx, id, instructions)
	})
}

// Rename runs Codex's /rename RPC, resuming a cold thread if needed.
func (m *Manager) Rename(ctx context.Context, id, cwd, title string) error {
	w, err := m.leaseWorker(ctx, id, cwd)
	if err != nil {
		return err
	}
	defer m.releaseWorker(w)
	m.mu.Lock()
	busy := w.busy || w.client.Busy(id)
	m.mu.Unlock()
	if busy {
		return fmt.Errorf("Codex session %s is busy", id)
	}
	return w.client.RenameThread(ctx, id, title)
}

func (m *Manager) startOperation(ctx context.Context, id, cwd string, start func(*Client) (<-chan TurnResult, <-chan Delta, error)) (<-chan TurnResult, <-chan Delta, error) {
	// Keep the worker leased until start() has registered the operation.
	w, err := m.leaseWorker(ctx, id, cwd)
	if err != nil {
		return nil, nil, err
	}
	defer m.releaseWorker(w)
	m.mu.Lock()
	if w.busy || w.client.Busy(id) {
		m.mu.Unlock()
		return nil, nil, fmt.Errorf("Codex session %s is busy", id)
	}
	w.busy, w.lastUsed = true, time.Now()
	m.mu.Unlock()
	inner, deltas, err := start(w.client)
	if err != nil {
		m.mu.Lock()
		w.busy = false
		m.mu.Unlock()
		return nil, nil, err
	}
	out := make(chan TurnResult, 1)
	go func() {
		result, ok := <-inner
		m.mu.Lock()
		if m.workers[id] == w {
			w.busy, w.lastUsed = false, time.Now()
		}
		m.mu.Unlock()
		if ok {
			out <- result
		}
		close(out)
	}()
	return out, deltas, nil
}

// Resume starts an idle app-server worker and attaches it to an existing
// thread without starting a turn.
func (m *Manager) Resume(ctx context.Context, id, cwd string) error {
	_, err := m.getOrResume(ctx, id, cwd)
	return err
}

// Skills resumes the session if needed, so it belongs on paths that are about
// to drive a turn anyway.
func (m *Manager) Skills(ctx context.Context, id, cwd string) ([]Skill, error) {
	w, err := m.leaseWorker(ctx, id, cwd)
	if err != nil {
		return nil, err
	}
	defer m.releaseWorker(w)
	return w.client.Skills(ctx, cwd)
}

// SkillsIfLive answers only from a worker that is already up, reporting no
// skills otherwise. Composer completion is speculative — the user may never
// finish the token — so it must not pay for a cold start or evict another
// session's warm worker to satisfy a popup.
func (m *Manager) SkillsIfLive(ctx context.Context, id, cwd string) ([]Skill, bool, error) {
	m.mu.Lock()
	w := m.workers[id]
	if w == nil || w.ready != nil {
		m.mu.Unlock()
		return nil, false, nil
	}
	w.leases++
	w.lastUsed = time.Now()
	m.mu.Unlock()
	defer m.releaseWorker(w)
	skills, err := w.client.Skills(ctx, cwd)
	return skills, err == nil, err
}

// RenameIfLive renames through an existing worker without entering the LRU.
func (m *Manager) RenameIfLive(ctx context.Context, id, title string) (bool, error) {
	m.mu.Lock()
	w := m.workers[id]
	if w == nil || w.ready != nil || w.err != nil || !w.client.Running() {
		m.mu.Unlock()
		return false, nil
	}
	w.leases++
	w.lastUsed = time.Now()
	m.mu.Unlock()
	defer m.releaseWorker(w)
	return true, w.client.RenameThread(ctx, id, title)
}

func (m *Manager) Interrupt(ctx context.Context, id string) error {
	m.mu.Lock()
	w := m.workers[id]
	m.mu.Unlock()
	if w == nil || w.ready != nil {
		return nil
	}
	return w.client.Interrupt(ctx, id)
}

func (m *Manager) Has(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.workers[id]
	return w != nil && w.ready == nil && w.err == nil && w.client.Running()
}

func (m *Manager) LiveSessions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.workers))
	for id, w := range m.workers {
		if w.ready == nil && w.err == nil && w.client.Running() {
			out = append(out, id)
		}
	}
	return out
}

func (m *Manager) Kill(ctx context.Context, id string) error {
	m.mu.Lock()
	w := m.workers[id]
	delete(m.workers, id)
	m.mu.Unlock()
	if w == nil {
		return nil
	}
	if w.ready != nil {
		<-w.ready
	}
	_ = w.client.Kill(ctx, id)
	w.client.Shutdown()
	return nil
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	workers := make([]*worker, 0, len(m.workers))
	for _, w := range m.workers {
		workers = append(workers, w)
	}
	m.workers = map[string]*worker{}
	m.mu.Unlock()
	for _, w := range workers {
		if w.ready != nil {
			<-w.ready
		}
		w.client.Shutdown()
	}
}
