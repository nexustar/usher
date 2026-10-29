package codex

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"github.com/nexustar/usher/pkg/backend"
)

const usageProbeTimeout = 20 * time.Second

type rateLimitSnapshot struct {
	LimitID   *string          `json:"limitId"`
	PlanType  *string          `json:"planType"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
}

type rateLimitWindow struct {
	UsedPercent float64 `json:"usedPercent"`
	Minutes     *int    `json:"windowDurationMins"`
	ResetsAt    *int64  `json:"resetsAt"`
}

func (s rateLimitSnapshot) usage() backend.Usage {
	u := backend.Usage{Windows: []backend.UsageWindow{}}
	if s.PlanType != nil {
		u.Plan = *s.PlanType
	}
	for _, w := range []*rateLimitWindow{s.Primary, s.Secondary} {
		if w == nil || w.Minutes == nil {
			continue
		}
		out := backend.UsageWindow{Minutes: *w.Minutes, UsedPercent: w.UsedPercent}
		if w.ResetsAt != nil {
			out.ResetsAt = time.Unix(*w.ResetsAt, 0)
		}
		u.Windows = append(u.Windows, out)
	}
	return u
}

// accountLimit skips snapshots of other limit ids.
func (s rateLimitSnapshot) accountLimit() bool {
	return s.LimitID == nil || *s.LimitID == "codex"
}

// Usage probes with a throwaway app-server that starts no thread.
func (m *Manager) Usage(ctx context.Context, refresh bool) (backend.Usage, error) {
	if _, err := exec.LookPath(m.bin); err != nil {
		return backend.Usage{}, backend.ErrNoUsage
	}
	return m.usage.Get(ctx, refresh, m.probeUsage)
}

func (m *Manager) probeUsage(ctx context.Context) (backend.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, usageProbeTimeout)
	defer cancel()
	c := New(m.bin, nil, nil, nil, m.env, nil, m.logger.With("probe", "usage"))
	defer c.Shutdown()
	if err := c.ensure(ctx); err != nil {
		return backend.Usage{}, err
	}
	var res struct {
		RateLimits rateLimitSnapshot `json:"rateLimits"`
	}
	if err := c.call(ctx, "account/rateLimits/read", nil, &res); err != nil {
		return backend.Usage{}, err
	}
	return res.RateLimits.usage(), nil
}

func (m *Manager) observeRateLimits(params json.RawMessage) {
	var p struct {
		RateLimits rateLimitSnapshot `json:"rateLimits"`
	}
	if json.Unmarshal(params, &p) != nil || !p.RateLimits.accountLimit() {
		return
	}
	u := p.RateLimits.usage()
	m.usage.Observe(u.Plan, u.Windows...)
}
