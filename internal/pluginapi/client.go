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
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/api"
	"github.com/nexustar/usher/internal/broker"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/interaction"
)

// Client talks to the plugin API socket and presents the same interface shape
// an in-process hub gets from the Router, so plugin hub code is written
// against channels and plain calls, not HTTP.
type Client struct {
	http   *http.Client
	logger *slog.Logger

	// EventTypes, when set before SubscribeAllSessions, asks the server to
	// stream only those event types. A tool_result "user" event carries an
	// entire tool output, so a consumer that renders a few types should not
	// pull the rest across the socket.
	EventTypes []string
}

// callTimeout bounds the synchronous calls (get / send / respond). SSE
// subscriptions use their own untimed client.
const callTimeout = 30 * time.Second
const startTimeout = 60 * time.Second
const attachmentTimeout = 2 * time.Minute

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
	var sess api.Session
	if err := c.getJSON(ctx, "/v1/sessions/"+neturl.PathEscape(id), &sess); err != nil {
		return core.Session{}, false
	}
	return sess.Session, true
}

// SendToSession routes text to a session as a user prompt.
func (c *Client) SendToSession(id, text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.post(ctx, "/v1/sessions/"+neturl.PathEscape(id)+"/send", api.SendRequest{Text: text})
}

// ListSessions returns every root session, archived ones included, with
// usher's own flags attached.
func (c *Client) ListSessions() ([]SessionView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var out []SessionView
	if err := c.getJSON(ctx, "/v1/sessions?include_archived=1", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SendToSessionRelayed routes a prompt on the router session's behalf and
// returns as soon as it is accepted. When the turn completes, its reply is
// delivered into the router session. The returned Delivery says whether a
// relay was actually attached: there may not be a router session.
func (c *Client) SendToSessionRelayed(id, text string) (Delivery, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var out Delivery
	err := c.postJSON(ctx, "/v1/sessions/"+neturl.PathEscape(id)+"/route", api.SendRequest{Text: text}, &out)
	return out, err
}

// CreateRequest spawns a session; see api.CreateRequest.
type CreateRequest = api.CreateRequest

// CreateSession spawns a session and returns as soon as its process is up. An
// empty Backend preserves model inference/default behavior.
func (c *Client) CreateSession(req CreateRequest) (Delivery, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	var out Delivery
	err := c.postJSON(ctx, "/v1/sessions", req, &out)
	return out, err
}

// RouteNewSession is CreateSession on the router session's behalf: the new
// session's first reply is delivered into the router session.
func (c *Client) RouteNewSession(req CreateRequest) (Delivery, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	var out Delivery
	err := c.postJSON(ctx, "/v1/route", req, &out)
	return out, err
}

// ReadTurns reads a page of a session's turns, oldest first: the newest page,
// the page just before the cursor before, or — when from is set — the turn at
// that cursor and every turn after it.
func (c *Client) ReadTurns(id, before, from string, limit int) (api.TranscriptPage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	values := neturl.Values{}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	if before != "" {
		values.Set("before", before)
	}
	if from != "" {
		values.Set("from", from)
	}
	var out api.TranscriptPage
	err := c.getJSON(ctx, "/v1/sessions/"+neturl.PathEscape(id)+"/transcript?"+values.Encode(), &out)
	return out, err
}

// SearchSessions searches one session's transcript when id is set, or ranks
// matching sessions across the machine when it is empty.
func (c *Client) SearchSessions(id, query string, limit, contextChars int) (SearchResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	values := neturl.Values{}
	values.Set("q", query)
	values.Set("limit", strconv.Itoa(limit))
	values.Set("context_chars", strconv.Itoa(contextChars))
	if id != "" {
		values.Set("session_id", id)
	}
	var out SearchResult
	err := c.getJSON(ctx, "/v1/search?"+values.Encode(), &out)
	return out, err
}

// SetArchived archives or unarchives a session.
func (c *Client) SetArchived(id string, archived bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	method := http.MethodPost
	if !archived {
		method = http.MethodDelete
	}
	return c.do(ctx, method, "/v1/sessions/"+neturl.PathEscape(id)+"/archive", nil, nil)
}

// SetAutoApprove toggles usher's permission auto-approval for a session.
func (c *Client) SetAutoApprove(id string, enabled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.post(ctx, "/v1/sessions/"+neturl.PathEscape(id)+"/auto-approve",
		map[string]bool{"enabled": enabled})
}

// ListPendingInteractions returns the currently-pending prompts as a snapshot.
// SubscribePendingInteractions is the streaming form; a caller that cannot
// hold a stream open (an MCP tool call) uses this one.
func (c *Client) ListPendingInteractions() ([]interaction.Pending, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var out []interaction.Pending
	if err := c.getJSON(ctx, "/v1/interactions", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SessionView is a session plus the flags usher keeps outside the jsonl.
type SessionView = api.Session

// UploadAttachment streams a file into an existing session's managed
// attachment directory and returns the absolute path agents can read.
func (c *Client) UploadAttachment(id, filename string, src io.Reader) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), attachmentTimeout)
	defer cancel()
	path := "/v1/sessions/" + neturl.PathEscape(id) + "/upload?filename=" + neturl.QueryEscape(filename)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url(path), src)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp)
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Path, nil
}

// RespondInteraction resolves a pending permission interaction.
func (c *Client) RespondInteraction(id string, resp interaction.Response) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return c.post(ctx, "/v1/interactions/"+neturl.PathEscape(id)+"/respond", resp)
}

// SubscribeAllSessions streams every session's events (filtered to
// EventTypes when set). The subscription reconnects with backoff until
// cancelled, so a usher restart heals itself.
func (c *Client) SubscribeAllSessions() (<-chan broker.Event, func()) {
	path := "/v1/events"
	if len(c.EventTypes) > 0 {
		path += "?types=" + neturl.QueryEscape(strings.Join(c.EventTypes, ","))
	}
	return subscribe[broker.Event](c, path)
}

// SubscribePendingInteractions streams pending permission prompts. On each
// (re)connect the server replays the currently-pending set before the live
// stream; consumers must dedupe by pending id.
func (c *Client) SubscribePendingInteractions() (<-chan interaction.Pending, func()) {
	events, cancel := subscribe[broker.Event](c, "/v1/events?types="+api.EventInteractionPending)
	ch := make(chan interaction.Pending, 64)
	go func() {
		defer close(ch)
		for ev := range events {
			var p interaction.Pending
			if err := json.Unmarshal(ev.Raw, &p); err != nil {
				c.logger.Warn("plugin api: bad pending interaction", "err", err)
				continue
			}
			ch <- p
		}
	}()
	return ch, cancel
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
	return c.postJSON(ctx, path, body, nil)
}

func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// do sends body as JSON (nil sends none) and decodes a 2xx reply into out.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url(path), reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// APIError is a rejection the server itself returned (the router refused the
// call — e.g. an interaction already resolved). Its absence on a failed call
// means the transport failed and usher may never have seen the request;
// consumers use that split to decide between "give up" and "retry".
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("plugin api status %d", e.Status)
}

// apiError extracts the server's {"error": ...} message from a non-2xx reply.
func apiError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return &APIError{Status: resp.StatusCode, Msg: e.Error}
	}
	return &APIError{Status: resp.StatusCode}
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
			connected := false
			err := c.streamOnce(ctx, path, func() { connected = true }, func(data []byte) bool {
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
			if connected {
				backoff = time.Second // the drop ended a working stream; retry eagerly
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

// streamOnce runs one SSE connection, invoking onConnect once the server
// accepts the stream and onData per data frame, until the stream ends
// (error) or onData returns false (cancelled).
func (c *Client) streamOnce(ctx context.Context, path string, onConnect func(), onData func([]byte) bool) error {
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
	onConnect()
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
