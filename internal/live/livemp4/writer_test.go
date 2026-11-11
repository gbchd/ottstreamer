package livemp4

import (
	"bytes"
	"slices"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/live"
)

func TestWriter_WriteSegment(t *testing.T) {
	samples := []live.Sample{
		{Data: []byte{0, 0, 0, 3, 0x65, 1, 2}, Sync: true},
		{Data: []byte{0, 0, 0, 2, 0x41, 3}},
		{Data: []byte{0, 0, 0, 2, 0x41, 4}},
	}
	f := live.Fragment{
		Seq:       8,
		TrackID:   1,
		BaseTime:  7 * 90000,
		SampleDur: 3600,
		Samples:   samples,
		Emsgs: []adbreak.Signal{
			{EventID: 7, PresentationTicks: 1 << 34, DurationTicks: 30 * 90000, Message: []byte{0xfc, 1}},
			{EventID: 8, PresentationTicks: 900000, Message: []byte{0xfc, 2}},
		},
		PadTo: 1000,
	}
	var buf bytes.Buffer
	if err := NewWriter().WriteSegment(&buf, f); err != nil {
		t.Fatal(err)
	}
	file, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	types := make([]string, len(file.Children))
	for i, b := range file.Children {
		types[i] = b.Type()
	}
	if want := []string{"styp", "emsg", "emsg", "moof", "mdat"}; !slices.Equal(types, want) {
		t.Fatalf("boxes = %v, want %v", types, want)
	}
	if len(file.Segments) != 1 || len(file.Segments[0].Fragments) != 1 {
		t.Fatalf("got %d segments, want 1 with 1 fragment", len(file.Segments))
	}
	seg := file.Segments[0]
	if seg.Styp.MajorBrand() != "cmfs" || !slices.Equal(seg.Styp.CompatibleBrands(), []string{"cmfs", "msdh", "msix"}) {
		t.Errorf("styp = %s %v", seg.Styp.MajorBrand(), seg.Styp.CompatibleBrands())
	}
	frag := seg.Fragments[0]
	if got := frag.Moof.Mfhd.SequenceNumber; got != 8 {
		t.Errorf("seq = %d, want 8", got)
	}
	tfdt := frag.Moof.Traf.Tfdt
	if tfdt.Version != 1 || tfdt.BaseMediaDecodeTime() != 7*90000 {
		t.Errorf("tfdt = v%d %d, want v1 %d", tfdt.Version, tfdt.BaseMediaDecodeTime(), 7*90000)
	}
	if !frag.Moof.Traf.Tfhd.DefaultBaseIfMoof() || frag.Moof.Traf.Tfhd.TrackID != 1 {
		t.Errorf("tfhd = %+v", frag.Moof.Traf.Tfhd)
	}

	for i, e := range frag.Emsgs {
		want := f.Emsgs[i]
		if e.Version != 1 || e.SchemeIDURI != scte35Scheme || e.Value != "" || e.TimeScale != 90000 ||
			e.PresentationTime != uint64(want.PresentationTicks) || int64(e.EventDuration) != want.DurationTicks ||
			e.ID != want.EventID || !bytes.Equal(e.MessageData, want.Message) {
			t.Errorf("emsg %d = %+v, want %+v", i, e, want)
		}
	}

	full, err := frag.GetFullSamples(nil)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for i, s := range full {
		total += int(s.Size)
		if s.Dur != 3600 || s.IsSync() != (i == 0) {
			t.Errorf("sample %d: dur %d sync %v", i, s.Dur, s.IsSync())
		}
		if !bytes.HasPrefix(s.Data, samples[i].Data) {
			t.Errorf("sample %d does not start with its real bytes", i)
		}
	}
	if len(full) != 3 || total != 1000 || len(frag.Mdat.Data) != 1000 {
		t.Errorf("%d samples, sizes sum %d, mdat %d bytes; want 3, 1000, 1000", len(full), total, len(frag.Mdat.Data))
	}
	// Every sample is still a valid run of length-prefixed NAL units.
	for i, s := range full {
		for d := s.Data; len(d) > 0; {
			n := 4 + int(d[0])<<24 | int(d[1])<<16 | int(d[2])<<8 | int(d[3])
			if n > len(d) {
				t.Fatalf("sample %d: NAL of %d bytes overruns %d", i, n, len(d))
			}
			d = d[n:]
		}
	}
}

func TestFillerSizes(t *testing.T) {
	five := make([]live.Sample, 5)
	five[0].Data = make([]byte, 100)
	tests := []struct {
		name  string
		padTo int
		want  []int
	}{
		{"no padding", 0, []int{0, 0, 0, 0, 0}},
		{"already larger", 50, []int{0, 0, 0, 0, 0}},
		{"too small for a NAL", 105, []int{0, 0, 0, 0, 0}},
		{"one NAL", 111, []int{11, 0, 0, 0, 0}},
		{"two NALs spread", 113, []int{7, 0, 6, 0, 0}},
		{"all samples", 1100, []int{200, 200, 200, 200, 200}},
		{"remainder", 1103, []int{201, 201, 201, 200, 200}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fillerSizes(five, tt.padTo)
			if !slices.Equal(got, tt.want) {
				t.Errorf("fillerSizes(%d) = %v, want %v", tt.padTo, got, tt.want)
			}
		})
	}
}

func TestFillerNAL(t *testing.T) {
	got := fillerNAL(8)
	if want := []byte{0, 0, 0, 4, 12, 0xFF, 0xFF, 0x80}; !bytes.Equal(got, want) {
		t.Errorf("fillerNAL(8) = %x, want %x", got, want)
	}
}
