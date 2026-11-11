package livehttp

import (
	"cmp"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Comcast/scte35-go/pkg/scte35"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/live"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/timeline"
)

const (
	schemeBin    = "urn:scte:scte35:2014:xml+bin"
	schemeXML    = "urn:scte:scte35:2013:xml"
	schemeInband = "urn:scte:scte35:2013:bin"
	// cueInBit tells a cue-in Event's id from its cue-out's: event IDs stay
	// below 2^30 for manual breaks, and recurring ones (0x80000000 + k) keep
	// k below 2^30.
	cueInBit = 1 << 30
)

type mpdXML struct {
	XMLName                    xml.Name      `xml:"MPD"`
	Xmlns                      string        `xml:"xmlns,attr"`
	XmlnsSCTE35                string        `xml:"xmlns:scte35,attr"`
	Type                       string        `xml:"type,attr"`
	Profiles                   string        `xml:"profiles,attr"`
	AvailabilityStartTime      string        `xml:"availabilityStartTime,attr"`
	PublishTime                string        `xml:"publishTime,attr"`
	MediaPresentationDuration  string        `xml:"mediaPresentationDuration,attr,omitempty"`
	MinimumUpdatePeriod        string        `xml:"minimumUpdatePeriod,attr,omitempty"`
	TimeShiftBufferDepth       string        `xml:"timeShiftBufferDepth,attr"`
	SuggestedPresentationDelay string        `xml:"suggestedPresentationDelay,attr"`
	MinBufferTime              string        `xml:"minBufferTime,attr"`
	Periods                    []periodXML   `xml:"Period"`
	UTCTiming                  descriptorXML `xml:"UTCTiming"`
}

type periodXML struct {
	ID             string             `xml:"id,attr"`
	Start          string             `xml:"start,attr"`
	EventStreams   []eventStreamXML   `xml:"EventStream"`
	AdaptationSets []adaptationSetXML `xml:"AdaptationSet"`
}

type eventStreamXML struct {
	SchemeIDURI string     `xml:"schemeIdUri,attr"`
	Timescale   int64      `xml:"timescale,attr"`
	PTO         int64      `xml:"presentationTimeOffset,attr,omitempty"`
	Events      []eventXML `xml:"Event"`
}

type eventXML struct {
	ID               uint32     `xml:"id,attr"`
	PresentationTime int64      `xml:"presentationTime,attr"`
	Duration         int64      `xml:"duration,attr,omitempty"`
	Signal           *signalXML `xml:"scte35:Signal"`
	Section          string     `xml:",innerxml"` // eventstream-xml: the SpliceInfoSection
}

type signalXML struct {
	Binary string `xml:"scte35:Binary"`
}

type descriptorXML struct {
	SchemeIDURI string `xml:"schemeIdUri,attr"`
	Value       string `xml:"value,attr"`
}

// adaptationSetXML keeps the schema's element order.
type adaptationSetXML struct {
	ContentType       string              `xml:"contentType,attr"`
	MimeType          string              `xml:"mimeType,attr"`
	Lang              string              `xml:"lang,attr,omitempty"`
	SegmentAlignment  bool                `xml:"segmentAlignment,attr"`
	StartWithSAP      int                 `xml:"startWithSAP,attr,omitempty"`
	MaxWidth          int                 `xml:"maxWidth,attr,omitempty"`
	MaxHeight         int                 `xml:"maxHeight,attr,omitempty"`
	FrameRate         int                 `xml:"frameRate,attr,omitempty"`
	ChannelConfig     *descriptorXML      `xml:"AudioChannelConfiguration"`
	InbandEventStream *descriptorXML      `xml:"InbandEventStream"`
	Label             string              `xml:"Label,omitempty"`
	Role              *descriptorXML      `xml:"Role"`
	SegmentTemplate   segmentTemplateXML  `xml:"SegmentTemplate"`
	Representations   []representationXML `xml:"Representation"`
}

type segmentTemplateXML struct {
	Timescale      int64        `xml:"timescale,attr"`
	Duration       int64        `xml:"duration,attr,omitempty"`
	StartNumber    *int64       `xml:"startNumber,attr"`
	PTO            int64        `xml:"presentationTimeOffset,attr,omitempty"`
	Media          string       `xml:"media,attr"`
	Initialization string       `xml:"initialization,attr"`
	Timeline       *timelineXML `xml:"SegmentTimeline"`
}

type timelineXML struct {
	S []sXML `xml:"S"`
}

type sXML struct {
	T *int64 `xml:"t,attr"`
	D int64  `xml:"d,attr"`
	R int64  `xml:"r,attr,omitempty"`
}

type representationXML struct {
	ID                string `xml:"id,attr"`
	Codecs            string `xml:"codecs,attr"`
	Width             int    `xml:"width,attr,omitempty"`
	Height            int    `xml:"height,attr,omitempty"`
	AudioSamplingRate int    `xml:"audioSamplingRate,attr,omitempty"`
	Bandwidth         int    `xml:"bandwidth,attr"`
}

// mpdOptions are the per-request choices of an MPD.
type mpdOptions struct {
	mode   stream.Addressing
	cues   []string // eventstream-bin, eventstream-xml
	multi  bool
	inband bool
	dvr    time.Duration
}

func (req request) mpdOptions(r *http.Request) (mpdOptions, error) {
	o := mpdOptions{
		mode:   req.spec.DASHAddressing,
		multi:  req.spec.Ads.DASHMultiPeriod,
		inband: slices.Contains(req.spec.Ads.DASH, "inband"),
	}
	q := r.URL.Query()
	if v := q.Get("addressing"); v != "" {
		o.mode = stream.Addressing(v)
		switch o.mode {
		case stream.AddressingNumber, stream.AddressingTimelineTime, stream.AddressingTimelineNumber:
		default:
			return o, fmt.Errorf("%w: addressing must be number, timeline-time or timeline-number", errBadRequest)
		}
	}
	if v := q.Get("periods"); v != "" {
		var err error
		if o.multi, err = strconv.ParseBool(v); err != nil {
			return o, fmt.Errorf("%w: periods must be true or false", errBadRequest)
		}
	}
	def := slices.DeleteFunc(slices.Clone(req.spec.Ads.DASH), func(s string) bool { return s == "inband" })
	var err error
	if o.cues, err = override(r, "dashcues", def, "eventstream-bin", "eventstream-xml"); err != nil {
		return o, err
	}
	o.dvr, err = req.dvr(r)
	return o, err
}

// span is a Period: segments [start, end) of the epoch, of which the
// manifest lists [first, last].
type span struct {
	id          string
	start, end  int64
	first, last int64
}

func (h *Handler) mpd(r *http.Request, req request) ([]byte, error) {
	o, err := req.mpdOptions(r)
	if err != nil {
		return nil, err
	}
	tl := req.tl
	first, last := req.segments(o.dvr)
	// Breaks up to the frozen horizon can no longer move, so their cue-out
	// is announced as soon as it is frozen.
	signalled, err := h.breaksIn(r.Context(), req, first, tl.FrozenHorizon(req.now)+1)
	if err != nil {
		return nil, err
	}
	spans := []span{{id: fmt.Sprintf("e%d", req.epoch), start: 0, end: last + 1, first: first, last: last}}
	if o.multi {
		// ponytail: reads every break since AST to name the first Period
		// stably; keep the previous break boundary if long epochs make it slow.
		all, err := h.breaksIn(r.Context(), req, 0, last+1)
		if err != nil {
			return nil, err
		}
		spans = periods(req.epoch, all, first, last)
	}

	m := mpdXML{
		Xmlns:                      "urn:mpeg:dash:schema:mpd:2011",
		XmlnsSCTE35:                "http://www.scte.org/schemas/35",
		Type:                       "dynamic",
		Profiles:                   "urn:mpeg:dash:profile:isoff-live:2011,urn:mpeg:dash:profile:cmaf:2019",
		AvailabilityStartTime:      tl.AST.UTC().Format(time.RFC3339),
		PublishTime:                pdt(req.now),
		MinimumUpdatePeriod:        iso(tl.SegDuration()),
		TimeShiftBufferDepth:       iso(o.dvr),
		SuggestedPresentationDelay: iso(3 * tl.SegDuration()),
		MinBufferTime:              iso(tl.SegDuration()),
		UTCTiming:                  descriptorXML{SchemeIDURI: "urn:mpeg:dash:utc:http-iso:2014", Value: "//" + r.Host + "/api/time?stream=" + url.QueryEscape(req.id)},
	}
	if tl.EndSeg >= 0 {
		m.MediaPresentationDuration, m.MinimumUpdatePeriod = iso(time.Duration(tl.EndSeg+1)*tl.SegDuration()), ""
	}
	for i, sp := range spans {
		p := periodXML{ID: sp.id, Start: iso(time.Duration(sp.start) * tl.SegDuration())}
		var mine []adbreak.Break
		for _, b := range signalled {
			// Each break's events go to the Period its start falls in.
			if (i == 0 || b.StartSeg >= sp.start) && (i == len(spans)-1 || b.StartSeg < sp.end) {
				mine = append(mine, b)
			}
		}
		if p.EventStreams, err = eventStreams(o.cues, tl, mine, last, ticks(tl, timeline.VideoTimescale, sp.start)); err != nil {
			return nil, err
		}
		p.AdaptationSets = req.adaptationSets(o, sp)
		m.Periods = append(m.Periods, p)
	}
	out, err := xml.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal mpd: %w", err)
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

// periods splits the epoch at every break, a Period for the break and one
// for the content after it, and returns those meeting [first, last].
func periods(epoch int, breaks []adbreak.Break, first, last int64) []span {
	all := []span{{id: fmt.Sprintf("e%d", epoch)}}
	cut := func(at int64, id string) {
		if at > last {
			return
		}
		if all[len(all)-1].start == at {
			all = all[:len(all)-1] // an empty Period
		}
		all = append(all, span{id: fmt.Sprintf("e%d-%s", epoch, id), start: at})
	}
	for _, b := range breaks {
		cut(b.StartSeg, "b"+b.ID)
		cut(b.EndSeg, "a"+b.ID)
	}
	var out []span
	for i, sp := range all {
		sp.end = last + 1
		if i+1 < len(all) {
			sp.end = all[i+1].start
		}
		if sp.end <= first && i+1 < len(all) {
			continue
		}
		sp.first, sp.last = max(sp.start, first), sp.end-1
		out = append(out, sp)
	}
	return out
}

// eventStreams returns one EventStream per style with a cue-out Event per
// break, and a cue-in Event once the break's end is published.
func eventStreams(styles []string, tl timeline.Timeline, breaks []adbreak.Break, last, pto int64) ([]eventStreamXML, error) {
	var out []eventStreamXML
	for _, style := range styles {
		es := eventStreamXML{SchemeIDURI: schemeBin, Timescale: timeline.VideoTimescale, PTO: pto}
		if style == "eventstream-xml" {
			es.SchemeIDURI = schemeXML
		}
		for _, b := range breaks {
			out := eventXML{ID: b.EventID, PresentationTime: tl.VideoTfdt(b.StartSeg), Duration: (b.PlannedEndSeg - b.StartSeg) * tl.SegTicks()}
			in := eventXML{ID: b.EventID ^ cueInBit, PresentationTime: tl.VideoTfdt(b.EndSeg)}
			if err := fillEvent(&out, style, b.CueOut); err != nil {
				return nil, err
			}
			es.Events = append(es.Events, out)
			if b.EndSeg <= last+1 {
				if err := fillEvent(&in, style, b.CueIn); err != nil {
					return nil, err
				}
				es.Events = append(es.Events, in)
			}
		}
		if len(es.Events) > 0 {
			out = append(out, es)
		}
	}
	return out, nil
}

func fillEvent(e *eventXML, style string, cue []byte) error {
	if style == "eventstream-bin" {
		e.Signal = &signalXML{Binary: base64.StdEncoding.EncodeToString(cue)}
		return nil
	}
	// Cancelled breaks are never listed, so no cancel cue, which scte35-go
	// v1.8.0 cannot decode, gets here.
	sis, err := scte35.DecodeBytes(cue)
	if err != nil {
		return fmt.Errorf("decode cue of event %d: %w", e.ID, err)
	}
	b, err := xml.Marshal(sis)
	if err != nil {
		return fmt.Errorf("marshal cue of event %d: %w", e.ID, err)
	}
	e.Section = string(b)
	return nil
}

// adaptationSets returns one video AdaptationSet per codec, then one audio
// AdaptationSet per (lang, channels, codec) whose bitrate tiers become
// Representations.
func (req request) adaptationSets(o mpdOptions, sp span) []adaptationSetXML {
	var sets []adaptationSetXML
	index := map[string]int{}
	add := func(key string, as adaptationSetXML, rep representationXML) *adaptationSetXML {
		i, ok := index[key]
		if !ok {
			i, index[key] = len(sets), len(sets)
			sets = append(sets, as)
		}
		sets[i].Representations = append(sets[i].Representations, rep)
		return &sets[i]
	}
	for _, v := range req.spec.Video {
		t, _ := req.rt.Track(v.ID)
		as := adaptationSetXML{
			ContentType: "video", MimeType: "video/mp4", SegmentAlignment: true, StartWithSAP: 1,
			FrameRate: req.spec.FPS, SegmentTemplate: req.template(o, sp, t),
		}
		if o.inband {
			as.InbandEventStream = &descriptorXML{SchemeIDURI: schemeInband}
		}
		set := add("video/"+string(v.Codec), as, representationXML{
			ID: v.ID, Codecs: t.Codec, Width: v.Width, Height: v.Height, Bandwidth: v.BitrateKbps * 1000,
		})
		set.MaxWidth, set.MaxHeight = max(set.MaxWidth, v.Width), max(set.MaxHeight, v.Height)
	}
	for _, a := range req.spec.Audio {
		t, _ := req.rt.Track(a.ID)
		as := adaptationSetXML{
			ContentType: "audio", MimeType: "audio/mp4", Lang: a.Lang, SegmentAlignment: true,
			ChannelConfig:   channelConfig(a),
			Label:           cmp.Or(a.Name, a.ID),
			Role:            &descriptorXML{SchemeIDURI: "urn:mpeg:dash:role:2011", Value: "alternate"},
			SegmentTemplate: req.template(o, sp, t),
		}
		set := add(fmt.Sprintf("audio/%s/%d/%s", a.Lang, a.Channels, a.Codec), as, representationXML{
			ID: a.ID, Codecs: t.Codec, AudioSamplingRate: timeline.AudioTimescale, Bandwidth: a.BitrateKbps * 1000,
		})
		if a.Default {
			set.Role.Value = "main"
		}
	}
	return sets
}

func channelConfig(a stream.Audio) *descriptorXML {
	if a.Codec == stream.CodecAC3 {
		masks := map[int]string{1: "4000", 2: "A000", 6: "F801"}
		return &descriptorXML{SchemeIDURI: "tag:dolby.com,2014:dash:audio_channel_configuration:2011", Value: masks[a.Channels]}
	}
	return &descriptorXML{SchemeIDURI: "urn:mpeg:mpegB:cicp:ChannelConfiguration", Value: strconv.Itoa(a.Channels)}
}

// template returns the SegmentTemplate of track t in span sp.
func (req request) template(o mpdOptions, sp span, t live.Track) segmentTemplateXML {
	tl := req.tl
	ts := int64(t.Timescale)
	q := strings.ReplaceAll(req.url(""), "$", "$$")
	st := segmentTemplateXML{
		Timescale:      ts,
		PTO:            ticks(tl, ts, sp.start),
		Media:          "$RepresentationID$/$Number$.m4s" + q,
		Initialization: "$RepresentationID$/init.mp4" + q,
	}
	switch o.mode {
	case stream.AddressingNumber:
		start := sp.start
		st.Duration, st.StartNumber = ticks(tl, ts, 1), &start
	case stream.AddressingTimelineNumber:
		start := sp.first
		st.StartNumber, st.Timeline = &start, segmentTimeline(tl, t, sp.first, sp.last)
	default: // timeline-time
		st.Media = "$RepresentationID$/t/$Time$.m4s" + q
		st.Timeline = segmentTimeline(tl, t, sp.first, sp.last)
	}
	return st
}

// segmentTimeline describes segments [from, to] of t exactly, merging equal
// neighbours with @r.
func segmentTimeline(tl timeline.Timeline, t live.Track, from, to int64) *timelineXML {
	st := &timelineXML{}
	for n := from; n <= to; n++ {
		start, d := tl.VideoTfdt(n), tl.SegTicks()
		if !t.Video {
			first, end := tl.AudioFrameRange(n, t.FrameSize)
			start, d = first*int64(t.FrameSize), (end-first)*int64(t.FrameSize)
		}
		if k := len(st.S) - 1; k >= 0 && st.S[k].D == d {
			st.S[k].R++
			continue
		}
		s := sXML{D: d}
		if n == from {
			s.T = &start
		}
		st.S = append(st.S, s)
	}
	return st
}

// ticks is the start of segment n in a track timescale: an integer for
// 90 kHz and 48 kHz at every allowed frame rate.
func ticks(tl timeline.Timeline, timescale, n int64) int64 {
	return n * tl.SegFrames * timescale / int64(tl.FPS)
}

// iso formats d as an xs:duration in seconds: PT2S, PT1.92S.
func iso(d time.Duration) string {
	return "PT" + secs(d) + "S"
}
