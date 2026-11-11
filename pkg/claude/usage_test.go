package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexustar/usher/pkg/backend"
)

// get_usage as Claude Code 2.1 answers it, trimmed.
const getUsageResponse = `{"session":{},"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":35,"resets_at":"2026-10-29T19:30:00.392983+00:00","limit_dollars":null},"seven_day":{"utilization":30,"resets_at":"2026-11-02T11:00:00.393004+00:00"},"seven_day_opus":null,"iguana_necktie":{"utilization":0,"resets_at":"2026-11-05T07:59:00+00:00"},"model_scoped":[{"display_name":"Fable","utilization":2,"resets_at":"2026-11-02T11:00:00.393004+00:00"}],"limits":[{"kind":"session","percent":35}]}}`

func TestProbeUsage(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := `#!/bin/sh
if [ "$1" = auth ]; then
  if [ -f "$0.out" ]; then echo '{"loggedIn":false,"authMethod":"none"}'; exit 1; fi
  echo '{"loggedIn":true,"authMethod":"claude.ai","email":"a@example.com","subscriptionType":"max","loginExpiresAt":"2026-12-09T07:47:32.154Z"}'
  exit 0
fi
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
	if u.Plan != "max" || len(u.Windows) != 3 || u.Windows[2].Model != "Fable" || u.Windows[2].UsedPercent != 2 || u.Account == nil || *u.Account != (backend.Account{SignedIn: true, Method: "claude.ai", Email: "a@example.com", Plan: "max", ExpiresAt: time.Date(2026, 12, 9, 7, 47, 32, 154e6, time.UTC)}) {
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
	// Signed out: no limits asked for, and the old ones are dropped.
	os.WriteFile(script+".out", nil, 0o600)
	os.Remove(script + ".args")
	u, err = m.Usage(ctx, true)
	if err != nil || u.Account == nil || u.Account.SignedIn || len(u.Windows) != 0 {
		t.Fatalf("signed out: usage = %+v, err = %v", u, err)
	}
	if _, err := os.Stat(script + ".args"); err == nil {
		t.Fatal("get_usage probed while signed out")
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
