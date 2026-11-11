// Package accesslog gives every HTTP request an ID (X-Request-Id), writes one
// JSON line per answered request and reports it to the HTTP metrics.
package accesslog

import (
	"cmp"
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Recorder receives every answered request.
type Recorder interface {
	HTTPRequest(route, kind string, status int, took time.Duration)
}

type ctxKey int

const (
	idKey ctxKey = iota
	entryKey
)

// entry collects what inner handlers know about the request.
type entry struct {
	mu     sync.Mutex
	routed bool
	route  string
	attrs  []slog.Attr
}

// New wraps next. The request keeps a valid X-Request-Id or gets a new one,
// echoed in the response. After next returns, one "http request" line goes
// to logger and one observation to rec, labelled by the matched route.
func New(logger *slog.Logger, rec Recorder, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-Id")
		if !validID(id) {
			id = rand.Text()
		}
		w.Header().Set("X-Request-Id", id)
		e := &entry{}
		ctx := context.WithValue(context.WithValue(r.Context(), idKey, id), entryKey, e)
		r = r.WithContext(ctx)
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)

		e.capture(r) // the mux routed r in place, unless an inner mux did first
		status := cmp.Or(sw.status, http.StatusOK)
		took := time.Since(start)
		rec.HTTPRequest(e.route, kindOf(r.URL.Path), status, took)
		attrs := append([]slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route", e.route),
			slog.Int("status", status),
			slog.Int64("bytes", sw.bytes),
			slog.Duration("took", took),
		}, e.attrs...)
		logger.LogAttrs(ctx, slog.LevelInfo, "http request", attrs...)
	})
}

// Routed wraps a mux that runs behind a middleware copying the request
// (r.WithContext), so the access line still gets the route it matched.
func Routed(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
		if e, ok := r.Context().Value(entryKey).(*entry); ok {
			e.capture(r)
		}
	})
}

// Add appends attributes to the request's access line, such as the API key
// name or an injected fault.
func Add(ctx context.Context, attrs ...slog.Attr) {
	if e, ok := ctx.Value(entryKey).(*entry); ok {
		e.mu.Lock()
		e.attrs = append(e.attrs, attrs...)
		e.mu.Unlock()
	}
}

// WithRequestID wraps h so that every record logged with a request's
// context carries its request_id.
func WithRequestID(h slog.Handler) slog.Handler {
	return idHandler{h}
}

type idHandler struct{ slog.Handler }

func (h idHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := ctx.Value(idKey).(string); ok {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h idHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return idHandler{h.Handler.WithAttrs(attrs)}
}

func (h idHandler) WithGroup(name string) slog.Handler {
	return idHandler{h.Handler.WithGroup(name)}
}

// capture records the first route matched and the stream, rendition and
// segment path values.
func (e *entry) capture(r *http.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.routed {
		return
	}
	e.routed = true
	e.route = cmp.Or(r.Pattern, "unmatched")
	for _, a := range []struct{ attr, value string }{
		{"stream", cmp.Or(r.PathValue("stream"), r.PathValue("id"))},
		{"rendition", r.PathValue("rendition")},
		{"segment", r.PathValue("segment")},
	} {
		if a.value != "" {
			e.attrs = append(e.attrs, slog.String(a.attr, a.value))
		}
	}
}

// kindOf is the metrics kind of a path: api, manifest, init, segment or other.
func kindOf(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/"):
		return "api"
	case !strings.HasPrefix(path, "/live/"):
		return "other"
	case strings.HasSuffix(path, "/init.mp4"):
		return "init"
	case strings.HasSuffix(path, ".m3u8"), strings.HasSuffix(path, ".mpd"):
		return "manifest"
	default:
		return "segment"
	}
}

// validID accepts 1 to 128 characters of [A-Za-z0-9._:-], so a client's ID
// cannot forge log lines or headers.
func validID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	return strings.IndexFunc(id, func(c rune) bool {
		return (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("._:-", c)
	}) < 0
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach Flush on the real writer.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
