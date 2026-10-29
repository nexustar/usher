package backend

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// UsageWindow is one rate-limit window, keyed by length (300 = 5h).
type UsageWindow struct {
	Minutes     int       `json:"window_minutes"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// Usage is an account's subscription limits.
type Usage struct {
	Plan      string        `json:"plan,omitempty"`
	Windows   []UsageWindow `json:"windows"`
	UpdatedAt time.Time     `json:"updated_at"`
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

// Get probes only on refresh; a failed probe keeps the previous value.
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
	}
	c.mu.Unlock()
	if err != nil {
		last, _ := c.snapshot()
		return last, err
	}
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
			if c.last.Windows[i].Minutes == w.Minutes {
				c.last.Windows[i], replaced = w, true
			}
		}
		if !replaced {
			c.last.Windows = append(c.last.Windows, w)
		}
	}
	c.last.UpdatedAt = time.Now()
}
