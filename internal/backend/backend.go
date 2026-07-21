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
	IsTurnAborted(raw []byte) bool
}

// StartRequest describes the first turn of a new backend session.
type StartRequest struct {
	Cwd    string
	Prompt string
	Model  string
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

// Forker is an optional capability because backends use materially different
// branching mechanisms and some may not support it at all.
type Forker interface {
	Fork(context.Context, string, string, string) (string, string, error)
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
	Models     ModelProvider
}
