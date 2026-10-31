package api

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

const consoleMissing = `<!doctype html><meta charset="utf-8"><title>ModelDock</title>
<body style="font-family:sans-serif;max-width:40em;margin:4em auto">
<h1>ModelDock API</h1>
<p>The web console is not embedded in this build. Build it with <code>make build</code>
(runs <code>npm run build</code> in <code>web/</code> and compiles with <code>-tags embedweb</code>),
or run it from source with <code>make web-dev</code>.</p>
<p>API: <code>/api/v1</code>, <code>/graphql</code>, <code>/healthz</code>, <code>/metrics</code>.</p>`

// consoleHandler serves the single-page app from fsys. Existing files are
// served as-is (hashed assets are cached for a year); any other path gets
// index.html so client-side routes survive a reload. If fsys is nil it
// serves a note explaining how to build the console.
func consoleHandler(fsys fs.FS) http.Handler {
	if fsys == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(consoleMissing))
		})
	}
	files := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != "index.html" {
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				if strings.HasPrefix(name, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if strings.HasPrefix(name, "assets/") || path.Ext(name) != "" {
				http.NotFound(w, r) // a missing file, not a client route
				return
			}
		}
		index, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			http.Error(w, "console index.html missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}
