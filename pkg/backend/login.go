package backend

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Login is a sign-in in progress, shown to the user to finish in any browser.
type Login struct {
	URL       string `json:"url,omitempty"`
	UserCode  string `json:"user_code,omitempty"`  // to enter at URL
	NeedsCode bool   `json:"needs_code,omitempty"` // the page shows a code to paste back
	State     string `json:"state,omitempty"`      // pending, done or failed; empty when none
	Error     string `json:"error,omitempty"`
}

// LoginProvider signs the backend's CLI in to a subscription.
type LoginProvider interface {
	StartLogin(ctx context.Context) (Login, error)
	SubmitLoginCode(code string) error
	LoginStatus() Login
	CancelLogin()
}

// LoginFlow is one attempt: Begin returns the prompt, Wait blocks until the
// CLI reports the outcome, Close stops it.
type LoginFlow interface {
	Begin(ctx context.Context) (Login, error)
	Submit(code string) error
	Wait(ctx context.Context) error
	Close()
}

// LoginTimeout matches the device code's lifetime.
const LoginTimeout = 15 * time.Minute

// LoginRunner keeps at most one attempt per backend; a new one replaces it.
type LoginRunner struct {
	mu     sync.Mutex
	cur    Login
	flow   LoginFlow
	cancel context.CancelFunc
}

func (r *LoginRunner) Start(ctx context.Context, f LoginFlow) (Login, error) {
	r.Cancel()
	l, err := f.Begin(ctx)
	if err != nil {
		f.Close()
		return Login{}, err
	}
	l.State = "pending"
	wctx, cancel := context.WithTimeout(context.Background(), LoginTimeout)
	r.mu.Lock()
	r.cur, r.flow, r.cancel = l, f, cancel
	r.mu.Unlock()
	go func() {
		err := f.Wait(wctx)
		f.Close()
		cancel()
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.flow != f {
			return
		}
		r.flow = nil
		r.cur.State = "done"
		if errors.Is(err, context.DeadlineExceeded) {
			err = errors.New("sign-in timed out")
		}
		if err != nil {
			r.cur.State, r.cur.Error = "failed", err.Error()
		}
	}()
	return l, nil
}

func (r *LoginRunner) Submit(code string) error {
	r.mu.Lock()
	f := r.flow
	r.mu.Unlock()
	if f == nil {
		return errors.New("no sign-in in progress")
	}
	return f.Submit(code)
}

func (r *LoginRunner) Status() Login {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

func (r *LoginRunner) Cancel() {
	r.mu.Lock()
	cancel := r.cancel
	r.cur, r.flow, r.cancel = Login{}, nil, nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
