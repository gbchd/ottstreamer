// Command origin serves synthetic live CMAF streams over HLS and DASH, with
// on-demand SCTE-35 ad breaks.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/adbreak/adbreakmem"
	"ottstreamer/internal/adbreak/adbreakscte35"
	"ottstreamer/internal/api"
	"ottstreamer/internal/apikey"
	"ottstreamer/internal/config"
	"ottstreamer/internal/cors"
	"ottstreamer/internal/live"
	"ottstreamer/internal/live/liveffmpeg"
	"ottstreamer/internal/live/livehttp"
	"ottstreamer/internal/live/livemp4"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/stream/streammem"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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
	enc, err := liveffmpeg.NewEncoder(cfg.FFmpeg, tmp, cfg.MaxFFmpeg, logger)
	if err != nil {
		return fmt.Errorf("ffmpeg encoder: %w", err)
	}

	repo := streammem.NewRepository()
	breaks := adbreak.NewService(repo, adbreakmem.NewRepository(), adbreakscte35.NewEncoder(), time.Now)
	mgr := live.NewManager(enc, livemp4.NewWriter(), breaks, cfg.CacheBytes, cfg.ProducerParallelism, time.Now, logger)
	defer mgr.Close() // after the server has drained, before the tmp dir goes
	streams := stream.NewService(repo, repo, mgr, breaks, time.Now)
	if err := streams.Resume(ctx); err != nil {
		return fmt.Errorf("resume streams: %w", err)
	}
	go expireEnded(ctx, streams, logger)

	apiMux := http.NewServeMux()
	api.Register(apiMux, streams, breaks, time.Now, logger)
	mux := http.NewServeMux()
	mux.Handle("/api/", auth(apiMux))
	mux.Handle("/api/time", apiMux) // open, more specific than /api/
	livehttp.NewHandler(mgr, repo, breaks, time.Now, logger).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ottstreamer origin\n")) })

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           recoverer(logger, cors.New(cfg.CORSAPIOrigins, mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.InfoContext(ctx, "listening", "addr", cfg.Listen)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.InfoContext(ctx, "shutting down")
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return srv.Shutdown(drain)
}

// expireEnded moves ended streams past their retention to stopped.
func expireEnded(ctx context.Context, streams *stream.Service, logger *slog.Logger) {
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
