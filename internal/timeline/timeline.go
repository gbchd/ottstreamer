// Package timeline maps segment numbers to wall-clock and media time.
//
// Everything derives from one anchor, the availability start time (AST), and
// integer counters from it: no floating-point time, so the same segment number
// always yields the same times and media offsets.
package timeline

import "time"

const (
	// VideoTimescale is the video track timescale, also the SCTE-35 PTS clock.
	VideoTimescale = 90000
	// AudioTimescale is the audio sample rate and track timescale.
	AudioTimescale = 48000
)

// Timeline maps segment numbers to wall-clock and media time for one epoch.
// Segment n covers video frames [n*SegFrames, (n+1)*SegFrames).
type Timeline struct {
	AST       time.Time
	FPS       int   // 24, 25, 30, 50, 60
	SegFrames int64 // video frames per segment (S*F)
	LeadSegs  int64 // ceil(leadSeconds / S)
	EndSeg    int64 // last segment of an ended epoch; -1 while running
}

// SegDuration is the exact segment duration S.
func (t Timeline) SegDuration() time.Duration {
	return time.Duration(t.SegFrames) * time.Second / time.Duration(t.FPS)
}

// SegTicks is the segment duration in 90 kHz ticks.
func (t Timeline) SegTicks() int64 {
	return t.SegFrames * VideoTimescale / int64(t.FPS)
}

// segSamples is the segment duration in 48 kHz samples. It is an integer for
// every allowed FPS, since each one divides 48000.
func (t Timeline) segSamples() int64 {
	return t.SegFrames * AudioTimescale / int64(t.FPS)
}

// SegsFor returns how many whole segments cover d, rounded up.
func (t Timeline) SegsFor(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return ceilDiv(int64(d), int64(t.SegDuration()))
}

// LiveEdge is the last segment n with AST+(n+1)S <= now, capped at EndSeg
// once the epoch has ended. It is -1 when no segment is complete yet.
func (t Timeline) LiveEdge(now time.Time) int64 {
	n := floorDiv(int64(now.Sub(t.AST)), int64(t.SegDuration())) - 1
	return max(t.capped(n), -1)
}

// FrozenHorizon is the last segment that can no longer change:
// LiveEdge + LeadSegs, capped at EndSeg.
func (t Timeline) FrozenHorizon(now time.Time) int64 {
	return t.capped(t.LiveEdge(now) + t.LeadSegs)
}

// Available reports whether segment n is published and still retained.
func (t Timeline) Available(n int64, now time.Time, retain time.Duration) bool {
	return n >= 0 && n <= t.LiveEdge(now) && now.Sub(t.SegEnd(n)) <= retain
}

// SegStart is the wall-clock start of segment n.
func (t Timeline) SegStart(n int64) time.Time {
	return t.AST.Add(time.Duration(n) * t.SegDuration())
}

// SegEnd is the wall-clock end of segment n.
func (t Timeline) SegEnd(n int64) time.Time {
	return t.SegStart(n + 1)
}

// SegAtOrAfter returns the smallest n >= 0 with SegStart(n) >= at.
func (t Timeline) SegAtOrAfter(at time.Time) int64 {
	d := at.Sub(t.AST)
	if d <= 0 {
		return 0
	}
	return ceilDiv(int64(d), int64(t.SegDuration()))
}

// FirstFrame is the global index of the first video frame of segment n.
func (t Timeline) FirstFrame(n int64) int64 {
	return n * t.SegFrames
}

// VideoTfdt is the video media time of segment n in 90 kHz ticks.
func (t Timeline) VideoTfdt(n int64) int64 {
	return n * t.SegTicks()
}

// AudioFrameRange returns the audio frames [first, end) of segment n: every
// frame of frameSize samples whose start falls in [n*S, (n+1)*S).
func (t Timeline) AudioFrameRange(n int64, frameSize int) (first, end int64) {
	s, a := t.segSamples(), int64(frameSize)
	return ceilDiv(n*s, a), ceilDiv((n+1)*s, a)
}

// VideoSegAt returns the segment whose video tfdt is ticks, if any.
func (t Timeline) VideoSegAt(ticks int64) (int64, bool) {
	st := t.SegTicks()
	if ticks < 0 || ticks%st != 0 {
		return 0, false
	}
	return ticks / st, true
}

// AudioSegAt returns the segment whose audio tfdt (48 kHz) is ticks, if any.
func (t Timeline) AudioSegAt(ticks int64, frameSize int) (int64, bool) {
	if ticks < 0 {
		return 0, false
	}
	n := ticks / t.segSamples()
	first, _ := t.AudioFrameRange(n, frameSize)
	if first*int64(frameSize) != ticks {
		return 0, false
	}
	return n, true
}

func (t Timeline) capped(n int64) int64 {
	if t.EndSeg >= 0 {
		return min(n, t.EndSeg)
	}
	return n
}

// ceilDiv rounds a/b up; a >= 0, b > 0.
func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}

// floorDiv rounds a/b down for any sign of a; b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}
