package webui_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ottstreamer/internal/webui"
)

func TestRegister(t *testing.T) {
	mux := http.NewServeMux()
	webui.Register(mux)
	tests := []struct {
		name, path string
		status     int
		typ, body  string
	}{
		{"root redirects", "/", http.StatusFound, "", ""},
		{"index", "/ui/", http.StatusOK, "text/html", `<script type="importmap">`},
		{"stream page", "/ui/streams/demo", http.StatusOK, "text/html", `<script type="importmap">`},
		{"app module", "/ui/app.js", http.StatusOK, "text/javascript", "render("},
		{"vendored module", "/ui/vendor/hls.min.mjs", http.StatusOK, "text/javascript", ""},
		{"stylesheet", "/ui/style.css", http.StatusOK, "text/css", ""},
		{"missing file", "/ui/nope.js", http.StatusNotFound, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil))
			body, _ := io.ReadAll(rec.Body)
			if rec.Code != tt.status || !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.typ) || !strings.Contains(string(body), tt.body) {
				t.Errorf("GET %s = %d %q, want %d %q containing %q", tt.path, rec.Code, rec.Header().Get("Content-Type"), tt.status, tt.typ, tt.body)
			}
			if tt.status == http.StatusFound && rec.Header().Get("Location") != "/ui/" {
				t.Errorf("Location = %q", rec.Header().Get("Location"))
			}
		})
	}
}

// The policy must allow the import map by its hash, or no module loads.
func TestRegister_CSP(t *testing.T) {
	mux := http.NewServeMux()
	webui.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ui/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self' 'sha256-") || !strings.Contains(csp, "worker-src 'self' blob:") {
		t.Errorf("CSP = %q", csp)
	}
}
