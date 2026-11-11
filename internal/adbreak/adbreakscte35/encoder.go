// Package adbreakscte35 encodes ad break cues as SCTE-35 splice_info_sections
// with github.com/Comcast/scte35-go.
package adbreakscte35

import (
	"errors"
	"fmt"

	"github.com/Comcast/scte35-go/pkg/scte35"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/stream"
)

var _ adbreak.CueEncoder = Encoder{}

// ptsMask wraps 90 kHz media time into the 33-bit pts_time.
const ptsMask = 1<<33 - 1

// segmentationTypes maps stream.Ads.SegmentationType to its start and end
// segmentation_type_id.
var segmentationTypes = map[string][2]uint32{
	"provider_placement_opportunity":    {scte35.SegmentationTypeProviderPOStart, scte35.SegmentationTypeProviderPOEnd},
	"provider_ad":                       {scte35.SegmentationTypeProviderAdStart, scte35.SegmentationTypeProviderAdEnd},
	"break":                             {scte35.SegmentationTypeBreakStart, scte35.SegmentationTypeBreakEnd},
	"distributor_placement_opportunity": {scte35.SegmentationTypeDistributorPOStart, scte35.SegmentationTypeDistributorPOEnd},
}

// Encoder is an adbreak.CueEncoder. pts_adjustment is 0, since media time
// already is the 90 kHz PTS.
type Encoder struct{}

// NewEncoder returns an Encoder.
func NewEncoder() Encoder { return Encoder{} }

// Encode builds the splice_info_section of c: a splice_insert (program
// splice, unique_program_id 1, break_duration with auto_return on a
// cue-out) or a time_signal with one segmentation descriptor (segment 1 of
// 1, segmentation_duration on a cue-out).
func (Encoder) Encode(c adbreak.Cue) ([]byte, error) {
	if c.PTS < 0 || c.DurationTicks < 0 || c.DurationTicks > ptsMask {
		return nil, errors.New("adbreakscte35: pts or duration out of range")
	}
	pts := uint64(c.PTS) & ptsMask
	dur := uint64(c.DurationTicks)
	sis := scte35.SpliceInfoSection{SAPType: scte35.SAPTypeNotSpecified, Tier: 0xFFF}
	switch c.Command {
	case stream.CommandSpliceInsert:
		ins := &scte35.SpliceInsert{SpliceEventID: c.EventID, SpliceEventCancelIndicator: c.Cancel}
		if !c.Cancel {
			ins.Program = scte35.NewSpliceInsertProgram(pts)
			ins.OutOfNetworkIndicator = c.Out
			ins.UniqueProgramID = 1
			if c.Out {
				ins.BreakDuration = &scte35.BreakDuration{AutoReturn: true, Duration: dur}
			}
		}
		sis.SpliceCommand = ins
	case stream.CommandTimeSignal:
		codes, ok := segmentationTypes[c.SegmentationType]
		if !ok {
			return nil, fmt.Errorf("adbreakscte35: unknown segmentation type %q", c.SegmentationType)
		}
		sd := &scte35.SegmentationDescriptor{SegmentationEventID: c.EventID, SegmentationEventCancelIndicator: c.Cancel}
		if !c.Cancel {
			sd.SegmentationTypeID, sd.SegmentNum, sd.SegmentsExpected = codes[1], 1, 1
			if c.Out {
				sd.SegmentationTypeID, sd.SegmentationDuration = codes[0], &dur
			}
		}
		sis.SpliceCommand = scte35.NewTimeSignal(pts)
		sis.SpliceDescriptors = scte35.SpliceDescriptors{sd}
	default:
		return nil, fmt.Errorf("adbreakscte35: unknown command %q", c.Command)
	}
	b, err := sis.Encode()
	if err != nil {
		return nil, fmt.Errorf("adbreakscte35: encode: %w", err)
	}
	return b, nil
}
