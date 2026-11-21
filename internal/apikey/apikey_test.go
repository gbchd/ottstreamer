package apikey_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ottstreamer/internal/apikey"
)

func TestNew(t *testing.T) {
	auth, err := apikey.New([]string{"ci:write:s3cret", "dash:read:peek:with:colons"})
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	h := auth(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = apikey.Name(r.Context()) + ":" + apikey.Role(r.Context())
	}))

	tests := []struct {
		name, method, header string
		status               int
		key                  string
	}{
		{"no header", http.MethodGet, "", http.StatusUnauthorized, ""},
		{"not bearer", http.MethodGet, "Basic s3cret", http.StatusUnauthorized, ""},
		{"unknown key", http.MethodGet, "Bearer nope", http.StatusUnauthorized, ""},
		{"prefix of a key", http.MethodGet, "Bearer s3cre", http.StatusUnauthorized, ""},
		{"write key mutates", http.MethodPost, "Bearer s3cret", http.StatusOK, "ci:write"},
		{"read key reads", http.MethodGet, "Bearer peek:with:colons", http.StatusOK, "dash:read"},
		{"read key heads", http.MethodHead, "Bearer peek:with:colons", http.StatusOK, "dash:read"},
		{"read key mutates", http.MethodDelete, "Bearer peek:with:colons", http.StatusForbidden, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = ""
			req := httptest.NewRequestWithContext(t.Context(), tt.method, "/api/streams", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.status || seen != tt.key {
				t.Errorf("status %d, key %q; want %d, %q", rec.Code, seen, tt.status, tt.key)
			}
		})
	}
}

func TestNew_Invalid(t *testing.T) {
	for _, e := range []string{"nokey", "a:admin:k", ":read:k", "a:read:"} {
		if _, err := apikey.New([]string{e}); err == nil {
			t.Errorf("New(%q) accepted", e)
		}
	}
}
