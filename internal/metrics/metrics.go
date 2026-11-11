// Package metrics keeps the origin's Prometheus metrics and serves them on
// /metrics. Labels never carry segment numbers or client data, so the series
// are bounded by streams × routes.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics records what the origin does. It is safe for concurrent use.
type Metrics struct {
	reg            *prometheus.Registry
	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	segmentsBuilt  *prometheus.CounterVec
	segmentBuild   *prometheus.HistogramVec
	segmentsLate   *prometheus.CounterVec
	ffmpegRunning  *prometheus.GaugeVec
	ffmpegFailures *prometheus.CounterVec
	adBreaks       *prometheus.CounterVec
	scte35Signals  *prometheus.CounterVec
	faults         *prometheus.CounterVec
}

// New returns Metrics on their own registry, with the Go and process
// collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := promauto.With(reg)
	return &Metrics{
		reg: reg,
		httpRequests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "HTTP requests answered.",
		}, []string{"route", "kind", "status"}),
		httpDuration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "Time to answer an HTTP request.", Buckets: prometheus.DefBuckets,
		}, []string{"route", "kind", "status"}),
		segmentsBuilt: f.NewCounterVec(prometheus.CounterOpts{
			Name: "segments_built_total", Help: "Segment sets encoded, every rendition of a track kind at once.",
		}, []string{"stream", "kind"}),
		segmentBuild: f.NewHistogramVec(prometheus.HistogramOpts{
			Name: "segment_build_seconds", Help: "Time to encode a segment set, retry included.", Buckets: prometheus.ExponentialBuckets(0.05, 2, 9),
		}, []string{"stream", "kind"}),
		segmentsLate: f.NewCounterVec(prometheus.CounterOpts{
			Name: "segments_late_total", Help: "Segments the producer finished after they were published.",
		}, []string{"stream"}),
		ffmpegRunning: f.NewGaugeVec(prometheus.GaugeOpts{
			Name: "ffmpeg_running", Help: "ffmpeg processes running.",
		}, []string{"stream"}),
		ffmpegFailures: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ffmpeg_failures_total", Help: "ffmpeg runs that failed, by reason (exit, timeout).",
		}, []string{"stream", "reason"}),
		adBreaks: f.NewCounterVec(prometheus.CounterOpts{
			Name: "ad_breaks_total", Help: "Breaks the live edge crossed, by type (started, ended).",
		}, []string{"stream", "source", "type"}),
		scte35Signals: f.NewCounterVec(prometheus.CounterOpts{
			Name: "scte35_signals_total", Help: "SCTE-35 signals the live edge crossed, by type (announced, cue_out, cue_in, cancel).",
		}, []string{"stream", "source", "type"}),
		faults: f.NewCounterVec(prometheus.CounterOpts{
			Name: "faults_injected_total", Help: "Faults injected into responses, by rule and action.",
		}, []string{"stream", "rule", "action"}),
	}
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// HTTPRequest records one answered request.
func (m *Metrics) HTTPRequest(route, kind string, status int, took time.Duration) {
	s := strconv.Itoa(status)
	m.httpRequests.WithLabelValues(route, kind, s).Inc()
	m.httpDuration.WithLabelValues(route, kind, s).Observe(took.Seconds())
}

// SegmentBuilt records one encoded segment set.
func (m *Metrics) SegmentBuilt(streamID, kind string, took time.Duration) {
	m.segmentsBuilt.WithLabelValues(streamID, kind).Inc()
	m.segmentBuild.WithLabelValues(streamID, kind).Observe(took.Seconds())
}

// SegmentLate records a segment finished after its publication.
func (m *Metrics) SegmentLate(streamID string) {
	m.segmentsLate.WithLabelValues(streamID).Inc()
}

// FFmpegStarted records a started ffmpeg process.
func (m *Metrics) FFmpegStarted(streamID string) {
	m.ffmpegRunning.WithLabelValues(streamID).Inc()
}

// FFmpegExited records the end of an ffmpeg process; a non-empty reason
// counts a failure.
func (m *Metrics) FFmpegExited(streamID, reason string) {
	m.ffmpegRunning.WithLabelValues(streamID).Dec()
	if reason != "" {
		m.ffmpegFailures.WithLabelValues(streamID, reason).Inc()
	}
}

// AdBreak records a break crossing the live edge; source is manual or
// recurring.
func (m *Metrics) AdBreak(streamID, source, typ string) {
	m.adBreaks.WithLabelValues(streamID, source, typ).Inc()
}

// SCTE35Signal records a signal crossing the live edge.
func (m *Metrics) SCTE35Signal(streamID, source, typ string) {
	m.scte35Signals.WithLabelValues(streamID, source, typ).Inc()
}

// FaultInjected records one action of a fault rule hitting a request. Rule
// IDs are bounded: at most 16 per stream, plus url-<kind> for URL faults.
func (m *Metrics) FaultInjected(streamID, rule, action string) {
	m.faults.WithLabelValues(streamID, rule, action).Inc()
}

// DropStream deletes every series of a deleted stream.
func (m *Metrics) DropStream(streamID string) {
	only := prometheus.Labels{"stream": streamID}
	for _, v := range []interface{ DeletePartialMatch(prometheus.Labels) int }{
		m.segmentsBuilt, m.segmentBuild, m.segmentsLate, m.ffmpegRunning, m.ffmpegFailures, m.adBreaks, m.scte35Signals, m.faults,
	} {
		v.DeletePartialMatch(only)
	}
}

// Cache reports the segment cache.
type Cache interface {
	CacheStats() (bytes, hits, misses int64)
}

// WatchCache exports cache_bytes, cache_hits_total and cache_misses_total,
// read from c at each scrape.
func (m *Metrics) WatchCache(c Cache) {
	f := promauto.With(m.reg)
	f.NewGaugeFunc(prometheus.GaugeOpts{Name: "cache_bytes", Help: "Encoded bytes in the segment cache."}, func() float64 {
		b, _, _ := c.CacheStats()
		return float64(b)
	})
	f.NewCounterFunc(prometheus.CounterOpts{Name: "cache_hits_total", Help: "Segment set lookups served from the cache."}, func() float64 {
		_, h, _ := c.CacheStats()
		return float64(h)
	})
	f.NewCounterFunc(prometheus.CounterOpts{Name: "cache_misses_total", Help: "Segment set lookups that had to encode."}, func() float64 {
		_, _, mi := c.CacheStats()
		return float64(mi)
	})
}

// WatchStreams exports streams{state}, counted by byState at each scrape.
func (m *Metrics) WatchStreams(byState func() (map[string]int, error)) {
	m.reg.MustRegister(stateCollector{
		desc: prometheus.NewDesc("streams", "Streams by state.", []string{"state"}, nil),
		fn:   byState,
	})
}

type stateCollector struct {
	desc *prometheus.Desc
	fn   func() (map[string]int, error)
}

func (c stateCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c stateCollector) Collect(ch chan<- prometheus.Metric) {
	counts, err := c.fn()
	if err != nil {
		ch <- prometheus.NewInvalidMetric(c.desc, err)
		return
	}
	for state, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), state)
	}
}
