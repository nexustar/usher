package backend

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// UsageWindow is one rate-limit window, keyed by length (300 = 5h) and, for
// a limit on one model only, that model.
type UsageWindow struct {
	Minutes     int       `json:"window_minutes"`
	Model       string    `json:"model,omitempty"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// Account is who the CLI is signed in as, from its own status check.
type Account struct {
	SignedIn bool   `json:"signed_in"`
	Method   string `json:"method,omitempty"` // e.g. claude.ai, chatgpt, apiKey
	Email    string `json:"email,omitempty"`
	Plan     string `json:"plan,omitempty"`
	// When the CLI will need signing in again; zero when it doesn't say.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// Usage is an account's subscription limits.
type Usage struct {
	Account   *Account      `json:"account,omitempty"`
	Plan      string        `json:"plan,omitempty"`
	Windows   []UsageWindow `json:"windows"`
	UpdatedAt time.Time     `json:"updated_at,omitzero"`
}

// ErrNoUsage reports a backend with no limits to show, such as one whose CLI
// is not installed.
var ErrNoUsage = errors.New("no usage")

// UsageProvider is optional. Only refresh may start a process.
type UsageProvider interface {
	Usage(ctx context.Context, refresh bool) (Usage, error)
}

// UsageCache holds the last usage probed or reported by a live session.
type UsageCache struct {
	probeMu sync.Mutex
	mu      sync.Mutex
	last    Usage
	err     error
}

// Get probes only on refresh. A probe that fails on the limits keeps the
// previous ones but still records the account it found.
func (c *UsageCache) Get(ctx context.Context, refresh bool, probe func(context.Context) (Usage, error)) (Usage, error) {
	if !refresh {
		return c.snapshot()
	}
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	u, err := probe(ctx)
	c.mu.Lock()
	if ctx.Err() == nil {
		c.err = err
	}
	if err == nil {
		u.UpdatedAt = time.Now()
		c.last = u
	} else if u.Account != nil {
		c.last.Account = u.Account
	}
	c.mu.Unlock()
	return c.snapshot()
}

func (c *UsageCache) snapshot() (Usage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.last
	u.Windows = slices.Clone(u.Windows)
	return u, c.err
}

// Observe merges windows reported by a live session.
func (c *UsageCache) Observe(plan string, windows ...UsageWindow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if plan != "" {
		c.last.Plan = plan
	}
	for _, w := range windows {
		replaced := false
		for i := range c.last.Windows {
			if c.last.Windows[i].Minutes == w.Minutes && c.last.Windows[i].Model == w.Model {
				c.last.Windows[i], replaced = w, true
			}
		}
		if !replaced {
			c.last.Windows = append(c.last.Windows, w)
		}
	}
	c.last.UpdatedAt = time.Now()
}
