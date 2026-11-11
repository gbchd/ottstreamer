package adbreak

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ottstreamer/internal/stream"
	"ottstreamer/internal/timeline"
)

// Streams reads the stream a schedule belongs to.
type Streams interface {
	Get(ctx context.Context, id string) (stream.Stream, error)
}

// Repository stores manual breaks and the overrides of recurring ones, keyed
// by stream, epoch and break ID. Stored values share no memory with callers.
type Repository interface {
	// List returns the stored breaks of one epoch, sorted by StartSeg.
	List(ctx context.Context, streamID string, epoch int) ([]Break, error)
	// Save inserts or replaces b.
	Save(ctx context.Context, b Break) error
	// DeleteAll removes every break of the stream, in every epoch.
	DeleteAll(ctx context.Context, streamID string) error
}

// Cue is one SCTE-35 signal of a break, before encoding.
type Cue struct {
	Command          stream.Command
	SegmentationType string // time_signal only, as in stream.Ads
	EventID          uint32
	Out              bool // cue-out (start of the break), else cue-in
	Cancel           bool // cancels the event; the encoder ignores Out and DurationTicks
	// PTS is the 90 kHz media time of the splice point, not wrapped: the
	// encoder applies mod 2^33.
	PTS           int64
	DurationTicks int64 // break duration on a cue-out, 0 otherwise
}

// CueEncoder turns a Cue into a splice_info_section.
type CueEncoder interface {
	Encode(c Cue) ([]byte, error)
}

// recurringEventBase is the SCTE-35 event ID of recurring break 0.
const recurringEventBase = 0x80000000

// listedPeriods is how many recurring periods past the frozen horizon List
// shows.
const listedPeriods = 3

// Service schedules the breaks of each stream's current epoch.
//
// The rule every method keeps: a change at time t never alters a segment up
// to the frozen horizon at t, neither its ad state nor the signals it
// carries. Signals are a pure function of the stored record, so a segment
// rebuilt later comes out byte for byte the same.
type Service struct {
	streams Streams
	repo    Repository
	cues    CueEncoder
	now     func() time.Time
	// ponytail: one lock for every stream; per-stream locks if schedule
	// changes ever contend.
	mu sync.Mutex // serializes changes, which read, check, then write
}

// NewService returns a Service.
func NewService(streams Streams, repo Repository, cues CueEncoder, now func() time.Time) *Service {
	return &Service{streams: streams, repo: repo, cues: cues, now: now}
}

// snapshot is what a schedule reads from its stream: the current epoch.
type snapshot struct {
	streamID string
	epoch    int
	running  bool
	tl       timeline.Timeline
	ads      stream.Ads
	dvr      time.Duration
}

// current returns the snapshot of the stream's current epoch.
func (s *Service) current(ctx context.Context, streamID string) (snapshot, error) {
	st, err := s.streams.Get(ctx, streamID)
	if err != nil {
		return snapshot{}, err
	}
	e := st.Status.Epoch
	if e == nil {
		return snapshot{}, fmt.Errorf("%w: %s has never started", ErrNotRunning, streamID)
	}
	return snapshot{
		streamID: streamID,
		epoch:    e.Number,
		running:  st.Status.State == stream.StateRunning,
		tl:       e.Timeline(),
		ads:      e.Spec.Ads,
		dvr:      e.Spec.Window.DVR,
	}, nil
}

// currentEpoch is current, failing unless epoch is the current one.
func (s *Service) currentEpoch(ctx context.Context, streamID string, epoch int) (snapshot, error) {
	sn, err := s.current(ctx, streamID)
	if err != nil {
		return snapshot{}, err
	}
	if sn.epoch != epoch {
		return snapshot{}, fmt.Errorf("%w: epoch %d of %s is not current", ErrNotFound, epoch, streamID)
	}
	return sn, nil
}

// Get returns one break of the current epoch.
func (s *Service) Get(ctx context.Context, streamID, breakID string) (Break, error) {
	sn, err := s.current(ctx, streamID)
	if err != nil {
		return Break{}, err
	}
	b, err := s.find(ctx, sn, breakID)
	if err != nil {
		return Break{}, err
	}
	return s.complete(sn, b)
}

// List returns the recent, active and upcoming breaks of the current epoch:
// every break overlapping the DVR window or later, with the recurring ones
// up to a few periods past the frozen horizon.
func (s *Service) List(ctx context.Context, streamID string) ([]Break, error) {
	sn, err := s.current(ctx, streamID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	from := max(0, sn.tl.LiveEdge(now)-sn.tl.SegsFor(sn.dvr)+1)
	to := sn.tl.FrozenHorizon(now) + 1
	if r := sn.ads.Recurring; r != nil {
		to += listedPeriods * sn.tl.SegsFor(r.Every)
	}
	return s.collect(ctx, sn, from, to, maxSeg)
}

const maxSeg = int64(1<<63 - 1)

// InRange returns the breaks of the epoch overlapping segments [from, to),
// recurring and cancelled ones included, sorted by StartSeg, cues filled.
func (s *Service) InRange(ctx context.Context, streamID string, epoch int, from, to int64) ([]Break, error) {
	sn, err := s.currentEpoch(ctx, streamID, epoch)
	if err != nil {
		return nil, err
	}
	return s.collect(ctx, sn, from, to, to)
}

func (s *Service) collect(ctx context.Context, sn snapshot, from, to, storedTo int64) ([]Break, error) {
	stored, err := s.repo.List(ctx, sn.streamID, sn.epoch)
	if err != nil {
		return nil, err
	}
	list := sn.merge(stored, from, to, storedTo)
	for i, b := range list {
		if list[i], err = s.complete(sn, b); err != nil {
			return nil, err
		}
	}
	return list, nil
}

// merge returns the stored breaks overlapping [from, storedTo) and the
// recurring breaks overlapping [from, to) that no stored override replaces,
// sorted by StartSeg.
func (sn snapshot) merge(stored []Break, from, to, storedTo int64) []Break {
	ids := make(map[string]bool, len(stored))
	var out []Break
	for _, b := range stored {
		ids[b.ID] = true
		if b.overlaps(from, storedTo) {
			out = append(out, b)
		}
	}
	if r := sn.ads.Recurring; r != nil {
		// k starts low enough that no earlier break can reach from.
		var k int64
		if past := sn.tl.SegStart(from).Sub(sn.tl.AST) - r.Offset - r.Duration - sn.tl.SegDuration(); past > 0 {
			k = int64(past / r.Every)
		}
		for ; ; k++ {
			b := sn.recurring(k)
			if b.StartSeg >= to {
				break
			}
			if b.overlaps(from, to) && !ids[b.ID] {
				out = append(out, b)
			}
		}
	}
	slices.SortFunc(out, func(a, b Break) int {
		return cmp.Or(cmp.Compare(a.StartSeg, b.StartSeg), cmp.Compare(a.EventID, b.EventID))
	})
	return out
}

// recurring returns recurring break k as computed from the snapshot, before
// any stored override. In an ended epoch it is trimmed the way Trim trims a
// stored break.
func (sn snapshot) recurring(k int64) Break {
	r := sn.ads.Recurring
	start := sn.tl.SegAtOrAfter(sn.tl.AST.Add(r.Offset + time.Duration(k)*r.Every))
	end := start + sn.tl.SegsFor(r.Duration)
	b := Break{
		ID:            "r-" + strconv.FormatInt(k, 10),
		EventID:       recurringEventBase + uint32(k), //nolint:gosec // k stays far below 2^31
		StreamID:      sn.streamID,
		Epoch:         sn.epoch,
		StartSeg:      start,
		EndSeg:        end,
		PlannedEndSeg: end,
		Requested:     r.Duration,
		Recurring:     true,
		CreatedAt:     sn.tl.AST,
	}
	if e := sn.tl.EndSeg; e >= 0 {
		b, _ = b.stopped(e + 1)
	}
	return b
}

// find returns a stored break, or a recurring one by its derived ID.
func (s *Service) find(ctx context.Context, sn snapshot, id string) (Break, error) {
	stored, err := s.repo.List(ctx, sn.streamID, sn.epoch)
	if err != nil {
		return Break{}, err
	}
	if i := slices.IndexFunc(stored, func(b Break) bool { return b.ID == id }); i >= 0 {
		return stored[i], nil
	}
	if n, ok := strings.CutPrefix(id, "r-"); ok && sn.ads.Recurring != nil {
		if k, err := strconv.ParseInt(n, 10, 64); err == nil && k >= 0 && strconv.FormatInt(k, 10) == n {
			return sn.recurring(k), nil
		}
	}
	return Break{}, fmt.Errorf("%w: break %s of %s", ErrNotFound, id, sn.streamID)
}

// complete fills the wall-clock times and the cues of b.
func (s *Service) complete(sn snapshot, b Break) (Break, error) {
	b.Start, b.End = sn.tl.SegStart(b.StartSeg), sn.tl.SegStart(b.EndSeg)
	var err error
	if b.CueOut, err = s.cues.Encode(sn.cueOut(b)); err != nil {
		return Break{}, fmt.Errorf("encode cue-out of %s: %w", b.ID, err)
	}
	if b.CueIn, err = s.cues.Encode(sn.cueIn(b, b.EndSeg)); err != nil {
		return Break{}, fmt.Errorf("encode cue-in of %s: %w", b.ID, err)
	}
	b.Cancel = nil
	if b.Cancelled {
		if b.Cancel, err = s.cues.Encode(sn.cancel(b)); err != nil {
			return Break{}, fmt.Errorf("encode cancel of %s: %w", b.ID, err)
		}
	}
	return b, nil
}

// cueOut announces the planned duration, even after an early return: the
// cue-out sent before the break cannot change.
func (sn snapshot) cueOut(b Break) Cue {
	return Cue{
		Command:          sn.ads.Command,
		SegmentationType: sn.ads.SegmentationType,
		EventID:          b.EventID,
		Out:              true,
		PTS:              sn.tl.VideoTfdt(b.StartSeg),
		DurationTicks:    (b.PlannedEndSeg - b.StartSeg) * sn.tl.SegTicks(),
	}
}

func (sn snapshot) cueIn(b Break, end int64) Cue {
	return Cue{
		Command:          sn.ads.Command,
		SegmentationType: sn.ads.SegmentationType,
		EventID:          b.EventID,
		PTS:              sn.tl.VideoTfdt(end),
	}
}

func (sn snapshot) cancel(b Break) Cue {
	c := sn.cueOut(b)
	c.Cancel, c.DurationTicks = true, 0
	return c
}
