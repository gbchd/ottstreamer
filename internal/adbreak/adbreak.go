// Package adbreak schedules SCTE-35 ad breaks of a stream's epoch: manual and
// recurring breaks, cancels and early returns, and the cues they signal.
package adbreak

import "time"

// Break is an ad break: whole segments [StartSeg, EndSeg) of one epoch plus
// an event ID. Every signal, emsg and red frame is computed from it.
type Break struct {
	ID        string // "1", "2", ... manual (per epoch); "r-<k>" recurring
	EventID   uint32 // manual: same number as ID; recurring: 0x80000000 + k
	StreamID  string
	Epoch     int
	StartSeg  int64         // first ad segment
	EndSeg    int64         // first segment after the break (exclusive)
	Requested time.Duration // duration asked for; real = (EndSeg-StartSeg)*S
	Recurring bool
	Cancelled bool
	CreatedAt time.Time
	CueOut    []byte // splice_info_section, as built by the CueEncoder
	CueIn     []byte
	Cancel    []byte // cancel cue; nil unless Cancelled
}

// Signal is one in-band SCTE-35 message (emsg v1, scheme
// urn:scte:scte35:2013:bin, timescale 90000).
type Signal struct {
	EventID           uint32
	PresentationTicks int64  // 90 kHz media time of the splice point (not mod 2^33)
	DurationTicks     int64  // break duration for cue-out, 0 otherwise
	Message           []byte // splice_info_section
}
