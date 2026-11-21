package adbreakscte35_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Comcast/scte35-go/pkg/scte35"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/stream"
)

// encoded lists every kind of cue the Encoder emits, by name.
func encoded(t *testing.T) map[string][]byte {
	t.Helper()
	si, ts := stream.CommandSpliceInsert, stream.CommandTimeSignal
	cues := map[string]adbreak.Cue{
		"splice_insert out":    {Command: si, EventID: 7, Out: true, PTS: start, DurationTicks: dur},
		"splice_insert in":     {Command: si, EventID: 7, PTS: start + dur},
		"splice_insert cancel": {Command: si, EventID: 7, Out: true, Cancel: true, PTS: start, DurationTicks: dur},
		"time_signal cancel":   {Command: ts, SegmentationType: "break", EventID: 3, Out: true, Cancel: true, PTS: start},
	}
	for _, st := range []string{"provider_placement_opportunity", "provider_ad", "break", "distributor_placement_opportunity"} {
		cues["time_signal out "+st] = adbreak.Cue{Command: ts, SegmentationType: st, EventID: 3, Out: true, PTS: start, DurationTicks: dur}
		cues["time_signal in "+st] = adbreak.Cue{Command: ts, SegmentationType: st, EventID: 3, PTS: start + dur}
	}
	out := make(map[string][]byte, len(cues))
	for name, c := range cues {
		b, err := adbreakscte35.NewEncoder().Encode(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = b
	}
	return out
}

// The JSON holds the whole section: scte35-go reads it back and encodes the
// same bytes.
func TestDecode(t *testing.T) {
	for name, b := range encoded(t) {
		t.Run(name, func(t *testing.T) {
			cmd, j, err := adbreakscte35.NewDecoder().Decode(b)
			if err != nil {
				t.Fatalf("decode %x: %v", b, err)
			}
			if want, _, _ := strings.Cut(name, " "); cmd != want {
				t.Errorf("command = %q, want %q", cmd, want)
			}
			var sis scte35.SpliceInfoSection
			if err := json.Unmarshal(j, &sis); err != nil {
				t.Fatalf("unmarshal %s: %v", j, err)
			}
			if got, err := sis.Encode(); err != nil || !bytes.Equal(got, b) {
				t.Errorf("JSON %s encodes to %x, %v; want %x", j, got, err, b)
			}
		})
	}
}

func TestDecode_Invalid(t *testing.T) {
	cues := encoded(t)
	flip := func(b []byte, i int) []byte {
		b = bytes.Clone(b)
		b[(i+len(b))%len(b)] ^= 0x01
		return b
	}
	out, cancel := cues["splice_insert out"], cues["splice_insert cancel"]
	for name, b := range map[string][]byte{
		"empty":               nil,
		"garbage":             []byte("not a splice_info_section"),
		"bad CRC":             flip(out, -1),
		"cancel bad CRC":      flip(cancel, -1),
		"cancel bad event id": flip(cancel, 17),
		"cancel truncated":    cancel[:len(cancel)-1],
	} {
		if _, _, err := adbreakscte35.NewDecoder().Decode(b); !errors.Is(err, adbreak.ErrInvalid) {
			t.Errorf("%s: error = %v, want adbreak.ErrInvalid", name, err)
		}
	}
}
