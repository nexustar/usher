// Package window reads a slice of a session log's turns without parsing the
// whole file. Logs are append-only, so a turn's cursor is the byte offset it
// starts at, with an index after it when one line yields several turns.
package window

import (
	"bufio"
	"errors"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/nexustar/usher/internal/backend"
	"github.com/nexustar/usher/internal/core"
)

const (
	defaultProbe  = 2 << 20
	defaultBudget = 8 << 20
	// spanMax caps span growth: the turns a span holds ahead of the page are
	// parsed for nothing.
	spanMax = 8 << 20
	// warm is how many turns a mid-file span parses and discards first, to
	// populate assembler state carried between turns.
	warm    = 2
	maxLine = 16 << 20
)

// ErrBadCursor reports a cursor no Reader produced.
var ErrBadCursor = errors.New("bad transcript cursor")

// Lines yields, in file order, the transcript lines in [from, end) with the
// offset each starts at, until yield returns false. from is a line start. For
// a log holding several transcripts, last picks the one the span's last line
// belongs to rather than the one the line at end does.
type Lines func(f *os.File, from, end int64, last bool, yield func(off int64, line []byte) bool) error

// Reader reads turns out of one backend's log format.
type Reader struct {
	NewAssembler func() backend.Assembler
	// Lines defaults to FileLines: every line is part of the transcript.
	Lines Lines
	// Probe is the first span read back from a page's end; Budget is how many
	// bytes a page may parse before it is cut short. Zero picks the defaults.
	Probe, Budget int64
}

// Before returns the turns just before cursor before ("" for the end of the
// log): at most limit (0: all), fewer once parsing them has cost the budget.
// more reports whether older turns exist.
func (r Reader) Before(path, before string, limit int) (turns []core.Turn, more bool, err error) {
	f, size, err := open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	end := pos{off: size}
	if before != "" {
		if end, err = parseCursor(before); err != nil {
			return nil, false, err
		}
		if end.off >= size {
			end = pos{off: size}
		}
	}

	// Read back span by span, each one ending where the turns kept so far
	// begin, so no byte is parsed twice beyond the warm-up turns.
	var at []pos
	span, parsed := r.probe(), int64(0)
	for {
		from := int64(0)
		if limit > 0 && span < end.off {
			if from, err = lineStart(f, end.off-span, size); err != nil {
				return nil, false, err
			}
		}
		t, p, cold, err := r.readSpan(f, from, end, size)
		if err != nil {
			return nil, false, err
		}
		parsed += end.off - from
		if from > 0 {
			if len(t) <= warm || slices.Contains(cold[warm:], true) {
				span *= 4
				continue
			}
			t, p = t[warm:], p[warm:]
		}
		turns, at = append(t, turns...), append(p, at...)
		if limit > 0 && len(turns) > limit {
			return turns[len(turns)-limit:], true, nil
		}
		if from == 0 {
			return turns, false, nil
		}
		if len(turns) == limit || parsed >= r.budget() {
			return turns, true, nil
		}
		end = at[0]
		if span < spanMax {
			span *= 2
		}
	}
}

// From returns the turn at cursor from and every turn after it, or nothing
// when no turn starts there any more.
func (r Reader) From(path, from string) ([]core.Turn, error) {
	at, err := parseCursor(from)
	if err != nil {
		return nil, err
	}
	f, size, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	for span := r.probe(); ; span *= 4 {
		start := int64(0)
		if span < at.off {
			if start, err = lineStart(f, at.off-span, size); err != nil {
				return nil, err
			}
		}
		turns, ps, cold, err := r.readSpan(f, start, pos{off: size}, size)
		if err != nil {
			return nil, err
		}
		if start > 0 {
			if len(turns) <= warm || at.before(ps[warm]) || slices.Contains(cold[warm:], true) {
				continue
			}
			turns, ps = turns[warm:], ps[warm:]
		}
		for i, p := range ps {
			if p == at {
				return turns[i:], nil
			}
		}
		return nil, nil
	}
}

func (r Reader) probe() int64 {
	if r.Probe > 0 {
		return r.Probe
	}
	return defaultProbe
}

func (r Reader) budget() int64 {
	if r.Budget > 0 {
		return r.Budget
	}
	return defaultBudget
}

// pos is where a turn starts: the offset of its first line, and how many
// turns that same line yielded ahead of it.
type pos struct {
	off int64
	nth int
}

func (p pos) before(q pos) bool { return p.off < q.off || (p.off == q.off && p.nth < q.nth) }

func (p pos) String() string {
	if p.nth == 0 {
		return strconv.FormatInt(p.off, 10)
	}
	return strconv.FormatInt(p.off, 10) + "." + strconv.Itoa(p.nth)
}

// readSpan assembles the turns that start in [from, end), with where each one
// starts. A mid-file span opens partway through a turn, so everything ahead of
// its first user turn is dropped; cold marks turns that still needed state
// from before the span.
func (r Reader) readSpan(f *os.File, from int64, end pos, size int64) (turns []core.Turn, at []pos, cold []bool, err error) {
	// An end partway through a line's turns takes reading that whole line.
	stop := end.off
	if end.nth > 0 {
		if stop, err = lineStart(f, end.off+1, size); err != nil {
			return nil, nil, nil, err
		}
	}
	asm := r.NewAssembler()
	missed := func() bool {
		a, ok := asm.(backend.ColdStartAssembler)
		return ok && a.MissedContext() && from > 0
	}
	whole := from == 0
	last := pos{off: -1}
	add := func(t core.Turn, off int64, missed bool) {
		p := pos{off: off}
		if last.off == off {
			p.nth = last.nth + 1
		}
		last = p
		if !p.before(end) || (!whole && t.Role != "user") {
			return
		}
		whole = true
		t.Cursor = p.String()
		turns, at, cold = append(turns, t), append(at, p), append(cold, missed)
	}
	// open is where the assistant turn being assembled began: the first line
	// after the one that completed a turn.
	open := int64(-1)
	lines := r.Lines
	if lines == nil {
		lines = FileLines
	}
	err = lines(f, from, stop, end.nth > 0 || stop >= size, func(off int64, line []byte) bool {
		if open < 0 {
			open = off
		}
		completed, _ := asm.FeedLine(line)
		if len(completed) == 0 {
			return true
		}
		m := missed()
		for _, t := range completed {
			if t.Role == "assistant" {
				add(t, open, m)
			} else {
				add(t, off, m)
			}
		}
		open = -1
		return true
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if t := asm.Flush(); t != nil {
		add(*t, open, missed())
	}
	return turns, at, cold, nil
}

// FileLines is the Lines of a log whose every line belongs to the transcript.
func FileLines(f *os.File, from, end int64, _ bool, yield func(off int64, line []byte) bool) error {
	pos := from
	sc := bufio.NewScanner(io.NewSectionReader(f, from, end-from))
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		pos += int64(advance)
		return advance, token, err
	})
	for {
		off := pos
		if !sc.Scan() || !yield(off, sc.Bytes()) {
			return sc.Err()
		}
	}
}

// lineStart returns the offset of the first line that starts at or after at,
// or end when none does.
func lineStart(f *os.File, at, end int64) (int64, error) {
	if at <= 0 {
		return 0, nil
	}
	pos := at - 1
	r := bufio.NewReader(io.NewSectionReader(f, pos, end-pos))
	for {
		chunk, err := r.ReadSlice('\n')
		pos += int64(len(chunk))
		switch err {
		case nil:
			return pos, nil
		case bufio.ErrBufferFull:
		case io.EOF:
			return end, nil
		default:
			return 0, err
		}
	}
}

func open(path string) (*os.File, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

func parseCursor(s string) (pos, error) {
	offset, nth, split := strings.Cut(s, ".")
	off, err := strconv.ParseInt(offset, 10, 64)
	if err != nil || off < 0 {
		return pos{}, ErrBadCursor
	}
	p := pos{off: off}
	if split {
		if p.nth, err = strconv.Atoi(nth); err != nil || p.nth <= 0 {
			return pos{}, ErrBadCursor
		}
	}
	return p, nil
}
