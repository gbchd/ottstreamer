package live

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"ottstreamer/internal/adbreak"
)

func TestManager_Init(t *testing.T) {
	h := newHarness(nil)
	h.add(t, running("a", ast))
	got, err := h.m.Init("a", "720p")
	if err != nil || string(got) != "init 720p" {
		t.Fatalf("Init = %q, %v", got, err)
	}
	for _, tc := range []struct{ stream, rendition string }{{"b", "720p"}, {"a", "1080p"}} {
		if _, err := h.m.Init(tc.stream, tc.rendition); !errors.Is(err, ErrNotFound) {
			t.Errorf("Init(%s, %s) = %v, want ErrNotFound", tc.stream, tc.rendition, err)
		}
	}
}

func TestManager_WriteSegment_notFound(t *testing.T) {
	h := newHarness(nil)
	h.add(t, running("a", ast))
	h.m.End("a", 5)
	tests := []struct {
		name, stream, rendition string
		n                       int64
	}{
		{"unknown stream", "b", "720p", 0},
		{"unknown rendition", "a", "1080p", 0},
		{"negative", "a", "720p", -1},
		{"after the end", "a", "720p", 6},
		{"audio after the end", "a", "en", 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.m.WriteSegment(t.Context(), &strings.Builder{}, tt.stream, tt.rendition, tt.n)
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("WriteSegment = %v, want ErrNotFound", err)
			}
		})
	}
	if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", 5); err != nil {
		t.Fatalf("end segment: %v", err)
	}
}

func TestManager_WriteSegment_video(t *testing.T) {
	h := newHarness(nil)
	h.add(t, running("a", ast))
	sig := []adbreak.Signal{{EventID: 7, PresentationTicks: 540000, Message: []byte{0xfc}}}
	h.sched.signals = sig

	var out strings.Builder
	if err := h.m.WriteSegment(t.Context(), &out, "a", "720p", 3); err != nil {
		t.Fatal(err)
	}
	if out.String() != "segment 4" {
		t.Fatalf("wrote %q", out.String())
	}
	f := h.w.last()
	if f.Seq != 4 || f.TrackID != 1 || f.BaseTime != 3*180000 || f.SampleDur != 3600 {
		t.Fatalf("fragment = seq %d track %d tfdt %d dur %d", f.Seq, f.TrackID, f.BaseTime, f.SampleDur)
	}
	if len(f.Samples) != 50 || !f.Samples[0].Sync {
		t.Fatalf("%d samples, first sync %t; want 50, true", len(f.Samples), f.Samples[0].Sync)
	}
	if !slices.EqualFunc(f.Emsgs, sig, func(a, b adbreak.Signal) bool { return a.EventID == b.EventID }) {
		t.Fatalf("emsgs = %v, want %v", f.Emsgs, sig)
	}
	if f.PadTo != 750000 { // 3000 kbit/s × 2 s
		t.Fatalf("PadTo = %d, want 750000", f.PadTo)
	}

	if err := h.m.WriteSegment(t.Context(), &out, "a", "360p", 3); err != nil {
		t.Fatal(err)
	}
	if got := h.w.last().PadTo; got != 200000 { // 800 kbit/s × 2 s
		t.Fatalf("360p PadTo = %d, want 200000", got)
	}
	if got := len(h.enc.encoded()); got != 1 {
		t.Fatalf("%d encodes for two renditions of one segment, want 1", got)
	}
}

func TestManager_WriteSegment_audio(t *testing.T) {
	h := newHarness(nil)
	rt := h.add(t, running("a", ast))
	loop := rt.tracks["en"].Loop

	wantFirst := []int64{0, 94, 188, 282, 375} // ceil(n × 96000 / 1024)
	wantCount := []int{94, 94, 94, 93, 94}
	for n := range int64(5) {
		if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "en", n); err != nil {
			t.Fatal(err)
		}
		f := h.w.last()
		if f.BaseTime != uint64(wantFirst[n]*1024) || f.SampleDur != 1024 || len(f.Samples) != wantCount[n] {
			t.Fatalf("segment %d: tfdt %d dur %d, %d samples; want tfdt %d dur 1024, %d samples",
				n, f.BaseTime, f.SampleDur, len(f.Samples), wantFirst[n]*1024, wantCount[n])
		}
		if f.Emsgs != nil || f.PadTo != 0 {
			t.Fatalf("segment %d: audio has emsgs %v or padding %d", n, f.Emsgs, f.PadTo)
		}
		for i, s := range f.Samples {
			want := loop[(wantFirst[n]+int64(i))%375]
			if !s.Sync || string(s.Data) != string(want.Data) {
				t.Fatalf("segment %d sample %d = %q sync %t, want %q sync", n, i, s.Data, s.Sync, want.Data)
			}
		}
	}
	if got := string(h.w.last().Samples[0].Data); got != "frame 0" {
		t.Fatalf("segment 4 starts at %q, want the loop to wrap to frame 0", got)
	}
	if got := len(h.enc.encoded()); got != 0 {
		t.Fatalf("audio ran the encoder %d times", got)
	}
}

func TestManager_WriteSegment_singleEncode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(nil)
		h.add(t, running("a", ast))
		h.enc.gate = make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 5)
		for i := range errs {
			wg.Go(func() {
				errs[i] = h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", 20)
			})
		}
		synctest.Wait() // one caller in the encoder, the rest waiting on it
		close(h.enc.gate)
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		if got := len(h.enc.encoded()); got != 1 {
			t.Fatalf("%d encodes for 5 concurrent requests, want 1", got)
		}
	})
}

func TestManager_WriteSegment_cachedByProducer(t *testing.T) {
	h := newHarness(nil)
	rt := h.add(t, running("a", ast))
	h.m.produceUpTo(t.Context(), rt, 0, 3)
	for n := range int64(4) {
		if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "360p", n); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(h.enc.encoded()); got != 4 {
		t.Fatalf("%d encodes, want 4 (requests hit the cache)", got)
	}
}

func TestManager_WriteSegment_inAd(t *testing.T) {
	h := newHarness(nil)
	h.add(t, running("a", ast))
	write := func(n int64) {
		t.Helper()
		if err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", n); err != nil {
			t.Fatal(err)
		}
	}
	write(10)
	h.sched.set(adbreak.Break{ID: "1", StartSeg: 10, EndSeg: 12}) // added after 10 was cached
	write(10)
	write(10)
	write(12) // first segment after the break
	h.sched.set(adbreak.Break{ID: "1", StartSeg: 10, EndSeg: 12, Cancelled: true})
	write(10)

	want := []encodeCall{{10, false}, {10, true}, {12, false}}
	if got := h.enc.encoded(); !slices.Equal(got, want) {
		t.Fatalf("encoded %v, want %v", got, want)
	}
}

func TestManager_WriteSegment_retry(t *testing.T) {
	tests := []struct {
		name    string
		fail    int
		wantErr error
	}{
		{"recovers on retry", 1, nil},
		{"fails twice", 2, ErrUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(nil)
			h.add(t, running("a", ast))
			h.enc.fail = tt.fail
			err := h.m.WriteSegment(t.Context(), &strings.Builder{}, "a", "720p", 3)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("WriteSegment = %v, want %v", err, tt.wantErr)
			}
			if got := len(h.enc.encoded()); got != 2 {
				t.Fatalf("%d encode attempts, want 2", got)
			}
		})
	}
}
