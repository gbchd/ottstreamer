// Package livemp4 writes CMAF media segments with mp4ff.
package livemp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/Eyevinn/mp4ff/mp4"

	"ottstreamer/internal/live"
)

const (
	scte35Scheme = "urn:scte:scte35:2013:bin"
	// minFiller is the smallest filler NAL unit: 4-byte length, header, trailing bits.
	minFiller = 6
)

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

	fillers := fillerSizes(f.Samples, f.PadTo)
	trun, mdat := frag.Moof.Traf.Trun, frag.Mdat
	for i, s := range f.Samples {
		flags := mp4.NonSyncSampleFlags
		if s.Sync {
			flags = mp4.SyncSampleFlags
		}
		mdat.AddSampleDataPart(s.Data)
		size := len(s.Data)
		if n := fillers[i]; n > 0 {
			mdat.AddSampleDataPart(fillerNAL(n))
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

// fillerSizes spreads padTo minus the real sample bytes over filler NAL
// units, at most one per sample, each at least minFiller bytes. The sizes sum
// to the deficit exactly; a deficit below minFiller is left unpadded.
func fillerSizes(samples []live.Sample, padTo int) []int {
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
