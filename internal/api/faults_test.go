package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// decode decodes a JSON body of any shape, for get.
func decode(t *testing.T, r response) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(r.raw), &v); err != nil {
		t.Fatalf("body %q: %v", r.raw, err)
	}
	return v
}

func TestFaults(t *testing.T) {
	srv, _ := newServer(t)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusCreated)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		expect(t, do(t, srv, m, "/api/streams/nope/faults", `[]`), http.StatusNotFound)
	}

	r := do(t, srv, http.MethodGet, "/api/streams/demo/faults", "")
	expect(t, r, http.StatusOK)
	if r.raw != "[]\n" {
		t.Errorf("no rules = %q, want []", r.raw)
	}

	r = do(t, srv, http.MethodPut, "/api/streams/demo/faults", `[{"id": "x", "match": {"kind": "segment"}, "percent": 101, "action": {"delayMs": 100}}]`)
	expect(t, r, http.StatusUnprocessableEntity)
	if get(r.body, "fields", 0, "field") != "faults[0].percent" {
		t.Errorf("422 = %s", r.raw)
	}

	put := `[{"id": "slow", "match": {"kind": "segment", "renditions": ["720p"], "segments": {"from": 10, "to": 20}},
		"ttlSeconds": 60, "action": {"delayMs": 3000}}]`
	r = do(t, srv, http.MethodPut, "/api/streams/demo/faults", put)
	expect(t, r, http.StatusOK)
	checks := []struct {
		path []any
		want any
	}{
		{[]any{0, "id"}, "slow"},
		{[]any{0, "percent"}, 100.0},
		{[]any{0, "mode"}, "sticky"},
		{[]any{0, "ttlSeconds"}, 60.0},
		{[]any{0, "expiresAt"}, "2026-11-06T18:41:00Z"},
		{[]any{0, "match", "renditions", 0}, "720p"},
		{[]any{0, "match", "segments", "to"}, 20.0},
		{[]any{0, "action", "delayMs"}, 3000.0},
	}
	for _, body := range []response{r, do(t, srv, http.MethodGet, "/api/streams/demo/faults", "")} {
		v := decode(t, body)
		for _, c := range checks {
			if got := get(v, c.path...); got != c.want {
				t.Errorf("%v = %v, want %v", c.path, got, c.want)
			}
		}
	}
	if strings.Contains(r.raw, "status") {
		t.Errorf("unset actions are written: %s", r.raw)
	}

	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo/faults", ""), http.StatusNoContent)
	if r := do(t, srv, http.MethodGet, "/api/streams/demo/faults", ""); r.raw != "[]\n" {
		t.Errorf("after clear = %q", r.raw)
	}
}

func TestSpecFaults(t *testing.T) {
	srv, _ := newServer(t)
	rule := `[{"id": "gone", "match": {"kind": "segment"}, "action": {"status": 404}}]`
	bad := strings.Replace(demo, `"ads":`, `"faults": [{"id": "gone", "match": {"kind": "mpd"}, "action": {"gap": true}}], "ads":`, 1)
	r := do(t, srv, http.MethodPost, "/api/streams", bad)
	expect(t, r, http.StatusUnprocessableEntity)
	if get(r.body, "fields", 0, "field") != "spec.faults[0].action.gap" {
		t.Errorf("422 = %s", r.raw)
	}

	expect(t, do(t, srv, http.MethodPost, "/api/streams", strings.Replace(demo, `"ads":`, `"faults": `+rule+`, "ads":`, 1)), http.StatusCreated)
	r = do(t, srv, http.MethodGet, "/api/streams/demo", "")
	if get(r.body, "spec", "faults", 0, "action", "status") != 404.0 || get(r.body, "spec", "faults", 0, "expiresAt") != nil {
		t.Errorf("spec.faults = %v", get(r.body, "spec", "faults"))
	}
	if r := do(t, srv, http.MethodGet, "/api/streams/demo/faults", ""); r.raw != "[]\n" {
		t.Errorf("active before start = %q", r.raw)
	}
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/start", ""), http.StatusOK)
	r = do(t, srv, http.MethodGet, "/api/streams/demo/faults", "")
	if get(decode(t, r), 0, "id") != "gone" {
		t.Errorf("active after start = %s", r.raw)
	}
}

func TestGetTime_ClockSkew(t *testing.T) {
	srv, now := newServer(t)
	*now = t0.Add(123 * time.Millisecond)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusCreated)
	expect(t, do(t, srv, http.MethodPut, "/api/streams/demo/faults",
		`[{"id": "skew", "match": {"kind": "time"}, "action": {"clockSkewSeconds": -90}}]`), http.StatusOK)

	tests := []struct{ query, want string }{
		{"", "2026-11-06T18:40:00.123Z"},
		{"?stream=demo", "2026-11-06T18:38:30.123Z"},
		{"?stream=nope", "2026-11-06T18:40:00.123Z"},
	}
	for _, tt := range tests {
		r := do(t, srv, http.MethodGet, "/api/time"+tt.query, "")
		expect(t, r, http.StatusOK)
		if r.raw != tt.want {
			t.Errorf("time%s = %q, want %q", tt.query, r.raw, tt.want)
		}
	}
}
