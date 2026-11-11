// Package livehttp serves the /live routes: HLS playlists, the DASH MPD,
// init segments and media segments of running and ended streams.
package livehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ottstreamer/internal/adbreak"
	"ottstreamer/internal/live"
	"ottstreamer/internal/stream"
	"ottstreamer/internal/timeline"
)

// Runtimes is the part of *live.Manager the handlers call.
type Runtimes interface {
	Runtime(id string) (*live.Runtime, bool)
	Init(streamID, rendition string) ([]byte, error)
	WriteSegment(ctx context.Context, w io.Writer, streamID, rendition string, n int64) error
}

// Streams reads the current stream record, whose window applies at once.
type Streams interface {
	Get(ctx context.Context, id string) (stream.Stream, error)
}

// Breaks reads the ad-break schedule.
type Breaks interface {
	// InRange returns the breaks overlapping segments [from, to), cancelled
	// ones included, sorted by StartSeg.
	InRange(ctx context.Context, streamID string, epoch int, from, to int64) ([]adbreak.Break, error)
}

// Handler serves /live.
type Handler struct {
	runtimes Runtimes
	streams  Streams
	breaks   Breaks
	now      func() time.Time
	logger   *slog.Logger
}

// NewHandler returns a Handler.
func NewHandler(runtimes Runtimes, streams Streams, breaks Breaks, now func() time.Time, logger *slog.Logger) *Handler {
	return &Handler{runtimes: runtimes, streams: streams, breaks: breaks, now: now, logger: logger}
}

// Register adds the /live routes to mux. CORS is the caller's middleware.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /live/{stream}/{file}", h.manifest)
	mux.HandleFunc("GET /live/{stream}/{rendition}/init.mp4", h.init)
	mux.HandleFunc("GET /live/{stream}/{rendition}/{segment}", h.segmentByNumber)
	mux.HandleFunc("GET /live/{stream}/{rendition}/t/{segment}", h.segmentByTime)
}

// errBadRequest marks a query parameter the handler refuses.
var errBadRequest = errors.New("bad request")

// request is what every /live route reads first.
type request struct {
	id     string
	rt     *live.Runtime
	spec   stream.Spec // epoch snapshot
	epoch  int
	tl     timeline.Timeline
	window stream.Window // current, edits apply at once
	now    time.Time
	query  string // raw query, copied onto every URL a manifest writes
}

// resolve reads the stream, its runtime and ?now=. It fails with
// live.ErrNotFound unless the stream is running or ended.
func (h *Handler) resolve(r *http.Request) (request, error) {
	id := r.PathValue("stream")
	s, err := h.streams.Get(r.Context(), id)
	if errors.Is(err, stream.ErrNotFound) {
		return request{}, fmt.Errorf("%w: stream %s", live.ErrNotFound, id)
	}
	if err != nil {
		return request{}, err
	}
	if st := s.Status.State; (st != stream.StateRunning && st != stream.StateEnded) || s.Status.Epoch == nil {
		return request{}, fmt.Errorf("%w: stream %s is %s", live.ErrNotFound, id, st)
	}
	rt, ok := h.runtimes.Runtime(id)
	if !ok {
		return request{}, fmt.Errorf("%w: no runtime for %s", live.ErrNotFound, id)
	}
	e := rt.Stream().Status.Epoch
	req := request{
		id:     id,
		rt:     rt,
		spec:   e.Spec,
		epoch:  e.Number,
		tl:     rt.Timeline(),
		window: s.Status.Epoch.Spec.Window,
		now:    h.now(),
		query:  r.URL.RawQuery,
	}
	if v := r.URL.Query().Get("now"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return request{}, fmt.Errorf("%w: now must be unix milliseconds", errBadRequest)
		}
		req.now = time.UnixMilli(ms)
	}
	return req, nil
}

// dvr returns the window length: ?dvr=<seconds> between 3 segments and the
// retention, or the stream's dvrSeconds.
func (req request) dvr(r *http.Request) (time.Duration, error) {
	v := r.URL.Query().Get("dvr")
	if v == "" {
		return req.window.DVR, nil
	}
	sec, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(sec) || sec > req.window.Retain.Seconds() {
		return 0, fmt.Errorf("%w: dvr must be between 3 segments and %v", errBadRequest, req.window.Retain)
	}
	d := time.Duration(sec * float64(time.Second))
	if d < 3*req.tl.SegDuration() || d > req.window.Retain {
		return 0, fmt.Errorf("%w: dvr must be between 3 segments and %v", errBadRequest, req.window.Retain)
	}
	return d, nil
}

// window returns the segments [first, last] a manifest lists; last is -1
// before the first segment is published.
func (req request) segments(dvr time.Duration) (first, last int64) {
	last = req.tl.LiveEdge(req.now)
	return max(0, last-req.tl.SegsFor(dvr)+1), last
}

// url appends the request's query to a manifest URL.
func (req request) url(path string) string {
	if req.query == "" {
		return path
	}
	return path + "?" + req.query
}

// breaksIn returns the breaks overlapping [from, to) that are not cancelled.
func (h *Handler) breaksIn(ctx context.Context, req request, from, to int64) ([]adbreak.Break, error) {
	if to <= from {
		return nil, nil
	}
	all, err := h.breaks.InRange(ctx, req.id, req.epoch, from, to)
	if err != nil {
		return nil, fmt.Errorf("breaks of %s: %w", req.id, err)
	}
	out := all[:0:0]
	for _, b := range all {
		if !b.Cancelled {
			out = append(out, b)
		}
	}
	return out, nil
}

func (h *Handler) manifest(w http.ResponseWriter, r *http.Request) {
	file := r.PathValue("file")
	switch {
	case file == "master.m3u8":
		h.serveManifest(w, r, stream.FormatHLS, "application/vnd.apple.mpegurl", h.multivariant)
	case file == "manifest.mpd":
		h.serveManifest(w, r, stream.FormatDASH, "application/dash+xml", h.mpd)
	case strings.HasSuffix(file, ".m3u8"):
		h.serveManifest(w, r, stream.FormatHLS, "application/vnd.apple.mpegurl", h.mediaPlaylist)
	default:
		http.NotFound(w, r)
	}
}

type render func(r *http.Request, req request) ([]byte, error)

func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, f stream.Format, contentType string, fn render) {
	req, err := h.resolve(r)
	if err == nil && !req.spec.Has(f) {
		err = fmt.Errorf("%w: %s has no %s", live.ErrNotFound, req.id, f)
	}
	var body []byte
	if err == nil {
		body, err = fn(r, req)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, contentType, int((req.tl.SegDuration() / 2).Seconds()), body)
}

func (h *Handler) init(w http.ResponseWriter, r *http.Request) {
	req, err := h.resolve(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rendition := r.PathValue("rendition")
	t, ok := req.rt.Track(rendition)
	if !ok {
		h.fail(w, r, fmt.Errorf("%w: rendition %s", live.ErrNotFound, rendition))
		return
	}
	b, err := h.runtimes.Init(req.id, rendition)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, mimeType(t), int(req.window.Retain.Seconds()), b)
}

func (h *Handler) segmentByNumber(w http.ResponseWriter, r *http.Request) {
	h.segment(w, r, func(_ live.Track, _ timeline.Timeline, v int64) (int64, bool) { return v, true })
}

func (h *Handler) segmentByTime(w http.ResponseWriter, r *http.Request) {
	h.segment(w, r, func(t live.Track, tl timeline.Timeline, v int64) (int64, bool) {
		if t.Video {
			return tl.VideoSegAt(v)
		}
		return tl.AudioSegAt(v, t.FrameSize)
	})
}

// segment serves {v}.m4s, where toSeg maps v to the segment number.
func (h *Handler) segment(w http.ResponseWriter, r *http.Request, toSeg func(live.Track, timeline.Timeline, int64) (int64, bool)) {
	req, err := h.resolve(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rendition := r.PathValue("rendition")
	t, ok := req.rt.Track(rendition)
	v, err := strconv.ParseInt(strings.TrimSuffix(r.PathValue("segment"), ".m4s"), 10, 64)
	if !ok || err != nil || !strings.HasSuffix(r.PathValue("segment"), ".m4s") {
		http.NotFound(w, r)
		return
	}
	n, ok := toSeg(t, req.tl, v)
	if !ok || !req.tl.Available(n, req.now, req.window.Retain) {
		http.NotFound(w, r)
		return
	}
	var buf bytes.Buffer
	if err := h.runtimes.WriteSegment(r.Context(), &buf, req.id, rendition, n); err != nil {
		h.fail(w, r, err)
		return
	}
	h.serve(w, r, mimeType(t), int(req.window.Retain.Seconds()), buf.Bytes())
}

func mimeType(t live.Track) string {
	if t.Video {
		return "video/mp4"
	}
	return "audio/mp4"
}

// serve writes body with Range support (206).
func (*Handler) serve(w http.ResponseWriter, r *http.Request, contentType string, maxAge int, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "max-age="+strconv.Itoa(maxAge))
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// fail maps an error to its status code.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, live.ErrNotFound), errors.Is(err, adbreak.ErrNotFound): // a restart replaced the epoch
		http.NotFound(w, r)
	case errors.Is(err, live.ErrUnavailable):
		h.logger.WarnContext(r.Context(), "live.unavailable", "path", r.URL.Path, "err", err)
		http.Error(w, "segment unavailable", http.StatusServiceUnavailable)
	case errors.Is(err, context.Canceled):
		// The client went away.
	default:
		h.logger.ErrorContext(r.Context(), "live.failed", "path", r.URL.Path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
