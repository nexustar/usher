package web

import (
	"compress/gzip"
	"net/http"
	"strings"
)

// gzipMiddleware compresses compressible 200 and 201 responses (the JSON API and the
// static SPA assets — app.js + vendored marked alone are ~100KB raw) when the
// client advertises gzip. This matters because usher's primary remote client
// is a phone: transcript JSON compresses 5–10×.
//
// SSE is unaffected by construction: text/event-stream is not in the
// compressible set, and the wrapper forwards Flush, so the /events and
// terminal screen handlers behave as they do unwrapped. Range requests
// bypass compression entirely (a gzipped 206 would corrupt the byte math).
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") ||
			r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		gw := &gzipWriter{ResponseWriter: w}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// gzipWriter decides per-response whether to compress: on WriteHeader it
// inspects the Content-Type the handler set and switches to a gzip body only
// for compressible types on a 200 or 201. Until then it is a transparent
// pass-through (404s, redirects, 304s, SSE streams all flow untouched).
type gzipWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	compress    bool
	wroteHeader bool
}

// compressibleTypes are matched as Content-Type prefixes (ignores charset).
var compressibleTypes = []string{
	"application/json",
	"application/javascript",
	"application/manifest+json",
	"text/html",
	"text/css",
	"text/javascript",
	"text/plain",
	"image/svg+xml",
}

func compressible(ct string) bool {
	for _, p := range compressibleTypes {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func (w *gzipWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if (code == http.StatusOK || code == http.StatusCreated) &&
		w.Header().Get("Content-Encoding") == "" &&
		compressible(w.Header().Get("Content-Type")) {
		w.compress = true
		// Length of the identity body no longer applies (FileServer sets it).
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.compress {
		if w.gz == nil {
			w.gz = gzip.NewWriter(w.ResponseWriter)
		}
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// Flush keeps the SSE handlers' `w.(http.Flusher)` assertion working through
// the wrapper. Compressed responses flush their gzip buffer first (unused in
// practice — streams are never compressed — but correct regardless).
func (w *gzipWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *gzipWriter) close() {
	if w.gz != nil {
		_ = w.gz.Close()
	}
}
