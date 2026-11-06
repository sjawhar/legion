package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogReadFailureLogsInfoWhenNobodyIsLeftToActOnIt(t *testing.T) {
	for _, c := range []struct {
		name          string
		cancelRequest bool
		beginStop     bool
		wantLevel     string
	}{
		{name: "a request whose client went away, on a running daemon", cancelRequest: true, wantLevel: "INFO"},
		{name: "a live request on a daemon whose stop has begun", beginStop: true, wantLevel: "INFO"},
		{name: "a live request on a running daemon", wantLevel: "ERROR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			stopping, beginStop := context.WithCancel(context.Background())
			if c.beginStop {
				beginStop()
			} else {
				defer beginStop()
			}
			testServer := &server{
				log:      slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
				stopping: stopping,
			}
			request := httptest.NewRequest(http.MethodGet, "/legion/v1/operator/claims", nil)
			requestContext, hangUp := context.WithCancel(request.Context())
			if c.cancelRequest {
				hangUp()
			} else {
				defer hangUp()
			}
			request = request.WithContext(requestContext)

			testServer.logReadFailure(request, "api: read the claims", "error", "connection refused")

			var record struct {
				Level   string `json:"level"`
				Message string `json:"msg"`
				Error   string `json:"error"`
			}
			if err := json.Unmarshal(bytes.TrimSpace(logged.Bytes()), &record); err != nil {
				t.Fatalf("the helper logged %q, which is not one JSON record: %v", logged.String(), err)
			}
			if record.Level != c.wantLevel {
				t.Errorf("level = %s, want %s", record.Level, c.wantLevel)
			}
			if record.Message != "api: read the claims" {
				t.Errorf("message = %q, want %q", record.Message, "api: read the claims")
			}
			if record.Error != "connection refused" {
				t.Errorf("error attribute = %q, want %q", record.Error, "connection refused")
			}
		})
	}
}
