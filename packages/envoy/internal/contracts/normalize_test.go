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
	if item.Topic != "notifications.github.sjawhar.envoy.pr" {
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
