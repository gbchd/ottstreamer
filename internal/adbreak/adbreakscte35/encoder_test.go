package adbreakscte35_test

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/Comcast/scte35-go/pkg/scte35"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/stream"
)

const (
	start = int64(146221200000) // above 2^33, so pts_time wraps
	dur   = int64(2700000)
)

func TestEncodeSpliceInsert(t *testing.T) {
	tests := []struct {
		name string
		cue  adbreak.Cue
	}{
		{"cue-out", adbreak.Cue{EventID: 7, Out: true, PTS: start, DurationTicks: dur}},
		{"cue-in", adbreak.Cue{EventID: 7, PTS: start + dur}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cue.Command = stream.CommandSpliceInsert
			sis := decode(t, tt.cue)
			ins, ok := sis.SpliceCommand.(*scte35.SpliceInsert)
			if !ok {
				t.Fatalf("command = %T, want splice_insert", sis.SpliceCommand)
			}
			if ins.SpliceEventID != 7 || ins.SpliceEventCancelIndicator {
				t.Errorf("event %d cancel %v, want 7 false", ins.SpliceEventID, ins.SpliceEventCancelIndicator)
			}
			if ins.OutOfNetworkIndicator != tt.cue.Out || ins.UniqueProgramID != 1 || ins.Program == nil {
				t.Errorf("out %v program %+v unique id %d", ins.OutOfNetworkIndicator, ins.Program, ins.UniqueProgramID)
			}
			if got := sis.SpliceTimePTS(); got != wrapped(tt.cue.PTS) {
				t.Errorf("pts = %d, want %d", got, wrapped(tt.cue.PTS))
			}
			switch {
			case tt.cue.Out && (ins.BreakDuration == nil || !ins.BreakDuration.AutoReturn || ins.BreakDuration.Duration != uint64(dur)):
				t.Errorf("break_duration = %+v, want auto return %d", ins.BreakDuration, dur)
			case !tt.cue.Out && ins.BreakDuration != nil:
				t.Errorf("cue-in has break_duration %+v", ins.BreakDuration)
			}
		})
	}
}

// scte35-go 1.8.0 cannot decode a splice_insert cancel: it reads
// unique_program_id and avail_* outside the cancel == 0 branch. So the cancel
// is checked byte for byte, with an independent CRC_32.
func TestEncodeSpliceInsertCancel(t *testing.T) {
	b, err := adbreakscte35.NewEncoder().Encode(adbreak.Cue{
		Command: stream.CommandSpliceInsert, EventID: 7, Out: true, Cancel: true, PTS: start, DurationTicks: dur,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = "fc3016000000000000" + "00fff0" + "05" + "05" + "00000007" + "ff" + "0000" // ... + CRC_32
	if got := hex.EncodeToString(b); len(b) != 25 || got[:len(want)] != want {
		t.Fatalf("cancel = %s, want %s + crc", got, want)
	}
	if crc := binary.BigEndian.Uint32(b[len(b)-4:]); crc != crc32MPEG2(b[:len(b)-4]) {
		t.Errorf("CRC_32 = %08x, want %08x", crc, crc32MPEG2(b[:len(b)-4]))
	}
}

func crc32MPEG2(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, c := range b {
		crc ^= uint32(c) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func TestEncodeTimeSignal(t *testing.T) {
	tests := []struct {
		name    string
		segType string
		cue     adbreak.Cue
		typeID  uint32
	}{
		{"cue-out", "provider_placement_opportunity", adbreak.Cue{EventID: 3, Out: true, PTS: start, DurationTicks: dur}, 0x34},
		{"cue-in", "provider_placement_opportunity", adbreak.Cue{EventID: 3, PTS: start + dur}, 0x35},
		{"provider ad out", "provider_ad", adbreak.Cue{EventID: 3, Out: true, PTS: start, DurationTicks: dur}, 0x30},
		{"provider ad in", "provider_ad", adbreak.Cue{EventID: 3, PTS: start}, 0x31},
		{"break out", "break", adbreak.Cue{EventID: 3, Out: true, PTS: start, DurationTicks: dur}, 0x22},
		{"break in", "break", adbreak.Cue{EventID: 3, PTS: start}, 0x23},
		{"distributor out", "distributor_placement_opportunity", adbreak.Cue{EventID: 3, Out: true, PTS: start, DurationTicks: dur}, 0x36},
		{"distributor in", "distributor_placement_opportunity", adbreak.Cue{EventID: 3, PTS: start}, 0x37},
		{"cancel", "break", adbreak.Cue{EventID: 3, Out: true, Cancel: true, PTS: start}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.cue.Command, tt.cue.SegmentationType = stream.CommandTimeSignal, tt.segType
			sis := decode(t, tt.cue)
			if _, ok := sis.SpliceCommand.(*scte35.TimeSignal); !ok {
				t.Fatalf("command = %T, want time_signal", sis.SpliceCommand)
			}
			if got := sis.SpliceTimePTS(); got != wrapped(tt.cue.PTS) {
				t.Errorf("pts = %d, want %d", got, wrapped(tt.cue.PTS))
			}
			if len(sis.SpliceDescriptors) != 1 {
				t.Fatalf("descriptors = %d, want 1", len(sis.SpliceDescriptors))
			}
			sd, ok := sis.SpliceDescriptors[0].(*scte35.SegmentationDescriptor)
			if !ok {
				t.Fatalf("descriptor = %T", sis.SpliceDescriptors[0])
			}
			if sd.SegmentationEventID != 3 || sd.SegmentationEventCancelIndicator != tt.cue.Cancel {
				t.Errorf("event %d cancel %v", sd.SegmentationEventID, sd.SegmentationEventCancelIndicator)
			}
			if tt.cue.Cancel {
				return
			}
			if sd.SegmentationTypeID != tt.typeID || sd.SegmentNum != 1 || sd.SegmentsExpected != 1 {
				t.Errorf("type %#x segment %d of %d, want %#x 1 of 1", sd.SegmentationTypeID, sd.SegmentNum, sd.SegmentsExpected, tt.typeID)
			}
			switch {
			case tt.cue.Out && (sd.SegmentationDuration == nil || *sd.SegmentationDuration != uint64(dur)):
				t.Errorf("segmentation_duration = %v, want %d", sd.SegmentationDuration, dur)
			case !tt.cue.Out && sd.SegmentationDuration != nil:
				t.Errorf("cue-in has segmentation_duration %d", *sd.SegmentationDuration)
			}
		})
	}
}

func TestEncodeRejects(t *testing.T) {
	enc := adbreakscte35.NewEncoder()
	for name, c := range map[string]adbreak.Cue{
		"negative pts":     {Command: stream.CommandSpliceInsert, PTS: -1},
		"unknown command":  {Command: "splice_null"},
		"unknown seg type": {Command: stream.CommandTimeSignal, SegmentationType: "chapter"},
	} {
		if _, err := enc.Encode(c); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// decode encodes c and decodes it back, which checks the CRC_32.
func decode(t *testing.T, c adbreak.Cue) *scte35.SpliceInfoSection {
	t.Helper()
	b, err := adbreakscte35.NewEncoder().Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	sis, err := scte35.DecodeBytes(b)
	if err != nil {
		t.Fatalf("decode %x: %v", b, err)
	}
	if sis.PTSAdjustment != 0 || sis.EncryptedPacketFlag() {
		t.Errorf("pts_adjustment %d, encrypted %v", sis.PTSAdjustment, sis.EncryptedPacketFlag())
	}
	return sis
}

func wrapped(pts int64) uint64 { return uint64(pts) % (1 << 33) }
