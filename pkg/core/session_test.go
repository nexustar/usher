package core

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The activity field leaves the wire when there is nothing to say.
func TestSessionActivityOmittedWhenZero(t *testing.T) {
	b, _ := json.Marshal(Session{ID: "s"})
	if strings.Contains(string(b), `"activity"`) {
		t.Fatalf("zero activity serialized: %s", b)
	}
	b, _ = json.Marshal(Session{ID: "s", Activity: Activity{Agents: 1}})
	if !strings.Contains(string(b), `"activity":{"agents":1}`) {
		t.Fatalf("activity missing: %s", b)
	}
}

func TestActivityPins(t *testing.T) {
	cases := []struct {
		name string
		a    Activity
		want bool
	}{
		{"nothing", Activity{}, false},
		{"goal alone", Activity{Goal: &Goal{Condition: "x", Status: "active"}}, false},
		{"agents", Activity{Agents: 1}, true},
		{"loop", Activity{Loops: []Loop{{ID: "a"}}}, true},
		{"wake ahead", Activity{NextWake: time.Now().Add(time.Hour)}, true},
		{"wake just missed", Activity{NextWake: time.Now().Add(-wakeGrace + time.Minute)}, true},
		{"wake long past", Activity{NextWake: time.Now().Add(-wakeGrace - time.Minute)}, false},
	}
	for _, c := range cases {
		if got := c.a.Pins(); got != c.want {
			t.Errorf("%s: Pins() = %v, want %v", c.name, got, c.want)
		}
	}
}
