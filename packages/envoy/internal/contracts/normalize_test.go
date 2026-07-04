package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeSummary(t *testing.T, raw string) map[string]string {
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
			"team_id":  "T123",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":    "app_mention",
				"user":    "U123",
				"channel": "C123",
				"text":    "hello envoy",
			},
		},
	})
	if item.Topic != "notifications.slack.T123.C123.mention" {
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
	if items[0].Topic != "notifications.github.sjawhar.envoy.comment" {
		t.Fatalf("unexpected comment topic: %s", items[0].Topic)
	}
	if items[1].Topic != "notifications.github.sjawhar.envoy.mention" {
		t.Fatalf("unexpected mention topic: %s", items[1].Topic)
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
	if items[1].Topic != "notifications.github.sjawhar.envoy.mention" {
		t.Fatalf("unexpected mention topic: %s", items[1].Topic)
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
						"name": "legion",
						"owner": map[string]any{
							"login": "sjawhar",
						},
					},
					"sender": map[string]any{"login": "someone", "type": tt.senderType},
				},
			}, "@legion")
			if len(items) != 1 {
				t.Fatalf("expected 1 envelope, got %d", len(items))
			}
			if items[0].Topic != "notifications.github.sjawhar.legion.issue.42.sub_issue" {
				t.Fatalf("unexpected topic: %s", items[0].Topic)
			}
			payload := decodeSummary(t, items[0].PayloadSummary)
			if payload["action"] != tt.action {
				t.Fatalf("unexpected action in payload summary: %s", payload["action"])
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
	if items[0].Topic != "notifications.github.sjawhar.envoy.issue.99.comment" {
		t.Fatalf("unexpected topic[0]: %s", items[0].Topic)
	}
	if items[1].Topic != "notifications.github.sjawhar.envoy.issue.99.mention" {
		t.Fatalf("unexpected topic[1]: %s", items[1].Topic)
	}
	if items[2].Topic != "notifications.github.sjawhar.envoy.mention" {
		t.Fatalf("unexpected topic[2]: %s", items[2].Topic)
	}
}

func TestSlackEnvelopesThread(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T123",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":      "message",
				"user":      "U123",
				"channel":   "C123",
				"text":      "reply in thread",
				"thread_ts": "1234567890.123456",
			},
		},
	})
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T123.C123.message" {
		t.Fatalf("unexpected channel topic: %s", items[0].Topic)
	}
	// Thread topic must use normalized ts (dot→underscore) and kind suffix
	if items[1].Topic != "notifications.slack.T123.C123.thread.1234567890_123456.message" {
		t.Fatalf("unexpected thread topic: %s", items[1].Topic)
	}
	// Both envelopes share the same dedupe_key (critical for single-delivery guarantee)
	if items[0].DedupeKey != items[1].DedupeKey {
		t.Fatalf("dedupe keys differ: channel=%s thread=%s", items[0].DedupeKey, items[1].DedupeKey)
	}
}

func TestSlackEnvelopesThreadMention(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T123",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":      "app_mention",
				"user":      "U123",
				"channel":   "C123",
				"text":      "@bot help in thread",
				"thread_ts": "1234567890.123456",
			},
		},
	})
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T123.C123.mention" {
		t.Fatalf("unexpected channel topic: %s", items[0].Topic)
	}
	// Thread mention must use .mention kind suffix
	if items[1].Topic != "notifications.slack.T123.C123.thread.1234567890_123456.mention" {
		t.Fatalf("unexpected thread topic: %s", items[1].Topic)
	}
}

func TestSlackEnvelopesNoThread(t *testing.T) {
	items := SlackEnvelopes(SlackEnvelopeInput{
		EventID: "e1",
		TraceID: "t1",
		Body: map[string]any{
			"team_id":  "T123",
			"event_id": "Ev123",
			"event": map[string]any{
				"type":    "app_mention",
				"user":    "U123",
				"channel": "C123",
				"text":    "hello",
				"ts":      "9999999999.000000",
			},
		},
	})
	// No thread_ts present — only channel-level envelope, NO ts fallback
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope (no thread fallback), got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T123.C123.mention" {
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

func TestTruncateBody(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		max      int
		expected string
	}{
		{name: "under limit", input: "hello", max: 500, expected: "hello"},
		{name: "at limit", input: "abcde", max: 5, expected: "abcde"},
		{name: "over limit", input: "abcdef", max: 5, expected: "abcde... [truncated]"},
		{name: "empty", input: "", max: 500, expected: ""},
		{name: "emoji preserved", input: "\U0001f525\U0001f525\U0001f525", max: 2, expected: "\U0001f525\U0001f525... [truncated]"},
		{name: "mixed multibyte", input: "hello\U0001f30dworld", max: 6, expected: "hello\U0001f30d... [truncated]"},
		{name: "zero max", input: "hello", max: 0, expected: "... [truncated]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateBody(tt.input, tt.max)
			if got != tt.expected {
				t.Fatalf("truncateBody(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.expected)
			}
		})
	}
}

func TestGithubSummaryJSON(t *testing.T) {
	tests := []struct {
		name         string
		event        string
		body         map[string]any
		expectedKeys []string
		checkValues  map[string]string
	}{
		{
			name:  "issue_comment on issue",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"issue":      map[string]any{"number": 42, "title": "Bug report"},
				"comment": map[string]any{
					"body": "Looks good to me", "html_url": "https://github.com/sjawhar/legion/issues/42#issuecomment-1",
					"user": map[string]any{"login": "reviewer"},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "title", "parent_kind", "author", "body", "url"},
			checkValues: map[string]string{
				"kind": "comment", "action": "created", "repo": "sjawhar/legion", "number": "42",
				"title": "Bug report", "parent_kind": "issue", "author": "reviewer",
				"body": "Looks good to me", "url": "https://github.com/sjawhar/legion/issues/42#issuecomment-1",
			},
		},
		{
			name:  "issue_comment on PR",
			event: "issue_comment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"issue":      map[string]any{"number": 99, "title": "Add feature", "pull_request": map[string]any{}},
				"comment": map[string]any{
					"body": "LGTM", "html_url": "https://github.com/sjawhar/legion/pull/99#issuecomment-2",
					"user": map[string]any{"login": "dev"},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "title", "parent_kind", "author", "body", "url"},
			checkValues:  map[string]string{"kind": "comment", "parent_kind": "pr", "number": "99", "title": "Add feature"},
		},
		{
			name:  "pull_request_review_comment",
			event: "pull_request_review_comment",
			body: map[string]any{
				"action":       "created",
				"repository":   map[string]any{"full_name": "sjawhar/legion"},
				"pull_request": map[string]any{"number": 55, "title": "Refactor auth"},
				"comment": map[string]any{
					"body": "Nit: rename this variable", "html_url": "https://github.com/sjawhar/legion/pull/55#discussion_r1",
					"user": map[string]any{"login": "reviewer"},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "title", "parent_kind", "author", "body", "url"},
			checkValues:  map[string]string{"kind": "comment", "parent_kind": "pr", "number": "55", "title": "Refactor auth"},
		},
		{
			name:  "pull_request_review",
			event: "pull_request_review",
			body: map[string]any{
				"action":       "submitted",
				"repository":   map[string]any{"full_name": "sjawhar/legion"},
				"pull_request": map[string]any{"number": 77, "title": "Add metrics"},
				"review": map[string]any{
					"body": "Approved with minor comments", "html_url": "https://github.com/sjawhar/legion/pull/77#pullrequestreview-1",
					"state": "approved", "user": map[string]any{"login": "lead"},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "title", "parent_kind", "author", "body", "url", "state"},
			checkValues:  map[string]string{"kind": "review", "parent_kind": "pr", "state": "approved", "number": "77", "author": "lead"},
		},
		{
			name:  "pull_request",
			event: "pull_request",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"pull_request": map[string]any{
					"number": 10, "title": "New feature", "body": "This PR adds a new feature",
					"html_url": "https://github.com/sjawhar/legion/pull/10",
					"user":     map[string]any{"login": "author"},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "title", "author", "body", "url"},
			checkValues: map[string]string{
				"kind": "pr", "action": "opened", "number": "10", "title": "New feature",
				"author": "author", "body": "This PR adds a new feature", "url": "https://github.com/sjawhar/legion/pull/10",
			},
		},
		{
			name:  "issues",
			event: "issues",
			body: map[string]any{
				"action":     "opened",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"issue": map[string]any{
					"number": 5, "title": "Bug: crash on startup", "body": "Steps to reproduce...",
					"html_url": "https://github.com/sjawhar/legion/issues/5",
					"user":     map[string]any{"login": "reporter"},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "title", "author", "body", "url"},
			checkValues:  map[string]string{"kind": "issue", "action": "opened", "number": "5", "title": "Bug: crash on startup", "author": "reporter"},
		},
		{
			name:  "push",
			event: "push",
			body: map[string]any{
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"ref":        "refs/heads/main",
			},
			expectedKeys: []string{"kind", "repo", "ref"},
			checkValues:  map[string]string{"kind": "push", "repo": "sjawhar/legion", "ref": "refs/heads/main"},
		},
		{
			name:  "check_run",
			event: "check_run",
			body: map[string]any{
				"action":     "completed",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"check_run": map[string]any{
					"name":          "test",
					"status":        "completed",
					"conclusion":    "success",
					"pull_requests": []any{},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "name", "status", "conclusion"},
			checkValues:  map[string]string{"kind": "ci", "action": "completed", "repo": "sjawhar/legion", "number": "", "name": "test", "status": "completed", "conclusion": "success"},
		},
		{
			name:  "check_suite",
			event: "check_suite",
			body: map[string]any{
				"action":     "completed",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
				"check_suite": map[string]any{
					"status":        "completed",
					"conclusion":    "failure",
					"pull_requests": []any{},
				},
			},
			expectedKeys: []string{"kind", "action", "repo", "number", "status", "conclusion"},
			checkValues:  map[string]string{"kind": "ci", "action": "completed", "repo": "sjawhar/legion", "number": "", "status": "completed", "conclusion": "failure"},
		},
		{
			name:  "unknown event",
			event: "deployment",
			body: map[string]any{
				"action":     "created",
				"repository": map[string]any{"full_name": "sjawhar/legion"},
			},
			expectedKeys: []string{"kind", "action", "repo"},
			checkValues:  map[string]string{"kind": "unknown", "action": "created", "repo": "sjawhar/legion"},
		},
		{
			name:         "missing webhook data produces empty strings",
			event:        "issue_comment",
			body:         map[string]any{"action": "created"},
			expectedKeys: []string{"kind", "action", "repo", "number", "title", "parent_kind", "author", "body", "url"},
			checkValues: map[string]string{
				"kind": "comment", "action": "created", "repo": "", "number": "", "title": "",
				"parent_kind": "issue", "author": "", "body": "", "url": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := githubSummary(tt.event, tt.body)
			parsed := decodeSummary(t, result)
			if len(parsed) != len(tt.expectedKeys) {
				t.Fatalf("expected %d keys, got %d: %v", len(tt.expectedKeys), len(parsed), parsed)
			}
			for _, key := range tt.expectedKeys {
				if _, ok := parsed[key]; !ok {
					t.Fatalf("missing expected key %q in %v", key, parsed)
				}
			}
			for key, want := range tt.checkValues {
				if got := parsed[key]; got != want {
					t.Fatalf("key %q: got %q, want %q", key, got, want)
				}
			}
		})
	}
}

func TestGithubSummaryTruncation(t *testing.T) {
	longBody := strings.Repeat("a", 600)
	result := githubSummary("issue_comment", map[string]any{
		"action":     "created",
		"repository": map[string]any{"full_name": "sjawhar/legion"},
		"comment":    map[string]any{"body": longBody},
	})
	var parsed map[string]string
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	truncSuffix := "... [truncated]"
	body := parsed["body"]
	if !strings.HasSuffix(body, truncSuffix) {
		t.Fatalf("expected truncated body to end with %q", truncSuffix)
	}
	if runes := []rune(body); len(runes) != 500+len([]rune(truncSuffix)) {
		t.Fatalf("expected body truncated to 500 chars + suffix, got %d", len(runes))
	}

	// Verify multi-byte truncation preserves valid UTF-8
	longEmoji := strings.Repeat("\U0001f525", 600)
	result2 := githubSummary("pull_request", map[string]any{
		"action":       "opened",
		"repository":   map[string]any{"full_name": "sjawhar/legion"},
		"pull_request": map[string]any{"body": longEmoji},
	})
	var parsed2 map[string]string
	if err := json.Unmarshal([]byte(result2), &parsed2); err != nil {
		t.Fatalf("emoji truncation produced invalid JSON: %v", err)
	}
	if runes2 := []rune(parsed2["body"]); len(runes2) != 500+len([]rune(truncSuffix)) {
		t.Fatalf("expected emoji body truncated to 500 chars + suffix, got %d", len(runes2))
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

// CI events (check_run/check_suite) are no longer published raw to pr.<n>.ci.
// They fold into envoy_ci_state via the webhook handler's CIRecorder and are
// re-emitted as a debounced per-commit summary by the listener. GithubEnvelopes
// therefore returns no envelope for them; extraction is covered by
// TestGithubCIObservations below.
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
			o.CheckName != "unit-tests" || o.Status != "completed" || o.Conclusion != "failure" {
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

	t.Run("check_suite ignored", func(t *testing.T) {
		body := map[string]any{
			"action":     "completed",
			"repository": map[string]any{"name": "legion", "owner": map[string]any{"login": "sjawhar"}},
			"check_suite": map[string]any{
				"status":        "completed",
				"conclusion":    "success",
				"head_sha":      "abc",
				"pull_requests": []any{map[string]any{"number": 99}},
			},
		}
		if obs := GithubCIObservations("check_suite", body); len(obs) != 0 {
			t.Fatalf("check_suite must be ignored, got %d observations", len(obs))
		}
	})
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
		PayloadSummary: `{"event_type":"session_ended","session_id":"20260326041405"}`,
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
			"team_id":  "T123",
			"event_id": "Ev123",
			"event":    "not-an-object",
		},
	})
	if item.Topic != "notifications.slack.T123.unknown.message" {
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
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"workflow_run": map[string]any{
				"id":          float64(42),
				"name":        "CI",
				"path":        ".github/workflows/ci.yml",
				"head_branch": "main",
				"status":      "in_progress",
				"html_url":    "https://github.com/sjawhar/legion/actions/runs/42",
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.workflow.ci_yml.in_progress" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
	summary := decodeSummary(t, items[0].PayloadSummary)
	if summary["kind"] != "workflow" || summary["action"] != "in_progress" || summary["branch"] != "main" {
		t.Fatalf("unexpected summary: %v", summary)
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
				"full_name": "sjawhar/legion",
				"name":      "legion",
				"owner":     map[string]any{"login": "sjawhar"},
			},
			"workflow_run": map[string]any{
				"id":          float64(43),
				"name":        "Release Prod",
				"path":        ".github/workflows/release-prod.yaml",
				"head_branch": "main",
				"status":      "completed",
				"conclusion":  "success",
				"html_url":    "https://github.com/sjawhar/legion/actions/runs/43",
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.workflow.release-prod_yaml.completed" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
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
	// Verify summary has duration
	summary := decodeSummary(t, item.PayloadSummary)
	if summary["duration"] == "" {
		t.Fatal("expected duration in summary")
	}
	if summary["session_id"] != "20260326041405" {
		t.Fatalf("unexpected session_id in summary: %s", summary["session_id"])
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
	// Verify summary has title
	summary := decodeSummary(t, item.PayloadSummary)
	if summary["title"] != "How are we gonna do the" {
		t.Fatalf("unexpected title in summary: %s", summary["title"])
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
	summary := decodeSummary(t, item.PayloadSummary)
	if summary["session_id"] != "" {
		t.Fatalf("expected empty session_id in summary, got %q", summary["session_id"])
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
	var summary map[string]string
	if err := json.Unmarshal([]byte(item.PayloadSummary), &summary); err != nil {
		t.Fatalf("invalid summary JSON: %v", err)
	}
	if summary["session_id"] != "2026.03/26 041405" {
		t.Fatalf("unexpected session_id in summary: %q", summary["session_id"])
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
	var summary map[string]string
	if err := json.Unmarshal([]byte(item.PayloadSummary), &summary); err != nil {
		t.Fatalf("invalid summary JSON: %v", err)
	}
	if summary["event_type"] != "summary_ready" {
		t.Fatalf("unexpected normalized event_type in summary: %q", summary["event_type"])
	}
}

func TestGhostWisprSummaryTruncatesTitle(t *testing.T) {
	longTitle := strings.Repeat("a", 600)
	item := GhostWisprEnvelope(GhostWisprEnvelopeInput{
		EventType: "summary_ready",
		Delivery:  "gw-delivery-title",
		EventID:   "e-title",
		TraceID:   "t-title",
		Body: map[string]any{
			"payload": map[string]any{
				"session_id": "20260326041629",
				"title":      longTitle,
			},
		},
	})
	var summary map[string]string
	if err := json.Unmarshal([]byte(item.PayloadSummary), &summary); err != nil {
		t.Fatalf("invalid summary JSON: %v", err)
	}
	expectedLen := 500 + len([]rune("... [truncated]"))
	if got := len([]rune(summary["title"])); got != expectedLen {
		t.Fatalf("unexpected truncated title length: %d, want %d", got, expectedLen)
	}
}

func TestGithubPayloadNotTruncated(t *testing.T) {
	longBody := strings.Repeat("a", 600)
	input := GithubEnvelopeInput{
		Event:    "issue_comment",
		Delivery: "d1",
		EventID:  "e1",
		TraceID:  "t1",
		Body: map[string]any{
			"action":     "created",
			"repository": map[string]any{"full_name": "sjawhar/legion", "name": "legion", "owner": map[string]any{"login": "sjawhar"}},
			"issue":      map[string]any{"number": 42, "title": "Test issue"},
			"comment": map[string]any{
				"body":     longBody,
				"html_url": "https://github.com/sjawhar/legion/issues/42#issuecomment-1",
				"user":     map[string]any{"login": "commenter"},
			},
		},
	}
	env := GithubEnvelope(input)

	// PayloadSummary should still be truncated to 500 + suffix
	summary := decodeSummary(t, env.PayloadSummary)
	truncSuffix := "... [truncated]"
	expectedLen := 500 + len([]rune(truncSuffix))
	if runes := []rune(summary["body"]); len(runes) != expectedLen {
		t.Fatalf("expected summary body truncated to %d chars (500 + suffix), got %d", expectedLen, len(runes))
	}

	// Payload should contain the full body (600 chars), not truncated
	if env.Payload == "" {
		t.Fatal("expected Payload to be populated")
	}
	payload := decodeSummary(t, env.Payload)
	if runes := []rune(payload["body"]); len(runes) != 600 {
		t.Fatalf("expected payload body to be full 600 chars, got %d", len(runes))
	}
	if payload["body"] != longBody {
		t.Fatal("payload body does not match original")
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
			name:       "push has no payload",
			event:      "push",
			body:       map[string]any{"repository": map[string]any{"full_name": "sjawhar/legion"}, "ref": "refs/heads/main"},
			hasPayload: false,
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
