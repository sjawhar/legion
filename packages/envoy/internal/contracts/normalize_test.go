package contracts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/core"
)

func decodePayload(t *testing.T, raw string) map[string]string {
	t.Helper()
	var summary map[string]string
	if err := json.Unmarshal([]byte(raw), &summary); err != nil {
		t.Fatalf("invalid summary JSON: %v", err)
	}
	return summary
}

func TestGithubEnvelope(t *testing.T) {
	item := GithubEnvelope(GithubEnvelopeInput{
		Event:    "pull_request",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "opened",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"pull_request": map[string]any{
				"number":   7,
				"title":    "hello",
				"body":     "PR description",
				"html_url": "https://github.com/sjawhar/envoy/pull/7",
				"user":     map[string]any{"login": "sjawhar"},
			},
		},
	})
	if item.Topic != "notifications.github.sjawhar.envoy.pr.7" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if item.DedupeKey != "github.d1" {
		t.Fatalf("unexpected dedupe key: %s", item.DedupeKey)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("expected valid envelope: %v", err)
	}
}

func TestSlackEnvelope(t *testing.T) {
	item := SlackEnvelope(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T01234567",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":    "app_mention",
				"user":    "U01234567",
				"channel": "C01234567",
				"text":    "hello envoy",
			},
		},
	})
	if item.Topic != "notifications.slack.T01234567.C01234567.mention" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if item.DedupeKey != "slack.Ev123" {
		t.Fatalf("unexpected dedupe key: %s", item.DedupeKey)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("expected valid envelope: %v", err)
	}
}

func TestContainsMention(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		trigger string
		want    bool
	}{
		{name: "matches mention at start", body: "@legion please help", trigger: "@legion", want: true},
		{name: "matches mention in middle", body: "hello @legion", trigger: "@legion", want: true},
		{name: "matches mention wrapped in punctuation", body: "(@legion)", trigger: "@legion", want: true},
		{name: "matches case-insensitively", body: "@LEGION", trigger: "@legion", want: true},
		{name: "finds a later valid mention after an invalid suffix match", body: "@legionnaire then @legion", trigger: "@legion", want: true},
		{name: "finds a later valid mention after email-like text", body: "user@legion.dev then ping @legion", trigger: "@legion", want: true},
		{name: "rejects suffix word", body: "@legionnaire", trigger: "@legion", want: false},
		{name: "rejects email", body: "user@legion.dev", trigger: "@legion", want: false},
		{name: "rejects dotted suffix", body: "@legion.dev", trigger: "@legion", want: false},
		{name: "rejects empty body", body: "", trigger: "@legion", want: false},
		{name: "rejects empty trigger", body: "@legion please help", trigger: "", want: false},
	}
	for _, item := range tests {
		t.Run(item.name, func(t *testing.T) {
			if got := ContainsMention(item.body, item.trigger); got != item.want {
				t.Fatalf("ContainsMention(%q, %q) = %v, want %v", item.body, item.trigger, got, item.want)
			}
		})
	}
}

func TestGithubEnvelopesNoMention(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "issue_comment",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "created",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"comment": map[string]any{
				"body":     "hello there",
				"html_url": "https://github.com/sjawhar/envoy/issues/1#issuecomment-1",
				"user":     map[string]any{"login": "commenter"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.envoy.comment" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesWithMention(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "issue_comment",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "created",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"comment": map[string]any{
				"body":     "@legion please help",
				"html_url": "https://github.com/sjawhar/envoy/issues/1#issuecomment-2",
				"user":     map[string]any{"login": "requester"},
			},
		},
	}, "@legion")
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	// Most-specific copy first: the shared dedupe key means whichever copy is
	// delivered first wins per (key, session); publish order puts the mention
	// copy ahead so mention subscribers see the mention-labeled copy.
	if items[0].Topic != "notifications.github.sjawhar.envoy.mention" {
		t.Fatalf("unexpected mention topic: %s", items[0].Topic)
	}
	if items[1].Topic != "notifications.github.sjawhar.envoy.comment" {
		t.Fatalf("unexpected comment topic: %s", items[1].Topic)
	}
	if items[0].DedupeKey != items[1].DedupeKey {
		t.Fatalf("dedupe keys differ: mention=%s comment=%s", items[0].DedupeKey, items[1].DedupeKey)
	}
}

func TestGithubEnvelopesReview(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "pull_request_review",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "submitted",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"review": map[string]any{
				"body":     "Can @legion take a look?",
				"html_url": "https://github.com/sjawhar/envoy/pull/1#pullrequestreview-1",
				"state":    "commented",
				"user":     map[string]any{"login": "reviewer"},
			},
		},
	}, "@legion")
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.envoy.mention" {
		t.Fatalf("unexpected mention topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesEmptyReview(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "pull_request_review",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "submitted",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"review": map[string]any{
				"body":     "",
				"html_url": "https://github.com/sjawhar/envoy/pull/1#pullrequestreview-2",
				"state":    "approved",
				"user":     map[string]any{"login": "approver"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
}

func TestGithubEnvelopesIssueCommentEditedDoesNotCreateMentionEnvelope(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "issue_comment",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "edited",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"issue": map[string]any{
				"number": 7,
				"title":  "Test issue",
			},
			"comment": map[string]any{
				"body":     "@legion please review",
				"html_url": "https://github.com/sjawhar/envoy/issues/7#issuecomment-5",
				"user":     map[string]any{"login": "editor"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope when action is edited, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.envoy.issue.7.comment" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesReviewRequiresSubmittedActionForMentionFanout(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "pull_request_review",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "edited",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"pull_request": map[string]any{
				"number": 1,
				"title":  "hello",
			},
			"review": map[string]any{
				"body":     "@legion can you take a look?",
				"html_url": "https://github.com/sjawhar/envoy/pull/1#pullrequestreview-3",
				"state":    "commented",
				"user":     map[string]any{"login": "reviewer"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope when review action is not submitted, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.envoy.pr.1.review" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubIsBotSender(t *testing.T) {
	if !GithubIsBotSender(map[string]any{"sender": map[string]any{"type": "Bot"}}) {
		t.Fatal("expected bot sender to be true")
	}
	if GithubIsBotSender(map[string]any{"sender": map[string]any{"type": "User"}}) {
		t.Fatal("expected user sender to be false")
	}
	if GithubIsBotSender(map[string]any{}) {
		t.Fatal("expected missing sender to be false")
	}
}

func TestGithubEnvelopesPRNumber(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "pull_request",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "opened",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"pull_request": map[string]any{
				"number":   42,
				"title":    "test",
				"body":     "Test PR",
				"html_url": "https://github.com/sjawhar/envoy/pull/42",
				"user":     map[string]any{"login": "sjawhar"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.envoy.pr.42" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesIssueCommentNumber(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "issue_comment",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "created",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"issue": map[string]any{
				"number": 7,
				"title":  "Test issue",
			},
			"comment": map[string]any{
				"body":     "just a comment",
				"html_url": "https://github.com/sjawhar/envoy/issues/7#issuecomment-3",
				"user":     map[string]any{"login": "commenter"},
			},
		},
	}, "@legion")
	// issue_comment with no pull_request field → issue.7.comment
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.envoy.issue.7.comment" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesSubIssuesTopic(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		senderType string
	}{
		{name: "sub_issue_added from user", action: "sub_issue_added", senderType: "User"},
		{name: "sub_issue_removed from user", action: "sub_issue_removed", senderType: "User"},
		{name: "sub_issue_added from bot", action: "sub_issue_added", senderType: "Bot"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := GithubEnvelopes(GithubEnvelopeInput{
				Event:    "sub_issues",
				Delivery: "d1",
				EventID:  "e1",
				TraceID:  "t1",
				Body: map[string]any{
					"action":       tt.action,
					"parent_issue": map[string]any{"number": 42},
					"sub_issue":    map[string]any{"number": 99},
					"repository": map[string]any{
						"name": "example-repo",
						"owner": map[string]any{
							"login": "example-org",
						},
					},
					"sender": map[string]any{"login": "someone", "type": tt.senderType},
				},
			}, "@legion")
			if len(items) != 1 {
				t.Fatalf("expected 1 envelope, got %d", len(items))
			}
			if items[0].Topic != "notifications.github.example-org.example-repo.issue.42.sub_issue" {
				t.Fatalf("unexpected topic: %s", items[0].Topic)
			}
			wantSummary := "sub issue " + tt.action + ": example-org/example-repo#42"
			if items[0].PayloadSummary != wantSummary {
				t.Fatalf("summary = %q, want %q", items[0].PayloadSummary, wantSummary)
			}
		})
	}
}

func TestGithubEnvelopesMentionWithNumber(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "issue_comment",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "created",
			"repository": map[string]any{
				"full_name": "sjawhar/envoy",
				"name":      "envoy",
				"owner": map[string]any{
					"login": "sjawhar",
				},
			},
			"issue": map[string]any{
				"number": 99,
				"title":  "Review request",
			},
			"comment": map[string]any{
				"body":     "@legion please review",
				"html_url": "https://github.com/sjawhar/envoy/issues/99#issuecomment-4",
				"user":     map[string]any{"login": "requester"},
			},
		},
	}, "@legion")
	// issue_comment with mention: issue.99.comment + issue.99.mention + repo mention
	if len(items) != 3 {
		t.Fatalf("expected 3 envelopes, got %d", len(items))
	}
	// Order: scoped mention, repo-wide mention, then the base comment copy.
	// Shared dedupe key + publish order = most-specific copy wins per session.
	if items[0].Topic != "notifications.github.sjawhar.envoy.issue.99.mention" {
		t.Fatalf("unexpected topic[0]: %s", items[0].Topic)
	}
	if items[1].Topic != "notifications.github.sjawhar.envoy.mention" {
		t.Fatalf("unexpected topic[1]: %s", items[1].Topic)
	}
	if items[2].Topic != "notifications.github.sjawhar.envoy.issue.99.comment" {
		t.Fatalf("unexpected topic[2]: %s", items[2].Topic)
	}
	if items[0].DedupeKey != "github.d1" || items[1].DedupeKey != "github.d1" || items[2].DedupeKey != "github.d1" {
		t.Fatalf("expected shared dedupe key github.d1: %s / %s / %s", items[0].DedupeKey, items[1].DedupeKey, items[2].DedupeKey)
	}
}

func TestSlackEnvelopesThread(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T01234567",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":      "message",
				"user":      "U01234567",
				"channel":   "C01234567",
				"text":      "reply in thread",
				"thread_ts": "1234567890.123456",
			},
		},
	})
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	// Thread copy first: the shared dedupe key means whichever copy is delivered
	// first wins per (key, session). Publishing the thread copy ahead of the
	// channel copy gives dual-subscribed sessions ONE push labeled with the most
	// specific topic — instead of the channel copy silently starving the thread
	// subscription (observed live 2026-07-20).
	if items[0].Topic != "notifications.slack.T01234567.C01234567.thread.1234567890_123456.message" {
		t.Fatalf("unexpected thread topic: %s", items[0].Topic)
	}
	if items[1].Topic != "notifications.slack.T01234567.C01234567.message" {
		t.Fatalf("unexpected channel topic: %s", items[1].Topic)
	}
	if items[0].DedupeKey != "slack.Ev123" || items[1].DedupeKey != "slack.Ev123" {
		t.Fatalf("expected shared dedupe key slack.Ev123: %s / %s", items[0].DedupeKey, items[1].DedupeKey)
	}
}

func TestSlackEnvelopesThreadMention(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T01234567",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":      "app_mention",
				"user":      "U01234567",
				"channel":   "C01234567",
				"text":      "@bot help in thread",
				"thread_ts": "1234567890.123456",
			},
		},
	})
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T01234567.C01234567.thread.1234567890_123456.mention" {
		t.Fatalf("unexpected thread topic: %s", items[0].Topic)
	}
	if items[1].Topic != "notifications.slack.T01234567.C01234567.mention" {
		t.Fatalf("unexpected channel topic: %s", items[1].Topic)
	}
	if items[0].DedupeKey != "slack.Ev123" || items[1].DedupeKey != "slack.Ev123" {
		t.Fatalf("expected shared dedupe key slack.Ev123: %s / %s", items[0].DedupeKey, items[1].DedupeKey)
	}
}

func TestSlackEnvelopesNoThread(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T01234567",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":    "app_mention",
				"user":    "U01234567",
				"channel": "C01234567",
				"text":    "hello",
				"ts":      "9999999999.000000",
			},
		},
	})
	// No thread_ts present — only channel-level envelope, NO ts fallback
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope (no thread fallback), got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T01234567.C01234567.mention" {
		t.Fatalf("unexpected channel topic: %s", items[0].Topic)
	}
}

func TestSlackEnvelopeDefaultsUnknownTeamAndChannel(t *testing.T) {
	item := SlackEnvelope(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"event_id": "Ev123",
			"event": map[string]any{
				"type": "message",
				"text": "hello envoy",
			},
		},
	})
	if item.Topic != "notifications.slack.unknown.unknown.message" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if item.DedupeKey != "slack.Ev123" {
		t.Fatalf("unexpected dedupe key: %s", item.DedupeKey)
	}
}

func TestGithubSummaryCapsLongFields(t *testing.T) {
	summary := githubSummary("issues", map[string]any{
		"action": "opened",
		"repository": map[string]any{
			"name":  "example-repo",
			"owner": map[string]any{"login": strings.Repeat("o", 200)},
		},
		"issue": map[string]any{"number": 1, "title": "Long repository owner"},
	})
	if got := len([]rune(summary)); got != 160 {
		t.Fatalf("summary has %d runes, want 160: %q", got, summary)
	}
	if !strings.HasSuffix(summary, "…") {
		t.Fatalf("summary must end with an ellipsis when capped: %q", summary)
	}
	if strings.Contains(summary, "\n") {
		t.Fatalf("summary contains a newline: %q", summary)
	}
}

func TestGithubSummaryTruncatesTitleRunes(t *testing.T) {
	title := strings.Repeat("🌍", 100)
	summary := githubSummary("pull_request", map[string]any{
		"action":       "opened",
		"repository":   map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
		"pull_request": map[string]any{"number": 1, "title": title},
	})
	wantTitle := strings.Repeat("🌍", 90) + "…"
	if !strings.HasSuffix(summary, wantTitle) {
		t.Fatalf("summary = %q, want title suffix %q", summary, wantTitle)
	}
}

func TestGithubResourceSubject(t *testing.T) {
	cases := []struct {
		owner        string
		repo         string
		resourceType string
		resourceNum  string
		want         string
	}{
		{owner: "acme", repo: "widgets", resourceType: "pr", resourceNum: "42", want: "notifications.github.acme.widgets.pr.42"},
		{owner: "sjawhar", repo: "legion", resourceType: "issue", resourceNum: "185", want: "notifications.github.sjawhar.legion.issue.185"},
		{owner: "org", repo: "repo", resourceType: "pr", resourceNum: "1", want: "notifications.github.org.repo.pr.1"},
	}
	for _, item := range cases {
		got := GithubResourceSubject(item.owner, item.repo, item.resourceType, item.resourceNum)
		if got != item.want {
			t.Fatalf("GithubResourceSubject(%s, %s, %s, %s) = %s, want %s", item.owner, item.repo, item.resourceType, item.resourceNum, got, item.want)
		}
	}
}

func TestGithubEnvelopesCICheckRunNoPRsDropsEnvelope(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "check_run",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"check_run": map[string]any{
				"name":          "test",
				"status":        "completed",
				"conclusion":    "success",
				"pull_requests": []any{},
			},
		},
	}, "@legion")
	// Un-PR'd check_run events are dropped (no active subscribers per #377).
	if len(items) != 0 {
		t.Fatalf("expected 0 envelopes for un-PR'd check_run, got %d", len(items))
	}
}

// CI events (check_run/check_suite) are not published raw. They fold into
// envoy_ci_state via the webhook handler's CIRecorder; the summary loop emits
// one settled checks envelope when the head's CI is complete.
func TestGithubEnvelopesCheckRunNotPublished(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "check_run",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"check_run": map[string]any{
				"name":       "test",
				"status":     "completed",
				"conclusion": "success",
				"head_sha":   "abc123",
				"pull_requests": []any{
					map[string]any{"number": 42},
					map[string]any{"number": 43},
				},
			},
		},
	}, "@legion")
	if len(items) != 0 {
		t.Fatalf("check_run must not be published raw, got %d envelopes", len(items))
	}
}

func TestGithubEnvelopesCheckSuiteNotPublished(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "check_suite",
		Delivery: "d2",
		EventID:  "e2",
		TraceID:  "t2",
		Body: map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"check_suite": map[string]any{
				"status":     "completed",
				"conclusion": "success",
				"pull_requests": []any{
					map[string]any{"number": 99},
				},
			},
		},
	}, "@legion")
	if len(items) != 0 {
		t.Fatalf("check_suite must not be published raw, got %d envelopes", len(items))
	}
}

func TestGithubCIObservations(t *testing.T) {
	checkRunBody := func(prs []any) map[string]any {
		return map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"check_run": map[string]any{
				"name":          "unit-tests",
				"status":        "completed",
				"conclusion":    "failure",
				"head_sha":      "deadbeef",
				"app":           map[string]any{"id": float64(12345)},
				"pull_requests": prs,
			},
		}
	}

	t.Run("check_run one PR", func(t *testing.T) {
		obs := GithubCIObservations("check_run", checkRunBody([]any{map[string]any{"number": 42}}))
		if len(obs) != 1 {
			t.Fatalf("expected 1 observation, got %d", len(obs))
		}
		o := obs[0]
		if o.Owner != "sjawhar" || o.Repo != "legion" || o.Number != "42" || o.SHA != "deadbeef" ||
			o.AppID != "12345" || o.CheckName != "unit-tests" ||
			o.Status != "completed" || o.Conclusion != "failure" {
			t.Fatalf("unexpected observation: %+v", o)
		}
	})

	t.Run("check_run multiple PRs fan out", func(t *testing.T) {
		obs := GithubCIObservations("check_run", checkRunBody([]any{
			map[string]any{"number": 42},
			map[string]any{"number": 43},
		}))
		if len(obs) != 2 {
			t.Fatalf("expected 2 observations, got %d", len(obs))
		}
		if obs[0].Number != "42" || obs[1].Number != "43" {
			t.Fatalf("unexpected PR numbers: %q, %q", obs[0].Number, obs[1].Number)
		}
	})

	t.Run("check_run no PR yields nothing", func(t *testing.T) {
		if obs := GithubCIObservations("check_run", checkRunBody([]any{})); len(obs) != 0 {
			t.Fatalf("expected no observations for un-PR'd check_run, got %d", len(obs))
		}
	})

	t.Run("check_run missing head_sha yields nothing", func(t *testing.T) {
		body := checkRunBody([]any{map[string]any{"number": 42}})
		delete(body["check_run"].(map[string]any), "head_sha")
		if obs := GithubCIObservations("check_run", body); len(obs) != 0 {
			t.Fatalf("expected no observations without head_sha, got %d", len(obs))
		}
	})

}

func TestGithubCIObservationsSkipMalformedPRNumbers(t *testing.T) {
	for _, number := range []any{42.5, "abc"} {
		body := map[string]any{
			"repository": map[string]any{
				"name":  "example-repo",
				"owner": map[string]any{"login": "example-org"},
			},
			"check_run": map[string]any{
				"name":          "build",
				"head_sha":      "abcdef",
				"pull_requests": []any{map[string]any{"number": number}},
			},
		}
		if observations := GithubCIObservations("check_run", body); len(observations) != 0 {
			t.Fatalf("PR number %v yielded observations: %+v", number, observations)
		}
	}
}

func TestGhostWisprSubject(t *testing.T) {
	cases := []struct {
		sessionId string
		kind      string
		want      string
	}{
		{sessionId: "20260326041405", kind: "session.started", want: "notifications.ghostwispr.20260326041405.session.started"},
		{sessionId: "20260326041405", kind: "session.ended", want: "notifications.ghostwispr.20260326041405.session.ended"},
		{sessionId: "20260326041629", kind: "summary.ready", want: "notifications.ghostwispr.20260326041629.summary.ready"},
	}
	for _, item := range cases {
		got := GhostWisprSubject(item.sessionId, item.kind)
		if got != item.want {
			t.Fatalf("GhostWisprSubject(%s, %s) = %s, want %s", item.sessionId, item.kind, got, item.want)
		}
	}
}

func TestGhostWisprTopicPrefix(t *testing.T) {
	if GhostWisprTopicPrefix != "notifications.ghostwispr." {
		t.Fatalf("unexpected prefix: %s", GhostWisprTopicPrefix)
	}
	subject := GhostWisprSubject("20260326041405", "session.ended")
	if !strings.HasPrefix(subject, GhostWisprTopicPrefix) {
		t.Fatalf("subject %s does not start with prefix %s", subject, GhostWisprTopicPrefix)
	}
}

func TestGhostWisprSourceValidation(t *testing.T) {
	env := Envelope{
		EventID:        "evt-1",
		Source:         "ghostwispr",
		SourceEventID:  "gw-delivery-1",
		Topic:          "notifications.ghostwispr.20260326041405.session.ended",
		DedupeKey:      "ghostwispr.gw-delivery-1",
		IssuedAt:       NowMillis(),
		PayloadSummary: "ghostwispr session_ended for session 20260326041405",
		TraceID:        "trace-1",
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("expected ghostwispr source to be valid: %v", err)
	}
}

func TestWhatsappSubject(t *testing.T) {
	cases := []struct {
		phone string
		jid   string
		kind  string
		want  string
	}{
		{phone: "15551234567", jid: "5551234567@s.whatsapp.net", kind: "message", want: "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message"},
		{phone: "15559876543", jid: "group-abc@g.us", kind: "message", want: "notifications.whatsapp.15559876543.group-abc@g.us.message"},
		{phone: "15551234567", jid: "5551234567@s.whatsapp.net", kind: "status", want: "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.status"},
	}
	for _, item := range cases {
		got := WhatsappSubject(item.phone, item.jid, item.kind)
		if got != item.want {
			t.Fatalf("WhatsappSubject(%s, %s, %s) = %s, want %s", item.phone, item.jid, item.kind, got, item.want)
		}
	}
}

func TestWhatsappSourceValidation(t *testing.T) {
	env := Envelope{
		EventID:        "evt-wa",
		Source:         "whatsapp",
		SourceEventID:  "whatsapp://messages/15551234567/5551234567@s.whatsapp.net",
		Topic:          WhatsappSubject("15551234567", "5551234567@s.whatsapp.net", "message"),
		DedupeKey:      "whatsapp.15551234567.5551234567@s.whatsapp.net.1712345678000",
		IssuedAt:       NowMillis(),
		PayloadSummary: "WhatsApp message in chat 5551234567@s.whatsapp.net",
		TraceID:        "trace-wa",
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("expected whatsapp source to be valid: %v", err)
	}
}

func TestSlackEnvelopeHandlesNonObjectEvent(t *testing.T) {
	item := SlackEnvelope(SlackEnvelopeInput{
		EventID: "e-malformed",
		TraceID: "t-malformed",
		Body: map[string]any{
			"team_id":  "T01234567",
			"event_id": "Ev123",
			"event":    "not-an-object",
		},
	})
	if item.Topic != "notifications.slack.T01234567.unknown.message" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("expected valid envelope: %v", err)
	}
}

func TestGithubEnvelopesMalformedCheckRunPullRequestsDropsEnvelope(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "check_run",
		Delivery: "d-ci",
		EventID:  "e-ci",
		TraceID:  "t-ci",
		Body: map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"name":  "envoy",
				"owner": map[string]any{"login": "sjawhar"},
			},
			"check_run": map[string]any{
				"name":          "ci",
				"status":        "completed",
				"conclusion":    "success",
				"pull_requests": []any{"bad-entry", 42},
			},
		},
	}, "@legion")
	// Malformed entries that yield no valid PR numbers behave like an empty list:
	// the envelope is dropped rather than emitted on a repo-wide ci topic.
	if len(items) != 0 {
		t.Fatalf("expected 0 envelopes for malformed pull_requests, got %d", len(items))
	}
}

func TestGithubEnvelopesPushToBranch(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "push",
		Delivery: "d-push-1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"ref": "refs/heads/main",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.push.branch.main" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesPushToTag(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "push",
		Delivery: "d-push-2",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"ref": "refs/tags/v1.0.0",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.push.tag.v1_0_0" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesPushToDottedBranch(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "push",
		Delivery: "d-push-3",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"ref": "refs/heads/release.v2",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.push.branch.release_v2" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesPushToSlashedBranch(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "push",
		Delivery: "d-push-4",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"ref": "refs/heads/feat/foo",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.push.branch.feat/foo" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
}

func TestGithubEnvelopesPushToNonHeadsTagsRefDropsEnvelope(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "push",
		Delivery: "d-push-5",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"ref": "refs/pull/123/merge",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
		},
	}, "@legion")
	if len(items) != 0 {
		t.Fatalf("expected 0 envelopes (non-heads/tags ref), got %d", len(items))
	}
}

func TestGithubEnvelopesWorkflowRunInProgress(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "workflow_run",
		Delivery: "d-wf-1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "in_progress",
			"repository": map[string]any{
				"full_name": "example-org/example-repo",
				"name":      "example-repo",
				"owner":     map[string]any{"login": "example-org"},
			},
			"workflow_run": map[string]any{
				"id":          float64(42),
				"name":        "CI",
				"path":        ".github/workflows/ci.yml",
				"head_branch": "main",
				"status":      "in_progress",
				"html_url":    "https://example-host/actions/runs/42",
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.example-org.example-repo.workflow.ci_yml.in_progress" {
		t.Fatalf("unexpected workflow topic: %s", items[0].Topic)
	}
	if got, want := items[0].PayloadSummary, "workflow CI main run 42 in_progress"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestGithubEnvelopesWorkflowRunCompleted(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "workflow_run",
		Delivery: "d-wf-2",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"full_name": "example-org/example-repo",
				"name":      "example-repo",
				"owner":     map[string]any{"login": "example-org"},
			},
			"workflow_run": map[string]any{
				"id":          float64(43),
				"name":        "Release Prod",
				"path":        ".github/workflows/release-prod.yaml",
				"head_branch": "main",
				"status":      "completed",
				"conclusion":  "success",
				"html_url":    "https://example-host/actions/runs/43",
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.example-org.example-repo.workflow.release-prod_yaml.completed" {
		t.Fatalf("unexpected workflow topic: %s", items[0].Topic)
	}
	if items[0].Payload == "" {
		t.Fatal("expected non-empty Payload for completed workflow_run")
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(items[0].Payload), &payload); err != nil {
		t.Fatalf("invalid payload JSON: %v", err)
	}
	if payload["conclusion"] != "success" || payload["run_id"] != "43" || payload["branch"] != "main" {
		t.Fatalf("unexpected payload: %v", payload)
	}
}

func TestGithubEnvelopesWorkflowRunLargeRunIDNotScientific(t *testing.T) {
	// Regression: real GitHub run_ids are 11+ digit integers. JSON unmarshals
	// them as float64, and fmt.Sprintf("%v", ...) used to render them in
	// scientific notation (e.g. "2.5964358269e+10"). The payload must carry
	// the integer form so downstream consumers can parse it as an ID.
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "workflow_run",
		Delivery: "d-wf-bigid",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"workflow_run": map[string]any{
				"id":          float64(25964358269), // 11-digit integer arriving via JSON
				"name":        "Legion Envoy and Contracts",
				"path":        ".github/workflows/envoy-and-contracts.yaml",
				"head_branch": "main",
				"status":      "completed",
				"conclusion":  "success",
				"html_url":    "https://github.com/sjawhar/legion/actions/runs/25964358269",
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(items[0].Payload), &payload); err != nil {
		t.Fatalf("invalid payload JSON: %v", err)
	}
	if payload["run_id"] != "25964358269" {
		t.Fatalf("run_id rendered incorrectly: got %q, want %q", payload["run_id"], "25964358269")
	}
}

func TestGithubEnvelopesWorkflowRunMissingPathDropsEnvelope(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event:    "workflow_run",
		Delivery: "d-wf-3",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action": "completed",
			"repository": map[string]any{
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"workflow_run": map[string]any{
				"id":     float64(44),
				"status": "completed",
			},
		},
	}, "@legion")
	if len(items) != 0 {
		t.Fatalf("expected 0 envelopes (missing path), got %d", len(items))
	}
}

func TestGhostWisprEnvelopeSessionEnded(t *testing.T) {
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: "session_ended",
		Delivery:  "gw-delivery-1",
		EventID:   "e1",
		TraceID:   "t1",
		Body: map[string]any{
			"id":         float64(11),
			"event_type": "session_ended",
			"payload": map[string]any{
				"session_id": "20260326041405",
				"timestamp":  "2026-03-26T04:14:58.198253094Z",
				"duration":   51.05,
				"type":       "session_ended",
				"version":    float64(1),
			},
			"created_at": "2026-03-26T04:14:58Z",
		},
	})
	if item.Topic != "notifications.ghostwispr.20260326041405.session.ended" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if item.DedupeKey != "ghostwispr.gw-delivery-1" {
		t.Fatalf("unexpected dedupe key: %s", item.DedupeKey)
	}
	if item.Source != "ghostwispr" {
		t.Fatalf("unexpected source: %s", item.Source)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("expected valid envelope: %v", err)
	}
	if got, want := item.PayloadSummary, "ghostwispr session_ended for session 20260326041405"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	payload := decodePayload(t, item.Payload)
	if got, want := payload["duration"], "51.05"; got != want {
		t.Fatalf("payload duration = %q, want %q", got, want)
	}
}

func TestGhostWisprEnvelopeSummaryReady(t *testing.T) {
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: "summary_ready",
		Delivery:  "gw-delivery-2",
		EventID:   "e2",
		TraceID:   "t2",
		Body: map[string]any{
			"id":         float64(19),
			"event_type": "summary_ready",
			"payload": map[string]any{
				"session_id":     "20260326041629",
				"status":         "completed",
				"summary":        "",
				"summary_preset": "default",
				"timestamp":      "2026-03-26T04:17:03.04177255Z",
				"title":          "How are we gonna do the",
				"type":           "summary_ready",
				"version":        float64(1),
			},
			"created_at": "2026-03-26T04:17:03Z",
		},
	})
	if item.Topic != "notifications.ghostwispr.20260326041629.summary.ready" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("expected valid envelope: %v", err)
	}
	if got, want := item.PayloadSummary, "ghostwispr summary_ready for session 20260326041629: How are we gonna do the"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	payload := decodePayload(t, item.Payload)
	if got, want := payload["summary_preset"], "default"; got != want {
		t.Fatalf("summary_preset = %q, want %q", got, want)
	}
}

func TestGhostWisprEnvelopeSessionStarted(t *testing.T) {
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: "session_started",
		Delivery:  "gw-delivery-3",
		EventID:   "e3",
		TraceID:   "t3",
		Body: map[string]any{
			"id":         float64(5),
			"event_type": "session_started",
			"payload": map[string]any{
				"session_id": "20260326041405",
				"timestamp":  "2026-03-26T04:14:05.79928117Z",
				"type":       "session_started",
				"version":    float64(1),
			},
			"created_at": "2026-03-26T04:14:05Z",
		},
	})
	if item.Topic != "notifications.ghostwispr.20260326041405.session.started" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("expected valid envelope: %v", err)
	}
}

func TestGhostWisprEnvelopeMissingSessionIdUsesUnknownTopic(t *testing.T) {
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: "session_ended",
		Delivery:  "gw-delivery-4",
		EventID:   "e4",
		TraceID:   "t4",
		Body:      map[string]any{"event_type": "session_ended"},
	})
	if item.Topic != "notifications.ghostwispr.unknown.session.ended" {
		t.Fatalf("unexpected topic for missing session_id: %s", item.Topic)
	}
	if err := item.Validate(); err != nil {
		t.Fatalf("expected valid envelope even without session_id: %v", err)
	}
	if got, want := item.PayloadSummary, "ghostwispr session_ended for session "; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestGhostWisprKindMapping(t *testing.T) {
	cases := []struct {
		eventType string
		wantKind  string
	}{
		{eventType: "session_started", wantKind: "session.started"},
		{eventType: "session_ended", wantKind: "session.ended"},
		{eventType: "summary_ready", wantKind: "summary.ready"},
		{eventType: "unknown_type", wantKind: "unknown_type"},
	}
	for _, item := range cases {
		env := GhostWisprEnvelope(GhostWisprEnvelopeInput{
			EventType: item.eventType,
			Delivery:  "d1",
			EventID:   "e1",
			TraceID:   "t1",
			Body: map[string]any{
				"payload": map[string]any{"session_id": "20260326041405"},
			},
		})
		want := GhostWisprSubject("20260326041405", item.wantKind)
		if env.Topic != want {
			t.Fatalf("eventType=%s: got topic %s, want %s", item.eventType, env.Topic, want)
		}
	}
}

func TestGhostWisprEnvelopeSanitizesSessionID(t *testing.T) {
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: "session_ended",
		Delivery:  "gw-delivery-sanitize",
		EventID:   "e-sanitize",
		TraceID:   "t-sanitize",
		Body: map[string]any{
			"event_type": "session_ended",
			"payload": map[string]any{
				"session_id": " 2026.03/26 041405 ",
				"type":       "session_ended",
			},
		},
	})
	if item.Topic != "notifications.ghostwispr.2026_03_26_041405.session.ended" {
		t.Fatalf("unexpected sanitized topic: %s", item.Topic)
	}
	if got, want := item.PayloadSummary, "ghostwispr session_ended for session 2026.03/26 041405"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestGhostWisprEnvelopeNormalizesEventType(t *testing.T) {
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: " SUMMARY.READY ",
		Delivery:  "gw-delivery-normalized",
		EventID:   "e-normalized",
		TraceID:   "t-normalized",
		Body: map[string]any{
			"payload": map[string]any{"session_id": "20260326041629"},
		},
	})
	if item.Topic != "notifications.ghostwispr.20260326041629.summary.ready" {
		t.Fatalf("unexpected topic: %s", item.Topic)
	}
	if got, want := item.PayloadSummary, "ghostwispr summary_ready for session 20260326041629"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestGhostWisprSummaryTruncatesTitle(t *testing.T) {
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: "summary_ready",
		Delivery:  "gw-delivery-title",
		EventID:   "e-title",
		TraceID:   "t-title",
		Body: map[string]any{
			"payload": map[string]any{
				"session_id": "20260326041629",
				"title":      strings.Repeat("a", 600),
			},
		},
	})
	wantTitle := strings.Repeat("a", 80) + "…"
	if !strings.HasSuffix(item.PayloadSummary, wantTitle) {
		t.Fatalf("summary = %q, want title suffix %q", item.PayloadSummary, wantTitle)
	}
}

func TestGithubPayloadCapsBodyRunes(t *testing.T) {
	body := strings.Repeat("🔥", 3000)
	payload := decodePayload(t, GithubEnvelope(GithubEnvelopeInput{
		Event:    "issue_comment",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action":     "created",
			"repository": map[string]any{"full_name": "example-org/example-repo"},
			"issue":      map[string]any{"number": 42, "title": "Test issue"},
			"comment": map[string]any{
				"body": body,
				"user": map[string]any{"login": "commenter"},
			},
		},
	}).Payload)
	if got := len([]rune(payload["body"])); got != 2048 {
		t.Fatalf("payload body has %d runes, want 2048", got)
	}
	if got, want := payload["body_truncated"], "true"; got != want {
		t.Fatalf("body_truncated = %q, want %q", got, want)
	}
}

func TestGithubPayloadAllEventTypes(t *testing.T) {
	tests := []struct {
		name       string
		event      string
		body       map[string]any
		hasPayload bool
	}{
		{
			name:  "issue_comment has payload",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"issue":      map[string]any{"number": 1, "title": "test"},
				"comment":    map[string]any{"body": "hello", "user": map[string]any{"login": "u"}},
			},
			hasPayload: true,
		},
		{
			name:  "pull_request has payload",
			event: "pull_request",
			body: map[string]any{
				"action":       "opened",
				"repository":   map[string]any{"full_name": "sjawhar/legion"},
				"pull_request": map[string]any{"number": 1, "title": "test", "body": "pr body", "user": map[string]any{"login": "u"}},
			},
			hasPayload: true,
		},
		{
			name:  "pull_request_review has payload",
			event: "pull_request_review",
			body: map[string]any{
				"action":       "submitted",
				"repository":   map[string]any{"full_name": "sjawhar/legion"},
				"pull_request": map[string]any{"number": 1, "title": "test"},
				"review":       map[string]any{"body": "review body", "state": "approved", "user": map[string]any{"login": "u"}},
			},
			hasPayload: true,
		},
		{
			name:  "pull_request_review_comment has payload",
			event: "pull_request_review_comment",
			body: map[string]any{
				"action":       "created",
				"repository":   map[string]any{"full_name": "sjawhar/legion"},
				"pull_request": map[string]any{"number": 1, "title": "test"},
				"comment":      map[string]any{"body": "comment body", "user": map[string]any{"login": "u"}},
			},
			hasPayload: true,
		},
		{
			name:  "issues has payload",
			event: "issues",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"issue":      map[string]any{"number": 1, "title": "test", "body": "issue body", "user": map[string]any{"login": "u"}},
			},
			hasPayload: true,
		},
		{
			name:       "push has payload",
			event:      "push",
			body:       map[string]any{"repository": map[string]any{"full_name": "example-org/example-repo"}, "ref": "refs/heads/main"},
			hasPayload: true,
		},
		{
			name:       "check_run has no payload",
			event:      "check_run",
			body:       map[string]any{"action": "completed", "repository": map[string]any{"full_name": "sjawhar/legion"}, "check_run": map[string]any{"name": "test", "status": "completed", "conclusion": "success", "pull_requests": []any{}}},
			hasPayload: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := GithubEnvelope(GithubEnvelopeInput{
				Event:    tt.event,
				Delivery: "d1",
				EventID:  "e1",
				TraceID:  "t1",
				Body:     tt.body,
			})
			if tt.hasPayload && env.Payload == "" {
				t.Fatal("expected Payload to be populated")
			}
			if !tt.hasPayload && env.Payload != "" {
				t.Fatalf("expected empty Payload for %s, got %s", tt.event, env.Payload)
			}
		})
	}
}

func TestGithubPayloadDispatchSession(t *testing.T) {
	issueMarker, err := core.BuildMetaMarker(core.MetaMarker{
		RequestID: "request-1",
		Urgency:   core.UrgencyMed,
		Origin:    &core.Origin{SessionID: "issue-session"},
	})
	if err != nil {
		t.Fatalf("build issue marker: %v", err)
	}
	commentMarker, err := core.BuildAskMarker(core.AskMarker{
		RequestID: "request-2",
		Origin:    &core.Origin{SessionID: "comment-session"},
	})
	if err != nil {
		t.Fatalf("build comment marker: %v", err)
	}

	tests := []struct {
		name                string
		event               string
		body                map[string]any
		wantDispatchSession string
	}{
		{
			name:  "issue opened with marker",
			event: "issues",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"issue": map[string]any{
					"number": 1,
					"title":  "Question",
					"body":   issueMarker,
					"user":   map[string]any{"login": "author"},
				},
			},
			wantDispatchSession: "issue-session",
		},
		{
			name:  "issue comment created with marker",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"issue":      map[string]any{"number": 1, "title": "Question"},
				"comment": map[string]any{
					"body": commentMarker,
					"user": map[string]any{"login": "author"},
				},
			},
			wantDispatchSession: "comment-session",
		},
		{
			name:  "comment without marker",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"issue":      map[string]any{"number": 1, "title": "Question"},
				"comment": map[string]any{
					"body": "Ordinary comment",
					"user": map[string]any{"login": "author"},
				},
			},
		},
		{
			name:  "malformed marker",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"issue":      map[string]any{"number": 1, "title": "Question"},
				"comment": map[string]any{
					"body": "<!-- dispatch:ask\nrequestId: request-3\norigin: [\n-->",
					"user": map[string]any{"login": "author"},
				},
			},
		},
		{
			name:  "pull request review comment untouched",
			event: "pull_request_review_comment",
			body: map[string]any{
				"action":       "created",
				"repository":   map[string]any{"full_name": "example-org/example-repo"},
				"pull_request": map[string]any{"number": 1, "title": "Question"},
				"comment": map[string]any{
					"body": commentMarker,
					"user": map[string]any{"login": "author"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := decodePayload(t, githubPayload(tt.event, tt.body))
			got, found := payload["dispatch_session"]
			if tt.wantDispatchSession == "" {
				if found {
					t.Fatalf("unexpected dispatch_session %q", got)
				}
				return
			}
			if !found {
				t.Fatal("missing dispatch_session")
			}
			if got != tt.wantDispatchSession {
				t.Fatalf("dispatch_session = %q, want %q", got, tt.wantDispatchSession)
			}
		})
	}
}

func TestGithubSummary(t *testing.T) {
	tests := []struct {
		name  string
		event string
		body  map[string]any
		want  string
	}{
		{
			name:  "created issue comment uses its first line",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"issue":      map[string]any{"number": 17},
				"comment":    map[string]any{"body": "Ship it\nwith a follow-up", "user": map[string]any{"login": "commenter"}},
			},
			want: "comment on example-org/example-repo#17 by commenter: Ship it",
		},
		{
			name:  "edited issue comment omits its body",
			event: "issue_comment",
			body: map[string]any{
				"action":     "edited",
				"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"issue":      map[string]any{"number": 17},
				"comment":    map[string]any{"body": "changed", "user": map[string]any{"login": "commenter"}},
			},
			want: "comment edited on example-org/example-repo#17 by commenter",
		},
		{
			name:  "deleted issue comment omits its body",
			event: "issue_comment",
			body: map[string]any{
				"action":     "deleted",
				"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"issue":      map[string]any{"number": 17},
				"comment":    map[string]any{"body": "removed", "user": map[string]any{"login": "commenter"}},
			},
			want: "comment deleted on example-org/example-repo#17 by commenter",
		},
		{
			name:  "review comment includes location and excerpt",
			event: "pull_request_review_comment",
			body: map[string]any{
				"action":       "created",
				"repository":   map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"pull_request": map[string]any{"number": 18},
				"comment": map[string]any{
					"body": "Use the existing helper",
					"user": map[string]any{"login": "reviewer"},
					"path": "internal/contracts/normalize.go",
					"line": 42,
				},
			},
			want: "review comment on example-org/example-repo#18 by reviewer (internal/contracts/normalize.go:42): Use the existing helper",
		},
		{
			name:  "edited review comment omits its body",
			event: "pull_request_review_comment",
			body: map[string]any{
				"action":       "edited",
				"repository":   map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"pull_request": map[string]any{"number": 18},
				"comment": map[string]any{
					"body": "secret edit body",
					"user": map[string]any{"login": "reviewer"},
					"path": "internal/contracts/normalize.go",
					"line": 42,
				},
			},
			want: "review comment edited on example-org/example-repo#18 by reviewer (internal/contracts/normalize.go:42)",
		},
		{
			name:  "review includes state and nonempty body",
			event: "pull_request_review",
			body: map[string]any{
				"repository":   map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"pull_request": map[string]any{"number": 19},
				"review":       map[string]any{"state": "approved", "body": "Looks good", "user": map[string]any{"login": "reviewer"}},
			},
			want: "review approved on example-org/example-repo#19 by reviewer: Looks good",
		},
		{
			name:  "review excludes empty body",
			event: "pull_request_review",
			body: map[string]any{
				"repository":   map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"pull_request": map[string]any{"number": 19},
				"review":       map[string]any{"state": "commented", "user": map[string]any{"login": "reviewer"}},
			},
			want: "review commented on example-org/example-repo#19 by reviewer",
		},
		{
			name:  "opened pull request includes title",
			event: "pull_request",
			body: map[string]any{
				"action":       "opened",
				"repository":   map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"pull_request": map[string]any{"number": 20, "title": "Improve envelopes"},
			},
			want: "pr opened: example-org/example-repo#20 Improve envelopes",
		},
		{
			name:  "merged pull request includes merger and short SHA",
			event: "pull_request",
			body: map[string]any{
				"action":     "closed",
				"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"pull_request": map[string]any{
					"number":           20,
					"merged":           true,
					"merged_by":        map[string]any{"login": "merger"},
					"merge_commit_sha": "abcdef0123456789",
				},
			},
			want: "pr merged: example-org/example-repo#20 by merger → abcdef0",
		},
		{
			name:  "issue includes title",
			event: "issues",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"issue":      map[string]any{"number": 21, "title": "Document the payload"},
			},
			want: "issue opened: example-org/example-repo#21 Document the payload",
		},
		{
			name:  "push includes branch subject short SHA and pusher",
			event: "push",
			body: map[string]any{
				"repository":  map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"ref":         "refs/heads/feature/demo",
				"after":       "0123456789abcdef",
				"pusher":      map[string]any{"name": "pusher"},
				"head_commit": map[string]any{"message": "Ship the normalizer\nwith details"},
			},
			want: "push to feature/demo: Ship the normalizer (0123456) by pusher",
		},
		{
			name:  "workflow includes conclusion",
			event: "workflow_run",
			body: map[string]any{
				"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"workflow_run": map[string]any{
					"id":          float64(22),
					"name":        "Checks",
					"head_branch": "main",
					"status":      "completed",
					"conclusion":  "success",
				},
			},
			want: "workflow Checks main run 22 completed/success",
		},
		{
			name:  "workflow omits an empty conclusion separator",
			event: "workflow_run",
			body: map[string]any{
				"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
				"workflow_run": map[string]any{
					"id":          float64(23),
					"name":        "Checks",
					"head_branch": "main",
					"status":      "in_progress",
				},
			},
			want: "workflow Checks main run 23 in_progress",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := githubSummary(tt.event, tt.body)
			if got != tt.want {
				t.Fatalf("githubSummary(%q) = %q, want %q", tt.event, got, tt.want)
			}
			if len([]rune(got)) > 160 {
				t.Fatalf("summary is %d runes, want at most 160", len([]rune(got)))
			}
			if strings.Contains(got, "\n") {
				t.Fatalf("summary contains a newline: %q", got)
			}
		})
	}
}

func TestGithubPayloadFields(t *testing.T) {
	longBody := strings.Repeat("a", 3000)
	tests := []struct {
		name        string
		event       string
		body        map[string]any
		want        map[string]string
		omitted     []string
		wantBodyLen int
	}{
		{
			name:  "push carries delivery facts",
			event: "push",
			body: map[string]any{
				"repository":  map[string]any{"full_name": "example-org/example-repo"},
				"ref":         "refs/heads/main",
				"before":      "1111111111111111",
				"after":       "2222222222222222",
				"pusher":      map[string]any{"name": "pusher"},
				"head_commit": map[string]any{"message": "Add actionable payloads\n\nBody"},
				"commits":     []any{map[string]any{"id": "1"}, map[string]any{"id": "2"}},
				"compare":     "https://example-host/compare",
			},
			want: map[string]string{
				"after": "2222222222222222", "before": "1111111111111111", "pusher": "pusher",
				"head_subject": "Add actionable payloads", "commit_count": "2", "compare_url": "https://example-host/compare",
			},
		},
		{
			name:  "merged pull request carries merge and ref facts",
			event: "pull_request",
			body: map[string]any{
				"action":     "closed",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"pull_request": map[string]any{
					"number":           24,
					"merged":           true,
					"head":             map[string]any{"sha": "head-sha", "ref": "feature/payloads"},
					"base":             map[string]any{"ref": "main"},
					"merge_commit_sha": "merge-sha",
					"merged_by":        map[string]any{"login": "merger"},
					"updated_at":       "2026-09-07T10:01:00Z",
				},
			},
			want: map[string]string{
				"head_sha": "head-sha", "head_ref": "feature/payloads", "base_ref": "main", "merged": "true",
				"merge_commit_sha": "merge-sha", "merged_by": "merger", "updated_at": "2026-09-07T10:01:00Z",
			},
		},
		{
			name:  "workflow run carries branch and pull request facts",
			event: "workflow_run",
			body: map[string]any{
				"action":     "completed",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"workflow_run": map[string]any{
					"id":             float64(25),
					"run_attempt":    float64(2),
					"head_sha":       "workflow-sha",
					"head_branch":    "feature/payloads",
					"pull_requests":  []any{map[string]any{"number": float64(12)}, map[string]any{"number": float64(34)}},
					"run_started_at": "2026-09-07T10:00:00Z",
					"updated_at":     "2026-09-07T10:01:00Z",
				},
			},
			want: map[string]string{
				"run_id": "25", "run_attempt": "2", "head_sha": "workflow-sha", "branch": "feature/payloads",
				"head_branch": "feature/payloads", "pr_numbers": "12,34", "run_started_at": "2026-09-07T10:00:00Z",
				"updated_at": "2026-09-07T10:01:00Z",
			},
		},
		{
			name:  "edited comment signals body change without resending it",
			event: "issue_comment",
			body: map[string]any{
				"action":     "edited",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"issue":      map[string]any{"number": 26},
				"comment":    map[string]any{"body": "Do not resend this", "user": map[string]any{"login": "commenter"}},
			},
			want:    map[string]string{"body_changed": "true"},
			omitted: []string{"body"},
		},
		{
			name:  "review comment carries location",
			event: "pull_request_review_comment",
			body: map[string]any{
				"action":       "created",
				"repository":   map[string]any{"full_name": "example-org/example-repo"},
				"pull_request": map[string]any{"number": 27},
				"comment": map[string]any{
					"body": "Use the helper", "path": "internal/contracts/normalize.go", "original_line": float64(44),
				},
			},
			want: map[string]string{"path": "internal/contracts/normalize.go", "line": "44"},
		},
		{
			name:  "long comment body is capped and marked",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "example-org/example-repo"},
				"issue":      map[string]any{"number": 28},
				"comment":    map[string]any{"body": longBody, "user": map[string]any{"login": "commenter"}},
			},
			want:        map[string]string{"body_truncated": "true"},
			wantBodyLen: 2048,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := decodePayload(t, GithubEnvelope(GithubEnvelopeInput{
				Event: tt.event, Delivery: "delivery", EventID: "event", TraceID: "trace", Body: tt.body,
			}).Payload)
			for key, want := range tt.want {
				if got := payload[key]; got != want {
					t.Fatalf("payload[%q] = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.omitted {
				if _, found := payload[key]; found {
					t.Fatalf("payload unexpectedly contains %q: %q", key, payload[key])
				}
			}
			if tt.wantBodyLen != 0 && len([]rune(payload["body"])) != tt.wantBodyLen {
				t.Fatalf("body has %d runes, want %d", len([]rune(payload["body"])), tt.wantBodyLen)
			}
		})
	}
}

func TestGithubEnvelopesWorkflowRunWithPullRequestsDropsEnvelope(t *testing.T) {
	items := GithubEnvelopes(GithubEnvelopeInput{
		Event: "workflow_run", Delivery: "delivery", EventID: "event", TraceID: "trace",
		Body: map[string]any{
			"action":     "completed",
			"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
			"workflow_run": map[string]any{
				"path":          ".github/workflows/checks.yml",
				"head_branch":   "feature/add.payload",
				"pull_requests": []any{map[string]any{"number": 30}},
			},
		},
	}, "@envoy")
	if len(items) != 0 {
		t.Fatalf("got %d envelopes, want none for a pull-request workflow run", len(items))
	}
}

func TestSlackSummary(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "mention",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "app_mention", "channel": "C01234567", "user": "U01234567", "text": "Please review"},
			},
			want: "slack mention in C01234567 from U01234567: Please review",
		},
		{
			name: "message",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "message", "channel": "C01234567", "user": "U01234567", "text": "An update"},
			},
			want: "slack message in C01234567 from U01234567: An update",
		},
		{
			name: "thread reply",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "message", "channel": "C01234567", "user": "U01234567", "thread_ts": "123.456", "text": "A reply"},
			},
			want: "slack thread reply in C01234567 from U01234567 (thread 123.456): A reply",
		},
		{
			name: "bot message prefers username",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "message", "subtype": "bot_message", "channel": "C01234567", "bot_id": "B01234567", "username": "build-bot", "text": "Build passed"},
			},
			want: "slack bot message in C01234567 from build-bot: Build passed",
		},
		{
			name: "changed message reads nested facts",
			body: map[string]any{
				"team_id": "T01234567",
				"event": map[string]any{
					"type": "message", "subtype": "message_changed", "channel": "C01234567",
					"message": map[string]any{"user": "U01234567", "ts": "456.789", "text": "New text"},
				},
			},
			want: "slack message edited in C01234567 by U01234567 at 456.789: New text",
		},
		{
			name: "deleted message has timestamp only",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "message", "subtype": "message_deleted", "channel": "C01234567", "deleted_ts": "456.789"},
			},
			want: "slack message deleted in C01234567: 456.789",
		},
		{
			name: "files add title",
			body: map[string]any{
				"team_id": "T01234567",
				"event": map[string]any{
					"type": "message", "channel": "C01234567", "user": "U01234567", "text": "See attachment",
					"files": []any{map[string]any{"title": "design", "filetype": "pdf"}},
				},
			},
			want: "slack message in C01234567 from U01234567: See attachment (1 file(s): design)",
		},
		{
			name: "unknown subtype",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "message", "subtype": "channel_join", "channel": "C01234567", "text": "Joined"},
			},
			want: "slack channel_join message in C01234567: Joined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SlackEnvelope(SlackEnvelopeInput{Body: tt.body, EventID: "event", TraceID: "trace"}).PayloadSummary
			if got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
			if len([]rune(got)) > 160 || strings.Contains(got, "\n") {
				t.Fatalf("summary must be a one-line maximum-160-rune string: %q", got)
			}
		})
	}
}

func TestSlackPayload(t *testing.T) {
	longText := strings.Repeat("a", 3000)
	tests := []struct {
		name        string
		body        map[string]any
		want        map[string]string
		omitted     []string
		wantTextLen int
		wantFiles   int
	}{
		{
			name: "bot message captures bot files and attachments",
			body: map[string]any{
				"team_id": "T01234567",
				"event": map[string]any{
					"type": "message", "subtype": "bot_message", "channel": "C01234567", "channel_type": "channel",
					"bot_id": "B01234567", "bot_profile": map[string]any{"name": "fallback-name"}, "username": "build-bot",
					"ts": "123.456", "event_ts": "123.457", "thread_ts": "123.000", "text": "Build passed",
					"files": []any{
						map[string]any{"name": "first.txt", "filetype": "text"},
						map[string]any{"name": "second.pdf", "filetype": "pdf"},
					},
					"attachments": []any{map[string]any{}, map[string]any{}},
				},
			},
			want: map[string]string{
				"kind": "message", "event_type": "message", "subtype": "bot_message", "team_id": "T01234567",
				"channel_id": "C01234567", "channel_type": "channel", "bot_id": "B01234567", "bot_name": "build-bot",
				"ts": "123.456", "event_ts": "123.457", "thread_ts": "123.000", "text": "Build passed",
				"file_count": "2", "attachment_count": "2",
			},
			omitted:   []string{"user_id", "root_ts"},
			wantFiles: 2,
		},
		{
			name: "changed message uses nested message fields",
			body: map[string]any{
				"team_id": "T01234567",
				"event": map[string]any{
					"type": "message", "subtype": "message_changed", "channel": "C01234567", "event_ts": "124.000",
					"message": map[string]any{
						"user": "U01234567", "ts": "123.456", "thread_ts": "123.000", "text": "Updated message",
						"edited": map[string]any{"user": "U01234567"},
					},
				},
			},
			want: map[string]string{
				"kind": "message", "event_type": "message", "subtype": "message_changed", "team_id": "T01234567",
				"channel_id": "C01234567", "user_id": "U01234567", "ts": "123.456", "event_ts": "124.000",
				"thread_ts": "123.000", "text": "Updated message", "edited_by": "U01234567",
			},
		},
		{
			name: "deleted message omits text",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "message", "subtype": "message_deleted", "channel": "C01234567", "deleted_ts": "123.456"},
			},
			want:    map[string]string{"deleted_ts": "123.456"},
			omitted: []string{"text"},
		},
		{
			name: "thread broadcast captures root timestamp",
			body: map[string]any{
				"team_id": "T01234567",
				"event": map[string]any{
					"type": "message", "subtype": "thread_broadcast", "channel": "C01234567", "user": "U01234567",
					"ts": "123.456", "text": "Broadcast", "root": map[string]any{"ts": "123.000"},
				},
			},
			want: map[string]string{"root_ts": "123.000"},
		},
		{
			name: "long text is capped and marked",
			body: map[string]any{
				"team_id": "T01234567",
				"event":   map[string]any{"type": "message", "channel": "C01234567", "user": "U01234567", "text": longText},
			},
			want:        map[string]string{"body_truncated": "true"},
			wantTextLen: 2048,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := decodePayload(t, SlackEnvelope(SlackEnvelopeInput{Body: tt.body, EventID: "event", TraceID: "trace"}).Payload)
			for key, want := range tt.want {
				if got := payload[key]; got != want {
					t.Fatalf("payload[%q] = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.omitted {
				if _, found := payload[key]; found {
					t.Fatalf("payload unexpectedly contains %q: %q", key, payload[key])
				}
			}
			if tt.wantTextLen != 0 && len([]rune(payload["text"])) != tt.wantTextLen {
				t.Fatalf("text has %d runes, want %d", len([]rune(payload["text"])), tt.wantTextLen)
			}
			if tt.wantFiles != 0 && len(strings.Split(payload["files"], ",")) != tt.wantFiles {
				t.Fatalf("files = %q, want %d pairs", payload["files"], tt.wantFiles)
			}
		})
	}
}

func TestGhostWisprSummary(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		body      map[string]any
		want      string
	}{
		{
			name:      "session started",
			eventType: "session_started",
			body:      map[string]any{"payload": map[string]any{"session_id": "session-start"}},
			want:      "ghostwispr session_started for session session-start",
		},
		{
			name:      "session ended with title",
			eventType: "session_ended",
			body:      map[string]any{"payload": map[string]any{"session_id": "session-end", "title": "Retrospective\nignored"}},
			want:      "ghostwispr session_ended for session session-end: Retrospective",
		},
		{
			name:      "summary ready with title",
			eventType: "summary_ready",
			body:      map[string]any{"payload": map[string]any{"session_id": "session-summary", "title": "Weekly summary"}},
			want:      "ghostwispr summary_ready for session session-summary: Weekly summary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GhostWisprEnvelope(GhostWisprEnvelopeInput{
				EventType: tt.eventType, Delivery: "delivery", EventID: "event", TraceID: "trace", Body: tt.body,
			}).PayloadSummary
			if got != tt.want {
				t.Fatalf("summary = %q, want %q", got, tt.want)
			}
			if len([]rune(got)) > 160 || strings.Contains(got, "\n") {
				t.Fatalf("summary must be a one-line maximum-160-rune string: %q", got)
			}
		})
	}
}

func TestGhostWisprPayload(t *testing.T) {
	longSummary := strings.Repeat("s", 3000)
	tests := []struct {
		name           string
		eventType      string
		body           map[string]any
		want           map[string]string
		omitted        []string
		wantSummaryLen int
	}{
		{
			name:      "session started",
			eventType: "session_started",
			body: map[string]any{
				"created_at": "2026-09-07T10:00:00Z",
				"payload":    map[string]any{"session_id": "session-start", "title": "Start", "type": "session_started"},
			},
			want: map[string]string{
				"event_type": "session_started", "session_id": "session-start", "title": "Start", "created_at": "2026-09-07T10:00:00Z",
			},
			omitted: []string{"status", "summary", "summary_preset"},
		},
		{
			name:      "session ended",
			eventType: "session_ended",
			body: map[string]any{
				"created_at": "2026-09-07T10:01:00Z",
				"payload":    map[string]any{"session_id": "session-end", "duration": float64(51.05), "type": "session_ended"},
			},
			want: map[string]string{
				"event_type": "session_ended", "session_id": "session-end", "duration": "51.05", "created_at": "2026-09-07T10:01:00Z",
			},
		},
		{
			name:      "summary ready carries capped summary metadata",
			eventType: "summary_ready",
			body: map[string]any{
				"created_at": "2026-09-07T10:02:00Z",
				"payload": map[string]any{
					"session_id": "session-summary", "title": "Summary", "duration": float64(60),
					"status": "completed", "summary": longSummary, "summary_preset": "default",
					"timestamp": "2026-09-07T10:02:00.000Z", "version": float64(2), "type": "summary_ready",
				},
			},
			want: map[string]string{
				"event_type": "summary_ready", "session_id": "session-summary", "title": "Summary", "duration": "60",
				"created_at": "2026-09-07T10:02:00Z", "status": "completed", "body_truncated": "true",
				"summary_preset": "default", "timestamp": "2026-09-07T10:02:00.000Z", "version": "2", "payload_type": "summary_ready",
			},
			wantSummaryLen: 2048,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := decodePayload(t, GhostWisprEnvelope(GhostWisprEnvelopeInput{
				EventType: tt.eventType, Delivery: "delivery", EventID: "event", TraceID: "trace", Body: tt.body,
			}).Payload)
			for key, want := range tt.want {
				if got := payload[key]; got != want {
					t.Fatalf("payload[%q] = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.omitted {
				if _, found := payload[key]; found {
					t.Fatalf("payload unexpectedly contains %q: %q", key, payload[key])
				}
			}
			if tt.wantSummaryLen != 0 && len([]rune(payload["summary"])) != tt.wantSummaryLen {
				t.Fatalf("summary has %d runes, want %d", len([]rune(payload["summary"])), tt.wantSummaryLen)
			}
		})
	}
}

func TestGithubSummaryUsesFullNameFallback(t *testing.T) {
	got := githubSummary("issues", map[string]any{
		"action":     "opened",
		"repository": map[string]any{"full_name": "example-org/example-repo"},
		"issue":      map[string]any{"number": 9, "title": "Fallback"},
	})
	const want = "issue opened: example-org/example-repo#9 Fallback"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestPayloadJSONOmitsEmptyFields(t *testing.T) {
	got := payloadJSON(map[string]string{"present": "value", "empty": ""})
	const want = `{"present":"value"}`
	if got != want {
		t.Fatalf("payload = %s, want %s", got, want)
	}
}

func TestGithubPayloadUsesRepositoryPartsFallback(t *testing.T) {
	payload := decodePayload(t, GithubEnvelope(GithubEnvelopeInput{
		Event: "issues",
		Body: map[string]any{
			"action":     "opened",
			"repository": map[string]any{"name": "example-repo", "owner": map[string]any{"login": "example-org"}},
			"issue":      map[string]any{"number": 10},
		},
	}).Payload)
	if got, want := payload["repo"], "example-org/example-repo"; got != want {
		t.Fatalf("repo = %q, want %q", got, want)
	}
}
func TestOneLineSummarySkipsLeadingBlankLinesAndCapsRunes(t *testing.T) {
	long := strings.Repeat("界", 161)
	if got := OneLineSummary("\n \r\n  first useful line\nsecond line"); got != "first useful line" {
		t.Fatalf("summary = %q", got)
	}
	if got := OneLineSummary(long); got != strings.Repeat("界", 159)+"…" {
		t.Fatalf("rune-safe summary = %q", got)
	}
}
func TestFirstSkipsLeadingBlankLines(t *testing.T) {
	if got := first("\n \r\n\tfirst useful line\nsecond line", 160); got != "first useful line" {
		t.Fatalf("first = %q, want first useful line", got)
	}
}
