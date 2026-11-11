// Package streamtest holds shared stream fixtures and the repository
// contract suite.
package streamtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"ottstreamer/internal/stream"
)

// Spec returns a small valid spec: HLS and DASH, two H.264 renditions and
// one AAC track, with defaults left for Normalize.
func Spec() stream.Spec {
	return stream.Spec{
		Formats:        []stream.Format{stream.FormatHLS, stream.FormatDASH},
		RenditionLabel: true,
		Video: []stream.Video{
			{ID: "720p", Width: 1280, Height: 720, BitrateKbps: 3000},
			{ID: "360p", Width: 640, Height: 360, BitrateKbps: 800},
		},
		Audio: []stream.Audio{
			{ID: "en", Lang: "en", Name: "English", Channels: 2, BitrateKbps: 128, Default: true},
		},
	}
}

// Stream returns a stopped stream with id and a normalized Spec(), not yet
// stored (Version 0).
func Stream(id string) stream.Stream {
	spec := Spec()
	spec.Normalize()
	return stream.Stream{ID: id, Name: "Test " + id, Spec: spec, Status: stream.Status{State: stream.StateStopped}}
}

// Store is a repository under contract test.
type Store interface {
	stream.Repository
	stream.Deleter
}

// RunRepositoryContract checks that a Store behaves like the reference
// in-memory one. newRepo must return an empty store on every call.
func RunRepositoryContract(t *testing.T, newRepo func() Store) {
	ctx := context.Background()

	t.Run("create sets version 1", func(t *testing.T) {
		r := newRepo()
		got, err := r.Save(ctx, Stream("a"), 0)
		if err != nil || got.Version != 1 {
			t.Fatalf("Save = %+v, %v; want version 1", got, err)
		}
		if _, err := r.Save(ctx, Stream("a"), 0); !errors.Is(err, stream.ErrExists) {
			t.Fatalf("second create: %v, want ErrExists", err)
		}
	})

	t.Run("get unknown", func(t *testing.T) {
		if _, err := newRepo().Get(ctx, "nope"); !errors.Is(err, stream.ErrNotFound) {
			t.Fatalf("Get: %v, want ErrNotFound", err)
		}
	})

	t.Run("update is compare-and-swap", func(t *testing.T) {
		r := newRepo()
		s, _ := r.Save(ctx, Stream("a"), 0)
		s.Name = "renamed"
		s, err := r.Save(ctx, s, 1)
		if err != nil || s.Version != 2 {
			t.Fatalf("update = %+v, %v; want version 2", s, err)
		}
		if _, err := r.Save(ctx, s, 1); !errors.Is(err, stream.ErrVersionMismatch) {
			t.Fatalf("stale update: %v, want ErrVersionMismatch", err)
		}
		got, err := r.Get(ctx, "a")
		if err != nil || got.Name != "renamed" || got.Version != 2 {
			t.Fatalf("Get = %+v, %v", got, err)
		}
		if _, err := r.Save(ctx, Stream("b"), 1); !errors.Is(err, stream.ErrNotFound) {
			t.Fatalf("update unknown: %v, want ErrNotFound", err)
		}
	})

	t.Run("deep copies in and out", func(t *testing.T) {
		r := newRepo()
		in := Stream("a")
		end := int64(7)
		in.Status.Epoch = &stream.Epoch{Number: 1, AST: time.Unix(0, 0), EndSegment: &end, Spec: in.Spec.Clone()}
		out, err := r.Save(ctx, in, 0)
		if err != nil {
			t.Fatal(err)
		}
		in.Spec.Video[0].ID = "changed"
		out.Spec.Formats[0] = "changed"
		*out.Status.Epoch.EndSegment = 99
		got, _ := r.Get(ctx, "a")
		got.Spec.Audio[0].ID = "changed"
		got, _ = r.Get(ctx, "a")
		if got.Spec.Video[0].ID == "changed" || got.Spec.Formats[0] == "changed" ||
			got.Spec.Audio[0].ID == "changed" || *got.Status.Epoch.EndSegment != 7 {
			t.Fatalf("stored stream shares memory with a caller: %+v", got)
		}
	})

	t.Run("list sorted by id", func(t *testing.T) {
		r := newRepo()
		for _, id := range []string{"c", "a", "b"} {
			if _, err := r.Save(ctx, Stream(id), 0); err != nil {
				t.Fatal(err)
			}
		}
		list, err := r.List(ctx)
		if err != nil || len(list) != 3 || list[0].ID != "a" || list[1].ID != "b" || list[2].ID != "c" {
			t.Fatalf("List = %v, %v", list, err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		r := newRepo()
		_, _ = r.Save(ctx, Stream("a"), 0)
		_, _ = r.Save(ctx, Stream("b"), 0)
		if err := r.Delete(ctx, "a", 2); !errors.Is(err, stream.ErrVersionMismatch) {
			t.Fatalf("stale delete: %v, want ErrVersionMismatch", err)
		}
		if err := r.Delete(ctx, "a", 1); err != nil {
			t.Fatalf("delete at version: %v", err)
		}
		if err := r.Delete(ctx, "b", 0); err != nil {
			t.Fatalf("unconditional delete: %v", err)
		}
		if err := r.Delete(ctx, "a", 0); !errors.Is(err, stream.ErrNotFound) {
			t.Fatalf("delete unknown: %v, want ErrNotFound", err)
		}
		if _, err := r.Get(ctx, "a"); !errors.Is(err, stream.ErrNotFound) {
			t.Fatalf("Get after delete: %v, want ErrNotFound", err)
		}
	})
}
