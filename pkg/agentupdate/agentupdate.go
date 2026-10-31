// Package agentupdate reports when an agent CLI has a newer release and runs
// the CLI's own update command. Install methods stay the CLI's business.
package agentupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/pkg/procutil"
)

const (
	versionTimeout = 20 * time.Second
	updateTimeout  = 5 * time.Minute
	maxOutput      = 16 << 10
)

var (
	ErrUnknown = errors.New("unknown agent")
	ErrBusy    = errors.New("update already running")
)

var versionRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?`)

// Spec describes one agent CLI. Latest nil leaves its newest release unknown.
type Spec struct {
	Name       string
	Cmd        string
	UpdateArgs []string
	Latest     func(context.Context) (string, error)
}

// Status is one agent CLI as last checked.
type Status struct {
	Name            string    `json:"name"`
	Path            string    `json:"path,omitempty"`
	Current         string    `json:"current,omitempty"`
	Latest          string    `json:"latest,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	CheckedAt       time.Time `json:"checked_at,omitzero"`
	CheckError      string    `json:"check_error,omitempty"`
	Command         string    `json:"command"`
	Updating        bool      `json:"updating"`
	Output          string    `json:"output,omitempty"`
	UpdateError     string    `json:"update_error,omitempty"`
}

type entry struct {
	spec   Spec
	status Status
	// generation drops a check that raced an update and saw the old version.
	generation int
}

// Manager keeps the Status of every agent CLI.
type Manager struct {
	ctx    context.Context
	logger *slog.Logger

	mu      sync.Mutex
	entries []*entry
}

// New builds a Manager; ctx bounds running updates. Nothing is checked or
// updated until asked.
func New(ctx context.Context, specs []Spec, logger *slog.Logger) *Manager {
	m := &Manager{ctx: ctx, logger: logger}
	for _, spec := range specs {
		command := strings.Join(append([]string{spec.Cmd}, spec.UpdateArgs...), " ")
		m.entries = append(m.entries, &entry{spec: spec, status: Status{Name: spec.Name, Command: command}})
	}
	return m
}

// List returns the last known status of every agent.
func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.status)
	}
	return out
}

// Check refreshes every agent in parallel.
func (m *Manager) Check(ctx context.Context) {
	var wg sync.WaitGroup
	for _, e := range m.entries {
		wg.Go(func() { m.check(ctx, e) })
	}
	wg.Wait()
}

func (m *Manager) check(ctx context.Context, e *entry) {
	m.mu.Lock()
	generation := e.generation
	updating := e.status.Updating
	m.mu.Unlock()
	if updating {
		return
	}
	found := lookupSpec(ctx, e.spec)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !e.status.Updating && e.generation == generation {
		m.apply(e, found)
	}
}

type lookup struct {
	path, current, latest string
	err                   error
}

func lookupSpec(ctx context.Context, spec Spec) lookup {
	var l lookup
	l.path, l.current, l.err = localVersion(ctx, spec.Cmd)
	if l.err == nil && spec.Latest != nil {
		l.latest, l.err = spec.Latest(ctx)
	}
	return l
}

// apply records l; the caller holds m.mu.
func (m *Manager) apply(e *entry, l lookup) {
	s := &e.status
	s.Path, s.Current = l.path, l.current
	s.CheckedAt = time.Now()
	s.CheckError = ""
	if l.err != nil {
		s.CheckError = l.err.Error()
		m.logger.Debug("agent update check failed", "backend", s.Name, "err", l.err)
	}
	// A failed lookup keeps the last known release.
	if l.latest != "" {
		s.Latest = l.latest
	}
	s.UpdateAvailable = s.Current != "" && s.Latest != "" && compareVersions(s.Latest, s.Current) > 0
}

func localVersion(ctx context.Context, name string) (path, version string, err error) {
	path, err = exec.LookPath(name)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Dir = os.TempDir()
	cmd.Env = cliEnv(path)
	out, err := cmd.Output()
	if err != nil {
		return path, "", fmt.Errorf("%s --version: %w", name, err)
	}
	version = versionRe.FindString(string(out))
	if version == "" {
		return path, "", fmt.Errorf("%s --version: no version in output", name)
	}
	return path, version, nil
}

// cliEnv puts the CLI's own directory first on PATH: pi's pre-1.0 installer
// keeps its Node there, which a daemon's PATH may lack (see pi's runtime).
func cliEnv(path string) []string {
	return append(os.Environ(), "PATH="+filepath.Dir(path)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// Update starts the agent's update command; poll List for the outcome.
func (m *Manager) Update(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.spec.Name != name {
			continue
		}
		if e.status.Updating {
			return ErrBusy
		}
		e.status.Updating = true
		e.status.Output = ""
		e.status.UpdateError = ""
		go m.update(e)
		return nil
	}
	return ErrUnknown
}

func (m *Manager) update(e *entry) {
	logger := m.logger.With("backend", e.spec.Name)
	logger.Info("agent update started", "command", e.status.Command)
	ctx, cancel := context.WithTimeout(m.ctx, updateTimeout)
	defer cancel()

	// Stdin stays /dev/null, so a CLI that prompts fails instead of hanging.
	cmd := exec.CommandContext(ctx, e.spec.Cmd, e.spec.UpdateArgs...)
	cmd.Dir = os.TempDir()
	if path, err := exec.LookPath(e.spec.Cmd); err == nil {
		cmd.Env = cliEnv(path)
	}
	procutil.ConfigureGroup(cmd)
	cmd.Cancel = func() error { return procutil.KillGroup(cmd) }
	cmd.WaitDelay = 5 * time.Second
	out := outputWriter{m: m, e: e}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err != nil {
		logger.Warn("agent update failed", "err", err)
	} else {
		logger.Info("agent update finished")
	}

	// Updating outlasts this lookup, so nobody reads the old version as the
	// outcome.
	found := lookupSpec(m.ctx, e.spec)
	m.mu.Lock()
	defer m.mu.Unlock()
	e.generation++
	e.status.Updating = false
	if err != nil {
		e.status.UpdateError = err.Error()
	}
	m.apply(e, found)
}

// outputWriter keeps the tail of a running update in its Status.
type outputWriter struct {
	m *Manager
	e *entry
}

func (w outputWriter) Write(p []byte) (int, error) {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	output := w.e.status.Output + string(p)
	if len(output) > maxOutput {
		output = output[len(output)-maxOutput:]
	}
	w.e.status.Output = output
	return len(p), nil
}

// compareVersions orders two versionRe matches as semver does, except that
// prerelease tags compare as plain strings.
func compareVersions(a, b string) int {
	aCore, aPre, _ := strings.Cut(a, "-")
	bCore, bPre, _ := strings.Cut(b, "-")
	aParts, bParts := strings.Split(aCore, "."), strings.Split(bCore, ".")
	for i := range min(len(aParts), len(bParts)) {
		x, _ := strconv.Atoi(aParts[i])
		y, _ := strconv.Atoi(bParts[i])
		if x != y {
			return x - y
		}
	}
	switch {
	case aPre == bPre:
		return 0
	case aPre == "":
		return 1
	case bPre == "":
		return -1
	}
	return strings.Compare(aPre, bPre)
}
