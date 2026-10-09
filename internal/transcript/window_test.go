package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/window"
)

// writeClaudeLog writes n prompts, each answered by an assistant turn that calls a
// tool, so every turn but the user's spans several lines.
func writeClaudeLog(t *testing.T, n int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"ask %d"}}`+"\n", i)
		fmt.Fprintf(&b, `{"type":"assistant","message":{"role":"assistant","model":"m","content":[{"type":"text","text":"on it %d"},{"type":"tool_use","id":"tu%d","name":"Bash","input":{"command":"echo %d"}}]}}`+"\n", i, i, i)
		fmt.Fprintf(&b, `{"type":"system","subtype":"noise"}`+"\n")
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu%d","content":"%s"}]}}`+"\n", i, strings.Repeat("x", i*7%50))
		fmt.Fprintf(&b, `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done %d"}]}}`+"\n", i)
	}
	path := filepath.Join(t.TempDir(), "log.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeCodexLog writes a rollout whose turns lean on lines far behind them: the
// model is named once, and a command started in the first turn is read again
// in a late one.
func writeCodexLog(t *testing.T, n int) string {
	t.Helper()
	text := func(s string) string {
		raw, _ := json.Marshal(s)
		return `{"type":"input_text","text":` + string(raw) + `}`
	}
	exec := func(b *strings.Builder, id, script, chunk string) {
		input, _ := json.Marshal(script)
		fmt.Fprintf(b, `{"type":"response_item","payload":{"type":"custom_tool_call","call_id":%q,"name":"exec","input":%s}}`+"\n", id, input)
		fmt.Fprintf(b, `{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":%q,"output":[%s,%s]}}`+"\n",
			id, text("Script completed\nWall time 1.0 seconds\nOutput:\n"), text(chunk))
	}
	var b strings.Builder
	b.WriteString(`{"type":"session_meta","payload":{"id":"s"}}` + "\n")
	b.WriteString(`{"type":"turn_context","payload":{"model":"gpt-test"}}` + "\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"user_message","message":"ask %d"}}`+"\n", i)
		switch {
		case i == 0:
			exec(&b, "c0", `text(await tools.exec_command({cmd:"make build",yield_time_ms:1000}))`,
				`{"chunk_id":"a0","session_id":42,"output":"START\n"}`)
		case i%5 == 4:
			exec(&b, fmt.Sprintf("c%d", i), `text(await tools.write_stdin({session_id:42,chars:"",yield_time_ms:5000}));`,
				fmt.Sprintf(`{"chunk_id":"a%d","session_id":42,"output":"step %d\n"}`, i, i))
		}
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"agent_message","message":"reply %d"}}`+"\n", i)
		fmt.Fprintf(&b, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-%d"}}`+"\n", i)
	}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pageBack walks a transcript from its newest page to its first, returning the
// pages joined in transcript order.
func pageBack(t *testing.T, r window.Reader, path string, limit int) []core.Turn {
	t.Helper()
	var all []core.Turn
	for before := ""; ; before = all[0].Cursor {
		page, more, err := r.Before(path, before, limit)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 || len(page) > limit {
			t.Fatalf("page of %d turns, limit %d", len(page), limit)
		}
		all = append(append([]core.Turn{}, page...), all...)
		if !more {
			return all
		}
	}
}

// TestWindowMatchesWholeRead proves that paging back from the tail, and
// catching up from any turn, yield exactly the turns of a whole-file read —
// wherever the spans happen to cut and however small the budget.
func TestWindowMatchesWholeRead(t *testing.T) {
	logs := []struct {
		name   string
		newAsm func() backend.Assembler
		path   string
	}{
		{"claude", Claude{}.NewAssembler, writeClaudeLog(t, 40)},
		{"codex", Codex{}.NewAssembler, writeCodexLog(t, 24)},
		{"codex tool", Codex{}.NewAssembler, "../codex/rollout/testdata/rollout-tool.jsonl"},
		{"codex hello", Codex{}.NewAssembler, "../codex/rollout/testdata/rollout-hello.jsonl"},
	}
	for _, l := range logs {
		want, more, err := window.Reader{NewAssembler: l.newAsm}.Before(l.path, "", 0)
		if err != nil {
			t.Fatalf("%s: %v", l.name, err)
		}
		if more || len(want) == 0 {
			t.Fatalf("%s: whole read gave %d turns, more=%v", l.name, len(want), more)
		}
		for _, probe := range []int64{1, 97, 700, 5000} {
			for _, budget := range []int64{1, 4000, 1 << 30} {
				r := window.Reader{NewAssembler: l.newAsm, Probe: probe, Budget: budget}
				for _, limit := range []int{1, 3, 100} {
					if got := pageBack(t, r, l.path, limit); !reflect.DeepEqual(got, want) {
						t.Errorf("%s probe=%d budget=%d limit=%d: %d turns paged, %d read whole", l.name, probe, budget, limit, len(got), len(want))
					}
				}
				for i := range want {
					got, err := r.From(l.path, want[i].Cursor)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want[i:]) {
						t.Errorf("%s probe=%d: from turn %d gave %d turns, want %d", l.name, probe, i, len(got), len(want)-i)
					}
				}
			}
		}
	}
}

// TestWindowCodexCarriesState proves a page that starts long after a
// background command did still titles its card with the command, and names
// the model set at the top of the log.
func TestWindowCodexCarriesState(t *testing.T) {
	path := writeCodexLog(t, 25)
	r := window.Reader{NewAssembler: Codex{}.NewAssembler, Probe: 600}
	page, more, err := r.Before(path, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !more || len(page) != 2 || page[1].Model != "gpt-test" {
		t.Fatalf("tail page = %+v more=%v", page, more)
	}
	var titles []string
	for _, p := range page[1].Parts {
		if p.Type == "tool" {
			titles = append(titles, p.ToolTarget)
		}
	}
	if !reflect.DeepEqual(titles, []string{"make build"}) {
		t.Errorf("background command card titles = %q, want the command", titles)
	}
}

// TestWindowTail proves the newest page carries the turn still in progress,
// that a cursor keeps naming the same turn once the log grows, and that a
// budget cuts a page short without losing turns.
func TestWindowTail(t *testing.T) {
	path := writeClaudeLog(t, 30)
	r := window.Reader{NewAssembler: Claude{}.NewAssembler, Probe: 300}
	page, more, err := r.Before(path, "", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !more || len(page) != 4 || page[3].Role != "assistant" || page[2].Content != "ask 29" {
		t.Fatalf("tail page = %+v more=%v", page, more)
	}
	if parts := page[3].Parts; len(parts) != 3 || parts[1].ToolName != "Bash" || parts[2].Content != "done 29" {
		t.Fatalf("tail assistant parts = %+v", parts)
	}
	older, _, err := r.Before(path, page[0].Cursor, 4)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(f, `{"type":"user","message":{"role":"user","content":"one more"}}`)
	f.Close()

	if again, _, err := r.Before(path, page[0].Cursor, 4); err != nil || !reflect.DeepEqual(again, older) {
		t.Errorf("page before %s changed after the log grew (err %v)", page[0].Cursor, err)
	}
	caught, err := r.From(path, page[3].Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(caught) != 2 || !reflect.DeepEqual(caught[0], page[3]) || caught[1].Content != "one more" {
		t.Errorf("catching up from the last turn = %+v", caught)
	}
	if gone, err := r.From(path, "7"); err != nil || len(gone) != 0 {
		t.Errorf("a cursor that names no turn = %+v, %v", gone, err)
	}

	short, more, err := window.Reader{NewAssembler: Claude{}.NewAssembler, Probe: 300, Budget: 600}.Before(path, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if !more || len(short) == 0 || len(short) >= 20 || short[len(short)-1].Content != "one more" {
		t.Errorf("budgeted page: %d turns, more=%v", len(short), more)
	}
}
