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

	// PlannedEndSeg is EndSeg as scheduled. An early return moves EndSeg
	// before it; the cue-out keeps announcing the planned duration.
	PlannedEndSeg int64
	// OpenSeg is the first segment that was not frozen yet when the break
	// was scheduled (0 for recurring breaks): no segment before it carries
	// any signal of the break.
	OpenSeg int64
	// CancelSeg is the first segment that was not frozen yet when the break
	// was cancelled. Only meaningful when Cancelled.
	CancelSeg int64
	// Start and End are the wall-clock times of StartSeg and EndSeg. Like
	// the cues, they are filled on the way out of the Service.
	Start, End time.Time
}

// Covers reports whether segment n is an ad segment of b.
func (b Break) Covers(n int64) bool {
	return !b.Cancelled && b.StartSeg <= n && n < b.EndSeg
}

// overlaps reports whether b's segments meet [from, to).
func (b Break) overlaps(from, to int64) bool {
	return b.StartSeg < to && from < b.EndSeg
}

// stopped returns b changed so that no segment from open on is an ad: a
// break that has not started is cancelled, an active one ends at open. ok is
// false when nothing is left to change.
func (b Break) stopped(open int64) (_ Break, ok bool) {
	switch {
	case b.Cancelled || b.EndSeg <= open:
		return b, false
	case b.StartSeg >= open:
		b.Cancelled, b.CancelSeg = true, open
	default:
		b.EndSeg = open
	}
	return b, true
}

// Signal is one in-band SCTE-35 message (emsg v1, scheme
// urn:scte:scte35:2013:bin, timescale 90000).
type Signal struct {
	EventID           uint32
	PresentationTicks int64  // 90 kHz media time of the splice point (not mod 2^33)
	DurationTicks     int64  // break duration for cue-out, 0 otherwise
	Message           []byte // splice_info_section
}
