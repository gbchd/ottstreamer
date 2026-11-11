package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/apikey"
)

var bearer = []string{"Authorization", "Bearer qa-secret"}

// newAuthServer serves the API behind a write key named qa, with stream
// demo started at t0 and the clock at live edge 9 (frozen horizon 14).
func newAuthServer(t *testing.T) (*httptest.Server, *time.Time) {
	t.Helper()
	auth, err := apikey.New([]string{"qa:write:qa-secret"})
	if err != nil {
		t.Fatal(err)
	}
	srv, now := serve(t, auth)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo, bearer...), http.StatusCreated)
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/start", "", bearer...), http.StatusOK)
	*now = t0.Add(1920 * time.Millisecond * 10)
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/breaks", `{"duration": 30}`, bearer...), http.StatusCreated) // [15,31)
	return srv, now
}

type page struct {
	Events []struct {
		ID, Type, Actor string
		Epoch, Segment  *int64
		Data            map[string]any
	}
	Next *string
}

func list(t *testing.T, srv *httptest.Server, query string) page {
	t.Helper()
	r := do(t, srv, http.MethodGet, "/api/streams/demo/events"+query, "", bearer...)
	expect(t, r, http.StatusOK)
	var p page
	if err := json.Unmarshal([]byte(r.raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p page) types() []string {
	out := make([]string, len(p.Events))
	for i, e := range p.Events {
		out[i] = e.Type
	}
	return out
}

func TestListEvents(t *testing.T) {
	srv, _ := newAuthServer(t)
	all := list(t, srv, "")
	if got, want := all.types(), []string{"stream.created", "stream.started", "break.scheduled"}; !slices.Equal(got, want) {
		t.Fatalf("types %v, want %v", got, want)
	}
	if all.Next != nil {
		t.Errorf("next = %q on the last page", *all.Next)
	}
	created, scheduled := all.Events[0], all.Events[2]
	if created.Actor != "api:qa" || created.Epoch != nil || created.Segment != nil || created.Data["name"] != "Demo channel" {
		t.Errorf("stream.created = %+v", created)
	}
	if *scheduled.Epoch != 1 || *scheduled.Segment != 15 || scheduled.Data["breakId"] != "1" || scheduled.Data["source"] != "manual" {
		t.Errorf("break.scheduled = %+v", scheduled)
	}

	first := list(t, srv, "?limit=2")
	if len(first.Events) != 2 || first.Next == nil || *first.Next != first.Events[1].ID {
		t.Fatalf("first page = %+v", first)
	}
	if rest := list(t, srv, "?limit=2&cursor="+*first.Next); !slices.Equal(rest.types(), []string{"break.scheduled"}) {
		t.Errorf("second page = %v", rest.types())
	}
	if got := list(t, srv, "?types=break.scheduled,stream.created").types(); !slices.Equal(got, []string{"stream.created", "break.scheduled"}) {
		t.Errorf("by types = %v", got)
	}
	if got := list(t, srv, "?from=2026-11-06T18:40:01Z").types(); !slices.Equal(got, []string{"break.scheduled"}) {
		t.Errorf("from = %v", got)
	}

	// The log outlives the stream; an id that never had one gives an empty page.
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/stop", "", bearer...), http.StatusOK)
	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo", "", bearer...), http.StatusNoContent)
	if got := list(t, srv, "?types=stream.deleted"); len(got.Events) != 1 || got.Events[0].Actor != "api:qa" {
		t.Errorf("after delete: %+v", got)
	}
	r := do(t, srv, http.MethodGet, "/api/streams/never/events", "", bearer...)
	if expect(t, r, http.StatusOK); r.raw != `{"events":[],"next":null}`+"\n" {
		t.Errorf("unknown stream: %s", r.raw)
	}
}

func TestStreamEvents(t *testing.T) {
	srv, _ := newAuthServer(t)
	ids := list(t, srv, "")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/streams/demo/events/live?since=ignored", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(bearer[0], bearer[1])
	req.Header.Set("Last-Event-ID", ids.Events[0].ID) // wins over since
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); res.StatusCode != http.StatusOK || ct != "text/event-stream" {
		t.Fatalf("status %d, content type %q", res.StatusCode, ct)
	}

	lines := bufio.NewScanner(res.Body)
	next := func() (id, typ string) {
		t.Helper()
		for lines.Scan() {
			line := lines.Text()
			if v, ok := strings.CutPrefix(line, "id: "); ok {
				id = v
			}
			if v, ok := strings.CutPrefix(line, "data: "); ok {
				var e struct{ ID, Type string }
				if err := json.Unmarshal([]byte(v), &e); err != nil || e.ID != id {
					t.Fatalf("data %s for id %s: %v", v, id, err)
				}
				return id, e.Type
			}
		}
		t.Fatalf("stream ended: %v", lines.Err())
		return "", ""
	}
	for _, want := range ids.Events[1:] { // the replay
		if id, typ := next(); id != want.ID || typ != want.Type {
			t.Errorf("replayed %s %s, want %s %s", id, typ, want.ID, want.Type)
		}
	}
	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo/breaks/1", "", bearer...), http.StatusOK)
	if _, typ := next(); typ != "break.cancelled" {
		t.Errorf("pushed %s, want break.cancelled", typ)
	}
}

func TestGetTimeline(t *testing.T) {
	srv, _ := newAuthServer(t)
	r := do(t, srv, http.MethodGet, "/api/streams/demo/timeline", "", bearer...)
	expect(t, r, http.StatusOK)
	for key, want := range map[string]any{
		"state": "running", "epoch": 1.0, "ast": "2026-11-06T18:40:00Z", "segmentSeconds": 1.92,
		"liveEdge": 9.0, "frozenHorizon": 14.0, "dvrStart": 0.0,
		"from": "2026-11-06T18:39:49.2Z", "to": "2026-11-06T18:40:49.2Z", // now ± the 30 s window
	} {
		if got := get(r.body, key); got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if get(r.body, "breaks", 0, "id") != "1" || get(r.body, "breaks", 1) != nil {
		t.Errorf("breaks = %v, want only 1", get(r.body, "breaks"))
	}
	if get(r.body, "events", 2, "type") != "break.scheduled" || get(r.body, "eventsNext") != nil {
		t.Errorf("events = %v", get(r.body, "events"))
	}

	// A later range shows the recurring r-0 [32,48) and no events.
	r = do(t, srv, http.MethodGet, "/api/streams/demo/timeline?from=2026-11-06T18:41:00Z&to=2026-11-06T18:41:10Z", "", bearer...)
	if expect(t, r, http.StatusOK); get(r.body, "breaks", 0, "id") != "r-0" || len(get(r.body, "events").([]any)) != 0 {
		t.Errorf("later range: breaks %v, events %v", get(r.body, "breaks"), get(r.body, "events"))
	}
	expect(t, do(t, srv, http.MethodGet, "/api/streams/never/timeline", "", bearer...), http.StatusNotFound)
}
