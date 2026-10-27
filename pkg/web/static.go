package web

import (
	"bytes"
	"io/fs"
	"net/http"
	"text/template"
)

// serveTemplated serves root/name with its {{.AppName}} placeholders filled,
// reading the file per request so --ui-dir edits show on reload.
func (s *Server) serveTemplated(root fs.FS, name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		src, err := fs.ReadFile(root, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var buf bytes.Buffer
		tmpl, err := template.New(name).Parse(string(src))
		if err == nil {
			err = tmpl.Execute(&buf, struct{ AppName string }{s.appName})
		}
		if err != nil {
			s.logger.Error("render static file", "file", name, "err", err)
			http.Error(w, "template error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(buf.Bytes())
	}
}
