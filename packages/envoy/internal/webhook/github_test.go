package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/contracts"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestGithubPullRequestHeadRequiresStrictPRNumber(t *testing.T) {
	for _, test := range []struct {
		name       string
		number     any
		wantNumber string
		wantOK     bool
	}{
		{name: "integer number", number: float64(42), wantNumber: "42", wantOK: true},
		{name: "decimal digit string", number: "42", wantNumber: "42", wantOK: true},
		{name: "fractional number", number: 42.5},
		{name: "non-numeric string", number: "abc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := map[string]any{
				"action": "opened",
				"number": test.number,
				"repository": map[string]any{
					"name":  "example-repo",
					"owner": map[string]any{"login": "example-org"},
				},
				"pull_request": map[string]any{
					"head": map[string]any{"sha": "abcdef"},
				},
			}
			_, _, number, _, _, ok := githubPullRequestHead("pull_request", payload)
			if number != test.wantNumber || ok != test.wantOK {
				t.Fatalf("githubPullRequestHead() = number %q, ok %t; want number %q, ok %t", number, ok, test.wantNumber, test.wantOK)
			}
		})
	}
}

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

	checkRun := `{
		"action": "completed",
		"check_run": {
			"id": 987654321,
			"name": "unit-tests",
			"status": "completed",
			"conclusion": "failure",
			"head_sha": "deadbeef",
			"pull_requests": [{"number": 42}]
		},
		"sender": {"login": "github-actions[bot]", "type": "Bot"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
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
		wantRecorded   int
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
			name:          "check_run records without publishing a raw observation",
			method:        "POST",
			body:          checkRun,
			delivery:      "d-ci-table",
			event:         "check_run",
			secret:        "s",
			wantStatus:    200,
			wantPublished: 0,
			wantRecorded:  1,
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
			recorder := &mockRecorder{}
			trigger := tc.mentionTrigger
			if trigger == "" {
				trigger = "@legion"
			}
			handler := GitHubHandler(tc.secret, trigger, "", pub, recorder)

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
			if len(recorder.calls) != tc.wantRecorded {
				t.Errorf("recorder calls = %d, want %d", len(recorder.calls), tc.wantRecorded)
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

// TestGitHubHandlerMergeGroup asserts GitHub's merge-queue event is acknowledged with 200 and
// neither published nor recorded: a merge group names no pull request head and nothing in
// Legion routes on it (LEGION-99). The fixture is production-shaped from GitHub's documented
// merge_group payload; the signature check still runs before the skip.
func TestGitHubHandlerMergeGroup(t *testing.T) {
	checksRequested, err := os.ReadFile(filepath.Join("testdata", "merge_group_checks_requested.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	destroyed := []byte(`{
		"action": "destroyed",
		"reason": "merged",
		"merge_group": {
			"head_sha": "9e1c7d2a4b6f8e0c1d3a5b7c9e1f2a4b6c8d0e2f",
			"head_ref": "refs/heads/gh-readonly-queue/main/pr-1050-7f60bbfb57cb2a1d0e9f8a7b6c5d4e3f2a1b0c9d",
			"base_sha": "7f60bbfb57cb2a1d0e9f8a7b6c5d4e3f2a1b0c9d",
			"base_ref": "refs/heads/main"
		},
		"sender": {"login": "github-merge-queue[bot]", "type": "Bot"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`)
	for i, tc := range []struct {
		name       string
		body       []byte
		signature  string // "" signs the body with the handler's secret
		wantStatus int
	}{
		{name: "checks_requested is acknowledged and not published", body: checksRequested, wantStatus: 200},
		{name: "destroyed is acknowledged and not published", body: destroyed, wantStatus: 200},
		{name: "an unsigned merge_group is still refused", body: checksRequested, signature: "sha256=invalid", wantStatus: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub := &mockPublisher{}
			recorder := &mockRecorder{}
			handler := GitHubHandler("s", "@legion", "", pub, recorder)
			req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(string(tc.body)))
			req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("d-merge-group-%d", i))
			req.Header.Set("X-GitHub-Event", "merge_group")
			sig := tc.signature
			if sig == "" {
				sig = githubSign("s", tc.body)
			}
			req.Header.Set("X-Hub-Signature-256", sig)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if len(pub.published) != 0 {
				t.Errorf("published = %d, want 0; first topic %q", len(pub.published), pub.published[0].Topic)
			}
			if n := len(recorder.calls) + len(recorder.suiteCalls) + len(recorder.headCalls); n != 0 {
				t.Errorf("recorder calls = %d, want 0", n)
			}
		})
	}
}

// TestGitHubHandlerCIRecordsObservations asserts a check_run webhook records
// one state fact per associated PR and never publishes the obsolete raw topic.
func TestGitHubHandlerCIRecordsObservations(t *testing.T) {
	checkRun := `{
		"action": "completed",
		"check_run": {
			"id": 987654321,
			"name": "unit-tests",
			"status": "completed",
			"conclusion": "failure",
			"completed_at": "2026-09-07T03:00:00Z",
			"head_sha": "deadbeef",
			"check_suite": {"id": 900},
			"pull_requests": [{"number": 42}, {"number": 43}]
		},
		"sender": {"login": "github-actions[bot]", "type": "Bot"},
		"repository": {"name": "legion", "owner": {"login": "sjawhar"}, "full_name": "sjawhar/legion"}
	}`

	t.Run("check_run records without publishing raw observations", func(t *testing.T) {
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
			t.Fatalf("raw CI envelope published: %+v", pub.published)
		}
		if len(rec.calls) != 2 {
			t.Fatalf("recorder calls = %d, want 2 (one per PR)", len(rec.calls))
		}
		if rec.calls[0].Number != "42" || rec.calls[1].Number != "43" {
			t.Fatalf("recorded PR numbers = %q, %q", rec.calls[0].Number, rec.calls[1].Number)
		}
		if rec.calls[0].ObservedAt != "2026-09-07T03:00:00Z" || rec.calls[1].ObservedAt != "2026-09-07T03:00:00Z" {
			t.Fatalf("check observation timestamps = %q, %q", rec.calls[0].ObservedAt, rec.calls[1].ObservedAt)
		}
	})

	t.Run("check_suite records without publishing raw observations", func(t *testing.T) {
		checkSuite := `{
			"action": "completed",
			"check_suite": {
				"id": 900,
				"status": "completed",
				"conclusion": "success",
				"updated_at": "2026-09-07T03:01:00Z",
				"head_sha": "abcdef1234567890abcdef1234567890abcdef12",
				"app": {"id": 77},
				"pull_requests": [{"number": 42}]
			},
			"repository": {"name": "example-repo", "owner": {"login": "example-org"}}
		}`
		pub := &mockPublisher{}
		rec := &mockRecorder{}
		handler := GitHubHandler("s", "@legion", "", pub, rec)
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(checkSuite))
		req.Header.Set("X-GitHub-Delivery", "d-ci-suite")
		req.Header.Set("X-GitHub-Event", "check_suite")
		req.Header.Set("X-Hub-Signature-256", githubSign("s", []byte(checkSuite)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
		}
		if len(pub.published) != 0 {
			t.Fatalf("raw CI envelope published: %+v", pub.published)
		}
		if len(rec.suiteCalls) != 1 {
			t.Fatalf("suite recorder calls = %d, want 1", len(rec.suiteCalls))
		}
		if got := rec.suiteCalls[0]; got != (contracts.CIObservation{
			Owner:      "example-org",
			Repo:       "example-repo",
			Number:     "42",
			SHA:        "abcdef1234567890abcdef1234567890abcdef12",
			SuiteID:    "900",
			AppID:      "77",
			Status:     "completed",
			Conclusion: "success",
			ObservedAt: "2026-09-07T03:01:00Z",
		}) {
			t.Fatalf("suite recorder call = %+v", got)
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
			t.Fatalf("status = %d, want 503", rr.Code)
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
		if len(pub.published) != 1 || len(rec.calls) != 0 {
			t.Fatalf("non-CI published=%d recorded=%d", len(pub.published), len(rec.calls))
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
				"id": 987654321,
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
		t.Run("right-App "+name+" verdict is recorded without raw publication", func(t *testing.T) {
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
			if got := rec.calls[0]; got.CheckName != name || got.Conclusion != "success" {
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
		if len(pub.published) != 0 {
			t.Fatalf("published = %d, want 0 for wrong-App tester", len(pub.published))
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
		if len(pub.published) != 0 {
			t.Fatalf("published = %d, want 0 without a reviewer App ID", len(pub.published))
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
		if got := rec.calls[0].CheckName; got != "unit-tests" {
			t.Fatalf("recorded check name = %q, want unit-tests", got)
		}
		if len(pub.published) != 0 {
			t.Fatalf("published = %d, want 0", len(pub.published))
		}
	})
}

func TestGitHubHandlerRecordsHeadOnPullRequestSynchronize(t *testing.T) {
	const (
		secret = "s"
		body   = `{
			"action": "synchronize",
			"number": 42,
			"pull_request": {
				"head": {"sha": "abcdef1234567"},
				"updated_at": "2026-09-07T03:00:00Z",
				"title": "Synchronize CI"
			},
			"sender": {"login": "ci-user", "type": "User"},
			"repository": {
				"name": "example-repo",
				"owner": {"login": "example-org"},
				"full_name": "example-org/example-repo"
			}
		}`
	)
	post := func(t *testing.T, rec *mockRecorder) (*httptest.ResponseRecorder, *mockPublisher) {
		t.Helper()
		pub := &mockPublisher{}
		handler := GitHubHandler(secret, "@legion", "", pub, rec)
		req := httptest.NewRequest("POST", "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", "delivery-head-sync")
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", githubSign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr, pub
	}

	t.Run("records the new head before publishing", func(t *testing.T) {
		rec := &mockRecorder{}
		rr, pub := post(t, rec)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		if len(rec.headCalls) != 1 {
			t.Fatalf("head calls = %d, want 1", len(rec.headCalls))
		}
		if got := rec.headCalls[0]; got != (headCall{
			owner: "example-org", repo: "example-repo", number: "42", sha: "abcdef1234567", updatedAt: "2026-09-07T03:00:00Z",
		}) {
			t.Fatalf("head call = %+v", got)
		}
		if len(pub.published) != 1 {
			t.Fatalf("published = %d, want 1", len(pub.published))
		}
	})

	t.Run("returns 503 when recording the head fails", func(t *testing.T) {
		rec := &mockRecorder{headErr: fmt.Errorf("kv down")}
		rr, pub := post(t, rec)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rr.Code)
		}
		if len(pub.published) != 0 {
			t.Fatalf("published = %d, want 0 after head recording failure", len(pub.published))
		}
	})
}

func TestGitHubHandlerRecordsHeadWithoutPullRequestUpdatedAt(t *testing.T) {
	const (
		secret = "s"
		body   = `{
			"action": "synchronize",
			"number": 42,
			"pull_request": {
				"head": {"sha": "abcdef1234567"}
			},
			"repository": {
				"name": "example-repo",
				"owner": {"login": "example-org"}
			}
		}`
	)
	pub := &mockPublisher{}
	recorder := &mockRecorder{}
	handler := GitHubHandler(secret, "@legion", "", pub, recorder)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "delivery-head-without-updated-at")
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, []byte(body)))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if len(recorder.headCalls) != 1 {
		t.Fatalf("head calls = %d, want 1", len(recorder.headCalls))
	}
	if got := recorder.headCalls[0]; got != (headCall{
		owner: "example-org", repo: "example-repo", number: "42", sha: "abcdef1234567",
	}) {
		t.Fatalf("head call = %+v", got)
	}
	if len(pub.published) != 1 {
		t.Fatalf("published = %d, want 1", len(pub.published))
	}
}
func TestGitHubHandlerTreatsMalformedHeadFieldsAsCallerData(t *testing.T) {
	const secret = "s"
	post := func(t *testing.T, body string, recorder *mockRecorder) (*httptest.ResponseRecorder, *mockPublisher) {
		t.Helper()
		pub := &mockPublisher{}
		handler := GitHubHandler(secret, "@legion", "", pub, recorder)
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", strings.NewReader(body))
		req.Header.Set("X-GitHub-Delivery", "delivery-malformed-head")
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", githubSign(secret, []byte(body)))
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		return rr, pub
	}

	t.Run("invalid updated_at is absent", func(t *testing.T) {
		body := `{
			"action":"synchronize",
			"number":42,
			"pull_request":{"head":{"sha":"abcdef1234567"},"updated_at":"not-a-timestamp"},
			"repository":{"name":"example-repo","owner":{"login":"example-org"}}
		}`
		recorder := &mockRecorder{}
		rr, pub := post(t, body, recorder)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
		}
		if len(recorder.headCalls) != 1 || recorder.headCalls[0].updatedAt != "" {
			t.Fatalf("head calls = %+v, want one timestamp-less call", recorder.headCalls)
		}
		if len(pub.published) != 1 {
			t.Fatalf("published = %d, want 1", len(pub.published))
		}
	})

	t.Run("invalid sha skips head recording", func(t *testing.T) {
		body := `{
			"action":"synchronize",
			"number":42,
			"pull_request":{"head":{"sha":"not-a-sha"},"updated_at":"2026-09-07T03:00:00Z"},
			"repository":{"name":"example-repo","owner":{"login":"example-org"}}
		}`
		recorder := &mockRecorder{headErr: cistore.ErrInvalidHeadSHA}
		rr, pub := post(t, body, recorder)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
		}
		if len(pub.published) != 1 {
			t.Fatalf("published = %d, want 1", len(pub.published))
		}
	})
}

// largePushPayload is a push listing commits commits, each carrying the fields GitHub sends for
// one and touching several files, the shape of a large branch push.
func largePushPayload(t *testing.T, commits int) []byte {
	t.Helper()
	list := make([]map[string]any, commits)
	for i := range list {
		sha := fmt.Sprintf("%040x", i+1)
		person := map[string]any{"name": "Example Author", "email": "author@example.com", "username": "example-author"}
		paths := make([]string, 12)
		for j := range paths {
			paths[j] = fmt.Sprintf("services/component-%03d/internal/package-%02d/source_file_%02d.go", i%50, j, j)
		}
		list[i] = map[string]any{
			"id":        sha,
			"tree_id":   fmt.Sprintf("%040x", i+1_000_000),
			"distinct":  true,
			"message":   fmt.Sprintf("Change %d\n\n%s", i, strings.Repeat("A longer commit message body line. ", 68)),
			"timestamp": "2026-09-24T01:29:00Z",
			"url":       "https://example-host/acme/widgets/commit/" + sha,
			"author":    person,
			"committer": person,
			"added":     paths[:4],
			"removed":   paths[4:6],
			"modified":  paths[6:],
		}
	}
	body, err := json.Marshal(map[string]any{
		"ref":         "refs/heads/main",
		"before":      fmt.Sprintf("%040x", 0),
		"after":       fmt.Sprintf("%040x", commits),
		"compare":     "https://example-host/acme/widgets/compare",
		"commits":     list,
		"head_commit": list[len(list)-1],
		"pusher":      map[string]any{"name": "example-author"},
		"sender":      map[string]any{"login": "example-author", "type": "User"},
		"repository":  map[string]any{"name": "widgets", "owner": map[string]any{"login": "acme"}, "full_name": "acme/widgets"},
	})
	if err != nil {
		t.Fatalf("marshal push: %v", err)
	}
	return body
}

// A push of about a thousand commits is a delivery of several megabytes, well inside the 25 MB
// GitHub delivers. The handler publishes it like any other push; refusing it loses the event for
// good, since a redelivery is the same body.
func TestGitHubHandlerPublishesAPushOfSeveralMegabytes(t *testing.T) {
	const secret = "s"
	body := largePushPayload(t, 1000)
	if len(body) < 3_500_000 {
		t.Fatalf("the push is %d bytes, want the ~3.6 MB of a thousand-commit push", len(body))
	}
	pub := &mockPublisher{}
	handler := GitHubHandler(secret, "@legion", "", pub, &mockRecorder{})
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", "delivery-large-push")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, body))
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	if len(pub.published) != 1 {
		t.Fatalf("published = %d, want 1", len(pub.published))
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(pub.published[0].Payload), &payload); err != nil {
		t.Fatalf("decode the push payload: %v", err)
	}
	if payload["commit_count"] != "1000" || payload["changed_paths_truncated"] != "true" {
		t.Fatalf("push payload commit_count = %q, changed_paths_truncated = %q; want 1000 and true",
			payload["commit_count"], payload["changed_paths_truncated"])
	}
}

// GitHub delivers nothing over 25 MB, so the handler reads a body up to that size and refuses a
// larger one as too large, before verifying or parsing any of it, whether or not the request
// declares its length.
func TestGitHubHandlerReadsABodyUpToGitHubsPayloadCap(t *testing.T) {
	const secret = "s"
	for _, tc := range []struct {
		name       string
		size       int
		undeclared bool
		want       int
	}{
		// A body of exactly the cap is read whole and reaches the JSON decode, which refuses it.
		{name: "at the cap", size: 25 << 20, want: http.StatusBadRequest},
		{name: "one byte over the cap", size: 25<<20 + 1, want: http.StatusRequestEntityTooLarge},
		{name: "at the cap, length undeclared", size: 25 << 20, undeclared: true, want: http.StatusBadRequest},
		{name: "one byte over the cap, length undeclared", size: 25<<20 + 1, undeclared: true, want: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("x"), tc.size)
			pub := &mockPublisher{}
			recorder := &mockRecorder{}
			handler := GitHubHandler(secret, "@legion", "", pub, recorder)
			req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(body))
			if tc.undeclared {
				req.ContentLength = -1
			}
			req.Header.Set("X-GitHub-Delivery", "delivery-oversized")
			req.Header.Set("X-GitHub-Event", "push")
			req.Header.Set("X-Hub-Signature-256", githubSign(secret, body))
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusBadRequest && !strings.Contains(rr.Body.String(), "invalid json") {
				t.Fatalf("body = %q, want the JSON decode's refusal: a body at the cap must be read whole", rr.Body.String())
			}
			if len(pub.published) != 0 || len(recorder.calls) != 0 || len(recorder.headCalls) != 0 {
				t.Fatalf("published %d, recorded %d checks and %d heads; want nothing", len(pub.published), len(recorder.calls), len(recorder.headCalls))
			}
		})
	}
}

// testBody is a request body of remaining bytes. It yields the first stallAfter bytes as fast as
// they are read and then waits for stall to close before failing, as a sender that stops sending
// does; a negative stallAfter never stalls. wait, when set, is closed before the first byte is
// yielded.
type testBody struct {
	remaining  int
	stallAfter int
	stall      <-chan struct{}
	wait       <-chan struct{}
	read       int
}

func (b *testBody) Read(p []byte) (int, error) {
	if b.read == 0 && b.wait != nil {
		<-b.wait
	}
	if b.stallAfter >= 0 && b.read >= b.stallAfter {
		<-b.stall
		return 0, io.ErrUnexpectedEOF
	}
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	if b.stallAfter >= 0 {
		n = min(n, b.stallAfter-b.read)
	}
	b.remaining -= n
	b.read += n
	return n, nil
}

// A body declared larger than GitHub delivers is refused before a byte of it is read.
func TestGitHubHandlerRefusesADeclaredOversizeBodyUnread(t *testing.T) {
	body := &testBody{remaining: githubMaxBody + 1, stallAfter: -1}
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
	req.ContentLength = githubMaxBody + 1
	req.Header.Set("X-GitHub-Delivery", "delivery-declared-oversize")
	req.Header.Set("X-GitHub-Event", "push")
	rr := httptest.NewRecorder()

	GitHubHandler("s", "@legion", "", &mockPublisher{}, &mockRecorder{}).ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", rr.Code, rr.Body.String())
	}
	if body.read != 0 {
		t.Fatalf("read %d bytes of a body declared over the cap, want none", body.read)
	}
}

// The handler buffers a body whole before it can check the signature, so anyone who can reach the
// route can make it hold a body. The budget it reads bodies against holds at most githubBodyBudget
// plus what the one request that has held its place longest charges past it, at most twice one
// cap-sized buffer (bodyBudget); the rest wait for room and are then served. Here sixteen cap-sized
// bodies with a wrong signature arrive together, sent as fast as they are read, and each is
// answered 401.
func TestGitHubHandlerBoundsTheBodyBytesItHoldsInABurst(t *testing.T) {
	const requests = 16
	bound := int64(githubBodyBudget + 2*githubMaxBody)
	bodies := newBodyBudget(githubBodyBudget)
	handler := githubHandler("s", "@legion", "", &mockPublisher{}, &mockRecorder{}, bodies)
	var mu sync.Mutex
	entered := 0
	start := make(chan struct{})
	codes := make([]int, requests)
	var wg sync.WaitGroup
	for i := range requests {
		body := &testBody{remaining: githubMaxBody, stallAfter: -1, wait: start}
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
		req.ContentLength = githubMaxBody
		req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-budget-%d", i))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", "sha256=0000")
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			entered++
			mu.Unlock()
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			codes[i] = rr.Code
		}()
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		ready := entered == requests
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests entered the handler", entered, requests)
		}
		time.Sleep(time.Millisecond)
	}
	// Let every request reach its body before any byte is sent.
	time.Sleep(20 * time.Millisecond)
	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the burst never drained: requests waited on each other's half-read bodies")
	}

	most := bodies.mostHeld()
	if most > bound {
		t.Fatalf("the budget held %d MiB at once, want at most %d MiB (a %d MiB budget plus twice one cap-sized buffer)", most>>20, bound>>20, githubBodyBudget>>20)
	}
	t.Logf("the budget held at most %d MiB", most>>20)
	for i, code := range codes {
		if code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 once its turn came", i, code)
		}
	}
}

// A sender without the secret can declare a cap-sized body and then send it a few bytes at a time,
// or send only its headers, holding its connection until the server's read timeout. It must not
// hold room for bytes it has not sent: GitHub gives a delivery 10 s, and one that waits behind such
// senders is lost. Here three trickling senders and one that sent only headers are mid-body when a
// signed 3.6 MB push arrives, and the push is served at once.
func TestGitHubHandlerServesADeliveryBehindSlowSenders(t *testing.T) {
	const secret = "s"
	handler := GitHubHandler(secret, "@legion", "", &mockPublisher{}, &mockRecorder{})
	stall := make(chan struct{})
	var mu sync.Mutex
	entered := 0
	var slow sync.WaitGroup
	for i, sent := range []int{4096, 4096, 4096, 0} {
		body := &testBody{remaining: githubMaxBody, stallAfter: sent, stall: stall}
		req := httptest.NewRequest(http.MethodPost, "/webhook/github", body)
		req.ContentLength = githubMaxBody
		req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("delivery-slow-%d", i))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", "sha256=0000")
		slow.Add(1)
		go func() {
			defer slow.Done()
			mu.Lock()
			entered++
			mu.Unlock()
			handler.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	defer func() {
		close(stall)
		slow.Wait()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		ready := entered == 4
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow senders never entered the handler")
		}
		time.Sleep(time.Millisecond)
	}
	// Let every slow sender reach its body, or the wait for room ahead of it.
	time.Sleep(20 * time.Millisecond)

	push := largePushPayload(t, 1000)
	req := httptest.NewRequest(http.MethodPost, "/webhook/github", bytes.NewReader(push))
	req.Header.Set("X-GitHub-Delivery", "delivery-behind-slow-senders")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", githubSign(secret, push))
	rr := httptest.NewRecorder()
	served := make(chan struct{})
	began := time.Now()
	go func() {
		handler.ServeHTTP(rr, req)
		close(served)
	}()
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("a signed push behind three trickling senders and one that sent only headers was not served within 5 s; GitHub gives up at 10 s")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	t.Logf("served in %s", time.Since(began).Round(time.Millisecond))
}
