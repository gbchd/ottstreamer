package ulid

import (
	"strings"
	"testing"
	"time"
)

func TestEncode(t *testing.T) {
	// The spec's example time, 1469918176385 ms, encodes as 01ARYZ6S41.
	var b [16]byte
	copy(b[:], []byte{0x01, 0x56, 0x3d, 0xf3, 0x64, 0x81})
	if got := encode(b); got != "01ARYZ6S41"+strings.Repeat("0", 16) {
		t.Errorf("encode = %s", got)
	}
	for i := range b {
		b[i] = 0xff
	}
	if got := encode(b); got != "7"+strings.Repeat("Z", 25) {
		t.Errorf("encode(max) = %s", got)
	}
}

func TestGenerator_New(t *testing.T) {
	var g Generator
	t0 := time.UnixMilli(1469918176385)
	ids := []string{
		g.New(t0),
		g.New(t0),                       // same millisecond: incremented
		g.New(t0.Add(-time.Second)),     // clock stepped back
		g.New(t0.Add(time.Millisecond)), // later: fresh entropy
	}
	for i, id := range ids {
		if len(id) != 26 || !strings.HasPrefix(id, "01ARYZ6S4") {
			t.Errorf("id %d = %s", i, id)
		}
		if i > 0 && id <= ids[i-1] {
			t.Errorf("id %d = %s, not after %s", i, id, ids[i-1])
		}
	}
}

func TestIncrement(t *testing.T) {
	b := []byte{0x00, 0xff, 0xff}
	if increment(b) || b[0] != 1 || b[1] != 0 || b[2] != 0 {
		t.Errorf("increment = %x", b)
	}
	b = []byte{0xff, 0xff}
	if !increment(b) || b[0] != 0 || b[1] != 0 {
		t.Errorf("increment(max) = %x, want a wrap", b)
	}
}
