package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakmem"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/api"
	"ottstreamer/internal/cors"
	"ottstreamer/internal/live"
	"ottstreamer/internal/live/liveffmpeg"
	"ottstreamer/internal/live/livehttp"
	"ottstreamer/internal/live/livemp4"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streammem"
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
	t     *testing.T
	srv   *httptest.Server
	clock *clock
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
	enc, err := liveffmpeg.NewEncoder("ffmpeg", t.TempDir(), 4, logger)
	if err != nil {
		t.Fatal(err)
	}
	repo := streammem.NewRepository()
	breaks := adbreak.NewService(repo, adbreakmem.NewRepository(), adbreakscte35.NewEncoder(), c.now)
	mgr := live.NewManager(enc, livemp4.NewWriter(), breaks, 64<<20, 2, c.now, logger)
	t.Cleanup(mgr.Close)
	streams := stream.NewService(repo, repo, mgr, breaks, c.now)

	mux := http.NewServeMux()
	api.Register(mux, streams, breaks, c.now, logger)
	livehttp.NewHandler(mgr, repo, breaks, c.now, logger).Register(mux)
	srv := httptest.NewServer(cors.New(nil, mux))
	t.Cleanup(srv.Close)
	return &e2e{t: t, srv: srv, clock: c}
}

func (e *e2e) do(method, path, body string, want int) []byte {
	e.t.Helper()
	req, err := http.NewRequestWithContext(e.t.Context(), method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
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

const e2eStream = `{"id": "e2e", "name": "End to end", "spec": {
  "formats": ["hls", "dash"],
  "segmentSeconds": 1,
  "video": [
    {"id": "180p", "width": 320, "height": 180, "bitrateKbps": 300},
    {"id": "144p", "width": 256, "height": 144, "bitrateKbps": 150}
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

	// Live edge 4, frozen horizon 6: a 3 s break 5 s from now starts at 10.
	e.clock.set(t0.Add(5 * time.Second))
	brk := e.do(http.MethodPost, "/api/streams/e2e/breaks", `{"duration": 3, "in": "5s"}`, http.StatusCreated)
	if !bytes.Contains(brk, []byte(`"startSegment":10`)) || !bytes.Contains(brk, []byte(`"endSegment":13`)) {
		t.Fatalf("break: %s", brk)
	}

	// Live edge 15: the whole break is in the 10 s window.
	e.clock.set(t0.Add(16 * time.Second))
	master := string(e.live("master.m3u8"))
	if !strings.Contains(master, `CODECS="avc1.`) || !strings.Contains(master, `,mp4a.40.2,ac-3"`) {
		t.Errorf("master:\n%s", master)
	}
	var cues string
	for _, r := range []string{"180p", "144p", "en", "en-51"} {
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
	for _, want := range []string{`<InbandEventStream schemeIdUri="urn:scte:scte35:2013:bin"`, `<Event id="1" presentationTime="900000" duration="270000">`} {
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
	if bytes.Contains(e.live("en/9.m4s"), scheme) {
		t.Error("audio segment 9 carries emsg")
	}

	// Ad segments are red, the others grey.
	// A spot right of the clock and above the disc, 7/8 across and 1/6 down.
	spots := map[string][2]int{"180p": {280, 30}, "144p": {224, 24}}
	for n, red := range map[int64]bool{9: false, 10: true, 12: true, 13: false} {
		want := [2]int{40, 128} // grey: Y, Cr
		if red {
			want = [2]int{63, 240}
		}
		for r, at := range spots {
			f := e.file(r, n)
			if c := probe(t, f); c != "h264 25" {
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
}
