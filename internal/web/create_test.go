package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// An omitted auto_approve keeps the agent profile's default; an explicit false
// turns it off for this one session.
func TestCreateSessionAutoApproveDistinguishesOmittedFromFalse(t *testing.T) {
	decode := func(body string) *bool {
		t.Helper()
		var req createSessionRequest
		if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
			t.Fatal(err)
		}
		return req.AutoApprove
	}

	if got := decode(`{"agent":"dev"}`); got != nil {
		t.Errorf("omitted auto_approve = %v, want nil", *got)
	}
	if got := decode(`{"agent":"dev","auto_approve":false}`); got == nil || *got {
		t.Errorf("explicit false = %v, want non-nil false", got)
	}
	if got := decode(`{"agent":"dev","auto_approve":true}`); got == nil || !*got {
		t.Errorf("explicit true = %v, want non-nil true", got)
	}
}
