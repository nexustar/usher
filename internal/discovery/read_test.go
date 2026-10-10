package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLongLogReadByItsEnds proves a long log is listed from its first and last
// lines, gains its schedules once read whole, and from then on costs only what
// is appended to it.
func TestLongLogReadByItsEnds(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-work", "long.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	prompt := func(text string, at time.Duration) string {
		return fmt.Sprintf(`{"type":"user","cwd":"/work","timestamp":%q,"message":{"role":"user","content":%q}}`, stamp(at), text)
	}
	tool := func(id, name, input, result string, at time.Duration) string {
		return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`, stamp(at), id, name, input) + "\n" +
			fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]},"toolUseResult":%s}`, stamp(at), id, result)
	}
	lines := []string{
		prompt("first ask", -time.Hour),
		tool("t1", "CronCreate", `{"cron":"*/5 * * * *","prompt":"say hi"}`, `{"id":"job1","humanSchedule":"Every 5 minutes","recurring":true,"durable":false}`, -time.Hour),
	}
	filler := fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, stamp(-30*time.Minute), strings.Repeat("x", 4000))
	for i := 0; i < 2*tailMin/len(filler); i++ {
		lines = append(lines, filler)
	}
	lines = append(lines,
		prompt("last ask", -time.Minute),
		`{"type":"ai-title","aiTitle":"The Title"}`,
	)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := newTestDiscovery(t, root)
	d.Upsert(path)
	s, ok := d.Get("long")
	if !ok {
		t.Fatal("session not discovered")
	}
	if s.Prompt != "first ask" || s.Title != "The Title" || s.Cwd != "/work" || !s.LastInputAt.Equal(now.Add(-time.Minute)) {
		t.Errorf("listed from its ends as %+v", s)
	}
	if d.readings["long"].whole || len(s.Activity.Loops) != 0 {
		t.Fatalf("a log of %d lines was read whole to list it (loops %+v)", len(lines), s.Activity.Loops)
	}

	d.Complete("long")
	if s, _ = d.Get("long"); len(s.Activity.Loops) != 1 || s.Activity.Loops[0].ID != "job1" {
		t.Fatalf("loops after a whole read = %+v", s.Activity.Loops)
	}

	scanner, read := d.readings["long"].scanner, d.readings["long"].off
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	more := tool("t2", "CronDelete", `{"id":"job1"}`, `{"id":"job1"}`, 0) + "\n" + prompt("one more", 0) + "\n"
	fmt.Fprint(f, more, `{"type":"user","timestamp":"unfinished`)
	f.Close()
	d.Upsert(path)
	s, _ = d.Get("long")
	if len(s.Activity.Loops) != 0 {
		t.Errorf("loops after the job was deleted = %+v", s.Activity.Loops)
	}
	r := d.readings["long"]
	if r.scanner != scanner || r.off != read+int64(len(more)) {
		t.Errorf("an append of %d bytes moved the read from %d to %d (same scan: %v)", len(more), read, r.off, r.scanner == scanner)
	}

	// A log that shrank was replaced: nothing read from it still holds.
	if err := os.WriteFile(path, []byte(prompt("anew", 0)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Upsert(path)
	if r := d.readings["long"]; r.scanner == scanner || !r.whole {
		t.Errorf("a replaced log kept its old read")
	}
}
