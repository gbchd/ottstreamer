package liveffmpeg

import (
	"slices"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/stream"
)

// testStream is a tiny ladder that encodes fast: 1 s segments at 25 fps.
func testStream(t testing.TB) stream.Stream {
	t.Helper()
	spec := stream.Spec{
		Formats:         []stream.Format{stream.FormatHLS, stream.FormatDASH},
		SegmentDuration: time.Second,
		Timezone:        "Europe/Paris",
		RenditionLabel:  true,
		Video: []stream.Video{
			{ID: "180p", Width: 320, Height: 180, BitrateKbps: 400},
			{ID: "90p", Width: 160, Height: 90, BitrateKbps: 150, Profile: "main"},
		},
		Audio: []stream.Audio{
			{ID: "en", Lang: "en", Channels: 2, BitrateKbps: 128, Default: true},
			{ID: "en-51", Lang: "en", Channels: 6, BitrateKbps: 384, Codec: stream.CodecAC3},
		},
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
	args := buildVideoArgs(*s.Status.Epoch, 7, paris, true, "/tmp/x/GoMono.ttf", "/tmp/x/seg")
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
		"(N+175)", "(n+175)",      // frame 7*25 since AST
		"geq=lum=", "split=2[s0][s1]", "scale=160:90",
		"90p · 160×90 · 150 kbit/s",                   // per-branch label
		"if(mod(floor((N+175)/25)\\,2)\\,480\\,1500)", // tic/toc Hz of the first audio track
	} {
		if !strings.Contains(graph, want) {
			t.Errorf("filter graph lacks %q", want)
		}
	}
	if strings.Contains(graph, "scale=320:180") {
		t.Error("top rendition is scaled")
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
