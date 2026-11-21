package live

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/event"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/timeline"
)

// Schedule is the ad-break schedule the media depends on.
type Schedule interface {
	// InRange returns the breaks overlapping segments [from, to), cancelled
	// ones included.
	InRange(ctx context.Context, streamID string, epoch int, from, to int64) ([]adbreak.Break, error)
	// Signals returns the in-band SCTE-35 messages of video segment n.
	Signals(ctx context.Context, streamID string, epoch int, n int64) ([]adbreak.Signal, error)
	// Crossed returns the break and signal events of publishing segment n.
	Crossed(ctx context.Context, streamID string, epoch int, n int64) ([]event.Event, error)
}

// Events records what the runtime sees.
type Events interface {
	Record(ctx context.Context, e event.Event)
}

// Metrics counts the segments the runtime builds.
type Metrics interface {
	// SegmentBuilt records one encoded segment set of a track kind (video).
	SegmentBuilt(streamID, kind string, took time.Duration)
	// SegmentLate records a segment the producer finished after it was published.
	SegmentLate(streamID string)
}

// Manager owns one Runtime per running or ended stream.
type Manager struct {
	enc          Encoder
	w            SegmentWriter
	sched        Schedule
	events       Events
	parallelism  int
	metrics      Metrics
	now          func() time.Time
	logger       *slog.Logger
	cache        *cache
	flight       singleflight.Group
	hits, misses atomic.Int64

	mu       sync.Mutex
	runtimes map[string]*Runtime
}

// NewManager returns a Manager whose segment cache holds up to cacheBytes of
// encoded video and whose producers encode up to parallelism segments of a
// stream at once.
func NewManager(enc Encoder, w SegmentWriter, sched Schedule, events Events, cacheBytes int64, parallelism int, metrics Metrics, now func() time.Time, logger *slog.Logger) *Manager {
	return &Manager{
		enc:         enc,
		w:           w,
		sched:       sched,
		events:      events,
		parallelism: max(parallelism, 1),
		metrics:     metrics,
		now:         now,
		logger:      logger,
		cache:       newCache(cacheBytes),
		runtimes:    make(map[string]*Runtime),
	}
}

// CacheStats returns the bytes the segment cache holds and its hit and miss
// counts since start.
func (m *Manager) CacheStats() (bytes, hits, misses int64) {
	return m.cache.bytes(), m.hits.Load(), m.misses.Load()
}

// Late returns the streams whose producer is more than maxSegs segments
// behind the live edge, sorted.
func (m *Manager) Late(maxSegs int64) []string {
	m.mu.Lock()
	all := make([]*Runtime, 0, len(m.runtimes))
	for _, rt := range m.runtimes {
		all = append(all, rt)
	}
	m.mu.Unlock()
	now := m.now()
	var late []string
	for _, rt := range all {
		select {
		case <-rt.done: // fully produced
			continue
		default:
		}
		if rt.Timeline().LiveEdge(now)-rt.produced.Load() > maxSegs {
			late = append(late, rt.stream.ID)
		}
	}
	slices.Sort(late)
	return late
}

// Runtime is the in-process media side of one epoch. It is safe for
// concurrent use.
type Runtime struct {
	stream stream.Stream // launch snapshot
	epoch  int
	tl     timeline.Timeline
	loc    *time.Location // of the burnt-in clock, which subtitles repeat
	endSeg atomic.Int64
	tracks map[string]Track

	produced atomic.Int64 // the producer has encoded every segment up to this one

	ctx    context.Context // lives until the runtime is dropped
	cancel context.CancelFunc
	done   chan struct{} // closed when the producer exits
}

// record records an event of the runtime's epoch at segment n.
func (m *Manager) record(rt *Runtime, t event.Type, n int64, data map[string]any) {
	m.events.Record(rt.ctx, event.Event{StreamID: rt.stream.ID, Epoch: rt.epoch, Segment: new(n), Type: t, Data: data})
}

// Stream returns a copy of the stream as it was launched.
func (rt *Runtime) Stream() stream.Stream {
	return rt.stream.Clone()
}

// Timeline returns the epoch's segment math; EndSeg reflects End.
func (rt *Runtime) Timeline() timeline.Timeline {
	tl := rt.tl
	tl.EndSeg = rt.endSeg.Load()
	return tl
}

// Track returns the track of a rendition of the epoch. Its Init and Loop are
// shared: callers must not modify them.
func (rt *Runtime) Track(id string) (Track, bool) {
	t, ok := rt.tracks[id]
	return t, ok
}

// Runtime returns the runtime of a stream.
func (m *Manager) Runtime(id string) (*Runtime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.runtimes[id]
	return rt, ok
}

// Launch builds the runtime of s's current epoch and starts its producer,
// replacing any runtime the stream already has.
func (m *Manager) Launch(ctx context.Context, s stream.Stream) error {
	m.Drop(s.ID)
	rt, err := m.newRuntime(ctx, s)
	if err != nil {
		return err
	}
	m.mu.Lock()
	old := m.runtimes[s.ID] // a concurrent Launch of the same stream
	m.runtimes[s.ID] = rt
	m.mu.Unlock()
	if old != nil {
		m.stop(old)
	}
	go m.produce(rt)
	return nil
}

// newRuntime builds the tracks of s's current epoch. The runtime outlives
// ctx: it lives until it is stopped, and what it records is the system's.
func (m *Manager) newRuntime(ctx context.Context, s stream.Stream) (*Runtime, error) {
	e := s.Status.Epoch
	if e == nil {
		return nil, fmt.Errorf("live: launch %s: no epoch", s.ID)
	}
	s = s.Clone()
	tracks, err := m.enc.StartEpoch(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("live: start epoch of %s: %w", s.ID, err)
	}
	if err := checkTracks(e.Spec, tracks); err != nil {
		return nil, fmt.Errorf("live: start epoch of %s: %w", s.ID, err)
	}
	for _, sub := range e.Spec.Subtitles {
		codec := CodecWebVTT
		if sub.Format == stream.SubtitleIMSC1 {
			codec = CodecIMSC1
		}
		init, err := m.w.TextInit(codec, sub.Lang)
		if err != nil {
			return nil, fmt.Errorf("live: start epoch of %s: subtitles %s: %w", s.ID, sub.ID, err)
		}
		tracks[sub.ID] = Track{ID: sub.ID, Kind: KindText, Timescale: timeline.VideoTimescale, Codec: codec, Lang: sub.Lang, Init: init}
	}
	loc, err := time.LoadLocation(e.Spec.Timezone)
	if err != nil {
		return nil, fmt.Errorf("live: start epoch of %s: %w", s.ID, err)
	}
	rctx, cancel := context.WithCancel(event.WithActor(context.WithoutCancel(ctx), event.System))
	rt := &Runtime{
		stream: s,
		epoch:  e.Number,
		tl:     e.Timeline(),
		loc:    loc,
		tracks: tracks,
		ctx:    rctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
	rt.endSeg.Store(rt.tl.EndSeg)
	return rt, nil
}

func checkTracks(spec stream.Spec, tracks map[string]Track) error {
	for _, v := range spec.Video {
		if t, ok := tracks[v.ID]; !ok || t.Kind != KindVideo {
			return fmt.Errorf("no video track %s", v.ID)
		}
	}
	for _, a := range spec.Audio {
		if t, ok := tracks[a.ID]; !ok || t.Kind != KindAudio || t.FrameSize <= 0 || len(t.Loop) == 0 {
			return fmt.Errorf("no audio loop for %s", a.ID)
		}
	}
	return nil
}

// End makes the runtime stop producing after segment endSeg.
func (m *Manager) End(id string, endSeg int64) {
	if rt, ok := m.Runtime(id); ok {
		rt.endSeg.Store(endSeg)
	}
}

// Drop stops the runtime of a stream and purges its cached segments.
func (m *Manager) Drop(id string) {
	m.mu.Lock()
	rt := m.runtimes[id]
	delete(m.runtimes, id)
	m.mu.Unlock()
	if rt != nil {
		m.stop(rt)
	}
}

// Close stops every runtime and waits for its producer to exit.
func (m *Manager) Close() {
	m.mu.Lock()
	all := m.runtimes
	m.runtimes = make(map[string]*Runtime)
	m.mu.Unlock()
	for _, rt := range all {
		rt.cancel()
	}
	for _, rt := range all {
		m.stop(rt)
	}
}

func (m *Manager) stop(rt *Runtime) {
	rt.cancel()
	<-rt.done
	m.cache.purge(rt.stream.ID, rt.epoch)
}
