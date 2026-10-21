package interaction

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nexustar/usher/pkg/core"
)

func TestManager_SubmitAndRespond(t *testing.T) {
	m := New()

	var (
		gotResp Response
		gotErr  error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		gotResp, gotErr = m.Submit(context.Background(), Request{
			SessionID: "s1",
			ToolName:  "Bash",
		})
	}()

	id := waitForPending(t, m)
	if err := m.Respond(id, Response{Behavior: "allow", Reason: "ok"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit did not return")
	}
	if gotErr != nil {
		t.Fatalf("err = %v", gotErr)
	}
	if gotResp.Behavior != "allow" || gotResp.Reason != "ok" {
		t.Errorf("Resp = %+v", gotResp)
	}
	if len(m.List()) != 0 {
		t.Errorf("expected empty list after respond, got %d", len(m.List()))
	}
}

func TestManagerDeduplicatesToolUseID(t *testing.T) {
	m := New()
	ev := Request{SessionID: "s", ToolUseID: "tool-1", ToolName: "Bash"}
	results := make(chan Response, 2)
	for range 2 {
		go func() { r, _ := m.Submit(context.Background(), ev); results <- r }()
	}
	deadline := time.Now().Add(time.Second)
	for len(m.List()) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pending := m.List()
	if len(pending) != 1 {
		t.Fatalf("pending=%d, want 1", len(pending))
	}
	if err := m.Respond(pending[0].ID, Response{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r := <-results; r.Behavior != "allow" {
			t.Fatalf("response=%+v", r)
		}
	}
}

func TestManager_ContextCancelReleases(t *testing.T) {
	m := New()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := m.Submit(ctx, Request{SessionID: "s"})
		done <- err
	}()

	waitForPending(t, m)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Error("expected ctx error")
		}
	case <-time.After(time.Second):
		t.Fatal("Submit did not unblock on ctx cancel")
	}
}

func TestManager_RespondUnknown(t *testing.T) {
	m := New()
	if err := m.Respond("nope", Response{Behavior: "allow"}); err == nil {
		t.Error("expected error")
	}
}

func TestManager_RespondTwice(t *testing.T) {
	m := New()
	go func() { _, _ = m.Submit(context.Background(), Request{SessionID: "s"}) }()
	id := waitForPending(t, m)
	if err := m.Respond(id, Response{Behavior: "allow"}); err != nil {
		t.Fatal(err)
	}
	// the entry will be deleted shortly after; second Respond should fail.
	// Loop a few ms to outlast the deletion goroutine.
	time.Sleep(50 * time.Millisecond)
	if err := m.Respond(id, Response{Behavior: "allow"}); err == nil {
		t.Error("expected error on second respond")
	}
}

func TestManagerSessionScopeRequiresBackendSupport(t *testing.T) {
	m := New()
	go func() { _, _ = m.Submit(context.Background(), Request{SessionID: "s", ToolName: "Bash"}) }()
	id := waitForPending(t, m)
	if err := m.Respond(id, Response{Behavior: "allow", Scope: "always"}); err == nil {
		t.Fatal("always scope succeeded without backend support")
	}
	if err := m.Respond(id, Response{Behavior: "deny", Scope: "always"}); err == nil {
		t.Fatal("session-scoped deny succeeded")
	}
	if err := m.Respond(id, Response{Behavior: "deny", Scope: "once"}); err != nil {
		t.Fatal(err)
	}
}

func TestManagerPassesSupportedSessionScopeToBackend(t *testing.T) {
	m := New()
	result := make(chan Response, 1)
	go func() {
		r, _ := m.Submit(context.Background(), Request{SessionID: "s", ToolName: "Bash", AllowAlways: true})
		result <- r
	}()
	id := waitForPending(t, m)
	pending := m.List()
	if len(pending) != 1 || !pending[0].AllowAlways {
		t.Fatalf("pending = %+v, want allow_always", pending)
	}
	if err := m.Respond(id, Response{Behavior: "allow", Scope: "always"}); err != nil {
		t.Fatal(err)
	}
	if got := <-result; got.Scope != "always" || got.Behavior != "allow" {
		t.Fatalf("response = %+v", got)
	}
}

func TestManager_ConcurrentSubmits(t *testing.T) {
	m := New()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				_, _ = m.Submit(ctx, Request{SessionID: "s"})
				close(done)
			}()
			// wait briefly then cancel
			time.Sleep(10 * time.Millisecond)
			cancel()
			<-done
		}()
	}
	wg.Wait()
	if n := len(m.List()); n != 0 {
		t.Errorf("leaked %d pending entries", n)
	}
}

func TestQuickDecide(t *testing.T) {
	m := New()

	// Empty manager: no decision yet, caller must block on UI.
	if resp, ok := m.QuickDecide(Request{SessionID: "s", ToolName: "Read"}); ok {
		t.Errorf("expected (zero,false) on empty manager; got (%+v,%v)", resp, ok)
	}

	// Full settles instantly.
	m.SetApproval("s", core.ApprovalFull)
	resp, ok := m.QuickDecide(Request{SessionID: "s", ToolName: "Bash"})
	if !ok || resp.Behavior != "allow" {
		t.Errorf("full QuickDecide = (%+v,%v); want allow/true", resp, ok)
	}
	m.SetApproval("s", "")
}

// AskUserQuestion must never be settled by QuickDecide: a bare "allow" from
// auto-approve would let the tool block on the pane TUI
// selector instead of being answered through the web UI.
func TestQuickDecideSkipsAskUserQuestion(t *testing.T) {
	m := New()
	m.SetApproval("s", core.ApprovalFull)
	if resp, ok := m.QuickDecide(Request{SessionID: "s", ToolName: "AskUserQuestion"}); ok {
		t.Errorf("AskUserQuestion under auto-approve = (%+v,%v); want (zero,false)", resp, ok)
	}
	// Sanity: a different tool in the same session is still auto-approved.
	if _, ok := m.QuickDecide(Request{SessionID: "s", ToolName: "Read"}); !ok {
		t.Errorf("Read under auto-approve should still settle")
	}
}

func TestApprovalAsksUntilSetAndFullReleasesWaiting(t *testing.T) {
	m := New()
	if got := m.Approval("s"); got != core.ApprovalAsk {
		t.Fatalf("unset approval = %q, want ask", got)
	}
	done := make(chan Response, 1)
	go func() {
		r, _ := m.Submit(context.Background(), Request{SessionID: "s", ToolName: "Bash"})
		done <- r
	}()
	waitForPending(t, m)
	m.SetApproval("s", core.ApprovalFull)
	select {
	case r := <-done:
		if r.Behavior != "allow" {
			t.Fatalf("waiting permission = %+v, want allow", r)
		}
	case <-time.After(time.Second):
		t.Fatal("entering full left the permission waiting")
	}
}

func waitForPending(t *testing.T, m *Manager) string {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		list := m.List()
		if len(list) > 0 {
			return list[0].ID
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no pending interaction registered")
	return ""
}
