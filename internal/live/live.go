// Package live runs the media side of running and ended streams: one
// runtime per epoch, its producer and its segment cache.
package live

import (
	"context"
	"io"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/stream"
)

// Sample is one encoded access unit. Its duration is implied: 90000/FPS
// ticks for video, FrameSize samples for audio.
type Sample struct {
	Data []byte
	Sync bool
}

// Track is one CMAF track of an epoch, built once at start.
type Track struct {
	ID        string
	Video     bool
	Timescale uint32   // 90000 video, 48000 audio
	Codec     string   // RFC 6381, e.g. avc1.640028, mp4a.40.2, ac-3
	Init      []byte   // ftyp (cmfc, iso6, dash) + moov
	FrameSize int      // audio: samples per frame
	Loop      []Sample // audio: steady-state 8 s loop (375 AAC / 250 AC-3 frames)
}

// Encoder produces the media of an epoch.
type Encoder interface {
	// StartEpoch builds every video and audio track of s's current epoch.
	StartEpoch(ctx context.Context, s stream.Stream) (map[string]Track, error)
	// EncodeSegment encodes video segment n for every video rendition in one
	// run: SegFrames samples each, the first one Sync.
	EncodeSegment(ctx context.Context, s stream.Stream, n int64, inAd bool) (map[string][]Sample, error)
}

// Fragment is everything needed to write one CMAF media segment.
type Fragment struct {
	Seq       uint32 // n + 1
	TrackID   uint32 // 1
	BaseTime  uint64 // tfdt in track timescale
	SampleDur uint32
	Samples   []Sample
	Emsgs     []adbreak.Signal // video only
	PadTo     int              // video: total mdat payload bytes to reach with H.264 filler NALs (type 12); 0 = none
}

// SegmentWriter writes CMAF media segments.
type SegmentWriter interface {
	WriteSegment(w io.Writer, f Fragment) error
}
