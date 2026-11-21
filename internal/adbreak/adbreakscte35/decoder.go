package adbreakscte35

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/Comcast/scte35-go/pkg/scte35"

	"ottstreamer/internal/adbreak"
)

// commands names each splice_command_type; scte35-go reads any other type
// as a private_command.
var commands = map[uint32]string{
	scte35.SpliceNullType:           "splice_null",
	scte35.SpliceScheduleType:       "splice_schedule",
	scte35.SpliceInsertType:         "splice_insert",
	scte35.TimeSignalType:           "time_signal",
	scte35.BandwidthReservationType: "bandwidth_reservation",
	scte35.PrivateCommandType:       "private_command",
}

// Decoder decodes splice_info_sections to scte35-go's JSON form.
type Decoder struct{}

// NewDecoder returns a Decoder.
func NewDecoder() Decoder { return Decoder{} }

// Decode returns the splice_command name of the splice_info_section in b
// and the section as scte35-go marshals it. A section that does not decode,
// CRC_32 included, gives an error wrapping adbreak.ErrInvalid.
func (Decoder) Decode(b []byte) (string, json.RawMessage, error) {
	sis, err := scte35.DecodeBytes(b)
	if err != nil {
		var ok bool
		if sis, ok = decodeCancel(b); !ok {
			return "", nil, fmt.Errorf("%w: scte35: %w", adbreak.ErrInvalid, err)
		}
	}
	j, err := json.Marshal(sis)
	if err != nil {
		return "", nil, fmt.Errorf("adbreakscte35: marshal: %w", err)
	}
	return commands[sis.SpliceCommand.Type()], j, nil
}

// decodeCancel decodes an unencrypted splice_insert with
// splice_event_cancel_indicator 1, which scte35-go 1.8.0 cannot: it reads
// unique_program_id and avail_* past the 5 bytes of such a command. It
// checks the CRC_32, lets scte35-go decode the section with a splice_null in
// place of the command, then puts the splice_insert back. Drop it once
// scte35-go reads those fields only when the cancel flag is 0.
func decodeCancel(b []byte) (*scte35.SpliceInfoSection, bool) {
	// The command follows the 14-byte header, which ends with the 12-bit
	// splice_command_length and the splice_command_type. A cancel is
	// splice_event_id, then the flag in the top bit of the next byte.
	const at, size = 14, 5
	if len(b) < at+size+2+4 || b[4]&0x80 != 0 || b[11]&0x0F != 0 || b[12] != size ||
		b[13] != scte35.SpliceInsertType || b[at+4]&0x80 == 0 || crc32MPEG2(b) != 0 {
		return nil, false
	}
	// section_length stays as it was: scte35-go skips it.
	null := slices.Concat(b[:12], []byte{0, scte35.SpliceNullType}, b[at+size:len(b)-4])
	sis, err := scte35.DecodeBytes(binary.BigEndian.AppendUint32(null, crc32MPEG2(null)))
	if err != nil {
		return nil, false
	}
	sis.SpliceCommand = &scte35.SpliceInsert{SpliceEventID: binary.BigEndian.Uint32(b[at:]), SpliceEventCancelIndicator: true}
	return sis, true
}

// crc32MPEG2 is the CRC_32 of a splice_info_section, which scte35-go does
// not export. Over a whole section, CRC_32 included, it is 0.
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
