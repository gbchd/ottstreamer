package liveffmpeg

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ottstreamer/internal/stream"
)

// barColours are the 75% SMPTE bars: grey, yellow, cyan, green, magenta, red, blue.
var barColours = []string{"0xBFBFBF", "0xBFBF00", "0x00BFBF", "0x00BF00", "0xBF00BF", "0xBF0000", "0x0000BF"}

// buildVideoArgs returns the ffmpeg arguments that draw segment n of epoch e
// once at the top rendition's size (see "Content generation" in the spec),
// scale it to every rendition and write <dir>/<rendition id>.mp4 each. The
// clock shows local time in loc.
func buildVideoArgs(e stream.Epoch, n int64, loc *time.Location, inAd bool, font, dir string) []string {
	spec := e.Spec
	tl := e.Timeline()
	fps := spec.FPS
	top := spec.Video[0]
	w, h := top.Width, top.Height
	disc := even((h*37 + 50) / 100)
	strip := even(max(h/34, 6))
	inset := max(h/68, 2)

	// Frame f counts from AST; drawtext and enable see it as n, geq as N.
	ff := fmt.Sprintf("(n+%d)", tl.FirstFrame(n))
	geqFF := fmt.Sprintf("(N+%d)", tl.FirstFrame(n))
	odd := func(f string) string { return fmt.Sprintf("mod(floor(%s/%d),2)", f, fps) }
	beep := func(f string) string { return fmt.Sprintf("eq(mod(%s,%d),0)", f, fps) }

	bg := "y=40:u=128:v=128"
	if inAd {
		bg = "y=63:u=102:v=240" // BT.709 red
	}

	// Sweep disc: a wedge from 12 o'clock completes a turn per second; the
	// two greys swap every second.
	r := disc / 2
	discLum := fmt.Sprintf("if(lt(mod(atan2(X-%[1]d,%[1]d-Y)+2*PI,2*PI),2*PI*mod(%[2]s,%[3]d)/%[3]d),if(%[4]s,240,102),if(%[4]s,102,240))",
		r, geqFF, fps, odd(geqFF))
	discAlpha := fmt.Sprintf("if(lt(hypot(X-%[1]d+0.5,Y-%[1]d+0.5),%[1]d),255,0)", r)

	// Waveform strip: the beep as it sounds during frame 0 of a second (Hz/fps
	// cycles across the width), a flat line otherwise.
	tic, toc := beepHz(spec)
	hz := fmt.Sprintf("if(%s,%d,%d)", odd(geqFF), tic, toc)
	waveAlpha := fmt.Sprintf("if(lt(abs(Y-%d-%s*%d*sin(2*PI*%s*X/(W*%d))),1.5),255,0)",
		strip/2, beep(geqFF), strip/2-2, hz, fps)

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
			label := fmt.Sprintf("%s · %d×%d · %d kbit/s", v.ID, v.Width, v.Height, v.BitrateKbps)
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
		fmt.Sprintf("[1:v]format=yuva420p,geq=lum=%s:cb=128:cr=128:a=%s[disc]", quote(discLum), quote(discAlpha)),
		fmt.Sprintf("[2:v]format=yuva420p,geq=lum=230:cb=128:cr=128:a=%s[wave]", quote(waveAlpha)),
		fmt.Sprintf("[bg][disc]overlay=x=%d:y=%d[d]", (w-disc)/2, (h-disc)/2),
		fmt.Sprintf("[d][wave]overlay=x=0:y=%d,%s,%s", h-inset-strip-strip, strings.Join(chain, ","), split),
	}, branches...), ";")

	color := func(cw, ch int) string { return fmt.Sprintf("color=c=black:s=%dx%d:r=%d", cw, ch, fps) }
	args := make([]string, 0, 18+35*len(spec.Video))
	args = append(args,
		"-hide_banner", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", color(w, h),
		"-f", "lavfi", "-i", color(disc, disc),
		"-f", "lavfi", "-i", color(w, strip),
		"-filter_complex", graph,
	)
	gop := strconv.FormatInt(int64(spec.GOPDuration)*int64(fps)/int64(time.Second), 10)
	frames := strconv.FormatInt(tl.SegFrames, 10)
	kbps := func(k int) string { return strconv.Itoa(k) + "k" }
	for i, v := range spec.Video {
		args = append(args,
			"-map", fmt.Sprintf("[o%d]", i),
			// One x264 thread per rendition: threaded VBV rate control is not
			// deterministic, and the filters, not x264, bound the wall time.
			"-c:v", "libx264", "-threads", "1", "-profile:v", v.Profile, "-preset", "veryfast",
			"-b:v", kbps(v.BitrateKbps), "-maxrate", kbps(v.BitrateKbps), "-bufsize", kbps(2*v.BitrateKbps),
			"-g", gop, "-keyint_min", gop, "-sc_threshold", "0", "-bf", "0", "-x264-params", "open-gop=0",
			"-frames:v", frames,
			"-f", "mp4", "-movflags", "empty_moov+default_base_moof+frag_keyframe",
			filepath.Join(dir, v.ID+".mp4"),
		)
	}
	return args
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
