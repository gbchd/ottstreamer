// Package config reads the origin's configuration from flags, overridden by
// ORIGIN_* environment variables.
package config

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// Config is the origin's configuration.
type Config struct {
	Listen              string
	APIKeys             []string // name:role:key
	FFmpeg              string
	FFmpegVersion       string // pinned; empty accepts any
	TmpDir              string
	CacheBytes          int64
	MaxFFmpeg           int
	ProducerParallelism int
	CORSAPIOrigins      []string
	ShutdownDelay       time.Duration
	UI                  bool
}

// Load parses args, then applies every non-empty ORIGIN_* variable on top.
// List values are comma-separated.
func Load(args []string, getenv func(string) string) (Config, error) {
	var c Config
	var keys, origins string
	fs := flag.NewFlagSet("origin", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", ":8080", "HTTP listen address (ORIGIN_LISTEN)")
	fs.StringVar(&keys, "api-keys", "", "comma-separated name:role:key API keys, role read or write; prefer the env var (ORIGIN_API_KEYS)")
	fs.StringVar(&c.FFmpeg, "ffmpeg", "ffmpeg", "ffmpeg binary (ORIGIN_FFMPEG)")
	fs.StringVar(&c.FFmpegVersion, "ffmpeg-version", "", "ffmpeg version /readyz requires, e.g. 9.0.2; empty accepts any (ORIGIN_FFMPEG_VERSION)")
	fs.StringVar(&c.TmpDir, "tmp", os.TempDir(), "scratch directory for encoder output (ORIGIN_TMP)")
	fs.Int64Var(&c.CacheBytes, "cache-bytes", 512<<20, "segment cache size in bytes (ORIGIN_CACHE_BYTES)")
	fs.IntVar(&c.MaxFFmpeg, "max-ffmpeg", max(1, runtime.NumCPU()/2), "concurrent ffmpeg processes (ORIGIN_MAX_FFMPEG)")
	fs.IntVar(&c.ProducerParallelism, "producer-parallelism", 2, "segments of one stream encoded at once (ORIGIN_PRODUCER_PARALLELISM)")
	fs.StringVar(&origins, "cors-api-origins", "", "comma-separated origins allowed to call /api from a browser (ORIGIN_CORS_API_ORIGINS)")
	fs.DurationVar(&c.ShutdownDelay, "shutdown-delay", 0, "on SIGTERM, how long /readyz answers 503 before the server stops accepting connections (ORIGIN_SHUTDOWN_DELAY)")
	fs.BoolVar(&c.UI, "ui", true, "serve the web UI at /ui/ (ORIGIN_UI)")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	for _, f := range []string{"listen", "api-keys", "ffmpeg", "ffmpeg-version", "tmp", "cache-bytes", "max-ffmpeg", "producer-parallelism", "cors-api-origins", "shutdown-delay", "ui"} {
		env := "ORIGIN_" + strings.ToUpper(strings.ReplaceAll(f, "-", "_"))
		if v := getenv(env); v != "" {
			if err := fs.Set(f, v); err != nil {
				return Config{}, fmt.Errorf("%s: %w", env, err)
			}
		}
	}
	c.APIKeys, c.CORSAPIOrigins = list(keys), list(origins)
	if c.CacheBytes <= 0 || c.MaxFFmpeg < 1 || c.ProducerParallelism < 1 || c.ShutdownDelay < 0 {
		return Config{}, fmt.Errorf("cache-bytes, max-ffmpeg and producer-parallelism must be positive, shutdown-delay not negative")
	}
	return c, nil
}

func list(s string) []string {
	var out []string
	for v := range strings.SplitSeq(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
