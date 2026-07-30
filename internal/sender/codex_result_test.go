package sender

import (
	"testing"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/codex"
)

func TestCodexTurnError(t *testing.T) {
	cases := []struct {
		name   string
		result codex.TurnResult
		want   string
	}{
		{"completed", codex.TurnResult{Status: "completed"}, ""},
		{"interrupted", codex.TurnResult{Status: "interrupted"}, backend.AbortedTurnMessage},
		{"failed", codex.TurnResult{Status: "failed"}, "codex turn failed"},
		// An explicit error outranks the status it arrives with.
		{"error wins", codex.TurnResult{Status: "interrupted", Error: "stream closed"}, "stream closed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexTurnError(tc.result); got != tc.want {
				t.Errorf("codexTurnError = %q, want %q", got, tc.want)
			}
		})
	}
}
