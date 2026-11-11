// Command origin serves synthetic live CMAF streams over HLS and DASH, with
// on-demand SCTE-35 ad breaks.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"ottstreamer/internal/accesslog"
	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakmem"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/api"
	"ottstreamer/internal/apikey"
	"ottstreamer/internal/config"
	"ottstreamer/internal/cors"
	"ottstreamer/internal/event"
	"ottstreamer/internal/event/eventmem"
	"ottstreamer/internal/fault"
	"ottstreamer/internal/fault/faultmem"
	"ottstreamer/internal/health"
	"ottstreamer/internal/live"
	"ottstreamer/internal/live/liveffmpeg"
	"ottstreamer/internal/live/livehttp"
	"ottstreamer/internal/live/livemp4"
	"ottstreamer/internal/metrics"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streammem"
	"ottstreamer/internal/ulid"
)

// eventsPerStream is how many events the log keeps per stream.
const eventsPerStream = 10000

func main() {
	logger := slog.New(accesslog.WithRequestID(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(logger); err != nil {
		logger.ErrorContext(context.Background(), "origin failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	auth, err := apikey.New(cfg.APIKeys)
	if err != nil {
		return fmt.Errorf("api keys: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(cfg.APIKeys) == 0 {
		logger.WarnContext(ctx, "no api keys configured, /api answers 401 except /api/time")
	}

	tmp, err := os.MkdirTemp(cfg.TmpDir, "ottstreamer-")
	if err != nil {
		return fmt.Errorf("create tmp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			logger.ErrorContext(ctx, "remove tmp dir", "dir", tmp, "error", err)
		}
	}()
	met := metrics.New()
	enc, err := liveffmpeg.NewEncoder(cfg.FFmpeg, tmp, cfg.MaxFFmpeg, met, logger)
	if err != nil {
		return fmt.Errorf("ffmpeg encoder: %w", err)
	}
	if v, err := enc.Check(ctx, cfg.FFmpegVersion); err != nil {
		logger.WarnContext(ctx, "ffmpeg check failed, /readyz answers 503", "error", err)
	} else {
		logger.InfoContext(ctx, "ffmpeg found", "version", v)
	}

	repo := streammem.NewRepository()
	var ids ulid.Generator
	events := event.NewService(eventmem.NewRepository(eventsPerStream), time.Now, ids.New, countEvents(met), logger)
	breaks := adbreak.NewService(repo, adbreakmem.NewRepository(), adbreakscte35.NewEncoder(), events, time.Now)
	faults := fault.NewService(faultmem.NewRepository(), events, met, time.Now, func() int { return rand.IntN(100) }) //nolint:gosec // fault draws need no crypto
	mgr := live.NewManager(enc, livemp4.NewWriter(), breaks, events, cfg.CacheBytes, cfg.ProducerParallelism, met, time.Now, logger)
	defer mgr.Close() // after the server has drained, before the tmp dir goes
	streams := stream.NewService(repo, repo, mgr, breaks, faults, events, time.Now)
	if err := streams.Resume(ctx); err != nil {
		return fmt.Errorf("resume streams: %w", err)
	}
	go housekeep(ctx, streams, faults, logger)

	met.WatchCache(mgr)
	met.WatchStreams(func() (map[string]int, error) { return countByState(ctx, repo) })
	ready := readiness(ctx, repo, enc, mgr, tmp, cfg.FFmpegVersion)

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: handler(deps{
			auth: auth, apiOrigins: cfg.CORSAPIOrigins, streams: streams, breaks: breaks, events: events, faults: faults, mgr: mgr, repo: repo,
			ready: ready, metrics: met, now: time.Now, logger: logger,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	srv.RegisterOnShutdown(events.Close) // event streams end, so the drain does not wait for them
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.InfoContext(ctx, "listening", "addr", cfg.Listen)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.InfoContext(ctx, "shutting down", "delay", cfg.ShutdownDelay)
	time.Sleep(cfg.ShutdownDelay) // /readyz answers 503 meanwhile
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return srv.Shutdown(drain)
}

// deps are what the HTTP pipeline serves.
type deps struct {
	auth       func(http.Handler) http.Handler
	apiOrigins []string
	streams    *stream.Service
	breaks     *adbreak.Service
	events     *event.Service
	faults     *fault.Service
	mgr        *live.Manager
	repo       *streammem.Repository
	ready      http.Handler
	metrics    *metrics.Metrics
	now        func() time.Time
	logger     *slog.Logger
}

// handler is the request pipeline: recover > request ID, access log and
// metrics > CORS > auth (/api except /api/time) > route. Faults apply in the
// /live handlers and /api/time, which know the segment a request is for.
func handler(d deps) http.Handler {
	apiMux := http.NewServeMux()
	api.Register(apiMux, d.streams, d.breaks, d.events, d.faults, d.faults, d.now, d.logger)
	mux := http.NewServeMux()
	mux.Handle("/api/", d.auth(accesslog.Routed(apiMux)))
	mux.Handle("/api/time", apiMux) // open, more specific than /api/
	livehttp.NewHandler(d.mgr, d.repo, d.breaks, d.faults, d.now, d.logger).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.Handle("GET /readyz", d.ready)
	mux.Handle("GET /metrics", d.metrics.Handler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ottstreamer origin\n")) })
	return recoverer(d.logger, accesslog.New(d.logger, d.metrics, cors.New(d.apiOrigins, mux)))
}

// readiness is /readyz. It turns 503 once ctx, the process's lifetime, is done.
func readiness(ctx context.Context, repo stream.Repository, enc *liveffmpeg.Encoder, mgr *live.Manager, tmp, ffmpegPin string) http.Handler {
	return health.Ready(
		health.Check{Name: "repository", Run: func(ctx context.Context) error {
			_, err := repo.List(ctx) // ponytail: a DB ping once there is a DB
			return err
		}},
		health.Check{Name: "ffmpeg", Run: func(ctx context.Context) error {
			_, err := enc.Check(ctx, ffmpegPin)
			return err
		}},
		health.Check{Name: "tmp", Run: health.Writable(tmp)},
		health.Check{Name: "producers", Run: func(context.Context) error {
			if late := mgr.Late(2); len(late) > 0 {
				return fmt.Errorf("more than 2 segments late: %s", strings.Join(late, ", "))
			}
			return nil
		}},
		health.Check{Name: "shutdown", Run: func(context.Context) error {
			if ctx.Err() != nil {
				return errors.New("shutting down")
			}
			return nil
		}},
	)
}

// countEvents feeds the metrics that come from the event log.
func countEvents(met *metrics.Metrics) func(event.Event) {
	return func(e event.Event) {
		group, what, _ := strings.Cut(string(e.Type), ".")
		source, _ := e.Data["source"].(string)
		switch {
		case e.Type == event.StreamDeleted:
			met.DropStream(e.StreamID)
		case e.Type == event.BreakStarted, e.Type == event.BreakEnded:
			met.AdBreak(e.StreamID, source, what)
		case group == "signal":
			met.SCTE35Signal(e.StreamID, source, what)
		}
	}
}

// countByState counts the streams in each state, zeros included.
func countByState(ctx context.Context, repo stream.Repository) (map[string]int, error) {
	ss, err := repo.List(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{string(stream.StateStopped): 0, string(stream.StateRunning): 0, string(stream.StateEnded): 0}
	for _, s := range ss {
		counts[string(s.Status.State)]++
	}
	return counts, nil
}

// housekeep moves ended streams past their retention to stopped and records
// the fault.injected events of every minute that is over.
func housekeep(ctx context.Context, streams *stream.Service, faults *fault.Service, logger *slog.Logger) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := streams.ExpireEnded(ctx); err != nil {
				logger.ErrorContext(ctx, "expire ended streams", "error", err)
			}
			faults.Flush(ctx)
		}
	}
}

// recoverer turns a handler panic into a logged 500.
func recoverer(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer recovered(r.Context(), logger, w, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// recovered must be deferred directly, so that recover sees the panic.
func recovered(ctx context.Context, logger *slog.Logger, w http.ResponseWriter, path string) {
	v := recover()
	if v == nil {
		return
	}
	if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(v)
	}
	logger.ErrorContext(ctx, "handler panic", "path", path, "panic", v, "stack", string(debug.Stack()))
	http.Error(w, "internal error", http.StatusInternalServerError)
}
