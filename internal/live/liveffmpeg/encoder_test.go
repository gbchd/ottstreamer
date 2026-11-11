package liveffmpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"

	"ottstreamer/internal/live"
	"ottstreamer/internal/live/livemp4"
)

var discardLogger = slog.New(slog.DiscardHandler)

// newEncoder returns an Encoder on the local ffmpeg, or skips the test.
func newEncoder(t testing.TB) *Encoder {
	t.Helper()
	if testing.Short() {
		t.Skip("runs ffmpeg")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	e, err := NewEncoder(ffmpeg, t.TempDir(), 4, discardLogger)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEncoder_StartEpoch(t *testing.T) {
	e := newEncoder(t)
	s := testStream(t)
	tracks, err := e.StartEpoch(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 4 {
		t.Fatalf("got %d tracks, want 4", len(tracks))
	}
	avc1 := regexp.MustCompile(`^avc1\.[0-9A-F]{6}$`)
	for _, v := range s.Spec.Video {
		tr := tracks[v.ID]
		if !tr.Video || tr.Timescale != 90000 || !avc1.MatchString(tr.Codec) {
			t.Errorf("%s: video %v, timescale %d, codec %q", v.ID, tr.Video, tr.Timescale, tr.Codec)
		}
		init := parseInit(t, tr.Init)
		trak := init.Moov.Trak
		entry := trak.Mdia.Minf.Stbl.Stsd.AvcX
		if trak.Mdia.Mdhd.Timescale != 90000 || entry == nil || entry.AvcC == nil ||
			int(entry.Width) != v.Width || int(entry.Height) != v.Height {
			t.Errorf("%s: init does not hold an avc1 track of %dx%d at 90 kHz", v.ID, v.Width, v.Height)
		}
	}
	tests := []struct {
		id              string
		codec           string
		frameSize, loop int
		channels        int
	}{
		{"en", "mp4a.40.2", 1024, 375, 2},
		{"en-51", "ac-3", 1536, 250, 6},
	}
	for _, tt := range tests {
		tr := tracks[tt.id]
		if tr.Video || tr.Codec != tt.codec || tr.Timescale != 48000 || tr.FrameSize != tt.frameSize || len(tr.Loop) != tt.loop {
			t.Errorf("%s: codec %q, timescale %d, frame size %d, loop %d frames; want %s, 48000, %d, %d",
				tt.id, tr.Codec, tr.Timescale, tr.FrameSize, len(tr.Loop), tt.codec, tt.frameSize, tt.loop)
		}
		init := parseInit(t, tr.Init)
		stsd := init.Moov.Trak.Mdia.Minf.Stbl.Stsd
		var channels int
		switch {
		case stsd.Mp4a != nil:
			channels = int(stsd.Mp4a.ChannelCount)
		case stsd.AC3 != nil:
			channels, _ = stsd.AC3.Dac3.ChannelInfo()
		}
		if channels != tt.channels || init.Moov.Trak.Mdia.Mdhd.Timescale != 48000 {
			t.Errorf("%s: %d channels at %d Hz, want %d at 48000", tt.id, channels, init.Moov.Trak.Mdia.Mdhd.Timescale, tt.channels)
		}
	}
}

// TestEncoder_AudioLoop decodes each loop played twice and checks that every
// second, from the second one on, starts with a beep on the expected
// channels and is silent on the others.
func TestEncoder_AudioLoop(t *testing.T) {
	e := newEncoder(t)
	s := testStream(t)
	tracks, err := e.StartEpoch(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range s.Spec.Audio {
		tr := tracks[a.ID]
		var buf bytes.Buffer
		buf.Write(tr.Init)
		loop := slices.Concat(tr.Loop, tr.Loop)
		err := livemp4.NewWriter().WriteSegment(&buf, live.Fragment{Seq: 1, TrackID: 1, SampleDur: uint32(tr.FrameSize), Samples: loop})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "a.mp4")
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		pcm, err := exec.CommandContext(t.Context(), "ffmpeg", "-v", "error", "-i", path, "-f", "f32le", "-").Output()
		if err != nil {
			t.Fatal(err)
		}
		sample := func(i, c int) float64 {
			return math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[4*(i*a.Channels+c):]))))
		}
		// The first second has no previous frame to overlap with.
		for sec := 1; sec < 16; sec++ {
			for c := range a.Channels {
				want := a.Channels != 6 || sec%8 == c || sec%8 >= 6
				start, onset := sec*48000, -1
				for i := start - 1000; i < start+1000; i++ {
					if sample(i, c) > 0.05 {
						onset = i - start
						break
					}
				}
				if want && (onset < 0 || onset > 30) || !want && onset >= 0 {
					t.Errorf("%s second %d channel %d: beep onset %d, want beep %v", a.ID, sec, c, onset, want)
				}
			}
		}
	}
}

// TestPriming checks the encoder delays the audio loop is shifted by.
func TestPriming(t *testing.T) {
	e := newEncoder(t)
	for _, a := range testStream(t).Spec.Audio {
		out := filepath.Join(t.TempDir(), "a.mp4")
		if err := e.run(t.Context(), 10*time.Second, buildAudioArgs(a, 25, out)); err != nil {
			t.Fatal(err)
		}
		f, err := readFile(out)
		if err != nil {
			t.Fatal(err)
		}
		edts := f.Init.Moov.Trak.Edts
		if edts == nil || len(edts.Elst) == 0 || len(edts.Elst[0].Entries) == 0 {
			t.Fatalf("%s: no edit list", a.ID)
		}
		if got := edts.Elst[0].Entries[0].MediaTime; got != int64(priming[a.Codec]) {
			t.Errorf("%s: elst media_time %d, want %d", a.ID, got, priming[a.Codec])
		}
	}
}

func TestEncoder_EncodeSegment(t *testing.T) {
	e := newEncoder(t)
	s := testStream(t)
	tracks, err := e.StartEpoch(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{0, 7} {
		start := time.Now()
		got, err := e.EncodeSegment(t.Context(), s, n, n == 7)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("segment %d: %v", n, time.Since(start))
		for _, v := range s.Spec.Video {
			ss := got[v.ID]
			if len(ss) != 25 {
				t.Fatalf("segment %d %s: %d samples, want 25", n, v.ID, len(ss))
			}
			for i, smp := range ss {
				if smp.Sync != (i == 0) {
					t.Errorf("segment %d %s: sample %d sync %v", n, v.ID, i, smp.Sync)
				}
			}
		}
	}

	// Determinism: the same segment twice gives the same bytes.
	a, err := e.EncodeSegment(t.Context(), s, 7, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.EncodeSegment(t.Context(), s, 7, true)
	if err != nil {
		t.Fatal(err)
	}
	for id := range a {
		for i := range a[id] {
			if !bytes.Equal(a[id][i].Data, b[id][i].Data) {
				t.Errorf("%s sample %d differs between runs", id, i)
				break
			}
		}
	}

	// A padded segment 7 behind the init segment decodes to 25 frames from 7 s.
	tl := s.Status.Epoch.Timeline()
	v := s.Spec.Video[0]
	padTo := v.BitrateKbps * 1000 / 8 // 1 s
	var buf bytes.Buffer
	buf.Write(tracks[v.ID].Init)
	err = livemp4.NewWriter().WriteSegment(&buf, live.Fragment{
		Seq: 8, TrackID: 1, BaseTime: uint64(tl.VideoTfdt(7)), SampleDur: 3600, Samples: a[v.ID], PadTo: padTo,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "seg.mp4")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if size := len(buf.Bytes()) - len(tracks[v.ID].Init); size < padTo {
		t.Errorf("segment is %d bytes, want at least %d", size, padTo)
	}
	probe := exec.CommandContext(t.Context(), "ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "stream=nb_read_frames,start_time", "-of", "csv=p=0", path)
	out, err := probe.CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "7.000000,25" {
		t.Errorf("ffprobe start_time,frames = %q, want 7.000000,25", got)
	}
}

func TestEncoder_EncodeSegmentCancel(t *testing.T) {
	e := newEncoder(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.EncodeSegment(ctx, testStream(t), 3, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func parseInit(t *testing.T, b []byte) *mp4.InitSegment {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if f.Init == nil || f.Init.Moov == nil || f.Init.Moov.Trak == nil {
		t.Fatal("not an init segment")
	}
	if f.Ftyp.MajorBrand() != "cmfc" || !f.Ftyp.HasCompatibleBrand("iso6") || !f.Ftyp.HasCompatibleBrand("dash") {
		t.Errorf("ftyp = %s %v", f.Ftyp.MajorBrand(), f.Ftyp.CompatibleBrands())
	}
	return f.Init
}

func BenchmarkEncoder_EncodeSegment(b *testing.B) {
	e := newEncoder(b)
	s := testStream(b)
	for b.Loop() {
		if _, err := e.EncodeSegment(b.Context(), s, 7, false); err != nil {
			b.Fatal(err)
		}
	}
}
