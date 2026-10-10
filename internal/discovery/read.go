package discovery

import (
	"bufio"
	"io"
	"os"
	"sync"

	"github.com/nexustar/usher/internal/core"
)

const (
	// headMax bounds the search for a log's first prompt.
	headMax = 1 << 20
	// tailMin is the least of a log's end that is read; the tail grows from
	// there to tailMax while it holds no user prompt.
	tailMin = 256 << 10
	tailMax = 16 << 20
)

// reading is how far one session's log has been read. Logs only grow, so a
// change costs a read of what was appended.
type reading struct {
	mu      sync.Mutex
	scanner MetaScanner
	off     int64 // every complete line before this was fed or skipped
	whole   bool  // none was skipped
}

// read brings r up to date with the log at path and returns its metadata.
// whole asks for every line to have been read; the result reports whether
// that is so. Call with r.mu held.
func (r *reading) read(src Source, path string, whole bool) (core.SessionMeta, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return core.SessionMeta{}, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return core.SessionMeta{}, false, err
	}
	size := st.Size()
	switch {
	case r.scanner != nil && size >= r.off && (r.whole || !whole):
		r.off, err = feed(f, r.scanner, r.off, size, nil)
	case whole || !src.Sparse():
		r.scanner, r.whole = src.NewMetaScanner(path), true
		r.off, err = feed(f, r.scanner, 0, size, nil)
	default:
		err = r.readSparse(src, path, f, size)
	}
	if err != nil {
		r.scanner = nil
		return core.SessionMeta{}, false, err
	}
	return r.scanner.Meta(), r.whole, nil
}

// readSparse reads the log's first lines, up to its first prompt, and its
// last ones, back to its latest prompt.
func (r *reading) readSparse(src Source, path string, f *os.File, size int64) error {
	for tail := int64(tailMin); ; tail *= 4 {
		s := src.NewMetaScanner(path)
		head, err := feed(f, s, 0, min(size, headMax), func() bool { return s.Meta().Prompt != "" })
		if err != nil {
			return err
		}
		input := s.Meta().LastInputAt
		start, err := lineStart(f, size-tail, size)
		if err != nil {
			return err
		}
		whole := start <= head
		off, err := feed(f, s, max(start, head), size, nil)
		if err != nil {
			return err
		}
		if whole || tail >= tailMax || !s.Meta().LastInputAt.Equal(input) {
			r.scanner, r.off, r.whole = s, off, whole
			return nil
		}
	}
}

// feed gives s the complete lines of f in [from, to), stopping early once done
// reports true, and returns the offset after the last one fed.
func feed(f *os.File, s MetaScanner, from, to int64, done func() bool) (int64, error) {
	r := bufio.NewReaderSize(io.NewSectionReader(f, from, to-from), 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			return from, nil // an unterminated line is still being written
		}
		if err != nil {
			return from, err
		}
		from += int64(len(line))
		s.Feed(line)
		if done != nil && done() {
			return from, nil
		}
	}
}

// lineStart returns the offset of the first line starting at or after at, or
// end when none does.
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
