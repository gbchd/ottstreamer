package api

import (
	"context"
	"net/http"
)

// BreakService is the part of *adbreak.Service the break handlers call.
// The break handlers are not implemented yet.
type BreakService interface{}

func (s *server) ListBreaks(context.Context, ListBreaksRequestObject) (ListBreaksResponseObject, error) {
	return notImplemented{}, nil
}

func (s *server) CreateBreak(context.Context, CreateBreakRequestObject) (CreateBreakResponseObject, error) {
	return notImplemented{}, nil
}

func (s *server) GetBreak(context.Context, GetBreakRequestObject) (GetBreakResponseObject, error) {
	return notImplemented{}, nil
}

func (s *server) DeleteBreak(context.Context, DeleteBreakRequestObject) (DeleteBreakResponseObject, error) {
	return notImplemented{}, nil
}

type notImplemented struct{}

func (notImplemented) write(w http.ResponseWriter) error {
	writeJSON(w, http.StatusNotImplemented, Error{Error: "ad breaks are not implemented yet"})
	return nil
}

func (n notImplemented) VisitListBreaksResponse(w http.ResponseWriter) error  { return n.write(w) }
func (n notImplemented) VisitCreateBreakResponse(w http.ResponseWriter) error { return n.write(w) }
func (n notImplemented) VisitGetBreakResponse(w http.ResponseWriter) error    { return n.write(w) }
func (n notImplemented) VisitDeleteBreakResponse(w http.ResponseWriter) error { return n.write(w) }
