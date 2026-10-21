package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nexustar/usher/pkg/agentprofile"
	"github.com/nexustar/usher/pkg/core"
)

// An omitted approval keeps the agent profile's; a given one wins.
func TestCreateSessionApprovalOverridesAgent(t *testing.T) {
	agents := agentprofile.New([]agentprofile.Profile{{Name: "dev", Approval: core.ApprovalFull}})
	resolve := func(body string) string {
		t.Helper()
		var req CreateRequest
		if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
			t.Fatal(err)
		}
		opts, err := req.Options(agents)
		if err != nil {
			t.Fatal(err)
		}
		return opts.Approval
	}
	if got := resolve(`{"agent":"dev"}`); got != core.ApprovalFull {
		t.Errorf("omitted approval = %q, want the agent's full", got)
	}
	if got := resolve(`{"agent":"dev","approval":"ask"}`); got != core.ApprovalAsk {
		t.Errorf("explicit ask = %q", got)
	}
}
