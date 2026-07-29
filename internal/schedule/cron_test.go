package schedule

import (
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) Spec {
	t.Helper()
	spec, err := ParseSpec(expr)
	if err != nil {
		t.Fatalf("ParseSpec(%q) = %v", expr, err)
	}
	return spec
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestNextAfter(t *testing.T) {
	cases := []struct {
		expr, from, want string
	}{
		{"0 9 * * *", "2026-07-28 08:59", "2026-07-28 09:00"},
		{"0 9 * * *", "2026-07-28 09:00", "2026-07-29 09:00"}, // strictly after
		{"*/15 * * * *", "2026-07-28 09:01", "2026-07-28 09:15"},
		{"5/20 * * * *", "2026-07-28 09:06", "2026-07-28 09:25"},
		{"0,30 * * * *", "2026-07-28 09:31", "2026-07-28 10:00"},
		{"0 0 1 1 *", "2026-07-28 09:00", "2027-01-01 00:00"},
		{"0 0 * * *", "2026-07-28 09:00", "2026-07-29 00:00"},
		{"0 * * * *", "2026-07-28 09:30", "2026-07-28 10:00"},
		{"0 9 * * mon-fri", "2026-07-31 09:00", "2026-08-03 09:00"}, // Fri → Mon
		{"0 9 * * 7", "2026-07-28 09:00", "2026-08-02 09:00"},       // 7 is Sunday
		{"0 12 1 jan *", "2026-07-28 09:00", "2027-01-01 12:00"},
		{"0 0 29 2 *", "2026-07-28 09:00", "2028-02-29 00:00"}, // next leap year
		// Both day fields narrowed: Vixie and POSIX OR them, so this is the
		// 13th *and* every Friday, not Friday the 13th. 2026-07-31 is a Friday.
		{"0 0 13 * 5", "2026-07-28 09:00", "2026-07-31 00:00"},
		// Only one narrowed: it alone decides, and the other is ANDed away.
		{"0 0 13 * *", "2026-07-28 09:00", "2026-08-13 00:00"},
		{"0 0 * * 5", "2026-07-28 09:00", "2026-07-31 00:00"},
		// A day field that narrows but still starts with `*` counts as
		// unnarrowed, which is how Vixie reads it: odd days AND Mondays, so
		// the next match is the first Monday that falls on an odd day. Read as
		// OR it would be tomorrow, 2026-07-29 being odd.
		{"0 0 */2 * 1", "2026-07-28 09:00", "2026-08-03 00:00"},
	}
	for _, c := range cases {
		got := mustParse(t, c.expr).NextAfter(at(t, c.from))
		if want := at(t, c.want); !got.Equal(want) {
			t.Errorf("%q after %s = %s, want %s", c.expr, c.from, got, want)
		}
	}
}

// February 30 parses (both fields are in range) but no calendar reaches it;
// the walk has to give up rather than spin.
func TestNextAfterUnreachable(t *testing.T) {
	if got := mustParse(t, "0 0 30 2 *").NextAfter(time.Now()); !got.IsZero() {
		t.Fatalf("NextAfter = %s, want zero", got)
	}
}

// Spring forward deletes an hour of wall clock, and asking time.Date for an
// instant inside it gets one at or before where the walk already is. Every
// case here hangs forever without the guard in NextAfter — including specs
// that only pass through the gap on their way somewhere else.
func TestNextAfterCrossesDSTGap(t *testing.T) {
	// 2026-03-08: New York goes 01:59 EST → 03:00 EDT.
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	cases := []struct {
		expr, from, want string
	}{
		// The hour the clock skipped: the match simply does not occur that
		// day, and the walk carries on to the next one.
		{"30 2 * * *", "2026-03-07 12:00", "2026-03-09 02:30"},
		// An hour the clock kept, reached by walking through the gap.
		{"0 3 * * *", "2026-03-07 12:00", "2026-03-08 03:00"},
		{"0 1 * * *", "2026-03-07 12:00", "2026-03-08 01:00"},
		// Every minute: the first one on the far side of the gap.
		{"* * * * *", "2026-03-08 01:59", "2026-03-08 03:00"},
	}
	for _, c := range cases {
		from := inZone(t, c.from, newYork)
		want := inZone(t, c.want, newYork)
		done := make(chan time.Time, 1)
		go func() { done <- mustParse(t, c.expr).NextAfter(from) }()
		select {
		case got := <-done:
			if !got.Equal(want) {
				t.Errorf("%q after %s = %s, want %s", c.expr, c.from, got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%q after %s did not return", c.expr, c.from)
		}
	}
}

func inZone(t *testing.T, value string, loc *time.Location) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, loc)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// The label has to be readable in the zones Go can name and in the ones it
// cannot — including the half-hour offsets that rule out an hours-only format.
func TestLocalZone(t *testing.T) {
	cases := []struct {
		zone *time.Location
		want string
	}{
		{time.FixedZone("JST", 9*3600), "JST"},
		{time.UTC, "UTC"},
		{time.FixedZone("", -3*3600-1800), "UTC-03:30"},     // no name at all
		{time.FixedZone("+0545", 5*3600+2700), "UTC+05:45"}, // numeric "name"
	}
	for _, c := range cases {
		// time.Local is the process's; override it so the fallback paths (an
		// unnamed zone, as in a container without /etc/localtime) are reachable.
		saved := time.Local
		time.Local = c.zone
		got := LocalZone(time.Date(2026, 7, 28, 9, 0, 0, 0, c.zone))
		time.Local = saved
		if got != c.want {
			t.Errorf("LocalZone(%v) = %q, want %q", c.zone, got, c.want)
		}
	}
}

func TestParseSpecErrors(t *testing.T) {
	cases := map[string]string{
		"* * * *":         "5 fields",
		"* * * * * *":     "5 fields",
		"60 * * * *":      "outside 0-59",
		"* 24 * * *":      "outside 0-23",
		"* * 0 * *":       "outside 1-31",
		"* * * 13 *":      "outside 1-12",
		"* * * * 8":       "outside 0-7",
		"nope * * * *":    "not a number",
		"*/0 * * * *":     "bad step",
		"10-5 * * * *":    "runs backwards",
		"1,,2 * * * *":    "empty value",
		"* * * * mon-xyz": "not a number",
		// Vixie's @-shorthands are deliberately absent, and a crontab is the
		// likeliest place an expression is copied from, so they must fail.
		"@daily": "5 fields",
	}
	for expr, want := range cases {
		_, err := ParseSpec(expr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseSpec(%q) = %v, want error containing %q", expr, err, want)
		}
	}
}

// The parser is the save-time gate, so the forms the pane suggests must all
// survive it.
func TestParseSpecAccepts(t *testing.T) {
	for _, expr := range []string{
		"* * * * *", "0 9 * * 1-5", "*/5 * * * *", "0 */2 * * *",
		"30 8,20 * * *", "0 0 1 * *", "15 3 * * SUN", "0 0 * DEC 1",
		"  0   9  *  *  *  ",
	} {
		if _, err := ParseSpec(expr); err != nil {
			t.Errorf("ParseSpec(%q) = %v", expr, err)
		}
	}
}
