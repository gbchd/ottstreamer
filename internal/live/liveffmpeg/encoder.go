// Package liveffmpeg implements live.Encoder with ffmpeg: one run per video
// segment for the whole ladder, one run per audio rendition per epoch.
package liveffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/Eyevinn/mp4ff/aac"
	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/sync/errgroup"

	"ottstreamer/internal/live"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/timeline"
)

var _ live.Encoder = (*Encoder)(nil)

// Encoder runs ffmpeg, at most maxProcs processes at a time.
type Encoder struct {
	ffmpeg string
	tmpDir string
	font   string
	sem    chan struct{}
	logger *slog.Logger
}

// NewEncoder checks that ffmpegPath runs and writes the Go Mono font into
// tmpDir, where every run also gets its own scratch directory.
func NewEncoder(ffmpegPath, tmpDir string, maxProcs int, logger *slog.Logger) (*Encoder, error) {
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
	return &Encoder{ffmpeg: path, tmpDir: tmpDir, font: font, sem: make(chan struct{}, maxProcs), logger: logger}, nil
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
// renditions are encoded alongside it.
func (e *Encoder) StartEpoch(ctx context.Context, s stream.Stream) (map[string]live.Track, error) {
	ep := s.Status.Epoch
	if ep == nil {
		return nil, fmt.Errorf("stream %s: no epoch", s.ID)
	}
	tracks := make([]live.Track, len(ep.Spec.Video)+len(ep.Spec.Audio))
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		files, err := e.encodeVideo(gctx, s, 0, false)
		if err != nil {
			return err
		}
		for i, v := range ep.Spec.Video {
			if tracks[i], err = videoTrack(v.ID, files[v.ID]); err != nil {
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

	tl := ep.Timeline()
	timeout := max(2*tl.SegDuration(), 10*time.Second)
	if err := e.run(ctx, timeout, buildVideoArgs(*ep, n, loc, inAd, e.font, dir)); err != nil {
		return nil, fmt.Errorf("segment %d: %w", n, err)
	}
	files := make(map[string]*mp4.File, len(ep.Spec.Video))
	for _, v := range ep.Spec.Video {
		f, err := readFile(filepath.Join(dir, v.ID+".mp4"))
		if err != nil {
			return nil, fmt.Errorf("segment %d, rendition %s: %w", n, v.ID, err)
		}
		if err := checkVideo(samples(f), tl.SegFrames); err != nil {
			return nil, fmt.Errorf("segment %d, rendition %s: %w", n, v.ID, err)
		}
		files[v.ID] = f
	}
	return files, nil
}

func checkVideo(ss []live.Sample, want int64) error {
	if int64(len(ss)) != want {
		return fmt.Errorf("got %d frames, want %d", len(ss), want)
	}
	if !ss[0].Sync || !avc.IsIDRSample(ss[0].Data) {
		return errors.New("first frame is not an IDR")
	}
	return nil
}

func (e *Encoder) audioTrack(ctx context.Context, s stream.Stream, a stream.Audio) (live.Track, error) {
	dir, err := os.MkdirTemp(e.tmpDir, fmt.Sprintf("%s-%d-%s-", s.ID, s.Status.Epoch.Number, a.ID))
	if err != nil {
		return live.Track{}, fmt.Errorf("create audio dir: %w", err)
	}
	defer os.RemoveAll(dir) // best effort: scratch files only

	out := filepath.Join(dir, a.ID+".mp4")
	if err := e.run(ctx, 10*time.Second, buildAudioArgs(a, s.Status.Epoch.Spec.FPS, out)); err != nil {
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

// run runs ffmpeg with args once a process slot is free. The whole process
// group is killed on cancel or after timeout.
func (e *Encoder) run(ctx context.Context, timeout time.Duration, args []string) error {
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.sem }()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, e.ffmpeg, args...) //nolint:gosec // the binary is operator config, args are built here
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
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

func videoTrack(id string, f *mp4.File) (live.Track, error) {
	avcx := f.Init.Moov.Trak.Mdia.Minf.Stbl.Stsd.AvcX
	if avcx == nil || avcx.AvcC == nil || len(avcx.AvcC.SPSnalus) == 0 {
		return live.Track{}, fmt.Errorf("rendition %s: no avcC in ffmpeg output", id)
	}
	sps, err := avc.ParseSPSNALUnit(avcx.AvcC.SPSnalus[0], false)
	if err != nil {
		return live.Track{}, fmt.Errorf("rendition %s: parse SPS: %w", id, err)
	}
	init, trak := newInit(timeline.VideoTimescale, "video", "und")
	if err := trak.SetAVCDescriptor("avc1", avcx.AvcC.SPSnalus, avcx.AvcC.PPSnalus, true); err != nil {
		return live.Track{}, fmt.Errorf("rendition %s: %w", id, err)
	}
	b, err := encodeInit(init)
	if err != nil {
		return live.Track{}, fmt.Errorf("rendition %s: %w", id, err)
	}
	return live.Track{ID: id, Video: true, Timescale: timeline.VideoTimescale, Codec: avc.CodecString("avc1", sps), Init: b}, nil
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
