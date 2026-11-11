package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"ottstreamer/internal/accesslog"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/stream"
)

// FaultService keeps the active fault rules of each stream.
type FaultService interface {
	Rules(ctx context.Context, streamID string) ([]fault.Rule, error)
	Replace(ctx context.Context, streamID string, rules []fault.Rule) ([]fault.Rule, error)
	Clear(ctx context.Context, streamID string) error
}

// FaultInjector decides what fault rules do to a request.
type FaultInjector interface {
	Inject(ctx context.Context, rules []fault.Rule, t fault.Target) fault.Injection
}

func (s *server) GetFaults(ctx context.Context, req GetFaultsRequestObject) (GetFaultsResponseObject, error) {
	if _, err := s.streams.Get(ctx, req.Id); err != nil {
		return nil, err
	}
	rules, err := s.faults.Rules(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return GetFaults200JSONResponse(fromRules(rules)), nil
}

func (s *server) ReplaceFaults(ctx context.Context, req ReplaceFaultsRequestObject) (ReplaceFaultsResponseObject, error) {
	if _, err := s.streams.Get(ctx, req.Id); err != nil {
		return nil, err
	}
	rules, err := s.faults.Replace(ctx, req.Id, toRules(*req.Body))
	if err != nil {
		return nil, err
	}
	return ReplaceFaults200JSONResponse(fromRules(rules)), nil
}

func (s *server) ClearFaults(ctx context.Context, req ClearFaultsRequestObject) (ClearFaultsResponseObject, error) {
	if _, err := s.streams.Get(ctx, req.Id); err != nil {
		return nil, err
	}
	if err := s.faults.Clear(ctx, req.Id); err != nil {
		return nil, err
	}
	return ClearFaults204Response{}, nil
}

func (s *server) GetTime(ctx context.Context, req GetTimeRequestObject) (GetTimeResponseObject, error) {
	now := s.now()
	if id := req.Params.Stream; id != "" {
		skew, err := s.clockSkew(ctx, id)
		if err != nil {
			return nil, err
		}
		now = now.Add(skew)
	}
	return GetTime200TextResponse(now.UTC().Format("2006-01-02T15:04:05.000Z")), nil
}

// clockSkew is what the time rules of a stream add to /api/time; 0 for an
// unknown stream.
func (s *server) clockSkew(ctx context.Context, id string) (time.Duration, error) {
	st, err := s.streams.Get(ctx, id)
	if errors.Is(err, stream.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rules, err := s.faults.Rules(ctx, id)
	if err != nil {
		return 0, err
	}
	t := fault.Target{StreamID: id, Kind: fault.KindTime}
	if e := st.Status.Epoch; e != nil {
		t.Epoch = e.Number
	}
	in := s.injector.Inject(ctx, rules, t)
	if in.Hit() {
		accesslog.Add(ctx, slog.Group("fault", slog.Any("rules", in.Rules), slog.Any("actions", in.Action.Names())))
	}
	return in.Action.ClockSkew, nil
}

// toRules maps rules from the API, keeping none as nil.
func toRules(in []FaultRule) []fault.Rule {
	if len(in) == 0 {
		return nil
	}
	out := make([]fault.Rule, len(in))
	for i, r := range in {
		a := r.Action
		out[i] = fault.Rule{
			ID:      r.Id,
			Match:   fault.Match{Kind: fault.Kind(r.Match.Kind), Renditions: r.Match.Renditions},
			Percent: r.Percent,
			Mode:    fault.Mode(r.Mode),
			Action: fault.Action{
				Status:          int(a.Status),
				Delay:           time.Duration(a.DelayMs) * time.Millisecond,
				ThrottleKbps:    a.ThrottleKbps,
				TruncatePercent: a.TruncatePercent,
				Reset:           a.Reset,
				Stale:           duration(a.StaleSeconds),
				Gap:             a.Gap,
				ClockSkew:       duration(a.ClockSkewSeconds),
			},
			TTL: duration(r.TtlSeconds),
		}
		if g := r.Match.Segments; g != nil {
			out[i].Match.Segments = &fault.Range{From: g.From, To: g.To}
		}
	}
	return out
}

func fromRules(rules []fault.Rule) []FaultRule {
	out := make([]FaultRule, len(rules))
	for i, r := range rules {
		a := r.Action
		out[i] = FaultRule{
			Id:      r.ID,
			Match:   FaultMatch{Kind: FaultMatchKind(r.Match.Kind), Renditions: r.Match.Renditions},
			Percent: r.Percent,
			Mode:    FaultRuleMode(r.Mode),
			Action: FaultAction{
				Status:           FaultActionStatus(a.Status),
				DelayMs:          int(a.Delay.Milliseconds()),
				ThrottleKbps:     a.ThrottleKbps,
				TruncatePercent:  a.TruncatePercent,
				Reset:            a.Reset,
				StaleSeconds:     seconds(a.Stale),
				Gap:              a.Gap,
				ClockSkewSeconds: seconds(a.ClockSkew),
			},
			TtlSeconds: seconds(r.TTL),
		}
		if g := r.Match.Segments; g != nil {
			out[i].Match.Segments = &FaultRange{From: g.From, To: g.To}
		}
		if !r.ExpiresAt.IsZero() {
			at := r.ExpiresAt.UTC()
			out[i].ExpiresAt = &at
		}
	}
	return out
}
