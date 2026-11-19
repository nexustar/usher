package codex

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDeliverCallbackFetchesOnlyTheWaitingCallback(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback/x" && r.URL.Query().Get("code") == "c1" {
			hits++
		}
	}))
	defer srv.Close()
	redirect := srv.URL + "/callback/x"
	ctx := context.Background()
	for _, bad := range []string{
		"http://example.com/callback/x?code=c1", // not loopback
		srv.URL + "/other?code=c1",              // another path
		"http://127.0.0.1:1/callback/x?code=c1", // another port
		"https://" + srv.Listener.Addr().String() + "/callback/x?code=c1",
	} {
		if err := deliverCallback(ctx, redirect, bad); err == nil {
			t.Errorf("delivered %s", bad)
		}
	}
	if err := deliverCallback(ctx, "", srv.URL+"/callback/x?code=c1"); err == nil {
		t.Error("delivered with no sign-in in progress")
	}
	if err := deliverCallback(ctx, redirect, redirect+"?code=c1&state=s"); err != nil || hits != 1 {
		t.Fatalf("err = %v, hits = %d", err, hits)
	}
}
