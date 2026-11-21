// Package livemp4 writes CMAF media segments with mp4ff.
package livemp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/Eyevinn/mp4ff/mp4"

	"ottstreamer/internal/live"
)

const scte35Scheme = "urn:scte:scte35:2013:bin"

// fillers maps a codec string's sample entry to its filler unit: the
// smallest size in bytes and the builder of one unit of n bytes.
var fillers = map[string]struct {
	min   int
	build func(n int) []byte
}{
	"avc1": {6, fillerNAL},  // 4-byte length, header, trailing bits
	"hvc1": {7, fillerHEVC}, // 4-byte length, 2-byte header, trailing bits
	"av01": {2, paddingOBU}, // header, obu_size 0
}

var _ live.SegmentWriter = (*Writer)(nil)

// Writer writes styp, emsg, moof and mdat for one fragment.
type Writer struct{}

// NewWriter returns a Writer.
func NewWriter() *Writer {
	return &Writer{}
}

// WriteSegment writes f as one CMAF media segment. Sample data is referenced,
// not copied; f must not change until WriteSegment returns.
func (*Writer) WriteSegment(w io.Writer, f live.Fragment) error {
	if len(f.Samples) == 0 {
		return fmt.Errorf("segment %d: no samples", f.Seq)
	}
	frag, err := mp4.CreateFragment(f.Seq, f.TrackID)
	if err != nil {
		return fmt.Errorf("create fragment: %w", err)
	}
	// Constant duration and non-sync flags move to tfhd defaults; the IDR
	// becomes trun's first-sample flags.
	frag.EncOptimize = mp4.OptimizeTrun
	for _, s := range f.Emsgs {
		frag.AddEmsg(&mp4.EmsgBox{
			Version:          1,
			TimeScale:        90000,
			PresentationTime: uint64(max(s.PresentationTicks, 0)),
			EventDuration:    uint32(min(max(s.DurationTicks, 0), math.MaxUint32)),
			ID:               s.EventID,
			SchemeIDURI:      scte35Scheme,
			MessageData:      s.Message,
		})
	}
	tfdt := frag.Moof.Traf.Tfdt
	tfdt.SetBaseMediaDecodeTime(f.BaseTime)
	tfdt.Version = 1

	entry, _, _ := strings.Cut(f.Codec, ".")
	sizes := make([]int, len(f.Samples)) // unknown codec: no padding
	filler, ok := fillers[entry]
	if ok {
		sizes = fillerSizes(f.Samples, f.PadTo, filler.min)
	}
	trun, mdat := frag.Moof.Traf.Trun, frag.Mdat
	for i, s := range f.Samples {
		flags := mp4.NonSyncSampleFlags
		if s.Sync {
			flags = mp4.SyncSampleFlags
		}
		mdat.AddSampleDataPart(s.Data)
		size := len(s.Data)
		if n := sizes[i]; n > 0 {
			mdat.AddSampleDataPart(filler.build(n))
			size += n
		}
		trun.AddSample(mp4.NewSample(flags, f.SampleDur, uint32(size), 0)) //nolint:gosec // sample sizes are far below 4 GiB
	}

	styp := mp4.NewStyp("cmfs", 0, []string{"cmfs", "msdh", "msix"})
	if err := styp.Encode(w); err != nil {
		return fmt.Errorf("write styp: %w", err)
	}
	if err := frag.Encode(w); err != nil {
		return fmt.Errorf("write fragment: %w", err)
	}
	return nil
}

// fillerSizes spreads padTo minus the real sample bytes over filler units,
// at most one per sample, each at least minFiller bytes. The sizes sum to the
// deficit exactly; a deficit below minFiller is left unpadded.
func fillerSizes(samples []live.Sample, padTo, minFiller int) []int {
	sizes := make([]int, len(samples))
	deficit := padTo
	for _, s := range samples {
		deficit -= len(s.Data)
	}
	m := min(len(samples), deficit/minFiller)
	for j := range m {
		sizes[j*len(samples)/m] = deficit/m + btoi(j < deficit%m)
	}
	return sizes
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fillerNAL returns an H.264 filler data NAL unit (type 12) of n bytes
// including its 4-byte length prefix: header, 0xFF payload, rbsp trailing bits.
func fillerNAL(n int) []byte {
	b := make([]byte, n)
	binary.BigEndian.PutUint32(b, uint32(n-4)) //nolint:gosec // n is a filler size, far below 4 GiB
	b[4] = 12
	for i := 5; i < n-1; i++ {
		b[i] = 0xFF
	}
	b[n-1] = 0x80
	return b
}

// fillerHEVC returns an HEVC filler data NAL unit (FD_NUT, type 38) of n
// bytes including its 4-byte length prefix: a 2-byte header (layer 0,
// TemporalId 0), 0xFF payload, rbsp trailing bits.
func fillerHEVC(n int) []byte {
	b := make([]byte, n)
	binary.BigEndian.PutUint32(b, uint32(n-4)) //nolint:gosec // n is a filler size, far below 4 GiB
	b[4], b[5] = 38<<1, 1
	for i := 6; i < n-1; i++ {
		b[i] = 0xFF
	}
	b[n-1] = 0x80
	return b
}

// paddingOBU returns an AV1 padding OBU (type 15) of n bytes: a header with
// obu_has_size_field, obu_size as leb128, then zero padding bytes and the
// trailing bits that every non-empty OBU other than tile data ends with
// (AV1 spec 5.3.1). obu_size takes 1 byte up to n = 129, else a fixed
// 4-byte leb128, so every n >= 2 is reachable.
func paddingOBU(n int) []byte {
	width := 1
	if n > 129 {
		width = 4
	}
	size := n - 1 - width
	b := make([]byte, n)
	b[0] = 15<<3 | 1<<1
	for i := range width {
		b[1+i] = byte(size >> (7 * i) & 0x7F)
		if i < width-1 {
			b[1+i] |= 0x80
		}
	}
	if size > 0 {
		b[n-1] = 0x80
	}
	return b
}
