package telegram

import (
	"encoding/json"
	"testing"
)

func TestImageRefs(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`{"message":{"content":[{"type":"tool_use","name":"mcp__usher__show_image","input":{"file_path":"out/chart.png"}}]}}`, []string{"out/chart.png"}},
		{`{"message":{"content":[{"type":"tool_use","name":"show_image","input":{"file_path":"/abs/a.jpg"}}]}}`, []string{"/abs/a.jpg"}},
		{`{"message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`, nil},
		{`{"message":{"content":[{"type":"tool_use","name":"mcp__x__show_image","input":{}}]}}`, nil}, // empty path skipped
		{`not json`, nil},
	}
	for _, c := range cases {
		got := imageRefs(json.RawMessage(c.raw))
		if len(got) != len(c.want) {
			t.Errorf("imageRefs(%s) = %v, want %v", c.raw, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("imageRefs(%s)[%d] = %q, want %q", c.raw, i, got[i], c.want[i])
			}
		}
	}
}
