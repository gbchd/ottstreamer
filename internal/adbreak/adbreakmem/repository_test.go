package adbreakmem_test

import (
	"testing"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakmem"
)

func TestRepository(t *testing.T) {
	ctx := t.Context()
	r := adbreakmem.NewRepository()
	in := []adbreak.Break{
		{ID: "2", StreamID: "a", Epoch: 1, StartSeg: 50, CueOut: []byte{1}},
		{ID: "1", StreamID: "a", Epoch: 1, StartSeg: 10},
		{ID: "1", StreamID: "a", Epoch: 2, StartSeg: 5},
		{ID: "1", StreamID: "b", Epoch: 1, StartSeg: 5},
	}
	for _, b := range in {
		if err := r.Save(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	in[0].CueOut[0] = 9 // must not reach the store

	got, err := r.List(ctx, "a", 1)
	if err != nil || len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("List = %+v, %v; want breaks 1, 2", got, err)
	}
	if got[1].CueOut[0] != 1 {
		t.Errorf("stored cue shares memory with the caller")
	}
	got[1].CueOut[0] = 7
	if again, _ := r.List(ctx, "a", 1); again[1].CueOut[0] != 1 {
		t.Errorf("listed cue shares memory with the store")
	}

	upd := in[1]
	upd.Cancelled = true
	if err := r.Save(ctx, upd); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.List(ctx, "a", 1); len(got) != 2 || !got[0].Cancelled {
		t.Errorf("Save did not replace break 1: %+v", got)
	}

	if err := r.DeleteAll(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for _, epoch := range []int{1, 2} {
		if got, _ := r.List(ctx, "a", epoch); len(got) != 0 {
			t.Errorf("epoch %d after DeleteAll: %+v", epoch, got)
		}
	}
	if got, _ := r.List(ctx, "b", 1); len(got) != 1 {
		t.Errorf("DeleteAll removed another stream's breaks: %+v", got)
	}
}
