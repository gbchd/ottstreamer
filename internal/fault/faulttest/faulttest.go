// Package faulttest holds a fake for the packages that build a
// fault.Service.
package faulttest

import (
	"context"

	"ottstreamer/internal/fault"
)

var _ fault.Streams = Streams{}

// Streams is a fault.Streams that answers with the same Stream for every
// stream ID.
type Streams fault.Stream

func (s Streams) Stream(context.Context, string) (fault.Stream, error) {
	return fault.Stream(s), nil
}
