// Package backend defines the backend-neutral contracts between the router and
// concrete coding-agent adapters. It contains no backend implementations.
package backend

import (
	"context"
	"encoding/json"

	"github.com/nexustar/usher/internal/core"
)

// Event is one raw transcript record or synthesized runtime event emitted by a
// backend. Type identifies the stable usher event vocabulary below.
type Event struct {
	Type string
	Raw  json.RawMessage
}

// CompletedOperation reports a local command that created no model turn.
func CompletedOperation(cwd string) <-chan Event {
	ch := make(chan Event, 2)
	started, _ := json.Marshal(ProcessStartedPayload{Cwd: cwd})
	ch <- Event{Type: EventProcessStarted, Raw: started}
	ch <- Event{Type: EventProcessExit, Raw: json.RawMessage(`{}`)}
	close(ch)
	return ch
}

const (
	EventProcessStarted = "subprocess.started"
	EventProcessExit    = "subprocess.exit"
	EventError          = "error"
	EventPartDelta      = "part.delta"
	EventTurnStatus     = "turn.status"
	EventRuntime        = "session.runtime"
	EventTurnUser       = "turn.user"
	EventPart           = "part"
)

// AbortedTurnMessage is the live wording for a turn that ended without
// completing. Only the live path shares it. Persisted markers deliberately keep
// whatever the backend itself recorded — pi's own errorMessage, codex's abort
// reason — so one interrupt can read differently before and after a reload.
// Claude uses neither side: it writes its own user-visible record instead.
const AbortedTurnMessage = "turn aborted before completion"

// Stable payloads for synthesized events. Persisted backend records remain raw
// because their schemas belong to the concrete transcript implementation.
type ProcessStartedPayload struct {
	Cwd   string `json:"cwd"`
	Fresh bool   `json:"fresh"`
}

type ErrorPayload struct {
	Message string `json:"message"`
}

type PartDeltaPayload struct {
	Delta string `json:"delta"`
}

type TurnStatusPayload struct {
	Status string `json:"status"`
}

// IsControlEvent reports whether an event is an usher runtime signal rather
// than a persisted backend transcript record suitable for an Assembler.
func IsControlEvent(t string) bool {
	switch t {
	case EventProcessStarted, EventProcessExit, EventError, EventPartDelta,
		EventTurnStatus, EventRuntime:
		return true
	default:
		return false
	}
}

// Assembler projects one backend's persisted records into display turns.
type Assembler interface {
	FeedLine(raw []byte) (completed []core.Turn, part *core.TurnPart)
	Flush() *core.Turn
	Model() string
}

// MultiPartAssembler optionally exposes every display part stored in one record.
type MultiPartAssembler interface {
	Assembler
	FeedLineParts(raw []byte) (completed []core.Turn, parts []*core.TurnPart)
}

// Transcript owns one backend's persisted session format.
type Transcript interface {
	ReadTurns(path string, limit int) ([]core.Turn, int, error)
	NewAssembler() Assembler
	IsTurnComplete(raw []byte) bool
}

// StartRequest describes the first turn of a new backend session.
type StartRequest struct {
	Cwd                string
	Prompt             string
	Model              string
	AppendSystemPrompt string
}

// Runtime owns live workers for one coding-agent backend.
type Runtime interface {
	Start(context.Context, StartRequest) (string, <-chan Event, error)
	Send(context.Context, string, string, string) (<-chan Event, error)
	Resume(context.Context, string, string) error
	Has(string) bool
	LiveSessions() []string
	Interrupt(string) error
	Kill(string) error
	Shutdown()
}

// ComposerItem is one completion advertised by a backend. Name is the bare
// command or skill identity; the frontend derives its / or $ sigil from the
// response source and Kind.
type ComposerItem struct {
	Name        string `json:"name"`
	Kind        string `json:"kind,omitempty"`
	Description string `json:"description,omitempty"`
}

// ComposerCatalog reports whether a backend could provide its authoritative
// catalog. Items must only be consumed when Available is true.
type ComposerCatalog struct {
	Items     []ComposerItem
	Available bool
}

// ComposerProvider is an optional runtime capability for cwd-dependent
// completions. Available is false when discovery deliberately avoids starting
// a cold backend.
type ComposerProvider interface {
	ComposerItems(context.Context, string, string) (ComposerCatalog, error)
}

// Forker is an optional capability because backends use materially different
// branching mechanisms and some may not support it at all.
type Forker interface {
	Fork(context.Context, string, string, string) (string, string, error)
}

// Renamer writes a title to backend-native metadata. path is the transcript.
type Renamer interface {
	Rename(context.Context, string, string, string) error
}

// SystemPrompter is an optional runtime capability: restoring a session's
// appended system prompt when a cold worker is rebuilt. StartRequest carries
// the prompt into a brand-new session, but no backend keeps it in the
// transcript, so a runtime that respawns processes (eviction, restart, resume)
// must ask for it again by session id. Runtimes that don't implement this lose
// the prompt on respawn.
type SystemPrompter interface {
	SetSystemPromptLookup(lookup func(sessionID string) string)
}

// Model is the backend-neutral model-picker projection.
type Model struct {
	ID             string   `json:"id"`
	DisplayName    string   `json:"display_name,omitempty"`
	ThinkingLevels []string `json:"thinking_levels,omitempty"`
}

// ModelProvider is an optional account-aware model catalog.
type ModelProvider interface {
	Models(context.Context) ([]Model, error)
	ValidateModel(context.Context, string) error
	DefaultEffort(context.Context, string) (string, error)
}

// Backend explicitly composes the capabilities registered for one agent CLI.
// Main constructs these values; there is deliberately no global init registry.
type Backend struct {
	Runtime    Runtime
	Transcript Transcript
	Forker     Forker
	Renamer    Renamer
	Models     ModelProvider
}
