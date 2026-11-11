// Package streammem stores streams in memory.
package streammem

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"ottstreamer/internal/stream"
)

var (
	_ stream.Repository = (*Repository)(nil)
	_ stream.Deleter    = (*Repository)(nil)
)

// Repository is an in-memory stream.Repository and stream.Deleter. Streams go
// in and out as deep copies, so callers never share memory with the store.
type Repository struct {
	mu      sync.RWMutex
	streams map[string]stream.Stream
}

// NewRepository returns an empty Repository.
func NewRepository() *Repository {
	return &Repository{streams: map[string]stream.Stream{}}
}

func (r *Repository) Get(_ context.Context, id string) (stream.Stream, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.streams[id]
	if !ok {
		return stream.Stream{}, stream.ErrNotFound
	}
	return s.Clone(), nil
}

func (r *Repository) List(_ context.Context) ([]stream.Stream, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]stream.Stream, 0, len(r.streams))
	for _, s := range r.streams {
		out = append(out, s.Clone())
	}
	slices.SortFunc(out, func(a, b stream.Stream) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func (r *Repository) Save(_ context.Context, s stream.Stream, expectedVersion int) (stream.Stream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.streams[s.ID]
	switch {
	case expectedVersion == 0 && ok:
		return stream.Stream{}, stream.ErrExists
	case expectedVersion != 0 && !ok:
		return stream.Stream{}, stream.ErrNotFound
	case expectedVersion != 0 && cur.Version != expectedVersion:
		return stream.Stream{}, stream.ErrVersionMismatch
	}
	s = s.Clone()
	s.Version = expectedVersion + 1
	r.streams[s.ID] = s
	return s.Clone(), nil
}

func (r *Repository) Delete(_ context.Context, id string, expectedVersion int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.streams[id]
	if !ok {
		return stream.ErrNotFound
	}
	if expectedVersion != 0 && cur.Version != expectedVersion {
		return stream.ErrVersionMismatch
	}
	delete(r.streams, id)
	return nil
}
