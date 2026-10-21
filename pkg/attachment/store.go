package attachment

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidName    = errors.New("invalid attachment filename")
	ErrInvalidSession = errors.New("invalid attachment session id")
	ErrTooLarge       = errors.New("attachment too large")
)

// Save writes one attachment into root/sessionID without overwriting an
// existing file. It returns the absolute path agents can reference.
func Save(root, sessionID, filename string, src io.Reader, maxBytes int64) (string, error) {
	if sessionID == "" || sessionID == "." || sessionID == ".." || filepath.Base(sessionID) != sessionID {
		return "", ErrInvalidSession
	}
	name := filepath.Base(filename)
	if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
		return "", ErrInvalidName
	}
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	dst := filepath.Join(dir, name)
	var out *os.File
	var err error
	for i := 1; ; i++ {
		out, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		name = fmt.Sprintf("%s_%d%s", base, i, ext)
		dst = filepath.Join(dir, name)
	}

	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	reader := src
	if maxBytes > 0 {
		reader = io.LimitReader(src, maxBytes+1)
	}
	n, err := io.Copy(out, reader)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && n > maxBytes {
		return "", ErrTooLarge
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	ok = true
	return dst, nil
}
