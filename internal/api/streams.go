package api

import (
	"context"
	"errors"
	"slices"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/event"
	"ottstreamer/internal/stream"
)

func (s *server) ListStreams(ctx context.Context, _ ListStreamsRequestObject) (ListStreamsResponseObject, error) {
	list, err := s.streams.List(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make(ListStreams200JSONResponse, 0, len(list))
	for _, st := range list {
		sum, err := s.summary(ctx, st, now)
		if errors.Is(err, stream.ErrNotFound) {
			continue // deleted since the list was read
		}
		if err != nil {
			return nil, err
		}
		v := fromStream(st)
		out = append(out, StreamListItem{
			Id: v.Id, Name: v.Name, Spec: v.Spec, Status: v.Status, Version: v.Version,
			CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, Summary: sum,
		})
	}
	return out, nil
}

// summary is what the streams page shows of a stream besides the stream:
// nil for a stopped one.
func (s *server) summary(ctx context.Context, st stream.Stream, now time.Time) (*StreamSummary, error) {
	e := liveEpoch(st)
	if e == nil {
		return nil, nil
	}
	edge := e.Timeline().LiveEdge(now)
	breaks, err := s.breaks.List(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	rules, err := s.faults.Rules(ctx, st.ID)
	if err != nil {
		return nil, err
	}
	late, _, err := s.events.List(ctx, st.ID, event.Query{
		Types: []event.Type{event.SegmentLate}, From: now.Add(-time.Minute), To: now, Limit: event.MaxLimit,
	})
	if err != nil {
		return nil, err
	}
	out := &StreamSummary{LiveEdge: edge, ActiveBreak: activeBreak(breaks, edge), ActiveFaultRules: len(rules), LateSegmentsLastMinute: len(late)}
	if i := slices.IndexFunc(breaks, func(b adbreak.Break) bool { return !b.Cancelled && b.StartSeg > edge }); i >= 0 {
		next := fromBreak(breaks[i])
		out.NextBreak = &next
	}
	return out, nil
}

// liveEpoch is the epoch a stream has a live position in: nil when stopped.
func liveEpoch(st stream.Stream) *stream.Epoch {
	if st.Status.State == stream.StateStopped {
		return nil
	}
	return st.Status.Epoch
}

// activeBreak is the break covering segment edge, nil when none does.
func activeBreak(breaks []adbreak.Break, edge int64) *Break {
	var active *Break
	for _, b := range breaks {
		if b.Covers(edge) {
			v := fromBreak(b)
			active = &v
		}
	}
	return active
}

func (s *server) CreateStream(ctx context.Context, req CreateStreamRequestObject) (CreateStreamResponseObject, error) {
	st, err := s.streams.Create(ctx, stream.Stream{ID: req.Body.Id, Name: req.Body.Name, Spec: toSpec(req.Body.Spec)})
	if err != nil {
		return nil, err
	}
	return CreateStream201JSONResponse{
		Body:    fromStream(st),
		Headers: CreateStream201ResponseHeaders{ETag: etag(st.Version), Location: "/api/streams/" + st.ID},
	}, nil
}

func (s *server) GetStream(ctx context.Context, req GetStreamRequestObject) (GetStreamResponseObject, error) {
	st, err := s.streams.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return GetStream200JSONResponse{Body: fromStream(st), Headers: GetStream200ResponseHeaders{ETag: etag(st.Version)}}, nil
}

func (s *server) ReplaceStream(ctx context.Context, req ReplaceStreamRequestObject) (ReplaceStreamResponseObject, error) {
	st, err := s.streams.Replace(ctx, req.Id, req.Body.Name, toSpec(req.Body.Spec), parseIfMatch(req.Params.IfMatch))
	if err != nil {
		return nil, err
	}
	return ReplaceStream200JSONResponse{Body: fromStream(st), Headers: ReplaceStream200ResponseHeaders{ETag: etag(st.Version)}}, nil
}

func (s *server) DeleteStream(ctx context.Context, req DeleteStreamRequestObject) (DeleteStreamResponseObject, error) {
	if err := s.streams.Delete(ctx, req.Id, parseIfMatch(req.Params.IfMatch)); err != nil {
		return nil, err
	}
	return DeleteStream204Response{}, nil
}

func (s *server) StartStream(ctx context.Context, req StartStreamRequestObject) (StartStreamResponseObject, error) {
	var body StartRequest
	if req.Body != nil {
		body = *req.Body
	}
	st, err := s.streams.Start(ctx, req.Id, body.Ast)
	if err != nil {
		return nil, err
	}
	return StartStream200JSONResponse(fromStream(st)), nil
}

func (s *server) StopStream(ctx context.Context, req StopStreamRequestObject) (StopStreamResponseObject, error) {
	st, err := s.streams.Stop(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	return StopStream200JSONResponse(fromStream(st)), nil
}

func (s *server) GetStreamStatus(ctx context.Context, req GetStreamStatusRequestObject) (GetStreamStatusResponseObject, error) {
	st, err := s.streams.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	pos := GetStreamStatus200JSONResponse{State: State(st.Status.State), Now: now.UTC()}
	if e := liveEpoch(st); e != nil {
		tl := e.Timeline()
		edge, horizon := tl.LiveEdge(now), tl.FrozenHorizon(now)
		pos.Epoch, pos.LiveEdge, pos.FrozenHorizon, pos.EndSegment = &e.Number, &edge, &horizon, e.EndSegment
		breaks, err := s.breaks.List(ctx, req.Id)
		if err != nil {
			return nil, err
		}
		pos.ActiveBreak = activeBreak(breaks, edge)
	}
	return pos, nil
}
