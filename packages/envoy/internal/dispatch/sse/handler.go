package sse

import (
	"net/http"
	"time"
)

const defaultHeartbeat = 15 * time.Second

// HandlerFor returns an http.HandlerFunc that streams hub events to a single
// authenticated client. The caller passes the user's login + watched-repo
// slugs; the hub uses the watched set to filter the GitHub event firehose.
func HandlerFor(hub *Hub, login string, watched []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		id, client := hub.AddClient(login, watched)
		defer hub.RemoveClient(id)

		// Initial connect event.
		if _, err := w.Write([]byte("event: connected\ndata: {}\n\n")); err != nil {
			return
		}
		flusher.Flush()

		heartbeat := time.NewTicker(defaultHeartbeat)
		defer heartbeat.Stop()
		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-client.Messages:
				if !ok {
					return
				}
				if _, err := w.Write(msg); err != nil {
					return
				}
				flusher.Flush()
			case <-heartbeat.C:
				if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}
