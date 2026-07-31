// Package push delivers browser Web Push notifications for usher: a turn
// finishing or a permission prompt arriving while the web UI is closed or
// backgrounded. It is a second consumer of the same event seams the web SSE
// layer uses — broker.SubscribeAll for turn-end, interaction.SubscribePending for
// permission requests — and fans those out to subscribed browsers.
//
// The protocol is implemented from the stdlib only (RFC 8291 message
// encryption, RFC 8292 VAPID), so usher gains push without a new dependency.
package push

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/interaction"
	"github.com/nexustar/usher/internal/textutil"
)

// vapidSubscriber is the VAPID "sub" claim: a contact URL the push service can
// reach if usher's traffic misbehaves. A project URL satisfies the spec without
// leaking a personal address.
const vapidSubscriber = "https://github.com/nexustar/usher"

// Subscription is a browser PushSubscription as serialized by
// PushSubscription.toJSON() and POSTed by the web client.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// SessionInfo is the slice of session metadata a notification needs; supplied
// by a Lookup so push doesn't depend on the router.
type SessionInfo struct {
	Title string
	Cwd   string
}

// Lookup resolves a session id to display metadata; ok is false when the
// session is no longer discoverable.
type Lookup func(sessionID string) (info SessionInfo, ok bool)

// EventSource and PendingSource are the two broker/hook seams push consumes.
// Declared as interfaces so the Manager is testable without the real ones and
// so they document exactly what push needs from each: every session's events,
// plus whether a session is being watched live (to suppress its notifications).
type EventSource interface {
	SubscribeAll() (<-chan broker.Event, func())
	HasViewers(sessionID string) bool
}

type PendingSource interface {
	SubscribePending() (<-chan interaction.Pending, func())
}

// Config wires a Manager. StorePath/KeyPath live under the usher data dir.
type Config struct {
	KeyPath   string // VAPID keypair JSON
	StorePath string // subscriptions JSON
	Lookup    Lookup
	Events    EventSource
	Pending   PendingSource
	Logger    *slog.Logger
}

// Manager owns the VAPID identity, the subscription store, and the dispatch
// loop. It is always constructed (keys are cheap); it simply does nothing while
// no browser has subscribed.
type Manager struct {
	keys    *vapidKeys
	store   *store
	lookup  Lookup
	events  EventSource
	pending PendingSource
	http    *http.Client
	logger  *slog.Logger
}

// New loads or creates the VAPID keys and subscription store. A failure to set
// up keys is fatal to push but the caller may choose to continue without it.
func New(cfg Config) (*Manager, error) {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	keys, err := loadOrCreateVAPID(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("vapid keys: %w", err)
	}
	lookup := cfg.Lookup
	if lookup == nil {
		lookup = func(string) (SessionInfo, bool) { return SessionInfo{}, false }
	}
	return &Manager{
		keys:    keys,
		store:   newStore(cfg.StorePath),
		lookup:  lookup,
		events:  cfg.Events,
		pending: cfg.Pending,
		http:    &http.Client{Timeout: 10 * time.Second},
		logger:  logger,
	}, nil
}

// suppressed reports whether a notification for sessionID should be skipped
// because a browser is actively viewing that session (its /events stream open).
func (m *Manager) suppressed(sessionID string) bool {
	return m.events != nil && m.events.HasViewers(sessionID)
}

// VAPIDPublicKey returns the applicationServerKey the browser needs to
// subscribe.
func (m *Manager) VAPIDPublicKey() string { return m.keys.publicKeyB64() }

// Subscribe records a browser subscription (idempotent by endpoint).
func (m *Manager) Subscribe(sub Subscription) error {
	if sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return fmt.Errorf("incomplete subscription")
	}
	m.store.add(sub)
	return nil
}

// Unsubscribe drops a subscription by endpoint (no error if unknown).
func (m *Manager) Unsubscribe(endpoint string) { m.store.remove(endpoint) }

// Run consumes turn-end and permission events until ctx is cancelled, fanning
// each out to all current subscriptions. Safe to call when Events/Pending are
// nil (it just blocks on ctx).
func (m *Manager) Run(ctx context.Context) {
	var evCh <-chan broker.Event
	var pendCh <-chan interaction.Pending
	if m.events != nil {
		ch, cancel := m.events.SubscribeAll()
		defer cancel()
		evCh = ch
	}
	if m.pending != nil {
		ch, cancel := m.pending.SubscribePending()
		defer cancel()
		pendCh = ch
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-evCh:
			if !ok {
				evCh = nil
				continue
			}
			// subprocess.exit is the broker-level turn-end signal (the tailer
			// emits it once the turn's turn_duration line lands).
			if ev.Type == backend.EventProcessExit {
				m.notifyTurnDone(ev.SessionID)
			}
		case p, ok := <-pendCh:
			if !ok {
				pendCh = nil
				continue
			}
			m.notifyPermission(p)
		}
	}
}

// notification is the JSON the service worker receives and renders. The client
// keys its visual treatment (sound vs silent, sticky vs transient, inline
// Allow/Deny buttons) off kind.
type notification struct {
	Kind          string `json:"kind"` // "permission" | "turn-done"
	Title         string `json:"title"`
	Body          string `json:"body"`
	InteractionID string `json:"interaction_id,omitempty"`
	Tag           string `json:"tag,omitempty"`
	URL           string `json:"url,omitempty"`
}

func (m *Manager) sessionLabel(sessionID string) string {
	if info, ok := m.lookup(sessionID); ok {
		if info.Title != "" {
			return textutil.Truncate(oneLine(info.Title), 60)
		}
		if info.Cwd != "" {
			return filepath.Base(info.Cwd) // last path segment, not the whole path
		}
	}
	return textutil.ShortID(sessionID)
}

func (m *Manager) notifyTurnDone(sessionID string) {
	if m.suppressed(sessionID) {
		return // a browser is watching this session live
	}
	label := m.sessionLabel(sessionID)
	// One tag per session so a session's latest turn-done collapses the prior
	// one instead of stacking; turn-end is low-priority, so the client renders
	// it silently.
	m.fanout(notification{
		Kind:  "turn-done",
		Title: label,
		Body:  "Responded",
		Tag:   "turn-" + sessionID,
		URL:   "/#/s/" + sessionID,
	}, urgencyNormal)
}

func (m *Manager) notifyPermission(p interaction.Pending) {
	if m.suppressed(p.SessionID) {
		return // a browser is watching this session live; the UI shows the prompt
	}
	body := "Needs your approval"
	if p.ToolName != "" {
		body = "Wants to use " + p.ToolName
	}
	// Distinct per-interaction tag so concurrent prompts don't overwrite each
	// other; high urgency + the client's sticky/actionable treatment.
	m.fanout(notification{
		Kind:          "permission",
		Title:         m.sessionLabel(p.SessionID),
		Body:          body,
		InteractionID: p.ID,
		Tag:           "perm-" + p.ID,
		URL:           "/#/s/" + p.SessionID,
	}, urgencyHigh)
}

const (
	urgencyNormal = "normal"
	urgencyHigh   = "high"
)

// fanout encrypts the notification once per subscription (each has its own key
// material) and POSTs concurrently. Subscriptions the push service reports as
// gone (404/410) are pruned.
func (m *Manager) fanout(n notification, urgency string) {
	subs := m.store.all()
	if len(subs) == 0 {
		return
	}
	payload, err := json.Marshal(n)
	if err != nil {
		m.logger.Warn("push: marshal notification", "err", err)
		return
	}
	for _, sub := range subs {
		go func(sub Subscription) {
			status, err := m.send(sub, payload, urgency)
			switch {
			case err != nil:
				m.logger.Warn("push: send", "endpoint", endpointHost(sub.Endpoint), "err", err)
			case status == http.StatusNotFound || status == http.StatusGone:
				m.store.remove(sub.Endpoint)
				m.logger.Info("push: subscription gone, pruned", "endpoint", endpointHost(sub.Endpoint))
			case status >= 400:
				m.logger.Warn("push: non-2xx", "endpoint", endpointHost(sub.Endpoint), "status", status)
			}
		}(sub)
	}
}

// send encrypts payload for one subscription and delivers it to the push
// service, returning the HTTP status (0 on transport error).
func (m *Manager) send(sub Subscription, payload []byte, urgency string) (int, error) {
	uaPublic, err := b64urlDecode(sub.Keys.P256dh)
	if err != nil {
		return 0, fmt.Errorf("decode p256dh: %w", err)
	}
	authSecret, err := b64urlDecode(sub.Keys.Auth)
	if err != nil {
		return 0, fmt.Errorf("decode auth: %w", err)
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return 0, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return 0, err
	}
	body, err := encryptPayload(payload, uaPublic, authSecret, asPriv, salt)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	authz, err := m.keys.authHeader(sub.Endpoint, vapidSubscriber, time.Now())
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", ttlFor(urgency))
	req.Header.Set("Urgency", urgency)

	resp, err := m.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// ttlFor sets how long the push service should retain an undelivered message.
// A high-urgency permission prompt is only relevant briefly; a turn-done can
// wait until the device next wakes.
func ttlFor(urgency string) string {
	if urgency == urgencyHigh {
		return "600" // 10 minutes
	}
	return "3600" // 1 hour
}

// oneLine collapses whitespace (including newlines) to single spaces so a
// multi-line title renders as one tidy notification line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// endpointHost is the push service host, for logs that shouldn't echo the full
// (secret-bearing) endpoint URL.
func endpointHost(endpoint string) string {
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		return u.Host
	}
	return endpoint
}
