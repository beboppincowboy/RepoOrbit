package platform

import (
	"context"
	"net/http"
)

type Pinger interface{ Ping(context.Context) error }

// Health serves GET /live (always 200 "ok") and GET /ready (200 "ready", or 503 when the database ping fails
// within the request deadline). No authentication.
func Health(db Pinger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := WithRequestDeadline(r.Context())
		defer cancel()
		if db.Ping(ctx) != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready"))
	})
	return mux
}
