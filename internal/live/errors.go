package live

import "errors"

var (
	// ErrNotFound means the stream has no runtime, the rendition is not in
	// its epoch, or the segment number is outside the epoch.
	ErrNotFound = errors.New("live: not found")
	// ErrUnavailable means the segment could not be encoded.
	ErrUnavailable = errors.New("live: segment unavailable")
)
