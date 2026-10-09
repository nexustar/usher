package pi

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
	"github.com/nexustar/usher/internal/window"
)

// TestBranchWindow proves paging a pi log follows the active branch only:
// abandoned replies sit between its entries in the file, and neither a page
// nor a catch-up read may pick them up, wherever a span happens to cut.
func TestBranchWindow(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"type":"session","version":3,"id":"sess-1","cwd":"/work"}` + "\n")
	parent := "null"
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, `{"type":"message","id":"u%d","parentId":%s,"message":{"role":"user","content":"ask %d"}}`+"\n", i, parent, i)
		// A failed reply is one record that yields two turns: what streamed
		// before the failure, and the error.
		if i%6 == 5 {
			fmt.Fprintf(&b, `{"type":"message","id":"e%d","parentId":"u%d","message":{"role":"assistant","content":[{"type":"text","text":"partial %d"}],"stopReason":"error","errorMessage":"boom %d"}}`+"\n", i, i, i, i)
			fmt.Fprintf(&b, `{"type":"message","id":"x%d","parentId":"u%d","message":{"role":"assistant","content":[{"type":"text","text":"abandoned %d"}]}}`+"\n", i, i, i)
			parent = fmt.Sprintf(`"e%d"`, i)
			continue
		}
		// A reply with nothing to show is no turn, wherever a page ends.
		if i%6 == 2 {
			fmt.Fprintf(&b, `{"type":"message","id":"t%d","parentId":"u%d","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}],"stopReason":"stop"}}`+"\n", i, i)
			parent = fmt.Sprintf(`"t%d"`, i)
			continue
		}
		fmt.Fprintf(&b, `{"type":"message","id":"x%d","parentId":"u%d","message":{"role":"assistant","content":[{"type":"text","text":"abandoned %d"}]}}`+"\n", i, i, i)
		fmt.Fprintf(&b, `{"type":"message","id":"a%d","parentId":"u%d","message":{"role":"assistant","content":[{"type":"text","text":"kept %d"},{"type":"toolCall","id":"tc%d","name":"bash","arguments":{"command":"ls"}}]}}`+"\n", i, i, i, i)
		fmt.Fprintf(&b, `{"type":"message","id":"r%d","parentId":"a%d","message":{"role":"toolResult","toolCallId":"tc%d","toolName":"bash","content":[{"type":"text","text":"ok"}]}}`+"\n", i, i, i)
		parent = fmt.Sprintf(`"r%d"`, i)
	}
	path := writeFixture(t, b.String())

	newAsm := func() backend.Assembler { return NewAssembler() }
	want, more, err := Transcript{}.ReadBefore(path, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(want) != 50 {
		t.Fatalf("whole read: %d turns, more=%v", len(want), more)
	}
	for _, turn := range want {
		for _, p := range turn.Parts {
			if strings.HasPrefix(p.Content, "abandoned") {
				t.Fatalf("abandoned branch in the transcript: %+v", turn)
			}
		}
	}
	for _, probe := range []int64{1, 150, 900, 6000} {
		r := window.Reader{NewAssembler: newAsm, Lines: branchLines, Probe: probe, Budget: 2000}
		var all []core.Turn
		for before := ""; ; before = all[0].Cursor {
			page, more, err := r.Before(path, before, 1+int(probe)%3)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				t.Fatalf("probe=%d: empty page before %q", probe, before)
			}
			all = append(append([]core.Turn{}, page...), all...)
			if !more {
				break
			}
		}
		if !reflect.DeepEqual(all, want) {
			t.Errorf("probe=%d: %d turns paged, %d read whole", probe, len(all), len(want))
		}
		for i := range want {
			got, err := r.From(path, want[i].Cursor)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want[i:]) {
				t.Errorf("probe=%d: from turn %d gave %d turns, want %d", probe, i, len(got), len(want)-i)
			}
		}
	}
}
