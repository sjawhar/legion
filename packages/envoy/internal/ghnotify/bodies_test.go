package ghnotify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// readFixture decodes one JSON file under testdata the way the poller decodes GitHub's answers, so
// numbers are float64 as contracts expects of a webhook body.
func readFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return out
}

// TestBodiesMatchWebhookEnvelopes builds each change of the spec's Events table and both rows of
// its issue event table from the REST objects GitHub's API returns, and compares the envelopes with
// the ones contracts.GithubEnvelopes makes from the webhook body GitHub sends for the same change:
// the same topics in the same order, the same summary, payload and dedupe key (A3).
func TestBodiesMatchWebhookEnvelopes(t *testing.T) {
	issueRepo := readFixture(t, "rest/thread-issue.json")["repository"].(map[string]any)
	prRepo := readFixture(t, "rest/thread-pr.json")["repository"].(map[string]any)
	issue := readFixture(t, "rest/issue.json")
	issueClosed := readFixture(t, "rest/issue-closed.json")
	prIssue := readFixture(t, "rest/pr-issue.json")
	pull := readFixture(t, "rest/pull.json")
	pullMerged := readFixture(t, "rest/pull-merged.json")

	for _, tc := range []struct {
		name        string
		build       func() (item, error)
		webhook     string
		event       string
		sourceID    string
		wantTopics  []string
		wantPayload map[string]string
	}{
		{
			name: "new comment on an issue",
			build: func() (item, error) {
				return issueCommentItem(readFixture(t, "rest/issue-comment.json"), issue, issueRepo)
			},
			webhook:  "webhook/issue-comment-created.json",
			event:    "issue_comment",
			sourceID: "notifications.issue_comment.9001",
			wantTopics: []string{
				"notifications.github.acme.widgets.issue.7.comment",
			},
			wantPayload: map[string]string{"kind": "comment", "action": "created", "parent_kind": "issue", "author": "maintainer"},
		},
		{
			name: "new comment on a pull request",
			build: func() (item, error) {
				return issueCommentItem(readFixture(t, "rest/pr-issue-comment.json"), prIssue, prRepo)
			},
			webhook:  "webhook/issue-comment-pr-created.json",
			event:    "issue_comment",
			sourceID: "notifications.issue_comment.9003",
			wantTopics: []string{
				"notifications.github.acme.widgets.pr.8.comment",
			},
			wantPayload: map[string]string{"parent_kind": "pr", "legion_footer": "true"},
		},
		{
			name: "new review comment",
			build: func() (item, error) {
				return reviewCommentItem(readFixture(t, "rest/review-comment.json"), pull, prRepo)
			},
			webhook:  "webhook/pull-request-review-comment-created.json",
			event:    "pull_request_review_comment",
			sourceID: "notifications.pull_request_review_comment.9101",
			wantTopics: []string{
				"notifications.github.acme.widgets.pr.8.comment",
			},
			wantPayload: map[string]string{"path": "src/spin.go", "line": "12", "head_sha": "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"},
		},
		{
			name:     "submitted review",
			build:    func() (item, error) { return reviewItem(readFixture(t, "rest/review.json"), pull, prRepo) },
			webhook:  "webhook/pull-request-review-submitted.json",
			event:    "pull_request_review",
			sourceID: "notifications.pull_request_review.7101",
			wantTopics: []string{
				"notifications.github.acme.widgets.pr.8.review",
			},
			// The REST API answers APPROVED; the webhook body, and so the envelope, says approved.
			wantPayload: map[string]string{"state": "approved", "review_id": "7101", "submitted_at": "2026-10-27T11:42:00Z"},
		},
		{
			name: "pull request closed and merged",
			build: func() (item, error) {
				return issueEventItem(readFixture(t, "rest/event-pr-closed.json"), prIssue, pullMerged, prRepo)
			},
			webhook:  "webhook/pull-request-closed-merged.json",
			event:    "pull_request",
			sourceID: "notifications.pull_request.event.15001",
			wantTopics: []string{
				"notifications.github.acme.widgets.pr.8",
			},
			wantPayload: map[string]string{"action": "closed", "merged": "true", "merged_by": "maintainer", "merge_commit_sha": "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"},
		},
		{
			name: "pull request reopened",
			build: func() (item, error) {
				return issueEventItem(readFixture(t, "rest/event-pr-reopened.json"), prIssue, pull, prRepo)
			},
			webhook:  "webhook/pull-request-reopened.json",
			event:    "pull_request",
			sourceID: "notifications.pull_request.event.15002",
			wantTopics: []string{
				"notifications.github.acme.widgets.pr.8",
			},
			wantPayload: map[string]string{"action": "reopened", "merged": "false"},
		},
		{
			name: "issue closed",
			build: func() (item, error) {
				return issueEventItem(readFixture(t, "rest/event-issue-closed.json"), issueClosed, nil, issueRepo)
			},
			webhook:  "webhook/issues-closed.json",
			event:    "issues",
			sourceID: "notifications.issues.event.15003",
			wantTopics: []string{
				"notifications.github.acme.widgets.issue.7",
			},
			wantPayload: map[string]string{"kind": "issue", "action": "closed"},
		},
		{
			name: "issue reopened",
			build: func() (item, error) {
				return issueEventItem(readFixture(t, "rest/event-issue-reopened.json"), issue, nil, issueRepo)
			},
			webhook:  "webhook/issues-reopened.json",
			event:    "issues",
			sourceID: "notifications.issues.event.15004",
			wantTopics: []string{
				"notifications.github.acme.widgets.issue.7",
			},
			wantPayload: map[string]string{"kind": "issue", "action": "reopened"},
		},
		{
			name: "a comment holding the mention trigger gives the two mention copies first",
			build: func() (item, error) {
				return issueCommentItem(readFixture(t, "rest/issue-comment-mention.json"), issue, issueRepo)
			},
			webhook:  "webhook/issue-comment-mention.json",
			event:    "issue_comment",
			sourceID: "notifications.issue_comment.9002",
			wantTopics: []string{
				"notifications.github.acme.widgets.issue.7.mention",
				"notifications.github.acme.widgets.mention",
				"notifications.github.acme.widgets.issue.7.comment",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it, err := tc.build()
			if err != nil {
				t.Fatalf("build the item: %v", err)
			}
			if it.event != tc.event || it.sourceID != tc.sourceID {
				t.Fatalf("item is %s %s, want %s %s", it.event, it.sourceID, tc.event, tc.sourceID)
			}
			got := it.envelopes("@legion")
			want := contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
				Event:    tc.event,
				Delivery: tc.sourceID,
				Body:     readFixture(t, tc.webhook),
				EventID:  "webhook-event",
				TraceID:  "webhook-trace",
			}, "@legion")
			if len(got) != len(want) || len(got) != len(tc.wantTopics) {
				t.Fatalf("got %d envelopes, the webhook makes %d, want %d", len(got), len(want), len(tc.wantTopics))
			}
			for i := range got {
				if got[i].Topic != tc.wantTopics[i] || want[i].Topic != tc.wantTopics[i] {
					t.Fatalf("envelope %d topic %q, webhook %q, want %q", i, got[i].Topic, want[i].Topic, tc.wantTopics[i])
				}
				if got[i].Source != "github" || got[i].Source != want[i].Source {
					t.Fatalf("envelope %d source %q, webhook %q", i, got[i].Source, want[i].Source)
				}
				if got[i].SourceEventID != tc.sourceID || got[i].DedupeKey != "github."+tc.sourceID || got[i].DedupeKey != want[i].DedupeKey {
					t.Fatalf("envelope %d source event %q dedupe %q, webhook dedupe %q", i, got[i].SourceEventID, got[i].DedupeKey, want[i].DedupeKey)
				}
				if got[i].PayloadSummary != want[i].PayloadSummary {
					t.Fatalf("envelope %d summary\n got %q\nwant %q", i, got[i].PayloadSummary, want[i].PayloadSummary)
				}
				if got[i].Payload != want[i].Payload {
					t.Fatalf("envelope %d payload\n got %s\nwant %s", i, got[i].Payload, want[i].Payload)
				}
				if err := got[i].Validate(); err != nil {
					t.Fatalf("envelope %d does not validate: %v", i, err)
				}
				// The stream stores a polled envelope under a MsgId, as it does a webhook's.
				if !contracts.DedupeKeyNamesTheUpstreamEvent(got[i]) {
					t.Fatalf("envelope %d dedupe key %q earns no MsgId", i, got[i].DedupeKey)
				}
			}
			// Every copy of one change is one event: one event id and trace, new for each call.
			for i := range got {
				if got[i].EventID == "" || got[i].EventID != got[0].EventID || got[i].TraceID != got[0].TraceID {
					t.Fatalf("envelope %d event %q trace %q, first %q %q", i, got[i].EventID, got[i].TraceID, got[0].EventID, got[0].TraceID)
				}
			}
			if again := it.envelopes("@legion"); again[0].EventID == got[0].EventID || again[0].TraceID == got[0].TraceID {
				t.Fatal("a second call reused the event id or trace id")
			}
			var payload map[string]string
			if err := json.Unmarshal([]byte(got[len(got)-1].Payload), &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			for key, value := range tc.wantPayload {
				if payload[key] != value {
					t.Fatalf("payload %s = %q, want %q (payload %v)", key, payload[key], value, payload)
				}
			}
		})
	}
}
