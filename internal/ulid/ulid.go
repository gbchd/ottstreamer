// Package ulid makes ULIDs: 128-bit IDs whose 26-character Crockford base32
// form sorts by creation time (https://github.com/ulid/spec).
package ulid

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Generator makes strictly increasing ULIDs, even within one millisecond or
// when the clock steps back. The zero value is ready; it is safe for
// concurrent use.
type Generator struct {
	mu      sync.Mutex
	ms      int64
	entropy [10]byte
}

// New returns a ULID for t: its millisecond, or the last one used if t is
// earlier, then 80 random bits, incremented instead of redrawn when the
// millisecond repeats.
func (g *Generator) New(t time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ms := t.UnixMilli(); ms > g.ms {
		g.ms = ms
		_, _ = rand.Read(g.entropy[:]) // never fails (Go 1.24+)
	} else if increment(g.entropy[:]) {
		g.ms++ // the 80 bits wrapped
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(g.ms)<<16) //nolint:gosec // ms > 0 after 1970; the top 48 bits are the time
	copy(b[6:], g.entropy[:])
	return encode(b)
}

// increment adds one to the big-endian number in b and reports a wrap.
func increment(b []byte) bool {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return false
		}
	}
	return true
}

// encode writes the 128 bits of b as 26 base32 digits, after two zero bits.
func encode(b [16]byte) string {
	var out [26]byte
	for i := range out {
		var v byte
		for j := range 5 {
			if p := i*5 + j - 2; p >= 0 && b[p/8]>>(7-p%8)&1 == 1 {
				v |= 1 << (4 - j)
			}
		}
		out[i] = alphabet[v]
	}
	return string(out[:])
}
