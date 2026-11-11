package live

import (
	"context"
	"sync"
	"time"
)

// produce encodes every segment of rt as soon as it is frozen, so players
// never wait for the encoder. Older segments are built on demand.
func (m *Manager) produce(rt *Runtime) {
	ctx := rt.ctx
	defer close(rt.done)
	next := max(0, rt.Timeline().LiveEdge(m.now())+1)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		var finished bool
		if next, finished = m.step(ctx, rt, next); finished {
			return
		}
		// The frozen horizon moves when the next segment is published.
		tl, now := rt.Timeline(), m.now()
		timer.Reset(tl.SegEnd(tl.LiveEdge(now) + 1).Sub(now)) // drops a pending tick (Go 1.23+)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

// step encodes segments next..FrozenHorizon(now) and returns the next
// segment to encode, and whether the epoch is fully produced.
func (m *Manager) step(ctx context.Context, rt *Runtime, next int64) (int64, bool) {
	tl := rt.Timeline()
	target := tl.FrozenHorizon(m.now())
	m.produceUpTo(ctx, rt, next, target)
	next = max(next, target+1)
	return next, tl.EndSeg >= 0 && next > tl.EndSeg
}

// produceUpTo encodes segments [from, to], up to m.parallelism at once.
func (m *Manager) produceUpTo(ctx context.Context, rt *Runtime, from, to int64) {
	sem := make(chan struct{}, m.parallelism)
	var wg sync.WaitGroup
	for n := from; n <= to && ctx.Err() == nil; n++ {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			m.produceOne(ctx, rt, n)
		})
	}
	wg.Wait()
}

// produceOne encodes segment n, or logs why it could not: a later request
// for it tries again on demand.
func (m *Manager) produceOne(ctx context.Context, rt *Runtime, n int64) {
	if _, err := m.segmentSet(ctx, rt, n); err != nil {
		if ctx.Err() == nil {
			m.logger.ErrorContext(ctx, "segment.encode_failed", "stream", rt.stream.ID, "epoch", rt.epoch, "segment", n, "err", err)
		}
		return
	}
	if late := m.now().Sub(rt.Timeline().SegEnd(n)); late > 0 {
		m.logger.WarnContext(ctx, "segment.late", "stream", rt.stream.ID, "epoch", rt.epoch, "segment", n, "late", late)
	}
}
