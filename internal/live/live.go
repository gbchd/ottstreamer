// Package live runs the media side of running and ended streams: one
// runtime per epoch, its producer and its segment cache.
package live

import (
	"context"
	"fmt"
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

// Kind is what a track carries.
type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
	KindText  Kind = "text" // subtitles, timed like video
)

// The codec strings of subtitle tracks.
const (
	CodecWebVTT = "wvtt"
	CodecIMSC1  = "stpp.ttml.im1t"
)

// Track is one CMAF track of an epoch, built once at start.
type Track struct {
	ID        string
	Kind      Kind
	Timescale uint32   // 90000 video and text, 48000 audio
	Codec     string   // RFC 6381, e.g. avc1.640028, hvc1.1.6.L120.90, av01.0.08M.08, mp4a.40.2, ac-3, ec-3, opus, wvtt
	Lang      string   // text: the track's BCP 47 language
	Init      []byte   // ftyp (cmfc, iso6, dash) + moov
	FrameSize int      // audio: samples per frame
	Loop      []Sample // audio: steady-state 8 s loop (375 AAC / 250 AC-3 and E-AC-3 / 400 Opus frames)
}

// Encoder produces the media of an epoch.
type Encoder interface {
	// StartEpoch builds every video and audio track of s's current epoch.
	StartEpoch(ctx context.Context, s stream.Stream) (map[string]Track, error)
	// EncodeSegment encodes video segment n for every video rendition in one
	// run: SegFrames samples each, the first one Sync.
	EncodeSegment(ctx context.Context, s stream.Stream, n int64, inAd bool) (map[string][]Sample, error)
}

// Cue is one subtitle cue over [Start, End), in 90 kHz media time.
type Cue struct {
	Start, End int64
	Text       string
}

// Timestamp formats 90 kHz media time as hh:mm:ss.mmm, the clock time that
// WebVTT and TTML share. Cue bounds are whole milliseconds: whole seconds or
// segment boundaries, which every valid segment duration puts on a
// millisecond.
func Timestamp(ticks int64) string {
	ms := ticks / 90
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// Fragment is everything needed to write one CMAF media segment.
type Fragment struct {
	Seq       uint32 // n + 1
	TrackID   uint32 // 1
	BaseTime  uint64 // tfdt in track timescale
	Codec     string // the track's RFC 6381 codec string; picks the filler
	SampleDur uint32
	Samples   []Sample
	Emsgs     []adbreak.Signal // video only
	PadTo     int              // video: total mdat payload bytes to reach with codec filler; 0 = none
	Cues      []Cue            // text: back to back over the segment, in place of Samples
	Lang      string           // text: the track's language
}

// SegmentWriter writes CMAF media segments, and the headers of the text
// tracks, which no encoder builds.
type SegmentWriter interface {
	WriteSegment(w io.Writer, f Fragment) error
	// TextInit returns the CMAF header of a subtitle track of codec
	// CodecWebVTT or CodecIMSC1.
	TextInit(codec, lang string) ([]byte, error)
}
