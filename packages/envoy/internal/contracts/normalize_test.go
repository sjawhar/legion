package contracts

import "testing"

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
				"number": 7,
				"title":  "hello",
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
				"body": "hello there",
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
				"body": "@legion please help",
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
				"body": "Can @legion take a look?",
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
				"body": "",
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
				"number": 42,
				"title":  "test",
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
			},
			"comment": map[string]any{
				"body": "just a comment",
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
			},
			"comment": map[string]any{
				"body": "@legion please review",
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
