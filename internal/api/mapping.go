package api

import (
	"math"
	"time"

	"ottstreamer/internal/stream"
)

// Mapping between the generated models and stream types. Durations travel
// as float seconds and are rounded to the nanosecond, so 1.92 stays exact.

func duration(seconds float64) time.Duration {
	return time.Duration(math.Round(seconds * float64(time.Second)))
}

func seconds(d time.Duration) float64 {
	return d.Seconds()
}

// convert maps a slice of one string type to another, keeping nil as nil.
func convert[U, T ~string](in []T) []U {
	if in == nil {
		return nil
	}
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = U(v)
	}
	return out
}

func toSpec(in Spec) stream.Spec {
	s := stream.Spec{
		Formats:         convert[stream.Format](in.Formats),
		SegmentDuration: duration(in.SegmentSeconds),
		GOPDuration:     duration(in.GopSeconds),
		FPS:             int(in.Fps),
		Timezone:        in.Timezone,
		RenditionLabel:  in.Overlay.RenditionLabel == nil || *in.Overlay.RenditionLabel,
		DASHAddressing:  stream.Addressing(in.Dash.Addressing),
		Ads: stream.Ads{
			Lead:             duration(in.Ads.LeadSeconds),
			Command:          stream.Command(in.Ads.Command),
			SegmentationType: string(in.Ads.SegmentationType),
			HLS:              convert[string](in.Ads.Hls),
			DASH:             convert[string](in.Ads.Dash),
			DASHMultiPeriod:  in.Ads.DashMultiPeriod,
		},
		Window: stream.Window{DVR: duration(in.Window.DvrSeconds), Retain: duration(in.Window.RetainSeconds)},
		Faults: toRules(in.Faults),
	}
	if r := in.Ads.Recurring; r != nil {
		s.Ads.Recurring = &stream.Recurring{
			Every:    duration(r.EverySeconds),
			Duration: duration(r.DurationSeconds),
			Offset:   duration(r.OffsetSeconds),
		}
	}
	for _, v := range in.Video {
		s.Video = append(s.Video, stream.Video{
			ID: v.Id, Codec: stream.VideoCodec(v.Codec), Profile: v.Profile, BitDepth: int(v.BitDepth),
			Width: v.Width, Height: v.Height, BitrateKbps: v.BitrateKbps,
		})
	}
	for _, a := range in.Audio {
		s.Audio = append(s.Audio, stream.Audio{
			ID: a.Id, Lang: a.Lang, Name: a.Name, Channels: int(a.Channels), BitrateKbps: a.BitrateKbps,
			Codec: stream.AudioCodec(a.Codec), Group: a.Group, Default: a.Default, TicHz: a.TicHz, TocHz: a.TocHz,
		})
	}
	for _, sub := range in.Subtitles {
		s.Subtitles = append(s.Subtitles, stream.Subtitle{
			ID: sub.Id, Lang: sub.Lang, Name: sub.Name, Format: stream.SubtitleFormat(sub.Format), Default: sub.Default,
		})
	}
	if c := in.Captions.Cea608; c != nil {
		s.CEA608 = &stream.CEA608{Channel: string(c.Channel), Lang: c.Lang}
	}
	return s
}

func fromSpec(s stream.Spec) Spec {
	label := s.RenditionLabel
	out := Spec{
		Formats:        convert[SpecFormats](s.Formats),
		SegmentSeconds: seconds(s.SegmentDuration),
		GopSeconds:     seconds(s.GOPDuration),
		Fps:            SpecFps(s.FPS),
		Timezone:       s.Timezone,
		Overlay:        Overlay{RenditionLabel: &label},
		Dash:           DASH{Addressing: DASHAddressing(s.DASHAddressing)},
		Ads: Ads{
			LeadSeconds:      seconds(s.Ads.Lead),
			Command:          AdsCommand(s.Ads.Command),
			SegmentationType: AdsSegmentationType(s.Ads.SegmentationType),
			Hls:              convert[AdsHls](s.Ads.HLS),
			Dash:             convert[AdsDash](s.Ads.DASH),
			DashMultiPeriod:  s.Ads.DASHMultiPeriod,
		},
		Window: Window{DvrSeconds: seconds(s.Window.DVR), RetainSeconds: seconds(s.Window.Retain)},
		Faults: fromRules(s.Faults),
		Video:  make([]Video, 0, len(s.Video)),
		Audio:  make([]Audio, 0, len(s.Audio)),
	}
	if r := s.Ads.Recurring; r != nil {
		out.Ads.Recurring = &Recurring{
			EverySeconds:    seconds(r.Every),
			DurationSeconds: seconds(r.Duration),
			OffsetSeconds:   seconds(r.Offset),
		}
	}
	for _, v := range s.Video {
		out.Video = append(out.Video, Video{
			Id: v.ID, Codec: VideoCodec(v.Codec), Profile: v.Profile, BitDepth: VideoBitDepth(v.BitDepth),
			Width: v.Width, Height: v.Height, BitrateKbps: v.BitrateKbps,
		})
	}
	for _, a := range s.Audio {
		out.Audio = append(out.Audio, Audio{
			Id: a.ID, Lang: a.Lang, Name: a.Name, Channels: AudioChannels(a.Channels), BitrateKbps: a.BitrateKbps,
			Codec: AudioCodec(a.Codec), Group: a.Group, Default: a.Default, TicHz: a.TicHz, TocHz: a.TocHz,
		})
	}
	for _, sub := range s.Subtitles {
		out.Subtitles = append(out.Subtitles, Subtitle{
			Id: sub.ID, Lang: sub.Lang, Name: sub.Name, Format: SubtitleFormat(sub.Format), Default: sub.Default,
		})
	}
	if c := s.CEA608; c != nil {
		out.Captions.Cea608 = &CEA608{Channel: CEA608Channel(c.Channel), Lang: c.Lang}
	}
	return out
}

func fromStream(s stream.Stream) Stream {
	out := Stream{
		Id:      s.ID,
		Name:    s.Name,
		Spec:    fromSpec(s.Spec),
		Version: s.Version,
		Status: Status{
			State:          State(s.Status.State),
			PendingChanges: s.Status.PendingChanges,
			AudioAligned:   s.Spec.AudioAligned(),
		},
		CreatedAt: s.CreatedAt.UTC(),
		UpdatedAt: s.UpdatedAt.UTC(),
	}
	if e := s.Status.Epoch; e != nil {
		out.Status.Epoch = &Epoch{Number: e.Number, Ast: e.AST.UTC(), EndSegment: e.EndSegment, Spec: fromSpec(e.Spec)}
	}
	return out
}
