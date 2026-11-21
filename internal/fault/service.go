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

// Streams reads what the rules of a stream depend on.
type Streams interface {
	// Stream returns the zero Stream for an unknown stream.
	Stream(ctx context.Context, streamID string) (Stream, error)
}

// Stream is what fault rules know of a stream.
type Stream struct {
	Epoch      int      // the current epoch, 0 before the first start
	Renditions []string // the video, audio and subtitle rendition IDs of its spec
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
	streams Streams
	events  Events
	metrics Metrics
	now     func() time.Time
	draw    func() int // uniform in [0, 100), for transient rules

	// ponytail: one lock for every stream, in this process only; a versioned
	// Put once replicas share the Repository.
	change sync.Mutex // serializes rule changes, which read then write

	mu   sync.Mutex
	hits map[bucket]int
}

// NewService returns a Service. draw returns a uniform random int in
// [0, 100).
func NewService(repo Repository, streams Streams, events Events, metrics Metrics, now func() time.Time, draw func() int) *Service {
	return &Service{repo: repo, streams: streams, events: events, metrics: metrics, now: now, draw: draw, hits: map[bucket]int{}}
}

// Rules returns the active rules of a stream, in order, without the expired
// ones, which stay stored until Prune deletes them.
func (s *Service) Rules(ctx context.Context, streamID string) ([]Rule, error) {
	rules, err := s.repo.Get(ctx, streamID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	return slices.DeleteFunc(rules, func(r Rule) bool { return r.expired(now) }), nil
}

func (r Rule) expired(now time.Time) bool {
	return !r.ExpiresAt.IsZero() && !now.Before(r.ExpiresAt)
}

// Replace validates rules against the stream's renditions and makes them
// its active rules at once. A rule with a TTL expires that long from now. It
// records faults.updated in the stream's current epoch, unless the stream
// had no rules and still has none.
func (s *Service) Replace(ctx context.Context, streamID string, rules []Rule) ([]Rule, error) {
	st, err := s.streams.Stream(ctx, streamID)
	if err != nil {
		return nil, err
	}
	rules, err = NormalizedFor(rules, st.Renditions)
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
	s.change.Lock()
	defer s.change.Unlock()
	old, err := s.Rules(ctx, streamID)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Put(ctx, streamID, rules); err != nil {
		return nil, err
	}
	if len(old) > 0 || len(rules) > 0 {
		s.events.Record(ctx, event.Event{StreamID: streamID, Epoch: st.Epoch, Type: event.FaultsUpdated, Data: map[string]any{"rules": ids(rules)}})
	}
	return Clone(rules), nil
}

// Prune deletes the expired rules of a stream and records faults.updated by
// the system, with the IDs of the rules that remain and of those that
// expired. It records nothing when no rule expired.
func (s *Service) Prune(ctx context.Context, streamID string) error {
	s.change.Lock()
	defer s.change.Unlock()
	rules, err := s.repo.Get(ctx, streamID)
	if err != nil {
		return err
	}
	now := s.now()
	var expired []string
	rules = slices.DeleteFunc(rules, func(r Rule) bool {
		gone := r.expired(now)
		if gone {
			expired = append(expired, r.ID)
		}
		return gone
	})
	if len(expired) == 0 {
		return nil
	}
	st, err := s.streams.Stream(ctx, streamID)
	if err != nil {
		return err
	}
	if err := s.repo.Put(ctx, streamID, rules); err != nil {
		return err
	}
	s.events.Record(event.WithActor(ctx, event.System), event.Event{StreamID: streamID, Epoch: st.Epoch, Type: event.FaultsUpdated, Data: map[string]any{
		"rules": ids(rules), "expired": expired,
	}})
	return nil
}

// ids returns the IDs of rules, never nil.
func ids(rules []Rule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.ID
	}
	return out
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
