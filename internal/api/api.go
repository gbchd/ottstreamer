package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"ottstreamer/internal/stream"
)

// StreamReader reads streams.
type StreamReader interface {
	Get(ctx context.Context, id string) (stream.Stream, error)
	List(ctx context.Context) ([]stream.Stream, error)
}

// StreamWriter changes stream config.
type StreamWriter interface {
	Create(ctx context.Context, s stream.Stream) (stream.Stream, error)
	Replace(ctx context.Context, id, name string, spec stream.Spec, ifMatch int) (stream.Stream, error)
	Delete(ctx context.Context, id string, ifMatch int) error
}

// StreamLifecycle starts and stops streams.
type StreamLifecycle interface {
	Start(ctx context.Context, id string, ast *time.Time) (stream.Stream, error)
	Stop(ctx context.Context, id string) (stream.Stream, error)
}

// StreamService is the part of *stream.Service the handlers call.
type StreamService interface {
	StreamReader
	StreamWriter
	StreamLifecycle
}

// CueDecoder decodes SCTE-35 cues.
type CueDecoder interface {
	// Decode returns the splice_command name of the splice_info_section in
	// cue and the section as JSON.
	Decode(cue []byte) (command string, section json.RawMessage, err error)
}

var _ StrictServerInterface = (*server)(nil)

type server struct {
	streams  StreamService
	breaks   BreakService
	events   EventService
	faults   FaultService
	injector FaultInjector
	cues     CueDecoder
	now      func() time.Time
	log      *slog.Logger
}

// Register adds every /api route of api/openapi.yaml to mux. Authentication
// and CORS are the caller's middleware; the API key a request carries is the
// actor of the events it causes.
func Register(mux *http.ServeMux, streams StreamService, breaks BreakService, events EventService, faults FaultService, injector FaultInjector, cues CueDecoder, now func() time.Time, logger *slog.Logger) {
	srv := &server{streams: streams, breaks: breaks, events: events, faults: faults, injector: injector, cues: cues, now: now, log: logger}
	strict := NewStrictHandlerWithOptions(srv, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  badRequest,
		ResponseErrorHandlerFunc: srv.writeError,
	})
	HandlerWithOptions(strict, StdHTTPServerOptions{BaseRouter: mux, ErrorHandlerFunc: badRequest, Middlewares: []MiddlewareFunc{withActor}})
}
