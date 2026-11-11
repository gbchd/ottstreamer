// Package apikey checks Bearer API keys given as "name:role:key", where role
// is read (GET and HEAD only) or write (everything).
package apikey

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
)

type key struct {
	name  string
	write bool
	hash  [sha256.Size]byte
}

type nameKey struct{}

// New parses entries and returns a middleware that answers 401 for a missing
// or unknown key and 403 for a read key on a mutation. Keys never appear in
// errors; the name of the matched key is in the request context.
func New(entries []string) (func(http.Handler) http.Handler, error) {
	keys := make([]key, 0, len(entries))
	for i, e := range entries {
		parts := strings.SplitN(e, ":", 3)
		if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
			return nil, fmt.Errorf("api key %d: want name:role:key", i+1)
		}
		if parts[1] != "read" && parts[1] != "write" {
			return nil, fmt.Errorf("api key %q: role must be read or write", parts[0])
		}
		keys = append(keys, key{name: parts[0], write: parts[1] == "write", hash: sha256.Sum256([]byte(parts[2]))})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := match(keys, r.Header.Get("Authorization"))
			switch {
			case k == nil:
				w.Header().Set("WWW-Authenticate", "Bearer")
				deny(w, http.StatusUnauthorized, "missing or unknown API key")
			case !k.write && r.Method != http.MethodGet && r.Method != http.MethodHead:
				deny(w, http.StatusForbidden, "read-only API key")
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nameKey{}, k.name)))
			}
		})
	}, nil
}

// match compares the presented key with every key in constant time. Hashing
// first makes the comparison independent of key lengths too.
func match(keys []key, authorization string) *key {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok || token == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(token))
	var found *key
	for i := range keys {
		if subtle.ConstantTimeCompare(keys[i].hash[:], sum[:]) == 1 {
			found = &keys[i]
		}
	}
	return found
}

func deny(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "{\"error\":%q}\n", msg) // msg is constant ASCII, so %q is valid JSON
}

// Name returns the name of the API key that authenticated the request, or "".
func Name(ctx context.Context) string {
	name, _ := ctx.Value(nameKey{}).(string)
	return name
}
