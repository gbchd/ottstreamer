package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakmem"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/api"
	"ottstreamer/internal/event"
	"ottstreamer/internal/event/eventmem"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/fault/faultmem"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streammem"
	"ottstreamer/internal/ulid"
)

var t0 = time.Date(2026, 11, 6, 18, 40, 0, 0, time.UTC)

type nopRuntimes struct{}

func (nopRuntimes) Launch(context.Context, stream.Stream) error { return nil }
func (nopRuntimes) End(string, int64)                           {}
func (nopRuntimes) Drop(string)                                 {}

type nopMetrics struct{}

func (nopMetrics) FaultInjected(string, string, string) {}

const demoSpec = `{
  "formats": ["hls", "dash"],
  "segmentSeconds": 1.92,
  "video": [{"id": "720p", "width": 1280, "height": 720, "bitrateKbps": 3000}],
  "audio": [{"id": "en", "lang": "en", "channels": 2, "bitrateKbps": 128, "default": true}],
  "subtitles": [{"id": "sub-en", "lang": "en", "name": "English", "format": "webvtt", "default": true}],
  "captions": {"cea608": {"lang": "en"}},
  "ads": {"recurring": {"everySeconds": 300, "durationSeconds": 30, "offsetSeconds": 60}}
}`

const demo = `{"id": "demo", "name": "Demo channel", "spec": ` + demoSpec + `}`

func newServer(t *testing.T) (*httptest.Server, *time.Time) {
	t.Helper()
	return serve(t, func(h http.Handler) http.Handler { return h })
}

// serve runs the API behind wrap, on a test clock, with an in-memory event
// log.
func serve(t *testing.T, wrap func(http.Handler) http.Handler) (*httptest.Server, *time.Time) {
	t.Helper()
	now := t0
	repo := streammem.NewRepository()
	clock := func() time.Time { return now }
	var ids ulid.Generator
	logger := slog.New(slog.DiscardHandler)
	events := event.NewService(eventmem.NewRepository(100), clock, ids.New, func(event.Event) {}, logger)
	breaks := adbreak.NewService(repo, adbreakmem.NewRepository(), adbreakscte35.NewEncoder(), events, clock)
	faults := fault.NewService(faultmem.NewRepository(), events, nopMetrics{}, clock, func() int { return 0 })
	svc := stream.NewService(repo, repo, nopRuntimes{}, breaks, faults, events, clock)
	mux := http.NewServeMux()
	api.Register(mux, svc, breaks, events, faults, faults, clock, logger)
	srv := httptest.NewServer(wrap(mux))
	t.Cleanup(srv.Close)
	t.Cleanup(events.Close) // ends event streams, so that srv.Close does not wait for them
	return srv, &now
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, header ...string) response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	r := response{status: res.StatusCode, header: res.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &r.body) // not every body is a JSON object
	return r
}

// get walks a decoded JSON body along keys and array indexes.
func get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, _ := v.(map[string]any)
			v = m[k]
		case int:
			a, _ := v.([]any)
			if k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func expect(t *testing.T, r response, status int) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status = %d, want %d; body %s", r.status, status, r.raw)
	}
}

func TestCreateStream(t *testing.T) {
	srv, _ := newServer(t)

	r := do(t, srv, http.MethodPost, "/api/streams", demo)
	expect(t, r, http.StatusCreated)
	if r.header.Get("ETag") != `"1"` || r.header.Get("Location") != "/api/streams/demo" {
		t.Errorf("headers = %v", r.header)
	}
	checks := []struct {
		path []any
		want any
	}{
		{[]any{"id"}, "demo"},
		{[]any{"version"}, 1.0},
		{[]any{"status", "state"}, "stopped"},
		{[]any{"status", "audioAligned"}, true},
		{[]any{"spec", "segmentSeconds"}, 1.92},
		{[]any{"spec", "gopSeconds"}, 1.92},
		{[]any{"spec", "fps"}, 25.0},
		{[]any{"spec", "overlay", "renditionLabel"}, true},
		{[]any{"spec", "dash", "addressing"}, "timeline-time"},
		{[]any{"spec", "ads", "leadSeconds"}, 8.0},
		{[]any{"spec", "ads", "recurring", "everySeconds"}, 300.0},
		{[]any{"spec", "window", "dvrSeconds"}, 30.0},
		{[]any{"spec", "video", 0, "codec"}, "h264"},
		{[]any{"spec", "audio", 0, "group"}, "main"},
		{[]any{"spec", "subtitles", 0, "format"}, "webvtt"},
		{[]any{"spec", "subtitles", 0, "default"}, true},
		{[]any{"spec", "captions", "cea608", "channel"}, "CC1"},
		{[]any{"spec", "captions", "cea608", "lang"}, "en"},
	}
	for _, c := range checks {
		if got := get(r.body, c.path...); got != c.want {
			t.Errorf("%v = %v, want %v", c.path, got, c.want)
		}
	}

	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusConflict)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", `{"id":`), http.StatusBadRequest)
}

func TestCreateStream_Invalid(t *testing.T) {
	srv, _ := newServer(t)
	body := strings.Replace(demo, `"bitrateKbps": 3000`, `"bitrateKbps": 3000, "codec": "vp9"`, 1)
	body = strings.Replace(body, `"segmentSeconds": 1.92`, `"segmentSeconds": 1.92, "fps": 30`, 1)

	r := do(t, srv, http.MethodPost, "/api/streams", body)
	expect(t, r, http.StatusUnprocessableEntity)
	fields := map[string]string{}
	for i := 0; get(r.body, "fields", i) != nil; i++ {
		fields[get(r.body, "fields", i, "field").(string)] = get(r.body, "fields", i, "message").(string)
	}
	if !strings.Contains(fields["spec.video[0].codec"], "must be") || fields["spec.segmentSeconds"] == "" {
		t.Errorf("fields = %v", fields)
	}
}

func TestGetAndReplaceStream(t *testing.T) {
	srv, _ := newServer(t)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusCreated)

	r := do(t, srv, http.MethodGet, "/api/streams/demo", "")
	expect(t, r, http.StatusOK)
	if r.header.Get("ETag") != `"1"` {
		t.Errorf("ETag = %q", r.header.Get("ETag"))
	}
	expect(t, do(t, srv, http.MethodGet, "/api/streams/nope", ""), http.StatusNotFound)

	put := `{"name": "Renamed", "spec": ` + demoSpec + `}`
	expect(t, do(t, srv, http.MethodPut, "/api/streams/demo", put), http.StatusPreconditionRequired)
	expect(t, do(t, srv, http.MethodPut, "/api/streams/demo", put, "If-Match", `"7"`), http.StatusPreconditionFailed)
	expect(t, do(t, srv, http.MethodPut, "/api/streams/demo", put, "If-Match", `garbage`), http.StatusPreconditionFailed)

	r = do(t, srv, http.MethodPut, "/api/streams/demo", put, "If-Match", `"1"`)
	expect(t, r, http.StatusOK)
	if r.header.Get("ETag") != `"2"` || get(r.body, "name") != "Renamed" {
		t.Errorf("PUT = %v %s", r.header, r.raw)
	}

	r = do(t, srv, http.MethodGet, "/api/streams", "")
	expect(t, r, http.StatusOK)
	if !strings.HasPrefix(r.raw, `[{"createdAt"`) {
		t.Errorf("list = %s", r.raw)
	}
}

func TestLifecycle(t *testing.T) {
	srv, now := newServer(t)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusCreated)
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/stop", ""), http.StatusConflict)

	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/start", `{"ast": "2026-11-06T18:40:01Z"}`), http.StatusUnprocessableEntity)
	r := do(t, srv, http.MethodPost, "/api/streams/demo/start", "")
	expect(t, r, http.StatusOK)
	if get(r.body, "status", "state") != "running" || get(r.body, "status", "epoch", "ast") != "2026-11-06T18:40:00Z" {
		t.Errorf("start = %s", r.raw)
	}
	if v, ok := get(r.body, "status").(map[string]any)["epoch"].(map[string]any)["endSegment"]; !ok || v != nil {
		t.Errorf("endSegment = %v, %v; want null", v, ok)
	}
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/start", ""), http.StatusConflict)
	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo", ""), http.StatusConflict)

	*now = t0.Add(1920 * time.Millisecond * 10) // live edge 9, 5 lead segments
	r = do(t, srv, http.MethodGet, "/api/streams/demo/status", "")
	expect(t, r, http.StatusOK)
	if get(r.body, "liveEdge") != 9.0 || get(r.body, "frozenHorizon") != 14.0 || get(r.body, "epoch") != 1.0 {
		t.Errorf("status = %s", r.raw)
	}

	r = do(t, srv, http.MethodPost, "/api/streams/demo/stop", "")
	expect(t, r, http.StatusOK)
	if get(r.body, "status", "state") != "ended" || get(r.body, "status", "epoch", "endSegment") != 14.0 {
		t.Errorf("stop = %s", r.raw)
	}
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/stop", ""), http.StatusConflict)

	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo", "", "If-Match", `"1"`), http.StatusPreconditionFailed)
	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo", ""), http.StatusNoContent)
	expect(t, do(t, srv, http.MethodGet, "/api/streams/demo", ""), http.StatusNotFound)
}

func TestGetTime(t *testing.T) {
	srv, now := newServer(t)
	*now = t0.Add(123 * time.Millisecond)
	r := do(t, srv, http.MethodGet, "/api/time", "")
	expect(t, r, http.StatusOK)
	if r.raw != "2026-11-06T18:40:00.123Z" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") {
		t.Errorf("time = %q, %q", r.raw, r.header.Get("Content-Type"))
	}
}
