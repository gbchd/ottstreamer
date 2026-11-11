package faultmem_test

import (
	"testing"

	"ottstreamer/internal/fault"
	"ottstreamer/internal/fault/faultmem"
)

func TestRepository(t *testing.T) {
	repo := faultmem.NewRepository()
	in := []fault.Rule{{ID: "a", Match: fault.Match{Renditions: []string{"720p"}}}}
	if err := repo.Put(t.Context(), "demo", in); err != nil {
		t.Fatal(err)
	}
	in[0].Match.Renditions[0] = "x"
	got, err := repo.Get(t.Context(), "demo")
	if err != nil || len(got) != 1 || got[0].Match.Renditions[0] != "720p" {
		t.Fatalf("Get = %+v, %v; want the stored copy", got, err)
	}
	got[0].ID = "changed"
	if again, _ := repo.Get(t.Context(), "demo"); again[0].ID != "a" {
		t.Error("Get shares memory with the store")
	}
	if err := repo.Put(t.Context(), "demo", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Get(t.Context(), "demo"); got != nil {
		t.Errorf("Get after an empty Put = %+v, want nil", got)
	}
}
