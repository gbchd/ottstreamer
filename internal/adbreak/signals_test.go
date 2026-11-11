package adbreak_test

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/stream"
)

// signalsAt summarizes the emsg list of segment n as "kind id@splice", with
// the splice point in segments.
func (f *fixture) signalsAt(t *testing.T, n int64) string {
	t.Helper()
	sigs, err := f.breaks.Signals(t.Context(), "s", 1, n)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(sigs))
	for i, s := range sigs {
		kind, _, _ := strings.Cut(string(s.Message), " ")
		out[i] = fmt.Sprintf("%s %d@%d", kind, s.EventID, s.PresentationTicks/segTicks)
	}
	return strings.Join(out, ",")
}

// placement returns the summaries of segments [from, to), keyed by segment,
// leaving out the segments that carry nothing.
func (f *fixture) placement(t *testing.T, from, to int64) map[int64]string {
	t.Helper()
	got := map[int64]string{}
	for n := from; n < to; n++ {
		if s := f.signalsAt(t, n); s != "" {
			got[n] = s
		}
	}
	return got
}

func TestSignalsPlacement(t *testing.T) {
	tests := []struct {
		name   string
		action func(t *testing.T, f *fixture)
		want   map[int64]string
	}{
		{
			name: "lead before start and end",
			action: func(t *testing.T, f *fixture) {
				f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(30)}) // [30,35)
			},
			want: map[int64]string{26: "out 1@30", 27: "out 1@30", 28: "out 1@30", 29: "out 1@30",
				31: "in 1@35", 32: "in 1@35", 33: "in 1@35", 34: "in 1@35"},
		},
		{
			name: "at the earliest start the cue-out rides in the first segment",
			action: func(t *testing.T, f *fixture) {
				f.add(t, adbreak.AddRequest{Duration: 10 * time.Second}) // [14,19), open 14
			},
			want: map[int64]string{14: "out 1@14", 15: "in 1@19", 16: "in 1@19", 17: "in 1@19", 18: "in 1@19"},
		},
		{
			name: "cancel after the cue-out went out",
			action: func(t *testing.T, f *fixture) {
				f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(30)})
				f.edge(23) // open 28
				if _, err := f.breaks.Delete(t.Context(), "s", "1"); err != nil {
					t.Fatal(err)
				}
			},
			want: map[int64]string{26: "out 1@30", 27: "out 1@30", 28: "cancel 1@30", 29: "cancel 1@30"},
		},
		{
			name: "cancel before the cue-out went out",
			action: func(t *testing.T, f *fixture) {
				f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(60)})
				if _, err := f.breaks.Delete(t.Context(), "s", "1"); err != nil {
					t.Fatal(err)
				}
			},
			want: map[int64]string{},
		},
		{
			name: "early return",
			action: func(t *testing.T, f *fixture) {
				f.add(t, adbreak.AddRequest{Duration: 30 * time.Second}) // [14,29)
				f.edge(22)                                               // open 27
				if _, err := f.breaks.Delete(t.Context(), "s", "1"); err != nil {
					t.Fatal(err)
				}
			},
			want: map[int64]string{14: "out 1@14", 25: "in 1@29", 26: "in 1@29", 27: "in 1@27"},
		},
		{
			name: "recurring breaks from the epoch start",
			action: func(t *testing.T, f *fixture) {
				if _, err := f.breaks.Delete(t.Context(), "s", "r-1"); err != nil {
					t.Fatal(err) // cancelled before announced: nothing
				}
			},
			want: map[int64]string{
				6: "out 2147483648@10", 7: "out 2147483648@10", 8: "out 2147483648@10", 9: "out 2147483648@10",
				11: "in 2147483648@15", 12: "in 2147483648@15", 13: "in 2147483648@15", 14: "in 2147483648@15",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, nil)
			if strings.HasPrefix(tt.name, "recurring") {
				f = newFixture(t, recurringSpec)
			} else {
				f.edge(9)
			}
			tt.action(t, f)
			got := f.placement(t, 0, 50)
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("signals\n got %v\nwant %v", got, tt.want)
			}
		})
	}
}

func TestSignalsFields(t *testing.T) {
	f := newFixture(t, nil)
	f.edge(9)
	f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(30)})
	sigs, err := f.breaks.Signals(t.Context(), "s", 1, 27)
	want := adbreak.Signal{EventID: 1, PresentationTicks: 30 * segTicks, DurationTicks: 5 * segTicks,
		Message: fmt.Appendf(nil, "out 1 %d %d splice_insert", 30*segTicks, 5*segTicks)}
	if err != nil || len(sigs) != 1 || fmt.Sprint(sigs[0]) != fmt.Sprint(want) {
		t.Errorf("Signals = %+v, %v; want %+v", sigs, err, want)
	}
	if sigs, _ := f.breaks.Signals(t.Context(), "s", 1, 31); len(sigs) != 1 || sigs[0].DurationTicks != 0 {
		t.Errorf("cue-in signal = %+v, want duration 0", sigs)
	}
}

func TestSignalsNeedInband(t *testing.T) {
	f := newFixture(t, func(s *stream.Spec) { s.Ads.DASH = []string{"eventstream-bin"} })
	f.edge(9)
	f.add(t, adbreak.AddRequest{Duration: 10 * time.Second, At: seg(30)})
	if got := f.placement(t, 0, 50); len(got) != 0 {
		t.Errorf("signals without inband: %v", got)
	}
}

// TestScheduleNeverChangesFrozenSegments drives random adds, cancels and
// early returns at advancing times, then a stop, and checks that no change
// ever alters the ad state or the emsg list of a segment that was already
// frozen when it was made.
func TestScheduleNeverChangesFrozenSegments(t *testing.T) {
	for seed := range uint64(12) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 1))
			f := newFixture(t, func(s *stream.Spec) {
				s.Ads.Recurring = &stream.Recurring{Every: 30 * time.Second, Duration: 8 * time.Second, Offset: 10 * time.Second}
			})
			frozen := map[int64]string{} // segment -> state when first frozen
			check := func(when string) {
				t.Helper()
				st, err := f.streams.Get(t.Context(), "s")
				if err != nil {
					t.Fatal(err)
				}
				horizon := st.Status.Epoch.Timeline().FrozenHorizon(f.now)
				for n := int64(0); n <= horizon; n++ {
					got := f.state(t, n)
					if want, ok := frozen[n]; !ok {
						frozen[n] = got
					} else if got != want {
						t.Fatalf("%s: frozen segment %d (horizon %d) changed\n got %s\nwant %s", when, n, horizon, got, want)
					}
				}
			}
			for step := range 50 {
				f.now = f.now.Add(time.Duration(rng.IntN(4000)) * time.Millisecond)
				check(fmt.Sprintf("step %d before", step))
				for range rng.IntN(3) {
					f.mutate(t, rng)
				}
				check(fmt.Sprintf("step %d after", step))
			}
			if _, err := f.streams.Stop(t.Context(), "s"); err != nil {
				t.Fatal(err)
			}
			check("after stop")
		})
	}
}

// state is the ad state and emsg list of segment n.
func (f *fixture) state(t *testing.T, n int64) string {
	t.Helper()
	list, err := f.breaks.InRange(t.Context(), "s", 1, n, n+1)
	if err != nil {
		t.Fatal(err)
	}
	ad := ""
	for _, b := range list {
		if b.Covers(n) {
			ad += "ad " + b.ID + ";"
		}
	}
	sigs, err := f.breaks.Signals(t.Context(), "s", 1, n)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s%+v", ad, sigs)
}

// mutate adds a break, or deletes a manual or recurring one; refusals are
// part of the game.
func (f *fixture) mutate(t *testing.T, rng *rand.Rand) {
	t.Helper()
	ctx := t.Context()
	switch rng.IntN(3) {
	case 0:
		_, _ = f.breaks.Add(ctx, "s", adbreak.AddRequest{
			Duration:         time.Duration(1+rng.IntN(20)) * time.Second,
			In:               ptr(time.Duration(rng.IntN(40)) * time.Second),
			ReplaceRecurring: rng.IntN(2) == 0,
		})
	case 1:
		list, err := f.breaks.List(ctx, "s")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 {
			_, _ = f.breaks.Delete(ctx, "s", list[rng.IntN(len(list))].ID)
		}
	default:
		_, _ = f.breaks.Delete(ctx, "s", fmt.Sprintf("r-%d", rng.IntN(8)))
	}
}
