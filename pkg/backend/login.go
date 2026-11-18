package backend

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Login is a sign-in in progress.
type Login struct {
	URL       string `json:"url,omitempty"`
	UserCode  string `json:"user_code,omitempty"`  // to enter at URL
	NeedsCode bool   `json:"needs_code,omitempty"` // the page shows a code to paste back
	State     string `json:"state,omitempty"`      // starting, pending, done or failed; empty when none
	Error     string `json:"error,omitempty"`
}

type LoginProvider interface {
	StartLogin(ctx context.Context) (Login, error)
	SubmitLoginCode(code string) error
	LoginStatus() Login
	CancelLogin()
}

// LoginFlow is one attempt: Begin returns the prompt, Wait the outcome.
type LoginFlow interface {
	Begin(ctx context.Context) (Login, error)
	Submit(code string) error
	Wait(ctx context.Context) error
	Close()
}

// LoginTimeout matches the device code's lifetime.
const LoginTimeout = 15 * time.Minute

// LoginRunner holds one attempt; a new one replaces it.
type LoginRunner struct {
	mu     sync.Mutex
	cur    Login
	flow   LoginFlow
	cancel context.CancelFunc
}

var ErrLoginCanceled = errors.New("sign-in canceled")

func (r *LoginRunner) Start(ctx context.Context, f LoginFlow) (Login, error) {
	r.Cancel()
	// Registered before Begin, so canceling while the CLI starts stops it too.
	bctx, bcancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.cur, r.flow, r.cancel = Login{State: "starting"}, f, bcancel
	r.mu.Unlock()
	l, err := f.Begin(bctx)
	bcancel()
	r.mu.Lock()
	if r.flow != f {
		r.mu.Unlock()
		f.Close()
		return Login{}, ErrLoginCanceled
	}
	if err != nil {
		r.cur, r.flow, r.cancel = Login{State: "failed", Error: err.Error()}, nil, nil
		r.mu.Unlock()
		f.Close()
		return Login{}, err
	}
	l.State = "pending"
	wctx, cancel := context.WithTimeout(context.Background(), LoginTimeout)
	r.cur, r.cancel = l, cancel
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
	f, state := r.flow, r.cur.State
	r.mu.Unlock()
	if f == nil || state != "pending" {
		return errors.New("no sign-in waiting for a code")
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
