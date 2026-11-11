package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
	"github.com/nexustar/usher/pkg/procutil"
)

// The URL `claude auth login` prints redirects to a page showing a code to
// paste back, so it works from a browser on any machine.
var loginURL = regexp.MustCompile(`https://\S*oauth/authorize\S*`)

func (m *Manager) StartLogin(ctx context.Context) (backend.Login, error) {
	if _, err := exec.LookPath(m.bin); err != nil {
		return backend.Login{}, core.ErrCLINotFound
	}
	m.logger.Info("login started")
	return m.login.Start(ctx, &loginFlow{bin: m.bin, logger: m.logger})
}

func (m *Manager) SubmitLoginCode(code string) error { return m.login.Submit(code) }
func (m *Manager) LoginStatus() backend.Login        { return m.login.Status() }
func (m *Manager) CancelLogin()                      { m.login.Cancel() }

type loginFlow struct {
	bin    string
	logger *slog.Logger
	cmd    *exec.Cmd
	in     io.WriteCloser
	exited chan struct{}
	err    error // cmd.Wait's, once exited closes
	mu     sync.Mutex
	tail   []string // last output lines, for the failure message
}

func (f *loginFlow) Begin(ctx context.Context) (backend.Login, error) {
	f.cmd = exec.CommandContext(context.Background(), f.bin, "auth", "login", "--claudeai")
	procutil.ConfigureGroup(f.cmd)
	f.cmd.Dir = os.TempDir()
	// BROWSER=true: never open a browser on the server.
	f.cmd.Env = append(scrubEnv(""), "BROWSER=true")
	in, err := f.cmd.StdinPipe()
	if err != nil {
		return backend.Login{}, err
	}
	f.in = in
	pr, pw := io.Pipe()
	f.cmd.Stdout, f.cmd.Stderr = pw, pw
	if err := f.cmd.Start(); err != nil {
		return backend.Login{}, err
	}
	f.exited = make(chan struct{})
	go func() { f.err = f.cmd.Wait(); pw.Close(); close(f.exited) }()
	found := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(pr)
		for s.Scan() {
			line := s.Text()
			f.mu.Lock()
			f.tail = append(f.tail, line)
			if len(f.tail) > 5 {
				f.tail = f.tail[1:]
			}
			f.mu.Unlock()
			if u := loginURL.FindString(line); u != "" {
				select {
				case found <- u:
				default:
				}
			}
		}
		close(found)
	}()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case <-ctx.Done():
		return backend.Login{}, fmt.Errorf("claude auth login: %w", ctx.Err())
	case u, ok := <-found:
		if !ok {
			return backend.Login{}, f.failure(errors.New("exited before printing a sign-in URL"))
		}
		return backend.Login{URL: u, NeedsCode: true}, nil
	}
}

func (f *loginFlow) Submit(code string) error {
	code = strings.TrimSpace(code)
	if code == "" || strings.ContainsAny(code, "\r\n") {
		return errors.New("invalid code")
	}
	_, err := io.WriteString(f.in, code+"\n")
	return err
}

func (f *loginFlow) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.exited:
	}
	if f.err != nil {
		return f.failure(f.err)
	}
	f.logger.Info("login finished")
	return nil
}

func (f *loginFlow) Close() {
	if f.in != nil {
		_ = f.in.Close()
	}
	if f.cmd != nil && f.cmd.Process != nil {
		_ = procutil.KillGroup(f.cmd)
	}
}

// failure prefers the CLI's own last words over the exit status.
func (f *loginFlow) failure(err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.tail) - 1; i >= 0; i-- {
		line := f.tail[i]
		// Output after the paste prompt lands on its line.
		if _, after, ok := strings.Cut(line, "prompted >"); ok {
			line = after
		}
		if line = strings.TrimSpace(line); line != "" {
			return errors.New(line)
		}
	}
	return err
}
