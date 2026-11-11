package livehttp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"ottstreamer/internal/accesslog"
	"ottstreamer/internal/fault"
)

// urlFaults returns the rules of the URL's per-session fault parameters, nil
// when it has none: one rule per kind in faultOn, with ID url-<kind>.
func urlFaults(r *http.Request) ([]fault.Rule, error) {
	q := r.URL.Query()
	if !slices.ContainsFunc([]string{"fault", "delay", "throttle", "truncate", "reset", "stale"}, q.Has) {
		return nil, nil
	}
	n := map[string]int{}
	for _, k := range []string{"fault", "delay", "throttle", "truncate", "reset", "stale", "faultPercent"} {
		if !q.Has(k) {
			continue
		}
		v, err := strconv.Atoi(q.Get(k))
		if err != nil {
			return nil, fmt.Errorf("%w: %s must be an integer", errBadRequest, k)
		}
		n[k] = v
	}
	kinds, err := override(r, "faultOn", []string{"segment"}, "segment", "init", "playlist", "mpd", "master")
	if err != nil {
		return nil, err
	}
	var renditions []string
	if v := q.Get("faultRenditions"); v != "" {
		renditions = strings.Split(v, ",")
	}
	rules := make([]fault.Rule, 0, len(kinds))
	for _, k := range kinds {
		rule := fault.Rule{
			ID:      "url-" + k,
			Match:   fault.Match{Kind: fault.Kind(k)},
			Percent: n["faultPercent"],
			Mode:    fault.Mode(q.Get("faultMode")),
			Action: fault.Action{
				Status:          n["fault"],
				Delay:           time.Duration(n["delay"]) * time.Millisecond,
				ThrottleKbps:    n["throttle"],
				TruncatePercent: n["truncate"],
				Reset:           n["reset"] != 0,
				Stale:           time.Duration(n["stale"]) * time.Second,
			},
		}
		if rule.Match.Kind != fault.KindMaster && rule.Match.Kind != fault.KindMPD {
			rule.Match.Renditions = renditions
		}
		rules = append(rules, rule)
	}
	rules, err = fault.Normalized(rules)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errBadRequest, err)
	}
	return rules, nil
}

// inject applies the faults hitting t that come before the body: clock
// skew, delay, then reset, gap or status. It returns the action left for
// serve; ok is false when the response is over.
func (h *Handler) inject(w http.ResponseWriter, r *http.Request, req request, t fault.Target) (a fault.Action, ok bool) {
	t.StreamID, t.Epoch = req.id, req.epoch
	inj := h.faults.Inject(r.Context(), req.rules, t)
	if !inj.Hit() {
		return a, true
	}
	a = inj.Action
	accesslog.Add(r.Context(), slog.Group("fault", slog.Any("rules", inj.Rules), slog.Any("actions", a.Names())))
	if a.ClockSkew != 0 {
		w.Header().Set("Date", h.now().Add(a.ClockSkew).UTC().Format(http.TimeFormat))
	}
	if a.Delay > 0 && !sleep(r.Context(), a.Delay) {
		return a, false // the client left
	}
	switch {
	case a.Reset:
		reset(w)
	case a.Gap:
		w.WriteHeader(http.StatusNotFound)
	case a.Status != 0:
		w.WriteHeader(a.Status)
	default:
		return a, true
	}
	return a, false
}

// sleep waits d, and reports false if ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// reset closes the connection without finishing the response. HTTP/2
// connections cannot be hijacked, so there it resets the stream instead.
func reset(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	_ = conn.Close()
}

// truncate announces the whole body, sends percent of it, then closes the
// connection. It ignores Range.
func truncate(w http.ResponseWriter, body []byte, percent int) {
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, bytes.NewReader(body[:len(body)*percent/100])); err != nil {
		return
	}
	// Hijack drops what is still buffered, so flush first.
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	reset(w)
}

// throttled writes at most bps bytes per second, flushing chunks of a tenth
// of a second.
type throttled struct {
	http.ResponseWriter
	ctx context.Context
	bps int
}

func (w throttled) Write(b []byte) (int, error) {
	rc := http.NewResponseController(w.ResponseWriter)
	n := 0
	for len(b) > n {
		m, err := w.ResponseWriter.Write(b[n:min(len(b), n+max(1, w.bps/10))])
		n += m
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return n, err
		}
		if !sleep(w.ctx, time.Duration(m)*time.Second/time.Duration(w.bps)) {
			return n, w.ctx.Err()
		}
	}
	return n, nil
}

// Unwrap lets http.ResponseController reach the real writer.
func (w throttled) Unwrap() http.ResponseWriter { return w.ResponseWriter }
