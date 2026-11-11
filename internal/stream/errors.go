package stream

import (
	"errors"
	"strings"
)

var (
	ErrNotFound             = errors.New("stream: not found")
	ErrExists               = errors.New("stream: id taken")
	ErrVersionMismatch      = errors.New("stream: version mismatch")
	ErrInvalid              = errors.New("stream: invalid")
	ErrConflict             = errors.New("stream: wrong state")
	ErrPreconditionRequired = errors.New("stream: version required")
)

// FieldError is one invalid field, named by its path in the stream resource
// (for example "spec.video[1].bitrateKbps").
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
	return "stream: invalid: " + strings.Join(msgs, "; ")
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

func (e *ValidationError) add(field, msg string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: msg})
}

// err returns e as an error, or nil when no field is invalid.
func (e *ValidationError) err() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}
