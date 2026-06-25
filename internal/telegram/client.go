package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is the public Bot API host. Overridable (NewClient's baseURL)
// so tests can point at an httptest server.
const DefaultBaseURL = "https://api.telegram.org"

// Client is a Telegram Bot API client. It is safe for concurrent use; the
// only mutable state is the embedded *http.Client.
type Client struct {
	token   string
	baseURL string
	hc      *http.Client
}

// NewClient builds a client for the given bot token. baseURL defaults to
// DefaultBaseURL when empty. The HTTP client has no overall timeout so that
// long-poll getUpdates can block server-side; per-call deadlines come from the
// context passed to each method.
func NewClient(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		token:   token,
		baseURL: baseURL,
		hc:      &http.Client{},
	}
}

// do POSTs a JSON request to method and decodes the "result" field into out
// (out may be nil to discard it). A non-ok envelope becomes an *APIError.
func (c *Client) do(ctx context.Context, method string, params, out any) error {
	var body []byte
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("telegram %s: marshal params: %w", method, err)
		}
		body = b
	}
	return c.doRaw(ctx, method, "application/json", body, out)
}

// maxRateLimitRetries bounds the 429 retry loop; retryAfterCap clamps a
// server-specified retry_after so a pathological value can't stall us for long.
const (
	maxRateLimitRetries = 3
	retryAfterCap       = 30 * time.Second
)

// doRaw POSTs body with the given content type to method and decodes the
// "result" field into out. It backs both the JSON do and multipart uploads, and
// retries (bounded) on Telegram's 429 Too Many Requests, honouring the
// retry_after the API returns.
func (c *Client) doRaw(ctx context.Context, method, contentType string, body []byte, out any) error {
	url := c.baseURL + "/bot" + c.token + "/" + method
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("telegram %s: new request: %w", method, err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		resp, err := c.hc.Do(req)
		if err != nil {
			return fmt.Errorf("telegram %s: %w", method, err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("telegram %s: read body: %w", method, err)
		}

		var env apiResponse
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("telegram %s: decode envelope (status %d): %w", method, resp.StatusCode, err)
		}
		if !env.OK {
			if env.ErrorCode == 429 && attempt < maxRateLimitRetries {
				wait := time.Duration(env.Parameters.RetryAfter) * time.Second
				if wait <= 0 {
					wait = time.Second
				}
				if wait > retryAfterCap {
					wait = retryAfterCap
				}
				if !sleepCtx(ctx, wait) {
					return ctx.Err()
				}
				continue
			}
			return &APIError{Code: env.ErrorCode, Description: env.Description, Method: method}
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("telegram %s: decode result: %w", method, err)
			}
		}
		return nil
	}
}

// GetMe returns the bot's own account, used at startup to validate the token.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, "getMe", nil, &u)
	return u, err
}

// GetUpdates long-polls for new updates. offset is the next update_id to fetch
// (last seen + 1; 0 for the first call). timeout is the server-side hold in
// seconds — the request blocks up to that long when there is nothing new, so
// the caller's context deadline (if any) must exceed it.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	params := map[string]any{
		"timeout": timeout,
	}
	if offset != 0 {
		params["offset"] = offset
	}
	var updates []Update
	err := c.do(ctx, "getUpdates", params, &updates)
	return updates, err
}

// SendMessage posts a message, optionally into a forum topic and/or with an
// inline keyboard. It returns the sent Message so callers can track ids.
func (c *Client) SendMessage(ctx context.Context, p SendMessageParams) (Message, error) {
	var m Message
	err := c.do(ctx, "sendMessage", p, &m)
	return m, err
}

// SendPhoto uploads an image (multipart) into a chat/topic. Telegram caps
// sendPhoto at 10 MB and may re-encode; callers handle the returned error
// (e.g. unsupported type / too large) by logging.
func (c *Client) SendPhoto(ctx context.Context, p SendPhotoParams, filename string, data []byte) (Message, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", strconv.FormatInt(p.ChatID, 10))
	if p.MessageThreadID != 0 {
		_ = mw.WriteField("message_thread_id", strconv.FormatInt(p.MessageThreadID, 10))
	}
	if p.Caption != "" {
		_ = mw.WriteField("caption", p.Caption)
	}
	if p.DisableNotification {
		_ = mw.WriteField("disable_notification", "true")
	}
	fw, err := mw.CreateFormFile("photo", filename)
	if err != nil {
		return Message{}, fmt.Errorf("telegram sendPhoto: form file: %w", err)
	}
	if _, err := fw.Write(data); err != nil {
		return Message{}, fmt.Errorf("telegram sendPhoto: write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return Message{}, fmt.Errorf("telegram sendPhoto: close: %w", err)
	}
	var m Message
	err = c.doRaw(ctx, "sendPhoto", mw.FormDataContentType(), buf.Bytes(), &m)
	return m, err
}

// CreateForumTopic creates a topic in the forum supergroup chatID and returns
// its message_thread_id. The bot must be an admin with can_manage_topics.
func (c *Client) CreateForumTopic(ctx context.Context, chatID int64, name string) (ForumTopic, error) {
	var t ForumTopic
	err := c.do(ctx, "createForumTopic", map[string]any{
		"chat_id": chatID,
		"name":    name,
	}, &t)
	return t, err
}

// CloseForumTopic closes (but does not delete) a topic, mirroring a session
// falling out of usher's active view.
func (c *Client) CloseForumTopic(ctx context.Context, chatID, threadID int64) error {
	return c.do(ctx, "closeForumTopic", map[string]any{
		"chat_id":           chatID,
		"message_thread_id": threadID,
	}, nil)
}

// SetMessageReaction sets a single emoji reaction on a message (pass one of
// Telegram's standard reaction emojis). It's used as a lightweight "received"
// acknowledgement on an inbound message — no extra message in the topic.
func (c *Client) SetMessageReaction(ctx context.Context, chatID, messageID int64, emoji string) error {
	return c.do(ctx, "setMessageReaction", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"reaction":   []map[string]any{{"type": "emoji", "emoji": emoji}},
	}, nil)
}

// AnswerCallbackQuery acknowledges a button tap, clearing the client-side
// progress spinner. text (optional) shows a brief toast to the user.
func (c *Client) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
	params := map[string]any{"callback_query_id": queryID}
	if text != "" {
		params["text"] = text
	}
	return c.do(ctx, "answerCallbackQuery", params, nil)
}

// EditMessageReplyMarkup replaces (markup) or removes (markup == nil) the
// inline keyboard on an existing message — used to strip the allow/deny
// buttons once a permission decision is made, so they can't be tapped twice.
func (c *Client) EditMessageReplyMarkup(ctx context.Context, chatID, messageID int64, markup *InlineKeyboardMarkup) error {
	if markup == nil {
		markup = &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{}}
	}
	return c.do(ctx, "editMessageReplyMarkup", map[string]any{
		"chat_id":      chatID,
		"message_id":   messageID,
		"reply_markup": markup,
	}, nil)
}

// PollContext derives a context whose deadline safely exceeds a long-poll of
// timeoutSec seconds, leaving margin for the round trip. Callers chain it off
// their loop context so cancellation still propagates.
func PollContext(parent context.Context, timeoutSec int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(timeoutSec+10)*time.Second)
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
