package livemp4

import (
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"ottstreamer/internal/live"
)

func TestWithParity(t *testing.T) {
	// Known pop-on codes on CC1: RCL 94 20, ENM 94 AE, EOC 94 2F.
	for _, tc := range []struct{ in, want byte }{{0x14, 0x94}, {0x20, 0x20}, {0x2E, 0xAE}, {0x2F, 0x2F}, {0x00, 0x80}, {'A', 0xC1}} {
		if got := withParity(tc.in); got != tc.want {
			t.Errorf("withParity(%#x) = %#x, want %#x", tc.in, got, tc.want)
		}
	}
}

func TestFramePairs(t *testing.T) {
	second := make([]pair, pairsPerSecond)
	for _, fps := range []int{24, 25, 30, 50, 60} {
		total := 0
		for i := range fps {
			n := len(framePairs(second, i, fps))
			if i == 0 && n == 0 {
				t.Errorf("%d fps: the first frame carries no pair", fps)
			}
			total += n
		}
		if total != pairsPerSecond {
			t.Errorf("%d fps: %d pairs a second, want %d", fps, total, pairsPerSecond)
		}
	}
}

// decoder608 plays pop-on captions of one channel: it keeps both memories
// and records what an end of caption shows.
type decoder608 struct {
	mem, shown [2][rowWidth]byte
	row, col   int
	shows      map[int64][2]string // frame of each end of caption -> rows shown
}

func (d *decoder608) feed(frame int64, p pair) {
	a, b := p[0]&0x7F, p[1]&0x7F
	switch {
	case a == 0 && b == 0:
	case a&^0x09 == 0x14 && b == enm:
		d.mem = [2][rowWidth]byte{}
	case a&^0x09 == 0x14 && b == eoc:
		d.mem, d.shown = d.shown, d.mem
		d.shows[frame] = [2]string{d.text(0), d.text(1)}
	case a&^0x09 == 0x14:
	case a&^0x08 == 0x12 && b >= 0x40: // rows 3 and 4
		d.row, d.col = int(b>>5&1), int(b>>1&7)*4
	case a >= 0x20:
		for _, c := range []byte{a, b} {
			if c >= 0x20 && d.col < rowWidth {
				d.mem[d.row][d.col] = c
				d.col++
			}
		}
	}
}

// text is a shown row, with its leading spaces as the column it starts at.
func (d *decoder608) text(row int) string {
	return strings.TrimRight(strings.Map(func(r rune) rune {
		if r == 0 {
			return ' '
		}
		return r
	}, string(d.shown[row][:])), " ")
}

// units returns the NAL units of an H.264 or HEVC sample.
func units(sample []byte) [][]byte {
	var out [][]byte
	for off := 0; off+4 <= len(sample); {
		n := int(binary.BigEndian.Uint32(sample[off:]))
		out = append(out, sample[off+4:off+4+n])
		off += 4 + n
	}
	return out
}

// pairsOf reads the cc_data pairs of a T.35 GA94 payload.
func pairsOf(t *testing.T, payload []byte) (pairs []pair, field2 bool) {
	t.Helper()
	if string(payload[:7]) != "\xb5\x00\x31GA94" || payload[7] != 3 {
		t.Fatalf("payload % x is not GA94 cc_data", payload)
	}
	n := int(payload[8] & 0x1F)
	for i := range n {
		tr := payload[10+3*i:]
		field2 = tr[0]&3 == 1
		pairs = append(pairs, pair{tr[1], tr[2]})
	}
	return pairs, field2
}

func avcSample(idr bool) []byte {
	var b []byte
	for _, nal := range [][]byte{{0x09, 0xF0}, {0x67, 0x64}, {0x68, 0xEE}, {0x65, 0x88, 0x84}} {
		if !idr && nal[0] != 0x09 {
			if nal[0] != 0x65 {
				continue
			}
			nal = []byte{0x41, 0x9A}
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(nal))) //nolint:gosec // tiny test NALs
		b = append(b, nal...)
	}
	return b
}

func TestWithCaptions_avc(t *testing.T) {
	// 2 s at 25 fps from second 6: shows caption 7 at frame 175, loads 7 and 8.
	f := live.Fragment{Codec: "avc1.640028", BaseTime: 150 * 3600, SampleDur: 3600, CCChannel: "CC1", Captions: []live.Caption{
		{Second: 7, Rows: [2]string{"CC1 - 18:00:07", "frame 175 - Bop!"}},
		{Second: 8, Rows: [2]string{"[AD 120/120] CC1 - 18:00:08", "frame 12345678 - Bip!"}},
	}}
	for i := range 50 {
		f.Samples = append(f.Samples, live.Sample{Data: avcSample(i == 0), Sync: i == 0})
	}
	got := withCaptions(f, "avc1")
	d := decoder608{shows: map[int64][2]string{}}
	perSecond := map[int64]int{}
	for i, s := range got {
		frame := int64(150 + i)
		nals := units(s.Data)
		k := slices.IndexFunc(nals, func(n []byte) bool { return n[0] == 6 })
		vcl := slices.IndexFunc(nals, func(n []byte) bool { return n[0]&0x1F >= 1 && n[0]&0x1F <= 5 })
		if k < 0 || k != vcl-1 || nals[0][0] != 0x09 {
			t.Fatalf("frame %d: SEI at %d, picture at %d, first NAL %#x", frame, k, vcl, nals[0][0])
		}
		sei := nals[k]
		if sei[1] != 4 || int(sei[2]) != len(sei)-4 || sei[len(sei)-1] != 0x80 {
			t.Fatalf("frame %d: SEI % x", frame, sei)
		}
		pairs, field2 := pairsOf(t, sei[3:len(sei)-1])
		if field2 {
			t.Fatalf("frame %d: CC1 on field 2", frame)
		}
		perSecond[frame/25] += len(pairs)
		for _, p := range pairs {
			d.feed(frame, p)
		}
		if string(f.Samples[i].Data) != string(avcSample(i == 0)) {
			t.Fatal("withCaptions changed the input samples")
		}
	}
	if perSecond[6] != pairsPerSecond || perSecond[7] != pairsPerSecond {
		t.Errorf("pairs per second = %v, want %d", perSecond, pairsPerSecond)
	}
	want := map[int64][2]string{
		150: {"", ""},
		175: {"        CC1 - 18:00:07", "        frame 175 - Bop!"},
	}
	if len(d.shows) != len(want) || d.shows[150] != want[150] || d.shows[175] != want[175] {
		t.Errorf("shown =\n%v\nwant\n%v", d.shows, want)
	}
	// Caption 8, the longest the live text makes, is fully loaded by the
	// end of second 7: its end of caption would show it.
	d.feed(200, ctrl(0x14, eoc))
	if got, want := d.shows[200], [2]string{"    [AD 120/120] CC1 - 18:00:08", "    frame 12345678 - Bip!"}; got != want {
		t.Errorf("caption 8 = %q, want %q", got, want)
	}
}

func TestWithCaptions_field2AndChannel2(t *testing.T) {
	f := live.Fragment{Codec: "avc1.640028", SampleDur: 1500, CCChannel: "CC4", Samples: []live.Sample{{Data: avcSample(true), Sync: true}}}
	s := withCaptions(f, "avc1")[0]
	nals := units(s.Data)
	pairs, field2 := pairsOf(t, nals[3][3:len(nals[3])-1])
	if !field2 || len(pairs) != 1 || pairs[0] != ctrl(0x1D, eoc) {
		t.Errorf("CC4 frame 0 = %v on field 2 %t, want EOC 9d 2f on field 2", pairs, field2)
	}
}

func TestWithCaptions_hevc(t *testing.T) {
	var sample []byte
	for _, nal := range [][]byte{{32 << 1, 1, 0xAA}, {33 << 1, 1, 0xBB}, {34 << 1, 1, 0xCC}, {19 << 1, 1, 0xAF}} { // VPS, SPS, PPS, IDR_W_RADL
		sample = binary.BigEndian.AppendUint32(sample, uint32(len(nal))) //nolint:gosec // tiny test NALs
		sample = append(sample, nal...)
	}
	f := live.Fragment{Codec: "hvc1.1.6.L93.90", SampleDur: 3600, CCChannel: "CC1", Samples: []live.Sample{{Data: sample, Sync: true}}}
	nals := units(withCaptions(f, "hvc1")[0].Data)
	if len(nals) != 5 || nals[3][0]>>1 != 39 || nals[3][1] != 1 || nals[4][0]>>1 != 19 {
		t.Fatalf("NAL units = % x", nals)
	}
	if pairs, _ := pairsOf(t, nals[3][4:len(nals[3])-1]); len(pairs) != 2 || pairs[0] != ctrl(0x14, eoc) {
		t.Errorf("pairs = % x", pairs)
	}
}

func TestWithCaptions_av1(t *testing.T) {
	seqHdr := []byte{1<<3 | 1<<1, 2, 0x00, 0x00}
	frame := []byte{6<<3 | 1<<1, 3, 0x10, 0x20, 0x30}
	f := live.Fragment{Codec: "av01.0.08M.08", SampleDur: 3600, CCChannel: "CC1", Samples: []live.Sample{{Data: append(slices.Clone(seqHdr), frame...), Sync: true}}}
	data := withCaptions(f, "av01")[0].Data
	if string(data[:len(seqHdr)]) != string(seqHdr) || string(data[len(data)-len(frame):]) != string(frame) {
		t.Fatalf("sample = % x", data)
	}
	obu := data[len(seqHdr) : len(data)-len(frame)]
	if obu[0] != 5<<3|1<<1 || int(obu[1]) != len(obu)-2 || obu[2] != 4 || obu[len(obu)-1] != 0x80 {
		t.Fatalf("metadata OBU = % x", obu)
	}
	if pairs, _ := pairsOf(t, obu[3:len(obu)-1]); len(pairs) != 2 || pairs[0] != ctrl(0x14, eoc) {
		t.Errorf("pairs = % x", pairs)
	}
}

func TestSEI_emulationPrevention(t *testing.T) {
	got := sei([]byte{0x00, 0x00, 0x01, 0x00, 0x00, 0x00})
	want := []byte{4, 6, 0x00, 0x00, 0x03, 0x01, 0x00, 0x00, 0x03, 0x00, 0x80}
	if string(got) != string(want) {
		t.Errorf("sei = % x, want % x", got, want)
	}
}
