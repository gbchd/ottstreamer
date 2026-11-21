package api

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"ottstreamer/internal/adbreak"
)

// BreakReader reads the breaks of a stream's current epoch.
type BreakReader interface {
	Get(ctx context.Context, streamID, breakID string) (adbreak.Break, error)
	List(ctx context.Context, streamID string) ([]adbreak.Break, error)
	// InRange returns the breaks overlapping segments [from, to), recurring
	// and cancelled ones included.
	InRange(ctx context.Context, streamID string, epoch int, from, to int64) ([]adbreak.Break, error)
}

// BreakWriter schedules, cancels and ends breaks.
type BreakWriter interface {
	Add(ctx context.Context, streamID string, req adbreak.AddRequest) (adbreak.Break, error)
	Delete(ctx context.Context, streamID, breakID string) (adbreak.Break, error)
}

// BreakService is the part of *adbreak.Service the handlers call.
type BreakService interface {
	BreakReader
	BreakWriter
}

func (s *server) ListBreaks(ctx context.Context, req ListBreaksRequestObject) (ListBreaksResponseObject, error) {
	list, err := s.breaks.List(ctx, req.Id)
	if err != nil {
		return nil, err
	}
	out := make(ListBreaks200JSONResponse, 0, len(list))
	for _, b := range list {
		out = append(out, fromBreak(b))
	}
	return out, nil
}

func (s *server) CreateBreak(ctx context.Context, req CreateBreakRequestObject) (CreateBreakResponseObject, error) {
	in := adbreak.AddRequest{
		Duration:         duration(req.Body.Duration),
		At:               req.Body.At,
		ReplaceRecurring: req.Body.ReplaceRecurring,
	}
	if req.Body.In != "" {
		d, err := time.ParseDuration(req.Body.In)
		if err != nil {
			return nil, fmt.Errorf("%w: in: %w", adbreak.ErrInvalid, err)
		}
		in.In = &d
	}
	b, err := s.breaks.Add(ctx, req.Id, in)
	if err != nil {
		return nil, err
	}
	return CreateBreak201JSONResponse(fromBreak(b)), nil
}

func (s *server) GetBreak(ctx context.Context, req GetBreakRequestObject) (GetBreakResponseObject, error) {
	b, err := s.breaks.Get(ctx, req.Id, req.BreakId)
	if err != nil {
		return nil, err
	}
	return GetBreak200JSONResponse(fromBreak(b)), nil
}

func (s *server) DeleteBreak(ctx context.Context, req DeleteBreakRequestObject) (DeleteBreakResponseObject, error) {
	b, err := s.breaks.Delete(ctx, req.Id, req.BreakId)
	if err != nil {
		return nil, err
	}
	return DeleteBreak200JSONResponse(fromBreak(b)), nil
}

func fromBreak(b adbreak.Break) Break {
	return Break{
		Id:               b.ID,
		EventId:          int64(b.EventID),
		StartSegment:     b.StartSeg,
		EndSegment:       b.EndSeg,
		Start:            b.Start.UTC(),
		End:              b.End.UTC(),
		RequestedSeconds: seconds(b.Requested),
		DurationSeconds:  seconds(b.End.Sub(b.Start)),
		Recurring:        b.Recurring,
		Cancelled:        b.Cancelled,
		CueOut:           cue(b.CueOut),
		CueIn:            cue(b.CueIn),
	}
}

func (s *server) DecodeCue(_ context.Context, req DecodeCueRequestObject) (DecodeCueResponseObject, error) {
	b, err := cueBytes(req.Params.Cue)
	if err != nil {
		return nil, fmt.Errorf("%w: cue: %w", adbreak.ErrInvalid, err)
	}
	cmd, section, err := s.cues.Decode(b)
	if err != nil {
		return nil, err
	}
	return DecodeCue200JSONResponse{Command: cmd, Section: section}, nil
}

// cueBytes reads a cue as Cue.Hex or Cue.Base64 writes it.
func cueBytes(s string) ([]byte, error) {
	if h, ok := strings.CutPrefix(s, "0x"); ok {
		return hex.DecodeString(h)
	}
	if h, ok := strings.CutPrefix(s, "0X"); ok {
		return hex.DecodeString(h)
	}
	return base64.StdEncoding.DecodeString(s)
}

// cue writes hex the way HLS SCTE35-OUT/IN attributes do: 0x, upper case.
func cue(b []byte) Cue {
	return Cue{Base64: base64.StdEncoding.EncodeToString(b), Hex: "0x" + strings.ToUpper(hex.EncodeToString(b))}
}
