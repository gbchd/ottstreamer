package liveffmpeg

import (
	"slices"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/stream"
)

// testStream is a tiny ladder that encodes fast: 1 s segments at 25 fps,
// every audio codec.
func testStream(t testing.TB) stream.Stream {
	t.Helper()
	return newStream(t, time.Second, []stream.Video{
		{ID: "180p", Width: 320, Height: 180, BitrateKbps: 400},
		{ID: "90p", Width: 160, Height: 90, BitrateKbps: 150, Profile: "main"},
	}, []stream.Audio{
		{ID: "en", Lang: "en", Channels: 2, BitrateKbps: 128, Default: true},
		{ID: "en-51", Lang: "en", Channels: 6, BitrateKbps: 384, Codec: stream.CodecAC3},
		{ID: "en-ec3", Lang: "en", Channels: 6, BitrateKbps: 256, Codec: stream.CodecEAC3},
		{ID: "en-opus", Lang: "en", Channels: 2, BitrateKbps: 96, Codec: stream.CodecOpus},
		{ID: "en-opus-51", Lang: "en", Channels: 6, BitrateKbps: 256, Codec: stream.CodecOpus},
	}...)
}

// newStream returns a running stream with 1 s GOPs at 25 fps, segments seg
// long, the given ladder and audio (one AAC track when none is given).
func newStream(t testing.TB, seg time.Duration, video []stream.Video, audio ...stream.Audio) stream.Stream {
	t.Helper()
	if len(audio) == 0 {
		audio = []stream.Audio{{ID: "en", Lang: "en", Channels: 2, BitrateKbps: 128, Default: true}}
	}
	spec := stream.Spec{
		Formats:         []stream.Format{stream.FormatHLS, stream.FormatDASH},
		SegmentDuration: seg,
		GOPDuration:     time.Second,
		Timezone:        "Europe/Paris",
		RenditionLabel:  true,
		Video:           video,
		Audio:           audio,
	}
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
	ast := time.Date(2026, 11, 6, 18, 30, 0, 0, time.UTC)
	return stream.Stream{
		ID:     "test",
		Spec:   spec,
		Status: stream.Status{State: stream.StateRunning, Epoch: &stream.Epoch{Number: 1, AST: ast, Spec: spec.Clone()}},
	}
}

func TestBuildVideoArgs(t *testing.T) {
	s := testStream(t)
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	args := buildVideoArgs(*s.Status.Epoch, 7, paris, true, "/tmp/x/GoMono.ttf", "/tmp/x/ov.nut", "/tmp/x/seg")
	t.Log("ffmpeg '" + strings.Join(args, "' '") + "'")

	// flag returns the values following every occurrence of name.
	flag := func(name string) []string {
		var v []string
		for i, a := range args[:len(args)-1] {
			if a == name {
				v = append(v, args[i+1])
			}
		}
		return v
	}
	checks := []struct {
		flag string
		want []string
	}{
		{"-map", []string{"[o0]", "[o1]"}},
		{"-g", []string{"25", "25"}},
		{"-keyint_min", []string{"25", "25"}},
		{"-bf", []string{"0", "0"}},
		{"-sc_threshold", []string{"0", "0"}},
		{"-b:v", []string{"400k", "150k"}},
		{"-maxrate", []string{"400k", "150k"}},
		{"-bufsize", []string{"800k", "300k"}},
		{"-profile:v", []string{"high", "main"}},
		{"-preset", []string{"veryfast", "veryfast"}},
		{"-threads", []string{"1", "1"}},
		{"-x264-params", []string{"open-gop=0", "open-gop=0"}},
		{"-frames:v", []string{"25", "25"}},
	}
	for _, c := range checks {
		if got := flag(c.flag); !slices.Equal(got, c.want) {
			t.Errorf("%s = %v, want %v", c.flag, got, c.want)
		}
	}
	if got := args[len(args)-1]; got != "/tmp/x/seg/90p.mp4" {
		t.Errorf("last output = %s", got)
	}

	graph := flag("-filter_complex")[0]
	for _, want := range []string{
		"lutyuv=y=63:u=102:v=240", // red background in an ad
		"(n+175)",                 // frame 7*25 since AST
		"trim=start_frame=25",     // 175 mod 2*25 into the overlay cycle
		"split=2[s0][s1]", "scale=160:90",
		"90p · H.264 · 160×90 · 150 kbit/s", // per-branch label
	} {
		if !strings.Contains(graph, want) {
			t.Errorf("filter graph lacks %q", want)
		}
	}
	if strings.Contains(graph, "scale=320:180") {
		t.Error("top rendition is scaled")
	}
}

// TestBuildVideoArgs_codecs checks each codec's encoder options, and that a
// ladder grouped by codec is drawn at its largest rendition.
func TestBuildVideoArgs_codecs(t *testing.T) {
	s := newStream(t, time.Second, []stream.Video{
		{ID: "av1", Codec: stream.CodecAV1, Width: 640, Height: 360, BitrateKbps: 800},
		{ID: "av1-10", Codec: stream.CodecAV1, BitDepth: 10, Width: 320, Height: 180, BitrateKbps: 300},
		{ID: "hevc", Codec: stream.CodecHEVC, Width: 640, Height: 360, BitrateKbps: 900},
		{ID: "hevc-10", Codec: stream.CodecHEVC, Profile: "main10", Width: 320, Height: 180, BitrateKbps: 350},
		{ID: "h264", Width: 1280, Height: 720, BitrateKbps: 3000},
	})
	args := buildVideoArgs(*s.Status.Epoch, 7, time.UTC, false, "/f.ttf", "/ov.nut", "/seg")
	if s.Spec.Video[0].ID != "av1" {
		t.Fatalf("ladder starts with %s, want av1", s.Spec.Video[0].ID)
	}

	// outputs splits args after the filter graph into one string per output.
	var outputs []string
	start := slices.Index(args, "-map")
	for i := start + 1; i <= len(args); i++ {
		if i == len(args) || args[i] == "-map" {
			outputs = append(outputs, strings.Join(args[start:i], " "))
			start = i
		}
	}
	const x265 = "-x265-params bframes=0:keyint=25:min-keyint=25:scenecut=0:open-gop=0:repeat-headers=0:info=0:pools=1:frame-threads=1:log-level=error"
	const svt = "-svtav1-params keyint=25:scd=0:rtc=1:rc=2:lp=1"
	wants := map[string][]string{
		"av1":     {"-c:v libsvtav1 -preset 10 -b:v 800k -bufsize 1600k -g 25", svt},
		"av1-10":  {"-c:v libsvtav1 -preset 10 -b:v 300k -bufsize 600k -g 25", svt},
		"hevc":    {"-c:v libx265 -tag:v hvc1 -profile:v main -preset ultrafast -b:v 900k -maxrate 900k -bufsize 1800k", x265},
		"hevc-10": {"-c:v libx265 -tag:v hvc1 -profile:v main10 -preset ultrafast -b:v 350k -maxrate 350k -bufsize 700k", x265},
		"h264":    {"-c:v libx264 -threads 1 -profile:v high", "-g 25 -keyint_min 25 -sc_threshold 0 -bf 0 -x264-params open-gop=0"},
	}
	for i, v := range s.Spec.Video {
		out := outputs[i]
		if !strings.HasSuffix(out, "/seg/"+v.ID+".mp4") {
			t.Fatalf("output %d is not %s: %s", i, v.ID, out)
		}
		for _, want := range wants[v.ID] {
			if !strings.Contains(out, want) {
				t.Errorf("%s lacks %q:\n%s", v.ID, want, out)
			}
		}
		if tenBit := strings.Contains(out, "-pix_fmt yuv420p10le"); tenBit != strings.HasSuffix(v.ID, "-10") {
			t.Errorf("%s: 10-bit output %v", v.ID, tenBit)
		}
	}

	graph := args[slices.Index(args, "-filter_complex")+1]
	for _, want := range []string{
		"color=c=black:s=1280x720:r=25", "scale=640:360", "scale=320:180",
		"av1 · AV1 · 640×360 · 800 kbit/s", "hevc-10 · HEVC · 320×180 · 350 kbit/s", "h264 · H.264 · 1280×720 · 3000 kbit/s",
	} {
		if !strings.Contains(strings.Join(args, " "), want) {
			t.Errorf("args lack %q", want)
		}
	}
	if strings.Contains(graph, "scale=1280:720") {
		t.Error("the largest rendition is scaled")
	}
	if ov := strings.Join(buildOverlayArgs(s.Spec, "/ov.nut"), " "); !strings.Contains(ov, "color=c=black:s=1280x") {
		t.Errorf("overlays are not drawn for 1280x720: %s", ov)
	}
}

func TestBuildOverlayArgs(t *testing.T) {
	s := testStream(t)
	args := buildOverlayArgs(s.Spec, "/tmp/x/ov.nut")
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"color=c=black:s=66x66:r=25",            // disc 0.37·180
		"color=c=black:s=320x6:r=25",            // strip
		"if(mod(floor(N/25)\\,2)\\,480\\,1500)", // tic/toc Hz of the first audio track
		"-frames:v 50", "-c:v ffv1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("overlay args lack %q", want)
		}
	}
	if args[len(args)-1] != "/tmp/x/ov.nut" {
		t.Errorf("last arg = %s", args[len(args)-1])
	}
}

func TestClockText(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		start time.Time
		want  string
	}{
		{"UTC", time.Date(2026, 11, 6, 18, 30, 0, 0, time.UTC), "%{pts:gmtime:1793989800.020000:'%H:%M:%S'}.%{eif:mod((n+0),25):d:2}"},
		{"winter +1h", time.Date(2026, 11, 6, 18, 30, 0, 0, time.UTC).In(paris), "%{pts:gmtime:1793993400.020000:"},
		{"summer +2h", time.Date(2026, 7, 6, 18, 30, 1, 920_000_000, time.UTC).In(paris), "%{pts:gmtime:1783369801.940000:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clockText(tt.start, 25, "(n+0)"); !strings.HasPrefix(got, tt.want) {
				t.Errorf("clockText = %s, want prefix %s", got, tt.want)
			}
		})
	}
}

func TestQuote(t *testing.T) {
	if got, want := quote("a,b:'c'"), `\'a\,b:\'\\\'\'c\'\\\'\'\'`; got != want {
		t.Errorf("quote = %s, want %s", got, want)
	}
}
