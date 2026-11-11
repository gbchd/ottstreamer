// Package eventtest holds a fake for the packages that record events.
package eventtest

import (
	"context"
	"sync"

	"ottstreamer/internal/event"
)

// Recorder keeps every event recorded, with the actor of its context. It is
// safe for concurrent use; the zero value is ready.
type Recorder struct {
	mu     sync.Mutex
	events []event.Event
}

// Record keeps e.
func (r *Recorder) Record(ctx context.Context, e event.Event) {
	e.Actor = event.Actor(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Events returns the events recorded so far and forgets them.
func (r *Recorder) Events() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

// Types returns the types of the events recorded so far and forgets them.
func (r *Recorder) Types() []event.Type {
	events := r.Events()
	out := make([]event.Type, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}
