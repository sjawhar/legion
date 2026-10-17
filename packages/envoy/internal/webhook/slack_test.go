package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
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
		"team_id": "T01234567",
		"event": {
			"type": "message",
			"channel": "C01234567",
			"user": "U01234567",
			"text": "hello world"
		}
	}`

	emptyEventID := `{
		"type": "event_callback",
		"event_id": "",
		"team_id": "T01234567",
		"event": {"type": "message", "channel": "C01234567"}
	}`

	nonEventCallback := `{
		"type": "app_rate_limited",
		"team_id": "T01234567"
	}`

	appMention := `{
		"type": "event_callback",
		"event_id": "ev-2",
		"team_id": "T01234567",
		"event": {
			"type": "app_mention",
			"channel": "C01234567",
			"user": "U01234567",
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
			name:          "a publish refused as too large returns 422",
			method:        "POST",
			body:          validEventCallback,
			secret:        "s",
			publishErr:    fmt.Errorf("publish: %w", bus.ErrTooLarge),
			wantStatus:    422,
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

// A bot's mention of the app is acknowledged and dropped, never published, and the drop is logged
// naming the channel, the bot and Slack's event id: a controller answering another bot's mention
// could answer a bot that answers it. A person's mention in the same delivery shape publishes.
func TestSlackHandlerDropsABotsMention(t *testing.T) {
	mention := func(extra string) string {
		return `{"type":"event_callback","event_id":"Ev0BOT1","team_id":"T01234567","event":{"type":"app_mention","channel":"C01234567","user":"U01234567","text":"<@U0LEGION00> hi","ts":"1700000000.000100"` + extra + `}}`
	}
	for _, tc := range []struct {
		name, body    string
		wantPublished int
		wantLog       string
	}{
		{"a person's mention", mention(""), 1, ""},
		{"a mention carrying bot_id", mention(`,"bot_id":"B0OTHER00"`), 0, "slack app_mention from a bot dropped team=T01234567 channel=C01234567 bot_id=B0OTHER00 subtype= event_id=Ev0BOT1"},
		{"a bot_message mention", mention(`,"subtype":"bot_message"`), 0, "slack app_mention from a bot dropped team=T01234567 channel=C01234567 bot_id= subtype=bot_message event_id=Ev0BOT1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			log.SetOutput(&logged)
			defer log.SetOutput(os.Stderr)
			pub := &mockPublisher{}
			req := httptest.NewRequest("POST", "/webhook/slack", strings.NewReader(tc.body))
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			req.Header.Set("X-Slack-Request-Timestamp", ts)
			req.Header.Set("X-Slack-Signature", slackSign("s", ts, []byte(tc.body)))
			rec := httptest.NewRecorder()
			SlackHandler("s", pub).ServeHTTP(rec, req)
			if rec.Code != 200 || len(pub.published) != tc.wantPublished {
				t.Fatalf("status %d, published %d; want 200 and %d", rec.Code, len(pub.published), tc.wantPublished)
			}
			if tc.wantLog == "" {
				if strings.Contains(logged.String(), "dropped") {
					t.Errorf("log %q drops a person's mention", logged.String())
				}
			} else if !strings.Contains(logged.String(), tc.wantLog) {
				t.Errorf("log %q, want it to contain %q", logged.String(), tc.wantLog)
			}
		})
	}
}
