package pluginapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/hook"
)

// Client talks to the plugin API socket and presents the same interface shape
// an in-process hub gets from the Router, so plugin hub code is written
// against channels and plain calls, not HTTP.
type Client struct {
	http   *http.Client
	logger *slog.Logger
}

// callTimeout bounds the synchronous calls (get / send / respond). SSE
// subscriptions use their own untimed client.
const callTimeout = 30 * time.Second

// NewClient returns a Client for the plugin API socket at path.
func NewClient(path string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Client{
		http:   &http.Client{Transport: transport},
		logger: logger,
	}
}

// url builds a request URL; the host is a placeholder (the transport always
// dials the socket).
func url(pathAndQuery string) string { return "http://usher" + pathAndQuery }

// Ping verifies the socket is reachable — a startup diagnostic so a plugin
// fails fast with a clear message when usher isn't running.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url("/v1/healthz"), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected healthz status %d", resp.StatusCode)
	}
	return nil
}

// GetSession fetches one session. Transport errors surface as "not found",
// matching the in-process signature; callers treat both as "skip".
func (c *Client) GetSession(id string) (core.Session, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var sess core.Session
	if err := c.getJSON(ctx, "/v1/sessions/"+id, &sess); err != nil {
		return core.Session{}, false
	}
	return sess, true
}

// SendToSession routes text to a session as a user prompt.
func (c *Client) SendToSession(id, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.post(ctx, "/v1/sessions/"+id+"/send", sendReq{Text: text})
}

// RespondInteraction resolves a pending permission interaction.
func (c *Client) RespondInteraction(id string, resp hook.Response) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.post(ctx, "/v1/interactions/"+id+"/respond", resp)
}

// SubscribeAllSessions streams every session's events. The subscription
// reconnects with backoff until cancelled, so a usher restart heals itself.
func (c *Client) SubscribeAllSessions() (<-chan broker.Event, func()) {
	return subscribe[broker.Event](c, "/v1/events")
}

// SubscribePendingInteractions streams pending permission prompts. On each
// (re)connect the server replays the currently-pending set before the live
// stream; consumers must dedupe by pending id.
func (c *Client) SubscribePendingInteractions() (<-chan hook.Pending, func()) {
	return subscribe[hook.Pending](c, "/v1/interactions")
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url(path), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url(path), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return apiError(resp)
	}
	return nil
}

// apiError extracts the server's {"error": ...} message from a non-2xx reply.
func apiError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	return fmt.Errorf("plugin api status %d", resp.StatusCode)
}

// maxSSELine caps one SSE data frame; an assistant event carries a whole
// jsonl line, so this is generous.
const maxSSELine = 16 << 20

// subscribe opens an auto-reconnecting SSE subscription and decodes each data
// frame into T. The channel closes when cancel is called; a dropped connection
// is retried with backoff, invisible to the consumer apart from the gap.
func subscribe[T any](c *Client, path string) (<-chan T, func()) {
	ch := make(chan T, 64)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(ch)
		backoff := time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			err := c.streamOnce(ctx, path, func(data []byte) bool {
				var v T
				if err := json.Unmarshal(data, &v); err != nil {
					c.logger.Warn("plugin api: bad SSE frame", "path", path, "err", err)
					return true
				}
				select {
				case ch <- v:
					return true
				case <-ctx.Done():
					return false
				}
			})
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				c.logger.Warn("plugin api: stream dropped, reconnecting", "path", path, "err", err, "backoff", backoff)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
		}
	}()
	return ch, cancel
}

// streamOnce runs one SSE connection, invoking onData per data frame until
// the stream ends (error) or onData returns false (cancelled).
func (c *Client) streamOnce(ctx context.Context, path string, onData func([]byte) bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url(path), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64<<10), maxSSELine)
	var data []byte
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		case line == "" && len(data) > 0:
			if !onData(data) {
				return nil
			}
			data = nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF // server closed the stream; reconnect
}
