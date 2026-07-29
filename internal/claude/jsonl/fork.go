package jsonl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ForkCopy forks the session at srcPath into a new session file at dstPath:
// a prefix copy keeping everything through the turn containing afterUUID,
// with each line's sessionId rewritten to newID. The jsonl is append-only,
// so any line-prefix is a state the file once was in, which `claude --resume`
// loads like any other session. The source is never written to.
//
// Conversation-only: the working tree is not rewound, and sidecar state keyed
// by the old id (todos, file-history) does not carry over. Errors when
// afterUUID is missing, its turn has not completed (the cut would strand
// half-finished tool calls), or dstPath already exists.
func ForkCopy(srcPath, dstPath, afterUUID, newID string) error {
	if afterUUID == "" {
		return fmt.Errorf("fork point uuid is required")
	}
	if _, err := os.Stat(dstPath); err == nil {
		return fmt.Errorf("fork target %s already exists", filepath.Base(dstPath))
	}
	oldID := strings.TrimSuffix(filepath.Base(srcPath), ".jsonl")
	prefix, err := forkPrefix(srcPath, afterUUID, oldID, newID)
	if err != nil {
		return err
	}
	// Temp name without the .jsonl suffix, so discovery can't pick up a
	// half-written session.
	tmp := dstPath + ".tmp"
	if err := os.WriteFile(tmp, prefix, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dstPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// forkPrefix scans srcPath once and returns the fork's content: every line
// through the turn containing afterUUID — up to the next top-level user
// prompt (a prompt only lands once the previous turn finished) or, for the
// last turn, EOF when a system/turn_duration line proves it completed.
//
// The sessionId rewrite is a literal token replacement, never a bare uuid
// (the id can appear inside message content). It aborts when the rewrite
// count disagrees with the parsed count — claude changed the field's
// formatting, and a fork still carrying the old id is worse than no fork.
func forkPrefix(srcPath, afterUUID, oldID, newID string) ([]byte, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	needle := []byte(`"sessionId":"` + oldID + `"`)
	repl := []byte(`"sessionId":"` + newID + `"`)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var buf bytes.Buffer
	var seen, completed bool
	parsed, replaced := 0, 0
	for sc.Scan() {
		line := sc.Bytes()
		// Malformed lines are copied as-is (every reader here skips them).
		var ev Event
		if err := json.Unmarshal(line, &ev); err == nil {
			if seen {
				if ev.Type == "user" && !hasToolResult(ev.Message) {
					completed = true
					break // cut: this prompt and everything after stay out
				}
				if IsTurnComplete(line) {
					completed = true
				}
			}
			if ev.SessionID == oldID {
				parsed++
			}
			if ev.UUID == afterUUID {
				seen = true
			}
		}
		if bytes.Contains(line, needle) {
			line = bytes.ReplaceAll(line, needle, repl)
			replaced++
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !seen {
		return nil, fmt.Errorf("fork point %s not found in session", afterUUID)
	}
	if !completed {
		return nil, fmt.Errorf("fork point is in a turn that has not completed")
	}
	if replaced != parsed {
		return nil, fmt.Errorf("sessionId rewrite mismatch (%d parsed, %d rewritten): jsonl format changed", parsed, replaced)
	}
	return buf.Bytes(), nil
}
