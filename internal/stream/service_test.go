package stream_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"ottstreamer/internal/event"
	"ottstreamer/internal/event/eventtest"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streammem"
	"ottstreamer/internal/stream/streamtest"
)

var t0 = time.Date(2026, 11, 6, 18, 40, 0, 0, time.UTC)

type fakeRuntimes struct {
	launched  []string
	ended     map[string]int64
	dropped   []string
	launchErr error
}

func (f *fakeRuntimes) Launch(_ context.Context, s stream.Stream) error {
	f.launched = append(f.launched, s.ID)
	return f.launchErr
}

func (f *fakeRuntimes) End(id string, endSeg int64) { f.ended[id] = endSeg }

func (f *fakeRuntimes) Drop(id string) { f.dropped = append(f.dropped, id) }

type fakeBreaks struct {
	trimmed map[string]int64
	deleted []string
}

func (f *fakeBreaks) Trim(_ context.Context, id string, _ int, endSeg int64) error {
	f.trimmed[id] = endSeg
	return nil
}

func (f *fakeBreaks) DeleteAll(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type env struct {
	svc    *stream.Service
	repo   *streammem.Repository
	rt     *fakeRuntimes
	breaks *fakeBreaks
	events *eventtest.Recorder
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		repo:   streammem.NewRepository(),
		rt:     &fakeRuntimes{ended: map[string]int64{}},
		breaks: &fakeBreaks{trimmed: map[string]int64{}},
		events: &eventtest.Recorder{},
		now:    t0,
	}
	e.svc = stream.NewService(e.repo, e.repo, e.rt, e.breaks, e.events, func() time.Time { return e.now })
	return e
}

// create stores a valid stream with id and returns it.
func (e *env) create(t *testing.T, id string) stream.Stream {
	t.Helper()
	s, err := e.svc.Create(t.Context(), stream.Stream{ID: id, Name: id, Spec: streamtest.Spec()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) start(t *testing.T, id string) stream.Stream {
	t.Helper()
	s, err := e.svc.Start(t.Context(), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fields(err error) []string {
	var v *stream.ValidationError
	if !errors.As(err, &v) {
		return nil
	}
	out := make([]string, 0, len(v.Fields))
	for _, f := range v.Fields {
		out = append(out, f.Field)
	}
	return out
}

func TestService_Create(t *testing.T) {
	e := newEnv(t)
	s := e.create(t, "demo")
	if s.Version != 1 || s.Status.State != stream.StateStopped || !s.CreatedAt.Equal(t0) || s.Spec.FPS != 25 {
		t.Errorf("Create = %+v", s)
	}

	_, err := e.svc.Create(t.Context(), stream.Stream{ID: "demo", Spec: streamtest.Spec()})
	if !errors.Is(err, stream.ErrExists) {
		t.Errorf("duplicate: %v, want ErrExists", err)
	}

	spec := streamtest.Spec()
	spec.Video[1].BitrateKbps = 20001 // sorts first, but errors keep the caller's index
	_, err = e.svc.Create(t.Context(), stream.Stream{ID: "Bad ID", Spec: spec})
	if got, want := fields(err), []string{"id", "spec.video[1].bitrateKbps"}; !errors.Is(err, stream.ErrInvalid) || !slices.Equal(got, want) {
		t.Errorf("invalid: %v, fields %v, want %v", err, got, want)
	}
}

func TestService_Replace(t *testing.T) {
	e := newEnv(t)
	e.create(t, "demo")
	spec := streamtest.Spec()
	spec.FPS = 50

	if _, err := e.svc.Replace(t.Context(), "demo", "x", spec, 0); !errors.Is(err, stream.ErrPreconditionRequired) {
		t.Errorf("no If-Match: %v, want ErrPreconditionRequired", err)
	}
	if _, err := e.svc.Replace(t.Context(), "demo", "x", spec, 7); !errors.Is(err, stream.ErrVersionMismatch) {
		t.Errorf("stale: %v, want ErrVersionMismatch", err)
	}
	if _, err := e.svc.Replace(t.Context(), "nope", "x", spec, 1); !errors.Is(err, stream.ErrNotFound) {
		t.Errorf("unknown: %v, want ErrNotFound", err)
	}
	bad := streamtest.Spec()
	bad.FPS = 29
	if _, err := e.svc.Replace(t.Context(), "demo", "x", bad, 1); !errors.Is(err, stream.ErrInvalid) {
		t.Errorf("invalid: %v, want ErrInvalid", err)
	}

	s, err := e.svc.Replace(t.Context(), "demo", "renamed", spec, 1)
	if err != nil || s.Version != 2 || s.Name != "renamed" || s.Spec.FPS != 50 || s.Status.PendingChanges {
		t.Fatalf("Replace = %+v, %v", s, err)
	}
}

func TestService_Replace_Running(t *testing.T) {
	e := newEnv(t)
	e.create(t, "demo")
	s := e.start(t, "demo")

	windowOnly := streamtest.Spec()
	windowOnly.Window = stream.Window{DVR: 60 * time.Second, Retain: 600 * time.Second}
	s, err := e.svc.Replace(t.Context(), "demo", "demo", windowOnly, s.Version)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status.PendingChanges || s.Status.Epoch.Spec.Window != windowOnly.Window {
		t.Errorf("window change: pending %v, epoch window %+v", s.Status.PendingChanges, s.Status.Epoch.Spec.Window)
	}

	media := windowOnly
	media.FPS = 50
	s, err = e.svc.Replace(t.Context(), "demo", "demo", media, s.Version)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Status.PendingChanges || s.Status.Epoch.Spec.FPS != 25 {
		t.Errorf("media change: pending %v, epoch fps %d", s.Status.PendingChanges, s.Status.Epoch.Spec.FPS)
	}
}

func TestService_Delete(t *testing.T) {
	e := newEnv(t)
	e.create(t, "demo")
	e.start(t, "demo")
	if err := e.svc.Delete(t.Context(), "demo", 0); !errors.Is(err, stream.ErrConflict) {
		t.Fatalf("delete running: %v, want ErrConflict", err)
	}
	s, err := e.svc.Stop(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(t.Context(), "demo", s.Version+1); !errors.Is(err, stream.ErrVersionMismatch) {
		t.Fatalf("stale delete: %v, want ErrVersionMismatch", err)
	}
	if err := e.svc.Delete(t.Context(), "demo", s.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Get(t.Context(), "demo"); !errors.Is(err, stream.ErrNotFound) {
		t.Errorf("Get after delete: %v", err)
	}
	if !slices.Equal(e.rt.dropped, []string{"demo"}) || !slices.Equal(e.breaks.deleted, []string{"demo"}) {
		t.Errorf("dropped %v, breaks deleted %v", e.rt.dropped, e.breaks.deleted)
	}
	if err := e.svc.Delete(t.Context(), "demo", 0); !errors.Is(err, stream.ErrNotFound) {
		t.Errorf("delete unknown: %v, want ErrNotFound", err)
	}
}

func TestService_events(t *testing.T) {
	e := newEnv(t)
	ctx := event.WithActor(t.Context(), "api:qa")
	if _, err := e.svc.Create(ctx, stream.Stream{ID: "demo", Name: "demo", Spec: streamtest.Spec()}); err != nil {
		t.Fatal(err)
	}
	s := e.start(t, "demo")
	spec := s.Spec.Clone()
	spec.FPS = 50
	spec.Window.DVR = time.Minute
	if _, err := e.svc.Replace(ctx, "demo", "renamed", spec, s.Version); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Minute)
	if _, err := e.svc.Stop(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Hour)
	if err := e.svc.ExpireEnded(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Delete(ctx, "demo", 0); err != nil {
		t.Fatal(err)
	}

	got := e.events.Events()
	want := []string{
		"stream.created api:qa epoch 0 map[name:demo]",
		"stream.started system epoch 1 map[ast:" + t0.String() + "]",
		"stream.updated api:qa epoch 1 map[fields:[name spec.fps spec.window.dvrSeconds] pendingChanges:true]",
		"stream.stopped api:qa epoch 1 segment 33 map[]",
		"stream.expired system epoch 1 segment 33 map[]",
		"stream.deleted api:qa epoch 1 map[]",
	}
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i, ev := range got {
		line := fmt.Sprintf("%s %s epoch %d", ev.Type, ev.Actor, ev.Epoch)
		if ev.Segment != nil {
			line += fmt.Sprintf(" segment %d", *ev.Segment)
		}
		if line += fmt.Sprintf(" %v", ev.Data); line != want[i] {
			t.Errorf("event %d:\n got %s\nwant %s", i, line, want[i])
		}
	}
	// changed lists the fields of Spec one by one.
	if n := reflect.TypeFor[stream.Spec]().NumField(); n != 11 {
		t.Errorf("Spec has %d fields: add the new ones to changed in service.go, then update this count", n)
	}
}
