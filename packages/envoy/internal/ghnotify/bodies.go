package ghnotify

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
)

// item is one change on a thread, held as the body GitHub's webhook sends for it, so the envelopes
// come from contracts.GithubEnvelopes, the webhook route's own builder: the topic, the fields, the
// summary and the mention copies are the webhook's by construction (R3). GitHub's REST API returns
// the same comment, review, issue and pull request objects a webhook body holds.
type item struct {
	// at is when the change happened: a comment's or an issue event's created_at, a review's
	// submitted_at. A thread's changes are published in its order.
	at time.Time
	// event is the webhook event the body is, and the events_total kind.
	event string
	// sourceID is the envelope's SourceEventID, built from the object GitHub never renumbers, so
	// a change read twice, by two polls or two pollers, has one dedupe key.
	sourceID string
	body     map[string]any
}

// issueCommentItem is a new comment on an issue or a pull request: issue_comment, created. A pull
// request's issue object carries pull_request, which makes the topic pr.<n>.comment.
func issueCommentItem(comment, issue, repository map[string]any) (item, error) {
	objectID, at, err := idAndTime(comment, "created_at")
	if err != nil {
		return item{}, fmt.Errorf("issue comment: %w", err)
	}
	return item{
		at:       at,
		event:    "issue_comment",
		sourceID: "notifications.issue_comment." + objectID,
		body:     map[string]any{"action": "created", "comment": comment, "issue": issue, "repository": repository},
	}, nil
}

// reviewCommentItem is a new review comment: pull_request_review_comment, created.
func reviewCommentItem(comment, pull, repository map[string]any) (item, error) {
	objectID, at, err := idAndTime(comment, "created_at")
	if err != nil {
		return item{}, fmt.Errorf("review comment: %w", err)
	}
	return item{
		at:       at,
		event:    "pull_request_review_comment",
		sourceID: "notifications.pull_request_review_comment." + objectID,
		body:     map[string]any{"action": "created", "comment": comment, "pull_request": pull, "repository": repository},
	}, nil
}

// reviewItem is a submitted review: pull_request_review, submitted. The REST API answers the
// review's state in capitals (APPROVED); a webhook body holds it in lower case, so the copy in the
// body does too.
func reviewItem(review, pull, repository map[string]any) (item, error) {
	objectID, at, err := idAndTime(review, "submitted_at")
	if err != nil {
		return item{}, fmt.Errorf("review: %w", err)
	}
	review = maps.Clone(review)
	if state, ok := review["state"].(string); ok {
		review["state"] = strings.ToLower(state)
	}
	return item{
		at:       at,
		event:    "pull_request_review",
		sourceID: "notifications.pull_request_review." + objectID,
		body:     map[string]any{"action": "submitted", "review": review, "pull_request": pull, "repository": repository},
	}, nil
}

// issueEventItem is a close or a reopen, read from the issue events API: pull_request with the
// pull request as GitHub returns it now (so merged is GitHub's), or issues with the issue when pull
// is nil. The event's id names it, so a close read again after a restart has the same key.
func issueEventItem(event, issue, pull, repository map[string]any) (item, error) {
	objectID, at, err := idAndTime(event, "created_at")
	if err != nil {
		return item{}, fmt.Errorf("issue event: %w", err)
	}
	action, _ := event["event"].(string)
	if action != "closed" && action != "reopened" {
		return item{}, fmt.Errorf("issue event %s is %q, neither closed nor reopened", objectID, action)
	}
	if pull != nil {
		return item{
			at:       at,
			event:    "pull_request",
			sourceID: "notifications.pull_request.event." + objectID,
			body:     map[string]any{"action": action, "pull_request": pull, "repository": repository},
		}, nil
	}
	return item{
		at:       at,
		event:    "issues",
		sourceID: "notifications.issues.event." + objectID,
		body:     map[string]any{"action": action, "issue": issue, "repository": repository},
	}, nil
}

// envelopes is the change's envelopes as the webhook route makes them, mention copies first. Each
// call is a new event: a new event id and trace id.
func (it item) envelopes(trigger string) []contracts.Envelope {
	return contracts.GithubEnvelopes(contracts.GithubEnvelopeInput{
		Event:    it.event,
		Delivery: it.sourceID,
		Body:     it.body,
		EventID:  id.New(),
		TraceID:  id.New(),
	}, trigger)
}

// idAndTime reads an object's positive integer id and the RFC 3339 time in field.
func idAndTime(object map[string]any, field string) (string, time.Time, error) {
	objectID := contracts.GithubPRNumber(object["id"])
	if objectID == "" || objectID == "0" {
		return "", time.Time{}, fmt.Errorf("no id in %v", object["id"])
	}
	raw, _ := object[field].(string)
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%s %s: %q is not a time", objectID, field, raw)
	}
	return objectID, at, nil
}
