package fault_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"ottstreamer/internal/event"
	"ottstreamer/internal/event/eventtest"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/fault/faultmem"
	"ottstreamer/internal/fault/faulttest"
)

var t0 = time.Date(2026, 11, 6, 18, 30, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type counter struct {
	mu sync.Mutex
	n  map[[3]string]int
}

func (c *counter) FaultInjected(streamID, rule, action string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[[3]string]int{}
	}
	c.n[[3]string{streamID, rule, action}]++
}

func newService(draw func() int) (*fault.Service, *eventtest.Recorder, *counter, *clock) {
	rec, met, clk := &eventtest.Recorder{}, &counter{}, &clock{t: t0}
	return fault.NewService(faultmem.NewRepository(), streams, rec, met, clk.now, draw), rec, met, clk
}

// streams answers for every stream: in epoch 3, with these renditions.
var streams = faulttest.Streams{Epoch: 3, Renditions: []string{"720p", "en", "sub-en"}}

func noDraw() int { panic("draw called for a sticky rule") }

func segmentRule(id string, percent int) fault.Rule {
	return fault.Rule{ID: id, Match: fault.Match{Kind: fault.KindSegment}, Percent: percent, Action: fault.Action{Status: 503}}
}

func TestService_Replace(t *testing.T) {
	svc, rec, _, clk := newService(noDraw)
	ctx := event.WithActor(t.Context(), "api:qa")
	r := segmentRule("a", 0)
	r.TTL = time.Minute
	got, err := svc.Replace(ctx, "demo", []fault.Rule{r, segmentRule("b", 5)})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Percent != 100 || !got[0].ExpiresAt.Equal(t0.Add(time.Minute)) || !got[1].ExpiresAt.IsZero() {
		t.Errorf("Replace = %+v, want defaults and expiry", got)
	}
	ev := rec.Events()
	if len(ev) != 1 || ev[0].Type != event.FaultsUpdated || ev[0].Actor != "api:qa" || ev[0].Epoch != 3 || !slices.Equal(ev[0].Data["rules"].([]string), []string{"a", "b"}) {
		t.Errorf("events = %+v, want one faults.updated by api:qa in epoch 3", ev)
	}

	clk.add(time.Minute)
	active, err := svc.Rules(t.Context(), "demo")
	if err != nil || len(active) != 1 || active[0].ID != "b" {
		t.Errorf("Rules after the TTL = %+v, %v; want only b", active, err)
	}

	if err := svc.Clear(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Clear(t.Context(), "demo"); err != nil {
		t.Fatal(err)
	}
	if types := rec.Types(); !slices.Equal(types, []event.Type{event.FaultsUpdated}) {
		t.Errorf("events of two clears = %v, want one faults.updated", types)
	}
}

func TestService_Replace_Invalid(t *testing.T) {
	svc, rec, _, _ := newService(noDraw)
	if _, err := svc.Replace(t.Context(), "demo", []fault.Rule{segmentRule("a", 200)}); err == nil {
		t.Fatal("Replace accepted percent 200")
	}
	if len(rec.Events()) != 0 {
		t.Error("an invalid rule set recorded an event")
	}
}

func TestService_Replace_UnknownRendition(t *testing.T) {
	svc, rec, _, _ := newService(noDraw)
	r := segmentRule("a", 0)
	r.Match.Renditions = []string{"720p", "en", "sub-en", "1080p"}
	_, err := svc.Replace(t.Context(), "demo", []fault.Rule{r})
	verr, ok := errors.AsType[*fault.ValidationError](err)
	if !ok || !errors.Is(err, fault.ErrInvalid) || len(verr.Fields) != 1 || verr.Fields[0].Field != "faults[0].match.renditions[3]" {
		t.Fatalf("Replace = %v, want one field error at faults[0].match.renditions[3]", err)
	}
	if len(rec.Events()) != 0 {
		t.Error("a rejected rule set recorded an event")
	}
}

func TestService_Prune(t *testing.T) {
	svc, rec, _, clk := newService(noDraw)
	ctx := event.WithActor(t.Context(), "api:qa")
	soon, later := segmentRule("soon", 0), segmentRule("later", 0)
	soon.TTL, later.TTL = time.Minute, 3*time.Minute
	if _, err := svc.Replace(ctx, "demo", []fault.Rule{soon, segmentRule("kept", 0), later}); err != nil {
		t.Fatal(err)
	}
	rec.Events()

	prune := func() []event.Event {
		t.Helper()
		if err := svc.Prune(ctx, "demo"); err != nil {
			t.Fatal(err)
		}
		return rec.Events()
	}
	if ev := prune(); len(ev) != 0 {
		t.Errorf("Prune before any expiry recorded %+v", ev)
	}

	clk.add(time.Minute)
	ev := prune()
	if len(ev) != 1 || ev[0].Type != event.FaultsUpdated || ev[0].Actor != event.System || ev[0].Epoch != 3 ||
		!slices.Equal(ev[0].Data["rules"].([]string), []string{"kept", "later"}) || !slices.Equal(ev[0].Data["expired"].([]string), []string{"soon"}) {
		t.Errorf("events = %+v, want one faults.updated by system: rules kept, later; expired soon", ev)
	}
	if ev := prune(); len(ev) != 0 {
		t.Errorf("second Prune recorded %+v", ev)
	}

	// Rules hides a rule that expired since the last Prune.
	clk.add(2 * time.Minute)
	if active, err := svc.Rules(t.Context(), "demo"); err != nil || len(active) != 1 || active[0].ID != "kept" {
		t.Errorf("Rules = %+v, %v; want only kept", active, err)
	}
	if ev := prune(); len(ev) != 1 || !slices.Equal(ev[0].Data["rules"].([]string), []string{"kept"}) || !slices.Equal(ev[0].Data["expired"].([]string), []string{"later"}) {
		t.Errorf("events = %+v, want later expired", ev)
	}
	if err := svc.Prune(t.Context(), "nope"); err != nil || len(rec.Events()) != 0 {
		t.Errorf("Prune of a stream without rules = %v", err)
	}
}

func TestService_Inject(t *testing.T) {
	svc, _, met, _ := newService(func() int { return 42 })
	slow := fault.Rule{ID: "slow", Match: fault.Match{Kind: fault.KindSegment}, Percent: 100, Mode: fault.ModeSticky, Action: fault.Action{Delay: time.Second, Status: 504}}
	lucky := fault.Rule{ID: "lucky", Match: fault.Match{Kind: fault.KindSegment}, Percent: 43, Mode: fault.ModeTransient, Action: fault.Action{Status: 503, ThrottleKbps: 100}}
	unlucky := fault.Rule{ID: "unlucky", Match: fault.Match{Kind: fault.KindSegment}, Percent: 42, Mode: fault.ModeTransient, Action: fault.Action{Reset: true}}
	initRule := fault.Rule{ID: "init", Match: fault.Match{Kind: fault.KindInit}, Percent: 100, Mode: fault.ModeSticky, Action: fault.Action{Reset: true}}

	in := svc.Inject(t.Context(), []fault.Rule{slow, lucky, unlucky, initRule}, fault.Target{StreamID: "demo", Kind: fault.KindSegment, Segment: 7})
	if !slices.Equal(in.Rules, []string{"slow", "lucky"}) {
		t.Errorf("rules hit = %v, want slow, lucky", in.Rules)
	}
	if want := (fault.Action{Delay: time.Second, Status: 504, ThrottleKbps: 100}); in.Action != want {
		t.Errorf("action = %+v, want %+v", in.Action, want)
	}
	if met.n[[3]string{"demo", "slow", "delayMs"}] != 1 || met.n[[3]string{"demo", "lucky", "throttleKbps"}] != 1 || len(met.n) != 4 {
		t.Errorf("metrics = %v", met.n)
	}
	if svc.Inject(t.Context(), nil, fault.Target{Kind: fault.KindSegment}).Hit() {
		t.Error("no rules hit")
	}
}

// Sticky selection depends on the request only: two services, as on two
// replicas, hit the same segments.
func TestService_Inject_StickyAcrossInstances(t *testing.T) {
	a, _, _, _ := newService(noDraw)
	b, _, _, _ := newService(noDraw)
	rules, err := fault.Normalized([]fault.Rule{segmentRule("five", 5)})
	if err != nil {
		t.Fatal(err)
	}
	hits := 0
	for n := range int64(2000) {
		tg := fault.Target{StreamID: "demo", Epoch: 3, Kind: fault.KindSegment, Rendition: "720p", Segment: n}
		ha, hb := a.Inject(t.Context(), rules, tg).Hit(), b.Inject(t.Context(), rules, tg).Hit()
		if ha != hb {
			t.Fatalf("segment %d: instance a hit %v, b hit %v", n, ha, hb)
		}
		if ha {
			hits++
		}
	}
	if hits == 0 || hits > 200 {
		t.Errorf("5%% rule hit %d of 2000", hits)
	}
}

func TestService_Flush(t *testing.T) {
	svc, rec, _, clk := newService(noDraw)
	rules := []fault.Rule{{ID: "a", Match: fault.Match{Kind: fault.KindSegment}, Percent: 100, Mode: fault.ModeSticky, Action: fault.Action{Status: 503}}}
	tg := fault.Target{StreamID: "demo", Epoch: 2, Kind: fault.KindSegment}
	for range 3 {
		svc.Inject(t.Context(), rules, tg)
	}
	svc.Flush(context.Background())
	if len(rec.Events()) != 0 {
		t.Fatal("Flush recorded a minute that is not over")
	}
	clk.add(time.Minute)
	svc.Inject(t.Context(), rules, tg)
	svc.Flush(context.Background())
	ev := rec.Events()
	if len(ev) != 1 {
		t.Fatalf("events = %+v, want one fault.injected", ev)
	}
	e := ev[0]
	if e.Type != event.FaultInjected || e.Epoch != 2 || e.Actor != event.System || e.Data["count"] != 3 || e.Data["rule"] != "a" || !e.Data["minute"].(time.Time).Equal(t0) {
		t.Errorf("event = %+v", e)
	}
}
