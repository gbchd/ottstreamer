package live

import (
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func segments(calls []encodeCall) []int64 {
	ns := make([]int64, 0, len(calls))
	for _, c := range calls {
		ns = append(ns, c.n)
	}
	slices.Sort(ns)
	return ns
}

func TestManager_step(t *testing.T) {
	h := newHarness(nil)
	rt := h.add(t, running("a", ast))

	h.clock.set(ast.Add(10 * time.Second)) // live edge 4, frozen horizon 8
	next, finished := h.m.step(t.Context(), rt, 5)
	if next != 9 || finished {
		t.Fatalf("step = %d, %t; want 9, false", next, finished)
	}
	if got := segments(h.enc.encoded()); !slices.Equal(got, []int64{5, 6, 7, 8}) {
		t.Fatalf("encoded %v, want 5..8", got)
	}

	h.m.End("a", 10)
	h.clock.set(ast.Add(30 * time.Second)) // horizon capped at 10
	next, finished = h.m.step(t.Context(), rt, next)
	if next != 11 || !finished {
		t.Fatalf("step = %d, %t; want 11, true", next, finished)
	}
	if got := segments(h.enc.encoded()); !slices.Equal(got, []int64{5, 6, 7, 8, 9, 10}) {
		t.Fatalf("encoded %v, want 5..10", got)
	}
	if !strings.Contains(h.logs.String(), "segment.late") {
		t.Fatalf("segments 9 and 10 finished after their publication, no segment.late in %q", h.logs)
	}
	if built, late := h.metrics.counts(); built != 6 || late != 2 {
		t.Fatalf("metrics built %d, late %d; want 6, 2", built, late)
	}
}

func TestManager_produceUpTo_parallelism(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(nil)
		rt := h.add(t, running("a", ast))
		h.enc.gate = make(chan struct{})
		done := make(chan struct{})
		go func() {
			h.m.produceUpTo(t.Context(), rt, 0, 5)
			close(done)
		}()
		synctest.Wait()
		h.enc.mu.Lock()
		inflight := h.enc.inflight
		h.enc.mu.Unlock()
		if inflight != 2 {
			t.Fatalf("%d encodes in flight, want parallelism 2", inflight)
		}
		close(h.enc.gate)
		<-done
		if got := segments(h.enc.encoded()); !slices.Equal(got, []int64{0, 1, 2, 3, 4, 5}) {
			t.Fatalf("encoded %v, want 0..5", got)
		}
		if h.enc.maxInflight != 2 {
			t.Fatalf("max %d in flight, want 2", h.enc.maxInflight)
		}
	})
}

func TestManager_produceOne_failure(t *testing.T) {
	h := newHarness(nil)
	rt := h.add(t, running("a", ast))
	h.enc.fail = 2

	h.m.produceOne(t.Context(), rt, 3)
	if got := len(h.enc.encoded()); got != 2 {
		t.Fatalf("%d encode attempts, want 2 (one retry)", got)
	}
	if !strings.Contains(h.logs.String(), "segment.encode_failed") {
		t.Fatalf("no segment.encode_failed in %q", h.logs)
	}
	// A later request tries again on demand.
	if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", 3); err != nil {
		t.Fatal(err)
	}
}
