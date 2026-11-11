package liveffmpeg

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"ottstreamer/internal/stream"
)

// audioSeconds of PCM are encoded per audio rendition; the middle loopSeconds
// period is kept, away from the encoder's start-up and flush.
const (
	audioSeconds = 24
	loopSeconds  = 8
)

// priming is the encoder delay in samples, the edit list media_time ffmpeg
// writes. The PCM is advanced by it so that output frame k plays pattern
// time k*FrameSize.
var priming = map[stream.AudioCodec]int{stream.CodecAAC: 1024, stream.CodecAC3: 256}

var layouts = map[int]string{1: "mono", 2: "stereo", 6: "5.1"}

var audioEncoders = map[stream.AudioCodec]string{stream.CodecAAC: "aac", stream.CodecAC3: "ac3"}

// buildAudioArgs returns the ffmpeg arguments that encode audioSeconds of
// beeps for a into the fragmented MP4 out.
func buildAudioArgs(a stream.Audio, fps int, out string) []string {
	src := fmt.Sprintf("aevalsrc=exprs=%s:s=48000:c=%s:d=%d",
		quote(strings.Join(audioExprs(a, fps, priming[a.Codec]), "|")), layouts[a.Channels], audioSeconds)
	return []string{
		"-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", src,
		"-c:a", audioEncoders[a.Codec], "-b:a", strconv.Itoa(a.BitrateKbps) + "k",
		"-f", "mp4", "-movflags", "empty_moov+delay_moov+default_base_moof", "-frag_duration", "1000000",
		out,
	}
}

// audioExprs returns one aevalsrc expression per channel: at the start of each
// second a burst one video frame long, TocHz on even seconds and TicHz on odd
// ones. On 5.1 the burst moves L, R, C, LFE (60 Hz, 100 ms), Ls, Rs, then
// sounds on all six for two seconds; other layouts beep on every channel.
func audioExprs(a stream.Audio, fps, shift int) []string {
	t := fmt.Sprintf("(t+%d/48000)", shift)
	sec := "floor(" + t + ")"
	u := "(" + t + "-" + sec + ")" // time within the second
	beep := burst(u, fmt.Sprintf("if(mod(%s,2),%d,%d)", sec, a.TicHz, a.TocHz), "1/"+strconv.Itoa(fps))
	if a.Channels != 6 {
		return slices.Repeat([]string{beep}, a.Channels)
	}
	exprs := make([]string, 6)
	for c := range exprs {
		b := beep
		if c == 3 {
			b = burst(u, "60", "0.1")
		}
		exprs[c] = fmt.Sprintf("if(eq(mod(%[1]s,8),%[2]d)+gte(mod(%[1]s,8),6),%[3]s,0)", sec, c, b)
	}
	return exprs
}

// burst is a sine of hz during [0, dur) of u, with 1 ms raised-cosine fades
// and a 0.5 (-6 dBFS) peak.
func burst(u, hz, dur string) string {
	// if() evaluates only the branch taken, so silence costs one comparison.
	return fmt.Sprintf("if(lt(%[1]s,%[3]s),0.25*(1-cos(PI*min(min(%[1]s,%[3]s-%[1]s),0.001)/0.001))*sin(2*PI*%[2]s*%[1]s),0)", u, hz, dur)
}
