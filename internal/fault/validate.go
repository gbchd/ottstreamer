package fault

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// MaxRules is the most rules a stream holds.
const MaxRules = 16

var ErrInvalid = errors.New("fault: invalid")

// FieldError is one invalid field, named by its path in the rule list (for
// example "faults[1].percent").
type FieldError struct {
	Field   string
	Message string
}

// ValidationError lists every invalid field. errors.Is(err, ErrInvalid) holds.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	msgs := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		msgs[i] = f.Field + ": " + f.Message
	}
	return "fault: invalid: " + strings.Join(msgs, "; ")
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

func (e *ValidationError) add(field, msg string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: msg})
}

var idPattern = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

var statuses = []int{404, 410, 500, 503, 504}

// Normalized returns a deep copy of rules with defaults filled in (percent
// 100, mode sticky), or a *ValidationError naming every invalid field.
func Normalized(rules []Rule) ([]Rule, error) {
	out := Clone(rules)
	var v ValidationError
	if len(out) > MaxRules {
		v.add("faults", fmt.Sprintf("at most %d rules", MaxRules))
	}
	for i := range out {
		r := &out[i]
		if r.Percent == 0 {
			r.Percent = 100
		}
		if r.Mode == "" {
			r.Mode = ModeSticky
		}
		path := fmt.Sprintf("faults[%d]", i)
		switch {
		case !idPattern.MatchString(r.ID):
			v.add(path+".id", "must match [a-z0-9-]{1,40}")
		case slices.IndexFunc(out, func(o Rule) bool { return o.ID == r.ID }) < i:
			v.add(path+".id", "duplicate rule id")
		}
		r.validate(&v, path)
	}
	if len(v.Fields) > 0 {
		return nil, &v
	}
	return out, nil
}

func (r Rule) validate(v *ValidationError, path string) {
	m := r.Match
	switch m.Kind {
	case KindSegment, KindInit, KindPlaylist, KindMPD, KindMaster, KindTime:
	default:
		v.add(path+".match.kind", `must be "segment", "init", "playlist", "mpd", "master" or "time"`)
	}
	if len(m.Renditions) > 0 && m.Kind != KindSegment && m.Kind != KindInit && m.Kind != KindPlaylist {
		v.add(path+".match.renditions", "only applies to segment, init and playlist")
	}
	if s := m.Segments; s != nil {
		switch {
		case m.Kind != KindSegment:
			v.add(path+".match.segments", "only applies to segment")
		case s.From < 0 || (s.To != nil && *s.To < s.From):
			v.add(path+".match.segments", "must have 0 <= from <= to")
		}
	}
	if r.Percent < 1 || r.Percent > 100 {
		v.add(path+".percent", "must be between 1 and 100")
	}
	if r.Mode != ModeSticky && r.Mode != ModeTransient {
		v.add(path+".mode", `must be "sticky" or "transient"`)
	}
	if r.TTL < 0 || r.TTL > 24*time.Hour {
		v.add(path+".ttlSeconds", "must be between 0 and 24 h")
	}
	r.Action.validate(v, path+".action", r)
}

func (a Action) validate(v *ValidationError, path string, r Rule) {
	kind := r.Match.Kind
	names := a.Names()
	if len(names) == 0 {
		v.add(path, "at least one action is required")
	}
	if kind == KindTime && slices.ContainsFunc(names, func(n string) bool { return n != "clockSkewSeconds" }) {
		v.add(path, "kind time takes clockSkewSeconds only")
	}
	if a.Status != 0 && !slices.Contains(statuses, a.Status) {
		v.add(path+".status", "must be 404, 410, 500, 503 or 504")
	}
	if a.Delay < 0 || a.Delay > time.Minute {
		v.add(path+".delayMs", "must be between 0 and 60000")
	}
	if a.ThrottleKbps < 0 || a.ThrottleKbps > 1_000_000 {
		v.add(path+".throttleKbps", "must be between 1 and 1000000")
	}
	switch {
	case a.TruncatePercent < 0 || a.TruncatePercent > 99:
		v.add(path+".truncatePercent", "must be between 1 and 99")
	case a.TruncatePercent != 0 && kind != KindSegment && kind != KindInit:
		v.add(path+".truncatePercent", "only applies to segment and init")
	}
	switch {
	case a.Stale < 0 || a.Stale > time.Hour:
		v.add(path+".staleSeconds", "must be between 0 and 3600")
	case a.Stale != 0 && kind != KindPlaylist && kind != KindMPD:
		v.add(path+".staleSeconds", "only applies to playlist and mpd")
	}
	switch {
	case a.Gap && kind != KindSegment:
		v.add(path+".gap", "only applies to segment")
	case a.Gap && r.Mode != ModeSticky:
		v.add(path+".gap", "needs mode sticky, so playlists and segments agree")
	}
	if a.ClockSkew < -24*time.Hour || a.ClockSkew > 24*time.Hour {
		v.add(path+".clockSkewSeconds", "must be within 24 h")
	}
	// A reply ends the request: at most one of them, and nothing that needs
	// the body.
	replies := 0
	for _, set := range []bool{a.Status != 0, a.Reset, a.Gap} {
		if set {
			replies++
		}
	}
	if replies > 1 {
		v.add(path, "status, reset and gap are exclusive")
	}
	if replies > 0 && (a.TruncatePercent != 0 || a.Stale != 0 || a.ThrottleKbps != 0) {
		v.add(path, "status, reset and gap send no body: truncatePercent, staleSeconds and throttleKbps cannot go with them")
	}
}
