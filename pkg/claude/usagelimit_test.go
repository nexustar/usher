package claude

import (
	"strings"
	"testing"
)

func runLimitTurn(t *testing.T, lines ...string) Result {
	t.Helper()
	req := &turnRequest{done: make(chan Result, 1), deltas: make(chan Delta, 4), uuid: "u1"}
	p := &process{id: "s", turns: []*turnRequest{req}}
	m := &Manager{}
	m.readLoop(p, strings.NewReader(strings.Join(lines, "\n")+"\n"))
	select {
	case res := <-req.done:
		return res
	default:
		t.Fatal("turn did not finish")
		return Result{}
	}
}

func TestUsageLimitResetFromAPIErrorParams(t *testing.T) {
	res := runLimitTurn(t,
		`{"type":"command_lifecycle","command_uuid":"u1","state":"started"}`,
		`{"type":"assistant","error":"rate_limit","api_error":"usage_limit_reached","api_error_params":{"rate_limit_info":{"status":"rejected","resetsAt":1790000000,"rateLimitType":"five_hour"}},"message":{"model":"<synthetic>","content":[{"type":"text","text":"You've hit your limit"}]}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
		`{"type":"command_lifecycle","command_uuid":"u1","state":"cancelled"}`,
	)
	if res.LimitResetsAt != 1790000000 || res.Subtype != "cancelled" {
		t.Fatalf("result = %+v", res)
	}
}

func TestUsageLimitResetFromRejectedRateLimitEvent(t *testing.T) {
	res := runLimitTurn(t,
		`{"type":"command_lifecycle","command_uuid":"u1","state":"started"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1790000123},"uuid":"x","session_id":"s"}`,
		`{"type":"assistant","error":"rate_limit","message":{"model":"<synthetic>","content":[]}}`,
		`{"type":"command_lifecycle","command_uuid":"u1","state":"cancelled"}`,
	)
	if res.LimitResetsAt != 1790000123 {
		t.Fatalf("result = %+v", res)
	}
}

func TestUsageLimitIgnoresOtherErrorsAndWarnings(t *testing.T) {
	// A warning carries a reset too, and a server throttle shares error
	// rate_limit; neither is a refused turn with a known reset.
	res := runLimitTurn(t,
		`{"type":"command_lifecycle","command_uuid":"u1","state":"started"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1790000000},"uuid":"x","session_id":"s"}`,
		`{"type":"assistant","error":"rate_limit","message":{"model":"<synthetic>","content":[]}}`,
		`{"type":"assistant","error":"server_error","api_error":"no_response","api_error_params":{"rate_limit_info":{"resetsAt":1790000000}},"message":{"model":"<synthetic>","content":[]}}`,
		`{"type":"command_lifecycle","command_uuid":"u1","state":"completed"}`,
	)
	if res.LimitResetsAt != 0 {
		t.Fatalf("result = %+v", res)
	}
}
