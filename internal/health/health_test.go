package health_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"ottstreamer/internal/health"
)

func TestReady(t *testing.T) {
	ok := health.Check{Name: "tmp", Run: health.Writable(t.TempDir())}
	bad := health.Check{Name: "ffmpeg", Run: func(context.Context) error { return errors.New("not found") }}
	missing := health.Check{Name: "tmp", Run: health.Writable(filepath.Join(t.TempDir(), "gone"))}
	tests := []struct {
		name   string
		checks []health.Check
		status int
		body   string
	}{
		{"all pass", []health.Check{ok}, 200, `{"status":"ready"}`},
		{"one fails", []health.Check{ok, bad}, 503, `{"status":"unready","failing":[{"check":"ffmpeg","error":"not found"}]}`},
		{"tmp missing", []health.Check{missing}, 503, `"check":"tmp","error":"tmp dir not writable`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			health.Ready(tt.checks...).ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), "GET", "/readyz", nil))
			if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.body) {
				t.Fatalf("got %d %s, want %d containing %s", w.Code, w.Body, tt.status, tt.body)
			}
		})
	}
}
