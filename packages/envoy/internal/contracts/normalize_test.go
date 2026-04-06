package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

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
		name string
		body string
		want bool
	}{
		{name: "prefix", body: "@legion please help", want: true},
		{name: "middle", body: "hello @legion", want: true},
		{name: "punctuation", body: "(@legion)", want: true},
		{name: "case", body: "@LEGION", want: true},
		{name: "suffix word", body: "@legionnaire", want: false},
		{name: "email", body: "user@legion.dev", want: false},
		{name: "empty", body: "", want: false},
	}
	for _, item := range tests {
		if got := ContainsMention(item.body, "@legion"); got != item.want {
			t.Fatalf("%s: expected %v, got %v", item.name, item.want, got)
		}
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
	if items[1].Topic != "notifications.slack.T123.C123.thread.1234567890.123456" {
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
	// channel mention + thread (using ts as fallback)
	if len(items) != 2 {
		t.Fatalf("expected 2 envelopes, got %d", len(items))
	}
	if items[0].Topic != "notifications.slack.T123.C123.mention" {
		t.Fatalf("unexpected channel topic: %s", items[0].Topic)
	}
	if items[1].Topic != "notifications.slack.T123.C123.thread.9999999999.000000" {
		t.Fatalf("unexpected thread topic: %s", items[1].Topic)
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
		{name: "over limit", input: "abcdef", max: 5, expected: "abcde"},
		{name: "empty", input: "", max: 500, expected: ""},
		{name: "emoji preserved", input: "\U0001f525\U0001f525\U0001f525", max: 2, expected: "\U0001f525\U0001f525"},
		{name: "mixed multibyte", input: "hello\U0001f30dworld", max: 6, expected: "hello\U0001f30d"},
		{name: "zero max", input: "hello", max: 0, expected: ""},
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
			var parsed map[string]string
			if err := json.Unmarshal([]byte(result), &parsed); err != nil {
				t.Fatalf("githubSummary returned invalid JSON: %v\nraw: %s", err, result)
			}
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
	if runes := []rune(parsed["body"]); len(runes) != 500 {
		t.Fatalf("expected body truncated to 500 chars, got %d", len(runes))
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
	if runes2 := []rune(parsed2["body"]); len(runes2) != 500 {
		t.Fatalf("expected emoji body truncated to 500 chars, got %d", len(runes2))
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

func TestGithubEnvelopesCICheckRunNoPRs(t *testing.T) {
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
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.ci" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
	if items[0].DedupeKey != "github.d1" {
		t.Fatalf("unexpected dedupe key: %s", items[0].DedupeKey)
	}
}

func TestGithubEnvelopesCICheckRunOnePR(t *testing.T) {
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
				"pull_requests": []any{
					map[string]any{"number": 42},
				},
			},
		},
	}, "@legion")
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.pr.42.ci" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
	if items[0].DedupeKey != "github.d1.pr.42" {
		t.Fatalf("unexpected dedupe key: %s", items[0].DedupeKey)
	}
}

func TestGithubEnvelopesCICheckRunMultiplePRs(t *testing.T) {
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
				"pull_requests": []any{
					map[string]any{"number": 42},
					map[string]any{"number": 43},
					map[string]any{"number": 44},
				},
			},
		},
	}, "@legion")
	if len(items) != 3 {
		t.Fatalf("expected 3 envelopes, got %d", len(items))
	}
	expectedTopics := []string{
		"notifications.github.sjawhar.legion.pr.42.ci",
		"notifications.github.sjawhar.legion.pr.43.ci",
		"notifications.github.sjawhar.legion.pr.44.ci",
	}
	expectedDedupeKeys := []string{
		"github.d1.pr.42",
		"github.d1.pr.43",
		"github.d1.pr.44",
	}
	for i, item := range items {
		if item.Topic != expectedTopics[i] {
			t.Fatalf("envelope[%d] unexpected topic: %s, want %s", i, item.Topic, expectedTopics[i])
		}
		if item.DedupeKey != expectedDedupeKeys[i] {
			t.Fatalf("envelope[%d] unexpected dedupe key: %s, want %s", i, item.DedupeKey, expectedDedupeKeys[i])
		}
	}
	seen := map[string]bool{}
	for _, item := range items {
		if seen[item.DedupeKey] {
			t.Fatalf("duplicate dedupe key: %s", item.DedupeKey)
		}
		seen[item.DedupeKey] = true
	}
}

func TestGithubEnvelopesCICheckSuiteOnePR(t *testing.T) {
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
	if len(items) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(items))
	}
	if items[0].Topic != "notifications.github.sjawhar.legion.pr.99.ci" {
		t.Fatalf("unexpected topic: %s", items[0].Topic)
	}
	if items[0].DedupeKey != "github.d2.pr.99" {
		t.Fatalf("unexpected dedupe key: %s", items[0].DedupeKey)
	}
}

func TestGhostWisprSubject(t *testing.T) {
	cases := []struct {
		recordingId string
		kind        string
		want        string
	}{
		{recordingId: "rec-abc123", kind: "transcript", want: "notifications.ghostwispr.rec-abc123.transcript"},
		{recordingId: "rec-abc123", kind: "summary", want: "notifications.ghostwispr.rec-abc123.summary"},
		{recordingId: "rec-xyz789", kind: "transcript", want: "notifications.ghostwispr.rec-xyz789.transcript"},
	}
	for _, item := range cases {
		got := GhostWisprSubject(item.recordingId, item.kind)
		if got != item.want {
			t.Fatalf("GhostWisprSubject(%s, %s) = %s, want %s", item.recordingId, item.kind, got, item.want)
		}
	}
}

func TestGhostWisprTopicPrefix(t *testing.T) {
	if GhostWisprTopicPrefix != "notifications.ghostwispr." {
		t.Fatalf("unexpected prefix: %s", GhostWisprTopicPrefix)
	}
	subject := GhostWisprSubject("rec-1", "transcript")
	if !strings.HasPrefix(subject, GhostWisprTopicPrefix) {
		t.Fatalf("subject %s does not start with prefix %s", subject, GhostWisprTopicPrefix)
	}
}

func TestGhostWisprSourceValidation(t *testing.T) {
	env := Envelope{
		EventID:        "evt-1",
		Source:         "ghostwispr",
		SourceEventID:  "src-1",
		Topic:          "notifications.ghostwispr.rec-1.transcript",
		DedupeKey:      "ghostwispr.src-1",
		IssuedAt:       NowMillis(),
		PayloadSummary: "{}",
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
