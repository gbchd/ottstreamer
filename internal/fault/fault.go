// Package fault holds the fault rules of a stream: what requests they hit,
// how a hit is chosen (sticky or transient), what it does to the response,
// and the Service that keeps the active rules and counts what was injected.
// Faults change how a response is delivered, never what the media contains.
package fault

import (
	"cmp"
	"fmt"
	"hash/fnv"
	"slices"
	"time"
)

// Kind is the kind of request a rule hits.
type Kind string

const (
	KindSegment  Kind = "segment"
	KindInit     Kind = "init"
	KindPlaylist Kind = "playlist" // an HLS media playlist
	KindMPD      Kind = "mpd"
	KindMaster   Kind = "master" // the HLS multivariant playlist
	KindTime     Kind = "time"   // /api/time?stream=<id>; clock skew only
)

// Mode is how hits are chosen among matching requests.
type Mode string

const (
	// ModeSticky hits the same requests on every retry and every replica.
	ModeSticky Mode = "sticky"
	// ModeTransient draws at random per request, so a retry may succeed.
	ModeTransient Mode = "transient"
)

// Range is a range of segment numbers, both ends included. To nil is open.
type Range struct {
	From int64
	To   *int64
}

// Match selects the requests a rule applies to.
type Match struct {
	Kind       Kind
	Renditions []string // empty: every rendition
	Segments   *Range   // segment kind only; nil: every segment
}

// Action is what a hit does. Zero fields do nothing.
type Action struct {
	Status          int           // reply with this code and an empty body
	Delay           time.Duration // wait before the first byte
	ThrottleKbps    int           // send the body at this rate
	TruncatePercent int           // send this share of the body, then close
	Reset           bool          // close the connection without a response
	Stale           time.Duration // serve the manifest as it was this long ago
	Gap             bool          // 404, and EXT-X-GAP in HLS playlists
	ClockSkew       time.Duration // shift the Date header and /api/time
}

// Names returns the names of the actions a sets, as in the rule API.
func (a Action) Names() []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"status", a.Status != 0},
		{"delayMs", a.Delay != 0},
		{"throttleKbps", a.ThrottleKbps != 0},
		{"truncatePercent", a.TruncatePercent != 0},
		{"reset", a.Reset},
		{"staleSeconds", a.Stale != 0},
		{"gap", a.Gap},
		{"clockSkewSeconds", a.ClockSkew != 0},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// merge fills the zero fields of a from b: an earlier rule wins per action.
func (a Action) merge(b Action) Action {
	return Action{
		Status:          cmp.Or(a.Status, b.Status),
		Delay:           cmp.Or(a.Delay, b.Delay),
		ThrottleKbps:    cmp.Or(a.ThrottleKbps, b.ThrottleKbps),
		TruncatePercent: cmp.Or(a.TruncatePercent, b.TruncatePercent),
		Reset:           a.Reset || b.Reset,
		Stale:           cmp.Or(a.Stale, b.Stale),
		Gap:             a.Gap || b.Gap,
		ClockSkew:       cmp.Or(a.ClockSkew, b.ClockSkew),
	}
}

// Rule is one fault rule.
type Rule struct {
	ID      string
	Match   Match
	Percent int  // share of matching requests hit, 1-100
	Mode    Mode // default sticky
	Action  Action
	TTL     time.Duration // 0: the rule stays until replaced
	// ExpiresAt is set by the Service from TTL; zero never expires.
	ExpiresAt time.Time
}

// Clone returns a deep copy of r.
func (r Rule) Clone() Rule {
	r.Match.Renditions = slices.Clone(r.Match.Renditions)
	if s := r.Match.Segments; s != nil {
		c := *s
		if s.To != nil {
			to := *s.To
			c.To = &to
		}
		r.Match.Segments = &c
	}
	return r
}

// Clone returns a deep copy of rules.
func Clone(rules []Rule) []Rule {
	if rules == nil {
		return nil
	}
	out := make([]Rule, len(rules))
	for i, r := range rules {
		out[i] = r.Clone()
	}
	return out
}

// Target is one request, as fault rules see it.
type Target struct {
	StreamID  string
	Epoch     int
	Kind      Kind
	Rendition string // empty for master, mpd and time
	// Segment is N for segments, the live edge for playlists, the MPD and
	// the master playlist, and 0 for init and time.
	Segment int64
}

// matches reports whether r applies to t, before percent.
func (r Rule) matches(t Target) bool {
	m := r.Match
	if m.Kind != t.Kind {
		return false
	}
	if len(m.Renditions) > 0 && !slices.Contains(m.Renditions, t.Rendition) {
		return false
	}
	if s := m.Segments; s != nil && (t.Segment < s.From || (s.To != nil && t.Segment > *s.To)) {
		return false
	}
	return true
}

// sticky reports whether t is in r's sticky share:
// hash(stream, epoch, rule id, rendition, N) mod 100 < percent. It depends
// on nothing else, so every replica and every retry agree.
func (r Rule) sticky(t Target) bool {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\x00%d", t.StreamID, t.Epoch, r.ID, t.Rendition, t.Segment)
	return int(h.Sum64()%100) < r.Percent
}

// Gap reports whether a gap rule hits segment t, so an HLS playlist marks it
// with EXT-X-GAP. Gap rules are sticky, so the playlist and the segment
// request agree.
func Gap(rules []Rule, t Target) bool {
	for _, r := range rules {
		if r.Action.Gap && r.matches(t) && r.sticky(t) {
			return true
		}
	}
	return false
}
