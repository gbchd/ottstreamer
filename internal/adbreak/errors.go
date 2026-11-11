package adbreak

import "errors"

var (
	ErrNotFound   = errors.New("adbreak: not found")
	ErrNotRunning = errors.New("adbreak: stream not running")
	ErrConflict   = errors.New("adbreak: break cannot change")
	ErrInvalid    = errors.New("adbreak: invalid request")
)

// OverlapError is returned when a new break overlaps an existing one.
// errors.Is(err, ErrConflict) holds.
type OverlapError struct {
	ID string // the break overlapped
}

func (e *OverlapError) Error() string { return "adbreak: overlaps break " + e.ID }

func (e *OverlapError) Unwrap() error { return ErrConflict }
