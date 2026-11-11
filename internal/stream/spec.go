package stream

import (
	"cmp"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"ottstreamer/internal/fault"
)

// Defaults filled in by Normalize.
const (
	DefaultSegmentDuration = 2 * time.Second
	DefaultFPS             = 25
	DefaultTimezone        = "UTC"
	DefaultProfile         = "high"
	DefaultGroup           = "main"
	DefaultLead            = 8 * time.Second
	DefaultDVR             = 30 * time.Second
	DefaultRetain          = 300 * time.Second
)

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	langPattern = regexp.MustCompile(`^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$`) // BCP 47, loosely

	hlsCueStyles      = []string{"daterange", "cueout", "oatcls", "adobe"}
	dashCueStyles     = []string{"eventstream-bin", "eventstream-xml", "inband"}
	segmentationTypes = []string{"provider_placement_opportunity", "provider_ad", "break", "distributor_placement_opportunity"}
)

// Normalize fills in defaults and stores the video ladder grouped by codec
// (AV1, HEVC, H.264), highest bitrate first.
func (s *Spec) Normalize() {
	s.setDefaults()
	s.sortVideo()
}

func (s *Spec) setDefaults() {
	if s.SegmentDuration == 0 {
		s.SegmentDuration = DefaultSegmentDuration
	}
	if s.GOPDuration == 0 {
		s.GOPDuration = s.SegmentDuration
	}
	if s.FPS == 0 {
		s.FPS = DefaultFPS
	}
	if s.Timezone == "" {
		s.Timezone = DefaultTimezone
	}
	if s.DASHAddressing == "" {
		s.DASHAddressing = AddressingTimelineTime
	}
	for i := range s.Video {
		v := &s.Video[i]
		if v.Codec == "" {
			v.Codec = CodecH264
		}
		if v.Profile == "" && v.Codec == CodecH264 {
			v.Profile = DefaultProfile
		}
	}
	// Beep tones step up per language, in order of first appearance, so
	// switching audio tracks is audible.
	var langs []string
	for i := range s.Audio {
		a := &s.Audio[i]
		if a.Codec == "" {
			a.Codec = CodecAAC
		}
		if a.Group == "" {
			a.Group = DefaultGroup
		}
		k := slices.Index(langs, a.Lang)
		if k < 0 {
			k = len(langs)
			langs = append(langs, a.Lang)
		}
		if a.TocHz == 0 {
			a.TocHz = 1500 + 500*k
		}
		if a.TicHz == 0 {
			a.TicHz = 480 + 160*k
		}
	}
	if s.Ads.Lead == 0 {
		s.Ads.Lead = DefaultLead
	}
	if s.Ads.Command == "" {
		s.Ads.Command = CommandSpliceInsert
	}
	if len(s.Ads.HLS) == 0 && s.Has(FormatHLS) {
		s.Ads.HLS = []string{"daterange"}
	}
	if len(s.Ads.DASH) == 0 && s.Has(FormatDASH) {
		s.Ads.DASH = []string{"eventstream-bin"}
	}
	if s.Window.DVR == 0 {
		s.Window.DVR = DefaultDVR
	}
	if s.Window.Retain == 0 {
		s.Window.Retain = DefaultRetain
	}
}

func (s *Spec) sortVideo() {
	rank := func(c VideoCodec) int {
		switch c {
		case CodecAV1:
			return 0
		case CodecHEVC:
			return 1
		case CodecH264:
			return 2
		}
		return 3
	}
	slices.SortStableFunc(s.Video, func(a, b Video) int {
		return cmp.Or(cmp.Compare(rank(a.Codec), rank(b.Codec)), cmp.Compare(b.BitrateKbps, a.BitrateKbps))
	})
}

// normalized returns a validated, normalized deep copy of s. Validation runs
// before the ladder is sorted, so field paths use the caller's indices.
func (s Spec) normalized(v *ValidationError) Spec {
	c := s.Clone()
	c.setDefaults()
	c.validate(v)
	c.sortVideo()
	rules, err := fault.Normalized(c.Faults)
	if ferr, ok := errors.AsType[*fault.ValidationError](err); ok {
		for _, f := range ferr.Fields {
			v.add("spec."+f.Field, f.Message)
		}
	}
	c.Faults = rules
	return c
}

// AudioAligned reports whether every audio segment has the same length: the
// segment is a whole number of frames of every audio codec in use.
func (s Spec) AudioAligned() bool {
	samples := int64(s.SegmentDuration) * 48000
	if samples%int64(time.Second) != 0 {
		return false
	}
	samples /= int64(time.Second)
	for _, a := range s.Audio {
		fs := int64(a.Codec.FrameSize())
		if fs == 0 || samples%fs != 0 {
			return false
		}
	}
	return true
}

// Validate checks a normalized spec. The error is a *ValidationError naming
// every invalid field; errors.Is(err, ErrInvalid) holds.
func (s Spec) Validate() error {
	var v ValidationError
	s.validate(&v)
	return v.err()
}

func (s Spec) validate(v *ValidationError) {
	s.validateFormats(v)
	segOK := s.validateTiming(v)
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		v.add("spec.timezone", "unknown time zone")
	}
	ids := map[string]bool{}
	s.validateVideo(v, ids)
	s.validateAudio(v, ids)
	switch s.DASHAddressing {
	case AddressingNumber, AddressingTimelineTime, AddressingTimelineNumber:
	default:
		v.add("spec.dash.addressing", `must be "number", "timeline-time" or "timeline-number"`)
	}
	s.validateAds(v, segOK)
	if segOK {
		s.validateWindow(v)
	}
}

func (s Spec) validateFormats(v *ValidationError) {
	if len(s.Formats) == 0 {
		v.add("spec.formats", "at least one format is required")
	}
	for i, f := range s.Formats {
		path := fmt.Sprintf("spec.formats[%d]", i)
		switch {
		case f != FormatHLS && f != FormatDASH:
			v.add(path, `must be "hls" or "dash"`)
		case slices.Index(s.Formats, f) < i:
			v.add(path, "duplicate format")
		}
	}
}

// validateTiming checks fps, segment and GOP durations. It reports whether
// the segment duration is usable for the checks that count in segments.
func (s Spec) validateTiming(v *ValidationError) bool {
	if !slices.Contains([]int{24, 25, 30, 50, 60}, s.FPS) {
		v.add("spec.fps", "must be 24, 25, 30, 50 or 60")
		return false
	}
	wholeFrames := func(d time.Duration) bool {
		return d > 0 && int64(d)*int64(s.FPS)%int64(time.Second) == 0
	}
	segOK := false
	switch {
	case s.SegmentDuration < 500*time.Millisecond || s.SegmentDuration > 10*time.Second:
		v.add("spec.segmentSeconds", "must be between 0.5 and 10 s")
	case !wholeFrames(s.SegmentDuration):
		v.add("spec.segmentSeconds", fmt.Sprintf("must be a whole number of frames at %d fps", s.FPS))
	default:
		segOK = true
	}
	switch {
	case !wholeFrames(s.GOPDuration):
		v.add("spec.gopSeconds", fmt.Sprintf("must be a whole number of frames at %d fps", s.FPS))
	case segOK && s.SegmentDuration%s.GOPDuration != 0:
		v.add("spec.gopSeconds", "must divide segmentSeconds evenly")
	}
	return segOK
}

func (s Spec) validateVideo(v *ValidationError, ids map[string]bool) {
	if len(s.Video) < 1 || len(s.Video) > 12 {
		v.add("spec.video", "must have 1 to 12 renditions")
	}
	type key struct {
		codec   VideoCodec
		bitrate int
	}
	bitrates := map[key]bool{}
	for i, r := range s.Video {
		path := fmt.Sprintf("spec.video[%d]", i)
		checkID(v, ids, path, r.ID)
		switch r.Codec {
		case CodecH264:
			if !slices.Contains([]string{"baseline", "main", "high"}, r.Profile) {
				v.add(path+".profile", `must be "baseline", "main" or "high"`)
			}
		case CodecHEVC, CodecAV1:
			v.add(path+".codec", string(r.Codec)+" is not supported yet")
		default:
			v.add(path+".codec", `must be "h264", "hevc" or "av1"`)
		}
		if r.Width <= 0 || r.Width%2 != 0 || r.Width > 1920 {
			v.add(path+".width", "must be even and at most 1920")
		}
		if r.Height <= 0 || r.Height%2 != 0 || r.Height > 1080 {
			v.add(path+".height", "must be even and at most 1080")
		}
		if r.BitrateKbps < 100 || r.BitrateKbps > 20000 {
			v.add(path+".bitrateKbps", "must be between 100 and 20000")
		}
		k := key{r.Codec, r.BitrateKbps}
		if bitrates[k] {
			v.add(path+".bitrateKbps", "duplicate bitrate for this codec")
		}
		bitrates[k] = true
	}
}

func (s Spec) validateAudio(v *ValidationError, ids map[string]bool) {
	if len(s.Audio) < 1 || len(s.Audio) > 8 {
		v.add("spec.audio", "must have 1 to 8 renditions")
	}
	type track struct {
		lang     string
		channels int
		codec    AudioCodec
	}
	groups := map[string][]track{}
	defaults := map[string]int{}
	var order []string
	for i, a := range s.Audio {
		path := fmt.Sprintf("spec.audio[%d]", i)
		checkID(v, ids, path, a.ID)
		if !langPattern.MatchString(a.Lang) {
			v.add(path+".lang", "must be a BCP 47 language tag")
		}
		if !slices.Contains([]int{1, 2, 6}, a.Channels) {
			v.add(path+".channels", "must be 1, 2 or 6")
		}
		minKbps, maxKbps := 0, 0
		switch a.Codec {
		case CodecAAC:
			minKbps, maxKbps = 32, 320
		case CodecAC3:
			minKbps, maxKbps = 192, 640
		case CodecEAC3, CodecOpus:
			v.add(path+".codec", string(a.Codec)+" is not supported yet")
		default:
			v.add(path+".codec", `must be "aac", "ac3", "eac3" or "opus"`)
		}
		if maxKbps > 0 && (a.BitrateKbps < minKbps || a.BitrateKbps > maxKbps) {
			v.add(path+".bitrateKbps", fmt.Sprintf("must be between %d and %d for %s", minKbps, maxKbps, a.Codec))
		}
		if !idPattern.MatchString(a.Group) {
			v.add(path+".group", "must match [a-z0-9-]{1,40}")
		}
		for _, hz := range []struct {
			field string
			value int
		}{{"ticHz", a.TicHz}, {"tocHz", a.TocHz}} {
			if hz.value < 20 || hz.value > 20000 {
				v.add(path+"."+hz.field, "must be between 20 and 20000")
			}
		}
		if _, ok := groups[a.Group]; !ok {
			order = append(order, a.Group)
		}
		groups[a.Group] = append(groups[a.Group], track{a.Lang, a.Channels, a.Codec})
		if a.Default {
			defaults[a.Group]++
		}
	}
	for _, g := range order {
		if defaults[g] != 1 {
			v.add("spec.audio", fmt.Sprintf("group %q must have exactly one default track", g))
		}
	}
	if !s.Has(FormatHLS) || len(order) < 2 {
		return
	}
	cmpTrack := func(a, b track) int {
		return cmp.Or(strings.Compare(a.lang, b.lang), cmp.Compare(a.channels, b.channels), strings.Compare(string(a.codec), string(b.codec)))
	}
	first := slices.SortedFunc(slices.Values(groups[order[0]]), cmpTrack)
	for _, g := range order[1:] {
		if !slices.Equal(first, slices.SortedFunc(slices.Values(groups[g]), cmpTrack)) {
			v.add("spec.audio", fmt.Sprintf("group %q must offer the same tracks (lang, channels, codec) as group %q for HLS", g, order[0]))
		}
	}
}

func checkID(v *ValidationError, ids map[string]bool, path, id string) {
	if !idPattern.MatchString(id) {
		v.add(path+".id", "must match [a-z0-9-]{1,40}")
		return
	}
	if ids[id] {
		v.add(path+".id", "duplicate rendition id")
	}
	ids[id] = true
}

func (s Spec) validateAds(v *ValidationError, segOK bool) {
	a := s.Ads
	if segOK && (a.Lead < 2*s.SegmentDuration || a.Lead > 60*time.Second) {
		v.add("spec.ads.leadSeconds", "must be between 2 segments and 60 s")
	}
	switch a.Command {
	case CommandSpliceInsert:
		if a.SegmentationType != "" {
			v.add("spec.ads.segmentationType", "only allowed with time_signal")
		}
	case CommandTimeSignal:
		if !slices.Contains(segmentationTypes, a.SegmentationType) {
			v.add("spec.ads.segmentationType", "must be one of "+strings.Join(segmentationTypes, ", "))
		}
	default:
		v.add("spec.ads.command", `must be "splice_insert" or "time_signal"`)
	}
	if s.Has(FormatHLS) {
		checkSubset(v, "spec.ads.hls", a.HLS, hlsCueStyles)
		if slices.Contains(a.HLS, "cueout") && slices.Contains(a.HLS, "oatcls") {
			v.add("spec.ads.hls", "cueout and oatcls are exclusive")
		}
	}
	if s.Has(FormatDASH) {
		checkSubset(v, "spec.ads.dash", a.DASH, dashCueStyles)
	}
	if r := a.Recurring; r != nil && segOK {
		seg := s.SegmentDuration
		if r.Duration < seg {
			v.add("spec.ads.recurring.durationSeconds", "must be at least one segment")
		}
		if r.Every < r.Duration+2*seg {
			v.add("spec.ads.recurring.everySeconds", "must be at least durationSeconds plus 2 segments")
		}
		if r.Offset < a.Lead {
			v.add("spec.ads.recurring.offsetSeconds", "must be at least leadSeconds")
		}
	}
}

func checkSubset(v *ValidationError, path string, got, allowed []string) {
	if len(got) == 0 {
		v.add(path, "at least one of "+strings.Join(allowed, ", ")+" is required")
	}
	for i, g := range got {
		switch {
		case !slices.Contains(allowed, g):
			v.add(fmt.Sprintf("%s[%d]", path, i), "must be one of "+strings.Join(allowed, ", "))
		case slices.Index(got, g) < i:
			v.add(fmt.Sprintf("%s[%d]", path, i), "duplicate value")
		}
	}
}

func (s Spec) validateWindow(v *ValidationError) {
	w := s.Window
	if w.DVR < 3*s.SegmentDuration || w.DVR > 24*time.Hour {
		v.add("spec.window.dvrSeconds", "must be between 3 segments and 24 h")
	}
	if w.Retain < w.DVR || w.Retain > 24*time.Hour {
		v.add("spec.window.retainSeconds", "must be between dvrSeconds and 24 h")
	}
}
