// Package terminal manages session-scoped tmux shells.
package terminal

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

const sessionName = "usher-terminal"

var (
	ErrUnavailable = errors.New("terminal unavailable: tmux is not installed")
	ErrNotOpen     = errors.New("terminal is not open")
)

type runner interface {
	run(args ...string) (string, error)
	runStdin(in string, args ...string) (string, error)
}

type execRunner struct {
	bin    string
	socket string
}

func (e execRunner) run(args ...string) (string, error) {
	return e.runStdin("", args...)
}

func (e execRunner) runStdin(in string, args ...string) (string, error) {
	full := append([]string{"-L", e.socket}, args...)
	cmd := exec.Command(e.bin, full...)
	if in != "" {
		cmd.Stdin = strings.NewReader(in)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Manager binds one tmux window to each conversation.
type Manager struct {
	runner    runner
	shell     string
	available bool
	mu        sync.Mutex
	seen      map[string]*recentRequests
}

type recentRequests struct {
	set   map[string]bool
	order []string
}

func New(tmuxBin, socket, shell string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	path, err := exec.LookPath(tmuxBin)
	if err != nil {
		logger.Info("session terminal disabled", "reason", err)
		return &Manager{shell: shell, seen: map[string]*recentRequests{}}
	}
	return &Manager{
		runner:    execRunner{bin: path, socket: socket},
		shell:     shell,
		available: true,
		seen:      map[string]*recentRequests{},
	}
}

func (m *Manager) Available() bool { return m != nil && m.available }

// Open starts or reuses a shell in cwd.
func (m *Manager) Open(id, cwd string, cols, rows int) error {
	if !m.Available() {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hasLocked(id) {
		return m.resizeLocked(id, cols, rows)
	}
	delete(m.seen, id)

	command := shellQuote(m.shell)
	var err error
	if !m.sessionExistsLocked() {
		_, err = m.runner.run("new-session", "-d", "-s", sessionName,
			"-n", id, "-c", cwd, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows), command)
	} else {
		_, err = m.runner.run("new-window", "-t", sessionName,
			"-n", id, "-c", cwd, command)
	}
	if err != nil {
		return fmt.Errorf("open terminal: %w", err)
	}
	_, _ = m.runner.run("set-window-option", "-t", target(id), "automatic-rename", "off")
	return m.resizeLocked(id, cols, rows)
}

func (m *Manager) Has(id string) bool {
	if !m.Available() {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hasLocked(id)
}

// Capture returns the rendered pane. A failed capture checks whether the
// window is gone before returning ErrNotOpen.
func (m *Manager) Capture(id string) (string, error) {
	if !m.Available() {
		return "", ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out, err := m.runner.run("capture-pane", "-p", "-e", "-t", target(id))
	if err != nil {
		if !m.hasLocked(id) {
			return "", ErrNotOpen
		}
		return "", err
	}
	return out, nil
}

// Submit pastes text and presses Enter.
func (m *Manager) Submit(id, requestID, text string) error {
	if !m.Available() {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasLocked(id) {
		return ErrNotOpen
	}
	if recent := m.seen[id]; recent != nil && recent.set[requestID] {
		return nil
	}
	buf := "usher-terminal-input"
	if _, err := m.runner.runStdin(text, "load-buffer", "-b", buf, "-"); err != nil {
		return fmt.Errorf("load terminal input: %w", err)
	}
	// Keep paste and Enter in one tmux invocation for safer retries.
	if _, err := m.runner.run("paste-buffer", "-p", "-d", "-b", buf, "-t", target(id),
		";", "send-keys", "-t", target(id), "Enter"); err != nil {
		return fmt.Errorf("submit terminal input: %w", err)
	}
	m.rememberLocked(id, requestID)
	return nil
}

// SendControl accepts already allow-listed tmux key names from the web layer.
func (m *Manager) SendControl(id string, keys ...string) error {
	if !m.Available() {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasLocked(id) {
		return ErrNotOpen
	}
	args := append([]string{"send-keys", "-t", target(id)}, keys...)
	_, err := m.runner.run(args...)
	return err
}

func (m *Manager) Resize(id string, cols, rows int) error {
	if !m.Available() {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasLocked(id) {
		return ErrNotOpen
	}
	return m.resizeLocked(id, cols, rows)
}

// Close destroys only this session's window. Other session terminals on the
// dedicated tmux server are unaffected.
func (m *Manager) Close(id string) error {
	if !m.Available() {
		return ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasLocked(id) {
		delete(m.seen, id)
		return nil
	}
	_, err := m.runner.run("kill-window", "-t", target(id))
	delete(m.seen, id)
	return err
}

func (m *Manager) rememberLocked(id, requestID string) {
	if m.seen == nil {
		m.seen = map[string]*recentRequests{}
	}
	recent := m.seen[id]
	if recent == nil {
		recent = &recentRequests{set: map[string]bool{}}
		m.seen[id] = recent
	}
	if recent.set[requestID] {
		return
	}
	recent.set[requestID] = true
	recent.order = append(recent.order, requestID)
	const maxRecent = 64
	if len(recent.order) > maxRecent {
		old := recent.order[0]
		recent.order = recent.order[1:]
		delete(recent.set, old)
	}
}

func (m *Manager) resizeLocked(id string, cols, rows int) error {
	_, err := m.runner.run("resize-window", "-t", target(id),
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	return err
}

func (m *Manager) hasLocked(id string) bool {
	out, err := m.runner.run("list-windows", "-t", sessionName, "-F", "#{window_name}")
	if err != nil {
		return false
	}
	for _, name := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(name) == id {
			return true
		}
	}
	return false
}

func (m *Manager) sessionExistsLocked() bool {
	_, err := m.runner.run("has-session", "-t", sessionName)
	return err == nil
}

func target(id string) string { return sessionName + ":" + id }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
