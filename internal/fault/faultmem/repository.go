// Package faultmem stores the active fault rules of each stream in memory.
package faultmem

import (
	"context"
	"sync"

	"ottstreamer/internal/fault"
)

var _ fault.Repository = (*Repository)(nil)

// Repository is an in-memory fault.Repository. Rules go in and out as deep
// copies.
type Repository struct {
	mu    sync.RWMutex
	rules map[string][]fault.Rule
}

// NewRepository returns an empty Repository.
func NewRepository() *Repository {
	return &Repository{rules: map[string][]fault.Rule{}}
}

func (r *Repository) Get(_ context.Context, streamID string) ([]fault.Rule, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return fault.Clone(r.rules[streamID]), nil
}

func (r *Repository) Put(_ context.Context, streamID string, rules []fault.Rule) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(rules) == 0 {
		delete(r.rules, streamID)
		return nil
	}
	r.rules[streamID] = fault.Clone(rules)
	return nil
}
