package api_test

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Comcast/scte35-go/pkg/scte35"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/stream"
)

// The demo stream has 1.92 s segments and 5 lead segments; recurring break
// r-0 is [32,48), r-1 starts at 188.
func TestBreaks(t *testing.T) {
	srv, now := newServer(t)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusCreated)
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/breaks", `{"duration": 30}`), http.StatusConflict)
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/start", ""), http.StatusOK)
	*now = t0.Add(1920 * time.Millisecond * 10) // live edge 9, frozen horizon 14

	r := do(t, srv, http.MethodPost, "/api/streams/demo/breaks", `{"duration": 30}`)
	expect(t, r, http.StatusCreated)
	checks := []struct {
		key  string
		want any
	}{
		{"id", "1"}, {"eventId", 1.0}, {"startSegment", 15.0}, {"endSegment", 31.0},
		{"requestedSeconds", 30.0}, {"durationSeconds", 30.72}, {"recurring", false}, {"cancelled", false},
		{"start", "2026-11-06T18:40:28.8Z"}, {"end", "2026-11-06T18:40:59.52Z"},
	}
	for _, c := range checks {
		if got := get(r.body, c.key); got != c.want {
			t.Errorf("%s = %v, want %v", c.key, got, c.want)
		}
	}
	sis, err := scte35.DecodeBase64(get(r.body, "cueOut", "base64").(string))
	if err != nil || sis.SpliceTimePTS() != 15*172800 || sis.DurationTicks() != 16*172800 {
		t.Errorf("cue-out = %+v, %v", sis, err)
	}
	if h, _ := get(r.body, "cueIn", "hex").(string); !strings.HasPrefix(h, "0xFC30") {
		t.Errorf("cue-in hex = %q", h)
	}

	errorCases := []struct {
		body   string
		status int
		msg    string
	}{
		{`{"duration": 30, "in": "20s"}`, http.StatusConflict, "adbreak: overlaps break 1"},
		{`{"duration": 10, "at": "2026-11-06T18:41:02Z"}`, http.StatusConflict, "adbreak: overlaps break r-0"},
		{`{"duration": 0}`, http.StatusUnprocessableEntity, ""},
		{`{"duration": 5, "in": "soon"}`, http.StatusUnprocessableEntity, ""},
	}
	for _, c := range errorCases {
		r := do(t, srv, http.MethodPost, "/api/streams/demo/breaks", c.body)
		expect(t, r, c.status)
		if c.msg != "" && get(r.body, "error") != c.msg {
			t.Errorf("%s: error = %v, want %q", c.body, get(r.body, "error"), c.msg)
		}
	}
	r = do(t, srv, http.MethodPost, "/api/streams/demo/breaks", `{"duration": 10, "at": "2026-11-06T18:41:02Z", "replaceRecurring": true}`)
	expect(t, r, http.StatusCreated)
	if get(r.body, "id") != "2" || get(r.body, "startSegment") != 33.0 {
		t.Errorf("replacing break = %s", r.raw)
	}

	r = do(t, srv, http.MethodGet, "/api/streams/demo/breaks", "")
	expect(t, r, http.StatusOK)
	var breaks []struct {
		ID        string `json:"id"`
		Cancelled bool   `json:"cancelled"`
	}
	if err := json.Unmarshal([]byte(r.raw), &breaks); err != nil {
		t.Fatal(err)
	}
	list := make([]string, 0, len(breaks))
	for _, b := range breaks {
		if b.Cancelled {
			b.ID += "x"
		}
		list = append(list, b.ID)
	}
	if strings.Join(list, " ") != "1 r-0x 2 r-1 r-2" {
		t.Errorf("list = %v", list)
	}

	expect(t, do(t, srv, http.MethodGet, "/api/streams/demo/breaks/r-1", ""), http.StatusOK)
	expect(t, do(t, srv, http.MethodGet, "/api/streams/demo/breaks/9", ""), http.StatusNotFound)
	expect(t, do(t, srv, http.MethodGet, "/api/streams/nope/breaks", ""), http.StatusNotFound)

	*now = t0.Add(1920 * time.Millisecond * 21) // live edge 20, frozen horizon 25
	r = do(t, srv, http.MethodGet, "/api/streams/demo/status", "")
	expect(t, r, http.StatusOK)
	if get(r.body, "activeBreak", "id") != "1" {
		t.Errorf("status = %s, want active break 1", r.raw)
	}
	r = do(t, srv, http.MethodDelete, "/api/streams/demo/breaks/1", "")
	expect(t, r, http.StatusOK)
	if get(r.body, "endSegment") != 26.0 || get(r.body, "cancelled") != false {
		t.Errorf("early return = %s", r.raw)
	}
	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo/breaks/1", ""), http.StatusConflict)
	r = do(t, srv, http.MethodDelete, "/api/streams/demo/breaks/2", "")
	expect(t, r, http.StatusOK)
	if get(r.body, "cancelled") != true {
		t.Errorf("cancel = %s", r.raw)
	}
}

func TestBreaksOfStreamNeverStarted(t *testing.T) {
	srv, _ := newServer(t)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusCreated)
	expect(t, do(t, srv, http.MethodGet, "/api/streams/demo/breaks", ""), http.StatusConflict)
	expect(t, do(t, srv, http.MethodGet, "/api/streams/demo/breaks/1", ""), http.StatusConflict)
	expect(t, do(t, srv, http.MethodDelete, "/api/streams/demo/breaks/1", ""), http.StatusConflict)
}

func TestDecodeCue(t *testing.T) {
	srv, now := newServer(t)
	expect(t, do(t, srv, http.MethodPost, "/api/streams", demo), http.StatusCreated)
	expect(t, do(t, srv, http.MethodPost, "/api/streams/demo/start", ""), http.StatusOK)
	*now = t0.Add(1920 * time.Millisecond * 10)
	r := do(t, srv, http.MethodPost, "/api/streams/demo/breaks", `{"duration": 30}`)
	expect(t, r, http.StatusCreated)
	cancel, err := adbreakscte35.NewEncoder().Encode(adbreak.Cue{Command: stream.CommandSpliceInsert, EventID: 1, Out: true, Cancel: true})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		cue          string
		out, cancels bool
	}{
		{get(r.body, "cueOut", "base64").(string), true, false},
		{get(r.body, "cueIn", "hex").(string), false, false},
		{base64.StdEncoding.EncodeToString(cancel), false, true},
		{"0x" + hex.EncodeToString(cancel), false, true},
	}
	for _, tt := range tests {
		r := do(t, srv, http.MethodGet, "/api/scte35?cue="+url.QueryEscape(tt.cue), "")
		expect(t, r, http.StatusOK)
		cmd := get(r.body, "section", "spliceCommand")
		if get(r.body, "command") != "splice_insert" || get(cmd, "spliceEventId") != 1.0 ||
			get(cmd, "outOfNetworkIndicator") != tt.out || get(cmd, "spliceEventCancelIndicator") != tt.cancels {
			t.Errorf("%s: %s", tt.cue, r.raw)
		}
	}

	for cue, status := range map[string]int{
		"":                  http.StatusBadRequest,
		"?cue=not%20base64": http.StatusUnprocessableEntity,
		"?cue=0xZZ":         http.StatusUnprocessableEntity,
		"?cue=" + url.QueryEscape(base64.StdEncoding.EncodeToString([]byte("garbage"))): http.StatusUnprocessableEntity,
	} {
		expect(t, do(t, srv, http.MethodGet, "/api/scte35"+cue, ""), status)
	}
}
