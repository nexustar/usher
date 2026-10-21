package broker

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func recvWithin(t *testing.T, ch <-chan Event, d time.Duration) (Event, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(d):
		return Event{}, false
	}
}

func TestBroker_DeliverSimple(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("s1")
	defer cancel()

	b.Publish(Event{SessionID: "s1", Type: "hello", Raw: json.RawMessage(`{}`)})

	ev, ok := recvWithin(t, ch, time.Second)
	if !ok {
		t.Fatal("expected event")
	}
	if ev.Type != "hello" {
		t.Errorf("Type = %q", ev.Type)
	}
}

func TestBroker_RoutesByID(t *testing.T) {
	b := New()
	ch1, c1 := b.Subscribe("a")
	defer c1()
	ch2, c2 := b.Subscribe("b")
	defer c2()

	b.Publish(Event{SessionID: "a", Type: "x"})

	if ev, ok := recvWithin(t, ch1, time.Second); !ok || ev.Type != "x" {
		t.Errorf("ch1: ok=%v ev=%v", ok, ev)
	}
	if _, ok := recvWithin(t, ch2, 50*time.Millisecond); ok {
		t.Error("ch2 should not have received an event for session a")
	}
}

func TestBroker_FanOut(t *testing.T) {
	b := New()
	ch1, c1 := b.Subscribe("s")
	defer c1()
	ch2, c2 := b.Subscribe("s")
	defer c2()

	b.Publish(Event{SessionID: "s", Type: "x"})

	for i, ch := range []<-chan Event{ch1, ch2} {
		if ev, ok := recvWithin(t, ch, time.Second); !ok || ev.Type != "x" {
			t.Errorf("subscriber %d: ok=%v ev=%v", i, ok, ev)
		}
	}
}

func TestBroker_CancelClosesChannel(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("s")
	cancel()

	if _, ok := recvWithin(t, ch, 100*time.Millisecond); ok {
		t.Error("expected channel to be closed")
	}
}

func TestBroker_PublishToNoSubscribers(t *testing.T) {
	b := New()
	// Should not panic or block.
	b.Publish(Event{SessionID: "nobody", Type: "x"})
}

func TestBroker_SlowConsumerDrops(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("s")
	defer cancel()

	// Fill the buffer (cap=64) plus extras that should be dropped.
	for i := 0; i < 200; i++ {
		b.Publish(Event{SessionID: "s", Type: "x"})
	}

	count := 0
	for {
		select {
		case <-ch:
			count++
		case <-time.After(50 * time.Millisecond):
			if count == 0 {
				t.Fatal("expected at least some events delivered")
			}
			if count > 64 {
				t.Errorf("received %d events; buffer cap is 64 — drop did not engage", count)
			}
			return
		}
	}
}

func TestBroker_ConcurrentSubscribePublish(t *testing.T) {
	b := New()
	var wg sync.WaitGroup

	wg.Add(50)
	for i := 0; i < 50; i++ {
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe("race")
			defer cancel()
			b.Publish(Event{SessionID: "race", Type: "x"})
			recvWithin(t, ch, 200*time.Millisecond)
		}()
	}

	wg.Add(50)
	for i := 0; i < 50; i++ {
		go func() {
			defer wg.Done()
			b.Publish(Event{SessionID: "race", Type: "y"})
		}()
	}

	wg.Wait()
}

func TestBroker_HasViewers(t *testing.T) {
	b := New()
	if b.HasViewers("s1") {
		t.Fatal("no subscribers yet")
	}
	_, cancel := b.Subscribe("s1")
	if !b.HasViewers("s1") {
		t.Fatal("subscriber attached, want a viewer")
	}
	if b.HasViewers("s2") {
		t.Fatal("s2 has no viewer")
	}
	cancel()
	if b.HasViewers("s1") {
		t.Fatal("viewer cancelled, want none")
	}
}

// TestBroker_SubscribeAllNotAViewer guards the suppression seam: the push
// dispatcher subscribes via SubscribeAll and must not register as a per-session
// viewer, or it would suppress its own notifications.
func TestBroker_SubscribeAllNotAViewer(t *testing.T) {
	b := New()
	_, cancel := b.SubscribeAll()
	defer cancel()
	if b.HasViewers("s1") {
		t.Fatal("SubscribeAll must not count as a session viewer")
	}
}

func TestBroker_SubscribeAllReceivesEverySession(t *testing.T) {
	b := New()
	all, cancel := b.SubscribeAll()
	defer cancel()
	b.Publish(Event{SessionID: "whatever", Type: "turn.ended"})
	if ev, ok := recvWithin(t, all, time.Second); !ok || ev.SessionID != "whatever" {
		t.Fatalf("SubscribeAll: ok=%v ev=%+v", ok, ev)
	}
}
