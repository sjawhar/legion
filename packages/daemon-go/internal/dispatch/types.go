// Package dispatch is the Go daemon's bounded client for Dispatch's native HTTP API.
package dispatch

import (
	"context"
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

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}
