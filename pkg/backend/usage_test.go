package backend

import (
	"context"
	"testing"
)

func TestUsageCacheProbesOnlyOnRefresh(t *testing.T) {
	var c UsageCache
	probes := 0
	probe := func(context.Context) (Usage, error) {
		probes++
		return Usage{Plan: "max", Windows: []UsageWindow{{Minutes: 300, UsedPercent: float64(probes)}}}, nil
	}
	ctx := context.Background()
	u, err := c.Get(ctx, false, probe)
	if err != nil || probes != 0 || !u.UpdatedAt.IsZero() {
		t.Fatalf("plain read: probes = %d, usage = %+v, err = %v", probes, u, err)
	}
	c.Get(ctx, true, probe)
	u, _ = c.Get(ctx, false, probe)
	if probes != 1 || u.Plan != "max" || u.UpdatedAt.IsZero() {
		t.Fatalf("after refresh: probes = %d, usage = %+v", probes, u)
	}
}
