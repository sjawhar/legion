package api

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// readHeaderTimeout bounds how long a client may take to send its request headers; without it a
// slow-header client holds a listener slot for as long as it likes.
const readHeaderTimeout = 10 * time.Second

// NewServer builds the daemon's HTTP server on bind:port — the configured address only, never
// every interface. The caller owns its lifecycle (ListenAndServe, Shutdown).
func NewServer(bind string, port int, src StateSource) *http.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /legion/v1/state", func(w http.ResponseWriter, r *http.Request) {
		state, err := src.State(r.Context())
		if err != nil {
			slog.Error("api: state source failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "state unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, state)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// The catch-all answers every unrouted request, including a known path asked for with the
	// wrong method: the daemon's API is read by one plugin over two GETs, so "no route" is the
	// whole story a caller needs.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no route"})
	})

	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(port)),
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		slog.Error("api: encode response", "error", err)
		http.Error(w, `{"error":"encode failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(encoded, '\n')); err != nil {
		slog.Debug("api: write response", "error", err)
	}
}
