package fault

import (
	"slices"
	"testing"
	"time"
)

func TestAction_Names(t *testing.T) {
	a := Action{Status: 503, Delay: time.Second, Gap: true}
	if got, want := a.Names(), []string{"status", "delayMs", "gap"}; !slices.Equal(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
}

func TestAction_merge(t *testing.T) {
	got := Action{Status: 503}.merge(Action{Status: 404, Delay: time.Second})
	if want := (Action{Status: 503, Delay: time.Second}); got != want {
		t.Errorf("merge = %+v, want %+v (earlier rule wins per action)", got, want)
	}
}

func TestRule_Clone(t *testing.T) {
	r := Rule{Match: Match{Renditions: []string{"720p"}, Segments: &Range{To: new(int64(9))}}}
	c := r.Clone()
	c.Match.Renditions[0] = "x"
	*c.Match.Segments.To = 1
	if r.Match.Renditions[0] != "720p" || *r.Match.Segments.To != 9 {
		t.Errorf("Clone shares memory: %+v", r.Match)
	}
}

func TestRule_matches(t *testing.T) {
	seg := Target{Kind: KindSegment, Rendition: "720p", Segment: 5}
	tests := []struct {
		name  string
		match Match
		t     Target
		want  bool
	}{
		{"kind and rendition", Match{Kind: KindSegment, Renditions: []string{"720p"}}, seg, true},
		{"every rendition", Match{Kind: KindSegment}, seg, true},
		{"other kind", Match{Kind: KindInit}, seg, false},
		{"other rendition", Match{Kind: KindSegment, Renditions: []string{"360p"}}, seg, false},
		{"in range", Match{Kind: KindSegment, Segments: &Range{From: 5, To: new(int64(5))}}, seg, true},
		{"open range", Match{Kind: KindSegment, Segments: &Range{From: 3}}, seg, true},
		{"before range", Match{Kind: KindSegment, Segments: &Range{From: 6}}, seg, false},
		{"after range", Match{Kind: KindSegment, Segments: &Range{To: new(int64(4))}}, seg, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Rule{Match: tt.match}).matches(tt.t); got != tt.want {
				t.Errorf("matches = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRule_sticky(t *testing.T) {
	r := Rule{ID: "r", Percent: 10}
	hits := 0
	for n := range int64(10000) {
		if r.sticky(Target{StreamID: "demo", Epoch: 1, Rendition: "720p", Segment: n}) {
			hits++
		}
	}
	if hits < 800 || hits > 1200 {
		t.Errorf("10%% sticky rule hit %d of 10000 segments", hits)
	}
	all := Rule{ID: "r", Percent: 100}
	if !all.sticky(Target{Segment: 3}) {
		t.Error("100% rule missed")
	}
}

func TestGap(t *testing.T) {
	gap := Rule{ID: "g", Match: Match{Kind: KindSegment, Segments: &Range{From: 10, To: new(int64(11))}}, Percent: 100, Mode: ModeSticky, Action: Action{Gap: true}}
	other := Rule{ID: "s", Match: Match{Kind: KindSegment}, Percent: 100, Mode: ModeSticky, Action: Action{Status: 503}}
	rules := []Rule{other, gap}
	for n, want := range map[int64]bool{9: false, 10: true, 11: true, 12: false} {
		if got := Gap(rules, Target{Kind: KindSegment, Segment: n}); got != want {
			t.Errorf("Gap(%d) = %v, want %v", n, got, want)
		}
	}
}
