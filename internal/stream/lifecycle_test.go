package stream_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"ottstreamer/internal/event"
	"ottstreamer/internal/stream"
)

func TestService_Start_DefaultAST(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"on an even second", t0, t0},
		{"just after an even second", t0.Add(time.Millisecond), t0.Add(2 * time.Second)},
		{"on an odd second", t0.Add(time.Second), t0.Add(2 * time.Second)},
		{"just before an even second", t0.Add(2*time.Second - 1), t0.Add(2 * time.Second)},
		{"other zone", t0.Add(time.Second).In(time.FixedZone("x", 3600)), t0.Add(2 * time.Second)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.create(t, "demo")
			e.now = tt.now
			s := e.start(t, "demo")
			if got := s.Status.Epoch.AST; !got.Equal(tt.want) || got.Location() != time.UTC {
				t.Errorf("AST = %v, want %v UTC", got, tt.want)
			}
		})
	}
}

func TestService_Start(t *testing.T) {
	e := newEnv(t)
	e.create(t, "demo")

	odd := t0.Add(time.Second)
	if _, err := e.svc.Start(t.Context(), "demo", &odd); !errors.Is(err, stream.ErrInvalid) {
		t.Fatalf("odd AST: %v, want ErrInvalid", err)
	}
	if _, err := e.svc.Start(t.Context(), "nope", nil); !errors.Is(err, stream.ErrNotFound) {
		t.Fatalf("unknown: %v, want ErrNotFound", err)
	}

	ast := t0.Add(10 * time.Second)
	s, err := e.svc.Start(t.Context(), "demo", &ast)
	if err != nil {
		t.Fatal(err)
	}
	ep := s.Status.Epoch
	if s.Status.State != stream.StateRunning || ep.Number != 1 || !ep.AST.Equal(ast) || ep.EndSegment != nil {
		t.Errorf("Start = %+v, epoch %+v", s.Status, ep)
	}
	if !slices.Equal(e.rt.launched, []string{"demo"}) {
		t.Errorf("launched %v", e.rt.launched)
	}
	if _, err := e.svc.Start(t.Context(), "demo", nil); !errors.Is(err, stream.ErrConflict) {
		t.Errorf("start running: %v, want ErrConflict", err)
	}

	// The snapshot is a deep copy: editing the spec leaves it alone.
	spec := s.Spec.Clone()
	spec.Video[0].BitrateKbps = 4000
	if _, err := e.svc.Replace(t.Context(), "demo", "demo", spec, s.Version); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.Get(t.Context(), "demo")
	if got.Status.Epoch.Spec.Video[0].BitrateKbps != 3000 {
		t.Errorf("snapshot changed with the spec")
	}

	// Restarting an ended stream drops the old runtime and opens epoch 2.
	if _, err := e.svc.Stop(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	s = e.start(t, "demo")
	if s.Status.Epoch.Number != 2 || s.Status.Epoch.Spec.Video[0].BitrateKbps != 4000 || s.Status.PendingChanges {
		t.Errorf("restart epoch = %+v", s.Status.Epoch)
	}
	if !slices.Equal(e.rt.dropped, []string{"demo"}) {
		t.Errorf("dropped %v", e.rt.dropped)
	}
}

func TestService_Start_LaunchFails(t *testing.T) {
	e := newEnv(t)
	e.create(t, "demo")
	boom := errors.New("ffmpeg missing")
	e.rt.launchErr = boom
	if _, err := e.svc.Start(t.Context(), "demo", nil); !errors.Is(err, boom) {
		t.Fatalf("Start: %v, want the launch error", err)
	}
	s, _ := e.svc.Get(t.Context(), "demo")
	if s.Status.State != stream.StateStopped {
		t.Errorf("state = %s, want stopped", s.Status.State)
	}
}

func TestService_Stop(t *testing.T) {
	e := newEnv(t)
	e.create(t, "demo")
	if _, err := e.svc.Stop(t.Context(), "demo"); !errors.Is(err, stream.ErrConflict) {
		t.Fatalf("stop stopped: %v, want ErrConflict", err)
	}
	e.start(t, "demo") // AST t0, 2 s segments, 4 lead segments

	e.now = t0.Add(21 * time.Second) // live edge 9
	s, err := e.svc.Stop(t.Context(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	end := s.Status.Epoch.EndSegment
	if s.Status.State != stream.StateEnded || end == nil || *end != 13 {
		t.Fatalf("Stop = %+v, end %v, want ended at 13", s.Status, end)
	}
	if e.breaks.trimmed["demo"] != 13 || e.rt.ended["demo"] != 13 {
		t.Errorf("trimmed %v, ended %v", e.breaks.trimmed, e.rt.ended)
	}
	if _, err := e.svc.Stop(t.Context(), "demo"); !errors.Is(err, stream.ErrConflict) {
		t.Errorf("stop ended: %v, want ErrConflict", err)
	}
}

func TestService_Resume(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"running", "ended", "stopped"} {
		e.create(t, id)
	}
	e.start(t, "running")
	e.start(t, "ended")
	if _, err := e.svc.Stop(t.Context(), "ended"); err != nil {
		t.Fatal(err)
	}
	e.rt.launched = nil
	e.events.Events()

	if err := e.svc.Resume(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.rt.launched, []string{"ended", "running"}) {
		t.Errorf("launched %v", e.rt.launched)
	}
	var resumed []string
	for _, ev := range e.events.Events() {
		if ev.Type == event.RuntimeResumed {
			resumed = append(resumed, ev.StreamID+" "+ev.Data["state"].(string))
		}
	}
	if !slices.Equal(resumed, []string{"ended ended", "running running"}) {
		t.Errorf("runtime.resumed for %v", resumed)
	}
}

func TestService_ExpireEnded(t *testing.T) {
	e := newEnv(t)
	e.create(t, "demo")
	e.start(t, "demo")
	e.now = t0.Add(10 * time.Second) // live edge 4, end 8, ends at 18 s
	if _, err := e.svc.Stop(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}

	e.now = t0.Add(18*time.Second + 300*time.Second) // exactly retain past the end
	if err := e.svc.ExpireEnded(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.svc.Get(t.Context(), "demo"); s.Status.State != stream.StateEnded || len(e.rt.dropped) != 0 {
		t.Fatalf("expired too early: %s, dropped %v", s.Status.State, e.rt.dropped)
	}

	e.now = e.now.Add(time.Millisecond)
	if err := e.svc.ExpireEnded(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.svc.Get(t.Context(), "demo"); s.Status.State != stream.StateStopped || !slices.Equal(e.rt.dropped, []string{"demo"}) {
		t.Errorf("after retain: %s, dropped %v", s.Status.State, e.rt.dropped)
	}
}
