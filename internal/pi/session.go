// Package pi adapts pi coding-agent sessions and its RPC protocol to usher.
package pi

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/textutil"
)

var sessionNameNewlines = regexp.MustCompile(`[\r\n]+`)

// SessionIDFromPath reads the stable id from a pi session header.
func SessionIDFromPath(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return ""
	}
	var h header
	if json.Unmarshal(sc.Bytes(), &h) != nil || h.Type != "session" {
		return ""
	}
	return h.ID
}

type header struct {
	Type          string    `json:"type"`
	ID            string    `json:"id"`
	Cwd           string    `json:"cwd"`
	Timestamp     time.Time `json:"timestamp"`
	ParentSession string    `json:"parentSession"`
}

type entry struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  *string         `json:"parentId"`
	Timestamp time.Time       `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
	Name      string          `json:"name"`
	// custom_message fields. Content mirrors a message's content shape.
	Content       json.RawMessage `json:"content"`
	Display       bool            `json:"display"`
	ThinkingLevel string          `json:"thinkingLevel"`
}

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Model        string          `json:"model"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Timestamp    int64           `json:"timestamp"`
	Usage        usage           `json:"usage"`
}

// usage is the provider's token count for one assistant message.
type usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
}

func (u usage) contextTokens() int64 {
	return u.Input + u.CacheRead + u.CacheWrite + u.Output
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// MetaScanner folds a session log's lines, fed in file order, into its
// SessionMeta.
type MetaScanner struct{ meta core.SessionMeta }

func NewMetaScanner() *MetaScanner { return &MetaScanner{} }

func (s *MetaScanner) Feed(raw []byte) {
	meta := &s.meta
	var e entry
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	if e.Type == "session" {
		var h header
		if json.Unmarshal(raw, &h) == nil {
			meta.ID, meta.Cwd, meta.StartedAt = h.ID, h.Cwd, h.Timestamp
			meta.ParentID = sessionIDFromParent(h.ParentSession)
		}
		return
	}
	if !e.Timestamp.IsZero() {
		meta.LastEventAt = e.Timestamp
	}
	if e.Type == "session_info" {
		meta.Title = e.Name
		return
	}
	// Pi persists every thinking-level change, so the last one is the
	// session's current level even when no worker is live. A session that
	// never changed level has no such record and keeps the model default,
	// which only the RPC state knows.
	if e.Type == "thinking_level_change" {
		meta.Runtime.Effort = e.ThinkingLevel
		return
	}
	// Usage recorded before a compaction describes the context it replaced,
	// so nothing describes the current one until the next assistant message
	// reports its own. Pi's live snapshot goes quiet in that window too.
	if e.Type == "compaction" {
		meta.Runtime.ContextTokens = 0
		return
	}
	if e.Type != "message" {
		return
	}
	var m message
	if json.Unmarshal(e.Message, &m) != nil {
		return
	}
	if m.Role == "user" {
		text := contentText(m.Content)
		if meta.Prompt == "" {
			meta.Prompt = textutil.Truncate(strings.TrimSpace(text), 60)
		}
		meta.LastInputAt = entryTime(e, m)
	}
	if m.Role == "assistant" {
		if m.Model != "" {
			meta.Runtime.Model = m.Model
		}
		// The provider's count for the newest message approximates the
		// context; a live worker's own estimate supersedes it.
		if tokens := m.Usage.contextTokens(); tokens > 0 {
			meta.Runtime.ContextTokens = tokens
		}
	}
}

// Meta is the metadata of the lines fed so far.
func (s *MetaScanner) Meta() core.SessionMeta { return s.meta }

func ReadSessionMeta(path string) (core.SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return core.SessionMeta{}, err
	}
	defer f.Close()
	s := NewMetaScanner()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		s.Feed(sc.Bytes())
	}
	return s.Meta(), sc.Err()
}

// RenameSession appends pi session_info metadata.
func RenameSession(path, title string) error {
	title = strings.TrimSpace(sessionNameNewlines.ReplaceAllString(title, " "))
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var parentID *string
	for sc.Scan() {
		var e entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
			id := e.ID
			parentID = &id
		}
	}
	closeErr := f.Close()
	if err := sc.Err(); err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	var idBytes [4]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return err
	}
	record := struct {
		Type      string  `json:"type"`
		ID        string  `json:"id"`
		ParentID  *string `json:"parentId"`
		Timestamp string  `json:"timestamp"`
		Name      string  `json:"name"`
	}{
		Type:      "session_info",
		ID:        fmt.Sprintf("%x", idBytes[:]),
		ParentID:  parentID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Name:      title,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = out.Write(data)
	return err
}

func sessionIDFromParent(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return ""
	}
	var h header
	if json.Unmarshal(sc.Bytes(), &h) != nil {
		return ""
	}
	return h.ID
}

func entryTime(e entry, m message) time.Time {
	if m.Timestamp > 0 {
		return time.UnixMilli(m.Timestamp)
	}
	return e.Timestamp
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var out []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}
