package metrics_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ottstreamer/internal/metrics"
)

type cache struct{}

func (cache) CacheStats() (bytes, hits, misses int64) { return 1024, 7, 3 }

func TestMetrics(t *testing.T) {
	m := metrics.New()
	m.WatchCache(cache{})
	m.WatchStreams(func() (map[string]int, error) { return map[string]int{"running": 2, "stopped": 0}, nil })
	m.HTTPRequest("GET /live/{stream}/{rendition}/{segment}", "segment", 200, 30*time.Millisecond)
	m.SegmentBuilt("demo", "video", 400*time.Millisecond)
	m.SegmentLate("demo")
	m.FFmpegStarted("demo")
	m.FFmpegStarted("demo")
	m.FFmpegExited("demo", "")
	m.FFmpegExited("demo", "timeout")

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{
		`http_requests_total{kind="segment",route="GET /live/{stream}/{rendition}/{segment}",status="200"} 1`,
		`http_request_duration_seconds_count{kind="segment",route="GET /live/{stream}/{rendition}/{segment}",status="200"} 1`,
		`segments_built_total{kind="video",stream="demo"} 1`,
		`segment_build_seconds_sum{kind="video",stream="demo"} 0.4`,
		`segments_late_total{stream="demo"} 1`,
		`ffmpeg_running{stream="demo"} 0`,
		`ffmpeg_failures_total{reason="timeout",stream="demo"} 1`,
		`cache_bytes 1024`, `cache_hits_total 7`, `cache_misses_total 3`,
		`streams{state="running"} 2`, `streams{state="stopped"} 0`,
		`go_goroutines `,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}
