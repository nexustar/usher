package api

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexustar/usher/pkg/attachment"
	"github.com/nexustar/usher/pkg/pathutil"
)

const maxUploadSize = 100 << 20

// draftsDir holds uploads made before a session exists, one subdirectory per
// upload. Session ids are UUIDs, so it can't collide with a session's own.
const draftsDir = "_drafts"

// Upload is the body of POST /uploads: the absolute path agents can read.
type Upload struct {
	Path string `json:"path"`
}

// handleUpload stores the request body as ?filename= under ?session_id='s
// attachments, or as a draft for a session not created yet.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	root, dir := s.AttachmentsDir, r.URL.Query().Get("session_id")
	if dir != "" {
		if !s.known(w, dir) {
			return
		}
	} else {
		var b [8]byte
		_, _ = rand.Read(b[:]) // documented never to fail
		root, dir = filepath.Join(s.AttachmentsDir, draftsDir), fmt.Sprintf("%x", b[:])
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	path, err := attachment.Save(root, dir, r.URL.Query().Get("filename"), r.Body, maxUploadSize)
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, attachment.ErrInvalidName):
		WriteError(w, http.StatusBadRequest, "invalid filename")
	case errors.Is(err, attachment.ErrTooLarge) || errors.As(err, &maxErr):
		WriteError(w, http.StatusRequestEntityTooLarge, "file too large")
	case err != nil:
		WriteError(w, http.StatusInternalServerError, "failed to write file")
	default:
		WriteJSON(w, http.StatusCreated, Upload{Path: path})
	}
}

// imageContentTypes is the /image allowlist (also the forced Content-Type).
// Raster only — SVG is excluded because directly navigating to a same-origin SVG
// would execute its scripts in usher's authenticated origin (an <img> load is
// inert, but the endpoint must be safe under direct navigation too).
var imageContentTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// handleImage serves an image file from a session's working directory or
// /tmp.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.Router.GetSession(id)
	if !ok {
		WriteError(w, http.StatusNotFound, "session not found")
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		WriteError(w, http.StatusBadRequest, "missing path")
		return
	}
	ctype, ok := imageContentTypes[strings.ToLower(filepath.Ext(rel))]
	if !ok {
		WriteError(w, http.StatusForbidden, "not a supported image type")
		return
	}
	// Beyond the cwd: this session's uploads, the pre-session uploads its
	// first message points at, Codex's generated images.
	full, ok := pathutil.ResolveImagePath(sess.Cwd, rel,
		filepath.Join(s.AttachmentsDir, id),
		filepath.Join(s.AttachmentsDir, draftsDir),
		pathutil.CodexGeneratedImagesDir(id))
	if !ok {
		WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=60")
	http.ServeContent(w, r, filepath.Base(full), info.ModTime(), f)
}
