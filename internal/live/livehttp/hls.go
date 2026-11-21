package livehttp

import (
	"cmp"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/live"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/timeline"
)

// hlsStyles returns ?cues= or the epoch's ads.hls.
func hlsStyles(r *http.Request, def []string) ([]string, error) {
	styles, err := override(r, "cues", def, "daterange", "cueout", "oatcls", "adobe")
	if err == nil && slices.Contains(styles, "cueout") && slices.Contains(styles, "oatcls") {
		err = fmt.Errorf("%w: cues cueout and oatcls are exclusive", errBadRequest)
	}
	return styles, err
}

// override returns the comma-separated values of query parameter key, each
// one of allowed, or def when the parameter is absent.
func override(r *http.Request, key string, def []string, allowed ...string) ([]string, error) {
	q := r.URL.Query()
	if !q.Has(key) {
		return def, nil
	}
	var out []string
	for v := range strings.SplitSeq(q.Get(key), ",") {
		if v == "" {
			continue
		}
		if !slices.Contains(allowed, v) {
			return nil, fmt.Errorf("%w: %s must be among %s", errBadRequest, key, strings.Join(allowed, ","))
		}
		out = append(out, v)
	}
	return out, nil
}

func (h *Handler) mediaPlaylist(r *http.Request, req request) ([]byte, error) {
	rendition := strings.TrimSuffix(r.PathValue("file"), ".m3u8")
	t, ok := req.rt.Track(rendition)
	if !ok {
		return nil, fmt.Errorf("%w: rendition %s", live.ErrNotFound, rendition)
	}
	styles, err := hlsStyles(r, req.spec.Ads.HLS)
	if err != nil {
		return nil, err
	}
	dvr, err := req.dvr(r)
	if err != nil {
		return nil, err
	}
	first, last := req.segments(dvr)
	breaks, err := h.breaksIn(r.Context(), req, first, last+1)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	tl := req.tl
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n",
		(tl.SegFrames+int64(tl.FPS)-1)/int64(tl.FPS), first)
	ext := "m4s"
	if t.Codec == live.CodecWebVTT {
		ext = "vtt" // plain WebVTT, no init segment
	} else {
		fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", req.url(rendition+"/init.mp4"))
	}
	cw := cueWriter{b: &b, styles: styles, tl: tl, command: req.spec.Ads.Command}
	for n := first; n <= last; n++ {
		// Sorted by start, so a break ending at n closes before the next opens.
		for _, br := range breaks {
			switch {
			case br.EndSeg == n:
				cw.in(br)
			case br.StartSeg == n || (n == first && br.StartSeg < n && n < br.EndSeg):
				cw.out(br) // the opening tags stay while the break is in the window
			case br.StartSeg < n && n < br.EndSeg:
				cw.cont(br, n)
			}
		}
		start, dur := segTiming(tl, t, n)
		fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:%s,\n", pdt(start), strconv.FormatFloat(dur.Seconds(), 'f', 3, 64))
		if fault.Gap(req.rules, fault.Target{StreamID: req.id, Epoch: req.epoch, Kind: fault.KindSegment, Rendition: rendition, Segment: n}) {
			b.WriteString("#EXT-X-GAP\n")
		}
		b.WriteString(req.url(fmt.Sprintf("%s/%d.%s", rendition, n, ext)) + "\n")
	}
	if tl.EndSeg >= 0 && last == tl.EndSeg {
		// A stop cuts an active break at the end segment: its cue-in segment
		// is never produced, so it closes here.
		for _, br := range breaks {
			if br.EndSeg > last {
				cw.in(br)
			}
		}
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return []byte(b.String()), nil
}

// webVTT returns a WebVTT segment of cues starting at media time start.
// Cue times are media time; X-TIMESTAMP-MAP ties them to the video's 90 kHz
// media time, which the fMP4 tfdt carries. Cue text comes from validated
// language tags and digits, so it needs no escaping.
func webVTT(start int64, cues []live.Cue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:%s\n", start%(1<<33), live.Timestamp(start))
	for _, c := range cues {
		fmt.Fprintf(&b, "\n%s --> %s\n%s\n", live.Timestamp(c.Start), live.Timestamp(c.End), c.Text)
	}
	return b.String()
}

// segTiming returns the real start and duration of segment n of track t.
func segTiming(tl timeline.Timeline, t live.Track, n int64) (time.Time, time.Duration) {
	if t.Kind != live.KindAudio {
		return tl.SegStart(n), tl.SegDuration()
	}
	first, end := tl.AudioFrameRange(n, t.FrameSize)
	a := int64(t.FrameSize)
	return tl.AST.Add(samples(first * a)), samples((end - first) * a)
}

// samples converts 48 kHz samples to a duration without overflowing.
func samples(s int64) time.Duration {
	const rate = timeline.AudioTimescale
	return time.Duration(s/rate)*time.Second + time.Duration(s%rate)*time.Second/rate
}

func pdt(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// secs formats d in seconds, without trailing zeros: 30, 30.72.
func secs(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// cueWriter writes the HLS ad tags of every style, identical in every media
// playlist of the stream.
type cueWriter struct {
	b       *strings.Builder
	styles  []string
	tl      timeline.Timeline
	command stream.Command
}

// planned is the duration the cue-out announces.
func (c cueWriter) planned(br adbreak.Break) time.Duration {
	return time.Duration(br.PlannedEndSeg-br.StartSeg) * c.tl.SegDuration()
}

// scte35Attr is the DATERANGE attribute carrying the cue: SCTE35-CMD for
// time_signal, else SCTE35-OUT or SCTE35-IN.
func (c cueWriter) scte35Attr(dir string) string {
	if c.command == stream.CommandTimeSignal {
		return "SCTE35-CMD"
	}
	return "SCTE35-" + dir
}

func (c cueWriter) out(br adbreak.Break) {
	d, b64 := secs(c.planned(br)), base64.StdEncoding.EncodeToString(br.CueOut)
	for _, s := range c.styles {
		switch s {
		case "daterange":
			fmt.Fprintf(c.b, "#EXT-X-DATERANGE:ID=\"%s\",START-DATE=\"%s\",PLANNED-DURATION=%.3f,%s=%s\n",
				br.ID, pdt(br.Start), c.planned(br).Seconds(), c.scte35Attr("OUT"), hexCue(br.CueOut))
		case "cueout":
			fmt.Fprintf(c.b, "#EXT-X-CUE-OUT:DURATION=%s\n", d)
		case "oatcls":
			// ponytail: no EXT-X-ASSET, since no cue carries a UPID yet.
			fmt.Fprintf(c.b, "#EXT-OATCLS-SCTE35:%s\n#EXT-X-CUE-OUT:%s\n", b64, d)
		case "adobe":
			fmt.Fprintf(c.b, "#EXT-X-SCTE35:CUE=\"%s\",ID=\"%s\",DURATION=%s,TIME=%s\n", b64, br.ID, d, unixTime(br.Start))
		}
	}
}

func (c cueWriter) cont(br adbreak.Break, n int64) {
	elapsed := secs(time.Duration(n-br.StartSeg) * c.tl.SegDuration())
	for _, s := range c.styles {
		switch s {
		case "cueout":
			fmt.Fprintf(c.b, "#EXT-X-CUE-OUT-CONT:ElapsedTime=%s,Duration=%s\n", elapsed, secs(c.planned(br)))
		case "oatcls":
			fmt.Fprintf(c.b, "#EXT-X-CUE-OUT-CONT:ElapsedTime=%s,Duration=%s,SCTE35=%s\n",
				elapsed, secs(c.planned(br)), base64.StdEncoding.EncodeToString(br.CueOut))
		}
	}
}

func (c cueWriter) in(br adbreak.Break) {
	for _, s := range c.styles {
		switch s {
		case "daterange":
			fmt.Fprintf(c.b, "#EXT-X-DATERANGE:ID=\"%s\",START-DATE=\"%s\",END-DATE=\"%s\",%s=%s\n",
				br.ID, pdt(br.Start), pdt(br.End), c.scte35Attr("IN"), hexCue(br.CueIn))
		case "cueout", "oatcls":
			c.b.WriteString("#EXT-X-CUE-IN\n")
		case "adobe":
			fmt.Fprintf(c.b, "#EXT-X-SCTE35:CUE=\"%s\",ID=\"%s\"\n", base64.StdEncoding.EncodeToString(br.CueIn), br.ID)
		}
	}
}

func hexCue(b []byte) string {
	return "0x" + strings.ToUpper(hex.EncodeToString(b))
}

// unixTime is t in Unix seconds, with at least one decimal: 1794076200.0.
func unixTime(t time.Time) string {
	s := strconv.FormatFloat(float64(t.UnixMilli())/1000, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// variant is one EXT-X-STREAM-INF: a video rendition with an audio group.
type variant struct {
	video     stream.Video
	group     string
	bandwidth int // bit/s, before the 10 % margin
	codecs    string
}

func (*Handler) multivariant(_ *http.Request, req request) ([]byte, error) {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	var groups []string
	maxAudio := map[string]int{}
	codecs := map[string][]string{}
	for _, a := range req.spec.Audio {
		if !slices.Contains(groups, a.Group) {
			groups = append(groups, a.Group)
		}
		maxAudio[a.Group] = max(maxAudio[a.Group], a.BitrateKbps*1000)
		if t, ok := req.rt.Track(a.ID); ok && !slices.Contains(codecs[a.Group], t.Codec) {
			codecs[a.Group] = append(codecs[a.Group], t.Codec)
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"%s\",LANGUAGE=\"%s\",NAME=\"%s\",CHANNELS=\"%d\",DEFAULT=%s,AUTOSELECT=YES,URI=\"%s\"\n",
			a.Group, a.Lang, cmp.Or(a.Name, a.ID), a.Channels, yesNo(a.Default), req.url(a.ID+".m3u8"))
	}
	// Every variant offers every subtitle track; IMSC1 tracks put their
	// codec in each variant's CODECS, as the HLS authoring spec asks.
	var subs, textCodec string
	for _, s := range req.spec.Subtitles {
		subs = `,SUBTITLES="subs"`
		if s.Format == stream.SubtitleIMSC1 {
			textCodec = live.CodecIMSC1
		}
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",LANGUAGE=\"%s\",NAME=\"%s\",DEFAULT=%s,AUTOSELECT=YES,FORCED=NO,URI=\"%s\"\n",
			s.Lang, cmp.Or(s.Name, s.ID), yesNo(s.Default), req.url(s.ID+".m3u8"))
	}
	// CEA-608 captions ride in every variant's video.
	var cc string
	if c := req.spec.CEA608; c != nil {
		cc = `,CLOSED-CAPTIONS="cc"`
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID=\"cc\",LANGUAGE=\"%s\",NAME=\"%s\",DEFAULT=NO,AUTOSELECT=YES,INSTREAM-ID=\"%s\"\n",
			c.Lang, c.Channel, c.Channel)
	}
	variants := make([]variant, 0, len(req.spec.Video)*len(groups))
	for _, v := range req.spec.Video {
		t, _ := req.rt.Track(v.ID)
		for _, g := range groups {
			all := append([]string{t.Codec}, codecs[g]...)
			if textCodec != "" {
				all = append(all, textCodec)
			}
			variants = append(variants, variant{
				video:     v,
				group:     g,
				bandwidth: v.BitrateKbps*1000 + maxAudio[g],
				codecs:    strings.Join(all, ","),
			})
		}
	}
	// The ladder is stored by codec preference (AV1, HEVC, H.264): keep that,
	// highest bandwidth first within each codec.
	rank := func(v variant) int {
		return slices.IndexFunc(req.spec.Video, func(o stream.Video) bool { return o.Codec == v.video.Codec })
	}
	slices.SortStableFunc(variants, func(x, y variant) int {
		return cmp.Or(cmp.Compare(rank(x), rank(y)), cmp.Compare(y.bandwidth, x.bandwidth))
	})
	for _, v := range variants {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,CODECS=\"%s\",RESOLUTION=%dx%d,FRAME-RATE=%.3f,AUDIO=\"%s\"%s%s\n%s\n",
			v.bandwidth*11/10, v.bandwidth, v.codecs, v.video.Width, v.video.Height, float64(req.spec.FPS), v.group, subs, cc, req.url(v.video.ID+".m3u8"))
	}
	return []byte(b.String()), nil
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}
