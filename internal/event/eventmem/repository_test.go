package eventmem

import (
	"fmt"
	"slices"
	"testing"

	"ottstreamer/internal/event"
)

func ids(events []event.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}

func TestRepository(t *testing.T) {
	r := NewRepository(3)
	for i := 1; i <= 5; i++ {
		typ := event.SegmentLate
		if i%2 == 0 {
			typ = event.SegmentRetried
		}
		if err := r.Append(t.Context(), event.Event{ID: fmt.Sprint(i), StreamID: "a", Type: typ}); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Append(t.Context(), event.Event{ID: "9", StreamID: "b"})

	tests := []struct {
		name string
		q    event.Query
		want []string
	}{
		{"the last three, oldest first", event.Query{}, []string{"3", "4", "5"}},
		{"limit", event.Query{Limit: 2}, []string{"3", "4"}},
		{"after a cursor", event.Query{After: "3"}, []string{"4", "5"}},
		{"by type", event.Query{Types: []event.Type{event.SegmentLate}}, []string{"3", "5"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.List(t.Context(), "a", tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ids(got), tt.want) {
				t.Errorf("List = %v, want %v", ids(got), tt.want)
			}
		})
	}
	if got, _ := r.List(t.Context(), "none", event.Query{}); got != nil {
		t.Errorf("unknown stream: %v", got)
	}
}
