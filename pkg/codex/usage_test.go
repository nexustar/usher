package codex

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nexustar/usher/pkg/backend"
)

func TestRateLimitsUpdatedReachesManager(t *testing.T) {
	m := NewManager("codex", nil, nil, nil, nil, 1, nil)
	t.Cleanup(m.Shutdown)
	c := m.newClient("sid", "", nil, false)
	c.dispatch(rpcMessage{Method: "account/rateLimits/updated", Params: []byte(`{"rateLimits":{"limitId":"codex","planType":"pro","primary":{"usedPercent":5,"windowDurationMins":300,"resetsAt":1785258222},"secondary":{"usedPercent":12,"windowDurationMins":10080,"resetsAt":1785558222}}}`)})
	// A model-scoped limit is not the account's plan limit.
	c.dispatch(rpcMessage{Method: "account/rateLimits/updated", Params: []byte(`{"rateLimits":{"limitId":"codex_other","primary":{"usedPercent":90,"windowDurationMins":300}}}`)})
	u, _ := m.usage.Get(context.Background(), false, nil)
	if u.Plan != "pro" || len(u.Windows) != 2 || u.Windows[0].UsedPercent != 5 || u.Windows[1].UsedPercent != 12 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestUsageHiddenWithoutCLI(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "codex"), nil, nil, nil, nil, 1, nil)
	t.Cleanup(m.Shutdown)
	if _, err := m.Usage(context.Background(), true); !errors.Is(err, backend.ErrNoUsage) {
		t.Fatalf("err = %v", err)
	}
}
