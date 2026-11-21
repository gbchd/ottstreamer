package api_test

import (
	"net/http"
	"strings"
	"testing"

	"ottstreamer/internal/apikey"
)

func TestGetMe(t *testing.T) {
	auth, err := apikey.New([]string{"qa:write:qa-secret", "dash:read:peek"})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := serve(t, auth)
	tests := []struct{ name, key, want string }{
		{"write key", "qa-secret", `{"name":"qa","role":"write"}`},
		{"read key", "peek", `{"name":"dash","role":"read"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := do(t, srv, http.MethodGet, "/api/me", "", "Authorization", "Bearer "+tt.key)
			expect(t, r, http.StatusOK)
			if strings.TrimSpace(r.raw) != tt.want {
				t.Errorf("me = %s, want %s", r.raw, tt.want)
			}
		})
	}
	expect(t, do(t, srv, http.MethodGet, "/api/me", ""), http.StatusUnauthorized)
}
