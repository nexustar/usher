package usheragent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/textutil"
)

// ChatClient is a minimal Chat Completions client. It speaks the OpenAI
// Chat Completions wire format, which is the de facto standard implemented
// by OpenAI itself, Ollama, DeepSeek, Together, Groq, OpenRouter, vLLM,
// LM Studio, and Anthropic's OpenAI-compatible endpoint.
//
// We intentionally do NOT use a vendor SDK: the protocol is a few hundred
// lines, hand-rolling stays provider-agnostic, and provider-specific
// extensions (Anthropic prompt caching, OpenAI structured outputs) are
// out of scope for usher's routing agent.
type ChatClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewChatClient builds a client for the given OpenAI-compatible endpoint.
// baseURL should include the API version segment (e.g. "https://api.openai.com/v1"
// or "http://localhost:11434/v1"); the trailing slash is optional.
// apiKey may be empty for backends that don't require auth (local Ollama).
func NewChatClient(baseURL, apiKey string) *ChatClient {
	return &ChatClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

// --- Wire types ---------------------------------------------------------

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []ChatTool    `json:"tools,omitempty"`
}

// ChatMessage's Extra holds any wire fields we don't model by name, so we can
// send them back unchanged. Reasoning models need this: DeepSeek puts
// `reasoning_content` on the message and 400s if it's not replayed on the next
// turn. ToolCall has the same Extra for Gemini's thought_signature.
type ChatMessage struct {
	Role       string     // system | user | assistant | tool
	Content    string     //
	Name       string     //
	ToolCalls  []ToolCall // assistant role
	ToolCallID string     // tool role
	Extra      map[string]json.RawMessage
}

type ToolCall struct {
	ID       string       // always present
	Type     string       // always "function"
	Function ToolCallFunc //
	// Extra keeps unknown fields like Gemini's extra_content.thought_signature.
	Extra map[string]json.RawMessage
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded string per OpenAI spec
}

// keys we have named fields for; anything else goes to Extra on decode.
var (
	knownMessageKeys  = []string{"role", "content", "name", "tool_calls", "tool_call_id"}
	knownToolCallKeys = []string{"id", "type", "function"}
)

func (m ChatMessage) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(m.Extra)+len(knownMessageKeys))
	for k, v := range m.Extra {
		out[k] = v
	}
	if err := putField(out, "role", m.Role, false); err != nil { // role has no omitempty
		return nil, err
	}
	if err := putField(out, "content", m.Content, m.Content == ""); err != nil {
		return nil, err
	}
	if err := putField(out, "name", m.Name, m.Name == ""); err != nil {
		return nil, err
	}
	if err := putField(out, "tool_calls", m.ToolCalls, len(m.ToolCalls) == 0); err != nil {
		return nil, err
	}
	if err := putField(out, "tool_call_id", m.ToolCallID, m.ToolCallID == ""); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func (m *ChatMessage) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if err := pullField(raw, "role", &m.Role); err != nil {
		return err
	}
	if err := pullField(raw, "content", &m.Content); err != nil {
		return err
	}
	if err := pullField(raw, "name", &m.Name); err != nil {
		return err
	}
	if err := pullField(raw, "tool_calls", &m.ToolCalls); err != nil {
		return err
	}
	if err := pullField(raw, "tool_call_id", &m.ToolCallID); err != nil {
		return err
	}
	for _, k := range knownMessageKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		m.Extra = raw
	}
	return nil
}

func (c ToolCall) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(c.Extra)+len(knownToolCallKeys))
	for k, v := range c.Extra {
		out[k] = v
	}
	// id/type/function are always emitted (no omitempty).
	if err := putField(out, "id", c.ID, false); err != nil {
		return nil, err
	}
	if err := putField(out, "type", c.Type, false); err != nil {
		return nil, err
	}
	if err := putField(out, "function", c.Function, false); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func (c *ToolCall) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if err := pullField(raw, "id", &c.ID); err != nil {
		return err
	}
	if err := pullField(raw, "type", &c.Type); err != nil {
		return err
	}
	if err := pullField(raw, "function", &c.Function); err != nil {
		return err
	}
	for _, k := range knownToolCallKeys {
		delete(raw, k)
	}
	if len(raw) > 0 {
		c.Extra = raw
	}
	return nil
}

// putField writes val under key unless omit is true (our omitempty).
func putField(out map[string]json.RawMessage, key string, val any, omit bool) error {
	if omit {
		return nil
	}
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	out[key] = b
	return nil
}

// pullField decodes raw[key] into dst if present.
func pullField(raw map[string]json.RawMessage, key string, dst any) error {
	v, ok := raw[key]
	if !ok {
		return nil
	}
	return json.Unmarshal(v, dst)
}

type ChatTool struct {
	Type     string       `json:"type"` // always "function"
	Function ChatFunction `json:"function"`
}

type ChatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ChatResponse struct {
	ID      string       `json:"id"`
	Choices []ChatChoice `json:"choices"`
	Usage   Usage        `json:"usage"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type apiErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"` // some providers return string, others int
	} `json:"error"`
}

// APIError carries the HTTP status alongside the message so retry logic
// can distinguish 429 / 5xx from permanent failures without string-matching.
type APIError struct {
	StatusCode int
	Message    string
	// RetryAfter is the parsed `Retry-After` header in seconds, if present.
	// 0 means the header was absent or unparseable.
	RetryAfter int
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message) }

// retry config — up to two retries on 429 or 5xx with exponential back-off.
const (
	maxRetryAttempts = 2
	defaultBackoff   = 2 * time.Second
	maxBackoff       = 60 * time.Second
)

// ChatCompletion sends a non-streaming request and returns the decoded
// response. On a transient failure (HTTP 429 or 5xx), retries up to
// maxRetryAttempts times, doubling the back-off per attempt and honoring
// the `Retry-After` header (capped at 60s) when present.
func (c *ChatClient) ChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	var resp ChatResponse
	var err error
	for attempt := 0; ; attempt++ {
		resp, err = c.doChatCompletion(ctx, req)
		if err == nil {
			return resp, nil
		}
		if attempt >= maxRetryAttempts || !shouldRetry(err) {
			return resp, err
		}
		delay := backoffFor(err) << attempt
		if delay > maxBackoff {
			delay = maxBackoff
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return resp, ctx.Err()
		}
	}
}

func (c *ChatClient) doChatCompletion(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("chat completion request: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read response body: %w", err)
	}

	if httpResp.StatusCode >= 400 {
		msg := ""
		var e apiErrorBody
		if err := json.Unmarshal(raw, &e); err == nil && e.Error.Message != "" {
			msg = e.Error.Message
		} else {
			msg = textutil.Truncate(string(raw), 500)
		}
		return ChatResponse{}, &APIError{
			StatusCode: httpResp.StatusCode,
			Message:    msg,
			RetryAfter: parseRetryAfter(httpResp.Header.Get("Retry-After")),
		}
	}

	var resp ChatResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ChatResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return resp, nil
}

// shouldRetry returns true for 429 and 5xx APIErrors. Network errors are
// not retried — most are local and unlikely to self-heal in 2s.
func shouldRetry(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.StatusCode == http.StatusTooManyRequests || (ae.StatusCode >= 500 && ae.StatusCode < 600)
}

// backoffFor returns the wait duration before retrying err. For 429 with
// a Retry-After header, that's the requested delay (capped at 60s); for
// other retryable errors it's a fixed default.
func backoffFor(err error) time.Duration {
	var ae *APIError
	if errors.As(err, &ae) && ae.RetryAfter > 0 {
		d := time.Duration(ae.RetryAfter) * time.Second
		if d > maxBackoff {
			d = maxBackoff
		}
		return d
	}
	return defaultBackoff
}

// parseRetryAfter parses the Retry-After header. The HTTP spec allows both
// "delta seconds" and an HTTP-date; we only handle the integer form, which
// is what every LLM provider uses in practice.
func parseRetryAfter(h string) int {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if n, err := strconv.Atoi(h); err == nil && n > 0 {
		return n
	}
	return 0
}
