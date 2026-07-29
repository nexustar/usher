// Package mainchat persists main-chat conversations as append-only jsonl
// files under <data-dir>/mainchats/<id>.jsonl.
//
// usher is single-process, so a per-store mutex is enough to serialize
// concurrent appends. There is no on-disk locking and no partitioning.
package mainchat

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nexustar/usher/internal/textutil"
)

type Message struct {
	// Role is "user" | "agent" | "relay" | "tool". A relay message is a session's
	// completed reply delivered verbatim into the chat by the server (the
	// agent routes; it does not restate session output).
	Role    string    `json:"role"`
	Content string    `json:"content"`
	Time    time.Time `json:"ts"`
	// FocusSession records which Claude Code session the agent operated on
	// during this turn (only set on agent messages; "" when the turn didn't
	// touch any session). Server uses the most recent non-empty value as
	// the implicit focus when the next user message is ambiguous.
	FocusSession string `json:"focus_session,omitempty"`
	// SourceSession is the session whose reply this is (relay messages only).
	SourceSession string `json:"source_session,omitempty"`
	// CoveredThrough (summary messages only) is the timestamp of the last
	// message this summary folded in. The store is append-only, so a
	// compaction's summary lands AFTER the recent tail it deliberately kept —
	// this field is how the derivation knows to keep messages newer than the
	// fold while dropping the folded ones.
	CoveredThrough time.Time `json:"covered_through,omitzero"`
	// Tool is an internal main-chat tool invocation. It is persisted for the
	// model's next turn but omitted from the user-facing messages endpoint.
	Tool *ToolEvent `json:"tool,omitempty"`
}

type ToolEvent struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Result    string `json:"result"`
}

type Chat struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

var idRE = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ValidateID reports whether id is a legal chat id (path-safe). Unknown ids
// are fine — chats are created lazily on first Append — so this is the whole
// request-time check, without parsing any file.
func ValidateID(id string) error {
	if !idRE.MatchString(id) {
		return errors.New("invalid mainchat id")
	}
	return nil
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+".jsonl"), nil
}

// Append writes msg to the chat's jsonl, creating the file if needed.
func (s *Store) Append(id string, msg Message) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	if msg.Time.IsZero() {
		msg.Time = time.Now().UTC()
	}
	line, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// Read returns the chat's messages. limit > 0 keeps only the latest N.
// A missing file returns (nil, nil) — chats are created lazily on Append.
func (s *Store) Read(id string, limit int) ([]Message, error) {
	p, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, nil
}

// List enumerates known chats by scanning the directory.
func (s *Store) List() ([]Chat, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Chat
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".jsonl")
		if !idRE.MatchString(id) {
			continue
		}
		title, _ := s.firstUserContent(id)
		out = append(out, Chat{ID: id, Title: title})
	}
	return out, nil
}

func (s *Store) firstUserContent(id string) (string, error) {
	msgs, err := s.Read(id, 0)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		if m.Role == "user" {
			return textutil.Truncate(m.Content, 60), nil
		}
	}
	return "", nil
}
