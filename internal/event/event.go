// Package event keeps the per-stream event log: every state change, who
// caused it, and what the producer saw as the live edge moved. The log is the
// history the UI draws and an audit trail; media never depends on it.
package event

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
)

// Type names what happened, as "<group>.<what>".
type Type string

const (
	StreamCreated Type = "stream.created"
	StreamUpdated Type = "stream.updated" // data: fields, the changed field paths; changes, a stream.Change
	StreamDeleted Type = "stream.deleted"
	StreamStarted Type = "stream.started"
	StreamStopped Type = "stream.stopped"
	StreamExpired Type = "stream.expired" // ended -> stopped

	BreakScheduled Type = "break.scheduled"
	BreakCancelled Type = "break.cancelled"
	BreakShortened Type = "break.shortened"
	BreakStarted   Type = "break.started" // the live edge reached the break
	BreakEnded     Type = "break.ended"

	SignalAnnounced Type = "signal.announced" // first segment carrying the emsg
	SignalCueOut    Type = "signal.cue_out"
	SignalCueIn     Type = "signal.cue_in"
	SignalCancel    Type = "signal.cancel"

	FaultsUpdated Type = "faults.updated" // data: rules, the IDs of the new rule set; expired, the IDs a TTL removed
	FaultInjected Type = "fault.injected" // data: rule, actions, count, minute; one per rule and minute

	SegmentLate    Type = "segment.late"
	SegmentRetried Type = "segment.retried"
	FFmpegFailed   Type = "ffmpeg.failed" // data: error, ending with ffmpeg's stderr tail
	RuntimeResumed Type = "runtime.resumed"
)

// System is the actor of what the server does on its own.
const System = "system"

// Event is one entry of a stream's log. Events are values: nobody modifies
// one, its Data included, after it is recorded.
type Event struct {
	ID       string // ULID: sorts by time, unique across replicas
	StreamID string
	Epoch    int // 0 for a stream event before its first start
	At       time.Time
	Segment  *int64 // media position, when there is one
	Type     Type
	Actor    string // "api:<key name>" or System
	Data     map[string]any
}

type actorKey struct{}

// WithActor returns ctx with the actor of the events it causes.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Actor returns the actor set by WithActor, or System.
func Actor(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return cmp.Or(a, System)
}

// Query selects events of one stream.
type Query struct {
	After    string    // cursor: only events with a greater ID
	From, To time.Time // At in [From, To); a zero bound is open
	Types    []Type    // empty: every type; "<group>.*" selects a whole group
	Limit    int       // page size
}

// Match reports whether e is selected by q, Limit aside.
func (q Query) Match(e Event) bool {
	return e.ID > q.After &&
		(q.From.IsZero() || !e.At.Before(q.From)) &&
		(q.To.IsZero() || e.At.Before(q.To)) &&
		(len(q.Types) == 0 || slices.ContainsFunc(q.Types, e.Type.is))
}

// is reports whether t is selected by p: p itself, or "<group>.*" for t in
// that group.
func (t Type) is(p Type) bool {
	group, ok := strings.CutSuffix(string(p), ".*")
	return t == p || ok && strings.HasPrefix(string(t), group+".")
}
