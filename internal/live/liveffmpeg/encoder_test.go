package liveffmpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
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
	"ottstreamer/internal/stream"
)

var discardLogger = slog.New(slog.DiscardHandler)

type nopMetrics struct{}

func (nopMetrics) FFmpegStarted(string)        {}
func (nopMetrics) FFmpegExited(string, string) {}

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
	e, err := NewEncoder(ffmpeg, t.TempDir(), 4, nopMetrics{}, discardLogger)
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
	if len(tracks) != 6 {
		t.Fatalf("got %d tracks, want 6", len(tracks))
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
		{"en-ec3", "ec-3", 1536, 250, 6},
		{"en-opus", "opus", 960, 400, 2},
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
		case stsd.EC3 != nil:
			channels, _ = stsd.EC3.Dec3.ChannelInfo()
		case stsd.Opus != nil && stsd.Opus.Dops != nil:
			channels = int(stsd.Opus.Dops.OutputChannelCount)
			if stsd.Opus.Dops.PreSkip != 0 {
				t.Errorf("%s: dOps pre-skip %d, want 0", tt.id, stsd.Opus.Dops.PreSkip)
			}
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
		if err := e.run(t.Context(), "test", 10*time.Second, buildAudioArgs(a, 25, out)); err != nil {
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
		Seq: 8, TrackID: 1, Codec: tracks[v.ID].Codec, BaseTime: uint64(tl.VideoTfdt(7)), SampleDur: 3600, Samples: a[v.ID], PadTo: padTo,
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

// TestEncoder_codecs encodes a ladder of every video codec and checks the
// init segments, key frames at every GOP start, determinism, and that
// padding keeps the decoded frames.
func TestEncoder_codecs(t *testing.T) {
	e := newEncoder(t)
	s := newStream(t, 2*time.Second, []stream.Video{
		{ID: "h264", Width: 320, Height: 180, BitrateKbps: 400},
		{ID: "hevc", Codec: stream.CodecHEVC, Width: 320, Height: 180, BitrateKbps: 400},
		{ID: "hevc-10", Codec: stream.CodecHEVC, Profile: "main10", Width: 160, Height: 90, BitrateKbps: 150},
		{ID: "av1", Codec: stream.CodecAV1, Width: 320, Height: 180, BitrateKbps: 400},
		{ID: "av1-10", Codec: stream.CodecAV1, BitDepth: 10, Width: 160, Height: 90, BitrateKbps: 150},
	})
	tracks, err := e.StartEpoch(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]struct {
		codec *regexp.Regexp
		entry string
	}{
		"h264":    {regexp.MustCompile(`^avc1\.64[0-9A-F]{4}$`), "avc1"},
		"hevc":    {regexp.MustCompile(`^hvc1\.1\.6\.L\d+\.[0-9A-F]+$`), "hvc1"}, // Main
		"hevc-10": {regexp.MustCompile(`^hvc1\.2\.4\.L\d+\.[0-9A-F]+$`), "hvc1"}, // Main 10
		"av1":     {regexp.MustCompile(`^av01\.0\.\d\dM\.08$`), "av01"},
		"av1-10":  {regexp.MustCompile(`^av01\.0\.\d\dM\.10$`), "av01"},
	}
	stsds := map[string]*mp4.StsdBox{}
	for _, v := range s.Spec.Video {
		tr, want := tracks[v.ID], wants[v.ID]
		stsd := parseInit(t, tr.Init).Moov.Trak.Mdia.Minf.Stbl.Stsd
		stsds[v.ID] = stsd
		if len(stsd.Children) != 1 || stsd.Children[0].Type() != want.entry || !want.codec.MatchString(tr.Codec) {
			t.Errorf("%s: sample entry %s, codec %q; want %s, %s", v.ID, stsd.Children[0].Type(), tr.Codec, want.entry, want.codec)
		}
	}

	const n = 3
	start := time.Now()
	a, err := e.EncodeSegment(t.Context(), s, n, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("segment %d of 5 renditions: %v", n, time.Since(start))
	for id, ss := range a {
		if len(ss) != 50 {
			t.Fatalf("%s: %d samples, want 50", id, len(ss))
		}
		for i, smp := range ss {
			if gopStart := i%25 == 0; smp.Sync != gopStart || keyFrame(stsds[id], smp.Data) != gopStart {
				t.Errorf("%s sample %d: sync %v, key frame %v", id, i, smp.Sync, keyFrame(stsds[id], smp.Data))
			}
		}
	}

	// Determinism: the same segment twice gives the same bytes, with every
	// encoder single-threaded (also verified at 1080p on x265 4.3 and SVT-AV1 4.3).
	b, err := e.EncodeSegment(t.Context(), s, n, false)
	if err != nil {
		t.Fatal(err)
	}
	for id := range a {
		if !slices.EqualFunc(a[id], b[id], func(x, y live.Sample) bool { return bytes.Equal(x.Data, y.Data) }) {
			t.Errorf("%s differs between runs", id)
		}
	}

	// Padding: within 1 % of bitrate × duration, decoding to the same frames.
	tl := s.Status.Epoch.Timeline()
	dir := t.TempDir()
	for _, v := range s.Spec.Video {
		padTo := v.BitrateKbps * 1000 / 8 * 2 // 2 s
		var md5s [2]string
		for i, pad := range []int{0, padTo} {
			var buf bytes.Buffer
			buf.Write(tracks[v.ID].Init)
			err := livemp4.NewWriter().WriteSegment(&buf, live.Fragment{
				Seq: n + 1, TrackID: 1, Codec: tracks[v.ID].Codec, BaseTime: uint64(tl.VideoTfdt(n)), SampleDur: 3600, Samples: a[v.ID], PadTo: pad,
			})
			if err != nil {
				t.Fatal(err)
			}
			// The mdat payload is what PadTo counts; moof adds about 500 bytes.
			if pad > 0 {
				f, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				if size := len(f.Segments[0].Fragments[0].Mdat.Data); math.Abs(float64(size-padTo)) > 0.01*float64(padTo) {
					t.Errorf("%s: padded mdat is %d bytes, want %d ± 1 %%", v.ID, size, padTo)
				}
			}
			path := filepath.Join(dir, fmt.Sprintf("%s-%d.mp4", v.ID, pad))
			if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd := exec.CommandContext(t.Context(), "ffmpeg", "-v", "warning", "-i", path, "-f", "framemd5", "-")
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil || stderr.Len() > 0 {
				t.Fatalf("%s: decode: %v: %s", v.ID, err, stderr.Bytes())
			}
			md5s[i] = string(out)
		}
		if md5s[0] != md5s[1] || strings.Count(md5s[0], "\n0, ") != 50 {
			t.Errorf("%s: padded and unpadded segments decode to different frames:\n%s\n%s", v.ID, md5s[0], md5s[1])
		}
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

// BenchmarkEncoder_EncodeSegmentCodec times one 2 s segment of a single
// rendition per codec and size.
func BenchmarkEncoder_EncodeSegmentCodec(b *testing.B) {
	e := newEncoder(b)
	for _, size := range [][2]int{{320, 180}, {1920, 1080}} {
		for _, c := range []stream.VideoCodec{stream.CodecH264, stream.CodecHEVC, stream.CodecAV1} {
			b.Run(fmt.Sprintf("%s/%dx%d", c, size[0], size[1]), func(b *testing.B) {
				s := newStream(b, 2*time.Second, []stream.Video{{ID: "v", Codec: c, Width: size[0], Height: size[1], BitrateKbps: 5000}})
				for b.Loop() {
					if _, err := e.EncodeSegment(b.Context(), s, 7, false); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

type recordMetrics struct{ started, exited []string }

func (m *recordMetrics) FFmpegStarted(id string) { m.started = append(m.started, id) }
func (m *recordMetrics) FFmpegExited(id string, reason string) {
	m.exited = append(m.exited, id+":"+reason)
}

// fakeFFmpeg returns an Encoder whose ffmpeg is a shell script.
func fakeFFmpeg(t *testing.T, script string, m Metrics) *Encoder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	e, err := NewEncoder(path, t.TempDir(), 1, m, discardLogger)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEncoder_Check(t *testing.T) {
	const ok = `echo "ffmpeg version 9.0.2 Copyright (c) 2000-2026"; echo "configuration: --enable-gpl --enable-libx264"`
	tests := []struct {
		name, script, pin, want string
	}{
		{"no pin", ok, "", ""},
		{"pinned", ok, "9.0.2", ""},
		{"other version", ok, "8.1", "want pinned version 8.1"},
		{"no libx264", `echo "ffmpeg version 9.0.2"; echo "configuration: --enable-gpl"`, "", "without libx264"},
		{"garbage", `echo hello`, "", "no version line"},
		{"fails", `exit 3`, "", "exit status 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fakeFFmpeg(t, tt.script, nopMetrics{}).Check(t.Context(), tt.pin)
			if (err == nil) != (tt.want == "") || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("Check = %v, want %q", err, tt.want)
			}
		})
	}
}

// encodersScript is a fake ffmpeg that lists libx264, libx265 and aac.
const encodersScript = `case "$2" in
-version) echo "ffmpeg version 9.0.2"; echo "configuration: --enable-libx264";;
-encoders) printf 'Encoders:\n V..... = Video\n ------\n V....D libx264    libx264 H.264\n V....D libx265    libx265 H.265 / HEVC\n A....D aac        AAC (Advanced Audio Coding)\n';;
*) exit 1;;
esac`

func TestEncoder_StartEpochMissingEncoder(t *testing.T) {
	m := &recordMetrics{}
	e := fakeFFmpeg(t, encodersScript, m)
	s := newStream(t, time.Second, []stream.Video{
		{ID: "h264", Width: 320, Height: 180, BitrateKbps: 400},
		{ID: "av1", Codec: stream.CodecAV1, Width: 320, Height: 180, BitrateKbps: 300},
		{ID: "hevc", Codec: stream.CodecHEVC, Width: 320, Height: 180, BitrateKbps: 300},
	}, stream.Audio{ID: "en", Lang: "en", Channels: 2, BitrateKbps: 128, Default: true},
		stream.Audio{ID: "en-opus", Lang: "en", Channels: 2, BitrateKbps: 96, Codec: stream.CodecOpus})
	_, err := e.StartEpoch(t.Context(), s)
	verr, ok := errors.AsType[*stream.ValidationError](err)
	if !ok {
		t.Fatalf("StartEpoch = %v, want a *stream.ValidationError", err)
	}
	want := []stream.FieldError{
		{Field: "spec.video[0].codec", Message: "libsvtav1 is missing from the local ffmpeg"},
		{Field: "spec.audio[1].codec", Message: "libopus is missing from the local ffmpeg"},
	}
	if !slices.Equal(verr.Fields, want) {
		t.Errorf("fields = %v, want %v", verr.Fields, want)
	}
	if len(m.started) != 0 || len(e.used) != 0 {
		t.Errorf("ran ffmpeg %d times, recorded %v as used; want none", len(m.started), e.used)
	}
}

func TestEncoder_CheckUsedEncoders(t *testing.T) {
	e := fakeFFmpeg(t, encodersScript, nopMetrics{})
	s := newStream(t, time.Second, []stream.Video{{ID: "hevc", Codec: stream.CodecHEVC, Width: 320, Height: 180, BitrateKbps: 300}})
	// The encoders pass; the fake ffmpeg then fails the encode itself.
	if _, err := e.StartEpoch(t.Context(), s); err == nil || errors.Is(err, stream.ErrInvalid) {
		t.Fatalf("StartEpoch = %v, want an ffmpeg failure", err)
	}
	if _, err := e.Check(t.Context(), ""); err != nil {
		t.Fatalf("Check = %v", err)
	}
	e.used["libsvtav1"] = true // as if the local build lost it since
	if _, err := e.Check(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "lacks libsvtav1") {
		t.Fatalf("Check = %v, want lacks libsvtav1", err)
	}
}

func TestEncoder_runFailure(t *testing.T) {
	m := &recordMetrics{}
	e := fakeFFmpeg(t, `[ "$1" = hang ] && exec sleep 10; echo "Unknown encoder libx999" >&2; exit 1`, m)
	err := e.run(t.Context(), "demo", time.Second, nil)
	if err == nil || !strings.Contains(err.Error(), "Unknown encoder libx999") {
		t.Fatalf("run = %v, want the stderr tail", err)
	}
	if err := e.run(t.Context(), "demo", 100*time.Millisecond, []string{"hang"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run of a hanging ffmpeg = %v, want DeadlineExceeded", err)
	}
	if !slices.Equal(m.started, []string{"demo", "demo"}) || !slices.Equal(m.exited, []string{"demo:exit", "demo:timeout"}) {
		t.Fatalf("metrics started %v, exited %v", m.started, m.exited)
	}
}
