// Package ui serves the read-only web UI under /ui/ (see "Web UI" in
// docs/spec/09-cli-api-ui.md): the built assets embedded in the binary,
// with a Content-Security-Policy that keeps the browser from any external
// call.
package ui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Prefix is the path of the UI.
const Prefix = "/ui/"

// CSP allows the UI to load and call only its own origin.
const CSP = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; font-src 'self'; " +
	"connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'none'; frame-ancestors 'none'"

// Handler serves the UI from fsys (the dist/ directory of the build: the
// UI is under app/). A build without the UI serves a page explaining how
// to build it.
func Handler(fsys fs.FS) http.Handler {
	app, err := fs.Sub(fsys, "app")
	if err == nil {
		if _, err = fs.Stat(app, "index.html"); err != nil {
			app = nil
		}
	}
	files := http.FileServerFS(app)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if app == nil {
			h.Set("Content-Type", "text/html; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(notBuilt))
			return
		}
		name := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(Prefix, "/"))
		if name == "" || name == "/" || name == "/index.html" {
			h.Set("Cache-Control", "no-cache")
			http.ServeFileFS(w, r, app, "index.html")
			return
		}
		if strings.HasPrefix(name, "/assets/") {
			// Hashed names: a new build gets new names.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if _, err := fs.Stat(app, path.Clean(strings.TrimPrefix(name, "/"))); err != nil {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = name
		files.ServeHTTP(w, r2)
	})
}

const notBuilt = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Tapyr</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:40rem;margin:4rem auto;padding:0 1rem;color:#0f2a3f;background:#f2f8fb}
code{background:#e8f2f7;padding:.1rem .3rem;border-radius:4px}@media(prefers-color-scheme:dark){body{color:#e4f1f7;background:#0b1824}code{background:#142c42}}</style>
</head><body>
<h1>Tapyr</h1>
<p>This binary was built without the web UI. Build it with <code>make ui</code> (Node.js 22.12 or later), then <code>make build</code>.</p>
<p>The status API answers at <a href="/api/v1/status">/api/v1/status</a>, the metrics at <a href="/metrics">/metrics</a> and <a href="/metrics/self">/metrics/self</a>.</p>
</body></html>
`
