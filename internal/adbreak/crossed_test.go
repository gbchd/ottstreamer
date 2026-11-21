package adbreak_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/event"
	"ottstreamer/internal/stream"
)

// describe writes events as "<segment> <type> <breakId> <source>".
func describe(events []event.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = fmt.Sprintf("%d %s %s %s", *e.Segment, e.Type, e.Data["breakId"], e.Data["source"])
	}
	return out
}

// crossings returns what Crossed reports for each segment of [from, to].
func (f *fixture) crossings(t *testing.T, from, to int64) []string {
	t.Helper()
	var all []event.Event
	for n := from; n <= to; n++ {
		events, err := f.breaks.Crossed(t.Context(), "s", 1, n)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, events...)
	}
	return describe(all)
}

func TestScheduleEvents(t *testing.T) {
	f := newFixture(t, recurringSpec) // r-0 [10,15), r-1 [40,45)
	f.edge(0)
	ctx := event.WithActor(t.Context(), "api:qa")
	if _, err := f.breaks.Add(ctx, "s", adbreak.AddRequest{Duration: 10 * time.Second, At: seg(20)}); err != nil { // 1 [20,25)
		t.Fatal(err)
	}
	if _, err := f.breaks.Add(ctx, "s", adbreak.AddRequest{Duration: 4 * time.Second, At: seg(42), ReplaceRecurring: true}); err != nil { // 2 [42,44)
		t.Fatal(err)
	}
	f.edge(8) // open 13
	if _, err := f.breaks.Delete(ctx, "s", "r-0"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.breaks.Delete(ctx, "s", "1"); err != nil {
		t.Fatal(err)
	}
	f.edge(40)                                                                                        // horizon 44: 2 is frozen whole
	if _, err := f.breaks.Add(ctx, "s", adbreak.AddRequest{Duration: 20 * time.Second}); err != nil { // 3 [45,55)
		t.Fatal(err)
	}
	f.edge(46)
	if _, err := f.streams.Stop(ctx, "s"); err != nil { // end 50
		t.Fatal(err)
	}

	got := f.events.Events()
	want := []string{
		"20 break.scheduled 1 manual",
		"40 break.cancelled r-1 recurring",
		"42 break.scheduled 2 manual",
		"13 break.shortened r-0 recurring",
		"20 break.cancelled 1 manual",
		"45 break.scheduled 3 manual",
		"51 break.shortened 3 manual",
		// Recurring breaks the stop cancels, up to 3 periods past it.
		"70 break.cancelled r-2 recurring",
		"100 break.cancelled r-3 recurring",
		"130 break.cancelled r-4 recurring",
	}
	var breaks []event.Event
	for _, e := range got {
		if e.Type != event.StreamStopped {
			breaks = append(breaks, e)
		}
	}
	if d := describe(breaks); !slices.Equal(d, want) {
		t.Errorf("events:\n got %q\nwant %q", d, want)
	}
	for _, e := range got {
		if e.Actor != "api:qa" || e.Epoch != 1 {
			t.Errorf("%s: actor %s epoch %d", e.Type, e.Actor, e.Epoch)
		}
	}
	if d := breaks[4].Data["durationSeconds"]; d != 10.0 {
		t.Errorf("durationSeconds = %v", d)
	}
}

func TestCrossed(t *testing.T) {
	f := newFixture(t, nil) // in-band, LeadSegs 4
	f.edge(0)               // open 5
	for _, at := range []int64{10, 20, 30} {
		f.add(t, adbreak.AddRequest{Duration: 6 * time.Second, At: seg(at)}) // 1..3: [at, at+3)
	}
	f.add(t, adbreak.AddRequest{Duration: 20 * time.Second, At: seg(40)}) // 4 [40,50)
	f.edge(12)                                                            // open 17
	for _, id := range []string{"2", "3"} {                               // 2 was announced at 16, 3 not yet
		if _, err := f.breaks.Delete(t.Context(), "s", id); err != nil {
			t.Fatal(err)
		}
	}
	f.edge(41) // open 46
	if _, err := f.breaks.Delete(t.Context(), "s", "4"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"6 signal.announced 1 manual",
		"10 break.started 1 manual", "10 signal.cue_out 1 manual",
		"13 break.ended 1 manual", "13 signal.cue_in 1 manual",
		"16 signal.announced 2 manual",
		"17 signal.cancel 2 manual",
		"36 signal.announced 4 manual",
		"40 break.started 4 manual", "40 signal.cue_out 4 manual",
		"46 break.ended 4 manual", "46 signal.cue_in 4 manual",
	}
	if got := f.crossings(t, 0, 60); !slices.Equal(got, want) {
		t.Errorf("crossings:\n got %q\nwant %q", got, want)
	}

	events, _ := f.breaks.Crossed(t.Context(), "s", 1, 10)
	d := events[1].Data
	if d["command"] != "splice_insert" || d["cue"] == "" || !slices.Equal(d["dash"].([]string), []string{"eventstream-bin", "inband"}) {
		t.Errorf("cue_out data = %v", d)
	}
}

func TestCrossed_noInband(t *testing.T) {
	f := newFixture(t, func(s *stream.Spec) { s.Ads.DASH = []string{"eventstream-bin"} })
	f.add(t, adbreak.AddRequest{Duration: 6 * time.Second, At: seg(10)})
	want := []string{"10 break.started 1 manual", "10 signal.cue_out 1 manual", "13 break.ended 1 manual", "13 signal.cue_in 1 manual"}
	if got := f.crossings(t, 0, 20); !slices.Equal(got, want) {
		t.Errorf("crossings:\n got %q\nwant %q", got, want)
	}
}

func TestCrossed_stopCutsBreak(t *testing.T) {
	f := newFixture(t, nil)
	f.add(t, adbreak.AddRequest{Duration: 20 * time.Second, At: seg(10)}) // [10,20)
	f.edge(10)
	if _, err := f.streams.Stop(t.Context(), "s"); err != nil { // end 14: the break ends at 15
		t.Fatal(err)
	}
	got := f.crossings(t, 13, 16)
	if want := []string{"15 break.ended 1 manual"}; !slices.Equal(got, want) {
		t.Errorf("crossings = %q, want %q: no cue-in after the last segment", got, want)
	}
}
