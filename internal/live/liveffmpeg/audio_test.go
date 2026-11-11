package liveffmpeg

import (
	"strings"
	"testing"

	"ottstreamer/internal/stream"
)

func TestBuildAudioArgs(t *testing.T) {
	args := strings.Join(buildAudioArgs(stream.Audio{Codec: stream.CodecAC3, Channels: 6, BitrateKbps: 384, TicHz: 480, TocHz: 1500}, 25, "/x/a.mp4"), " ")
	for _, want := range []string{":s=48000:c=5.1:d=24", "-c:a ac3 -b:a 384k", "(t+256/48000)", "/x/a.mp4"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
}

func TestAudioExprs(t *testing.T) {
	a := stream.Audio{TicHz: 480, TocHz: 1500}
	for _, ch := range []int{1, 2} {
		a.Channels = ch
		exprs := audioExprs(a, 25, 1024)
		if len(exprs) != ch {
			t.Fatalf("%d channels: %d expressions", ch, len(exprs))
		}
		for _, e := range exprs {
			if e != exprs[0] || !strings.Contains(e, "if(mod(floor((t+1024/48000)),2),480,1500)") || !strings.HasPrefix(e, "if(lt((") {
				t.Errorf("%d channels: unexpected expression %s", ch, e)
			}
		}
	}
	a.Channels = 6
	exprs := audioExprs(a, 50, 0)
	if len(exprs) != 6 {
		t.Fatalf("5.1: %d expressions", len(exprs))
	}
	for c, e := range exprs {
		lfe := strings.Contains(e, "*60*") && strings.Contains(e, ",0.1)")
		if lfe != (c == 3) {
			t.Errorf("channel %d: LFE tone %v", c, lfe)
		}
		if !strings.HasPrefix(e, "if(eq(mod(floor((t+0/48000)),8),"+string(rune('0'+c))+")+gte(") {
			t.Errorf("channel %d gate: %s", c, e)
		}
		if c != 3 && !strings.Contains(e, ",1/50)") {
			t.Errorf("channel %d: burst is not one frame long: %s", c, e)
		}
	}
}
