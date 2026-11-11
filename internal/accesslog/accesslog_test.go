package accesslog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/accesslog"
)

type observation struct {
	route, kind string
	status      int
}

type recorder struct{ got []observation }

func (r *recorder) HTTPRequest(route, kind string, status int, _ time.Duration) {
	r.got = append(r.got, observation{route, kind, status})
}

// server is /live routed by the outer mux, and /api behind a middleware that
// copies the request, as apikey does, then an inner mux.
func server(logs *bytes.Buffer, rec *recorder) http.Handler {
	logger := slog.New(accesslog.WithRequestID(slog.NewJSONHandler(logs, nil)))
	inner := http.NewServeMux()
	inner.HandleFunc("GET /api/streams/{id}", func(w http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "inside")
		w.WriteHeader(http.StatusTeapot)
	})
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			accesslog.Add(r.Context(), slog.String("key", "qa"))
			next.ServeHTTP(w, r.WithContext(r.Context()))
		})
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", auth(accesslog.Routed(inner)))
	mux.HandleFunc("GET /live/{stream}/{rendition}/{segment}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("media"))
	})
	return accesslog.New(logger, rec, mux)
}

func lines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for l := range strings.Lines(logs.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

func TestNew_live(t *testing.T) {
	var logs bytes.Buffer
	rec := &recorder{}
	h := server(&logs, rec)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/live/demo/720p/42.m4s", nil))

	if id := w.Header().Get("X-Request-Id"); len(id) != 26 {
		t.Errorf("generated X-Request-Id = %q, want 26 characters", id)
	}
	want := observation{"GET /live/{stream}/{rendition}/{segment}", "segment", 200}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Errorf("metrics = %v, want %v", rec.got, want)
	}
	l := lines(t, &logs)
	if len(l) != 1 {
		t.Fatalf("%d log lines, want 1", len(l))
	}
	for k, v := range map[string]any{
		"msg": "http request", "route": want.route, "stream": "demo", "rendition": "720p",
		"segment": "42.m4s", "status": 200.0, "bytes": 5.0, "request_id": w.Header().Get("X-Request-Id"),
	} {
		if l[0][k] != v {
			t.Errorf("%s = %v, want %v", k, l[0][k], v)
		}
	}
}

func TestNew_nestedMux(t *testing.T) {
	var logs bytes.Buffer
	rec := &recorder{}
	h := server(&logs, rec)
	r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/streams/demo", nil)
	r.Header.Set("X-Request-Id", "abc-123")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if id := w.Header().Get("X-Request-Id"); id != "abc-123" {
		t.Errorf("X-Request-Id = %q, want the client's", id)
	}
	want := observation{"GET /api/streams/{id}", "api", http.StatusTeapot}
	if len(rec.got) != 1 || rec.got[0] != want {
		t.Errorf("metrics = %v, want %v", rec.got, want)
	}
	l := lines(t, &logs)
	if len(l) != 2 || l[0]["msg"] != "inside" || l[0]["request_id"] != "abc-123" {
		t.Fatalf("log lines %v, want the handler's line with request_id first", l)
	}
	if l[1]["stream"] != "demo" || l[1]["key"] != "qa" || l[1]["route"] != want.route {
		t.Errorf("access line %v, want stream demo, key qa, route %s", l[1], want.route)
	}
}

func TestNew_invalidRequestID(t *testing.T) {
	var logs bytes.Buffer
	h := server(&logs, &recorder{})
	for _, id := range []string{"has space", "new\nline", strings.Repeat("a", 129)} {
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/nowhere", nil)
		r.Header.Set("X-Request-Id", id)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if got := w.Header().Get("X-Request-Id"); got == id || len(got) != 26 {
			t.Errorf("X-Request-Id %q kept as %q, want a new one", id, got)
		}
	}
	if l := lines(t, &logs); l[0]["route"] != "unmatched" || l[0]["status"] != 404.0 {
		t.Errorf("unmatched request logged as %v", l[0])
	}
}
