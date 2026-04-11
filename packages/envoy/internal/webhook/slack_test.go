package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func slackSign(secret string, timestamp string, body []byte) string {
	basestring := "v0:" + timestamp + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(basestring))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestSlackHandler(t *testing.T) {
	urlVerification := `{"type":"url_verification","challenge":"test-challenge-123"}`

	validEventCallback := `{
		"type": "event_callback",
		"event_id": "ev-1",
		"team_id": "T09FRELLTS8",
		"event": {
			"type": "message",
			"channel": "C0A0DHVU8HE",
			"user": "U12345",
			"text": "hello world"
		}
	}`

	emptyEventID := `{
		"type": "event_callback",
		"event_id": "",
		"team_id": "T09FRELLTS8",
		"event": {"type": "message", "channel": "C0A0DHVU8HE"}
	}`

	nonEventCallback := `{
		"type": "app_rate_limited",
		"team_id": "T09FRELLTS8"
	}`

	appMention := `{
		"type": "event_callback",
		"event_id": "ev-2",
		"team_id": "T09FRELLTS8",
		"event": {
			"type": "app_mention",
			"channel": "C0A0DHVU8HE",
			"user": "U12345",
			"text": "@legion help"
		}
	}`

	cases := []struct {
		name          string
		method        string
		body          string
		secret        string
		timestamp     string
		signature     string
		publishErr    error
		wantStatus    int
		wantPublished int
		wantBody      string // if non-empty, response body must contain this
	}{
		{
			name:       "non-POST returns 200",
			method:     "GET",
			secret:     "s",
			wantStatus: 200,
		},
		{
			name:       "url_verification echoes challenge before signature check",
			method:     "POST",
			body:       urlVerification,
			secret:     "s",
			wantStatus: 200,
			wantBody:   "test-challenge-123",
		},
		{
			name:       "invalid signature",
			method:     "POST",
			body:       validEventCallback,
			secret:     "s",
			timestamp:  "1234567890",
			signature:  "v0=invalid",
			wantStatus: 401,
		},
		{
			name:          "valid event_callback with event_id publishes",
			method:        "POST",
			body:          validEventCallback,
			secret:        "s",
			wantStatus:    200,
			wantPublished: 1,
		},
		{
			name:       "event_callback with empty event_id — no publish",
			method:     "POST",
			body:       emptyEventID,
			secret:     "s",
			wantStatus: 200,
		},
		{
			name:       "non-event_callback type — no publish",
			method:     "POST",
			body:       nonEventCallback,
			secret:     "s",
			wantStatus: 200,
		},
		{
			name:          "publish failure returns 503",
			method:        "POST",
			body:          validEventCallback,
			secret:        "s",
			publishErr:    fmt.Errorf("nats down"),
			wantStatus:    503,
			wantPublished: 1,
		},
		{
			name:          "app_mention event type",
			method:        "POST",
			body:          appMention,
			secret:        "s",
			wantStatus:    200,
			wantPublished: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &mockPublisher{err: tc.publishErr}
			handler := SlackHandler(tc.secret, pub)

			body := []byte(tc.body)
			req := httptest.NewRequest(tc.method, "/webhook/slack", strings.NewReader(tc.body))

			nowTS := strconv.FormatInt(time.Now().Unix(), 10)
			ts := tc.timestamp
			if ts == "" {
				ts = nowTS
			}
			req.Header.Set("X-Slack-Request-Timestamp", ts)
			sig := tc.signature
			if sig == "" && tc.secret != "" && len(body) > 0 {
				sig = slackSign(tc.secret, ts, body)
			}
			if sig != "" {
				req.Header.Set("X-Slack-Signature", sig)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(pub.published) != tc.wantPublished {
				t.Errorf("published = %d, want %d", len(pub.published), tc.wantPublished)
			}
			if tc.wantBody != "" {
				var parsed map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err == nil {
					if challenge, ok := parsed["challenge"].(string); ok {
						if challenge != tc.wantBody {
							t.Errorf("challenge = %q, want %q", challenge, tc.wantBody)
						}
					} else {
						t.Errorf("response body missing challenge field: %s", rec.Body.String())
					}
				} else if !strings.Contains(rec.Body.String(), tc.wantBody) {
					t.Errorf("response body %q does not contain %q", rec.Body.String(), tc.wantBody)
				}
			}
		})
	}
}
