package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func ghostWisprSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGhostWisprHandler(t *testing.T) {
	validBody := `{"event_type":"summary_ready","payload":{"session_id":"20260326041629","type":"summary_ready"}}`

	cases := []struct {
		name          string
		secret        string
		method        string
		body          string
		delivery      string
		event         string
		signature     string
		publishErr    error
		wantStatus    int
		wantPublished int
	}{
		{
			name:       "non-POST returns 200",
			method:     "GET",
			wantStatus: 200,
		},
		{
			name:       "missing delivery header",
			method:     "POST",
			body:       validBody,
			event:      "summary_ready",
			wantStatus: 400,
		},
		{
			name:       "missing event header",
			method:     "POST",
			body:       validBody,
			delivery:   "d-123",
			wantStatus: 400,
		},
		{
			name:       "invalid signature",
			secret:     "test-secret",
			method:     "POST",
			body:       validBody,
			delivery:   "d-123",
			event:      "summary_ready",
			signature:  "sha256=invalid",
			wantStatus: 401,
		},
		{
			name:          "valid summary_ready with signing",
			secret:        "test-secret",
			method:        "POST",
			body:          validBody,
			delivery:      "d-123",
			event:         "summary_ready",
			wantStatus:    200,
			wantPublished: 1,
		},
		{
			name:          "valid event without signing (empty secret)",
			method:        "POST",
			body:          `{"event_type":"session_ended","payload":{"session_id":"20260326041629","type":"session_ended"}}`,
			delivery:      "d-456",
			event:         "session_ended",
			wantStatus:    200,
			wantPublished: 1,
		},
		{
			name:       "non-publishable event type",
			method:     "POST",
			body:       `{"event_type":"recording_started","payload":{"session_id":"abc","type":"recording_started"}}`,
			delivery:   "d-789",
			event:      "recording_started",
			wantStatus: 200,
		},
		{
			name:       "missing session_id",
			method:     "POST",
			body:       `{"event_type":"summary_ready","payload":{"type":"summary_ready"}}`,
			delivery:   "d-000",
			event:      "summary_ready",
			wantStatus: 400,
		},
		{
			name:       "event type mismatch (header vs body)",
			method:     "POST",
			body:       `{"event_type":"session_started","payload":{"session_id":"abc","type":"session_started"}}`,
			delivery:   "d-mismatch",
			event:      "summary_ready",
			wantStatus: 400,
		},
		{
			name:          "publish failure returns 503",
			method:        "POST",
			body:          validBody,
			delivery:      "d-fail",
			event:         "summary_ready",
			publishErr:    fmt.Errorf("nats unavailable"),
			wantStatus:    503,
			wantPublished: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &mockPublisher{err: tc.publishErr}
			handler := GhostWisprHandler(tc.secret, pub)

			body := []byte(tc.body)
			req := httptest.NewRequest(tc.method, "/webhook/ghostwispr", strings.NewReader(tc.body))

			if tc.delivery != "" {
				req.Header.Set("X-GhostWispr-Delivery", tc.delivery)
			}
			if tc.event != "" {
				req.Header.Set("X-GhostWispr-Event", tc.event)
			}
			sig := tc.signature
			if sig == "" && tc.secret != "" {
				sig = ghostWisprSign(tc.secret, body)
			}
			if sig != "" {
				req.Header.Set("X-GhostWispr-Signature", sig)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(pub.published) != tc.wantPublished {
				t.Errorf("published = %d, want %d", len(pub.published), tc.wantPublished)
			}
		})
	}
}
