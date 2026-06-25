// Package pathutil holds path-safety helpers shared across usher. Keeping the
// containment check in one place avoids divergence between the web image
// endpoint and the Telegram image mirror — both must resolve a user-supplied
// path strictly inside a session's working directory.
package pathutil

import (
	"path/filepath"
	"strings"
)

// ResolveWithinDir resolves rel (absolute, or relative to dir) and returns the
// real path only if it lies inside dir after symlink evaluation. The lexical
// check defends against ../ in rel; the EvalSymlinks check defends against a
// symlink inside dir pointing out. Both the file and dir must exist.
func ResolveWithinDir(dir, rel string) (string, bool) {
	if dir == "" || rel == "" {
		return "", false
	}
	full := rel
	if !filepath.IsAbs(full) {
		full = filepath.Join(dir, rel)
	}
	full = filepath.Clean(full)
	if !withinDir(dir, full) {
		return "", false
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", false
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}
	if !withinDir(realDir, realFull) {
		return "", false
	}
	return realFull, true
}

func withinDir(dir, path string) bool {
	rp, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rp != ".." && !strings.HasPrefix(rp, ".."+string(filepath.Separator))
}
