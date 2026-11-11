// Package adbreakmem stores ad breaks in memory.
package adbreakmem

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"ottstreamer/internal/adbreak"
)

var _ adbreak.Repository = (*Repository)(nil)

type epochKey struct {
	streamID string
	epoch    int
}

// Repository is an in-memory adbreak.Repository. Breaks go in and out as
// deep copies.
type Repository struct {
	mu     sync.RWMutex
	epochs map[epochKey]map[string]adbreak.Break // by break ID
}

// NewRepository returns an empty Repository.
func NewRepository() *Repository {
	return &Repository{epochs: map[epochKey]map[string]adbreak.Break{}}
}

func (r *Repository) List(_ context.Context, streamID string, epoch int) ([]adbreak.Break, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byID := r.epochs[epochKey{streamID, epoch}]
	out := make([]adbreak.Break, 0, len(byID))
	for _, b := range byID {
		out = append(out, clone(b))
	}
	slices.SortFunc(out, func(a, b adbreak.Break) int {
		return cmp.Or(cmp.Compare(a.StartSeg, b.StartSeg), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

func (r *Repository) Save(_ context.Context, b adbreak.Break) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := epochKey{b.StreamID, b.Epoch}
	if r.epochs[k] == nil {
		r.epochs[k] = map[string]adbreak.Break{}
	}
	r.epochs[k][b.ID] = clone(b)
	return nil
}

func (r *Repository) DeleteAll(_ context.Context, streamID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.epochs {
		if k.streamID == streamID {
			delete(r.epochs, k)
		}
	}
	return nil
}

func clone(b adbreak.Break) adbreak.Break {
	b.CueOut, b.CueIn, b.Cancel = slices.Clone(b.CueOut), slices.Clone(b.CueIn), slices.Clone(b.Cancel)
	return b
}
