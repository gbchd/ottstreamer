package cors_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ottstreamer/internal/cors"
)

func TestNew(t *testing.T) {
	h := cors.New([]string{"https://ui.example"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // reached only when CORS lets the request through
	}))
	tests := []struct {
		name, method, path, origin string
		status                     int
		allowOrigin, allowMethods  string
	}{
		{"playback get", http.MethodGet, "/live/demo/master.m3u8", "https://player.example", http.StatusTeapot, "*", ""},
		{"playback preflight", http.MethodOptions, "/live/demo/720p/1.m4s", "https://player.example", http.StatusNoContent, "*", "GET, HEAD, OPTIONS"},
		{"time preflight", http.MethodOptions, "/api/time", "https://player.example", http.StatusNoContent, "*", "GET, HEAD, OPTIONS"},
		{"api allowed origin", http.MethodGet, "/api/streams", "https://ui.example", http.StatusTeapot, "https://ui.example", ""},
		{"api preflight allowed", http.MethodOptions, "/api/streams/demo", "https://ui.example", http.StatusNoContent, "https://ui.example", "GET, POST, PUT, DELETE, OPTIONS"},
		{"api preflight other origin", http.MethodOptions, "/api/streams", "https://evil.example", http.StatusNoContent, "", ""},
		{"api other origin", http.MethodGet, "/api/streams", "https://evil.example", http.StatusTeapot, "", ""},
		{"no cors elsewhere", http.MethodGet, "/healthz", "https://ui.example", http.StatusTeapot, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			req.Header.Set("Origin", tt.origin)
			if tt.method == http.MethodOptions {
				req.Header.Set("Access-Control-Request-Method", http.MethodGet)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			got := rec.Header()
			if rec.Code != tt.status || got.Get("Access-Control-Allow-Origin") != tt.allowOrigin ||
				got.Get("Access-Control-Allow-Methods") != tt.allowMethods {
				t.Errorf("status %d, headers %v", rec.Code, got)
			}
			if tt.status == http.StatusNoContent && tt.allowOrigin != "" && got.Get("Access-Control-Max-Age") != "86400" {
				t.Errorf("Max-Age = %q", got.Get("Access-Control-Max-Age"))
			}
		})
	}
}
