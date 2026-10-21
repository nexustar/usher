package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient returns a client pointed at a handler that inspects the
// request and writes a canned envelope. The handler receives the method name
// (last path segment) and the decoded JSON body.
func newTestClient(t *testing.T, handler func(method string, body map[string]any) (ok bool, result any, desc string, code int)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		method := parts[len(parts)-1]
		if !strings.HasPrefix(parts[len(parts)-2], "bot") {
			t.Errorf("path missing bot token segment: %s", r.URL.Path)
		}
		var body map[string]any
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
		}
		ok, result, desc, code := handler(method, body)
		resultJSON, _ := json.Marshal(result)
		env := apiResponse{OK: ok, Result: resultJSON, Description: desc, ErrorCode: code}
		_ = json.NewEncoder(w).Encode(env)
	}))
	t.Cleanup(srv.Close)
	return NewClient("TESTTOKEN", srv.URL)
}

func TestRateLimitRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// First call: 429 with a 0s retry_after (loop floors it to ~1s, but
			// the test still completes quickly since we just assert the retry).
			_ = json.NewEncoder(w).Encode(apiResponse{
				OK: false, ErrorCode: 429, Description: "Too Many Requests",
				Parameters: struct {
					RetryAfter int `json:"retry_after"`
				}{RetryAfter: 0},
			})
			return
		}
		resultJSON, _ := json.Marshal(User{ID: 5})
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: resultJSON})
	}))
	t.Cleanup(srv.Close)

	c := NewClient("T", srv.URL)
	u, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe after 429 retry: %v", err)
	}
	if u.ID != 5 {
		t.Errorf("u.ID = %d, want 5", u.ID)
	}
	if calls != 2 {
		t.Errorf("expected 1 retry (2 calls), got %d", calls)
	}
}

func TestAPIError(t *testing.T) {
	c := newTestClient(t, func(_ string, _ map[string]any) (bool, any, string, int) {
		return false, nil, "Bad Request: chat not found", 400
	})
	_, err := c.GetMe(context.Background())
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err type = %T, want *APIError", err)
	}
	if apiErr.Code != 400 || !strings.Contains(apiErr.Description, "chat not found") {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "getMe") {
		t.Errorf("Error() should name the method: %s", apiErr.Error())
	}
}
