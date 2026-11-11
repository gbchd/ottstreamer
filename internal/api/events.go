package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"ottstreamer/internal/apikey"
	"ottstreamer/internal/event"
	"ottstreamer/internal/stream"
)

// EventService reads a stream's event log.
type EventService interface {
	List(ctx context.Context, streamID string, q event.Query) ([]event.Event, string, error)
	Subscribe(ctx context.Context, streamID string) <-chan event.Event
}

// withActor makes the request's API key the actor of the events it causes.
func withActor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name := apikey.Name(r.Context()); name != "" {
			r = r.WithContext(event.WithActor(r.Context(), "api:"+name))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) ListEvents(ctx context.Context, req ListEventsRequestObject) (ListEventsResponseObject, error) {
	p := req.Params
	q := event.Query{After: p.Cursor, From: p.From, To: p.To, Limit: p.Limit}
	for _, t := range p.Types {
		q.Types = append(q.Types, event.Type(t))
	}
	events, next, err := s.events.List(ctx, req.Id, q)
	if err != nil {
		return nil, err
	}
	return ListEvents200JSONResponse{Events: fromEvents(events), Next: cursor(next)}, nil
}

func (s *server) StreamEvents(ctx context.Context, req StreamEventsRequestObject) (StreamEventsResponseObject, error) {
	return eventStream{ctx: ctx, events: s.events, log: s.log, streamID: req.Id, since: cmp.Or(req.Params.LastEventID, req.Params.Since)}, nil
}

// pingEvery keeps idle event streams open through proxies.
const pingEvery = 15 * time.Second

// eventStream writes the Server-Sent Events of one stream: a replay after
// since, then every new event, until the client goes or the service closes
// the subscription.
type eventStream struct {
	ctx      context.Context
	events   EventService
	log      *slog.Logger
	streamID string
	since    string
}

func (es eventStream) VisitStreamEventsResponse(w http.ResponseWriter) error {
	live := es.events.Subscribe(es.ctx, es.streamID) // before the replay, so nothing falls between
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	last := es.since
	send := func(e event.Event) error {
		b, err := json.Marshal(fromEvent(e))
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "id: %s\ndata: %s\n\n", e.ID, b); err != nil {
			return err
		}
		last = e.ID
		return nil
	}
	err := es.replay(send, last)
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for err == nil {
		if err = rc.Flush(); err != nil {
			break
		}
		select {
		case e, ok := <-live:
			if !ok {
				return nil // ctx done, Close, or too slow: the client resumes from last
			}
			if e.ID > last {
				err = send(e)
			}
		case <-ping.C:
			_, err = fmt.Fprint(w, ": ping\n\n")
		}
	}
	if es.ctx.Err() == nil {
		es.log.WarnContext(es.ctx, "event stream ended", "stream", es.streamID, "error", err)
	}
	return nil // the status line is out: the strict handler must not write an error body
}

// replay sends the stored events after since, when there is one.
func (es eventStream) replay(send func(event.Event) error, since string) error {
	for since != "" {
		events, next, err := es.events.List(es.ctx, es.streamID, event.Query{After: since, Limit: event.MaxLimit})
		if err != nil {
			return err
		}
		for _, e := range events {
			if err := send(e); err != nil {
				return err
			}
		}
		since = next
	}
	return nil
}

func (s *server) GetTimeline(ctx context.Context, req GetTimelineRequestObject) (GetTimelineResponseObject, error) {
	st, err := s.streams.Get(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	now := s.now()
	dvr := st.Spec.Window.DVR
	if e := st.Status.Epoch; e != nil {
		dvr = e.Spec.Window.DVR
	}
	from := cmp.Or(req.Params.From, now.Add(-dvr))
	to := cmp.Or(req.Params.To, now.Add(dvr))
	out := GetTimeline200JSONResponse{Now: now.UTC(), From: from.UTC(), To: to.UTC(), State: State(st.Status.State), Breaks: []Break{}}
	if e := st.Status.Epoch; e != nil && st.Status.State != stream.StateStopped {
		tl := e.Timeline()
		edge, horizon := tl.LiveEdge(now), tl.FrozenHorizon(now)
		dvrStart := max(0, edge-tl.SegsFor(dvr)+1)
		ast, seg := e.AST.UTC(), seconds(tl.SegDuration())
		out.Epoch, out.Ast, out.SegmentSeconds, out.EndSegment = &e.Number, &ast, &seg, e.EndSegment
		out.LiveEdge, out.FrozenHorizon, out.DvrStart = &edge, &horizon, &dvrStart
		// From the segment holding from to the one holding to.
		first, end := max(0, tl.SegAtOrAfter(from)-1), tl.SegAtOrAfter(to)+1
		breaks, err := s.breaks.InRange(ctx, req.Id, e.Number, first, end)
		if err != nil {
			return nil, err
		}
		for _, b := range breaks {
			out.Breaks = append(out.Breaks, fromBreak(b))
		}
	}
	events, next, err := s.events.List(ctx, req.Id, event.Query{From: from, To: to, Limit: event.MaxLimit})
	if err != nil {
		return nil, err
	}
	out.Events, out.EventsNext = fromEvents(events), cursor(next)
	return out, nil
}

func fromEvents(events []event.Event) []Event {
	out := make([]Event, len(events))
	for i, e := range events {
		out[i] = fromEvent(e)
	}
	return out
}

func fromEvent(e event.Event) Event {
	out := Event{Id: e.ID, StreamId: e.StreamID, At: e.At.UTC(), Segment: e.Segment, Type: string(e.Type), Actor: e.Actor, Data: e.Data}
	if e.Epoch != 0 {
		out.Epoch = &e.Epoch
	}
	if out.Data == nil {
		out.Data = map[string]any{}
	}
	return out
}

// cursor is next as a nullable JSON string.
func cursor(next string) *string {
	if next == "" {
		return nil
	}
	return &next
}
