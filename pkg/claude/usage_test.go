package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// get_usage as Claude Code 2.1 answers it, trimmed.
const getUsageResponse = `{"session":{},"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":35,"resets_at":"2026-10-29T19:30:00.392983+00:00","limit_dollars":null},"seven_day":{"utilization":30,"resets_at":"2026-11-02T11:00:00.393004+00:00"},"seven_day_opus":null,"iguana_necktie":{"utilization":0,"resets_at":"2026-11-05T07:59:00+00:00"},"model_scoped":[],"limits":[{"kind":"session","percent":35}]}}`

func TestProbeUsage(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
printf '%s\n' "$*" > "$0.args"
IFS= read -r line
printf '{"type":"system","subtype":"init"}\n'
printf '{"type":"control_response","response":{"subtype":"success","request_id":"usher-usage","response":%s}}\n' '` + getUsageResponse + `'
cat >/dev/null
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	m := New(script, "", "", nil, 1, nil, nil)
	t.Cleanup(m.Shutdown)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	u, err := m.Usage(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "max" || len(u.Windows) != 2 {
		t.Fatalf("usage = %+v", u)
	}
	w := u.Windows[0]
	if w.Minutes != 300 || w.UsedPercent != 35 || w.ResetsAt.Unix() != time.Date(2026, 10, 29, 19, 30, 0, 0, time.UTC).Unix() {
		t.Fatalf("five_hour = %+v", w)
	}
	if u.Windows[1].Minutes != 10080 || u.Windows[1].UsedPercent != 30 {
		t.Fatalf("seven_day = %+v", u.Windows[1])
	}
	args, _ := os.ReadFile(script + ".args")
	for _, want := range []string{"--no-session-persistence", "--strict-mcp-config", "disableAllHooks"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("args %q lack %s", args, want)
		}
	}
}

func TestObserveRateLimitEvent(t *testing.T) {
	m := New("claude", "", "", nil, 1, nil, nil)
	t.Cleanup(m.Shutdown)
	m.observeRateLimit([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.82,"resetsAt":1793530800}}`))
	m.observeRateLimit([]byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"overage","utilization":0.1}}`))
	u, _ := m.usage.Get(context.Background(), false, nil)
	if len(u.Windows) != 1 || u.Windows[0].Minutes != 10080 || u.Windows[0].UsedPercent != 82 || u.Windows[0].ResetsAt.Unix() != 1793530800 {
		t.Fatalf("usage = %+v", u)
	}
}
