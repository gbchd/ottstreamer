package event_test

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"ottstreamer/internal/event"
	"ottstreamer/internal/event/eventmem"
)

var t0 = time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)

func TestQuery_Match(t *testing.T) {
	e := event.Event{ID: "02", At: t0, Type: event.BreakStarted}
	tests := []struct {
		name string
		q    event.Query
		want bool
	}{
		{"empty query", event.Query{}, true},
		{"after an earlier id", event.Query{After: "01"}, true},
		{"after its own id", event.Query{After: "02"}, false},
		{"from its time", event.Query{From: t0}, true},
		{"from later", event.Query{From: t0.Add(time.Millisecond)}, false},
		{"to its time, exclusive", event.Query{To: t0}, false},
		{"to later", event.Query{To: t0.Add(time.Millisecond)}, true},
		{"its type", event.Query{Types: []event.Type{event.BreakEnded, event.BreakStarted}}, true},
		{"other types", event.Query{Types: []event.Type{event.BreakEnded}}, false},
		{"its group", event.Query{Types: []event.Type{"signal.*", "break.*"}}, true},
		{"another group", event.Query{Types: []event.Type{"stream.*"}}, false},
		{"a group prefixing its own", event.Query{Types: []event.Type{"brea.*", "break"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.q.Match(e); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

// newService returns a Service whose IDs count up from 001, on a clock that
// ticks one second per event, and the events its observer saw.
func newService(t *testing.T) (*event.Service, *[]event.Event) {
	t.Helper()
	var seen []event.Event
	n := 0
	now := func() time.Time { n++; return t0.Add(time.Duration(n) * time.Second) }
	newID := func(at time.Time) string { return fmt.Sprintf("%03d", at.Sub(t0)/time.Second) }
	svc := event.NewService(eventmem.NewRepository(100), now, newID, func(e event.Event) { seen = append(seen, e) }, slog.New(slog.DiscardHandler))
	return svc, &seen
}

func TestService_Record(t *testing.T) {
	svc, seen := newService(t)
	svc.Record(t.Context(), event.Event{StreamID: "a", Type: event.StreamCreated})
	svc.Record(event.WithActor(t.Context(), "api:qa"), event.Event{StreamID: "a", Type: event.StreamStarted, Epoch: 1})

	got, _, err := svc.List(t.Context(), "a", event.Query{})
	if err != nil {
		t.Fatal(err)
	}
	want := []event.Event{
		{ID: "001", StreamID: "a", At: t0.Add(time.Second), Type: event.StreamCreated, Actor: event.System},
		{ID: "002", StreamID: "a", Epoch: 1, At: t0.Add(2 * time.Second), Type: event.StreamStarted, Actor: "api:qa"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events:\n got %v\nwant %v", got, want)
	}
	if fmt.Sprint(*seen) != fmt.Sprint(want) {
		t.Errorf("observed %v", *seen)
	}
}

func TestService_List(t *testing.T) {
	svc, _ := newService(t)
	for range 5 {
		svc.Record(t.Context(), event.Event{StreamID: "a", Type: event.SegmentLate})
	}
	var ids []string
	cursor := ""
	for page := 0; ; page++ {
		events, next, err := svc.List(t.Context(), "a", event.Query{After: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			ids = append(ids, e.ID)
		}
		if next == "" {
			break
		}
		if page > 5 {
			t.Fatal("no last page")
		}
		cursor = next
	}
	if want := []string{"001", "002", "003", "004", "005"}; !slices.Equal(ids, want) {
		t.Errorf("paged ids %v, want %v", ids, want)
	}
}

func TestService_Subscribe(t *testing.T) {
	svc, _ := newService(t)
	ctx, cancel := context.WithCancel(t.Context())
	a := svc.Subscribe(ctx, "a")
	svc.Record(t.Context(), event.Event{StreamID: "b", Type: event.StreamCreated})
	svc.Record(t.Context(), event.Event{StreamID: "a", Type: event.StreamCreated})
	if e := <-a; e.ID != "002" {
		t.Errorf("got %v, want only stream a's event", e)
	}
	cancel()
	if _, ok := <-a; ok {
		t.Error("channel open after ctx is done")
	}
}

func TestService_Subscribe_slow(t *testing.T) {
	svc, _ := newService(t)
	ch := svc.Subscribe(t.Context(), "a")
	for range 300 { // more than the subscriber buffer
		svc.Record(t.Context(), event.Event{StreamID: "a", Type: event.SegmentLate})
	}
	n := 0
	for range ch {
		n++
	}
	if n == 0 || n >= 300 {
		t.Errorf("a slow subscriber got %d events before its channel closed", n)
	}
}

func TestService_Close(t *testing.T) {
	svc, _ := newService(t)
	before := svc.Subscribe(t.Context(), "a")
	svc.Close()
	after := svc.Subscribe(t.Context(), "a")
	for name, ch := range map[string]<-chan event.Event{"before": before, "after": after} {
		if _, ok := <-ch; ok {
			t.Errorf("subscription %s Close is open", name)
		}
	}
	svc.Record(t.Context(), event.Event{StreamID: "a", Type: event.StreamCreated}) // still records
	if events, _, _ := svc.List(t.Context(), "a", event.Query{}); len(events) != 1 {
		t.Errorf("events after Close: %v", events)
	}
}
