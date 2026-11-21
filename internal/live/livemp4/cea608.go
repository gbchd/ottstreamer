package livemp4

import (
	"encoding/binary"
	"strings"

	"ottstreamer/internal/live"
)

// CEA-608 pop-on captions, carried as ATSC A/53 cc_data: in an SEI NAL unit
// (user_data_registered_itu_t_t35) for H.264 and HEVC, in an ITU-T T.35
// metadata OBU for AV1.
//
// Each clock second gets pairsPerSecond byte pairs, spread over its frames
// as evenly as whole pairs allow (the 608 field rate, rounded to 30/s), the
// first one on the second's first frame. The pairs of second s are:
// end of caption, which shows caption s exactly on its first frame; then
// resume caption loading, erase non-displayed memory and caption s+1, each
// row a preamble address code (rows 3 and 4, centred) and its characters;
// then null pairs. Control
// codes are sent once, not doubled, to fit the budget.

const (
	pairsPerSecond = 30
	rowWidth       = 32
	firstRow       = 3 // rows 3 and 4: below the burnt-in clock, above the subtitle cue
)

type pair [2]byte

var nullPair = pair{0x80, 0x80} // 0x00 0x00 with odd parity

// channel608 is what a 608 channel changes in the bytes: the first byte of
// miscellaneous control codes, the data channel bit of preamble address
// codes, and the field.
type channel608 struct {
	misc   byte
	ch2    byte // 0x08 on CC2 and CC4
	field2 bool
}

var channels608 = map[string]channel608{
	"CC1": {misc: 0x14},
	"CC2": {misc: 0x1C, ch2: 0x08},
	"CC3": {misc: 0x15, field2: true},
	"CC4": {misc: 0x1D, ch2: 0x08, field2: true},
}

// Miscellaneous control codes, the second byte.
const (
	rcl = 0x20 // resume caption loading: pop-on mode
	enm = 0x2E // erase non-displayed memory
	eoc = 0x2F // end of caption: swap memories
)

// withParity sets bit 7 so that b has odd parity.
func withParity(b byte) byte {
	b &= 0x7F
	ones := 0
	for v := b; v != 0; v >>= 1 {
		ones += int(v & 1)
	}
	if ones%2 == 0 {
		b |= 0x80
	}
	return b
}

func ctrl(a, b byte) pair { return pair{withParity(a), withParity(b)} }

// pac is the preamble address code of row (1-15) at indent column (a
// multiple of 4), white, no underline.
func pac(ch channel608, row, indent int) pair {
	first := [16]byte{0, 0x11, 0x11, 0x12, 0x12, 0x15, 0x15, 0x16, 0x16, 0x17, 0x17, 0x10, 0x13, 0x13, 0x14, 0x14}
	high := [16]byte{0, 0, 0x20, 0, 0x20, 0, 0x20, 0, 0x20, 0, 0x20, 0, 0, 0x20, 0, 0x20}
	return ctrl(first[row]|ch.ch2, 0x50|high[row]|byte(indent/4)<<1) //nolint:gosec // indent is 0-28
}

// textPairs returns s as character pairs of the basic 608 set; characters
// outside it become '?'. An odd last character is paired with a null.
func textPairs(s string) []pair {
	b := make([]byte, 0, len(s)+1)
	for _, r := range s {
		if r < 0x20 || r > 0x7E || strings.ContainsRune("*\\^_`{|}~", r) { // these codes are accented letters in 608
			r = '?'
		}
		b = append(b, byte(r))
	}
	if len(b)%2 == 1 {
		b = append(b, 0)
	}
	out := make([]pair, 0, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		out = append(out, pair{withParity(b[i]), withParity(b[i+1])})
	}
	return out
}

// secondPairs returns the pairsPerSecond pairs of a second that shows the
// caption loaded before it and loads next, when there is one.
func secondPairs(ch channel608, next *live.Caption) []pair {
	out := make([]pair, 0, pairsPerSecond)
	out = append(out, ctrl(ch.misc, eoc))
	if next != nil {
		out = append(out, ctrl(ch.misc, rcl), ctrl(ch.misc, enm))
		for i, row := range next.Rows {
			if row = row[:min(len(row), rowWidth)]; row == "" {
				continue
			}
			// Centred to the nearest indent; tab offsets would cost a pair.
			free := rowWidth - len(row)
			indent := min((free/2+2)/4*4, free/4*4)
			out = append(out, pac(ch, firstRow+i, indent))
			out = append(out, textPairs(row)...)
		}
	}
	out = out[:min(len(out), pairsPerSecond)] // ponytail: an over-long caption loses its end; rows of the live text stay within 30 pairs
	for len(out) < pairsPerSecond {
		out = append(out, nullPair)
	}
	return out
}

// framePairs returns the pairs of frame i (from 0) of a second of fps
// frames, out of that second's pairs: frame i gets pairs
// [ceil(i*30/fps), ceil((i+1)*30/fps)), so the first frame always gets one.
func framePairs(second []pair, i, fps int) []pair {
	lo := (i*pairsPerSecond + fps - 1) / fps
	hi := ((i+1)*pairsPerSecond + fps - 1) / fps
	return second[lo:hi]
}

// ccData is the ITU-T T.35 payload of pairs: country code (United States),
// provider (ATSC), user identifier GA94, then A/53 cc_data.
func ccData(pairs []pair, field2 bool) []byte {
	b := make([]byte, 0, 11+3*len(pairs))
	b = append(b, 0xB5, 0x00, 0x31, 'G', 'A', '9', '4', 0x03, 0xC0|byte(len(pairs)), 0xFF) //nolint:gosec // at most 2 pairs a frame
	typ := byte(0xFC)                                                                      // marker bits, cc_valid, cc_type 0: field 1
	if field2 {
		typ = 0xFD
	}
	for _, p := range pairs {
		b = append(b, typ, p[0], p[1])
	}
	return append(b, 0xFF)
}

// captionUnit returns the NAL unit or OBU carrying payload for codec entry
// (avc1, hvc1 or av01), with its 4-byte length prefix for NAL units.
func captionUnit(entry string, payload []byte) []byte {
	switch entry {
	case "av01":
		body := append([]byte{4}, payload...)                        // metadata_type ITUT_T35
		body = append(body, 0x80)                                    // trailing bits
		return append([]byte{5<<3 | 1<<1, byte(len(body))}, body...) //nolint:gosec // under 128 bytes, so obu_size is one leb128 byte
	case "hvc1":
		return lengthPrefixed(append([]byte{39 << 1, 1}, sei(payload)...)) // PREFIX_SEI_NUT, layer 0, TemporalId 0
	default:
		return lengthPrefixed(append([]byte{6}, sei(payload)...))
	}
}

// sei is an SEI message of type 4 (user_data_registered_itu_t_t35) with
// emulation prevention, then the rbsp trailing bits.
func sei(payload []byte) []byte {
	raw := append([]byte{4, byte(len(payload))}, payload...) //nolint:gosec // under 255 bytes
	out := make([]byte, 0, len(raw)+4)
	zeros := 0
	for _, b := range raw {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return append(out, 0x80)
}

func lengthPrefixed(nal []byte) []byte {
	return append(binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(nal)), uint32(len(nal))), nal...) //nolint:gosec // a short SEI
}

// firstPicture returns the offset in sample of its first VCL NAL unit (H.264
// and HEVC, 4-byte lengths) or frame OBU (AV1), where the caption unit goes:
// after parameter sets and sequence headers, before the picture.
func firstPicture(entry string, sample []byte) int {
	for off := 0; off < len(sample); {
		if entry == "av01" {
			typ := sample[off] >> 3 & 0x0F
			if typ == 3 || typ == 4 || typ == 6 { // frame header, tile group, frame
				return off
			}
			hdr := 1 + int(sample[off]>>2&1) // extension byte
			size, n := leb128(sample[off+hdr:])
			off += hdr + n + size
			continue
		}
		if off+5 > len(sample) {
			break
		}
		typ := sample[off+4]
		if entry == "hvc1" && typ>>1&0x3F < 32 || entry != "hvc1" && typ&0x1F >= 1 && typ&0x1F <= 5 {
			return off
		}
		off += 4 + int(binary.BigEndian.Uint32(sample[off:]))
	}
	return 0
}

func leb128(b []byte) (value, n int) {
	for i := 0; i < len(b) && i < 8; i++ {
		value |= int(b[i]&0x7F) << (7 * i)
		if b[i]&0x80 == 0 {
			return value, i + 1
		}
	}
	return value, len(b)
}

// withCaptions returns f's samples with the caption unit of each frame
// inserted, in new buffers; frames without pairs are left as they are.
func withCaptions(f live.Fragment, entry string) []live.Sample {
	ch, ok := channels608[f.CCChannel]
	if !ok || f.SampleDur == 0 {
		return f.Samples
	}
	fps := int(90000 / f.SampleDur)
	byS := make(map[int64]*live.Caption, len(f.Captions))
	for i := range f.Captions {
		byS[f.Captions[i].Second] = &f.Captions[i]
	}
	first := int64(f.BaseTime / uint64(f.SampleDur)) //nolint:gosec // frame indexes fit in int64
	out := make([]live.Sample, len(f.Samples))
	var second []pair
	for i, s := range f.Samples {
		frame := first + int64(i)
		sec, idx := frame/int64(fps), int(frame%int64(fps))
		if idx == 0 || second == nil {
			second = secondPairs(ch, byS[sec+1])
		}
		out[i] = s
		pairs := framePairs(second, idx, fps)
		if len(pairs) == 0 {
			continue
		}
		unit := captionUnit(entry, ccData(pairs, ch.field2))
		at := firstPicture(entry, s.Data)
		data := make([]byte, 0, len(s.Data)+len(unit))
		data = append(append(append(data, s.Data[:at]...), unit...), s.Data[at:]...)
		out[i].Data = data
	}
	return out
}
