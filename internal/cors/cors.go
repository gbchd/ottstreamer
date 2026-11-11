// Package cors answers CORS for the origin: playback (/live and /api/time)
// is open to every origin, the control API only to configured ones.
package cors

import (
	"net/http"
	"slices"
	"strings"
)

type policy struct {
	origins       []string // nil allows every origin
	methods       string
	allowHeaders  string
	exposeHeaders string
}

var playback = policy{
	methods:       "GET, HEAD, OPTIONS",
	allowHeaders:  "Range, Origin, Accept",
	exposeHeaders: "Content-Length, Content-Range, Date",
}

// New wraps next with CORS headers. Preflight requests are answered with 204
// here, before authentication runs.
func New(apiOrigins []string, next http.Handler) http.Handler {
	control := policy{
		origins:       append([]string{}, apiOrigins...), // never nil: no origin by default
		methods:       "GET, POST, PUT, DELETE, OPTIONS",
		allowHeaders:  "Authorization, Content-Type, If-Match",
		exposeHeaders: "ETag, Location",
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p *policy
		switch path := r.URL.Path; {
		case strings.HasPrefix(path, "/live/") || path == "/api/time":
			p = &playback
		case strings.HasPrefix(path, "/api/"):
			p = &control
		default:
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		allow := "*"
		if p.origins != nil {
			h.Add("Vary", "Origin")
			allow = ""
			if o := r.Header.Get("Origin"); slices.Contains(p.origins, o) {
				allow = o
			}
		}
		if allow != "" {
			h.Set("Access-Control-Allow-Origin", allow)
			h.Set("Access-Control-Expose-Headers", p.exposeHeaders)
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if allow != "" {
				h.Set("Access-Control-Allow-Methods", p.methods)
				h.Set("Access-Control-Allow-Headers", p.allowHeaders)
				h.Set("Access-Control-Max-Age", "86400")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
