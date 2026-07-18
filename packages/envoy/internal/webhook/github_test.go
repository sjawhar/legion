package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

	// sub_issues event from a user sender
	subIssuesUser := `{
		"action": "sub_issue_added",
		"parent_issue": {"number": 42, "title": "parent issue"},
		"sub_issue": {"number": 99, "title": "child issue"},
		"sender": {"login": "octocat", "type": "User"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`

	// sub_issues event from a bot sender
	subIssuesBot := `{
		"action": "sub_issue_added",
		"parent_issue": {"number": 42, "title": "parent issue"},
		"sub_issue": {"number": 99, "title": "child issue"},
		"sender": {"login": "linear[bot]", "type": "Bot"},
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
		wantTopics     []string
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
			name:          "bot sender on comment event — publishes 1 envelope",
			method:        "POST",
			body:          botComment,
			delivery:      "d-3",
			event:         "issue_comment",
			secret:        "s",
			wantStatus:    200,
			wantPublished: 1,
		},
		{
			name:          "sub_issues user sender — publishes 1 envelope",
			method:        "POST",
			body:          subIssuesUser,
			delivery:      "d-sub-1",
			event:         "sub_issues",
			secret:        "s",
			wantStatus:    200,
			wantPublished: 1,
			wantTopics:    []string{"notifications.github.sjawhar.legion.issue.42.sub_issue"},
		},
		{
			name:          "sub_issues bot sender — publishes 1 envelope",
			method:        "POST",
			body:          subIssuesBot,
			delivery:      "d-sub-2",
			event:         "sub_issues",
			secret:        "s",
			wantStatus:    200,
			wantPublished: 1,
			wantTopics:    []string{"notifications.github.sjawhar.legion.issue.42.sub_issue"},
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
			handler := GitHubHandler(tc.secret, trigger, "", pub, &mockRecorder{})

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
			if len(tc.wantTopics) > 0 {
				if len(pub.published) != len(tc.wantTopics) {
					t.Fatalf("published topics = %d, want %d", len(pub.published), len(tc.wantTopics))
				}
				for i, wantTopic := range tc.wantTopics {
					if pub.published[i].Topic != wantTopic {
						t.Errorf("published[%d].Topic = %q, want %q", i, pub.published[i].Topic, wantTopic)
					}
				}
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

// TestGitHubHandlerSubIssuesFixture exercises a production-shaped sub_issues webhook payload
// (captured from GitHub's documented schema) end-to-end through GitHubHandler and asserts
// the published topic matches the parent issue's number, not the child's.
func TestGitHubHandlerSubIssuesFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sub_issues_added.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	pub := &mockPublisher{}
	handler := GitHubHandler("s", "@legion", "", pub, &mockRecorder{})

	req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Delivery", "d-fixture-sub-issues")
	req.Header.Set("X-GitHub-Event", "sub_issues")
	req.Header.Set("X-Hub-Signature-256", githubSign("s", body))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if len(pub.published) != 1 {
		t.Fatalf("published = %d, want 1", len(pub.published))
	}
	want := "notifications.github.sjawhar.legion.issue.641.sub_issue"
	if got := pub.published[0].Topic; got != want {
		t.Errorf("Topic = %q, want %q", got, want)
	}
}

// TestGitHubHandlerCIRecordsNotPublishes asserts a check_run webhook records
// into the CIRecorder (once per associated PR) and publishes zero envelopes,
// while a non-CI event publishes as before and does not touch the recorder.
func TestGitHubHandlerCIRecordsNotPublishes(t *testing.T) {
	checkRun := `{
		"action": "completed",
		"check_run": {
			"name": "unit-tests",
			"status": "completed",
			"conclusion": "failure",
			"head_sha": "deadbeef",
			"pull_requests": [{"number": 42}, {"number": 43}]
		},
		"sender": {"login": "github-actions[bot]", "type": "Bot"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`

	t.Run("check_run records per PR, publishes nothing", func(t *testing.T) {
		pub := &mockPublisher{}
		rec := &mockRecorder{}
		handler := GitHubHandler("s", "@legion", "", pub, rec)
		body := []byte(checkRun)
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(checkRun))
		req.Header.Set("X-GitHub-Delivery", "d-ci-1")
		req.Header.Set("X-GitHub-Event", "check_run")
		req.Header.Set("X-Hub-Signature-256", githubSign("s", body))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
		}
		if len(pub.published) != 0 {
			t.Fatalf("published = %d, want 0 (CI is not published raw)", len(pub.published))
		}
		if len(rec.calls) != 2 {
			t.Fatalf("recorder calls = %d, want 2 (one per PR)", len(rec.calls))
		}
		if rec.calls[0].number != "42" || rec.calls[1].number != "43" {
			t.Fatalf("recorded PR numbers = %q, %q", rec.calls[0].number, rec.calls[1].number)
		}
		if rec.calls[0].checkName != "unit-tests" || rec.calls[0].conclusion != "failure" || rec.calls[0].sha != "deadbeef" {
			t.Fatalf("unexpected recorded call: %+v", rec.calls[0])
		}
	})

	t.Run("recorder error returns 503", func(t *testing.T) {
		pub := &mockPublisher{}
		rec := &mockRecorder{err: fmt.Errorf("kv down")}
		handler := GitHubHandler("s", "@legion", "", pub, rec)
		body := []byte(checkRun)
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(checkRun))
		req.Header.Set("X-GitHub-Delivery", "d-ci-2")
		req.Header.Set("X-GitHub-Event", "check_run")
		req.Header.Set("X-Hub-Signature-256", githubSign("s", body))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 503 {
			t.Fatalf("status = %d, want 503 on recorder failure", rr.Code)
		}
	})

	t.Run("non-CI event publishes and does not record", func(t *testing.T) {
		push := `{"ref": "refs/heads/main", "sender": {"login": "octocat", "type": "User"}, "repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}}`
		pub := &mockPublisher{}
		rec := &mockRecorder{}
		handler := GitHubHandler("s", "@legion", "", pub, rec)
		body := []byte(push)
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(push))
		req.Header.Set("X-GitHub-Delivery", "d-push-1")
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", githubSign("s", body))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status = %d, want 200", rr.Code)
		}
		if len(pub.published) != 1 {
			t.Fatalf("published = %d, want 1", len(pub.published))
		}
		if len(rec.calls) != 0 {
			t.Fatalf("recorder calls = %d, want 0 for non-CI event", len(rec.calls))
		}
	})
}

func TestGitHubHandlerFiltersReviewerVerdicts(t *testing.T) {
	const (
		secret        = "s"
		reviewerAppID = "12345"
	)
	checkRun := func(name, appID string) string {
		return fmt.Sprintf(`{
			"action": "completed",
			"check_run": {
				"name": %q,
				"status": "completed",
				"conclusion": "success",
				"head_sha": "deadbeef",
				"app": {"id": %s},
				"pull_requests": [{"number": 42}]
			},
			"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
		}`, name, appID)
	}
	post := func(t *testing.T, handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", "d-reviewer-verdict")
		req.Header.Set("X-GitHub-Event", "check_run")
		req.Header.Set("X-Hub-Signature-256", githubSign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
		}
		return rr
	}

	for _, name := range []string{"tester", "architect"} {
		t.Run("right-App "+name+" verdict is recorded", func(t *testing.T) {
			pub := &mockPublisher{}
			rec := &mockRecorder{}
			handler := GitHubHandler(secret, "@legion", reviewerAppID, pub, rec)
			post(t, handler, checkRun(name, reviewerAppID))

			if len(pub.published) != 0 {
				t.Fatalf("published = %d, want 0", len(pub.published))
			}
			if len(rec.calls) != 1 {
				t.Fatalf("recorder calls = %d, want 1", len(rec.calls))
			}
			if got := rec.calls[0]; got.checkName != name || got.conclusion != "success" {
				t.Fatalf("unexpected recorded verdict: %+v", got)
			}
		})
	}

	t.Run("wrong-App tester verdict is dropped", func(t *testing.T) {
		pub := &mockPublisher{}
		rec := &mockRecorder{}
		handler := GitHubHandler(secret, "@legion", reviewerAppID, pub, rec)
		post(t, handler, checkRun("tester", "98765"))
		if len(rec.calls) != 0 {
			t.Fatalf("recorder calls = %d, want 0 for wrong-App tester", len(rec.calls))
		}
	})

	t.Run("missing reviewer App ID drops bare verdict", func(t *testing.T) {
		pub := &mockPublisher{}
		rec := &mockRecorder{}
		handler := GitHubHandler(secret, "@legion", "", pub, rec)
		post(t, handler, checkRun("tester", reviewerAppID))
		if len(rec.calls) != 0 {
			t.Fatalf("recorder calls = %d, want 0 without a reviewer App ID", len(rec.calls))
		}
	})

	t.Run("ordinary CI check is recorded from another App", func(t *testing.T) {
		pub := &mockPublisher{}
		rec := &mockRecorder{}
		handler := GitHubHandler(secret, "@legion", reviewerAppID, pub, rec)
		post(t, handler, checkRun("unit-tests", "98765"))
		if len(rec.calls) != 1 {
			t.Fatalf("recorder calls = %d, want 1", len(rec.calls))
		}
		if got := rec.calls[0].checkName; got != "unit-tests" {
			t.Fatalf("recorded check name = %q, want unit-tests", got)
		}
	})
}
