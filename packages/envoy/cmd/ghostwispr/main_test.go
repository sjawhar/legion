package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookHandler(t *testing.T) {
	validBody := `{"id":11,"event_type":"session_ended","payload":{"session_id":"20260326041405","timestamp":"2026-03-26T04:14:58Z","duration":51.05,"type":"session_ended","version":1},"created_at":"2026-03-26T04:14:58Z"}`

	cases := []struct {
		name           string
		method         string
		event          string
		delivery       string
		signature      string
		body           string
		secret         string
		publishErr     error
		wantStatus     int
		wantPublished  bool
		wantBodyPrefix string
	}{
		{
			name:          "valid session_ended event",
			method:        "POST",
			event:         "session_ended",
			delivery:      "gw-del-1",
			body:          validBody,
			wantStatus:    http.StatusOK,
			wantPublished: true,
		},
		{
			name:          "valid summary_ready event",
			method:        "POST",
			event:         "summary_ready",
			delivery:      "gw-del-2",
			body:          `{"id":19,"event_type":"summary_ready","payload":{"session_id":"20260326041629","title":"Test","type":"summary_ready","version":1},"created_at":"2026-03-26T04:17:03Z"}`,
			wantStatus:    http.StatusOK,
			wantPublished: true,
		},
		{
			name:          "non-POST returns 200",
			method:        "GET",
			wantStatus:    http.StatusOK,
			wantPublished: false,
		},
		{
			name:           "missing headers returns 400",
			method:         "POST",
			body:           validBody,
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "missing ghostwispr headers",
		},
		{
			name:           "missing delivery returns 400",
			method:         "POST",
			event:          "session_ended",
			body:           validBody,
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "missing ghostwispr headers",
		},
		{
			name:           "missing event returns 400",
			method:         "POST",
			delivery:       "gw-del-3",
			body:           validBody,
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "missing ghostwispr headers",
		},
		{
			name:           "invalid JSON returns 400",
			method:         "POST",
			event:          "session_ended",
			delivery:       "gw-del-4",
			body:           "not json",
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "invalid json",
		},
		{
			name:           "null JSON returns 400",
			method:         "POST",
			event:          "session_ended",
			delivery:       "gw-del-4b",
			body:           "null",
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "invalid json",
		},
		{
			name:          "unknown event returns 200 (skip)",
			method:        "POST",
			event:         "live_transcript",
			delivery:      "gw-del-5",
			body:          `{"event_type":"live_transcript","payload":{"session_id":"test"}}`,
			wantStatus:    http.StatusOK,
			wantPublished: false,
		},
		{
			name:          "unknown event component_status returns 200 (skip)",
			method:        "POST",
			event:         "component_status",
			delivery:      "gw-del-6",
			body:          `{"event_type":"component_status","payload":{}}`,
			wantStatus:    http.StatusOK,
			wantPublished: false,
		},
		{
			name:          "valid signature accepted",
			method:        "POST",
			event:         "session_ended",
			delivery:      "gw-del-7",
			body:          validBody,
			secret:        "test-secret",
			signature:     sign("test-secret", []byte(validBody)),
			wantStatus:    http.StatusOK,
			wantPublished: true,
		},
		{
			name:           "invalid signature returns 401",
			method:         "POST",
			event:          "session_ended",
			delivery:       "gw-del-8",
			body:           validBody,
			secret:         "test-secret",
			signature:      "sha256=deadbeef",
			wantStatus:     http.StatusUnauthorized,
			wantPublished:  false,
			wantBodyPrefix: "invalid signature",
		},
		{
			name:           "missing signature when required returns 401",
			method:         "POST",
			event:          "session_ended",
			delivery:       "gw-del-9",
			body:           validBody,
			secret:         "test-secret",
			wantStatus:     http.StatusUnauthorized,
			wantPublished:  false,
			wantBodyPrefix: "invalid signature",
		},
		{
			name:          "normalized headers and signature accepted",
			method:        "POST",
			event:         " SESSION.ENDED ",
			delivery:      "  gw-del-9b  ",
			body:          validBody,
			secret:        "test-secret",
			signature:     "  SHA256=" + strings.ToUpper(strings.TrimPrefix(sign("test-secret", []byte(validBody)), "sha256=")) + "  ",
			wantStatus:    http.StatusOK,
			wantPublished: true,
		},
		{
			name:          "no secret skips verification",
			method:        "POST",
			event:         "session_ended",
			delivery:      "gw-del-10",
			body:          validBody,
			secret:        "",
			wantStatus:    http.StatusOK,
			wantPublished: true,
		},
		{
			name:           "mismatched top-level event returns 400",
			method:         "POST",
			event:          "summary_ready",
			delivery:       "gw-del-10b",
			body:           validBody,
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "event mismatch",
		},
		{
			name:           "mismatched payload type returns 400",
			method:         "POST",
			event:          "session_ended",
			delivery:       "gw-del-10c",
			body:           `{"id":11,"event_type":"session_ended","payload":{"session_id":"20260326041405","type":"summary_ready"}}`,
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "event mismatch",
		},
		{
			name:           "missing session id returns 400",
			method:         "POST",
			event:          "session_ended",
			delivery:       "gw-del-10d",
			body:           `{"id":11,"event_type":"session_ended","payload":{"type":"session_ended"}}`,
			wantStatus:     http.StatusBadRequest,
			wantPublished:  false,
			wantBodyPrefix: "missing session_id",
		},
		{
			name:           "publish failure returns 503",
			method:         "POST",
			event:          "session_ended",
			delivery:       "gw-del-11",
			body:           validBody,
			publishErr:     fmt.Errorf("nats connection lost"),
			wantStatus:     http.StatusServiceUnavailable,
			wantPublished:  true,
			wantBodyPrefix: "service unavailable",
		},
		{
			name:           "body too large returns 413",
			method:         "POST",
			body:           strings.Repeat("x", (1<<20)+1),
			wantStatus:     http.StatusRequestEntityTooLarge,
			wantPublished:  false,
			wantBodyPrefix: "body too large",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var published bool
			publish := func(item contracts.Envelope) error {
				published = true
				return tt.publishErr
			}
			handler := webhookHandler(tt.secret, publishFunc(publish))

			req := httptest.NewRequest(tt.method, "/webhook/ghostwispr", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			if tt.event != "" {
				req.Header.Set("X-GhostWispr-Event", tt.event)
			}
			if tt.delivery != "" {
				req.Header.Set("X-GhostWispr-Delivery", tt.delivery)
			}
			if tt.signature != "" {
				req.Header.Set("X-GhostWispr-Signature", tt.signature)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d (body: %s)", rr.Code, tt.wantStatus, rr.Body.String())
			}
			if published != tt.wantPublished {
				t.Fatalf("published: got %v, want %v", published, tt.wantPublished)
			}
			if tt.wantBodyPrefix != "" && !strings.HasPrefix(rr.Body.String(), tt.wantBodyPrefix) {
				t.Fatalf("body: got %q, want prefix %q", rr.Body.String(), tt.wantBodyPrefix)
			}
		})
	}
}

func TestGhostWisprSkip(t *testing.T) {
	supported := []string{"session_started", "session_ended", "summary_ready", " SESSION.STARTED ", "SUMMARY.READY"}
	for _, event := range supported {
		if ghostWisprSkip(event) {
			t.Fatalf("expected %s to NOT be skipped", event)
		}
	}
	unsupported := []string{"live_transcript", "component_status", "unknown", ""}
	for _, event := range unsupported {
		if !ghostWisprSkip(event) {
			t.Fatalf("expected %s to be skipped", event)
		}
	}
}
