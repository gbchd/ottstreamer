package adbreak

import (
	"context"
	"fmt"
	"slices"
)

// Signals returns the in-band SCTE-35 messages of video segment n, empty
// unless the epoch signals "inband" in DASH.
func (s *Service) Signals(ctx context.Context, streamID string, epoch int, n int64) ([]Signal, error) {
	sn, err := s.currentEpoch(ctx, streamID, epoch)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(sn.ads.DASH, "inband") {
		return nil, nil
	}
	stored, err := s.repo.List(ctx, streamID, epoch)
	if err != nil {
		return nil, err
	}
	// Every cue a segment carries splices at most LeadSegs+1 segments later,
	// and a cue-in at most one segment after a break ends.
	far := n + sn.tl.LeadSegs + 1
	var out []Signal
	for _, b := range sn.merge(stored, n-1, far, far) {
		for _, c := range sn.carried(b, n) {
			msg, err := s.cues.Encode(c)
			if err != nil {
				return nil, fmt.Errorf("encode cue of %s: %w", b.ID, err)
			}
			out = append(out, Signal{EventID: c.EventID, PresentationTicks: c.PTS, DurationTicks: c.DurationTicks, Message: msg})
		}
	}
	return out, nil
}

// carried returns the cues segment n carries in band for b.
//
// A cue for a splice at segment x is repeated in every segment from
// max(x - LeadSegs, the first open segment) to x-1; when that span is empty,
// because x itself was the first open segment, the cue rides in segment x,
// with a presentation time equal to its start. "Open" means not frozen when
// the record last changed, so frozen segments keep exactly what they had:
//
//   - cue-out: open from OpenSeg; a cancel stops it at CancelSeg and sends the
//     cancel from CancelSeg on, if the cue-out was already out by then.
//   - cue-in at the planned end: open from OpenSeg, never before the break
//     starts; an early return stops it at the new end.
//   - early return: the cue-in at the new end rides in that segment alone,
//     the first one open when the break was ended.
func (sn snapshot) carried(b Break, n int64) []Cue {
	var cues []Cue
	outFrom := max(b.StartSeg-sn.tl.LeadSegs, b.OpenSeg)
	if spans(n, outFrom, b.StartSeg) && (!b.Cancelled || n < b.CancelSeg) {
		cues = append(cues, sn.cueOut(b))
	}
	if b.Cancelled {
		if outFrom < b.CancelSeg && spans(n, b.CancelSeg, b.StartSeg) {
			cues = append(cues, sn.cancel(b))
		}
		return cues
	}
	inFrom := max(b.PlannedEndSeg-sn.tl.LeadSegs, b.OpenSeg, b.StartSeg)
	if spans(n, inFrom, b.PlannedEndSeg) && n < b.EndSeg {
		cues = append(cues, sn.cueIn(b, b.PlannedEndSeg))
	}
	if b.EndSeg < b.PlannedEndSeg && n == b.EndSeg {
		cues = append(cues, sn.cueIn(b, b.EndSeg))
	}
	return cues
}

// spans reports whether n carries a cue first sent in segment from, for a
// splice at segment x: from <= n < x, or n == from when from >= x.
func spans(n, from, x int64) bool {
	return from <= n && n <= max(x-1, from)
}
