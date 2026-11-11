package fault

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"ottstreamer/internal/event"
)

// Repository stores the active rules of each stream. Stored values share no
// memory with callers.
type Repository interface {
	// Get returns the rules of the stream, nil when it has none.
	Get(ctx context.Context, streamID string) ([]Rule, error)
	// Put replaces the rules of the stream; an empty list removes them.
	Put(ctx context.Context, streamID string, rules []Rule) error
}

// Events records rule changes and what was injected.
type Events interface {
	Record(ctx context.Context, e event.Event)
}

// Metrics counts injected faults.
type Metrics interface {
	FaultInjected(streamID, rule, action string)
}

// Injection is what the rules hitting one request do.
type Injection struct {
	Action Action   // merged in rule order: an earlier rule wins per action
	Rules  []string // the IDs of the rules that hit, in order
}

// Hit reports whether any rule hit.
func (i Injection) Hit() bool { return len(i.Rules) > 0 }

// bucket counts the hits of one rule in one minute.
type bucket struct {
	streamID string
	epoch    int
	rule     string
	actions  string // comma-separated Action.Names
	minute   time.Time
}

// Service keeps the active rules of each stream and decides which requests
// they hit.
type Service struct {
	repo    Repository
	events  Events
	metrics Metrics
	now     func() time.Time
	draw    func() int // uniform in [0, 100), for transient rules

	mu   sync.Mutex
	hits map[bucket]int
}

// NewService returns a Service. draw returns a uniform random int in
// [0, 100).
func NewService(repo Repository, events Events, metrics Metrics, now func() time.Time, draw func() int) *Service {
	return &Service{repo: repo, events: events, metrics: metrics, now: now, draw: draw, hits: map[bucket]int{}}
}

// Rules returns the active rules of a stream, in order, without the expired
// ones.
func (s *Service) Rules(ctx context.Context, streamID string) ([]Rule, error) {
	rules, err := s.repo.Get(ctx, streamID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	// ponytail: expired rules are hidden, not deleted, and no event marks
	// their expiry; a janitor can prune them and record faults.updated.
	return slices.DeleteFunc(rules, func(r Rule) bool {
		return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
	}), nil
}

// Replace validates rules and makes them the active rules of the stream at
// once. A rule with a TTL expires that long from now. It records
// faults.updated unless the stream had no rules and still has none.
func (s *Service) Replace(ctx context.Context, streamID string, rules []Rule) ([]Rule, error) {
	rules, err := Normalized(rules)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for i := range rules {
		rules[i].ExpiresAt = time.Time{}
		if rules[i].TTL > 0 {
			rules[i].ExpiresAt = now.Add(rules[i].TTL)
		}
	}
	old, err := s.Rules(ctx, streamID)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Put(ctx, streamID, rules); err != nil {
		return nil, err
	}
	if len(old) > 0 || len(rules) > 0 {
		ids := make([]string, len(rules))
		for i, r := range rules {
			ids[i] = r.ID
		}
		s.events.Record(ctx, event.Event{StreamID: streamID, Type: event.FaultsUpdated, Data: map[string]any{"rules": ids}})
	}
	return Clone(rules), nil
}

// Clear removes every rule of the stream.
func (s *Service) Clear(ctx context.Context, streamID string) error {
	_, err := s.Replace(ctx, streamID, nil)
	return err
}

// Inject returns what rules do to request t, and counts each rule that hits
// in faults_injected_total and in the per-minute fault.injected events.
// rules are the stream's active rules, plus any from the request's URL.
func (s *Service) Inject(_ context.Context, rules []Rule, t Target) Injection {
	var in Injection
	for _, r := range rules {
		if !r.matches(t) {
			continue
		}
		hit := r.sticky(t)
		if r.Mode == ModeTransient {
			hit = s.draw() < r.Percent
		}
		if !hit {
			continue
		}
		in.Action = in.Action.merge(r.Action)
		in.Rules = append(in.Rules, r.ID)
		names := r.Action.Names()
		for _, n := range names {
			s.metrics.FaultInjected(t.StreamID, r.ID, n)
		}
		b := bucket{streamID: t.StreamID, epoch: t.Epoch, rule: r.ID, actions: strings.Join(names, ","), minute: s.now().Truncate(time.Minute)}
		s.mu.Lock()
		s.hits[b]++
		s.mu.Unlock()
	}
	return in
}

// Flush records one fault.injected event per rule and minute for every
// minute that is over, with the number of hits.
func (s *Service) Flush(ctx context.Context) {
	current := s.now().Truncate(time.Minute)
	s.mu.Lock()
	var done []bucket
	counts := map[bucket]int{}
	for b, n := range s.hits {
		if b.minute.Before(current) {
			done = append(done, b)
			counts[b] = n
			delete(s.hits, b)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(done, func(a, b bucket) int { return a.minute.Compare(b.minute) })
	for _, b := range done {
		s.events.Record(ctx, event.Event{StreamID: b.streamID, Epoch: b.epoch, Type: event.FaultInjected, Data: map[string]any{
			"rule": b.rule, "actions": strings.Split(b.actions, ","), "count": counts[b], "minute": b.minute,
		}})
	}
}
