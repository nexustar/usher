package codex

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/core"
)

// StartLogin uses ChatGPT's device-code flow, which any browser can finish.
func (m *Manager) StartLogin(ctx context.Context) (backend.Login, error) {
	if _, err := exec.LookPath(m.bin); err != nil {
		return backend.Login{}, core.ErrCLINotFound
	}
	m.logger.Info("login started")
	return m.login.Start(ctx, &loginFlow{m: m, logger: m.logger})
}

func (m *Manager) SubmitLoginCode(string) error { return errors.New("codex needs no code") }
func (m *Manager) LoginStatus() backend.Login   { return m.login.Status() }
func (m *Manager) CancelLogin()                 { m.login.Cancel() }

type loginFlow struct {
	m         *Manager
	logger    *slog.Logger
	c         *Client
	id        string
	exited    <-chan struct{}
	completed chan loginResult
}

type loginResult struct {
	id  string
	err error
}

func (f *loginFlow) Begin(ctx context.Context) (backend.Login, error) {
	// Buffered and matched in Wait: a completion may beat the start reply.
	f.completed = make(chan loginResult, 8)
	f.c = New(f.m.bin, nil, nil, nil, f.m.env, nil, f.logger.With("probe", "login"))
	f.c.onAccount = func(method string, params json.RawMessage) {
		if method != "account/login/completed" {
			return
		}
		var p struct {
			LoginID *string `json:"loginId"`
			Success bool    `json:"success"`
			Error   *string `json:"error"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		var r loginResult
		if p.LoginID != nil {
			r.id = *p.LoginID
		}
		var err error
		if !p.Success {
			err = errors.New("sign-in failed")
			if p.Error != nil {
				err = errors.New(*p.Error)
			}
		}
		r.err = err
		select {
		case f.completed <- r:
		default:
		}
	}
	if err := f.c.ensure(ctx); err != nil {
		return backend.Login{}, err
	}
	f.c.mu.Lock()
	f.exited = f.c.waitDone
	f.c.mu.Unlock()
	var res struct {
		LoginID         string `json:"loginId"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	if err := f.c.call(ctx, "account/login/start", map[string]any{"type": "chatgptDeviceCode"}, &res); err != nil {
		return backend.Login{}, err
	}
	f.id = res.LoginID
	return backend.Login{URL: res.VerificationURL, UserCode: res.UserCode}, nil
}

func (f *loginFlow) Submit(string) error { return errors.New("codex needs no code") }

func (f *loginFlow) Wait(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.exited:
			return errors.New("codex app-server exited")
		case r := <-f.completed:
			if r.id != "" && r.id != f.id {
				continue
			}
			if r.err == nil {
				f.logger.Info("login finished")
			}
			return r.err
		}
	}
}

func (f *loginFlow) Close() {
	if f.c != nil {
		f.c.Shutdown()
	}
}
