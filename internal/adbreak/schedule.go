package adbreak

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// maxDuration bounds a break, which keeps every duration well inside the
// 33-bit SCTE-35 tick fields.
const maxDuration = 24 * time.Hour

// AddRequest asks for a break. The start is now, or In from now, or At, and
// never earlier than the segment after the frozen horizon.
type AddRequest struct {
	Duration         time.Duration
	In               *time.Duration
	At               *time.Time
	ReplaceRecurring bool // end or cancel overlapping recurring breaks instead of failing
}

func (r AddRequest) validate() error {
	switch {
	case r.Duration <= 0 || r.Duration > maxDuration:
		return fmt.Errorf("%w: duration must be more than 0 and at most %v", ErrInvalid, maxDuration)
	case r.In != nil && r.At != nil:
		return fmt.Errorf("%w: in and at are exclusive", ErrInvalid)
	case r.In != nil && *r.In < 0:
		return fmt.Errorf("%w: in must not be negative", ErrInvalid)
	}
	return nil
}

// Add schedules a manual break in the running epoch, snapped to whole
// segments after the frozen horizon. It fails with an *OverlapError when the
// break would overlap another one, unless that one is recurring and
// req.ReplaceRecurring is set.
func (s *Service) Add(ctx context.Context, streamID string, req AddRequest) (Break, error) {
	if err := req.validate(); err != nil {
		return Break{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, err := s.current(ctx, streamID)
	if err != nil {
		return Break{}, err
	}
	if !sn.running {
		return Break{}, fmt.Errorf("%w: %s", ErrNotRunning, streamID)
	}
	now := s.now()
	open := sn.tl.FrozenHorizon(now) + 1
	at := now
	switch {
	case req.At != nil:
		at = *req.At
	case req.In != nil:
		at = now.Add(*req.In)
	}
	start := max(open, sn.tl.SegAtOrAfter(at))
	end := start + sn.tl.SegsFor(req.Duration)

	stored, err := s.repo.List(ctx, streamID, sn.epoch)
	if err != nil {
		return Break{}, err
	}
	var replaced []Break
	for _, b := range sn.merge(stored, start, end, end) {
		if b.Cancelled {
			continue
		}
		if !b.Recurring || !req.ReplaceRecurring {
			return Break{}, &OverlapError{ID: b.ID}
		}
		r, _ := b.stopped(open) // b ends after start >= open, so it changes
		replaced = append(replaced, r)
	}
	n := 1
	for _, b := range stored {
		if !b.Recurring {
			n++
		}
	}
	b := Break{
		ID:            strconv.Itoa(n),
		EventID:       uint32(n),
		StreamID:      streamID,
		Epoch:         sn.epoch,
		StartSeg:      start,
		EndSeg:        end,
		PlannedEndSeg: end,
		OpenSeg:       open,
		Requested:     req.Duration,
		CreatedAt:     now,
	}
	for _, r := range append(replaced, b) {
		if err := s.repo.Save(ctx, r); err != nil {
			return Break{}, fmt.Errorf("save break %s: %w", r.ID, err)
		}
	}
	return s.complete(sn, b)
}

// Delete cancels a break of the running epoch that has not started, or ends
// an active one early at the first segment after the frozen horizon. A
// cancelled break, or one with no segment left to change, is ErrConflict.
func (s *Service) Delete(ctx context.Context, streamID, breakID string) (Break, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn, err := s.current(ctx, streamID)
	if err != nil {
		return Break{}, err
	}
	if !sn.running {
		return Break{}, fmt.Errorf("%w: %s", ErrNotRunning, streamID)
	}
	b, err := s.find(ctx, sn, breakID)
	if err != nil {
		return Break{}, err
	}
	b, ok := b.stopped(sn.tl.FrozenHorizon(s.now()) + 1)
	if !ok {
		return Break{}, fmt.Errorf("%w: break %s is cancelled or over", ErrConflict, breakID)
	}
	if err := s.repo.Save(ctx, b); err != nil {
		return Break{}, fmt.Errorf("save break %s: %w", b.ID, err)
	}
	return s.complete(sn, b)
}

// Trim applies a stop at endSeg, the epoch's last segment: stored breaks
// starting after it are cancelled, and one active at it ends at endSeg+1, so
// endSeg, already frozen, stays its last ad segment. Its cue-in would fall
// on endSeg+1, which is never produced. Recurring breaks without an override
// are trimmed the same way when computed, from the epoch's end segment.
func (s *Service) Trim(ctx context.Context, streamID string, epoch int, endSeg int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, err := s.repo.List(ctx, streamID, epoch)
	if err != nil {
		return err
	}
	for _, b := range stored {
		if t, ok := b.stopped(endSeg + 1); ok {
			if err := s.repo.Save(ctx, t); err != nil {
				return fmt.Errorf("save break %s: %w", b.ID, err)
			}
		}
	}
	return nil
}

// DeleteAll removes every break of the stream.
func (s *Service) DeleteAll(ctx context.Context, streamID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.repo.DeleteAll(ctx, streamID)
}
