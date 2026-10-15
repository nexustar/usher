// Package broker is a small in-process pub/sub for session events.
//
// Subscribers receive a buffered channel; if the consumer is slow and the
// buffer fills, events are dropped silently rather than blocking the
// publisher. This keeps the sender's stream goroutine moving even when an
// SSE client stalls.
package broker

import (
	"encoding/json"
	"sync"
)

type Event struct {
	SessionID string          `json:"session_id"`
	Type      string          `json:"type"`
	Raw       json.RawMessage `json:"data,omitempty"`
}

type Broker struct {
	mu   sync.RWMutex
	subs map[string]map[*subscriber]struct{} // sessionID -> set
	all  map[*subscriber]struct{}            // subscribers to every session's events
}

type subscriber struct {
	ch chan Event
}

func New() *Broker {
	return &Broker{
		subs: map[string]map[*subscriber]struct{}{},
		all:  map[*subscriber]struct{}{},
	}
}

// Subscribe returns a buffered channel of events for sessionID and a cancel
// function to stop receiving and free the slot.
func (b *Broker) Subscribe(sessionID string) (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, 64)}
	b.mu.Lock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[*subscriber]struct{}{}
	}
	b.subs[sessionID][sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[sessionID]; ok {
				delete(set, sub)
				if len(set) == 0 {
					delete(b.subs, sessionID)
				}
			}
			b.mu.Unlock()
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

// SubscribeAll returns a buffered channel receiving events for EVERY session,
// plus a cancel function. Used by consumers that watch all sessions at once
// (the web-push dispatcher) rather than one open session (an SSE client). Same
// drop-on-slow-consumer semantics as Subscribe.
func (b *Broker) SubscribeAll() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, 256)}
	b.mu.Lock()
	b.all[sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.all, sub)
			b.mu.Unlock()
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

// HasViewers reports whether any per-session subscriber is currently attached
// to sessionID — i.e. a browser has its /events stream open on that session.
// Global (SubscribeAll) subscribers don't count, so the push dispatcher asking
// this never sees itself. Used to suppress notifications for the session a user
// is actively looking at.
func (b *Broker) HasViewers(sessionID string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[sessionID]) > 0
}

// Publish delivers ev to every current subscriber of ev.SessionID and to every
// global (SubscribeAll) subscriber. The non-blocking sends run while holding the
// read lock so a concurrent cancel — which closes a channel under the write lock
// — can't close a channel mid-send. Each send is non-blocking (drop-on-full), so
// holding the lock never stalls the publisher.
func (b *Broker) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	send := func(s *subscriber) {
		select {
		case s.ch <- ev:
		default:
			// Slow consumer: drop. Prefer recoverable behavior over
			// blocking the publisher and stalling the whole pipeline.
		}
	}
	for s := range b.subs[ev.SessionID] {
		send(s)
	}
	for s := range b.all {
		send(s)
	}
}
