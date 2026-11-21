package stream_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streamtest"
)

func TestSpec_Normalize(t *testing.T) {
	s := streamtest.Spec()
	s.Video = append(s.Video,
		stream.Video{ID: "1080p", Width: 1920, Height: 1080, BitrateKbps: 5000},
		stream.Video{ID: "hevc", Codec: stream.CodecHEVC, Width: 1920, Height: 1080, BitrateKbps: 3500},
	)
	s.Audio = append(s.Audio,
		stream.Audio{ID: "fr", Lang: "fr", Channels: 2, BitrateKbps: 128},
		stream.Audio{ID: "en-51", Lang: "en", Channels: 6, BitrateKbps: 384, Codec: stream.CodecAC3},
		stream.Audio{ID: "de", Lang: "de", Channels: 2, BitrateKbps: 128, TicHz: 700},
	)
	s.Normalize()

	if s.SegmentDuration != 2*time.Second || s.GOPDuration != 2*time.Second || s.FPS != 25 ||
		s.Timezone != "UTC" || s.DASHAddressing != stream.AddressingTimelineTime {
		t.Errorf("timing defaults = %v %v %d %q %q", s.SegmentDuration, s.GOPDuration, s.FPS, s.Timezone, s.DASHAddressing)
	}
	ids := make([]string, 0, len(s.Video))
	for _, v := range s.Video {
		ids = append(ids, v.ID)
	}
	if want := []string{"hevc", "1080p", "720p", "360p"}; !slices.Equal(ids, want) {
		t.Errorf("video order = %v, want %v", ids, want)
	}
	if v := s.Video[1]; v.Codec != stream.CodecH264 || v.Profile != "high" {
		t.Errorf("video defaults = %q %q", v.Codec, v.Profile)
	}
	tones := []struct {
		id       string
		toc, tic int
		codec    stream.AudioCodec
	}{
		{"en", 1500, 480, stream.CodecAAC},
		{"fr", 2000, 640, stream.CodecAAC},
		{"en-51", 1500, 480, stream.CodecAC3},
		{"de", 2500, 700, stream.CodecAAC},
	}
	for i, want := range tones {
		a := s.Audio[i]
		if a.ID != want.id || a.TocHz != want.toc || a.TicHz != want.tic || a.Codec != want.codec || a.Group != "main" {
			t.Errorf("audio[%d] = %+v, want %+v in group main", i, a, want)
		}
	}
	if s.Ads.Lead != 8*time.Second || s.Ads.Command != stream.CommandSpliceInsert ||
		!slices.Equal(s.Ads.HLS, []string{"daterange"}) || !slices.Equal(s.Ads.DASH, []string{"eventstream-bin"}) {
		t.Errorf("ads defaults = %+v", s.Ads)
	}
	if s.Window != (stream.Window{DVR: 30 * time.Second, Retain: 300 * time.Second}) {
		t.Errorf("window defaults = %+v", s.Window)
	}
}

func TestSpec_Validate(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*stream.Spec)
		fields []string // invalid fields expected, nil = valid
	}{
		{"default spec", func(*stream.Spec) {}, nil},
		{"1.92 s at 25 fps", func(s *stream.Spec) { s.SegmentDuration = 1920 * time.Millisecond }, nil},
		{"0.96 s GOP in 1.92 s", func(s *stream.Spec) {
			s.SegmentDuration, s.GOPDuration = 1920*time.Millisecond, 960*time.Millisecond
		}, nil},
		{"time_signal with type", func(s *stream.Spec) {
			s.Ads.Command, s.Ads.SegmentationType = stream.CommandTimeSignal, "provider_ad"
		}, nil},
		{"recurring", func(s *stream.Spec) {
			s.Ads.Recurring = &stream.Recurring{Every: 300 * time.Second, Duration: 30 * time.Second, Offset: 60 * time.Second}
		}, nil},
		{"two audio groups", func(s *stream.Spec) {
			s.Audio = append(s.Audio, stream.Audio{ID: "en-low", Lang: "en", Channels: 2, BitrateKbps: 64, Group: "low", Default: true})
		}, nil},

		{"no formats", func(s *stream.Spec) { s.Formats = nil }, []string{"spec.formats"}},
		{"unknown format", func(s *stream.Spec) { s.Formats = []stream.Format{"rtmp"} }, []string{"spec.formats[0]"}},
		{"duplicate format", func(s *stream.Spec) { s.Formats = []stream.Format{"hls", "hls"} }, []string{"spec.formats[1]"}},
		{"fps 29", func(s *stream.Spec) { s.FPS = 29 }, []string{"spec.fps"}},
		{"segment too short", func(s *stream.Spec) {
			s.SegmentDuration, s.GOPDuration = 400*time.Millisecond, 400*time.Millisecond
		}, []string{"spec.segmentSeconds"}},
		{"segment too long", func(s *stream.Spec) { s.SegmentDuration = 11 * time.Second }, []string{"spec.segmentSeconds"}},
		{"1.92 s at 30 fps", func(s *stream.Spec) {
			s.FPS, s.SegmentDuration = 30, 1920*time.Millisecond
		}, []string{"spec.segmentSeconds", "spec.gopSeconds"}},
		{"GOP does not divide", func(s *stream.Spec) { s.GOPDuration = 1200 * time.Millisecond }, []string{"spec.gopSeconds"}},
		{"GOP not whole frames", func(s *stream.Spec) { s.GOPDuration = 1010 * time.Millisecond }, []string{"spec.gopSeconds"}},
		{"bad timezone", func(s *stream.Spec) { s.Timezone = "Mars/Olympus" }, []string{"spec.timezone"}},
		{"no video", func(s *stream.Spec) { s.Video = nil }, []string{"spec.video"}},
		{"13 video renditions", func(s *stream.Spec) {
			s.Video = nil
			for i := range 13 {
				s.Video = append(s.Video, stream.Video{ID: string(rune('a' + i)), Codec: "h264", Profile: "high", Width: 640, Height: 360, BitrateKbps: 100 + i})
			}
		}, []string{"spec.video"}},
		{"hevc profile", func(s *stream.Spec) { s.Video[0].Codec, s.Video[0].Profile = stream.CodecHEVC, "high" }, []string{"spec.video[0].profile"}},
		{"av1 profile and depth", func(s *stream.Spec) {
			s.Video[0].Codec, s.Video[0].Profile, s.Video[0].BitDepth = stream.CodecAV1, "high", 12
		}, []string{"spec.video[0].profile", "spec.video[0].bitDepth"}},
		{"bit depth on h264", func(s *stream.Spec) { s.Video[0].BitDepth = 10 }, []string{"spec.video[0].bitDepth"}},
		{"vp9", func(s *stream.Spec) { s.Video[1].Codec = "vp9" }, []string{"spec.video[1].codec"}},
		{"bad profile", func(s *stream.Spec) { s.Video[0].Profile = "main10" }, []string{"spec.video[0].profile"}},
		{"odd width", func(s *stream.Spec) { s.Video[0].Width = 1279 }, []string{"spec.video[0].width"}},
		{"too tall", func(s *stream.Spec) { s.Video[0].Height = 1440 }, []string{"spec.video[0].height"}},
		{"bitrate too low", func(s *stream.Spec) { s.Video[1].BitrateKbps = 99 }, []string{"spec.video[1].bitrateKbps"}},
		{"bitrate too high", func(s *stream.Spec) { s.Video[0].BitrateKbps = 20001 }, []string{"spec.video[0].bitrateKbps"}},
		{"duplicate bitrate", func(s *stream.Spec) { s.Video[1].BitrateKbps = 3000 }, []string{"spec.video[1].bitrateKbps"}},
		{"bad rendition id", func(s *stream.Spec) { s.Video[0].ID = "720 p" }, []string{"spec.video[0].id"}},
		{"rendition id shared with audio", func(s *stream.Spec) { s.Audio[0].ID = "720p" }, []string{"spec.audio[0].id"}},
		{"no audio", func(s *stream.Spec) { s.Audio = nil }, []string{"spec.audio"}},
		{"bad lang", func(s *stream.Spec) { s.Audio[0].Lang = "english!" }, []string{"spec.audio[0].lang"}},
		{"4 channels", func(s *stream.Spec) { s.Audio[0].Channels = 4 }, []string{"spec.audio[0].channels"}},
		{"eac3 bitrate", func(s *stream.Spec) { s.Audio[0].Codec, s.Audio[0].BitrateKbps = stream.CodecEAC3, 64 }, []string{"spec.audio[0].bitrateKbps"}},
		{"opus bitrate", func(s *stream.Spec) { s.Audio[0].Codec, s.Audio[0].BitrateKbps = stream.CodecOpus, 400 }, []string{"spec.audio[0].bitrateKbps"}},
		{"mp3", func(s *stream.Spec) { s.Audio[0].Codec = "mp3" }, []string{"spec.audio[0].codec"}},
		{"aac bitrate", func(s *stream.Spec) { s.Audio[0].BitrateKbps = 384 }, []string{"spec.audio[0].bitrateKbps"}},
		{"ac3 bitrate", func(s *stream.Spec) { s.Audio[0].Codec = stream.CodecAC3 }, []string{"spec.audio[0].bitrateKbps"}},
		{"no default track", func(s *stream.Spec) { s.Audio[0].Default = false }, []string{"spec.audio"}},
		{"two default tracks", func(s *stream.Spec) {
			s.Audio = append(s.Audio, stream.Audio{ID: "fr", Lang: "fr", Channels: 2, BitrateKbps: 128, Group: "main", Default: true, TicHz: 640, TocHz: 2000})
		}, []string{"spec.audio"}},
		{"groups differ with HLS", func(s *stream.Spec) {
			s.Audio = append(s.Audio, stream.Audio{ID: "fr-low", Lang: "fr", Channels: 2, BitrateKbps: 64, Group: "low", Default: true, TicHz: 640, TocHz: 2000})
		}, []string{"spec.audio"}},
		{"bad tone", func(s *stream.Spec) { s.Audio[0].TicHz = 30000 }, []string{"spec.audio[0].ticHz"}},
		{"bad addressing", func(s *stream.Spec) { s.DASHAddressing = "time-duration" }, []string{"spec.dash.addressing"}},
		{"lead under 2 segments", func(s *stream.Spec) { s.Ads.Lead = 3 * time.Second }, []string{"spec.ads.leadSeconds"}},
		{"lead over 60 s", func(s *stream.Spec) { s.Ads.Lead = 61 * time.Second }, []string{"spec.ads.leadSeconds"}},
		{"bad command", func(s *stream.Spec) { s.Ads.Command = "splice_null" }, []string{"spec.ads.command"}},
		{"time_signal without type", func(s *stream.Spec) { s.Ads.Command = stream.CommandTimeSignal }, []string{"spec.ads.segmentationType"}},
		{"type without time_signal", func(s *stream.Spec) { s.Ads.SegmentationType = "break" }, []string{"spec.ads.segmentationType"}},
		{"unknown hls cue", func(s *stream.Spec) { s.Ads.HLS = []string{"scte35"} }, []string{"spec.ads.hls[0]"}},
		{"cueout with oatcls", func(s *stream.Spec) { s.Ads.HLS = []string{"cueout", "oatcls"} }, []string{"spec.ads.hls"}},
		{"unknown dash cue", func(s *stream.Spec) { s.Ads.DASH = []string{"inband", "emsg"} }, []string{"spec.ads.dash[1]"}},
		{"hls cue ignored without hls", func(s *stream.Spec) {
			s.Formats, s.Ads.HLS = []stream.Format{stream.FormatDASH}, []string{"nonsense"}
		}, nil},
		{"recurring too short", func(s *stream.Spec) {
			s.Ads.Recurring = &stream.Recurring{Every: 300 * time.Second, Duration: time.Second, Offset: 60 * time.Second}
		}, []string{"spec.ads.recurring.durationSeconds"}},
		{"recurring too often", func(s *stream.Spec) {
			s.Ads.Recurring = &stream.Recurring{Every: 33 * time.Second, Duration: 30 * time.Second, Offset: 60 * time.Second}
		}, []string{"spec.ads.recurring.everySeconds"}},
		{"recurring offset under lead", func(s *stream.Spec) {
			s.Ads.Recurring = &stream.Recurring{Every: 300 * time.Second, Duration: 30 * time.Second, Offset: 4 * time.Second}
		}, []string{"spec.ads.recurring.offsetSeconds"}},
		{"dvr under 3 segments", func(s *stream.Spec) { s.Window.DVR = 5 * time.Second }, []string{"spec.window.dvrSeconds"}},
		{"retain under dvr", func(s *stream.Spec) { s.Window.Retain = 20 * time.Second }, []string{"spec.window.retainSeconds"}},
		{"retain over 24 h", func(s *stream.Spec) { s.Window.Retain = 25 * time.Hour }, []string{"spec.window.retainSeconds"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := streamtest.Spec()
			tt.edit(&s)
			s.Normalize()
			err := s.Validate()
			if tt.fields == nil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			var verr *stream.ValidationError
			if !errors.As(err, &verr) || !errors.Is(err, stream.ErrInvalid) {
				t.Fatalf("Validate = %v, want a *ValidationError", err)
			}
			var got []string
			for _, f := range verr.Fields {
				got = append(got, f.Field)
			}
			if !slices.Equal(got, tt.fields) {
				t.Errorf("invalid fields = %v, want %v (%v)", got, tt.fields, err)
			}
		})
	}
}

func TestSpec_Normalize_CodecDefaults(t *testing.T) {
	s := streamtest.Spec()
	s.Video = append(s.Video,
		stream.Video{ID: "hevc", Codec: stream.CodecHEVC, Width: 1280, Height: 720, BitrateKbps: 2000},
		stream.Video{ID: "av1", Codec: stream.CodecAV1, Width: 1280, Height: 720, BitrateKbps: 1500})
	s.Audio = append(s.Audio,
		stream.Audio{ID: "ec3", Lang: "en", Channels: 6, BitrateKbps: 384, Codec: stream.CodecEAC3},
		stream.Audio{ID: "opus", Lang: "en", Channels: 2, BitrateKbps: 96, Codec: stream.CodecOpus})
	s.Normalize()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if av1, hevc := s.Video[0], s.Video[1]; av1.ID != "av1" || av1.Profile != "main" || av1.BitDepth != 8 || hevc.ID != "hevc" || hevc.Profile != "main" || hevc.BitDepth != 0 {
		t.Errorf("ladder = %+v, want av1 main 8-bit, then hevc main", s.Video[:2])
	}
}

func TestSpec_AudioAligned(t *testing.T) {
	tests := []struct {
		name string
		seg  time.Duration
		ac3  bool
		want bool
	}{
		{"2 s AAC", 2 * time.Second, false, false},
		{"1.92 s AAC", 1920 * time.Millisecond, false, true},
		{"1.92 s AAC and AC-3", 1920 * time.Millisecond, true, true},
		{"1.6 s AAC and AC-3", 1600 * time.Millisecond, true, true},
		{"0.64 s AC-3", 640 * time.Millisecond, true, true},
		{"8 s AAC", 8 * time.Second, false, true},
		{"6 s AAC", 6 * time.Second, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := streamtest.Spec()
			s.SegmentDuration = tt.seg
			if tt.ac3 {
				s.Audio = append(s.Audio, stream.Audio{ID: "en-51", Lang: "en", Channels: 6, BitrateKbps: 384, Codec: stream.CodecAC3})
			}
			s.Normalize()
			if got := s.AudioAligned(); got != tt.want {
				t.Errorf("AudioAligned = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEpoch_Timeline(t *testing.T) {
	s := streamtest.Spec()
	s.SegmentDuration = 1920 * time.Millisecond
	s.Normalize()
	ast := time.Date(2026, 11, 6, 18, 40, 0, 0, time.UTC)
	e := stream.Epoch{Number: 1, AST: ast, Spec: s}

	tl := e.Timeline()
	if tl.SegFrames != 48 || tl.FPS != 25 || tl.LeadSegs != 5 || tl.EndSeg != -1 || !tl.AST.Equal(ast) {
		t.Errorf("Timeline = %+v", tl)
	}
	end := int64(42)
	e.EndSegment = &end
	if got := e.Timeline().EndSeg; got != 42 {
		t.Errorf("EndSeg = %d, want 42", got)
	}
}
