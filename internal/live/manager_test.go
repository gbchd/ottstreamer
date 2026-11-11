package live

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streamtest"
)

var _ stream.Runtimes = (*Manager)(nil)

var ast = time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)

// running returns stream id in its first epoch: 720p (3000 kbit/s) and
// 360p video, AAC audio "en", 2 s segments at 25 fps, 8 s lead (4 segments).
func running(id string, start time.Time) stream.Stream {
	s := streamtest.Stream(id)
	s.Status = stream.Status{State: stream.StateRunning, Epoch: &stream.Epoch{Number: 1, AST: start, Spec: s.Spec.Clone()}}
	return s
}

type encodeCall struct {
	n    int64
	inAd bool
}

type fakeEncoder struct {
	mu          sync.Mutex
	calls       []encodeCall
	fail        int           // fail the next fail calls
	gate        chan struct{} // when set, EncodeSegment waits for it to close
	inflight    int
	maxInflight int
}

func (e *fakeEncoder) StartEpoch(_ context.Context, s stream.Stream) (map[string]Track, error) {
	tracks := make(map[string]Track)
	for _, v := range s.Status.Epoch.Spec.Video {
		tracks[v.ID] = Track{ID: v.ID, Video: true, Timescale: 90000, Init: []byte("init " + v.ID)}
	}
	for _, a := range s.Status.Epoch.Spec.Audio {
		loop := make([]Sample, 375)
		for i := range loop {
			loop[i] = Sample{Data: fmt.Appendf(nil, "frame %d", i)}
		}
		tracks[a.ID] = Track{ID: a.ID, Timescale: 48000, FrameSize: a.Codec.FrameSize(), Loop: loop, Init: []byte("init " + a.ID)}
	}
	return tracks, nil
}

func (e *fakeEncoder) EncodeSegment(ctx context.Context, s stream.Stream, n int64, inAd bool) (map[string][]Sample, error) {
	e.mu.Lock()
	e.calls = append(e.calls, encodeCall{n, inAd})
	e.inflight++
	e.maxInflight = max(e.maxInflight, e.inflight)
	fail := e.fail > 0
	if fail {
		e.fail--
	}
	gate := e.gate
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.inflight--
		e.mu.Unlock()
	}()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return nil, errors.New("ffmpeg exited with status 1")
	}
	set := make(map[string][]Sample)
	for _, v := range s.Status.Epoch.Spec.Video {
		samples := make([]Sample, s.Status.Epoch.Timeline().SegFrames)
		for i := range samples {
			samples[i] = Sample{Data: make([]byte, 100), Sync: i == 0}
		}
		set[v.ID] = samples
	}
	return set, nil
}

func (e *fakeEncoder) encoded() []encodeCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]encodeCall(nil), e.calls...)
}

type fakeWriter struct {
	mu    sync.Mutex
	frags []Fragment
}

func (w *fakeWriter) WriteSegment(out io.Writer, f Fragment) error {
	w.mu.Lock()
	w.frags = append(w.frags, f)
	w.mu.Unlock()
	_, err := fmt.Fprintf(out, "segment %d", f.Seq)
	return err
}

func (w *fakeWriter) last() Fragment {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.frags[len(w.frags)-1]
}

type fakeSchedule struct {
	mu      sync.Mutex
	breaks  []adbreak.Break
	signals []adbreak.Signal
}

func (s *fakeSchedule) InRange(_ context.Context, _ string, _ int, from, to int64) ([]adbreak.Break, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []adbreak.Break
	for _, b := range s.breaks {
		if b.StartSeg < to && from < b.EndSeg {
			out = append(out, b)
		}
	}
	return out, nil
}

func (s *fakeSchedule) Signals(context.Context, string, int, int64) ([]adbreak.Signal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signals, nil
}

func (s *fakeSchedule) set(breaks ...adbreak.Break) {
	s.mu.Lock()
	s.breaks = breaks
	s.mu.Unlock()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

type harness struct {
	m     *Manager
	enc   *fakeEncoder
	w     *fakeWriter
	sched *fakeSchedule
	clock *clock
	logs  *bytes.Buffer
}

func newHarness(now func() time.Time) *harness {
	h := &harness{enc: &fakeEncoder{}, w: &fakeWriter{}, sched: &fakeSchedule{}, clock: &clock{t: ast}, logs: &bytes.Buffer{}}
	if now == nil {
		now = h.clock.now
	}
	logger := slog.New(slog.NewTextHandler(h.logs, nil))
	h.m = NewManager(h.enc, h.w, h.sched, 1<<20, 2, now, logger)
	return h
}

// add registers the runtime of s without starting its producer.
func (h *harness) add(t *testing.T, s stream.Stream) *Runtime {
	t.Helper()
	rt, err := h.m.newRuntime(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	close(rt.done)
	h.m.mu.Lock()
	h.m.runtimes[s.ID] = rt
	h.m.mu.Unlock()
	t.Cleanup(h.m.Close)
	return rt
}

func isDone(rt *Runtime) bool {
	select {
	case <-rt.done:
		return true
	default:
		return false
	}
}

func TestManager_Lifecycle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(time.Now) // the bubble's fake clock
		s := running("a", time.Now())
		if err := h.m.Launch(t.Context(), s); err != nil {
			t.Fatal(err)
		}
		rt, ok := h.m.Runtime("a")
		if !ok {
			t.Fatal("no runtime after Launch")
		}
		if got := rt.Stream(); got.ID != "a" || got.Status.Epoch.Number != 1 {
			t.Fatalf("Stream() = %s epoch %d", got.ID, got.Status.Epoch.Number)
		}

		synctest.Wait() // producer encodes 0..LeadSegs-1 and sleeps
		if got := len(h.enc.encoded()); got != 4 {
			t.Fatalf("encoded %d segments at AST, want 4 (the lead)", got)
		}
		time.Sleep(2 * time.Second) // segment 0 published: horizon moves to 4
		synctest.Wait()
		if got := len(h.enc.encoded()); got != 5 {
			t.Fatalf("encoded %d segments at AST+2s, want 5", got)
		}

		h.m.End("a", 6)
		if got := rt.Timeline().EndSeg; got != 6 {
			t.Fatalf("EndSeg = %d, want 6", got)
		}
		<-rt.done // the producer exits after the end segment
		calls := h.enc.encoded()
		if len(calls) != 7 || calls[len(calls)-1].n != 6 {
			t.Fatalf("encoded %v, want 0..6", calls)
		}

		h.m.Drop("a")
		if _, ok := h.m.Runtime("a"); ok {
			t.Fatal("runtime still there after Drop")
		}
		if _, ok := h.m.cache.get(segKey{stream: "a", epoch: 1, n: 6}); ok {
			t.Fatal("cache not purged by Drop")
		}
	})
}

func TestManager_LaunchReplaces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(time.Now)
		for range 2 {
			if err := h.m.Launch(t.Context(), running("a", time.Now())); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(h.m.runtimes); n != 1 {
			t.Fatalf("%d runtimes, want 1", n)
		}
		h.m.Close()
	})
}

// synctest fails the test if a producer outlives Close.
func TestManager_Close(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(time.Now)
		h.enc.gate = make(chan struct{}) // keep encodes in flight
		rts := make([]*Runtime, 0, 2)
		for _, id := range []string{"a", "b"} {
			if err := h.m.Launch(t.Context(), running(id, time.Now())); err != nil {
				t.Fatal(err)
			}
			rt, _ := h.m.Runtime(id)
			rts = append(rts, rt)
		}
		synctest.Wait()
		h.m.Close()
		for _, rt := range rts {
			if !isDone(rt) {
				t.Fatalf("producer of %s still running after Close", rt.stream.ID)
			}
		}
		if _, ok := h.m.Runtime("a"); ok {
			t.Fatal("runtime still there after Close")
		}
	})
}

func TestManager_LaunchWithoutEpoch(t *testing.T) {
	h := newHarness(nil)
	if err := h.m.Launch(t.Context(), streamtest.Stream("a")); err == nil {
		t.Fatal("Launch of a never started stream succeeded")
	}
	if _, ok := h.m.Runtime("a"); ok {
		t.Fatal("runtime registered after a failed Launch")
	}
}
