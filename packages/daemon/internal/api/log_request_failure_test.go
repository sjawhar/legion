package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLogRequestFailureLogsWarnOnceTheRequestItselfHasEnded(t *testing.T) {
	for _, c := range []struct {
		name          string
		err           error
		cancelRequest bool
		expireRequest bool
		beginStop     bool
		wantLevel     string
	}{
		{
			name:          "a request whose client went away, whose call failed with that cancellation, on a running daemon",
			err:           context.Canceled,
			cancelRequest: true,
			wantLevel:     "WARN",
		},
		{
			name:          "a request whose own deadline passed, whose call failed with it, on a running daemon",
			err:           context.DeadlineExceeded,
			expireRequest: true,
			wantLevel:     "WARN",
		},
		{
			name:          "a request whose client went away, whose call failed of its own, on a running daemon",
			err:           errors.New("connection refused"),
			cancelRequest: true,
			wantLevel:     "ERROR",
		},
		{
			name:          "a request whose client went away, whose call failed with an inner deadline that passed as it did",
			err:           context.DeadlineExceeded,
			cancelRequest: true,
			wantLevel:     "ERROR",
		},
		{
			name:      "a live request whose call failed with something else's cancellation, on a running daemon",
			err:       context.Canceled,
			wantLevel: "ERROR",
		},
		{
			name:      "a live request on a daemon whose stop has begun",
			err:       errors.New("connection refused"),
			beginStop: true,
			wantLevel: "INFO",
		},
		{
			name:      "a live request on a running daemon",
			err:       errors.New("connection refused"),
			wantLevel: "ERROR",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			stopping, beginStop := context.WithCancel(context.Background())
			defer beginStop()
			if c.beginStop {
				beginStop()
			}
			testServer := &server{
				log:      slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
				stopping: stopping,
			}
			request := httptest.NewRequest(http.MethodGet, "/legion/v1/operator/claims", nil)
			requestContext, hangUp := context.WithCancel(request.Context())
			defer hangUp()
			switch {
			case c.cancelRequest:
				hangUp()
			case c.expireRequest:
				// A deadline already in the past leaves the request's own context ended with
				// DeadlineExceeded, with no wait for the clock.
				var expired context.CancelFunc
				requestContext, expired = context.WithDeadline(request.Context(), time.Now().Add(-time.Minute))
				defer expired()
			}
			request = request.WithContext(requestContext)

			testServer.logRequestFailure(request, c.err, "api: read the claims")

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
			if record.Error != c.err.Error() {
				t.Errorf("error attribute = %q, want %q", record.Error, c.err.Error())
			}
		})
	}
}
