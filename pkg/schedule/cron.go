package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed five-field cron expression (minute hour day-of-month month
// day-of-week), each field a bitmask of the values it admits.
//
// The baseline is the POSIX crontab expression: five space-separated fields,
// each an `*`, a number, an inclusive `a-b` range, or a `,` list of those;
// weekday 0-6 with 0 being Sunday; and the day-of-month/day-of-week
// combination rule in matchDay.
//
// Dropped from POSIX:
//
//	nothing in the expression itself. usher stores one expression per task,
//	so the rest of the crontab file format — `#` comments, environment
//	assignments, the command — has nowhere to appear.
//
// Added from Vixie cron:
//
//	*/n, a-b/n, a/n    step values ("a/n" runs a to the field's maximum)
//	jan .. dec         month names, case-insensitive, usable in ranges
//	sun .. sat         weekday names, case-insensitive, usable in ranges
//	7                  a second spelling of Sunday
//
// Present in Vixie, not implemented here:
//
//	@daily @midnight @hourly @weekly @monthly @yearly @annually
//	@reboot
type Spec struct {
	minute, hour, dom, month, dow uint64

	// Whether each day field began with `*`, which is what decides how the
	// two combine. See matchDay.
	domStar, dowStar bool
}

// LocalZone names the clock specs are read on — the usher process's own — for
// frontends to label the cron field with. It prefers the IANA name
// ("Asia/Tokyo"), then the abbreviation ("JST"), then an offset computed from
// now ("UTC+05:45"), which in a DST zone follows the season.
func LocalZone(now time.Time) string {
	abbrev, offset := now.Zone()
	for _, name := range []string{time.Local.String(), abbrev} {
		if usableZoneName(name) {
			return name
		}
	}
	seconds, sign := offset, "+"
	if seconds < 0 {
		seconds, sign = -seconds, "-"
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, seconds/3600, seconds%3600/60)
}

// usableZoneName rejects what Go reports when it has no zone database to draw
// a name from: the empty string, "Local", and numeric "abbreviations" such as
// "+09" or "+0545".
func usableZoneName(name string) bool {
	return name != "" && name != "Local" &&
		!strings.HasPrefix(name, "+") && !strings.HasPrefix(name, "-")
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
	"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dayNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
}

type fieldDef struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteField = fieldDef{"minute", 0, 59, nil}
	hourField   = fieldDef{"hour", 0, 23, nil}
	domField    = fieldDef{"day of month", 1, 31, nil}
	monthField  = fieldDef{"month", 1, 12, monthNames}
	// 7 is folded into 0 on parse.
	dowField = fieldDef{"day of week", 0, 7, dayNames}
)

// ParseSpec is the only validation gate: the store calls it before saving, so
// a spec that reaches the runner is always well-formed.
func ParseSpec(expr string) (Spec, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Spec{}, fmt.Errorf(
			"cron needs 5 fields (minute hour day-of-month month day-of-week), got %d", len(parts))
	}
	var s Spec
	var err error
	if s.minute, err = parseField(parts[0], minuteField); err != nil {
		return Spec{}, err
	}
	if s.hour, err = parseField(parts[1], hourField); err != nil {
		return Spec{}, err
	}
	if s.dom, err = parseField(parts[2], domField); err != nil {
		return Spec{}, err
	}
	if s.month, err = parseField(parts[3], monthField); err != nil {
		return Spec{}, err
	}
	if s.dow, err = parseField(parts[4], dowField); err != nil {
		return Spec{}, err
	}
	s.domStar = strings.HasPrefix(parts[2], "*")
	s.dowStar = strings.HasPrefix(parts[4], "*")
	return s, nil
}

func parseField(field string, def fieldDef) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0, fmt.Errorf("%s: empty value in %q", def.name, field)
		}
		step := 1
		if base, stepText, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(strings.TrimSpace(stepText))
			if err != nil || n < 1 {
				return 0, fmt.Errorf("%s: bad step %q", def.name, stepText)
			}
			step, part = n, strings.TrimSpace(base)
		}
		lo, hi := def.min, def.max
		switch loText, hiText, isRange := strings.Cut(part, "-"); {
		case part == "*":
			// keep the full range
		case isRange:
			var err error
			if lo, err = parseValue(loText, def); err != nil {
				return 0, err
			}
			if hi, err = parseValue(hiText, def); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("%s: range %q runs backwards", def.name, part)
			}
		default:
			v, err := parseValue(part, def)
			if err != nil {
				return 0, err
			}
			lo = v
			// "5/15" is 5,20,35,50; a bare "5" is just itself.
			if step == 1 {
				hi = v
			}
		}
		for v := lo; v <= hi; v += step {
			bit := v
			if def.max == 7 && bit == 7 { // Sunday, spelled the other way
				bit = 0
			}
			mask |= 1 << uint(bit)
		}
	}
	return mask, nil
}

func parseValue(text string, def fieldDef) (int, error) {
	text = strings.TrimSpace(text)
	if def.names != nil {
		if v, ok := def.names[strings.ToLower(text)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", def.name, text)
	}
	if v < def.min || v > def.max {
		return 0, fmt.Errorf("%s: %d is outside %d-%d", def.name, v, def.min, def.max)
	}
	return v, nil
}

func bitSet(mask uint64, v int) bool { return mask&(1<<uint(v)) != 0 }

// matchDay combines the two day fields: AND when either began with `*`, OR
// when both were narrowed. "0 0 13 * *" is the 13th, "0 0 * * fri" is every
// Friday, and "0 0 13 * fri" is both. A field counts as narrowed by its first
// character, so `*/2` does not.
func (s Spec) matchDay(t time.Time) bool {
	dom, dow := bitSet(s.dom, t.Day()), bitSet(s.dow, int(t.Weekday()))
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}

// NextAfter returns the first matching minute strictly after t, or the zero
// time when none falls within five years (February 30 parses but no calendar
// reaches it). It walks by calendar component, rebuilding through time.Date at
// every step so that overflow normalizes and the zone offset is re-resolved.
func (s Spec) NextAfter(t time.Time) time.Time {
	loc := t.Location()
	t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc).Add(time.Minute)
	limit := t.Year() + 5
	for t.Year() <= limit {
		var next time.Time
		switch {
		case !bitSet(s.month, int(t.Month())):
			next = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.matchDay(t):
			next = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !bitSet(s.hour, t.Hour()):
			next = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !bitSet(s.minute, t.Minute()):
			next = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute()+1, 0, 0, loc)
		default:
			return t
		}
		// An hour the wall clock skips (spring forward) has no instant to name,
		// so time.Date answers with one at or before t — asked for 02:00 in
		// New York it returns 01:00. Stepping in absolute time leaves the gap.
		if !next.After(t) {
			next = t.Add(time.Minute)
		}
		t = next
	}
	return time.Time{}
}
