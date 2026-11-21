package stream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ottstreamer/internal/event"
)

// Start begins a new epoch: it freezes a snapshot of the spec, makes
// spec.faults the active fault rules and launches the runtime. ast defaults
// to now rounded up to an even second; a given ast must be on an even second.
func (svc *Service) Start(ctx context.Context, id string, ast *time.Time) (Stream, error) {
	start, err := svc.ast(ast)
	if err != nil {
		return Stream{}, err
	}
	s, err := svc.repo.Get(ctx, id)
	if err != nil {
		return Stream{}, err
	}
	if s.Status.State == StateRunning {
		return Stream{}, errConflict(s)
	}
	wasEnded := s.Status.State == StateEnded
	number := 1
	if s.Status.Epoch != nil {
		number = s.Status.Epoch.Number + 1
	}
	s.Status = Status{
		State: StateRunning,
		Epoch: &Epoch{Number: number, AST: start, Spec: s.Spec.Clone()},
	}
	s.UpdatedAt = svc.now()
	s, err = svc.repo.Save(ctx, s, s.Version)
	if err != nil {
		return Stream{}, err
	}
	if wasEnded {
		svc.rt.Drop(id)
	}
	// Seeded once the new epoch is saved, so faults.updated carries it.
	if _, err := svc.faults.Replace(ctx, id, s.Spec.Faults); err != nil {
		return Stream{}, svc.abortStart(ctx, s, fmt.Errorf("seed faults of %s: %w", id, err))
	}
	if err := svc.rt.Launch(ctx, s); err != nil {
		return Stream{}, svc.abortStart(ctx, s, fmt.Errorf("launch %s: %w", id, err))
	}
	svc.record(ctx, s, event.StreamStarted, nil, map[string]any{"ast": start})
	return s, nil
}

// abortStart marks s, saved as running, stopped again after err.
func (svc *Service) abortStart(ctx context.Context, s Stream, err error) error {
	s.Status.State = StateStopped
	s.UpdatedAt = svc.now()
	if _, serr := svc.repo.Save(ctx, s, s.Version); serr != nil {
		err = errors.Join(err, fmt.Errorf("mark stopped: %w", serr))
	}
	return err
}

func (svc *Service) ast(ast *time.Time) (time.Time, error) {
	const step = 2 * time.Second
	if ast == nil {
		now := svc.now()
		t := now.Truncate(step) // the zero time is on an even Unix second
		if t.Before(now) {
			t = t.Add(step)
		}
		return t.UTC(), nil
	}
	if !ast.Equal(ast.Truncate(step)) {
		return time.Time{}, &ValidationError{Fields: []FieldError{{Field: "ast", Message: "must be on an even second"}}}
	}
	return ast.UTC(), nil
}

// Stop ends the running epoch at the frozen horizon, so every segment
// already announced or produced is still delivered.
func (svc *Service) Stop(ctx context.Context, id string) (Stream, error) {
	s, err := svc.repo.Get(ctx, id)
	if err != nil {
		return Stream{}, err
	}
	if s.Status.State != StateRunning {
		return Stream{}, errConflict(s)
	}
	e := s.Status.Epoch
	end := e.Timeline().FrozenHorizon(svc.now())
	e.EndSegment = &end
	s.Status.State = StateEnded
	s.UpdatedAt = svc.now()
	s, err = svc.repo.Save(ctx, s, s.Version)
	if err != nil {
		return Stream{}, err
	}
	if err := svc.breaks.Trim(ctx, id, e.Number, end); err != nil {
		return Stream{}, fmt.Errorf("trim breaks of %s: %w", id, err)
	}
	svc.rt.End(id, end)
	svc.record(ctx, s, event.StreamStopped, new(end), nil)
	return s, nil
}

// Resume launches the runtime of every running or ended stream, after a
// restart. It launches as many as it can and returns every failure.
func (svc *Service) Resume(ctx context.Context) error {
	// ponytail: no reseed of the fault rules, so in-memory active rules are
	// lost on a restart; a shared FaultRepository keeps them.
	streams, err := svc.repo.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range streams {
		if s.Status.State == StateStopped {
			continue
		}
		if err := svc.rt.Launch(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("launch %s: %w", s.ID, err))
			continue
		}
		svc.record(ctx, s, event.RuntimeResumed, nil, map[string]any{"state": string(s.Status.State)})
	}
	return errors.Join(errs...)
}

// ExpireEnded moves ended streams whose last segment is past the retention
// window to stopped and drops their runtime.
func (svc *Service) ExpireEnded(ctx context.Context) error {
	streams, err := svc.repo.List(ctx)
	if err != nil {
		return err
	}
	now := svc.now()
	var errs []error
	for _, s := range streams {
		e := s.Status.Epoch
		if s.Status.State != StateEnded || e == nil || e.EndSegment == nil {
			continue
		}
		if now.Sub(e.Timeline().SegEnd(*e.EndSegment)) <= s.Spec.Window.Retain {
			continue
		}
		s.Status.State = StateStopped
		s.UpdatedAt = now
		// Save first: if a start won the race, its runtime must survive.
		if _, err := svc.repo.Save(ctx, s, s.Version); err != nil {
			if !errors.Is(err, ErrVersionMismatch) {
				errs = append(errs, fmt.Errorf("expire %s: %w", s.ID, err))
			}
			continue
		}
		svc.rt.Drop(s.ID)
		svc.record(ctx, s, event.StreamExpired, e.EndSegment, nil)
	}
	return errors.Join(errs...)
}
