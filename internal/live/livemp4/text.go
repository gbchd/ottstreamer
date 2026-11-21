package livemp4

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"

	"github.com/Eyevinn/mp4ff/mp4"

	"ottstreamer/internal/live"
	"ottstreamer/internal/timeline"
)

// TextInit returns the CMAF header of a subtitle track at 90 kHz: a wvtt
// sample entry (WebVTT in ISOBMFF, ISO/IEC 14496-30) or an stpp one holding
// TTML, for IMSC1.
func (*Writer) TextInit(codec, lang string) ([]byte, error) {
	init := mp4.CreateEmptyInit()
	init.SetFtyp(mp4.NewFtyp("cmfc", 0, []string{"cmfc", "iso6", "dash"}))
	var err error
	switch codec {
	case live.CodecWebVTT:
		err = init.AddEmptyTrack(timeline.VideoTimescale, "wvtt", lang).SetWvttDescriptor("WEBVTT")
	case live.CodecIMSC1:
		err = init.AddEmptyTrack(timeline.VideoTimescale, "subtitle", lang).SetStppDescriptor("http://www.w3.org/ns/ttml", "", "")
	default:
		err = fmt.Errorf("unknown text codec %q", codec)
	}
	if err != nil {
		return nil, fmt.Errorf("text init: %w", err)
	}
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode text init: %w", err)
	}
	return buf.Bytes(), nil
}

// writeText writes the cues of f: one wvtt sample per cue, or a single stpp
// sample with a TTML document of every cue. The cues cover the segment back
// to back, so no empty-cue (vtte) samples are needed.
func writeText(w io.Writer, f live.Fragment) error {
	if len(f.Cues) == 0 {
		return fmt.Errorf("segment %d: no cues", f.Seq)
	}
	frag, err := mp4.CreateFragment(f.Seq, f.TrackID)
	if err != nil {
		return fmt.Errorf("create fragment: %w", err)
	}
	frag.EncOptimize = mp4.OptimizeTrun
	frag.Moof.Traf.Tfdt.SetBaseMediaDecodeTime(f.BaseTime)
	frag.Moof.Traf.Tfdt.Version = 1
	add := func(data []byte, dur int64) {
		frag.Mdat.AddSampleDataPart(data)
		frag.Moof.Traf.Trun.AddSample(mp4.NewSample(mp4.SyncSampleFlags, uint32(dur), uint32(len(data)), 0)) //nolint:gosec // a segment lasts at most 10 s; cue text is short
	}
	switch f.Codec {
	case live.CodecWebVTT:
		for _, c := range f.Cues {
			vttc := &mp4.VttcBox{}
			vttc.AddChild(&mp4.PaylBox{CueText: c.Text})
			var buf bytes.Buffer
			if err := vttc.Encode(&buf); err != nil {
				return fmt.Errorf("encode cue: %w", err)
			}
			add(buf.Bytes(), c.End-c.Start)
		}
	case live.CodecIMSC1:
		add(ttml(f.Lang, f.Cues), f.Cues[len(f.Cues)-1].End-f.Cues[0].Start)
	default:
		return fmt.Errorf("segment %d: unknown text codec %q", f.Seq, f.Codec)
	}
	return encode(w, frag)
}

// ttml returns an IMSC1 Text Profile document of cues, timed in media time
// as ISO/IEC 14496-30 has it, in a region along the bottom of the picture.
// lang is a validated BCP 47 tag, so it needs no escaping.
func ttml(lang string, cues []live.Cue) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<tt xmlns="http://www.w3.org/ns/ttml" xmlns:ttp="http://www.w3.org/ns/ttml#parameter" xmlns:tts="http://www.w3.org/ns/ttml#styling" ttp:profile="http://www.w3.org/ns/ttml/profile/imsc1/text" xml:lang="%s">
<head>
<styling><style xml:id="cue" tts:color="white" tts:backgroundColor="#000000C0"/></styling>
<layout><region xml:id="bottom" tts:origin="10%% 80%%" tts:extent="80%% 15%%" tts:displayAlign="after" tts:textAlign="center"/></layout>
</head>
<body region="bottom"><div>
`, lang)
	for _, c := range cues {
		fmt.Fprintf(&b, `<p begin="%s" end="%s"><span style="cue">`, live.Timestamp(c.Start), live.Timestamp(c.End))
		_ = xml.EscapeText(&b, []byte(c.Text)) // a bytes.Buffer never fails
		b.WriteString("</span></p>\n")
	}
	b.WriteString("</div></body>\n</tt>\n")
	return b.Bytes()
}
