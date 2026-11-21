package live

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/event"
	"ottstreamer/internal/timeline"
)

// WriteSegment writes media segment n of a rendition to w. Video comes from
// the cache or is encoded on demand; audio is sliced from the track's loop;
// subtitle cues are generated. It does not check n against the live window.
func (m *Manager) WriteSegment(ctx context.Context, w io.Writer, streamID, rendition string, n int64) error {
	rt, t, err := m.segment(streamID, rendition, n)
	if err != nil {
		return err
	}
	f, err := m.fragment(ctx, rt, t, n)
	if err != nil {
		return err
	}
	return m.w.WriteSegment(w, f)
}

// Cues returns the cues of segment n of a subtitle rendition, as in its
// media segment. It does not check n against the live window.
func (m *Manager) Cues(ctx context.Context, streamID, rendition string, n int64) ([]Cue, error) {
	rt, t, err := m.segment(streamID, rendition, n)
	if err != nil {
		return nil, err
	}
	if t.Kind != KindText {
		return nil, fmt.Errorf("%w: %s of %s has no cues", ErrNotFound, rendition, streamID)
	}
	return m.cues(ctx, rt, t, n)
}

// segment finds the runtime and track of segment n of a rendition.
func (m *Manager) segment(streamID, rendition string, n int64) (*Runtime, Track, error) {
	rt, ok := m.Runtime(streamID)
	if !ok {
		return nil, Track{}, fmt.Errorf("%w: stream %s", ErrNotFound, streamID)
	}
	t, ok := rt.Track(rendition)
	if !ok {
		return nil, Track{}, fmt.Errorf("%w: rendition %s of %s", ErrNotFound, rendition, streamID)
	}
	if tl := rt.Timeline(); n < 0 || (tl.EndSeg >= 0 && n > tl.EndSeg) {
		return nil, Track{}, fmt.Errorf("%w: segment %d of %s", ErrNotFound, n, streamID)
	}
	return rt, t, nil
}

func (m *Manager) fragment(ctx context.Context, rt *Runtime, t Track, n int64) (Fragment, error) {
	tl := rt.Timeline()
	f := Fragment{Seq: uint32(n + 1), TrackID: 1, Codec: t.Codec} //nolint:gosec // mfhd sequence_number is 32-bit and wraps
	switch t.Kind {
	case KindText:
		cues, err := m.cues(ctx, rt, t, n)
		if err != nil {
			return Fragment{}, err
		}
		f.BaseTime = uint64(tl.VideoTfdt(n)) //nolint:gosec // n >= 0
		f.Cues, f.Lang = cues, t.Lang
		return f, nil
	case KindAudio:
		first, end := tl.AudioFrameRange(n, t.FrameSize)
		f.Samples = make([]Sample, 0, end-first)
		for i := first; i < end; i++ {
			f.Samples = append(f.Samples, Sample{Data: t.Loop[i%int64(len(t.Loop))].Data, Sync: true})
		}
		f.BaseTime = uint64(first * int64(t.FrameSize)) //nolint:gosec // n >= 0, so first >= 0
		f.SampleDur = uint32(t.FrameSize)               //nolint:gosec // 960..1536
		return f, nil
	}

	set, err := m.segmentSet(ctx, rt, n)
	if err != nil {
		return Fragment{}, err
	}
	samples, ok := set[t.ID]
	if !ok {
		return Fragment{}, fmt.Errorf("%w: no %s in segment %d of %s", ErrUnavailable, t.ID, n, rt.stream.ID)
	}
	signals, err := m.sched.Signals(ctx, rt.stream.ID, rt.epoch, n)
	if err != nil {
		return Fragment{}, fmt.Errorf("live: signals of segment %d of %s: %w", n, rt.stream.ID, err)
	}
	f.BaseTime = uint64(tl.VideoTfdt(n))                   //nolint:gosec // n >= 0
	f.SampleDur = uint32(timeline.VideoTimescale / tl.FPS) //nolint:gosec // 1500..3750
	f.Samples = samples
	f.Emsgs = signals
	f.PadTo = padBytes(rt, t.ID, tl)
	if c := rt.stream.Status.Epoch.Spec.CEA608; c != nil {
		f.CCChannel = c.Channel
		if f.Captions, err = m.captions(ctx, rt, c.Channel, n); err != nil {
			return Fragment{}, err
		}
	}
	return f, nil
}

// padBytes is the rendition's bitrate × segment duration, in bytes.
func padBytes(rt *Runtime, rendition string, tl timeline.Timeline) int {
	for _, v := range rt.stream.Status.Epoch.Spec.Video {
		if v.ID == rendition {
			return int(int64(v.BitrateKbps) * 1000 * tl.SegFrames / (8 * int64(tl.FPS)))
		}
	}
	return 0
}

// segmentSet returns every video rendition of segment n, from the cache or
// encoded once however many callers ask at the same time.
func (m *Manager) segmentSet(ctx context.Context, rt *Runtime, n int64) (map[string][]Sample, error) {
	inAd, err := m.inAd(ctx, rt, n)
	if err != nil {
		return nil, err
	}
	key := segKey{stream: rt.stream.ID, epoch: rt.epoch, n: n, inAd: inAd}
	if set, ok := m.cache.get(key); ok {
		m.hits.Add(1)
		return set, nil
	}
	m.misses.Add(1)
	v, err, _ := m.flight.Do(key.String(), func() (any, error) {
		if set, ok := m.cache.get(key); ok {
			return set, nil
		}
		set, err := m.encode(rt, n, inAd)
		if err != nil {
			return nil, err
		}
		m.cache.put(key, set)
		return set, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(map[string][]Sample), nil
}

// encode runs the encoder on the runtime's context, so a request that goes
// away does not cancel a build other callers share. It retries once, and
// records each failure and the retry.
func (m *Manager) encode(rt *Runtime, n int64, inAd bool) (map[string][]Sample, error) {
	start := m.now()
	set, err := m.enc.EncodeSegment(rt.ctx, rt.stream, n, inAd)
	if err != nil && rt.ctx.Err() == nil {
		m.record(rt, event.FFmpegFailed, n, map[string]any{"error": err.Error()})
		m.record(rt, event.SegmentRetried, n, nil)
		if set, err = m.enc.EncodeSegment(rt.ctx, rt.stream, n, inAd); err != nil && rt.ctx.Err() == nil {
			m.record(rt, event.FFmpegFailed, n, map[string]any{"error": err.Error()})
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: segment %d of %s: %w", ErrUnavailable, n, rt.stream.ID, err)
	}
	m.metrics.SegmentBuilt(rt.stream.ID, "video", m.now().Sub(start))
	return set, nil
}

// inAd reports whether segment n falls inside a break that is not cancelled.
func (m *Manager) inAd(ctx context.Context, rt *Runtime, n int64) (bool, error) {
	b, err := m.breakAt(ctx, rt, n)
	return b != nil, err
}

// breakAt returns the break segment n falls inside, nil if none or if it is
// cancelled.
func (m *Manager) breakAt(ctx context.Context, rt *Runtime, n int64) (*adbreak.Break, error) {
	breaks, err := m.sched.InRange(ctx, rt.stream.ID, rt.epoch, n, n+1)
	if err != nil {
		return nil, fmt.Errorf("live: breaks of segment %d of %s: %w", n, rt.stream.ID, err)
	}
	for _, b := range breaks {
		if !b.Cancelled && b.StartSeg <= n && n < b.EndSeg {
			return &b, nil
		}
	}
	return nil, nil
}

// cues returns the cues of text track t in segment n: one per second of the
// burnt-in clock, cut at the segment's bounds, so they cover it back to
// back. A cue reads "FR · SUB · 18:30:05 · frame 1250 · Bip!": the clock
// second, its first frame and its beep, as the picture shows them. In a
// break it starts with "[AD k/D]": clock second k of the D seconds the
// planned break touches.
func (m *Manager) cues(ctx context.Context, rt *Runtime, t Track, n int64) ([]Cue, error) {
	br, err := m.breakAt(ctx, rt, n)
	if err != nil {
		return nil, err
	}
	const second = timeline.VideoTimescale
	tl := rt.Timeline()
	start, end := tl.VideoTfdt(n), tl.VideoTfdt(n+1)
	// The picture takes the zone offset at the segment start (see clockText
	// in liveffmpeg), so the cues do too.
	_, off := tl.SegStart(n).In(rt.loc).Zone()
	zone := time.FixedZone("", off)
	lang := strings.ToUpper(t.Lang)

	first, last := start/second, (end-1)/second
	cues := make([]Cue, 0, last-first+1)
	for s := first; s <= last; s++ {
		text := fmt.Sprintf("%s%s · SUB · %s · frame %d · %s", adPrefix(tl, br, s), lang, clockAt(tl, zone, s), s*int64(tl.FPS), beep(s))
		cues = append(cues, Cue{Start: max(s*second, start), End: min((s+1)*second, end), Text: text})
	}
	return cues, nil
}

// captions returns the CEA-608 captions the frames of video segment n show
// or load: a caption is loaded during the second before it shows, so they
// run from the second after the segment's first frame to the one after its
// last. Caption s reads "[AD k/D] CC1 - 18:30:05" over "frame 1250 - Bip!",
// with the break and the zone offset of the segment holding its first frame,
// as cues and the picture have them.
//
// Loading reaches into the next segments (at most two, since segments last
// at least 0.5 s), but no further than the frozen horizon, which is at least
// two segments past any published segment: the breaks a caption depends on
// are settled whenever a segment carrying it is served, so its bytes never
// change.
func (m *Manager) captions(ctx context.Context, rt *Runtime, channel string, n int64) ([]Caption, error) {
	tl := rt.Timeline()
	fps := int64(tl.FPS)
	first, last := tl.FirstFrame(n)/fps+1, (tl.FirstFrame(n+1)-1)/fps+1
	out := make([]Caption, 0, last-first+1)
	for s := first; s <= last; s++ {
		seg := s * fps / tl.SegFrames
		br, err := m.breakAt(ctx, rt, seg)
		if err != nil {
			return nil, err
		}
		_, off := tl.SegStart(seg).In(rt.loc).Zone()
		row1 := fmt.Sprintf("%s%s - %s", adPrefix(tl, br, s), channel, clockAt(tl, time.FixedZone("", off), s))
		out = append(out, Caption{Second: s, Rows: [2]string{row1, fmt.Sprintf("frame %d - %s", s*fps, beep(s))}})
	}
	return out, nil
}

// adPrefix is "[AD k/D] " for clock second s inside br: second k of the D
// clock seconds the planned break touches; "" outside a break.
func adPrefix(tl timeline.Timeline, br *adbreak.Break, s int64) string {
	if br == nil {
		return ""
	}
	const second = timeline.VideoTimescale
	from := tl.VideoTfdt(br.StartSeg) / second
	total := (tl.VideoTfdt(br.PlannedEndSeg)+second-1)/second - from
	return fmt.Sprintf("[AD %d/%d] ", s-from+1, total)
}

// clockAt is the burnt-in clock of second s, hh:mm:ss in zone.
func clockAt(tl timeline.Timeline, zone *time.Location, s int64) string {
	return tl.AST.Add(time.Duration(s) * time.Second).In(zone).Format("15:04:05")
}

// beep is what second s sounds: Bip! on even seconds, Bop! on odd ones.
func beep(s int64) string {
	if s%2 == 1 {
		return "Bop!"
	}
	return "Bip!"
}
