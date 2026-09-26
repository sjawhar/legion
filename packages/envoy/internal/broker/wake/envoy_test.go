package wake

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// capturingHandler records every slog record it is asked to handle, so a test can assert that
// Notify logged a warning without caring about the exact message text.
type capturingHandler struct {
	records *[]slog.Record
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	*h.records = append(*h.records, r)
	return nil
}

func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h capturingHandler) WithGroup(string) slog.Handler { return h }

func TestNotifySendsTargetSessionPayloadAndBearerToken(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody sendBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	e := Envoy{URL: srv.URL, Token: "tok", HTTP: srv.Client()}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if gotMethod != http.MethodPost {
		t.Fatalf("method: got %q, want POST", gotMethod)
	}
	if gotPath != "/v1/messages/send" {
		t.Fatalf("path: got %q, want /v1/messages/send", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("authorization: got %q, want %q", gotAuth, "Bearer tok")
	}
	if gotBody.TargetSession != "sess-123" {
		t.Fatalf("target_session: got %q, want %q", gotBody.TargetSession, "sess-123")
	}

	var payload secretRequestPayload
	if err := json.Unmarshal([]byte(gotBody.Payload), &payload); err != nil {
		t.Fatalf("payload is not JSON-encoded text: %v", err)
	}
	if payload.Type != "secret-request" {
		t.Fatalf("payload.type: got %q, want %q", payload.Type, "secret-request")
	}
	if payload.RequestID != "req-456" {
		t.Fatalf("payload.request_id: got %q, want %q", payload.RequestID, "req-456")
	}
	if payload.State != "approved" {
		t.Fatalf("payload.state: got %q, want %q", payload.State, "approved")
	}
}

func TestNotifySwallowsServerErrorAndLogsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var records []slog.Record
	previous := slog.Default()
	slog.SetDefault(slog.New(capturingHandler{records: &records}))
	defer slog.SetDefault(previous)

	e := Envoy{URL: srv.URL, Token: "tok", HTTP: srv.Client()}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the 500 response, got none")
	}
	if records[0].Level != slog.LevelWarn {
		t.Fatalf("level: got %v, want Warn", records[0].Level)
	}
}

func TestNotifySwallowsNotFoundAndLogsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	var records []slog.Record
	previous := slog.Default()
	slog.SetDefault(slog.New(capturingHandler{records: &records}))
	defer slog.SetDefault(previous)

	e := Envoy{URL: srv.URL, Token: "tok", HTTP: srv.Client()}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the 404 (no live session) response, got none")
	}
}

func TestNotifySwallowsUnreachableServerAndLogsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	srv.Close() // guarantees connection failure below

	var records []slog.Record
	previous := slog.Default()
	slog.SetDefault(slog.New(capturingHandler{records: &records}))
	defer slog.SetDefault(previous)

	e := Envoy{URL: srv.URL, Token: "tok", HTTP: srv.Client()}
	e.Notify(context.Background(), "sess-123", "req-456", "approved")

	if len(records) == 0 {
		t.Fatal("expected a warning to be logged for the connection failure, got none")
	}
}
