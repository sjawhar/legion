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

func TestGitHubHandler(t *testing.T) {
	// Minimal issue_comment payload (no mention)
	issueCommentNoMention := `{
		"action": "created",
		"issue": {"number": 42, "title": "test issue"},
		"comment": {"body": "just a comment", "user": {"login": "octocat"}},
		"sender": {"login": "octocat", "type": "User"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`

	// issue_comment with @legion mention
	issueCommentWithMention := `{
		"action": "created",
		"issue": {"number": 42, "title": "test issue"},
		"comment": {"body": "hey @legion please fix this", "user": {"login": "octocat"}},
		"sender": {"login": "octocat", "type": "User"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`

	// Bot sender on comment event
	botComment := `{
		"action": "created",
		"issue": {"number": 42, "title": "test issue"},
		"comment": {"body": "automated response", "user": {"login": "github-actions[bot]"}},
		"sender": {"login": "github-actions[bot]", "type": "Bot"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`

	// Push event
	pushEvent := `{
		"ref": "refs/heads/main",
		"sender": {"login": "octocat", "type": "User"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`

	// Custom mention trigger
	issueCommentCustomMention := `{
		"action": "created",
		"issue": {"number": 7, "title": "custom"},
		"comment": {"body": "hey @mybot do this", "user": {"login": "dev"}},
		"sender": {"login": "dev", "type": "User"},
		"repository": {"name": "widgets", "owner": {"login": "acme"}, "full_name": "acme/widgets"}
	}`

	cases := []struct {
		name           string
		method         string
		body           string
		delivery       string
		event          string
		secret         string
		signature      string
		mentionTrigger string
		publishErr     error
		wantStatus     int
		wantPublished  int
	}{
		{
			name:       "non-POST returns 200",
			method:     "GET",
			secret:     "s",
			wantStatus: 200,
		},
		{
			name:       "missing delivery header",
			method:     "POST",
			body:       issueCommentNoMention,
			event:      "issue_comment",
			secret:     "s",
			wantStatus: 400,
		},
		{
			name:       "missing event header",
			method:     "POST",
			body:       issueCommentNoMention,
			delivery:   "d-1",
			secret:     "s",
			wantStatus: 400,
		},
		{
			name:       "invalid signature",
			method:     "POST",
			body:       issueCommentNoMention,
			delivery:   "d-1",
			event:      "issue_comment",
			secret:     "s",
			signature:  "sha256=invalid",
			wantStatus: 401,
		},
		{
			name:          "valid issue_comment no mention",
			method:        "POST",
			body:          issueCommentNoMention,
			delivery:      "d-1",
			event:         "issue_comment",
			secret:        "s",
			wantStatus:    200,
			wantPublished: 1,
		},
		{
			name:          "valid issue_comment with @legion mention — 3 envelopes",
			method:        "POST",
			body:          issueCommentWithMention,
			delivery:      "d-2",
			event:         "issue_comment",
			secret:        "s",
			wantStatus:    200,
			wantPublished: 3,
		},
		{
			name:       "bot sender on comment event — skipped",
			method:     "POST",
			body:       botComment,
			delivery:   "d-3",
			event:      "issue_comment",
			secret:     "s",
			wantStatus: 200,
		},
		{
			name:          "push event — publishes 1 envelope",
			method:        "POST",
			body:          pushEvent,
			delivery:      "d-4",
			event:         "push",
			secret:        "s",
			wantStatus:    200,
			wantPublished: 1,
		},
		{
			name:          "publish failure returns 503",
			method:        "POST",
			body:          pushEvent,
			delivery:      "d-5",
			event:         "push",
			secret:        "s",
			publishErr:    fmt.Errorf("nats down"),
			wantStatus:    503,
			wantPublished: 1,
		},
		{
			name:           "custom mention trigger fan-out",
			method:         "POST",
			body:           issueCommentCustomMention,
			delivery:       "d-6",
			event:          "issue_comment",
			secret:         "s",
			mentionTrigger: "@mybot",
			wantStatus:     200,
			wantPublished:  3,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pub := &mockPublisher{err: tc.publishErr}
			trigger := tc.mentionTrigger
			if trigger == "" {
				trigger = "@legion"
			}
			handler := GitHubHandler(tc.secret, trigger, pub)

			body := []byte(tc.body)
			req := httptest.NewRequest(tc.method, "/webhook/github", strings.NewReader(tc.body))
			if tc.delivery != "" {
				req.Header.Set("X-GitHub-Delivery", tc.delivery)
			}
			if tc.event != "" {
				req.Header.Set("X-GitHub-Event", tc.event)
			}
			sig := tc.signature
			if sig == "" && tc.secret != "" && len(body) > 0 {
				sig = githubSign(tc.secret, body)
			}
			if sig != "" {
				req.Header.Set("X-Hub-Signature-256", sig)
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

// githubSign computes HMAC SHA256 signature matching GitHub's format.
func githubSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
