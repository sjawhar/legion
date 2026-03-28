package contracts

import (
	"fmt"
	"strings"
)

type GithubEnvelopeInput struct {
	Event    string
	Delivery string
	Body     map[string]any
	EventID  string
	TraceID  string
}

func GithubEnvelope(input GithubEnvelopeInput) Envelope {
	owner := nestedString(input.Body, "repository", "owner", "login")
	if owner == "" {
		owner = "unknown"
	}
	repo := nestedString(input.Body, "repository", "name")
	if repo == "" {
		repo = "unknown"
	}
	return Envelope{
		EventID:        input.EventID,
		Source:         "github",
		SourceEventID:  input.Delivery,
		Topic:          GithubSubject(owner, repo, githubKind(input.Event)),
		DedupeKey:      "github." + input.Delivery,
		IssuedAt:       NowMillis(),
		PayloadSummary: githubSummary(input.Event, input.Body),
		TraceID:        input.TraceID,
	}
}

type SlackEnvelopeInput struct {
	Body    map[string]any
	EventID string
	TraceID string
}

func SlackEnvelope(input SlackEnvelopeInput) Envelope {
	team := stringValue(input.Body["team_id"])
	if team == "" {
		team = "unknown"
	}
	event, _ := input.Body["event"].(map[string]any)
	channel := stringValue(event["channel"])
	if channel == "" {
		channel = "unknown"
	}
	return Envelope{
		EventID:        input.EventID,
		Source:         "slack",
		SourceEventID:  stringValue(input.Body["event_id"]),
		Topic:          SlackSubject(team, channel, slackKind(input.Body)),
		DedupeKey:      "slack." + stringValue(input.Body["event_id"]),
		IssuedAt:       NowMillis(),
		PayloadSummary: slackSummary(input.Body),
		TraceID:        input.TraceID,
	}
}

func githubKind(event string) string {
	switch event {
	case "pull_request":
		return "pr"
	case "issues":
		return "issue"
	case "push":
		return "push"
	case "check_run", "check_suite":
		return "ci"
	default:
		return "comment"
	}
}

func githubSummary(event string, body map[string]any) string {
	repo := nestedString(body, "repository", "full_name")
	switch event {
	case "pull_request":
		return fmt.Sprintf("PR #%v %s in %s: %s", nested(body, "pull_request", "number"), stringValue(body["action"]), repo, nestedString(body, "pull_request", "title"))
	case "issues":
		return fmt.Sprintf("Issue #%v %s in %s: %s", nested(body, "issue", "number"), stringValue(body["action"]), repo, nestedString(body, "issue", "title"))
	case "push":
		return fmt.Sprintf("Push to %s on %s", repo, stringValue(body["ref"]))
	case "check_run", "check_suite":
		return fmt.Sprintf("CI %s in %s", stringValue(body["action"]), repo)
	default:
		return fmt.Sprintf("Comment %s in %s", stringValue(body["action"]), repo)
	}
}

func slackKind(body map[string]any) string {
	event, _ := body["event"].(map[string]any)
	if stringValue(event["type"]) == "app_mention" {
		return "mention"
	}
	return "message"
}

func slackSummary(body map[string]any) string {
	event, _ := body["event"].(map[string]any)
	text := stringValue(event["text"])
	if len(text) > 160 {
		text = text[:160]
	}
	return strings.TrimSpace(fmt.Sprintf("Slack %s from %s in %s: %s", slackKind(body), stringValue(event["user"]), stringValue(event["channel"]), text))
}

func nested(body map[string]any, keys ...string) any {
	var cur any = body
	for _, key := range keys {
		item, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = item[key]
	}
	return cur
}

func nestedString(body map[string]any, keys ...string) string {
	return stringValue(nested(body, keys...))
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
