package adbreak_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakmem"
	"ottstreamer/internal/event/eventtest"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streammem"
	"ottstreamer/internal/stream/streamtest"
)

// t0 is the AST of every fixture. With the default 2 s segments and 8 s
// lead, LeadSegs is 4.
var t0 = time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)

const segTicks = 180000 // 2 s at 90 kHz

type nopRuntimes struct{}

func (nopRuntimes) Launch(context.Context, stream.Stream) error { return nil }
func (nopRuntimes) End(string, int64)                           {}
func (nopRuntimes) Drop(string)                                 {}

type nopFaults struct{}

func (nopFaults) Replace(_ context.Context, _ string, r []fault.Rule) ([]fault.Rule, error) {
	return r, nil
}
func (nopFaults) Clear(context.Context, string) error { return nil }

// fakeCues encodes a cue as readable text holding every field that matters.
type fakeCues struct{}

func (fakeCues) Encode(c adbreak.Cue) ([]byte, error) {
	kind := "in"
	switch {
	case c.Cancel:
		kind = "cancel"
	case c.Out:
		kind = "out"
	}
	return fmt.Appendf(nil, "%s %d %d %d %s", kind, c.EventID, c.PTS, c.DurationTicks, c.Command), nil
}

type fixture struct {
	now     time.Time
	streams *stream.Service
	breaks  *adbreak.Service
	events  *eventtest.Recorder
}

// newFixture starts stream "s" at t0, with in-band signals and the spec
// edited by edit.
func newFixture(t *testing.T, edit func(*stream.Spec)) *fixture {
	t.Helper()
	f := &fixture{now: t0, events: &eventtest.Recorder{}}
	clock := func() time.Time { return f.now }
	repo := streammem.NewRepository()
	f.breaks = adbreak.NewService(repo, adbreakmem.NewRepository(), fakeCues{}, f.events, clock)
	f.streams = stream.NewService(repo, repo, nopRuntimes{}, f.breaks, nopFaults{}, f.events, clock)
	spec := streamtest.Spec()
	spec.Ads.DASH = []string{"eventstream-bin", "inband"}
	if edit != nil {
		edit(&spec)
	}
	if _, err := f.streams.Create(t.Context(), stream.Stream{ID: "s", Spec: spec}); err != nil {
		t.Fatal(err)
	}
	ast := t0
	if _, err := f.streams.Start(t.Context(), "s", &ast); err != nil {
		t.Fatal(err)
	}
	f.events.Events() // stream events
	return f
}

// edge moves the clock to where the live edge is segment e (2 s segments):
// the frozen horizon is e+4, the first open segment e+5.
func (f *fixture) edge(e int64) {
	f.now = t0.Add(time.Duration(e+1)*2*time.Second + time.Second)
}

func (f *fixture) add(t *testing.T, req adbreak.AddRequest) adbreak.Break {
	t.Helper()
	b, err := f.breaks.Add(t.Context(), "s", req)
	if err != nil {
		t.Fatalf("Add(%+v): %v", req, err)
	}
	return b
}

// seg returns the time of the start of segment n (2 s segments).
func seg(n int64) *time.Time {
	at := t0.Add(time.Duration(n) * 2 * time.Second)
	return &at
}

func ptr[T any](v T) *T { return &v }

func span(b adbreak.Break) string {
	return fmt.Sprintf("[%d,%d)", b.StartSeg, b.EndSeg)
}

func TestAddSnapping(t *testing.T) {
	tests := []struct {
		name string
		req  adbreak.AddRequest
		want string
	}{
		{"now is the earliest", adbreak.AddRequest{Duration: 30 * time.Second}, "[14,29)"},
		{"in before earliest", adbreak.AddRequest{Duration: 30 * time.Second, In: ptr(time.Second)}, "[14,29)"},
		{"in snaps up", adbreak.AddRequest{Duration: 30 * time.Second, In: ptr(10 * time.Second)}, "[16,31)"},
		{"at on a boundary", adbreak.AddRequest{Duration: 30 * time.Second, At: seg(50)}, "[50,65)"},
		{"at snaps up", adbreak.AddRequest{Duration: 30 * time.Second, At: ptr(seg(50).Add(time.Millisecond))}, "[51,66)"},
		{"at in the past", adbreak.AddRequest{Duration: 30 * time.Second, At: seg(2)}, "[14,29)"},
		{"duration rounds up", adbreak.AddRequest{Duration: 31 * time.Second, At: seg(50)}, "[50,66)"},
		{"tiny duration is one segment", adbreak.AddRequest{Duration: time.Nanosecond, At: seg(50)}, "[50,51)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.edge(9)
			b := f.add(t, tt.req)
			if span(b) != tt.want || b.PlannedEndSeg != b.EndSeg || b.OpenSeg != 14 {
				t.Errorf("break %s planned end %d open %d, want %s planned end = end, open 14", span(b), b.PlannedEndSeg, b.OpenSeg, tt.want)
			}
			if b.Requested != tt.req.Duration || !b.Start.Equal(*seg(b.StartSeg)) || !b.End.Equal(*seg(b.EndSeg)) {
				t.Errorf("requested %v start %v end %v", b.Requested, b.Start, b.End)
			}
		})
	}
}

func TestAddRoundsToFractionalSegments(t *testing.T) {
	f := newFixture(t, func(s *stream.Spec) { s.SegmentDuration = 1920 * time.Millisecond })
	b := f.add(t, adbreak.AddRequest{Duration: 30 * time.Second})
	if got := b.EndSeg - b.StartSeg; got != 16 {
		t.Errorf("30 s on 1.92 s segments = %d segments, want 16", got)
	}
	if got := b.End.Sub(b.Start); got != 30720*time.Millisecond {
		t.Errorf("real duration = %v, want 30.72s", got)
	}
	// 8 s lead on 1.92 s segments is 5 segments: at t0 the live edge is -1,
	// the frozen horizon 4.
	if b.StartSeg != 5 || string(b.CueOut) != fmt.Sprintf("out 1 %d %d splice_insert", 5*172800, 16*172800) {
		t.Errorf("break starts at %d with cue-out %q", b.StartSeg, b.CueOut)
	}
}

func TestAddRejects(t *testing.T) {
	tests := []struct {
		name string
		req  adbreak.AddRequest
		want error
	}{
		{"no duration", adbreak.AddRequest{}, adbreak.ErrInvalid},
		{"too long", adbreak.AddRequest{Duration: 25 * time.Hour}, adbreak.ErrInvalid},
		{"in and at", adbreak.AddRequest{Duration: time.Second, In: ptr(time.Second), At: seg(20)}, adbreak.ErrInvalid},
		{"negative in", adbreak.AddRequest{Duration: time.Second, In: ptr(-time.Second)}, adbreak.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, nil)
			if _, err := f.breaks.Add(t.Context(), "s", tt.req); !errors.Is(err, tt.want) {
				t.Errorf("Add = %v, want %v", err, tt.want)
			}
		})
	}

	t.Run("unknown stream", func(t *testing.T) {
		f := newFixture(t, nil)
		if _, err := f.breaks.Add(t.Context(), "nope", adbreak.AddRequest{Duration: time.Second}); !errors.Is(err, stream.ErrNotFound) {
			t.Errorf("Add = %v, want stream.ErrNotFound", err)
		}
	})
	t.Run("stopped stream", func(t *testing.T) {
		f := newFixture(t, nil)
		if _, err := f.streams.Stop(t.Context(), "s"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.breaks.Add(t.Context(), "s", adbreak.AddRequest{Duration: time.Second}); !errors.Is(err, adbreak.ErrNotRunning) {
			t.Errorf("Add = %v, want ErrNotRunning", err)
		}
	})
}

func TestAddOverlap(t *testing.T) {
	f := newFixture(t, nil)
	f.edge(9)
	first := f.add(t, adbreak.AddRequest{Duration: 10 * time.Second}) // [14,19)
	_, err := f.breaks.Add(t.Context(), "s", adbreak.AddRequest{Duration: 10 * time.Second, At: seg(18)})
	var overlap *adbreak.OverlapError
	if !errors.As(err, &overlap) || overlap.ID != first.ID || !errors.Is(err, adbreak.ErrConflict) {
		t.Fatalf("overlapping Add = %v, want OverlapError naming %s", err, first.ID)
	}
	second := f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(19)}) // adjacent is fine
	if first.ID != "1" || first.EventID != 1 || second.ID != "2" || second.EventID != 2 {
		t.Errorf("ids %s/%d %s/%d, want 1/1 2/2", first.ID, first.EventID, second.ID, second.EventID)
	}

	// A cancelled break no longer blocks its segments.
	if _, err := f.breaks.Delete(t.Context(), "s", second.ID); err != nil {
		t.Fatal(err)
	}
	if third := f.add(t, adbreak.AddRequest{Duration: 4 * time.Second, At: seg(20)}); third.ID != "3" {
		t.Errorf("third break id %s, want 3", third.ID)
	}
}

func TestDelete(t *testing.T) {
	t.Run("not started cancels", func(t *testing.T) {
		f := newFixture(t, nil)
		f.edge(9)
		b := f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(30)})
		f.edge(12)
		got, err := f.breaks.Delete(t.Context(), "s", b.ID)
		if err != nil || !got.Cancelled || got.CancelSeg != 17 || span(got) != "[30,35)" || got.Cancel == nil {
			t.Fatalf("Delete = %+v, %v; want cancelled at 17", got, err)
		}
		if again, _ := f.breaks.Get(t.Context(), "s", b.ID); !again.Cancelled {
			t.Errorf("Get after cancel = %+v", again)
		}
		if _, err := f.breaks.Delete(t.Context(), "s", b.ID); !errors.Is(err, adbreak.ErrConflict) {
			t.Errorf("second Delete = %v, want ErrConflict", err)
		}
	})
	t.Run("active ends early", func(t *testing.T) {
		f := newFixture(t, nil)
		f.edge(9)
		b := f.add(t, adbreak.AddRequest{Duration: 30 * time.Second}) // [14,29)
		f.edge(15)                                                    // horizon 19
		got, err := f.breaks.Delete(t.Context(), "s", b.ID)
		if err != nil || got.Cancelled || span(got) != "[14,20)" || got.PlannedEndSeg != 29 {
			t.Fatalf("Delete = %+v, %v; want [14,20) planned to 29", got, err)
		}
		if string(got.CueIn) != fmt.Sprintf("in 1 %d 0 splice_insert", 20*segTicks) {
			t.Errorf("cue-in = %q, want at segment 20", got.CueIn)
		}
		if string(got.CueOut) != fmt.Sprintf("out 1 %d %d splice_insert", 14*segTicks, 15*segTicks) {
			t.Errorf("cue-out = %q, want the planned 15 segments", got.CueOut)
		}
		if _, err := f.breaks.Delete(t.Context(), "s", b.ID); !errors.Is(err, adbreak.ErrConflict) {
			t.Errorf("second Delete = %v, want ErrConflict", err)
		}
	})
	t.Run("over is a conflict", func(t *testing.T) {
		f := newFixture(t, nil)
		f.edge(9)
		b := f.add(t, adbreak.AddRequest{Duration: 10 * time.Second}) // [14,19)
		f.edge(14)                                                    // horizon 18: every segment frozen
		if _, err := f.breaks.Delete(t.Context(), "s", b.ID); !errors.Is(err, adbreak.ErrConflict) {
			t.Errorf("Delete = %v, want ErrConflict", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		f := newFixture(t, nil)
		for _, id := range []string{"1", "r-0", "x"} {
			if _, err := f.breaks.Delete(t.Context(), "s", id); !errors.Is(err, adbreak.ErrNotFound) {
				t.Errorf("Delete(%s) = %v, want ErrNotFound", id, err)
			}
		}
	})
}

func TestStopTrims(t *testing.T) {
	f := newFixture(t, nil)
	f.edge(9)
	active := f.add(t, adbreak.AddRequest{Duration: 30 * time.Second})             // [14,29)
	later := f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(40)}) // [40,45)
	f.edge(15)
	st, err := f.streams.Stop(t.Context(), "s")
	if err != nil || *st.Status.Epoch.EndSegment != 19 {
		t.Fatalf("Stop = %+v, %v; want end segment 19", st.Status, err)
	}
	got, _ := f.breaks.Get(t.Context(), "s", active.ID)
	if span(got) != "[14,20)" || got.Cancelled {
		t.Errorf("active break after stop = %s cancelled %v, want [14,20): its last ad segment is 19", span(got), got.Cancelled)
	}
	got, _ = f.breaks.Get(t.Context(), "s", later.ID)
	if !got.Cancelled || got.CancelSeg != 20 {
		t.Errorf("later break after stop = %+v, want cancelled at 20", got)
	}
	if _, err := f.breaks.Delete(t.Context(), "s", active.ID); !errors.Is(err, adbreak.ErrNotRunning) {
		t.Errorf("Delete after stop = %v, want ErrNotRunning", err)
	}
}

// recurringSpec: break k is [10+30k, 15+30k) on 2 s segments.
func recurringSpec(s *stream.Spec) {
	s.Ads.Recurring = &stream.Recurring{Every: time.Minute, Duration: 10 * time.Second, Offset: 20 * time.Second}
}

func ids(list []adbreak.Break) string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.ID + span(b)
		if b.Cancelled {
			out[i] += "x"
		}
	}
	return strings.Join(out, " ")
}

func TestRecurring(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, recurringSpec)

	list, err := f.breaks.List(ctx, "s")
	if err != nil || ids(list) != "r-0[10,15) r-1[40,45) r-2[70,75)" {
		t.Fatalf("List = %s, %v", ids(list), err)
	}
	if b := list[1]; b.EventID != 0x80000001 || !b.Recurring || b.Requested != 10*time.Second || b.OpenSeg != 0 {
		t.Errorf("r-1 = %+v", b)
	}

	// Overrides: cancel r-1, end r-0 early.
	if _, err := f.breaks.Delete(ctx, "s", "r-1"); err != nil {
		t.Fatal(err)
	}
	f.edge(8) // horizon 12
	if _, err := f.breaks.Delete(ctx, "s", "r-0"); err != nil {
		t.Fatal(err)
	}
	list, _ = f.breaks.InRange(ctx, "s", 1, 0, 80)
	if ids(list) != "r-0[10,13) r-1[40,45)x r-2[70,75)" {
		t.Errorf("InRange after overrides = %s", ids(list))
	}

	// A manual break over a recurring one: 409, unless replaceRecurring.
	_, err = f.breaks.Add(ctx, "s", adbreak.AddRequest{Duration: 4 * time.Second, At: seg(72)})
	if overlap, ok := errors.AsType[*adbreak.OverlapError](err); !ok || overlap.ID != "r-2" {
		t.Fatalf("Add over r-2 = %v, want OverlapError r-2", err)
	}
	f.add(t, adbreak.AddRequest{Duration: 4 * time.Second, At: seg(72), ReplaceRecurring: true})
	// replaceRecurring on an active recurring break ends it early instead.
	f.edge(98) // horizon 102, r-3 is [100,105)
	f.add(t, adbreak.AddRequest{Duration: 4 * time.Second, ReplaceRecurring: true})
	list, _ = f.breaks.InRange(ctx, "s", 1, 60, 110)
	if ids(list) != "r-2[70,75)x 1[72,74) r-3[100,103) 2[103,105)" {
		t.Errorf("InRange after replacements = %s", ids(list))
	}

	// Stop: recurring breaks are trimmed at the end segment as computed.
	if _, err := f.streams.Stop(ctx, "s"); err != nil { // end segment 102
		t.Fatal(err)
	}
	list, _ = f.breaks.InRange(ctx, "s", 1, 95, 140)
	if ids(list) != "r-3[100,103) 2[103,105)x r-4[130,135)x" {
		t.Errorf("InRange after stop = %s", ids(list))
	}
	if _, err := f.breaks.InRange(ctx, "s", 2, 0, 10); !errors.Is(err, adbreak.ErrNotFound) {
		t.Errorf("InRange of another epoch = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	f := newFixture(t, nil)
	f.edge(9)
	old := f.add(t, adbreak.AddRequest{Duration: 4 * time.Second})                 // [14,16)
	far := f.add(t, adbreak.AddRequest{Duration: 4 * time.Second, At: seg(10000)}) // far ahead
	f.edge(40)                                                                     // the 30 s DVR window is [26,40]
	recent := f.add(t, adbreak.AddRequest{Duration: 4 * time.Second, At: seg(50)})
	list, err := f.breaks.List(t.Context(), "s")
	if err != nil || !slices.EqualFunc(list, []adbreak.Break{recent, far}, func(a, b adbreak.Break) bool { return a.ID == b.ID }) {
		t.Errorf("List = %s, %v; want %s and %s but not %s", ids(list), err, recent.ID, far.ID, old.ID)
	}
}
