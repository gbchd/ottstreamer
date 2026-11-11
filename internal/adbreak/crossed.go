package adbreak

import (
	"context"
	"encoding/base64"
	"slices"

	"ottstreamer/internal/event"
)

// Crossed returns the events of the moment segment n of the epoch is
// published: the breaks starting or ending at n and the signals n is the
// first to carry. Each segment is asked about once, as the live edge reaches
// it, so the log reads as history.
func (s *Service) Crossed(ctx context.Context, streamID string, epoch int, n int64) ([]event.Event, error) {
	sn, err := s.currentEpoch(ctx, streamID, epoch)
	if err != nil || (sn.tl.EndSeg >= 0 && n > sn.tl.EndSeg) {
		return nil, err // nothing is published after an ended epoch's last segment
	}
	stored, err := s.repo.List(ctx, streamID, sn.epoch)
	if err != nil {
		return nil, err
	}
	// Signals first ride at most LeadSegs before their splice point.
	far := n + sn.tl.LeadSegs + 1
	var out []event.Event
	for _, b := range sn.merge(stored, n-1, far, far) {
		if b, err = s.complete(sn, b); err != nil {
			return nil, err
		}
		out = append(out, sn.crossed(b, n)...)
	}
	return out, nil
}

// crossed returns the events of b at segment n, by the placement rules of
// carried:
//
//   - signal.announced: n is the first segment carrying the cue-out in band,
//     unless a cancel came first.
//   - break.started and signal.cue_out: n is the first ad segment.
//   - break.ended: n is the first segment after the break, or the epoch's
//     last segment cuts the break; signal.cue_in only in the first case,
//     since a cut break signals no cue-in.
//   - signal.cancel: n is the first segment carrying the cancel in band.
func (sn snapshot) crossed(b Break, n int64) []event.Event {
	inband := slices.Contains(sn.ads.DASH, "inband")
	outFrom := max(b.StartSeg-sn.tl.LeadSegs, b.OpenSeg)
	var out []event.Event
	if inband && n == outFrom && (!b.Cancelled || n < b.CancelSeg) {
		out = append(out, sn.signal(event.SignalAnnounced, b, n, b.CueOut))
	}
	if b.Cancelled {
		if inband && n == b.CancelSeg && outFrom < b.CancelSeg {
			out = append(out, sn.signal(event.SignalCancel, b, n, b.Cancel))
		}
		return out
	}
	if n == b.StartSeg {
		out = append(out, sn.event(event.BreakStarted, b, n), sn.signal(event.SignalCueOut, b, n, b.CueOut))
	}
	switch {
	case n == b.EndSeg:
		out = append(out, sn.event(event.BreakEnded, b, n), sn.signal(event.SignalCueIn, b, n, b.CueIn))
	case n == sn.tl.EndSeg && b.Covers(n):
		out = append(out, sn.event(event.BreakEnded, b, b.EndSeg))
	}
	return out
}

// signal is a signal event of b: its command, cue styles and cue.
func (sn snapshot) signal(t event.Type, b Break, n int64, cue []byte) event.Event {
	e := sn.event(t, b, n)
	e.Data["command"] = string(sn.ads.Command)
	e.Data["hls"] = sn.ads.HLS
	e.Data["dash"] = sn.ads.DASH
	e.Data["cue"] = base64.StdEncoding.EncodeToString(cue)
	return e
}
