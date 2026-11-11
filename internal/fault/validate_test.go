package fault

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalized_Defaults(t *testing.T) {
	in := []Rule{{ID: "a", Match: Match{Kind: KindSegment}, Action: Action{Status: 503}}}
	got, err := Normalized(in)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Percent != 100 || got[0].Mode != ModeSticky {
		t.Errorf("defaults = %d %q, want 100 sticky", got[0].Percent, got[0].Mode)
	}
	if in[0].Percent != 0 {
		t.Error("Normalized modified its argument")
	}
}

func TestNormalized_Invalid(t *testing.T) {
	ok := Rule{ID: "a", Match: Match{Kind: KindSegment}, Action: Action{Status: 503}}
	with := func(f func(*Rule)) []Rule {
		r := ok.Clone()
		f(&r)
		return []Rule{r}
	}
	tests := []struct {
		name  string
		rules []Rule
		field string
	}{
		{"too many", make([]Rule, MaxRules+1), "faults"},
		{"bad id", with(func(r *Rule) { r.ID = "A b" }), "faults[0].id"},
		{"duplicate id", []Rule{ok, ok}, "faults[1].id"},
		{"bad kind", with(func(r *Rule) { r.Match.Kind = "video" }), "faults[0].match.kind"},
		{"renditions on mpd", with(func(r *Rule) { r.Match.Kind = KindMPD; r.Match.Renditions = []string{"720p"} }), "faults[0].match.renditions"},
		{"segments on init", with(func(r *Rule) { r.Match.Kind = KindInit; r.Match.Segments = &Range{} }), "faults[0].match.segments"},
		{"reversed range", with(func(r *Rule) { r.Match.Segments = &Range{From: 5, To: new(int64(4))} }), "faults[0].match.segments"},
		{"percent", with(func(r *Rule) { r.Percent = 101 }), "faults[0].percent"},
		{"mode", with(func(r *Rule) { r.Mode = "sometimes" }), "faults[0].mode"},
		{"ttl", with(func(r *Rule) { r.TTL = 25 * time.Hour }), "faults[0].ttlSeconds"},
		{"no action", with(func(r *Rule) { r.Action = Action{} }), "faults[0].action"},
		{"status", with(func(r *Rule) { r.Action.Status = 418 }), "faults[0].action.status"},
		{"delay", with(func(r *Rule) { r.Action = Action{Delay: 2 * time.Minute} }), "faults[0].action.delayMs"},
		{"truncate on playlist", with(func(r *Rule) { r.Match.Kind = KindPlaylist; r.Action = Action{TruncatePercent: 50} }), "faults[0].action.truncatePercent"},
		{"stale on segment", with(func(r *Rule) { r.Action = Action{Stale: time.Second} }), "faults[0].action.staleSeconds"},
		{"gap on init", with(func(r *Rule) { r.Match.Kind = KindInit; r.Action = Action{Gap: true} }), "faults[0].action.gap"},
		{"transient gap", with(func(r *Rule) { r.Mode = ModeTransient; r.Action = Action{Gap: true} }), "faults[0].action.gap"},
		{"time takes skew only", with(func(r *Rule) { r.Match.Kind = KindTime }), "faults[0].action"},
		{"status and reset", with(func(r *Rule) { r.Action.Reset = true }), "faults[0].action"},
		{"status and truncate", with(func(r *Rule) { r.Action.TruncatePercent = 50 }), "faults[0].action"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalized(tt.rules)
			verr, ok := errors.AsType[*ValidationError](err)
			if !ok || !errors.Is(err, ErrInvalid) {
				t.Fatalf("Normalized = %v, want a *ValidationError", err)
			}
			for _, f := range verr.Fields {
				if f.Field == tt.field {
					return
				}
			}
			t.Errorf("Normalized = %v, want an error on %s", err, tt.field)
		})
	}
}

func TestNormalized_Valid(t *testing.T) {
	rules := []Rule{
		{ID: "slow-720p", Match: Match{Kind: KindSegment, Renditions: []string{"720p"}, Segments: &Range{}}, Percent: 10, Action: Action{Delay: 3 * time.Second, ThrottleKbps: 500}},
		{ID: "cut", Match: Match{Kind: KindInit}, Mode: ModeTransient, Action: Action{TruncatePercent: 50}},
		{ID: "old", Match: Match{Kind: KindMPD}, Action: Action{Stale: 10 * time.Second, ClockSkew: -5 * time.Second}},
		{ID: "holes", Match: Match{Kind: KindSegment}, Percent: 5, Action: Action{Gap: true}},
		{ID: "skew", Match: Match{Kind: KindTime}, Action: Action{ClockSkew: 90 * time.Second}, TTL: time.Hour},
	}
	if _, err := Normalized(rules); err != nil {
		t.Errorf("Normalized = %v", strings.ReplaceAll(err.Error(), "; ", "\n"))
	}
}
