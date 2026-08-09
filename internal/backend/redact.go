package backend

import (
	"fmt"
	"strings"
)

// RedactSpawnArgs returns a copy of a spawn argv safe for logging: flag
// values that carry conversation text (appended system prompts) are replaced
// by a length placeholder, so transcripts never end up in a log pipeline.
func RedactSpawnArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out)-1; i++ {
		switch {
		case out[i] == "--append-system-prompt":
			out[i+1] = redactedText(out[i+1])
			i++
		case out[i] == "-c" && strings.HasPrefix(out[i+1], "developer_instructions="):
			out[i+1] = "developer_instructions=" +
				redactedText(strings.TrimPrefix(out[i+1], "developer_instructions="))
			i++
		}
	}
	return out
}

func redactedText(v string) string { return fmt.Sprintf("[%d chars]", len(v)) }
