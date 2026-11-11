// Package health serves the readiness probe from named checks.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

// Check is one readiness condition: Run returns why it is not met.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
}

type failure struct {
	Check string `json:"check"`
	Error string `json:"error"`
}

type report struct {
	Status  string    `json:"status"`
	Failing []failure `json:"failing,omitempty"`
}

// Ready answers 200 {"status":"ready"} when every check passes, else 503
// {"status":"unready","failing":[{"check":...,"error":...}]}. The checks run
// in order, within 5 s together.
func Ready(checks ...Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		rep := report{Status: "ready"}
		for _, c := range checks {
			if err := c.Run(ctx); err != nil {
				rep.Failing = append(rep.Failing, failure{Check: c.Name, Error: err.Error()})
			}
		}
		status := http.StatusOK
		if len(rep.Failing) > 0 {
			rep.Status, status = "unready", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(rep) // the prober is gone if this fails
	})
}

// Writable checks that a file can be created in dir.
func Writable(dir string) func(context.Context) error {
	return func(context.Context) error {
		f, err := os.CreateTemp(dir, "readyz-")
		if err != nil {
			return fmt.Errorf("tmp dir not writable: %w", err)
		}
		return errors.Join(f.Close(), os.Remove(f.Name()))
	}
}
