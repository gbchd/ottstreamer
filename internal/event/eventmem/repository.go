// Package eventmem implements event.Repository in memory: one ring buffer
// per stream, the oldest events dropped first.
package eventmem

import (
	"context"
	"sync"

	"ottstreamer/internal/event"
)

var _ event.Repository = (*Repository)(nil)

// Repository keeps the last size events of each stream. A stream's ring
// outlives the stream, as its audit trail.
//
// ponytail: rings of deleted streams are never freed, so memory grows with
// every stream id ever used; sweep old rings if ids churn.
type Repository struct {
	size  int
	mu    sync.Mutex
	rings map[string]*ring
}

type ring struct {
	events []event.Event
	start  int // index of the oldest event once full
}

// NewRepository returns a Repository keeping size events per stream.
func NewRepository(size int) *Repository {
	return &Repository{size: max(size, 1), rings: make(map[string]*ring)}
}

// Append adds e to its stream's ring, dropping the oldest event when full.
func (r *Repository) Append(_ context.Context, e event.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rg := r.rings[e.StreamID]
	if rg == nil {
		rg = &ring{}
		r.rings[e.StreamID] = rg
	}
	if len(rg.events) < r.size {
		rg.events = append(rg.events, e)
		return nil
	}
	rg.events[rg.start] = e
	rg.start = (rg.start + 1) % r.size
	return nil
}

// List returns up to q.Limit (all when 0) of the stream's events matching q,
// oldest first.
func (r *Repository) List(_ context.Context, streamID string, q event.Query) ([]event.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rg := r.rings[streamID]
	if rg == nil {
		return nil, nil
	}
	var out []event.Event
	for i := range rg.events {
		e := rg.events[(rg.start+i)%len(rg.events)]
		if !q.Match(e) {
			continue
		}
		out = append(out, e)
		if len(out) == q.Limit {
			break
		}
	}
	return out, nil
}
