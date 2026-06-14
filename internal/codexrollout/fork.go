package codexrollout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RolloutFilename is the on-disk name of a rollout for session id at time ts:
// rollout-<YYYY-MM-DDThh-mm-ss>-<id>.jsonl. Used to name a fork so discovery
// (which matches that shape) picks it up.
func RolloutFilename(id string, ts time.Time) string {
	return "rollout-" + ts.UTC().Format("2006-01-02T15-04-05") + "-" + id + ".jsonl"
}

// ForkCopy writes a new rollout at dstPath that is the prefix of the rollout at
// srcPath through the turn whose task_complete carries throughTurnID (inclusive).
// The new rollout's session_meta header is rewritten with the new id and a
// forked_from_id pointing at srcID; every other line is copied verbatim — Codex
// rollout lines (response_item / event_msg) don't embed the session id, so only
// the header changes. `codex resume <newID>` then continues from the fork point.
//
// This mirrors jsonl.ForkCopy (Claude) for the Codex rollout schema. It is a
// pure file operation: no process is spawned, and the fork is resumed lazily on
// its first send like any idle session.
func ForkCopy(srcPath, dstPath, throughTurnID, newID, srcID string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := newScanner(f)
	var out bytes.Buffer
	first := true
	forked := false
	for sc.Scan() {
		raw := sc.Bytes()
		if first {
			first = false
			hdr, err := rewriteForkHeader(raw, newID, srcID)
			if err != nil {
				return err
			}
			out.Write(hdr)
			out.WriteByte('\n')
			continue
		}
		out.Write(raw)
		out.WriteByte('\n')
		if isTaskComplete(raw, throughTurnID) {
			forked = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if first {
		return errors.New("source rollout is empty")
	}
	if !forked {
		return fmt.Errorf("fork turn %q not found in rollout", throughTurnID)
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dstPath, out.Bytes(), 0o644)
}

// rewriteForkHeader edits the session_meta header line: payload.id = newID and
// payload.forked_from_id = parent_thread_id = srcID. It round-trips through maps
// so every other header field (cwd, cli_version, base_instructions, …) is
// preserved; key order is not (JSON parsing is order-independent).
func rewriteForkHeader(headerLine []byte, newID, srcID string) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(headerLine, &top); err != nil {
		return nil, fmt.Errorf("parse session_meta header: %w", err)
	}
	if t, _ := unquote(top["type"]); t != "session_meta" {
		return nil, fmt.Errorf("first rollout line is %q, not session_meta", t)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(top["payload"], &payload); err != nil {
		return nil, fmt.Errorf("parse session_meta payload: %w", err)
	}
	payload["id"] = quote(newID)
	payload["forked_from_id"] = quote(srcID)
	payload["parent_thread_id"] = quote(srcID)

	pb, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	top["payload"] = pb
	return json.Marshal(top)
}

// isTaskComplete reports whether raw is an event_msg/task_complete with the
// given turn_id — the inclusive end of the forked turn.
func isTaskComplete(raw []byte, turnID string) bool {
	var l line
	if err := json.Unmarshal(raw, &l); err != nil || l.Type != "event_msg" {
		return false
	}
	var p struct {
		Type   string `json:"type"`
		TurnID string `json:"turn_id"`
	}
	if err := json.Unmarshal(l.Payload, &p); err != nil {
		return false
	}
	return p.Type == "task_complete" && p.TurnID == turnID
}

func quote(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func unquote(raw json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}
