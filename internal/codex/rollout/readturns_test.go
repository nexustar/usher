package rollout

import (
	"os"

	"github.com/nexustar/usher/internal/core"
)

// ReadTurns assembles a whole log in one pass: limit > 0 keeps the most recent
// N turns, and total is the count before that trim.
func ReadTurns(path string, limit int) (turns []core.Turn, total int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()

	asm := NewAssembler()
	sc := newScanner(f)
	for sc.Scan() {
		completed, _ := asm.Feed(sc.Bytes())
		turns = append(turns, completed...)
	}
	if t := asm.Flush(); t != nil {
		turns = append(turns, *t)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	total = len(turns)
	if limit > 0 && len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	return turns, total, nil
}
