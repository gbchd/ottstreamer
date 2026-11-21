package livemp4

import (
	"bytes"
	"encoding/xml"
	"slices"
	"strings"
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"

	"ottstreamer/internal/live"
)

func TestWriter_TextInit(t *testing.T) {
	tests := []struct {
		codec, lang, entry, handler string
	}{
		{live.CodecWebVTT, "en", "wvtt", "text"},
		{live.CodecIMSC1, "fra", "stpp", "subt"},
	}
	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			b, err := NewWriter().TextInit(tt.codec, tt.lang)
			if err != nil {
				t.Fatal(err)
			}
			f, err := mp4.DecodeFile(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			if f.Init == nil || len(f.Init.Moov.Traks) != 1 {
				t.Fatal("no init segment with one track")
			}
			if f.Init.Ftyp.MajorBrand() != "cmfc" {
				t.Errorf("ftyp = %s", f.Init.Ftyp.MajorBrand())
			}
			mdia := f.Init.Moov.Trak.Mdia
			lang := mdia.Mdhd.GetLanguage()
			if mdia.Elng != nil {
				lang = mdia.Elng.Language
			}
			if mdia.Mdhd.Timescale != 90000 || mdia.Hdlr.HandlerType != tt.handler || lang != tt.lang {
				t.Errorf("mdhd timescale %d, hdlr %s, lang %s; want 90000, %s, %s", mdia.Mdhd.Timescale, mdia.Hdlr.HandlerType, lang, tt.handler, tt.lang)
			}
			if got := mdia.Minf.Stbl.Stsd.Children[0].Type(); got != tt.entry {
				t.Errorf("sample entry = %s, want %s", got, tt.entry)
			}
		})
	}
	if _, err := NewWriter().TextInit("srt", "en"); err == nil {
		t.Error("TextInit(srt) succeeded")
	}
}

var cues = []live.Cue{
	{Start: 172800, End: 180000, Text: "EN · SUB · 18:00:01 · frame 25 · Bop!"},
	{Start: 180000, End: 270000, Text: "EN · SUB · 18:00:02 · frame 50 · Bip!"},
	{Start: 270000, End: 345600, Text: "[AD 1/5] EN · SUB · 18:00:03 · frame 75 · Bop!"},
}

// writeCues writes cues as segment 2 of codec and returns its samples.
func writeCues(t *testing.T, codec string) []mp4.FullSample {
	t.Helper()
	var buf bytes.Buffer
	f := live.Fragment{Seq: 2, TrackID: 1, BaseTime: 172800, Codec: codec, Lang: "en", Cues: cues}
	if err := NewWriter().WriteSegment(&buf, f); err != nil {
		t.Fatal(err)
	}
	file, err := mp4.DecodeFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Segments) != 1 || len(file.Segments[0].Fragments) != 1 {
		t.Fatal("want one segment with one fragment")
	}
	frag := file.Segments[0].Fragments[0]
	if frag.Moof.Mfhd.SequenceNumber != 2 || frag.Moof.Traf.Tfdt.BaseMediaDecodeTime() != 172800 {
		t.Errorf("seq %d tfdt %d, want 2 and 172800", frag.Moof.Mfhd.SequenceNumber, frag.Moof.Traf.Tfdt.BaseMediaDecodeTime())
	}
	full, err := frag.GetFullSamples(nil)
	if err != nil {
		t.Fatal(err)
	}
	return full
}

func TestWriter_WriteSegment_wvtt(t *testing.T) {
	full := writeCues(t, live.CodecWebVTT)
	if len(full) != len(cues) {
		t.Fatalf("%d samples, want %d", len(full), len(cues))
	}
	for i, s := range full {
		c := cues[i]
		box, err := mp4.DecodeBox(0, bytes.NewReader(s.Data))
		if err != nil {
			t.Fatal(err)
		}
		vttc, ok := box.(*mp4.VttcBox)
		if !ok || vttc.Payl == nil || vttc.Payl.CueText != c.Text {
			t.Errorf("sample %d = %T %+v, want vttc with %q", i, box, box, c.Text)
		}
		if s.DecodeTime != uint64(c.Start) || int64(s.Dur) != c.End-c.Start || !s.IsSync() {
			t.Errorf("sample %d at %d for %d (sync %t), want %d for %d", i, s.DecodeTime, s.Dur, s.IsSync(), c.Start, c.End-c.Start)
		}
	}
}

func TestWriter_WriteSegment_stpp(t *testing.T) {
	full := writeCues(t, live.CodecIMSC1)
	if len(full) != 1 || full[0].Dur != 345600-172800 {
		t.Fatalf("%d samples, want 1 lasting the segment", len(full))
	}
	var doc struct {
		Lang string `xml:"lang,attr"`
		Ps   []struct {
			Begin string `xml:"begin,attr"`
			End   string `xml:"end,attr"`
			Span  string `xml:"span"`
		} `xml:"body>div>p"`
	}
	if err := xml.Unmarshal(full[0].Data, &doc); err != nil {
		t.Fatalf("TTML does not parse: %v\n%s", err, full[0].Data)
	}
	got := make([]string, 0, len(doc.Ps))
	for _, p := range doc.Ps {
		got = append(got, p.Begin+" "+p.End+" "+p.Span)
	}
	want := []string{
		"00:00:01.920 00:00:02.000 EN · SUB · 18:00:01 · frame 25 · Bop!",
		"00:00:02.000 00:00:03.000 EN · SUB · 18:00:02 · frame 50 · Bip!",
		"00:00:03.000 00:00:03.840 [AD 1/5] EN · SUB · 18:00:03 · frame 75 · Bop!",
	}
	if doc.Lang != "en" || !slices.Equal(got, want) {
		t.Errorf("lang %q, cues\n%s\nwant\n%s", doc.Lang, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
