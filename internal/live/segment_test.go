package live

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/stream"
)

func TestManager_WriteSegment_notFound(t *testing.T) {
	h := newHarness(nil)
	h.add(t, running("a", ast))
	h.m.End("a", 5)
	tests := []struct {
		name, stream, rendition string
		n                       int64
	}{
		{"unknown stream", "b", "720p", 0},
		{"unknown rendition", "a", "1080p", 0},
		{"negative", "a", "720p", -1},
		{"after the end", "a", "720p", 6},
		{"audio after the end", "a", "en", 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.m.WriteSegment(t.Context(), &strings.Builder{}, tt.stream, tt.rendition, tt.n)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("WriteSegment = %v, want ErrNotFound", err)
			}
		})
	}
	if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", 5); err != nil {
		t.Fatalf("end segment: %v", err)
	}
}

func TestManager_WriteSegment_video(t *testing.T) {
	h := newHarness(nil)
	h.add(t, running("a", ast))
	sig := []adbreak.Signal{{EventID: 7, PresentationTicks: 540000, Message: []byte{0xfc}}}
	h.sched.signals = sig

	var out strings.Builder
	if err := h.m.WriteSegment(t.Context(), &out, "a", "720p", 3); err != nil {
		t.Fatal(err)
	}
	if out.String() != "segment 4" {
		t.Fatalf("wrote %q", out.String())
	}
	f := h.w.last()
	if f.Seq != 4 || f.TrackID != 1 || f.BaseTime != 3*180000 || f.SampleDur != 3600 || f.Codec != "avc1.64001f" {
		t.Fatalf("fragment = seq %d track %d tfdt %d dur %d codec %q", f.Seq, f.TrackID, f.BaseTime, f.SampleDur, f.Codec)
	}
	if len(f.Samples) != 50 || !f.Samples[0].Sync {
		t.Fatalf("%d samples, first sync %t; want 50, true", len(f.Samples), f.Samples[0].Sync)
	}
	if !slices.EqualFunc(f.Emsgs, sig, func(a, b adbreak.Signal) bool { return a.EventID == b.EventID }) {
		t.Fatalf("emsgs = %v, want %v", f.Emsgs, sig)
	}
	if f.PadTo != 750000 { // 3000 kbit/s × 2 s
		t.Fatalf("PadTo = %d, want 750000", f.PadTo)
	}

	if err := h.m.WriteSegment(t.Context(), &out, "a", "360p", 3); err != nil {
		t.Fatal(err)
	}
	if got := h.w.last().PadTo; got != 200000 { // 800 kbit/s × 2 s
		t.Fatalf("360p PadTo = %d, want 200000", got)
	}
	if got := len(h.enc.encoded()); got != 1 {
		t.Fatalf("%d encodes for two renditions of one segment, want 1", got)
	}
}

func TestManager_WriteSegment_audio(t *testing.T) {
	h := newHarness(nil)
	rt := h.add(t, running("a", ast))
	loop := rt.tracks["en"].Loop

	wantFirst := []int64{0, 94, 188, 282, 375} // ceil(n × 96000 / 1024)
	wantCount := []int{94, 94, 94, 93, 94}
	for n := range int64(5) {
		if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "en", n); err != nil {
			t.Fatal(err)
		}
		f := h.w.last()
		if f.BaseTime != uint64(wantFirst[n]*1024) || f.SampleDur != 1024 || len(f.Samples) != wantCount[n] {
			t.Fatalf("segment %d: tfdt %d dur %d, %d samples; want tfdt %d dur 1024, %d samples",
				n, f.BaseTime, f.SampleDur, len(f.Samples), wantFirst[n]*1024, wantCount[n])
		}
		if f.Emsgs != nil || f.PadTo != 0 {
			t.Fatalf("segment %d: audio has emsgs %v or padding %d", n, f.Emsgs, f.PadTo)
		}
		for i, s := range f.Samples {
			want := loop[(wantFirst[n]+int64(i))%375]
			if !s.Sync || string(s.Data) != string(want.Data) {
				t.Fatalf("segment %d sample %d = %q sync %t, want %q sync", n, i, s.Data, s.Sync, want.Data)
			}
		}
	}
	if got := string(h.w.last().Samples[0].Data); got != "frame 0" {
		t.Fatalf("segment 4 starts at %q, want the loop to wrap to frame 0", got)
	}
	if got := len(h.enc.encoded()); got != 0 {
		t.Fatalf("audio ran the encoder %d times", got)
	}
}

func TestManager_WriteSegment_singleEncode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(nil)
		h.add(t, running("a", ast))
		h.enc.gate = make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 5)
		for i := range errs {
			wg.Go(func() {
				errs[i] = h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", 20)
			})
		}
		synctest.Wait() // one caller in the encoder, the rest waiting on it
		close(h.enc.gate)
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		if got := len(h.enc.encoded()); got != 1 {
			t.Fatalf("%d encodes for 5 concurrent requests, want 1", got)
		}
	})
}

func TestManager_WriteSegment_cachedByProducer(t *testing.T) {
	h := newHarness(nil)
	rt := h.add(t, running("a", ast))
	h.m.produceUpTo(t.Context(), rt, 0, 3)
	for n := range int64(4) {
		if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "360p", n); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(h.enc.encoded()); got != 4 {
		t.Fatalf("%d encodes, want 4 (requests hit the cache)", got)
	}
	// 2 renditions × 100 bytes × 50 frames × 4 segments; the producer missed 4 times.
	if bytes, hits, misses := h.m.CacheStats(); bytes != 40000 || hits != 4 || misses != 4 {
		t.Fatalf("CacheStats = %d bytes, %d hits, %d misses; want 40000, 4, 4", bytes, hits, misses)
	}
}

func TestManager_WriteSegment_inAd(t *testing.T) {
	h := newHarness(nil)
	h.add(t, running("a", ast))
	write := func(n int64) {
		t.Helper()
		if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", n); err != nil {
			t.Fatal(err)
		}
	}
	write(10)
	h.sched.set(adbreak.Break{ID: "1", StartSeg: 10, EndSeg: 12}) // added after 10 was cached
	write(10)
	write(10)
	write(12) // first segment after the break
	h.sched.set(adbreak.Break{ID: "1", StartSeg: 10, EndSeg: 12, Cancelled: true})
	write(10)

	want := []encodeCall{{10, false}, {10, true}, {12, false}}
	if got := h.enc.encoded(); !slices.Equal(got, want) {
		t.Fatalf("encoded %v, want %v", got, want)
	}
}

func TestManager_WriteSegment_retry(t *testing.T) {
	tests := []struct {
		name    string
		fail    int
		wantErr error
	}{
		{"recovers on retry", 1, nil},
		{"fails twice", 2, ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(nil)
			h.add(t, running("a", ast))
			h.enc.fail = tt.fail
			err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", 3)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("WriteSegment = %v, want %v", err, tt.wantErr)
			}
			if got := len(h.enc.encoded()); got != 2 {
				t.Fatalf("%d encode attempts, want 2", got)
			}
		})
	}
}

// subtitled returns running(id) with an English WebVTT and a French IMSC1
// track, segments of seg and the clock in zone.
func subtitled(id string, seg time.Duration, zone string) stream.Stream {
	s := running(id, ast)
	s.Spec.Subtitles = []stream.Subtitle{
		{ID: "sub-en", Lang: "en", Format: stream.SubtitleWebVTT, Default: true},
		{ID: "sub-fr", Lang: "fr", Format: stream.SubtitleIMSC1},
	}
	s.Spec.SegmentDuration, s.Spec.GOPDuration, s.Spec.Timezone = seg, seg, zone
	s.Status.Epoch.Spec = s.Spec.Clone()
	return s
}

func TestManager_textTracks(t *testing.T) {
	h := newHarness(nil)
	rt := h.add(t, subtitled("a", 2*time.Second, "UTC"))
	for _, want := range []Track{
		{ID: "sub-en", Kind: KindText, Timescale: 90000, Codec: CodecWebVTT, Lang: "en", Init: []byte("init wvtt en")},
		{ID: "sub-fr", Kind: KindText, Timescale: 90000, Codec: CodecIMSC1, Lang: "fr", Init: []byte("init stpp.ttml.im1t fr")},
	} {
		got, ok := rt.Track(want.ID)
		if !ok || got.Kind != want.Kind || got.Timescale != want.Timescale || got.Codec != want.Codec || got.Lang != want.Lang || string(got.Init) != string(want.Init) {
			t.Errorf("track %s = %+v, %t; want %+v", want.ID, got, ok, want)
		}
	}
}

func TestManager_Cues(t *testing.T) {
	tests := []struct {
		name      string
		seg       time.Duration
		zone      string
		rendition string
		breaks    []adbreak.Break
		n         int64
		want      []Cue
	}{
		{"two whole seconds", 2 * time.Second, "UTC", "sub-en", nil, 3, []Cue{
			{540000, 630000, "EN · SUB · 18:00:06 · frame 150 · Bip!"},
			{630000, 720000, "EN · SUB · 18:00:07 · frame 175 · Bop!"},
		}},
		{"cut at 1.92 s segment bounds", 1920 * time.Millisecond, "UTC", "sub-fr", nil, 1, []Cue{
			{172800, 180000, "FR · SUB · 18:00:01 · frame 25 · Bop!"},
			{180000, 270000, "FR · SUB · 18:00:02 · frame 50 · Bip!"},
			{270000, 345600, "FR · SUB · 18:00:03 · frame 75 · Bop!"},
		}},
		{"local clock", 2 * time.Second, "Europe/Paris", "sub-en", nil, 0, []Cue{
			{0, 90000, "EN · SUB · 19:00:00 · frame 0 · Bip!"},
			{90000, 180000, "EN · SUB · 19:00:01 · frame 25 · Bop!"},
		}},
		{"in a break returned early", 2 * time.Second, "UTC", "sub-en", []adbreak.Break{{ID: "1", StartSeg: 10, EndSeg: 12, PlannedEndSeg: 15}}, 11, []Cue{
			{1980000, 2070000, "[AD 3/10] EN · SUB · 18:00:22 · frame 550 · Bip!"},
			{2070000, 2160000, "[AD 4/10] EN · SUB · 18:00:23 · frame 575 · Bop!"},
		}},
		{"break starting mid-second", 1920 * time.Millisecond, "UTC", "sub-en", []adbreak.Break{{ID: "1", StartSeg: 2, EndSeg: 4, PlannedEndSeg: 4}}, 2, []Cue{
			{345600, 360000, "[AD 1/5] EN · SUB · 18:00:03 · frame 75 · Bop!"},
			{360000, 450000, "[AD 2/5] EN · SUB · 18:00:04 · frame 100 · Bip!"},
			{450000, 518400, "[AD 3/5] EN · SUB · 18:00:05 · frame 125 · Bop!"},
		}},
		{"cancelled break", 2 * time.Second, "UTC", "sub-en", []adbreak.Break{{ID: "1", StartSeg: 0, EndSeg: 2, PlannedEndSeg: 2, Cancelled: true}}, 0, []Cue{
			{0, 90000, "EN · SUB · 18:00:00 · frame 0 · Bip!"},
			{90000, 180000, "EN · SUB · 18:00:01 · frame 25 · Bop!"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(nil)
			h.add(t, subtitled("a", tt.seg, tt.zone))
			h.sched.set(tt.breaks...)
			got, err := h.m.Cues(t.Context(), "a", tt.rendition, tt.n)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Cues =\n%v\nwant\n%v", got, tt.want)
			}
		})
	}
}

func TestManager_Cues_notFound(t *testing.T) {
	h := newHarness(nil)
	h.add(t, subtitled("a", 2*time.Second, "UTC"))
	h.m.End("a", 5)
	for _, tc := range []struct {
		rendition string
		n         int64
	}{{"720p", 0}, {"en", 0}, {"sub-de", 0}, {"sub-en", 6}} {
		if _, err := h.m.Cues(t.Context(), "a", tc.rendition, tc.n); !errors.Is(err, ErrNotFound) {
			t.Errorf("Cues(%s, %d) = %v, want ErrNotFound", tc.rendition, tc.n, err)
		}
	}
}

func TestManager_WriteSegment_text(t *testing.T) {
	h := newHarness(nil)
	h.add(t, subtitled("a", 2*time.Second, "UTC"))
	if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "sub-fr", 3); err != nil {
		t.Fatal(err)
	}
	f := h.w.last()
	if f.Seq != 4 || f.BaseTime != 540000 || f.Codec != CodecIMSC1 || f.Lang != "fr" || len(f.Cues) != 2 || len(f.Samples) != 0 || f.PadTo != 0 {
		t.Fatalf("fragment = %+v", f)
	}
	if got := len(h.enc.encoded()); got != 0 {
		t.Fatalf("subtitles ran the encoder %d times", got)
	}
}

func TestManager_WriteSegment_captions(t *testing.T) {
	tests := []struct {
		name   string
		seg    time.Duration
		breaks []adbreak.Break
		n      int64
		want   []Caption
	}{
		{"loads the next second", 2 * time.Second, nil, 3, []Caption{
			{7, [2]string{"CC3 - 18:00:07", "frame 175 - Bop!"}},
			{8, [2]string{"CC3 - 18:00:08", "frame 200 - Bip!"}},
		}},
		{"break of the next segment", 2 * time.Second, []adbreak.Break{{ID: "1", StartSeg: 4, EndSeg: 6, PlannedEndSeg: 6}}, 3, []Caption{
			{7, [2]string{"CC3 - 18:00:07", "frame 175 - Bop!"}},
			{8, [2]string{"[AD 1/4] CC3 - 18:00:08", "frame 200 - Bip!"}},
		}},
		{"1.92 s segment across three seconds", 1920 * time.Millisecond, nil, 1, []Caption{
			{2, [2]string{"CC3 - 18:00:02", "frame 50 - Bip!"}},
			{3, [2]string{"CC3 - 18:00:03", "frame 75 - Bop!"}},
			{4, [2]string{"CC3 - 18:00:04", "frame 100 - Bip!"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(nil)
			s := subtitled("a", tt.seg, "UTC")
			s.Spec.CEA608 = &stream.CEA608{Channel: "CC3", Lang: "en"}
			s.Status.Epoch.Spec = s.Spec.Clone()
			h.add(t, s)
			h.sched.set(tt.breaks...)
			if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", tt.n); err != nil {
				t.Fatal(err)
			}
			if f := h.w.last(); f.CCChannel != "CC3" || !slices.Equal(f.Captions, tt.want) {
				t.Errorf("channel %q, captions =\n%v\nwant\n%v", f.CCChannel, f.Captions, tt.want)
			}
		})
	}
}

func TestTimestamp(t *testing.T) {
	for ticks, want := range map[int64]string{0: "00:00:00.000", 172800: "00:00:01.920", 90000 * 3723: "01:02:03.000", 90000 * 100 * 3600: "100:00:00.000"} {
		if got := Timestamp(ticks); got != want {
			t.Errorf("Timestamp(%d) = %s, want %s", ticks, got, want)
		}
	}
}
