package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/stream"
)

// writeError is the one place that maps domain errors to status codes.
func (s *server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var fields []FieldError
	if verr, ok := errors.AsType[*stream.ValidationError](err); ok {
		for _, f := range verr.Fields {
			fields = append(fields, FieldError{Field: f.Field, Message: f.Message})
		}
	}
	if verr, ok := errors.AsType[*fault.ValidationError](err); ok {
		for _, f := range verr.Fields {
			fields = append(fields, FieldError{Field: f.Field, Message: f.Message})
		}
	}
	if fields != nil {
		writeJSON(w, http.StatusUnprocessableEntity, Error{Error: err.Error(), Fields: fields})
		return
	}
	status := statusOf(err)
	if status == http.StatusInternalServerError {
		s.log.ErrorContext(r.Context(), "api request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeJSON(w, status, Error{Error: "internal error"})
		return
	}
	writeJSON(w, status, Error{Error: err.Error()})
}

func statusOf(err error) int {
	switch {
	case errors.Is(err, stream.ErrNotFound), errors.Is(err, adbreak.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, stream.ErrExists), errors.Is(err, stream.ErrConflict),
		errors.Is(err, adbreak.ErrNotRunning), errors.Is(err, adbreak.ErrConflict): // ErrConflict covers *OverlapError
		return http.StatusConflict
	case errors.Is(err, stream.ErrInvalid), errors.Is(err, adbreak.ErrInvalid), errors.Is(err, fault.ErrInvalid):
		return http.StatusUnprocessableEntity
	case errors.Is(err, stream.ErrVersionMismatch):
		return http.StatusPreconditionFailed
	case errors.Is(err, stream.ErrPreconditionRequired):
		return http.StatusPreconditionRequired
	}
	return http.StatusInternalServerError
}

// badRequest answers a request whose parameters or body cannot be decoded.
func badRequest(w http.ResponseWriter, _ *http.Request, err error) {
	writeJSON(w, http.StatusBadRequest, Error{Error: err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the client is gone if this fails
}

// etag is the ETag of a stream version: "<version>".
func etag(version int) string {
	return `"` + strconv.Itoa(version) + `"`
}

// parseIfMatch returns the version in an If-Match header: 0 when absent, -1
// when it can match no version.
func parseIfMatch(h string) int {
	if h == "" {
		return 0
	}
	v, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(h, "W/"), `"`))
	if err != nil || v <= 0 {
		return -1
	}
	return v
}
