package event

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Repository stores the log of every stream, in the order of appends.
type Repository interface {
	Append(ctx context.Context, e Event) error
	// List returns up to q.Limit events of the stream matching q, oldest first.
	List(ctx context.Context, streamID string, q Query) ([]Event, error)
}

const (
	// MaxLimit is the largest page List returns.
	MaxLimit     = 1000
	defaultLimit = 100
	// subBuffer is how far a subscriber may fall behind before it is cut off.
	subBuffer = 256
)

// Service records events and fans them out to subscribers. It is safe for
// concurrent use.
type Service struct {
	repo    Repository
	now     func() time.Time
	newID   func(time.Time) string
	observe func(Event)
	logger  *slog.Logger

	// ponytail: one lock around ID, append and fan-out keeps the three in
	// the same order, so a resumed subscriber misses nothing. It serializes
	// every append: per-stream locks once appends go to a database.
	mu     sync.Mutex
	subs   map[*sub]struct{}
	closed bool
}

type sub struct {
	streamID string
	ch       chan Event
}

// NewService returns a Service. newID makes increasing IDs for a time, and
// observe sees every recorded event, synchronously.
func NewService(repo Repository, now func() time.Time, newID func(time.Time) string, observe func(Event), logger *slog.Logger) *Service {
	return &Service{repo: repo, now: now, newID: newID, observe: observe, logger: logger, subs: make(map[*sub]struct{})}
}

// Record stamps e with an ID, the time and the actor of ctx, and appends it.
// A failed append is logged: the log never fails the change it records.
func (s *Service) Record(ctx context.Context, e Event) {
	s.mu.Lock()
	e.At = s.now()
	e.ID = s.newID(e.At)
	e.Actor = Actor(ctx)
	err := s.repo.Append(ctx, e)
	if err == nil {
		for sb := range s.subs {
			if sb.streamID != e.StreamID {
				continue
			}
			select {
			case sb.ch <- e:
			default:
				s.drop(sb)
			}
		}
	}
	s.mu.Unlock()
	if err != nil {
		s.logger.ErrorContext(ctx, "append event", "stream", e.StreamID, "type", e.Type, "error", err)
		return
	}
	s.observe(e)
}

// List returns a page of the stream's events matching q, oldest first, and
// the cursor of the next page, "" after the last. q.Limit defaults to 100
// and is capped at MaxLimit.
func (s *Service) List(ctx context.Context, streamID string, q Query) ([]Event, string, error) {
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	q.Limit = min(q.Limit, MaxLimit)
	events, err := s.repo.List(ctx, streamID, q)
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(events) == q.Limit {
		next = events[len(events)-1].ID
	}
	return events, next, nil
}

// Subscribe returns the stream's events recorded from now on. The channel
// closes when ctx is done, on Close, or when the subscriber falls too far
// behind; it can then catch up with List after the last ID it saw.
func (s *Service) Subscribe(ctx context.Context, streamID string) <-chan Event {
	sb := &sub{streamID: streamID, ch: make(chan Event, subBuffer)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		close(sb.ch)
		return sb.ch
	}
	s.subs[sb] = struct{}{}
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.drop(sb)
	})
	return sb.ch
}

// Close ends every subscription, and those made later, so that streaming
// responses finish when the server shuts down.
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for sb := range s.subs {
		s.drop(sb)
	}
}

// drop ends a subscription. s.mu must be held.
func (s *Service) drop(sb *sub) {
	if _, ok := s.subs[sb]; ok {
		delete(s.subs, sb)
		close(sb.ch)
	}
}
