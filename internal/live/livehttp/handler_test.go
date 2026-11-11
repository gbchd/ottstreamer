package livehttp_test

import (
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/event"
	"ottstreamer/internal/event/eventtest"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/fault/faultmem"
	"ottstreamer/internal/live"
	"ottstreamer/internal/live/livehttp"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streamtest"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

var (
	ast = time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	// now puts the live edge at segment 499 (2 s segments): the 30 s window
	// lists 485..499 and the frozen horizon is 503.
	now   = ast.Add(1000*time.Second + 500*time.Millisecond)
	nowMS = strconv.FormatInt(now.UnixMilli(), 10)
)

// fakeEncoder builds tracks with the codec strings liveffmpeg reports.
type fakeEncoder struct{}

var codecs = map[string]string{"720p": "avc1.64001F", "360p": "avc1.64001E", "en": "mp4a.40.2", "en-51": "ac-3"}

func (fakeEncoder) StartEpoch(_ context.Context, s stream.Stream) (map[string]live.Track, error) {
	tracks := map[string]live.Track{}
	for _, v := range s.Status.Epoch.Spec.Video {
		tracks[v.ID] = live.Track{ID: v.ID, Video: true, Timescale: 90000, Codec: codecs[v.ID], Init: []byte("init " + v.ID)}
	}
	for _, a := range s.Status.Epoch.Spec.Audio {
		fs := a.Codec.FrameSize()
		tracks[a.ID] = live.Track{ID: a.ID, Timescale: 48000, Codec: codecs[a.ID], Init: []byte("init " + a.ID),
			FrameSize: fs, Loop: make([]live.Sample, 8*48000/fs)}
	}
	return tracks, nil
}

func (fakeEncoder) EncodeSegment(_ context.Context, s stream.Stream, _ int64, _ bool) (map[string][]live.Sample, error) {
	out := map[string][]live.Sample{}
	for _, v := range s.Status.Epoch.Spec.Video {
		out[v.ID] = []live.Sample{{Data: []byte{0}, Sync: true}}
	}
	return out, nil
}

type nopMetrics struct{}

func (nopMetrics) SegmentBuilt(string, string, time.Duration) {}
func (nopMetrics) SegmentLate(string)                         {}
func (nopMetrics) FaultInjected(string, string, string)       {}

// segmentWrites counts the media segments written for a request.
var segmentWrites atomic.Int64

// fakeWriter writes a readable summary of the fragment.
type fakeWriter struct{}

func (fakeWriter) WriteSegment(w io.Writer, f live.Fragment) error {
	segmentWrites.Add(1)
	_, err := fmt.Fprintf(w, "seq=%d tfdt=%d samples=%d", f.Seq, f.BaseTime, len(f.Samples))
	return err
}

type fakeStreams map[string]stream.Stream

func (f fakeStreams) Get(_ context.Context, id string) (stream.Stream, error) {
	s, ok := f[id]
	if !ok {
		return stream.Stream{}, stream.ErrNotFound
	}
	return s, nil
}

// fakeBreaks holds breaks per stream, cues and times filled.
type fakeBreaks map[string][]adbreak.Break

func (f fakeBreaks) InRange(_ context.Context, id string, _ int, from, to int64) ([]adbreak.Break, error) {
	var out []adbreak.Break
	for _, b := range f[id] {
		if b.StartSeg < to && from < b.EndSeg {
			out = append(out, b)
		}
	}
	return out, nil
}

func (fakeBreaks) Signals(context.Context, string, int, int64) ([]adbreak.Signal, error) {
	return nil, nil
}

func (fakeBreaks) Crossed(context.Context, string, int, int64) ([]event.Event, error) {
	return nil, nil
}

func spec() stream.Spec {
	s := streamtest.Spec()
	s.Audio = append(s.Audio, stream.Audio{ID: "en-51", Lang: "en", Name: "English 5.1", Channels: 6, BitrateKbps: 384, Codec: stream.CodecAC3})
	s.Ads.HLS = []string{"daterange", "cueout"}
	s.Ads.DASH = []string{"eventstream-bin", "inband"}
	s.Normalize()
	return s
}

func running(id string, sp stream.Spec, end *int64) stream.Stream {
	s := streamtest.Stream(id)
	s.Spec = sp
	s.Status = stream.Status{State: stream.StateRunning, Epoch: &stream.Epoch{Number: 3, AST: ast, EndSegment: end, Spec: sp.Clone()}}
	if end != nil {
		s.Status.State = stream.StateEnded
	}
	return s
}

// newBreak returns break id over [start, end), planned to end at planned,
// cued like adbreak.Service does.
func newBreak(t *testing.T, sp stream.Spec, id string, start, end, planned int64) adbreak.Break {
	t.Helper()
	tl := (&stream.Epoch{AST: ast, Spec: sp}).Timeline()
	ev, _ := strconv.Atoi(id)
	b := adbreak.Break{ID: id, EventID: uint32(ev), StartSeg: start, EndSeg: end, PlannedEndSeg: planned,
		Start: tl.SegStart(start), End: tl.SegStart(end)}
	enc := adbreakscte35.NewEncoder()
	var err error
	cue := adbreak.Cue{Command: sp.Ads.Command, SegmentationType: sp.Ads.SegmentationType, EventID: b.EventID}
	out, in := cue, cue
	out.Out, out.PTS, out.DurationTicks = true, tl.VideoTfdt(start), (planned-start)*tl.SegTicks()
	in.PTS = tl.VideoTfdt(end)
	if b.CueOut, err = enc.Encode(out); err != nil {
		t.Fatal(err)
	}
	if b.CueIn, err = enc.Encode(in); err != nil {
		t.Fatal(err)
	}
	return b
}

// newServer serves newHandler.
func newServer(t *testing.T, rules ...fault.Rule) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newHandler(t, rules...))
	t.Cleanup(srv.Close)
	return srv
}

// newHandler returns the /live routes, with rules active on demo.
func newHandler(t *testing.T, rules ...fault.Rule) http.Handler {
	t.Helper()
	sp := spec()
	ts := spec()
	ts.Ads.Command, ts.Ads.SegmentationType = stream.CommandTimeSignal, "provider_ad"
	hlsOnly := spec()
	hlsOnly.Formats = []stream.Format{stream.FormatHLS}
	end := int64(497)

	streams := fakeStreams{
		"demo":     running("demo", sp, nil),
		"ts":       running("ts", ts, nil),
		"hls-only": running("hls-only", hlsOnly, nil),
		"ended":    running("ended", sp, &end),
		"stopped":  running("stopped", sp, nil),
	}
	cancelled := newBreak(t, sp, "9", 496, 498, 498)
	cancelled.Cancelled = true
	breaks := fakeBreaks{
		// 6 started before the window, 7 is inside it, 8 is frozen but not
		// published yet, 9 is cancelled.
		"demo":  {newBreak(t, sp, "6", 480, 487, 487), newBreak(t, sp, "7", 490, 495, 495), cancelled, newBreak(t, sp, "8", 502, 505, 505)},
		"ts":    {newBreak(t, ts, "7", 490, 495, 495)},
		"ended": {newBreak(t, sp, "7", 495, end+1, 505)}, // cut by the stop
	}
	clock := func() time.Time { return now }
	logger := slog.New(slog.DiscardHandler)
	mgr := live.NewManager(fakeEncoder{}, fakeWriter{}, breaks, &eventtest.Recorder{}, 1<<20, 1, nopMetrics{}, clock, logger)
	t.Cleanup(mgr.Close)
	for _, s := range streams {
		if err := mgr.Launch(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	stopped := streams["stopped"]
	stopped.Status.State = stream.StateStopped // its runtime is still there
	streams["stopped"] = stopped

	faults := fault.NewService(faultmem.NewRepository(), &eventtest.Recorder{}, nopMetrics{}, clock, func() int { return 0 })
	if _, err := faults.Replace(t.Context(), "demo", rules); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	livehttp.NewHandler(mgr, streams, breaks, faults, clock, logger).Register(mux)
	return mux
}

type response struct {
	status int
	header http.Header
	body   string
}

func get(t *testing.T, srv *httptest.Server, path string, header ...string) response {
	t.Helper()
	r, err := fetch(t, srv, path, header...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// fetch is get that returns the transport error and what was read before it.
func fetch(t *testing.T, srv *httptest.Server, path string, header ...string) (response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "origin.test"
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := srv.Client().Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return response{status: res.StatusCode, header: res.Header, body: string(b)}, err
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (run with -update to accept):\n%s", name, path, got)
	}
}

func TestManifests(t *testing.T) {
	srv := newServer(t)
	tests := []struct {
		name, path, contentType string
	}{
		{"hls_720p", "/live/demo/720p.m3u8", "application/vnd.apple.mpegurl"},
		{"hls_720p_daterange", "/live/demo/720p.m3u8?cues=daterange", ""},
		{"hls_720p_cueout", "/live/demo/720p.m3u8?cues=cueout", ""},
		{"hls_720p_oatcls", "/live/demo/720p.m3u8?cues=oatcls", ""},
		{"hls_720p_adobe", "/live/demo/720p.m3u8?cues=adobe", ""},
		{"hls_720p_time_signal", "/live/ts/720p.m3u8", ""},
		{"hls_en_aac", "/live/demo/en.m3u8", ""},
		{"hls_en51_ac3", "/live/demo/en-51.m3u8", ""},
		{"hls_master", "/live/demo/master.m3u8", "application/vnd.apple.mpegurl"},
		{"hls_ended", "/live/ended/720p.m3u8", ""},
		{"mpd_timeline_time", "/live/demo/manifest.mpd", "application/dash+xml"},
		{"mpd_timeline_number", "/live/demo/manifest.mpd?addressing=timeline-number", ""},
		{"mpd_number", "/live/demo/manifest.mpd?addressing=number", ""},
		{"mpd_eventstream_xml", "/live/demo/manifest.mpd?dashcues=eventstream-xml", ""},
		{"mpd_multiperiod", "/live/demo/manifest.mpd?periods=true", ""},
		{"mpd_ended", "/live/ended/manifest.mpd", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := get(t, srv, tt.path)
			if r.status != http.StatusOK {
				t.Fatalf("status %d: %s", r.status, r.body)
			}
			if tt.contentType != "" && r.header.Get("Content-Type") != tt.contentType {
				t.Errorf("Content-Type %q, want %q", r.header.Get("Content-Type"), tt.contentType)
			}
			if cc := r.header.Get("Cache-Control"); cc != "max-age=1" {
				t.Errorf("Cache-Control %q, want max-age=1 (S/2)", cc)
			}
			golden(t, tt.name, r.body)
		})
	}
}

// cueTags returns the ad tags of a playlist.
func cueTags(playlist string) string {
	var tags []string
	for line := range strings.Lines(playlist) {
		if strings.HasPrefix(line, "#EXT-X-DATERANGE") || strings.HasPrefix(line, "#EXT-X-CUE") ||
			strings.HasPrefix(line, "#EXT-OATCLS") || strings.HasPrefix(line, "#EXT-X-SCTE35") {
			tags = append(tags, line)
		}
	}
	return strings.Join(tags, "")
}

func TestMediaPlaylist_SameCuesInEveryRendition(t *testing.T) {
	srv := newServer(t)
	want := cueTags(get(t, srv, "/live/demo/720p.m3u8").body)
	if want == "" {
		t.Fatal("no cue tags")
	}
	for _, r := range []string{"360p", "en", "en-51"} {
		if got := cueTags(get(t, srv, "/live/demo/"+r+".m3u8").body); got != want {
			t.Errorf("%s cue tags differ from 720p:\n%s", r, got)
		}
	}
}

func TestNotFound(t *testing.T) {
	srv := newServer(t)
	for _, path := range []string{
		"/live/nope/master.m3u8",
		"/live/stopped/master.m3u8",
		"/live/stopped/720p.m3u8",
		"/live/stopped/manifest.mpd",
		"/live/stopped/720p/init.mp4",
		"/live/stopped/720p/499.m4s",
		"/live/hls-only/manifest.mpd",
		"/live/demo/1080p.m3u8",
		"/live/demo/1080p/init.mp4",
		"/live/demo/1080p/499.m4s",
		"/live/demo/720p/500.m4s",         // not published yet
		"/live/demo/720p/349.m4s",         // past retention (300 s)
		"/live/demo/720p/abc.m4s",         // not a number
		"/live/demo/720p/499.mp4",         // not a segment
		"/live/ended/720p/498.m4s",        // after the end segment
		"/live/demo/720p/t/89820001.m4s",  // one tick after segment 499
		"/live/demo/en/t/47904769.m4s",    // one tick after AAC segment 499
		"/live/demo/en-51/t/47904769.m4s", // one tick after AC-3 segment 499
		"/live/demo/720p/t/89820000.m4s?now=" + strconv.FormatInt(ast.Add(999*time.Second).UnixMilli(), 10), // ?now= before it is published
	} {
		if r := get(t, srv, path); r.status != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, r.status)
		}
	}
}

func TestDVR(t *testing.T) {
	srv := newServer(t)
	for _, tt := range []struct {
		dvr    string
		status int
		segs   int
	}{
		{"5.9", http.StatusBadRequest, 0},
		{"6", http.StatusOK, 3},
		{"60", http.StatusOK, 30},
		{"300", http.StatusOK, 150},
		{"301", http.StatusBadRequest, 0},
		{"x", http.StatusBadRequest, 0},
	} {
		for _, path := range []string{"/live/demo/720p.m3u8", "/live/demo/manifest.mpd"} {
			r := get(t, srv, path+"?dvr="+tt.dvr)
			if r.status != tt.status {
				t.Errorf("%s?dvr=%s: %d, want %d", path, tt.dvr, r.status, tt.status)
				continue
			}
			if tt.status == http.StatusOK && strings.HasSuffix(path, ".m3u8") {
				if n := strings.Count(r.body, "#EXTINF"); n != tt.segs {
					t.Errorf("dvr=%s: %d segments, want %d", tt.dvr, n, tt.segs)
				}
			}
			if tt.status == http.StatusOK && strings.HasSuffix(path, ".mpd") && !strings.Contains(r.body, `timeShiftBufferDepth="PT`+tt.dvr+`S"`) {
				t.Errorf("dvr=%s: MPD lacks timeShiftBufferDepth", tt.dvr)
			}
		}
	}
}

func TestBadOverrides(t *testing.T) {
	srv := newServer(t)
	for _, path := range []string{
		"/live/demo/720p.m3u8?cues=nope",
		"/live/demo/720p.m3u8?cues=cueout,oatcls",
		"/live/demo/720p.m3u8?now=soon",
		"/live/demo/manifest.mpd?addressing=list",
		"/live/demo/manifest.mpd?dashcues=inband",
		"/live/demo/manifest.mpd?periods=maybe",
	} {
		if r := get(t, srv, path); r.status != http.StatusBadRequest {
			t.Errorf("GET %s: %d, want 400", path, r.status)
		}
	}
}

func TestQueryPropagation(t *testing.T) {
	srv := newServer(t)
	for _, q := range []string{"now=" + nowMS + "&cues=adobe", "faultOn=init,segment&faultRenditions=720p&faultPercent=10&delay=1"} {
		queryPropagates(t, srv, q)
	}
}

// queryPropagates checks that every URL the manifests write carries q.
func queryPropagates(t *testing.T, srv *httptest.Server, q string) {
	t.Helper()
	for _, path := range []string{"/live/demo/master.m3u8", "/live/demo/720p.m3u8", "/live/demo/en.m3u8"} {
		r := get(t, srv, path+"?"+q)
		for line := range strings.Lines(r.body) {
			line = strings.TrimSpace(line)
			uri := line
			if _, after, ok := strings.Cut(line, `URI="`); ok {
				uri = strings.TrimSuffix(after, `"`)
			} else if strings.HasPrefix(line, "#") || line == "" {
				continue
			}
			if !strings.HasSuffix(uri, "?"+q) {
				t.Errorf("%s: URL %q lacks the query", path, uri)
			}
		}
	}
	r := get(t, srv, "/live/demo/manifest.mpd?"+q)
	esc := strings.ReplaceAll(q, "&", "&amp;")
	if n := strings.Count(r.body, ".m4s?"+esc+`"`) + strings.Count(r.body, "init.mp4?"+esc+`"`); n != 2*3 {
		t.Errorf("MPD: %d templates carry the query, want 6:\n%s", n, r.body)
	}
}

func TestSegments(t *testing.T) {
	srv := newServer(t)
	tests := []struct {
		path, contentType, body string
	}{
		{"/live/demo/720p/init.mp4", "video/mp4", "init 720p"},
		{"/live/demo/en-51/init.mp4", "audio/mp4", "init en-51"},
		{"/live/demo/720p/499.m4s", "video/mp4", "seq=500 tfdt=89820000 samples=1"},
		{"/live/demo/720p/t/89820000.m4s", "video/mp4", "seq=500 tfdt=89820000 samples=1"},
		{"/live/demo/720p/350.m4s", "video/mp4", "seq=351 tfdt=63000000 samples=1"},
		{"/live/demo/en/t/47904768.m4s", "audio/mp4", "seq=500 tfdt=47904768 samples=93"},
		{"/live/demo/en-51/t/47904768.m4s", "audio/mp4", "seq=500 tfdt=47904768 samples=62"},
		{"/live/ended/720p/497.m4s", "video/mp4", "seq=498 tfdt=89460000 samples=1"},
	}
	for _, tt := range tests {
		r := get(t, srv, tt.path)
		if r.status != http.StatusOK || r.body != tt.body || r.header.Get("Content-Type") != tt.contentType {
			t.Errorf("GET %s: %d %q %q, want 200 %q %q", tt.path, r.status, r.header.Get("Content-Type"), r.body, tt.contentType, tt.body)
		}
		if cc := r.header.Get("Cache-Control"); cc != "max-age=300" {
			t.Errorf("GET %s: Cache-Control %q, want max-age=300", tt.path, cc)
		}
	}
	r := get(t, srv, "/live/demo/720p/499.m4s", "Range", "bytes=4-6")
	if r.status != http.StatusPartialContent || r.body != "500" || r.header.Get("Content-Range") != "bytes 4-6/31" {
		t.Errorf("Range: %d %q %q", r.status, r.body, r.header.Get("Content-Range"))
	}
}

type mpdDoc struct {
	Periods []struct {
		AdaptationSets []struct {
			SegmentTemplate struct {
				Timescale int64  `xml:"timescale,attr"`
				Media     string `xml:"media,attr"`
				S         []struct {
					T *int64 `xml:"t,attr"`
					D int64  `xml:"d,attr"`
					R int64  `xml:"r,attr"`
				} `xml:"SegmentTimeline>S"`
			}
			Representations []struct {
				ID string `xml:"id,attr"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

// TestMPD_TimelineMatchesSegments checks every <S> against the timeline's
// tfdt and durations, and fetches every segment it describes.
func TestMPD_TimelineMatchesSegments(t *testing.T) {
	srv := newServer(t)
	var doc mpdDoc
	if err := xml.Unmarshal([]byte(get(t, srv, "/live/demo/manifest.mpd").body), &doc); err != nil {
		t.Fatal(err)
	}
	sp := spec()
	tl := (&stream.Epoch{AST: ast, Spec: sp}).Timeline()
	frameSize := map[string]int{"720p": 0, "360p": 0, "en": 1024, "en-51": 1536}
	sets := doc.Periods[0].AdaptationSets
	if len(sets) != 3 {
		t.Fatalf("%d AdaptationSets, want 3", len(sets))
	}
	for _, as := range sets {
		var got [][2]int64
		next := int64(-1)
		for _, s := range as.SegmentTemplate.S {
			if s.T != nil {
				next = *s.T
			}
			for range s.R + 1 {
				got = append(got, [2]int64{next, s.D})
				next += s.D
			}
		}
		for _, rep := range as.Representations {
			fs := frameSize[rep.ID]
			var want [][2]int64
			for n := int64(485); n <= 499; n++ {
				if fs == 0 {
					want = append(want, [2]int64{tl.VideoTfdt(n), tl.SegTicks()})
					continue
				}
				first, end := tl.AudioFrameRange(n, fs)
				want = append(want, [2]int64{first * int64(fs), (end - first) * int64(fs)})
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%s: timeline %v, want %v", rep.ID, got, want)
			}
			for _, e := range got {
				url := strings.NewReplacer("$RepresentationID$", rep.ID, "$Time$", strconv.FormatInt(e[0], 10)).Replace(as.SegmentTemplate.Media)
				if r := get(t, srv, "/live/demo/"+url); r.status != http.StatusOK {
					t.Errorf("GET %s: %d", url, r.status)
				}
			}
		}
	}
}
