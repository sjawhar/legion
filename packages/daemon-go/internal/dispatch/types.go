// Package dispatch is the Go daemon's bounded client for Dispatch's native HTTP API.
package dispatch

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Client is the Dispatch surface the workflow needs. It deliberately exposes only the reads and
// writes the daemon owns; callers preserve the order ListIssues returns when deciding admission.
type Client interface {
	ListIssues(ctx context.Context, project string, statuses []string) ([]IssueSummary, error)
	GetIssue(ctx context.Context, key string) (Issue, error)
	SetStatus(ctx context.Context, key, status string) error
	PostMessage(ctx context.Context, key, body string) error
	MessageBodiesSince(ctx context.Context, key string, since time.Time) ([]string, error)
	Approval(ctx context.Context, artifactID string) (Approval, error)
}

// IssueSummary is one issue from Dispatch's project list. Rank is Dispatch's fractional key;
// callers preserve the list's order when filtering.
type IssueSummary struct {
	Key    string
	Title  string
	Status string
	Parent *string
	Rank   string
	// LastSeq is how far Dispatch's own event log for the issue has run. A record whose applied
	// sequence is behind it has events still to come on the stream, which carry the actor this
	// snapshot does not.
	LastSeq int64
}

// Issue is the workflow data from Dispatch's issue-detail route.
type Issue struct {
	Key               string
	Project           string
	Title             string
	Status            string
	Parent            *string
	PrimaryArtifactID string
	LastSeq           int64
}

// Approval is the current approval state Dispatch derives for a document, and the issue carrying
// the document (empty for a project document).
type Approval struct {
	IssueKey      string
	State         string
	LatestVersion int
	Version       *int
	Reason        *string
}

// Error is a non-success response from Dispatch. Code is empty only when the response did not
// carry Dispatch's normal error body.
type Error struct {
	Status  int
	Code    string
	Message string
}

// Missing says whether a Dispatch call failed because what it asked for is not there, as against
// a call that failed: the one answer a handler turns into its own 404 rather than a 502.
func Missing(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Status == http.StatusNotFound
}

// PermanentRefusal is a write Dispatch will refuse the same way however many times it is made:
// the issue is gone, or what it carries is something Dispatch will not take. Only a refusal
// Dispatch itself made counts, which is a 4xx carrying one of its error codes.
//
// Everything else is an outage a caller rides out: every 5xx, every transport failure, "come back
// later" (408, 429), a codeless 4xx from whatever sits in front of Dispatch, and a credential
// answer (401, 403). A revoked or expired token says nothing about the write — an operator
// restores it and the same body is taken — so reading one as permanent would drop the write for
// good.
func PermanentRefusal(err error) (*Error, bool) {
	var refusal *Error
	if !errors.As(err, &refusal) {
		return nil, false
	}
	switch {
	case refusal.Code == "",
		refusal.Status < 400 || refusal.Status >= 500,
		refusal.Status == http.StatusRequestTimeout,
		refusal.Status == http.StatusTooManyRequests,
		refusal.Status == http.StatusUnauthorized,
		refusal.Status == http.StatusForbidden:
		return nil, false
	}
	return refusal, true
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}
