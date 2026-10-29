package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/nexustar/usher/pkg/backend"
	"github.com/nexustar/usher/pkg/procutil"
)

const usageProbeTimeout = 20 * time.Second

var usageWindows = map[string]int{"five_hour": 300, "seven_day": 10080}

// Usage probes with a throwaway process asked get_usage before any turn: no
// tokens, no transcript.
func (m *Manager) Usage(ctx context.Context, refresh bool) (backend.Usage, error) {
	if _, err := exec.LookPath(m.bin); err != nil {
		return backend.Usage{}, backend.ErrNoUsage
	}
	return m.usage.Get(ctx, refresh, m.probeUsage)
}

func (m *Manager) probeUsage(ctx context.Context) (backend.Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, usageProbeTimeout)
	defer cancel()
	// No hooks or MCP servers.
	cmd := exec.CommandContext(context.Background(), m.bin,
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence", "--strict-mcp-config", "--settings", `{"disableAllHooks":true}`)
	procutil.ConfigureGroup(cmd)
	cmd.Dir = os.TempDir()
	cmd.Env = scrubEnv("")
	in, err := cmd.StdinPipe()
	if err != nil {
		return backend.Usage{}, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return backend.Usage{}, err
	}
	if err := cmd.Start(); err != nil {
		return backend.Usage{}, err
	}
	start := time.Now()
	defer func() {
		_ = in.Close()
		_ = procutil.KillGroup(cmd)
		_ = cmd.Wait()
		m.logger.Debug("usage probe done", "took", time.Since(start).Round(time.Millisecond))
	}()
	const requestID = "usher-usage"
	req, _ := json.Marshal(map[string]any{
		"type": "control_request", "request_id": requestID,
		"request": map[string]any{"subtype": "get_usage", "skip_behaviors": true},
	})
	if _, err := in.Write(append(req, '\n')); err != nil {
		return backend.Usage{}, err
	}
	type result struct {
		usage backend.Usage
		err   error
	}
	done := make(chan result, 1)
	go func() {
		s := bufio.NewScanner(out)
		s.Buffer(make([]byte, 64<<10), 16<<20)
		for s.Scan() {
			var msg struct {
				Type     string `json:"type"`
				Response struct {
					Subtype   string          `json:"subtype"`
					RequestID string          `json:"request_id"`
					Error     string          `json:"error"`
					Payload   json.RawMessage `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(s.Bytes(), &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != requestID {
				continue
			}
			if msg.Response.Subtype != "success" {
				done <- result{err: fmt.Errorf("get_usage: %s", msg.Response.Error)}
				return
			}
			u, err := parseUsage(msg.Response.Payload)
			done <- result{u, err}
			return
		}
		done <- result{err: errors.New("claude exited before reporting usage")}
	}()
	select {
	case <-ctx.Done():
		return backend.Usage{}, fmt.Errorf("get_usage: %w", ctx.Err())
	case r := <-done:
		return r.usage, r.err
	}
}

type claudeWindow struct {
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

// parseUsage reads the experimental get_usage response; rate_limits also
// holds lists, so only the two windows are decoded.
func parseUsage(raw json.RawMessage) (backend.Usage, error) {
	var body struct {
		SubscriptionType string `json:"subscription_type"`
		RateLimits       struct {
			FiveHour *claudeWindow `json:"five_hour"`
			SevenDay *claudeWindow `json:"seven_day"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return backend.Usage{}, fmt.Errorf("get_usage: %w", err)
	}
	u := backend.Usage{Plan: body.SubscriptionType, Windows: []backend.UsageWindow{}}
	for minutes, w := range map[int]*claudeWindow{300: body.RateLimits.FiveHour, 10080: body.RateLimits.SevenDay} {
		if w != nil {
			u.Windows = append(u.Windows, backend.UsageWindow{Minutes: minutes, UsedPercent: w.Utilization, ResetsAt: w.ResetsAt})
		}
	}
	slices.SortFunc(u.Windows, func(a, b backend.UsageWindow) int { return a.Minutes - b.Minutes })
	return u, nil
}

// observeRateLimit reads a rate_limit_event: one window, utilization 0-1.
func (m *Manager) observeRateLimit(raw []byte) {
	var e struct {
		Info struct {
			Type        string   `json:"rateLimitType"`
			Utilization *float64 `json:"utilization"`
			ResetsAt    int64    `json:"resetsAt"`
		} `json:"rate_limit_info"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Info.Utilization == nil {
		return
	}
	minutes, ok := usageWindows[e.Info.Type]
	if !ok {
		return
	}
	w := backend.UsageWindow{Minutes: minutes, UsedPercent: *e.Info.Utilization * 100}
	if e.Info.ResetsAt > 0 {
		w.ResetsAt = time.Unix(e.Info.ResetsAt, 0)
	}
	m.usage.Observe("", w)
}
