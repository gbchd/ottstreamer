package stream

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"time"

	"ottstreamer/internal/event"
	"ottstreamer/internal/fault"
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

// Faults holds the active fault rules of each stream.
type Faults interface {
	// Replace makes rules the active rules of the stream.
	Replace(ctx context.Context, streamID string, rules []fault.Rule) ([]fault.Rule, error)
	// Clear removes every active rule of the stream.
	Clear(ctx context.Context, streamID string) error
}

// Events records what happens to streams.
type Events interface {
	Record(ctx context.Context, e event.Event)
}

// Service runs the stream use cases.
type Service struct {
	repo   Repository
	del    Deleter
	rt     Runtimes
	breaks Breaks
	faults Faults
	events Events
	now    func() time.Time
}

// NewService returns a Service.
func NewService(repo Repository, del Deleter, rt Runtimes, br Breaks, faults Faults, events Events, now func() time.Time) *Service {
	return &Service{repo: repo, del: del, rt: rt, breaks: br, faults: faults, events: events, now: now}
}

// record records an event of s, in its current epoch if it has one.
func (svc *Service) record(ctx context.Context, s Stream, t event.Type, seg *int64, data map[string]any) {
	e := event.Event{StreamID: s.ID, Type: t, Segment: seg, Data: data}
	if s.Status.Epoch != nil {
		e.Epoch = s.Status.Epoch.Number
	}
	svc.events.Record(ctx, e)
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
	s, err := svc.repo.Save(ctx, Stream{
		ID:        s.ID,
		Name:      s.Name,
		Spec:      spec,
		Status:    Status{State: StateStopped},
		CreatedAt: now,
		UpdatedAt: now,
	}, 0)
	if err != nil {
		return Stream{}, err
	}
	svc.record(ctx, s, event.StreamCreated, nil, map[string]any{"name": s.Name})
	return s, nil
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
// to the running epoch at once. Faults never touch the active rules: they
// seed them at the next start.
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
	fields := changed(s, name, spec)
	s.Name, s.Spec, s.UpdatedAt = name, spec, svc.now()
	if e := s.Status.Epoch; e != nil {
		e.Spec.Window = spec.Window
		s.Status.PendingChanges = s.Status.State != StateStopped && !sameMedia(spec, e.Spec)
	}
	s, err = svc.repo.Save(ctx, s, ifMatch)
	if err != nil {
		return Stream{}, err
	}
	svc.record(ctx, s, event.StreamUpdated, nil, map[string]any{"fields": fields, "pendingChanges": s.Status.PendingChanges})
	return s, nil
}

// changed returns the paths, as in the stream resource, of the fields that
// a new name and spec change in s.
func changed(s Stream, name string, spec Spec) []string {
	a := s.Spec
	fields := []struct {
		path string
		same bool
	}{
		{"name", s.Name == name},
		{"spec.formats", slices.Equal(a.Formats, spec.Formats)},
		{"spec.segmentSeconds", a.SegmentDuration == spec.SegmentDuration},
		{"spec.gopSeconds", a.GOPDuration == spec.GOPDuration},
		{"spec.fps", a.FPS == spec.FPS},
		{"spec.timezone", a.Timezone == spec.Timezone},
		{"spec.overlay.renditionLabel", a.RenditionLabel == spec.RenditionLabel},
		{"spec.video", slices.Equal(a.Video, spec.Video)},
		{"spec.audio", slices.Equal(a.Audio, spec.Audio)},
		{"spec.subtitles", slices.Equal(a.Subtitles, spec.Subtitles)},
		{"spec.captions", reflect.DeepEqual(a.CEA608, spec.CEA608)},
		{"spec.dash.addressing", a.DASHAddressing == spec.DASHAddressing},
		{"spec.ads", reflect.DeepEqual(a.Ads, spec.Ads)},
		{"spec.window.dvrSeconds", a.Window.DVR == spec.Window.DVR},
		{"spec.window.retainSeconds", a.Window.Retain == spec.Window.Retain},
		{"spec.faults", slices.EqualFunc(a.Faults, spec.Faults, func(x, y fault.Rule) bool { return reflect.DeepEqual(x, y) })},
	}
	out := []string{}
	for _, f := range fields {
		if !f.same {
			out = append(out, f.path)
		}
	}
	return out
}

// sameMedia reports whether a and b produce the same media: they may differ
// only in settings that apply immediately, and in faults, which never change
// the media.
func sameMedia(a, b Spec) bool {
	a.Window, b.Window = Window{}, Window{}
	a.Faults, b.Faults = nil, nil
	return reflect.DeepEqual(a, b)
}

// Delete removes a stream that is not running, with its runtime, breaks and
// active fault rules.
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
	svc.record(ctx, s, event.StreamDeleted, nil, nil)
	if err := svc.breaks.DeleteAll(ctx, id); err != nil {
		return fmt.Errorf("delete breaks of %s: %w", id, err)
	}
	if err := svc.faults.Clear(ctx, id); err != nil {
		return fmt.Errorf("clear faults of %s: %w", id, err)
	}
	return nil
}

// errConflict wraps ErrConflict with the stream's id and state.
func errConflict(s Stream) error {
	return fmt.Errorf("%w: %s is %s", ErrConflict, s.ID, s.Status.State)
}
