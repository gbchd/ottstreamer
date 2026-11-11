package api

import (
	"context"

	"ottstreamer/internal/stream"
)

func (s *server) ListStreams(ctx context.Context, _ ListStreamsRequestObject) (ListStreamsResponseObject, error) {
	list, err := s.streams.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(ListStreams200JSONResponse, 0, len(list))
	for _, st := range list {
		out = append(out, fromStream(st))
	}
	return out, nil
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
	if e := st.Status.Epoch; e != nil && st.Status.State != stream.StateStopped {
		tl := e.Timeline()
		edge, horizon := tl.LiveEdge(now), tl.FrozenHorizon(now)
		pos.Epoch, pos.LiveEdge, pos.FrozenHorizon, pos.EndSegment = &e.Number, &edge, &horizon, e.EndSegment
	}
	return pos, nil
}
