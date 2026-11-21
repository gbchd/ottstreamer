package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakmem"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/apikey"
	"ottstreamer/internal/event"
	"ottstreamer/internal/event/eventmem"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/fault/faultmem"
	"ottstreamer/internal/live"
	"ottstreamer/internal/live/liveffmpeg"
	"ottstreamer/internal/live/livemp4"
	"ottstreamer/internal/metrics"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streammem"
	"ottstreamer/internal/ulid"
)

// clock is a wall clock the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type e2e struct {
	t      *testing.T
	srv    *httptest.Server
	clock  *clock
	faults *fault.Service
}

// newE2E builds the origin's graph with real adapters on a test clock.
func newE2E(t *testing.T, t0 time.Time) *e2e {
	t.Helper()
	if testing.Short() {
		t.Skip("runs ffmpeg")
	}
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	c := &clock{t: t0}
	logger := slog.New(slog.DiscardHandler)
	met := metrics.New()
	tmp := t.TempDir()
	enc, err := liveffmpeg.NewEncoder("ffmpeg", tmp, 4, met, logger)
	if err != nil {
		t.Fatal(err)
	}
	repo := streammem.NewRepository()
	var ids ulid.Generator
	events := event.NewService(eventmem.NewRepository(eventsPerStream), c.now, ids.New, countEvents(met), logger)
	t.Cleanup(events.Close)
	breaks := adbreak.NewService(repo, adbreakmem.NewRepository(), adbreakscte35.NewEncoder(), events, c.now)
	faults := fault.NewService(faultmem.NewRepository(), events, met, c.now, func() int { return 0 })
	mgr := live.NewManager(enc, livemp4.NewWriter(), breaks, events, 64<<20, 2, met, c.now, logger)
	t.Cleanup(mgr.Close)
	streams := stream.NewService(repo, repo, mgr, breaks, faults, events, c.now)
	met.WatchCache(mgr)
	auth, err := apikey.New([]string{"e2e:write:" + e2eKey})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(handler(deps{
		auth: auth, streams: streams, breaks: breaks, events: events, faults: faults, mgr: mgr, repo: repo,
		ready: readiness(t.Context(), repo, enc, mgr, tmp, ""), metrics: met, now: c.now, logger: logger,
	}))
	t.Cleanup(srv.Close)
	return &e2e{t: t, srv: srv, clock: c, faults: faults}
}

func (e *e2e) do(method, path, body string, want int) []byte {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e2eKey)
	res, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	if res.StatusCode != want {
		e.t.Fatalf("%s %s: %d, want %d: %s", method, path, res.StatusCode, want, b)
	}
	return b
}

// live fetches a /live path at the test clock's time, passed as ?now=.
func (e *e2e) live(path string) []byte {
	e.t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return e.do(http.MethodGet, "/live/e2e/"+path+sep+"now="+strconv.FormatInt(e.clock.now().UnixMilli(), 10), "", http.StatusOK)
}

// file writes init + segment n of a rendition to a file and returns its path.
func (e *e2e) file(rendition string, n int64) string {
	e.t.Helper()
	b := append(e.live(rendition+"/init.mp4"), e.live(rendition+"/"+strconv.FormatInt(n, 10)+".m4s")...)
	path := filepath.Join(e.t.TempDir(), rendition+".mp4")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// probe decodes every frame of the file and returns "<codec> <frames>".
func probe(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-count_frames",
		"-show_entries", "stream=codec_name,nb_read_frames", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

// background returns Y and Cr of the first frame at (x, y), away from every
// overlay of a 320x180 picture.
func background(t *testing.T, path string, x, y int) (luma, cr byte) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-i", path, "-frames:v", "1",
		"-vf", "crop=2:2:"+strconv.Itoa(x)+":"+strconv.Itoa(y)+",format=yuv444p", "-f", "rawvideo", "-").Output()
	if err != nil || len(out) != 12 {
		t.Fatalf("decode %s: %v (%d bytes)", path, err, len(out))
	}
	return out[0], out[8]
}

func near(v byte, want int) bool {
	return int(v) >= want-6 && int(v) <= want+6
}

const e2eKey = "e2e-secret"

const e2eStream = `{"id": "e2e", "name": "End to end", "spec": {
  "formats": ["hls", "dash"],
  "segmentSeconds": 1,
  "video": [
    {"id": "180p", "width": 320, "height": 180, "bitrateKbps": 300},
    {"id": "144p", "width": 256, "height": 144, "bitrateKbps": 150},
    {"id": "144p-hevc", "codec": "hevc", "width": 256, "height": 144, "bitrateKbps": 150},
    {"id": "144p-av1", "codec": "av1", "width": 256, "height": 144, "bitrateKbps": 150}
  ],
  "audio": [
    {"id": "en", "lang": "en", "name": "English", "channels": 2, "bitrateKbps": 96, "default": true},
    {"id": "en-51", "lang": "en", "name": "English 5.1", "channels": 6, "bitrateKbps": 192, "codec": "ac3"}
  ],
  "ads": {"leadSeconds": 2, "hls": ["daterange", "cueout"], "dash": ["eventstream-bin", "inband"]},
  "window": {"dvrSeconds": 10, "retainSeconds": 60}
}}`

func TestEndToEnd(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	e := newE2E(t, t0)
	e.do(http.MethodPost, "/api/streams", e2eStream, http.StatusCreated)
	e.do(http.MethodPost, "/api/streams/e2e/start", "", http.StatusOK) // AST = t0
	// The test clock jumps ahead of the producer from here on, so check
	// readiness while nothing is published yet.
	if ready := string(e.do(http.MethodGet, "/readyz", "", http.StatusOK)); ready != "{\"status\":\"ready\"}\n" {
		t.Errorf("readyz: %s", ready)
	}

	// Live edge 4, frozen horizon 6: a 3 s break 5 s from now starts at 10.
	e.clock.set(t0.Add(5 * time.Second))
	brk := e.do(http.MethodPost, "/api/streams/e2e/breaks", `{"duration": 3, "in": "5s"}`, http.StatusCreated)
	if !bytes.Contains(brk, []byte(`"startSegment":10`)) || !bytes.Contains(brk, []byte(`"endSegment":13`)) {
		t.Fatalf("break: %s", brk)
	}

	// Live edge 15: the whole break is in the 10 s window.
	e.clock.set(t0.Add(16 * time.Second))
	master := string(e.live("master.m3u8"))
	av1, hevc, avc := strings.Index(master, `CODECS="av01.`), strings.Index(master, `CODECS="hvc1.`), strings.Index(master, `CODECS="avc1.`)
	if av1 < 0 || hevc < av1 || avc < hevc || !strings.Contains(master, `,mp4a.40.2,ac-3"`) {
		t.Errorf("master:\n%s", master)
	}
	var cues string
	for _, r := range []string{"180p", "144p", "144p-hevc", "144p-av1", "en", "en-51"} {
		pl := string(e.live(r + ".m3u8"))
		for _, tag := range []string{`#EXT-X-DATERANGE:ID="1",START-DATE="2026-11-06T18:00:10.000Z",PLANNED-DURATION=3.000,SCTE35-OUT=0xFC30`,
			"#EXT-X-CUE-OUT:DURATION=3", "#EXT-X-CUE-OUT-CONT:ElapsedTime=1,Duration=3", `END-DATE="2026-11-06T18:00:13.000Z",SCTE35-IN=0xFC30`, "#EXT-X-CUE-IN"} {
			if !strings.Contains(pl, tag) {
				t.Errorf("%s.m3u8 lacks %s:\n%s", r, tag, pl)
			}
		}
		var tags []string
		for line := range strings.Lines(pl) {
			if strings.HasPrefix(line, "#EXT-X-DATERANGE") || strings.HasPrefix(line, "#EXT-X-CUE") {
				tags = append(tags, line)
			}
		}
		if got := strings.Join(tags, ""); cues == "" {
			cues = got
		} else if got != cues {
			t.Errorf("%s.m3u8 cue tags differ from 180p:\n%s", r, got)
		}
	}
	mpd := string(e.live("manifest.mpd"))
	for _, want := range []string{`<InbandEventStream schemeIdUri="urn:scte:scte35:2013:bin"`, `<Event id="1" presentationTime="900000" duration="270000">`, `/api/time?stream=e2e"`,
		`<SupplementalProperty schemeIdUri="urn:mpeg:dash:adaptation-set-switching:2016" value="2,3">`} {
		if !strings.Contains(mpd, want) {
			t.Errorf("MPD lacks %s:\n%s", want, mpd)
		}
	}

	// The cue-out rides in the lead segments 8 and 9, not in 7.
	scheme := []byte("urn:scte:scte35:2013:bin")
	for n, want := range map[int64]bool{7: false, 8: true, 9: true} {
		if got := bytes.Contains(e.live("180p/"+strconv.FormatInt(n, 10)+".m4s"), scheme); got != want {
			t.Errorf("segment %d carries emsg: %v, want %v", n, got, want)
		}
	}
	for _, r := range []string{"144p-hevc", "144p-av1"} {
		if !bytes.Contains(e.live(r+"/8.m4s"), scheme) {
			t.Errorf("%s segment 8 lacks the emsg", r)
		}
	}
	if bytes.Contains(e.live("en/9.m4s"), scheme) {
		t.Error("audio segment 9 carries emsg")
	}

	// Ad segments are red, the others grey.
	// A spot right of the clock and above the disc, 7/8 across and 1/6 down.
	spots := map[string][2]int{"180p": {280, 30}, "144p": {224, 24}, "144p-hevc": {224, 24}, "144p-av1": {224, 24}}
	codecOf := map[string]string{"180p": "h264", "144p": "h264", "144p-hevc": "hevc", "144p-av1": "av1"}
	for n, red := range map[int64]bool{9: false, 10: true, 12: true, 13: false} {
		want := [2]int{40, 128} // grey: Y, Cr
		if red {
			want = [2]int{63, 240}
		}
		for r, at := range spots {
			f := e.file(r, n)
			if c := probe(t, f); c != codecOf[r]+" 25" {
				t.Errorf("%s/%d: codec %q", r, n, c)
			}
			if y, cr := background(t, f, at[0], at[1]); !near(y, want[0]) || !near(cr, want[1]) {
				t.Errorf("%s/%d: background Y=%d Cr=%d, want %v", r, n, y, cr, want)
			}
		}
	}
	// Segment 11 holds AAC frames 516..562 and AC-3 frames 344..374.
	for r, codec := range map[string]string{"en": "aac 47", "en-51": "ac3 31"} {
		if c := probe(t, e.file(r, 11)); c != codec {
			t.Errorf("%s: codec %q, want %s", r, c, codec)
		}
	}

	e.checkFaults()

	// Stop: the end segment is the frozen horizon, 17.
	e.do(http.MethodPost, "/api/streams/e2e/stop", "", http.StatusOK)
	e.clock.set(t0.Add(30 * time.Second))
	for _, r := range []string{"180p", "en"} {
		pl := string(e.live(r + ".m3u8"))
		if !strings.HasSuffix(pl, r+"/17.m4s?now="+strconv.FormatInt(e.clock.now().UnixMilli(), 10)+"\n#EXT-X-ENDLIST\n") {
			t.Errorf("%s.m3u8 does not end at 17:\n%s", r, pl)
		}
	}
	if mpd := string(e.live("manifest.mpd")); !strings.Contains(mpd, `mediaPresentationDuration="PT18S"`) || strings.Contains(mpd, "minimumUpdatePeriod") {
		t.Errorf("ended MPD:\n%s", mpd)
	}
	e.live("180p/17.m4s")
	e.clock.set(t0.Add(61 * time.Second)) // the minute of the faults is over
	e.faults.Flush(t.Context())

	prom := string(e.do(http.MethodGet, "/metrics", "", http.StatusOK))
	for _, want := range []string{
		`segments_built_total{kind="video",stream="e2e"}`,
		`http_requests_total{kind="segment",route="GET /live/{stream}/{rendition}/{segment}",status="200"}`,
		`http_requests_total{kind="api",route="POST /api/streams/{id}/breaks",status="201"} 1`,
		`ffmpeg_running{stream="e2e"} `, // the producer may be encoding
		"cache_hits_total ",
	} {
		if !strings.Contains(prom, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	e.checkEvents()
}

// checkEvents checks the stream's log once the producer has crossed the
// break, the metrics that come from it, and what deleting the stream does.
func (e *e2e) checkEvents() {
	t := e.t
	want := []string{
		"stream.created api:e2e -", "stream.started api:e2e -", "break.scheduled api:e2e 10",
		"signal.announced system 8", "break.started system 10", "signal.cue_out system 10",
		"break.ended system 13", "signal.cue_in system 13", "stream.stopped api:e2e 17",
		"faults.updated api:e2e -", "fault.injected system -", "fault.injected system -", "fault.injected system -",
	}
	var got []string
	// The producer records crossings on real timers, behind the test clock.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		var page struct {
			Events []struct {
				Type, Actor string
				Segment     *int64
			}
		}
		if err := json.Unmarshal(e.do(http.MethodGet, "/api/streams/e2e/events", "", http.StatusOK), &page); err != nil {
			t.Fatal(err)
		}
		got = got[:0]
		for _, ev := range page.Events {
			if ev.Type == "segment.late" { // the test clock runs ahead of the producer
				continue
			}
			seg := "-"
			if ev.Segment != nil {
				seg = strconv.FormatInt(*ev.Segment, 10)
			}
			got = append(got, ev.Type+" "+ev.Actor+" "+seg)
		}
		slices.Sort(got)
		if slices.Equal(got, slices.Sorted(slices.Values(want))) {
			break
		}
	}
	if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}

	prom := string(e.do(http.MethodGet, "/metrics", "", http.StatusOK))
	for _, want := range []string{
		`ad_breaks_total{source="manual",stream="e2e",type="started"} 1`,
		`ad_breaks_total{source="manual",stream="e2e",type="ended"} 1`,
		`scte35_signals_total{source="manual",stream="e2e",type="announced"} 1`,
		`scte35_signals_total{source="manual",stream="e2e",type="cue_in"} 1`,
		`faults_injected_total{action="gap",rule="drop-14",stream="e2e"} 1`,
		`faults_injected_total{action="status",rule="url-segment",stream="e2e"} 1`,
		`faults_injected_total{action="clockSkewSeconds",rule="skew",stream="e2e"} 1`,
	} {
		if !strings.Contains(prom, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// Deleting the stream drops its series; its log stays readable.
	e.do(http.MethodDelete, "/api/streams/e2e", "", http.StatusNoContent)
	if prom := string(e.do(http.MethodGet, "/metrics", "", http.StatusOK)); strings.Contains(prom, `stream="e2e"`) {
		t.Error("metrics keep series of the deleted stream")
	}
	if log := e.do(http.MethodGet, "/api/streams/e2e/events?types=stream.deleted", "", http.StatusOK); !bytes.Contains(log, []byte(`"actor":"api:e2e"`)) {
		t.Errorf("no stream.deleted after delete: %s", log)
	}
}

// checkFaults sets live rules at live edge 15: a gap on 180p segment 14 and
// a minute of clock skew; then a URL fault hits one request only.
func (e *e2e) checkFaults() {
	t := e.t
	e.do(http.MethodPut, "/api/streams/e2e/faults", `[
  {"id": "drop-14", "match": {"kind": "segment", "renditions": ["180p"], "segments": {"from": 14, "to": 14}}, "action": {"gap": true}},
  {"id": "skew", "match": {"kind": "time"}, "action": {"clockSkewSeconds": 60}}
]`, http.StatusOK)
	now := "now=" + strconv.FormatInt(e.clock.now().UnixMilli(), 10)
	if pl := string(e.live("180p.m3u8")); !strings.Contains(pl, "#EXT-X-GAP\n180p/14.m4s?"+now) || strings.Count(pl, "#EXT-X-GAP") != 1 {
		t.Errorf("180p.m3u8 does not mark only segment 14 as a gap:\n%s", pl)
	}
	if pl := string(e.live("144p.m3u8")); strings.Contains(pl, "#EXT-X-GAP") {
		t.Errorf("144p.m3u8 has a gap:\n%s", pl)
	}
	e.do(http.MethodGet, "/live/e2e/180p/14.m4s?"+now, "", http.StatusNotFound)
	e.live("144p/14.m4s")
	e.do(http.MethodGet, "/live/e2e/144p/13.m4s?fault=503&"+now, "", http.StatusServiceUnavailable)
	e.live("144p/13.m4s")
	if got, want := string(e.do(http.MethodGet, "/api/time?stream=e2e", "", http.StatusOK)), "2026-11-06T18:01:16.000Z"; got != want {
		t.Errorf("/api/time?stream=e2e = %s, want %s", got, want)
	}
}
