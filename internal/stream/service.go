package stream

import (
	"context"
	"fmt"
	"reflect"
	"time"
)

// Repository stores streams.
type Repository interface {
	// Get returns ErrNotFound for an unknown id.
	Get(ctx context.Context, id string) (Stream, error)
	// List returns every stream, sorted by id.
	List(ctx context.Context) ([]Stream, error)
	// Save creates s when expectedVersion is 0 (ErrExists if the id is
	// taken), else replaces it if its stored version is expectedVersion
	// (ErrVersionMismatch otherwise). It bumps Version and returns the stored
	// stream. Stored values share no memory with callers.
	Save(ctx context.Context, s Stream, expectedVersion int) (Stream, error)
}

// Deleter removes streams.
type Deleter interface {
	// Delete removes the stream; expectedVersion 0 deletes unconditionally.
	Delete(ctx context.Context, id string, expectedVersion int) error
}

// Runtimes runs the media side of running and ended streams.
type Runtimes interface {
	// Launch builds the runtime of s's current epoch and starts producing.
	Launch(ctx context.Context, s Stream) error
	// End makes the runtime stop producing after segment endSeg.
	End(id string, endSeg int64)
	// Drop stops the runtime and releases everything it holds.
	Drop(id string)
}

// Breaks is the ad-break schedule of a stream.
type Breaks interface {
	// Trim cancels breaks starting after endSeg and cuts one active at it.
	Trim(ctx context.Context, streamID string, epoch int, endSeg int64) error
	// DeleteAll removes every break of the stream.
	DeleteAll(ctx context.Context, streamID string) error
}

// Service runs the stream use cases.
type Service struct {
	repo   Repository
	del    Deleter
	rt     Runtimes
	breaks Breaks
	now    func() time.Time
}

// NewService returns a Service.
func NewService(repo Repository, del Deleter, rt Runtimes, br Breaks, now func() time.Time) *Service {
	return &Service{repo: repo, del: del, rt: rt, breaks: br, now: now}
}

// Create validates and stores a new stream in state stopped. Only ID, Name
// and Spec of s are used.
func (svc *Service) Create(ctx context.Context, s Stream) (Stream, error) {
	var v ValidationError
	if !idPattern.MatchString(s.ID) {
		v.add("id", "must match [a-z0-9-]{1,40}")
	}
	spec := s.Spec.normalized(&v)
	if err := v.err(); err != nil {
		return Stream{}, err
	}
	now := svc.now()
	return svc.repo.Save(ctx, Stream{
		ID:        s.ID,
		Name:      s.Name,
		Spec:      spec,
		Status:    Status{State: StateStopped},
		CreatedAt: now,
		UpdatedAt: now,
	}, 0)
}

// Get returns one stream.
func (svc *Service) Get(ctx context.Context, id string) (Stream, error) {
	return svc.repo.Get(ctx, id)
}

// List returns every stream.
func (svc *Service) List(ctx context.Context) ([]Stream, error) {
	return svc.repo.List(ctx)
}

// Replace replaces name and spec if the stream is still at version ifMatch.
// The spec applies on the next start, except the window, which also applies
// to the running epoch at once.
func (svc *Service) Replace(ctx context.Context, id, name string, spec Spec, ifMatch int) (Stream, error) {
	if ifMatch == 0 {
		return Stream{}, ErrPreconditionRequired
	}
	var v ValidationError
	spec = spec.normalized(&v)
	if err := v.err(); err != nil {
		return Stream{}, err
	}
	s, err := svc.repo.Get(ctx, id)
	if err != nil {
		return Stream{}, err
	}
	if s.Version != ifMatch {
		return Stream{}, ErrVersionMismatch
	}
	s.Name, s.Spec, s.UpdatedAt = name, spec, svc.now()
	if e := s.Status.Epoch; e != nil {
		e.Spec.Window = spec.Window
		s.Status.PendingChanges = s.Status.State != StateStopped && !sameMedia(spec, e.Spec)
	}
	return svc.repo.Save(ctx, s, ifMatch)
}

// sameMedia reports whether a and b produce the same media: they may differ
// only in settings that apply immediately.
func sameMedia(a, b Spec) bool {
	a.Window, b.Window = Window{}, Window{}
	return reflect.DeepEqual(a, b)
}

// Delete removes a stream that is not running, with its runtime and breaks.
// ifMatch 0 skips the version check.
func (svc *Service) Delete(ctx context.Context, id string, ifMatch int) error {
	s, err := svc.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	if s.Status.State == StateRunning {
		return errConflict(s)
	}
	if ifMatch != 0 && ifMatch != s.Version {
		return ErrVersionMismatch
	}
	// Deleting at the version checked above fails if a start slipped in.
	if err := svc.del.Delete(ctx, id, s.Version); err != nil {
		return err
	}
	svc.rt.Drop(id)
	if err := svc.breaks.DeleteAll(ctx, id); err != nil {
		return fmt.Errorf("delete breaks of %s: %w", id, err)
	}
	return nil
}

// errConflict wraps ErrConflict with the stream's id and state.
func errConflict(s Stream) error {
	return fmt.Errorf("%w: %s is %s", ErrConflict, s.ID, s.Status.State)
}
