package live

import (
	"context"
	"fmt"
	"io"

	"ottstreamer/internal/timeline"
)

// Init returns the CMAF header of a rendition.
func (m *Manager) Init(streamID, rendition string) ([]byte, error) {
	rt, ok := m.Runtime(streamID)
	if !ok {
		return nil, fmt.Errorf("%w: stream %s", ErrNotFound, streamID)
	}
	t, ok := rt.Track(rendition)
	if !ok {
		return nil, fmt.Errorf("%w: rendition %s of %s", ErrNotFound, rendition, streamID)
	}
	return t.Init, nil
}

// WriteSegment writes media segment n of a rendition to w. Video comes from
// the cache or is encoded on demand; audio is sliced from the track's loop.
// It does not check n against the live window.
func (m *Manager) WriteSegment(ctx context.Context, w io.Writer, streamID, rendition string, n int64) error {
	rt, ok := m.Runtime(streamID)
	if !ok {
		return fmt.Errorf("%w: stream %s", ErrNotFound, streamID)
	}
	t, ok := rt.Track(rendition)
	if !ok {
		return fmt.Errorf("%w: rendition %s of %s", ErrNotFound, rendition, streamID)
	}
	tl := rt.Timeline()
	if n < 0 || (tl.EndSeg >= 0 && n > tl.EndSeg) {
		return fmt.Errorf("%w: segment %d of %s", ErrNotFound, n, streamID)
	}
	f, err := m.fragment(ctx, rt, t, n)
	if err != nil {
		return err
	}
	return m.w.WriteSegment(w, f)
}

func (m *Manager) fragment(ctx context.Context, rt *Runtime, t Track, n int64) (Fragment, error) {
	tl := rt.Timeline()
	f := Fragment{Seq: uint32(n + 1), TrackID: 1} //nolint:gosec // mfhd sequence_number is 32-bit and wraps
	if !t.Video {
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
		return set, nil
	}
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
// away does not cancel a build other callers share. It retries once.
func (m *Manager) encode(rt *Runtime, n int64, inAd bool) (map[string][]Sample, error) {
	set, err := m.enc.EncodeSegment(rt.ctx, rt.stream, n, inAd)
	if err != nil && rt.ctx.Err() == nil {
		set, err = m.enc.EncodeSegment(rt.ctx, rt.stream, n, inAd)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: segment %d of %s: %w", ErrUnavailable, n, rt.stream.ID, err)
	}
	return set, nil
}

// inAd reports whether segment n falls inside a break that is not cancelled.
func (m *Manager) inAd(ctx context.Context, rt *Runtime, n int64) (bool, error) {
	breaks, err := m.sched.InRange(ctx, rt.stream.ID, rt.epoch, n, n+1)
	if err != nil {
		return false, fmt.Errorf("live: breaks of segment %d of %s: %w", n, rt.stream.ID, err)
	}
	for _, b := range breaks {
		if !b.Cancelled && b.StartSeg <= n && n < b.EndSeg {
			return true, nil
		}
	}
	return false, nil
}
