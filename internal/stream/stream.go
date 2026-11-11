// Package stream holds the stream resource: its editable spec, the epochs a
// start freezes from it, validation, and the Service that runs the lifecycle.
package stream

import (
	"slices"
	"time"

	"ottstreamer/internal/timeline"
)

// Format is a delivery protocol.
type Format string

const (
	FormatHLS  Format = "hls"
	FormatDASH Format = "dash"
)

// VideoCodec is a video rendition's codec.
type VideoCodec string

const (
	CodecH264 VideoCodec = "h264"
	CodecHEVC VideoCodec = "hevc" // refused for now
	CodecAV1  VideoCodec = "av1"  // refused for now
)

// AudioCodec is an audio rendition's codec.
type AudioCodec string

const (
	CodecAAC  AudioCodec = "aac"
	CodecAC3  AudioCodec = "ac3"
	CodecEAC3 AudioCodec = "eac3" // refused for now
	CodecOpus AudioCodec = "opus" // refused for now
)

// FrameSize is the number of 48 kHz samples per audio frame, 0 if unknown.
func (c AudioCodec) FrameSize() int {
	switch c {
	case CodecAAC:
		return 1024
	case CodecAC3, CodecEAC3:
		return 1536
	case CodecOpus:
		return 960
	}
	return 0
}

// Addressing is a DASH SegmentTemplate mode.
type Addressing string

const (
	AddressingNumber         Addressing = "number"
	AddressingTimelineTime   Addressing = "timeline-time"
	AddressingTimelineNumber Addressing = "timeline-number"
)

// Command is the SCTE-35 command used for cues.
type Command string

const (
	CommandSpliceInsert Command = "splice_insert"
	CommandTimeSignal   Command = "time_signal"
)

// State is a stream's lifecycle state.
type State string

const (
	StateStopped State = "stopped"
	StateRunning State = "running"
	StateEnded   State = "ended"
)

// Video is one video rendition of the ladder.
type Video struct {
	ID          string
	Codec       VideoCodec
	Profile     string
	Width       int
	Height      int
	BitrateKbps int
}

// Audio is one audio rendition. TocHz beeps on even seconds, TicHz on odd ones.
type Audio struct {
	ID, Lang, Name string
	Channels       int
	BitrateKbps    int
	Codec          AudioCodec
	Group          string
	Default        bool
	TicHz, TocHz   int
}

// Recurring schedules break k at AST + Offset + k*Every, lasting Duration.
type Recurring struct {
	Every, Duration, Offset time.Duration
}

// Ads holds the ad signalling settings.
type Ads struct {
	Lead             time.Duration
	Command          Command
	SegmentationType string   // time_signal only
	HLS              []string // daterange | cueout | oatcls | adobe
	DASH             []string // eventstream-bin | eventstream-xml | inband
	DASHMultiPeriod  bool
	Recurring        *Recurring
}

// Window is the live window. It applies immediately, even while running.
type Window struct {
	DVR, Retain time.Duration
}

// Spec is the editable part of a stream.
type Spec struct {
	Formats         []Format
	SegmentDuration time.Duration
	GOPDuration     time.Duration
	FPS             int
	Timezone        string
	RenditionLabel  bool // overlay.renditionLabel
	Video           []Video
	Audio           []Audio
	DASHAddressing  Addressing
	Ads             Ads
	Window          Window
}

// Has reports whether f is one of the spec's formats.
func (s Spec) Has(f Format) bool {
	return slices.Contains(s.Formats, f)
}

// Clone returns a deep copy of s.
func (s Spec) Clone() Spec {
	s.Formats = slices.Clone(s.Formats)
	s.Video = slices.Clone(s.Video)
	s.Audio = slices.Clone(s.Audio)
	s.Ads.HLS = slices.Clone(s.Ads.HLS)
	s.Ads.DASH = slices.Clone(s.Ads.DASH)
	if s.Ads.Recurring != nil {
		r := *s.Ads.Recurring
		s.Ads.Recurring = &r
	}
	return s
}

// Epoch is one run of a stream: a frozen snapshot of the spec and its AST.
type Epoch struct {
	Number     int
	AST        time.Time
	EndSegment *int64 // nil while running
	Spec       Spec
}

// Timeline returns the segment math of the epoch.
func (e Epoch) Timeline() timeline.Timeline {
	tl := timeline.Timeline{
		AST:       e.AST,
		FPS:       e.Spec.FPS,
		SegFrames: int64(e.Spec.SegmentDuration) * int64(e.Spec.FPS) / int64(time.Second),
		EndSeg:    -1,
	}
	tl.LeadSegs = tl.SegsFor(e.Spec.Ads.Lead)
	if e.EndSegment != nil {
		tl.EndSeg = *e.EndSegment
	}
	return tl
}

// Status is the read-only part of a stream.
type Status struct {
	State          State
	Epoch          *Epoch // nil until the first start
	PendingChanges bool   // spec differs from the epoch snapshot
}

// Stream is the stream resource.
type Stream struct {
	ID, Name  string
	Spec      Spec
	Status    Status
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Clone returns a deep copy of s, sharing no pointers or slices with it.
func (s Stream) Clone() Stream {
	s.Spec = s.Spec.Clone()
	if e := s.Status.Epoch; e != nil {
		c := *e
		c.Spec = e.Spec.Clone()
		if e.EndSegment != nil {
			end := *e.EndSegment
			c.EndSegment = &end
		}
		s.Status.Epoch = &c
	}
	return s
}
