package liveffmpeg

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"ottstreamer/internal/stream"
)

// barColours are the 75% SMPTE bars: grey, yellow, cyan, green, magenta, red, blue.
var barColours = []string{"0xBFBFBF", "0xBFBF00", "0x00BFBF", "0x00BF00", "0xBF00BF", "0xBF0000", "0x0000BF"}

// top returns the rendition the picture is drawn at before scaling: the
// tallest, then the widest. The ladder is grouped by codec, so it need not
// be the first one.
func top(spec stream.Spec) stream.Video {
	return slices.MaxFunc(spec.Video, func(a, b stream.Video) int {
		return cmp.Or(cmp.Compare(a.Height, b.Height), cmp.Compare(a.Width, b.Width))
	})
}

// geometry returns the sweep disc's diameter, the waveform strip's height
// and the colour bars' inset for a top rendition h pixels tall.
func geometry(h int) (disc, strip, inset int) {
	return even((h*37 + 50) / 100), even(max(h/34, 6)), max(h/68, 2)
}

// buildOverlayArgs returns the ffmpeg arguments that render one cycle of the
// sweep disc and the waveform strip to out: two lossless FFV1 streams of
// 2·fps frames, frame i showing frame f of any second pair with f ≡ i mod
// 2·fps. Both pictures depend on nothing else, so segments replay this file
// instead of evaluating geq per pixel, which cost more than x264 did.
func buildOverlayArgs(spec stream.Spec, out string) []string {
	fps := spec.FPS
	t := top(spec)
	w, h := t.Width, t.Height
	disc, strip, _ := geometry(h)
	odd := fmt.Sprintf("mod(floor(N/%d),2)", fps)

	// Sweep disc: a wedge from 12 o'clock completes a turn per second; the
	// two greys swap every second.
	r := disc / 2
	discLum := fmt.Sprintf("if(lt(mod(atan2(X-%[1]d,%[1]d-Y)+2*PI,2*PI),2*PI*mod(N,%[2]d)/%[2]d),if(%[3]s,240,102),if(%[3]s,102,240))",
		r, fps, odd)
	discAlpha := fmt.Sprintf("if(lt(hypot(X-%[1]d+0.5,Y-%[1]d+0.5),%[1]d),255,0)", r)

	// Waveform strip: the beep as it sounds during frame 0 of a second (Hz/fps
	// cycles across the width), a flat line otherwise.
	tic, toc := beepHz(spec)
	hz := fmt.Sprintf("if(%s,%d,%d)", odd, tic, toc)
	waveAlpha := fmt.Sprintf("if(lt(abs(Y-%d-eq(mod(N,%d),0)*%d*sin(2*PI*%s*X/(W*%d))),1.5),255,0)",
		strip/2, fps, strip/2-2, hz, fps)

	color := func(cw, ch int) string { return fmt.Sprintf("color=c=black:s=%dx%d:r=%d", cw, ch, fps) }
	return []string{
		"-hide_banner", "-nostdin", "-v", "error", "-y",
		"-f", "lavfi", "-i", color(disc, disc),
		"-f", "lavfi", "-i", color(w, strip),
		"-filter_complex", strings.Join([]string{
			fmt.Sprintf("[0:v]format=yuva420p,geq=lum=%s:cb=128:cr=128:a=%s[disc]", quote(discLum), quote(discAlpha)),
			fmt.Sprintf("[1:v]format=yuva420p,geq=lum=230:cb=128:cr=128:a=%s[wave]", quote(waveAlpha)),
		}, ";"),
		"-map", "[disc]", "-map", "[wave]", "-frames:v", strconv.Itoa(2 * fps),
		"-c:v", "ffv1", "-f", "nut", out,
	}
}

// buildVideoArgs returns the ffmpeg arguments that draw segment n of epoch e
// once at the top rendition's size (see "Content generation" in the spec),
// scale it to every rendition and write <dir>/<rendition id>.mp4 each. The
// clock shows local time in loc; overlays is the output of buildOverlayArgs.
func buildVideoArgs(e stream.Epoch, n int64, loc *time.Location, inAd bool, font, overlays, dir string) []string {
	spec := e.Spec
	tl := e.Timeline()
	fps := spec.FPS
	t := top(spec)
	w, h := t.Width, t.Height
	disc, strip, inset := geometry(h)

	// Frame f counts from AST; drawtext and enable see it as n.
	ff := fmt.Sprintf("(n+%d)", tl.FirstFrame(n))
	odd := func(f string) string { return fmt.Sprintf("mod(floor(%s/%d),2)", f, fps) }
	beep := func(f string) string { return fmt.Sprintf("eq(mod(%s,%d),0)", f, fps) }

	bg := "y=40:u=128:v=128"
	if inAd {
		bg = "y=63:u=102:v=240" // BT.709 red
	}
	// The overlay cycle, looped, from the frame this segment starts on.
	replay := fmt.Sprintf("trim=start_frame=%d,setpts=N/(%d*TB)", tl.FirstFrame(n)%int64(2*fps), fps)

	text := func(opts string) string { return "drawtext=fontfile=" + quote(font) + ":" + opts }
	chain := make([]string, 0, 1+len(barColours)+4)
	chain = append(chain, fmt.Sprintf("drawbox=x=0:y=0:w=%d:h=%d:t=2:c=0xD0D0D0", w, h))
	for i, c := range barColours {
		chain = append(chain, fmt.Sprintf("drawbox=x=%d:y=%d:w=%d:h=%d:t=fill:c=%s", inset+i*strip, h-inset-strip, strip, strip, c))
	}
	chain = append(chain,
		text(fmt.Sprintf("text=%s:x=%d:y=%d:fontsize=%d:fontcolor=white",
			quote(clockText(tl.SegStart(n).In(loc), fps, ff)), h/24, h/24, h/12)),
		text(fmt.Sprintf("text=%s:x=(w-tw)/2:y=%d:fontsize=%d:fontcolor=black:borderw=%d:bordercolor=white",
			quote("%{eif:"+ff+":d:10}"), h/2+h/20, h/20, max(h/360, 1))),
		text(fmt.Sprintf("text=Bip!:enable=%s:x=(w-tw)/2:y=%d-th:fontsize=%d:fontcolor=0x404040",
			quote(beep(ff)+"*eq("+odd(ff)+",0)"), h/2-h/10, h/14)),
		text(fmt.Sprintf("text=Bop!:enable=%s:x=(w-tw)/2:y=%d-th:fontsize=%d:fontcolor=white",
			quote(beep(ff)+"*eq("+odd(ff)+",1)"), h/2-h/10, h/14)),
	)

	split := fmt.Sprintf("split=%d", len(spec.Video))
	branches := make([]string, 0, len(spec.Video))
	for i, v := range spec.Video {
		split += fmt.Sprintf("[s%d]", i)
		var ops []string
		if v.Width != w || v.Height != h {
			ops = append(ops, fmt.Sprintf("scale=%d:%d", v.Width, v.Height))
		}
		if spec.RenditionLabel {
			label := fmt.Sprintf("%s · %s · %d×%d · %d kbit/s", v.ID, codecNames[v.Codec], v.Width, v.Height, v.BitrateKbps)
			ops = append(ops, text(fmt.Sprintf("text=%s:x=w-tw-%[2]d:y=h-th-%[2]d:fontsize=%d:fontcolor=white:box=1:boxcolor=black@0.5:boxborderw=%d",
				quote(label), v.Height/24, v.Height/18, max(v.Height/120, 1))))
		}
		if len(ops) == 0 {
			ops = []string{"null"}
		}
		branches = append(branches, fmt.Sprintf("[s%d]%s[o%d]", i, strings.Join(ops, ","), i))
	}

	graph := strings.Join(append([]string{
		"[0:v]format=yuv420p,lutyuv=" + bg + "[bg]",
		"[1:v:0]" + replay + "[disc]",
		"[1:v:1]" + replay + "[wave]",
		fmt.Sprintf("[bg][disc]overlay=x=%d:y=%d[d]", (w-disc)/2, (h-disc)/2),
		fmt.Sprintf("[d][wave]overlay=x=0:y=%d,%s,%s", h-inset-strip-strip, strings.Join(chain, ","), split),
	}, branches...), ";")

	args := make([]string, 0, 16+35*len(spec.Video))
	args = append(args,
		"-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=black:s=%dx%d:r=%d", w, h, fps),
		"-stream_loop", "-1", "-i", overlays,
		"-filter_complex", graph,
	)
	gop := strconv.FormatInt(int64(spec.GOPDuration)*int64(fps)/int64(time.Second), 10)
	frames := strconv.FormatInt(tl.SegFrames, 10)
	for i, v := range spec.Video {
		args = append(args, "-map", fmt.Sprintf("[o%d]", i))
		args = append(args, encoderArgs(v, gop)...)
		args = append(args,
			"-frames:v", frames,
			"-f", "mp4", "-movflags", "empty_moov+default_base_moof+frag_keyframe",
			filepath.Join(dir, v.ID+".mp4"),
		)
	}
	return args
}

// videoEncoders is the ffmpeg encoder of each video codec.
var videoEncoders = map[stream.VideoCodec]string{
	stream.CodecH264: "libx264", stream.CodecHEVC: "libx265", stream.CodecAV1: "libsvtav1",
}

// codecNames label the renditions.
var codecNames = map[stream.VideoCodec]string{stream.CodecH264: "H.264", stream.CodecHEVC: "HEVC", stream.CodecAV1: "AV1"}

// encoderArgs returns the encoder options of rendition v: v's bitrate, a key
// frame every gop frames starting a closed GOP, no B-frames, one thread.
// Single-threaded encoders keep rate control deterministic, and the filters,
// not the encoders, bound the wall time.
func encoderArgs(v stream.Video, gop string) []string {
	rate, buf := strconv.Itoa(v.BitrateKbps)+"k", strconv.Itoa(2*v.BitrateKbps)+"k"
	switch v.Codec {
	case stream.CodecHEVC:
		args := []string{
			"-c:v", "libx265", "-tag:v", "hvc1", "-profile:v", v.Profile, "-preset", "ultrafast",
			"-b:v", rate, "-maxrate", rate, "-bufsize", buf,
			// info=0 drops the SEI that repeats the encoder settings.
			"-x265-params", "bframes=0:keyint=" + gop + ":min-keyint=" + gop +
				":scenecut=0:open-gop=0:repeat-headers=0:info=0:pools=1:frame-threads=1:log-level=error",
		}
		if v.Profile == "main10" {
			args = append(args, "-pix_fmt", "yuv420p10le")
		}
		return args
	case stream.CodecAV1:
		// rtc=1 is SVT-AV1's low-delay mode, the only one with CBR (rc=2): no
		// hidden frames, no maxrate.
		args := []string{
			"-c:v", "libsvtav1", "-preset", "10", "-b:v", rate, "-bufsize", buf, "-g", gop,
			"-svtav1-params", "keyint=" + gop + ":scd=0:rtc=1:rc=2:lp=1",
		}
		if v.BitDepth == 10 {
			args = append(args, "-pix_fmt", "yuv420p10le")
		}
		return args
	default:
		return []string{
			"-c:v", "libx264", "-threads", "1", "-profile:v", v.Profile, "-preset", "veryfast",
			"-b:v", rate, "-maxrate", rate, "-bufsize", buf,
			"-g", gop, "-keyint_min", gop, "-sc_threshold", "0", "-bf", "0", "-x264-params", "open-gop=0",
		}
	}
}

// clockText is the drawtext text for HH:MM:SS.FF at local time start + f/fps,
// where ff is frame f of the run's first frame. The clock is drawtext's
// gmtime of the frame's pts plus the segment's local start as an offset.
//
// ponytail: the zone offset is taken at the segment start, so a DST change
// shows from the next segment on.
func clockText(start time.Time, fps int, ff string) string {
	_, off := start.Zone()
	// Half a frame keeps float rounding of pts from flooring a whole second
	// down to the one before.
	base := start.Add(time.Duration(off)*time.Second + time.Second/time.Duration(2*fps))
	return fmt.Sprintf("%%{pts:gmtime:%d.%06d:'%%H:%%M:%%S'}.%%{eif:mod(%s,%d):d:2}",
		base.Unix(), base.Nanosecond()/1000, ff, fps)
}

// beepHz returns the tic and toc frequencies the waveform strip shows: those
// of the first audio rendition.
func beepHz(spec stream.Spec) (tic, toc int) {
	if len(spec.Audio) == 0 {
		return 480, 1500
	}
	return spec.Audio[0].TicHz, spec.Audio[0].TocHz
}

func even(v int) int {
	return max(v&^1, 2)
}

// graphEscaper escapes the characters the filtergraph parser treats as
// special, so a quoted option value reaches the filter's option parser intact.
var graphEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`, `[`, `\[`, `]`, `\]`, `,`, `\,`, `;`, `\;`)

// quote makes v a literal filter option value inside -filter_complex or a
// lavfi input: single-quoted for the option parser, then escaped for the graph.
func quote(v string) string {
	return graphEscaper.Replace("'" + strings.ReplaceAll(v, "'", `'\''`) + "'")
}
