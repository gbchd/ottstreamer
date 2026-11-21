package livehttp_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/accesslog"
	"ottstreamer/internal/fault"
)

func segRange(from, to int64) *fault.Range { return &fault.Range{From: from, To: &to} }

func TestFaults_StatusSkipsTheBuild(t *testing.T) {
	srv := newServer(t, fault.Rule{ID: "s503", Match: fault.Match{Kind: fault.KindSegment, Renditions: []string{"720p"}, Segments: segRange(499, 499)},
		Action: fault.Action{Status: 503}})
	before := segmentWrites.Load()
	if r := get(t, srv, "/live/demo/720p/499.m4s"); r.status != http.StatusServiceUnavailable || r.body != "" {
		t.Errorf("hit: %d %q, want 503 and no body", r.status, r.body)
	}
	if n := segmentWrites.Load() - before; n != 0 {
		t.Errorf("%d segments written for a 503", n)
	}
	for _, path := range []string{"/live/demo/720p/498.m4s", "/live/demo/360p/499.m4s", "/live/demo/720p/init.mp4", "/live/demo/720p.m3u8"} {
		if r := get(t, srv, path); r.status != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, r.status)
		}
	}
	// A request that would 404 stays a plain 404.
	if r := get(t, srv, "/live/demo/720p/500.m4s"); r.status != http.StatusNotFound {
		t.Errorf("unpublished segment: %d, want 404", r.status)
	}
}

func TestFaults_Delay(t *testing.T) {
	srv := newServer(t, fault.Rule{ID: "slow", Match: fault.Match{Kind: fault.KindSegment}, Action: fault.Action{Delay: 50 * time.Millisecond}})
	start := time.Now()
	r := get(t, srv, "/live/demo/720p/499.m4s")
	if took := time.Since(start); took < 50*time.Millisecond || r.status != http.StatusOK {
		t.Errorf("%d after %v, want 200 after 50ms", r.status, took)
	}
}

func TestFaults_Reset(t *testing.T) {
	srv := newServer(t, fault.Rule{ID: "rst", Match: fault.Match{Kind: fault.KindSegment}, Action: fault.Action{Reset: true}})
	if r, err := fetch(t, srv, "/live/demo/720p/499.m4s"); err == nil {
		t.Errorf("got %d %q, want a transport error", r.status, r.body)
	}
}

func TestFaults_Truncate(t *testing.T) {
	srv := newServer(t, fault.Rule{ID: "cut", Match: fault.Match{Kind: fault.KindInit}, Action: fault.Action{TruncatePercent: 50}})
	r, err := fetch(t, srv, "/live/demo/720p/init.mp4", "Range", "bytes=0-1") // Range is ignored
	if err == nil || r.status != http.StatusOK || r.header.Get("Content-Length") != "9" || r.body != "init" {
		t.Errorf("%d Content-Length %s %q (err %v), want 200, 9, the first 4 bytes and an error",
			r.status, r.header.Get("Content-Length"), r.body, err)
	}
}

func TestFaults_Throttle(t *testing.T) {
	// 1 kbps is 125 B/s: chunks of 12, 12, 12 and 2 bytes, 96 ms apart.
	srv := newServer(t, fault.Rule{ID: "thr", Match: fault.Match{Kind: fault.KindSegment}, Action: fault.Action{ThrottleKbps: 1}})
	start := time.Now()
	r := get(t, srv, "/live/demo/720p/499.m4s")
	if took := time.Since(start); took < 180*time.Millisecond || took > 900*time.Millisecond || r.body != "seq=500 tfdt=89820000 samples=1 cues=0" {
		t.Errorf("%q after %v, want the whole segment after about 200ms", r.body, took)
	}
	r = get(t, srv, "/live/demo/720p/499.m4s", "Range", "bytes=4-6")
	if r.status != http.StatusPartialContent || r.body != "500" {
		t.Errorf("Range: %d %q, want 206 \"500\"", r.status, r.body)
	}
}

func TestFaults_StalePlaylist(t *testing.T) {
	srv := newServer(t, fault.Rule{ID: "old", Match: fault.Match{Kind: fault.KindPlaylist}, Action: fault.Action{Stale: 10 * time.Second}})
	// 10 s of 2 s segments: 485 becomes 480.
	if r := get(t, srv, "/live/demo/720p.m3u8"); !strings.Contains(r.body, "#EXT-X-MEDIA-SEQUENCE:480\n") {
		t.Errorf("stale playlist:\n%s", r.body)
	}
}

func TestFaults_Gap(t *testing.T) {
	rule := fault.Rule{ID: "gap", Match: fault.Match{Kind: fault.KindSegment, Renditions: []string{"720p"}, Segments: segRange(488, 497)},
		Percent: 50, Mode: fault.ModeSticky, Action: fault.Action{Gap: true}}
	srv := newServer(t, rule)
	want := map[string]bool{}
	for n := int64(485); n <= 499; n++ {
		if fault.Gap([]fault.Rule{rule}, fault.Target{StreamID: "demo", Epoch: 3, Kind: fault.KindSegment, Rendition: "720p", Segment: n}) {
			want["720p/"+strconv.FormatInt(n, 10)+".m4s"] = true
		}
	}
	if len(want) == 0 || len(want) == 10 {
		t.Fatalf("%d of 10 segments gapped, want some", len(want))
	}
	got := map[string]bool{}
	gap := false
	for line := range strings.Lines(get(t, srv, "/live/demo/720p.m3u8").body) {
		line = strings.TrimSpace(line)
		switch {
		case line == "#EXT-X-GAP":
			gap = true
		case !strings.HasPrefix(line, "#"):
			if gap {
				got[line] = true
			}
			wantStatus := http.StatusOK
			if want[line] {
				wantStatus = http.StatusNotFound
			}
			if r := get(t, srv, "/live/demo/"+line); r.status != wantStatus || (want[line] && r.body != "") {
				t.Errorf("GET %s: %d %q, want %d", line, r.status, r.body, wantStatus)
			}
			gap = false
		}
	}
	if len(got) != len(want) {
		t.Errorf("EXT-X-GAP on %v, want %v", got, want)
	}
	for uri := range want {
		if !got[uri] {
			t.Errorf("no EXT-X-GAP on %s", uri)
		}
	}
	if strings.Contains(get(t, srv, "/live/demo/360p.m3u8").body, "#EXT-X-GAP") {
		t.Error("360p has gaps")
	}
}

func TestFaults_ClockSkew(t *testing.T) {
	srv := newServer(t, fault.Rule{ID: "skew", Match: fault.Match{Kind: fault.KindMaster}, Action: fault.Action{ClockSkew: time.Hour}})
	r := get(t, srv, "/live/demo/master.m3u8")
	if want := now.Add(time.Hour).Format(http.TimeFormat); r.status != http.StatusOK || r.header.Get("Date") != want {
		t.Errorf("%d Date %q, want 200 %q", r.status, r.header.Get("Date"), want)
	}
}

func TestFaults_URL(t *testing.T) {
	srv := newServer(t)
	for _, tt := range []struct {
		path   string
		status int
	}{
		{"/live/demo/720p.m3u8?faultOn=playlist&fault=503", http.StatusServiceUnavailable},
		{"/live/demo/720p/499.m4s?faultOn=playlist&fault=503", http.StatusOK},
		{"/live/demo/720p/499.m4s?fault=503", http.StatusServiceUnavailable}, // default faultOn=segment
		{"/live/demo/720p/499.m4s?fault=503&faultRenditions=360p", http.StatusOK},
		{"/live/demo/master.m3u8?faultOn=master,segment&faultRenditions=720p&fault=504", http.StatusGatewayTimeout},
		{"/live/demo/manifest.mpd?faultOn=mpd&stale=10", http.StatusOK},
		{"/live/demo/720p/499.m4s?fault=abc", http.StatusBadRequest},
		{"/live/demo/720p/499.m4s?fault=418", http.StatusBadRequest},
		{"/live/demo/720p/499.m4s?faultOn=time&fault=503", http.StatusBadRequest},
		{"/live/demo/720p/499.m4s?stale=10", http.StatusBadRequest}, // stale on segments
		{"/live/demo/720p/499.m4s?fault=503&faultPercent=101", http.StatusBadRequest},
		{"/live/demo/720p/499.m4s?fault=503&faultMode=often", http.StatusBadRequest},
	} {
		if r := get(t, srv, tt.path); r.status != tt.status {
			t.Errorf("GET %s: %d %q, want %d", tt.path, r.status, r.body, tt.status)
		}
	}
}

func TestFaults_StickyAcrossHandlers(t *testing.T) {
	rule := fault.Rule{ID: "flaky", Match: fault.Match{Kind: fault.KindSegment}, Percent: 30, Action: fault.Action{Status: 503}}
	a, b := newServer(t, rule), newServer(t, rule)
	hits := 0
	for n := 485; n <= 499; n++ {
		path := "/live/demo/720p/" + strconv.Itoa(n) + ".m4s"
		sa, sb := get(t, a, path).status, get(t, b, path).status
		if sa != sb {
			t.Errorf("%s: %d and %d", path, sa, sb)
		}
		if sa == http.StatusServiceUnavailable {
			hits++
		}
		if get(t, a, path).status != sa {
			t.Errorf("%s: a retry differs", path)
		}
	}
	if hits == 0 || hits == 15 {
		t.Errorf("%d of 15 segments hit at 30%%", hits)
	}
}

type nopRequests struct{}

func (nopRequests) HTTPRequest(string, string, int, time.Duration) {}

func TestFaults_AccessLog(t *testing.T) {
	var buf bytes.Buffer
	h := accesslog.New(slog.New(slog.NewJSONHandler(&buf, nil)), nopRequests{},
		newHandler(t, fault.Rule{ID: "s503", Match: fault.Match{Kind: fault.KindSegment}, Action: fault.Action{Status: 503, Delay: time.Millisecond}}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/live/demo/720p/499.m4s", http.NoBody))
	if want := `"fault":{"rules":["s503"],"actions":["status","delayMs"]}`; w.Code != http.StatusServiceUnavailable || !strings.Contains(buf.String(), want) {
		t.Errorf("%d, log %s, want 503 and %s", w.Code, buf.String(), want)
	}
}
