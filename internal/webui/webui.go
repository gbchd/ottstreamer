// Package webui serves the browser UI at /ui/: static files embedded in the
// binary that call /api like any other client.
package webui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Register adds the UI to mux: its files under /ui/, index.html for the
// page paths, and a redirect from / to /ui/.
func Register(mux *http.ServeMux) {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic(err) // static is embedded, so this cannot fail at run time
	}
	index, err := fs.ReadFile(files, "index.html")
	if err != nil {
		panic(err)
	}
	h := secure(index, http.StripPrefix("/ui/", http.FileServerFS(files)))
	page := secure(index, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	}))
	mux.Handle("GET /ui/", h)
	mux.Handle("GET /ui/{$}", page)
	mux.Handle("GET /ui/streams/", page) // client-side routes
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))
}

// secure adds a Content-Security-Policy that allows only the UI's own
// scripts and the inline import map of index, so an injected script cannot
// read the API key from sessionStorage. hls.js runs its worker and both
// players their media from blob: URLs; hls.js adds a data: text track.
func secure(index []byte, next http.Handler) http.Handler {
	csp := "default-src 'self'; script-src 'self' '" + importMapHash(index) + "'; style-src 'self' 'unsafe-inline'; " +
		"media-src 'self' blob: data:; worker-src 'self' blob:; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// ponytail: embedded files carry no modtime or ETag, so every load
		// downloads them again (1.4 MB with a player); hash-named files or
		// an ETag from a content hash if that ever matters.
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// importMapHash is the CSP source of the import map script in index.
func importMapHash(index []byte) string {
	const open, closing = `<script type="importmap">`, "</script>"
	_, rest, ok := bytes.Cut(index, []byte(open))
	if !ok {
		panic("index.html has no import map")
	}
	body, _, _ := bytes.Cut(rest, []byte(closing))
	sum := sha256.Sum256(body)
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}
