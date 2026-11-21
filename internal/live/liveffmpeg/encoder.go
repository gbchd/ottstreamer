// Package liveffmpeg implements live.Encoder with ffmpeg: one run per video
// segment for the whole ladder, one run per audio rendition per epoch.
package liveffmpeg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/av1"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/hevc"
	"github.com/Eyevinn/mp4ff/mp4"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/sync/errgroup"

	"ottstreamer/internal/live"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/timeline"
)

var _ live.Encoder = (*Encoder)(nil)

// Metrics counts ffmpeg processes.
type Metrics interface {
	// FFmpegStarted records a process started for a stream.
	FFmpegStarted(streamID string)
	// FFmpegExited records its end: reason is "" when it succeeded or was
	// cancelled, else "timeout" or "exit".
	FFmpegExited(streamID, reason string)
}

// Encoder runs ffmpeg, at most maxProcs processes at a time.
type Encoder struct {
	ffmpeg  string
	tmpDir  string
	font    string
	sem     chan struct{}
	metrics Metrics
	logger  *slog.Logger

	mu       sync.Mutex
	overlays map[string]bool // overlay cycle files already rendered

	encMu    sync.Mutex
	encoders map[string]bool // what ffmpeg -encoders listed, on first use
	used     map[string]bool // encoders of every epoch started since boot
}

// NewEncoder checks that ffmpegPath runs and writes the Go Mono font into
// tmpDir, where every run also gets its own scratch directory.
func NewEncoder(ffmpegPath, tmpDir string, maxProcs int, metrics Metrics, logger *slog.Logger) (*Encoder, error) {
	if maxProcs < 1 {
		return nil, fmt.Errorf("maxProcs %d: must be at least 1", maxProcs)
	}
	path, err := exec.LookPath(ffmpegPath)
	if err != nil {
		return nil, fmt.Errorf("find ffmpeg: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return nil, fmt.Errorf("create tmp dir: %w", err)
	}
	font := filepath.Join(tmpDir, "GoMono.ttf")
	if err := os.WriteFile(font, gomono.TTF, 0o600); err != nil {
		return nil, fmt.Errorf("write font: %w", err)
	}
	return &Encoder{ffmpeg: path, tmpDir: tmpDir, font: font, sem: make(chan struct{}, maxProcs), metrics: metrics, logger: logger, overlays: map[string]bool{}, used: map[string]bool{}}, nil
}

// Check runs ffmpeg -version and returns the version it reports. It fails
// when ffmpeg does not run, lacks libx264, differs from pin (when set), or
// no longer lists an encoder used by an epoch started since boot: segments
// are byte-identical only across replicas on the same ffmpeg.
func (e *Encoder) Check(ctx context.Context, pin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, e.ffmpeg, "-hide_banner", "-version").Output() //nolint:gosec // the binary is operator config
	if err != nil {
		return "", fmt.Errorf("ffmpeg -version: %w", err)
	}
	f := strings.Fields(string(out)) // ffmpeg version 9.0.2 Copyright ...
	if len(f) < 3 || f[0] != "ffmpeg" || f[1] != "version" {
		return "", errors.New("ffmpeg -version: no version line")
	}
	v := f[2]
	switch {
	case !bytes.Contains(out, []byte("--enable-libx264")):
		return v, fmt.Errorf("ffmpeg %s: built without libx264", v)
	case pin != "" && v != pin:
		return v, fmt.Errorf("ffmpeg %s: want pinned version %s", v, pin)
	}
	e.encMu.Lock()
	used := slices.Sorted(maps.Keys(e.used))
	e.encMu.Unlock()
	if len(used) == 0 {
		return v, nil
	}
	have, err := e.listEncoders(ctx)
	if err != nil {
		return v, err
	}
	var missing []string
	for _, name := range used {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return v, fmt.Errorf("ffmpeg %s: lacks %s, used by a started stream", v, strings.Join(missing, ", "))
	}
	return v, nil
}

// listEncoders returns the encoder names ffmpeg -encoders lists.
func (e *Encoder) listEncoders(ctx context.Context) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, e.ffmpeg, "-hide_banner", "-encoders").Output() //nolint:gosec // the binary is operator config
	if err != nil {
		return nil, fmt.Errorf("ffmpeg -encoders: %w", err)
	}
	// A legend, a " ------" line, then one " V....D name  description" per encoder.
	_, list, _ := bytes.Cut(out, []byte("------"))
	names := map[string]bool{}
	for line := range bytes.Lines(list) {
		if f := bytes.Fields(line); len(f) >= 2 {
			names[string(f[1])] = true
		}
	}
	return names, nil
}

// checkEncoders fails with a *stream.ValidationError naming every rendition
// of spec whose encoder the local ffmpeg lacks, and records the encoders of
// spec as used otherwise.
//
// ponytail: used never shrinks when streams stop, so /readyz keeps checking
// an encoder no running stream needs; refcount by stream if that matters.
func (e *Encoder) checkEncoders(ctx context.Context, spec stream.Spec) error {
	e.encMu.Lock()
	defer e.encMu.Unlock()
	if e.encoders == nil {
		have, err := e.listEncoders(ctx)
		if err != nil {
			return err
		}
		e.encoders = have
	}
	var verr stream.ValidationError
	need := make([]string, 0, len(spec.Video)+len(spec.Audio))
	add := func(field, name string) {
		need = append(need, name)
		if !e.encoders[name] {
			verr.Fields = append(verr.Fields, stream.FieldError{Field: field, Message: name + " is missing from the local ffmpeg"})
		}
	}
	for i, v := range spec.Video {
		add(fmt.Sprintf("spec.video[%d].codec", i), videoEncoders[v.Codec])
	}
	for i, a := range spec.Audio {
		add(fmt.Sprintf("spec.audio[%d].codec", i), audioEncoders[a.Codec])
	}
	if len(verr.Fields) > 0 {
		return &verr
	}
	for _, name := range need {
		e.used[name] = true
	}
	return nil
}

// EncodeSegment encodes video segment n of s's current epoch for every
// rendition in one ffmpeg run.
func (e *Encoder) EncodeSegment(ctx context.Context, s stream.Stream, n int64, inAd bool) (map[string][]live.Sample, error) {
	files, err := e.encodeVideo(ctx, s, n, inAd)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]live.Sample, len(files))
	for id, f := range files {
		out[id] = samples(f)
	}
	return out, nil
}

// StartEpoch builds the init segments of every track of s's current epoch
// and the audio loops. Video comes from a run of segment 0; the audio
// renditions are encoded alongside it. It fails with a
// *stream.ValidationError before running ffmpeg when an encoder is missing.
func (e *Encoder) StartEpoch(ctx context.Context, s stream.Stream) (map[string]live.Track, error) {
	ep := s.Status.Epoch
	if ep == nil {
		return nil, fmt.Errorf("stream %s: no epoch", s.ID)
	}
	if err := e.checkEncoders(ctx, ep.Spec); err != nil {
		return nil, err
	}
	tracks := make([]live.Track, len(ep.Spec.Video)+len(ep.Spec.Audio))
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		files, err := e.encodeVideo(gctx, s, 0, false)
		if err != nil {
			return err
		}
		for i, v := range ep.Spec.Video {
			if tracks[i], err = videoTrack(v, files[v.ID]); err != nil {
				return err
			}
		}
		return nil
	})
	for i, a := range ep.Spec.Audio {
		g.Go(func() (err error) {
			tracks[len(ep.Spec.Video)+i], err = e.audioTrack(gctx, s, a)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	out := make(map[string]live.Track, len(tracks))
	for _, t := range tracks {
		out[t.ID] = t
	}
	return out, nil
}

// encodeVideo runs ffmpeg for segment n and returns each rendition's parsed
// output, checked to hold SegFrames samples starting with an IDR.
func (e *Encoder) encodeVideo(ctx context.Context, s stream.Stream, n int64, inAd bool) (map[string]*mp4.File, error) {
	ep := s.Status.Epoch
	if ep == nil {
		return nil, fmt.Errorf("stream %s: no epoch", s.ID)
	}
	loc, err := time.LoadLocation(ep.Spec.Timezone)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	dir, err := os.MkdirTemp(e.tmpDir, fmt.Sprintf("%s-%d-%d-", s.ID, ep.Number, n))
	if err != nil {
		return nil, fmt.Errorf("create segment dir: %w", err)
	}
	defer os.RemoveAll(dir) // best effort: scratch files only

	overlays, err := e.overlayFile(ctx, s.ID, ep.Spec)
	if err != nil {
		return nil, err
	}
	tl := ep.Timeline()
	timeout := max(2*tl.SegDuration(), 10*time.Second)
	if err := e.run(ctx, s.ID, timeout, buildVideoArgs(*ep, n, loc, inAd, e.font, overlays, dir)); err != nil {
		return nil, fmt.Errorf("segment %d: %w", n, err)
	}
	files := make(map[string]*mp4.File, len(ep.Spec.Video))
	for _, v := range ep.Spec.Video {
		f, err := readFile(filepath.Join(dir, v.ID+".mp4"))
		if err != nil {
			return nil, fmt.Errorf("segment %d, rendition %s: %w", n, v.ID, err)
		}
		if err := checkVideo(f, tl.SegFrames); err != nil {
			return nil, fmt.Errorf("segment %d, rendition %s: %w", n, v.ID, err)
		}
		files[v.ID] = f
	}
	return files, nil
}

// overlayFile returns the overlay cycle file for spec, rendering it on first
// use. Streams with the same picture geometry, fps and beep share one file.
func (e *Encoder) overlayFile(ctx context.Context, streamID string, spec stream.Spec) (string, error) {
	sum := sha256.Sum256([]byte(strings.Join(buildOverlayArgs(spec, ""), "\x00")))
	path := filepath.Join(e.tmpDir, fmt.Sprintf("overlays-%x.nut", sum[:8]))
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.overlays[path] {
		return path, nil
	}
	if err := e.run(ctx, streamID, 30*time.Second, buildOverlayArgs(spec, path)); err != nil {
		return "", fmt.Errorf("overlays: %w", err)
	}
	e.overlays[path] = true
	return path, nil
}

// checkVideo checks f holds want samples, the first one a key frame.
func checkVideo(f *mp4.File, want int64) error {
	ss := samples(f)
	if int64(len(ss)) != want {
		return fmt.Errorf("got %d frames, want %d", len(ss), want)
	}
	if !ss[0].Sync || !keyFrame(f.Init.Moov.Trak.Mdia.Minf.Stbl.Stsd, ss[0].Data) {
		return errors.New("first frame is not a key frame")
	}
	return nil
}

// keyFrame reports whether sample is an IDR (H.264, HEVC) or a key frame
// (AV1) of the track stsd describes.
func keyFrame(stsd *mp4.StsdBox, sample []byte) bool {
	switch {
	case stsd.AvcX != nil:
		return avc.IsIDRSample(sample)
	case stsd.HvcX != nil:
		return hevc.IsIDRSample(sample)
	case stsd.Av01 != nil && stsd.Av01.Av1C != nil:
		sh, err := stsd.Av01.Av1C.SequenceHeader()
		if err != nil {
			return false
		}
		key, err := av1.IsRAPSample(sample, sh)
		return err == nil && key
	}
	return false
}

func (e *Encoder) audioTrack(ctx context.Context, s stream.Stream, a stream.Audio) (live.Track, error) {
	dir, err := os.MkdirTemp(e.tmpDir, fmt.Sprintf("%s-%d-%s-", s.ID, s.Status.Epoch.Number, a.ID))
	if err != nil {
		return live.Track{}, fmt.Errorf("create audio dir: %w", err)
	}
	defer os.RemoveAll(dir) // best effort: scratch files only

	out := filepath.Join(dir, a.ID+".mp4")
	if err := e.run(ctx, s.ID, 10*time.Second, buildAudioArgs(a, s.Status.Epoch.Spec.FPS, out)); err != nil {
		return live.Track{}, fmt.Errorf("audio %s: %w", a.ID, err)
	}
	f, err := readFile(out)
	if err != nil {
		return live.Track{}, fmt.Errorf("audio %s: %w", a.ID, err)
	}
	t, err := audioTrack(a, f)
	if err != nil {
		return live.Track{}, fmt.Errorf("audio %s: %w", a.ID, err)
	}
	return t, nil
}

// run runs ffmpeg for a stream with args once a process slot is free. The
// whole process group is killed on cancel or after timeout. A failure's
// error ends with the tail of ffmpeg's stderr.
func (e *Encoder) run(ctx context.Context, streamID string, timeout time.Duration, args []string) (err error) {
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.sem }()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	e.metrics.FFmpegStarted(streamID)
	defer func() {
		var reason string
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			reason = "timeout"
		case err != nil && ctx.Err() == nil:
			reason = "exit"
		}
		e.metrics.FFmpegExited(streamID, reason)
	}()
	start := time.Now()
	cmd := exec.CommandContext(ctx, e.ffmpeg, args...) //nolint:gosec // the binary is operator config, args are built here
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	if err != nil {
		tail := stderr.Bytes()
		tail = tail[max(len(tail)-2048, 0):]
		return fmt.Errorf("ffmpeg: %w: %s", err, bytes.TrimSpace(tail))
	}
	e.logger.DebugContext(ctx, "ffmpeg run", slog.Duration("took", time.Since(start)))
	return nil
}

func readFile(path string) (*mp4.File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("parse output: %w", err)
	}
	if f.Init == nil || f.Init.Moov == nil || f.Init.Moov.Trak == nil || f.Init.Moov.Mvex == nil {
		return nil, errors.New("parse output: not a fragmented mp4 with one track")
	}
	return f, nil
}

// samples returns every sample of every fragment of f, in order. Data
// slices share f's buffer.
func samples(f *mp4.File) []live.Sample {
	var out []live.Sample
	for _, seg := range f.Segments {
		for _, frag := range seg.Fragments {
			for s, err := range frag.Samples(f.Init.Moov.Mvex.Trex) {
				if err != nil {
					return out // a short list fails the caller's count check
				}
				out = append(out, live.Sample{Data: s.Data, Sync: s.IsSync()})
			}
		}
	}
	return out
}

// videoTrack builds v's init segment from the encoder's own configuration:
// avc1 + avcC, hvc1 + hvcC or av01 + av1C.
func videoTrack(v stream.Video, f *mp4.File) (live.Track, error) {
	codec, init, err := videoInit(v.Codec, f.Init.Moov.Trak.Mdia.Minf.Stbl.Stsd)
	if err != nil {
		return live.Track{}, fmt.Errorf("rendition %s: %w", v.ID, err)
	}
	b, err := encodeInit(init)
	if err != nil {
		return live.Track{}, fmt.Errorf("rendition %s: %w", v.ID, err)
	}
	return live.Track{ID: v.ID, Kind: live.KindVideo, Timescale: timeline.VideoTimescale, Codec: codec, Init: b}, nil
}

// videoInit returns the codec string and init segment of a video track
// whose sample entry ffmpeg wrote as entry.
func videoInit(c stream.VideoCodec, entry *mp4.StsdBox) (string, *mp4.InitSegment, error) {
	init, trak := newInit(timeline.VideoTimescale, "video", "und")
	switch c {
	case stream.CodecHEVC:
		if entry.HvcX == nil || entry.HvcX.HvcC == nil {
			return "", nil, errors.New("no hvcC in ffmpeg output")
		}
		hvcC := entry.HvcX.HvcC
		spss := hvcC.GetNalusForType(hevc.NALU_SPS)
		if len(spss) == 0 {
			return "", nil, errors.New("no SPS in hvcC")
		}
		sps, err := hevc.ParseSPSNALUnit(spss[0])
		if err != nil {
			return "", nil, fmt.Errorf("parse SPS: %w", err)
		}
		err = trak.SetHEVCDescriptor("hvc1", hvcC.GetNalusForType(hevc.NALU_VPS), spss, hvcC.GetNalusForType(hevc.NALU_PPS), nil, true)
		return hevc.CodecString("hvc1", sps), init, err
	case stream.CodecAV1:
		av01 := entry.Av01
		if av01 == nil || av01.Av1C == nil {
			return "", nil, errors.New("no av1C in ffmpeg output")
		}
		err := trak.SetAV1Descriptor("av01", av01.Av1C, av01.Width, av01.Height)
		return av01.Av1C.CodecString("av01"), init, err
	default:
		avcx := entry.AvcX
		if avcx == nil || avcx.AvcC == nil || len(avcx.AvcC.SPSnalus) == 0 {
			return "", nil, errors.New("no avcC in ffmpeg output")
		}
		sps, err := avc.ParseSPSNALUnit(avcx.AvcC.SPSnalus[0], false)
		if err != nil {
			return "", nil, fmt.Errorf("parse SPS: %w", err)
		}
		err = trak.SetAVCDescriptor("avc1", avcx.AvcC.SPSnalus, avcx.AvcC.PPSnalus, true)
		return avc.CodecString("avc1", sps), init, err
	}
}

// audioTrack builds a's init segment from the encoder's own configuration and
// keeps the middle loopSeconds of samples as the loop.
func audioTrack(a stream.Audio, f *mp4.File) (live.Track, error) {
	entry := f.Init.Moov.Trak.Mdia.Minf.Stbl.Stsd
	init, trak := newInit(timeline.AudioTimescale, "audio", a.Lang)
	var codec string
	switch a.Codec {
	case stream.CodecAAC:
		mp4a := entry.Mp4a
		if mp4a == nil || mp4a.Esds == nil || mp4a.Esds.DecConfigDescriptor == nil || mp4a.Esds.DecConfigDescriptor.DecSpecificInfo == nil {
			return live.Track{}, errors.New("no esds in ffmpeg output")
		}
		asc, err := aac.DecodeAudioSpecificConfig(bytes.NewReader(mp4a.Esds.DecConfigDescriptor.DecSpecificInfo.DecConfig))
		if err != nil {
			return live.Track{}, fmt.Errorf("parse AudioSpecificConfig: %w", err)
		}
		if err := trak.SetAACDescriptorFromConfig(asc); err != nil {
			return live.Track{}, err
		}
		codec = "mp4a.40.2"
	case stream.CodecAC3:
		if entry.AC3 == nil || entry.AC3.Dac3 == nil {
			return live.Track{}, errors.New("no dac3 in ffmpeg output")
		}
		if err := trak.SetAC3Descriptor(entry.AC3.Dac3); err != nil {
			return live.Track{}, err
		}
		codec = "ac-3"
	case stream.CodecEAC3:
		if entry.EC3 == nil || entry.EC3.Dec3 == nil || len(entry.EC3.Dec3.EC3Subs) == 0 {
			return live.Track{}, errors.New("no dec3 in ffmpeg output")
		}
		if err := trak.SetEC3Descriptor(entry.EC3.Dec3); err != nil {
			return live.Track{}, err
		}
		codec = "ec-3"
	case stream.CodecOpus:
		// mp4ff has no SetOpusDescriptor: an Opus entry carries ffmpeg's dOps,
		// with no pre-skip. The loop starts long after the encoder's priming,
		// and frame k already plays pattern time k*FrameSize, as AAC and AC-3
		// frames do without an edit list.
		if entry.Opus == nil || entry.Opus.Dops == nil {
			return live.Track{}, errors.New("no dOps in ffmpeg output")
		}
		dops := *entry.Opus.Dops
		dops.PreSkip = 0
		trak.Mdia.Minf.Stbl.Stsd.AddChild(mp4.CreateAudioSampleEntryBox("Opus", uint16(dops.OutputChannelCount), 16, timeline.AudioTimescale, &dops))
		codec = "opus"
	default:
		return live.Track{}, fmt.Errorf("unsupported audio codec %q", a.Codec)
	}
	b, err := encodeInit(init)
	if err != nil {
		return live.Track{}, err
	}

	frameSize := a.Codec.FrameSize()
	period := loopSeconds * timeline.AudioTimescale / frameSize
	ss := samples(f)
	if len(ss) < 2*period {
		return live.Track{}, fmt.Errorf("got %d frames, want at least %d", len(ss), 2*period)
	}
	loop := slices.Clone(ss[period : 2*period])
	for i := range loop {
		loop[i].Sync = true // every audio frame is a sync sample
	}
	return live.Track{
		ID:        a.ID,
		Kind:      live.KindAudio,
		Timescale: timeline.AudioTimescale,
		Codec:     codec,
		Init:      b,
		FrameSize: frameSize,
		Loop:      loop,
	}, nil
}

// newInit returns a CMAF header with one empty track and brands cmfc, iso6, dash.
func newInit(timescale uint32, kind, lang string) (*mp4.InitSegment, *mp4.TrakBox) {
	init := mp4.CreateEmptyInit()
	init.SetFtyp(mp4.NewFtyp("cmfc", 0, []string{"cmfc", "iso6", "dash"}))
	return init, init.AddEmptyTrack(timescale, kind, lang)
}

func encodeInit(init *mp4.InitSegment) ([]byte, error) {
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode init: %w", err)
	}
	return buf.Bytes(), nil
}
