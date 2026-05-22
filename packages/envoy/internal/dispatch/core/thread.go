package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-github/v66/github"

	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
)

// DispatchInput captures every parameter the envoy_dispatch tool accepts.
type DispatchInput struct {
	Parent  string         `json:"parent"`
	Subject string         `json:"subject"`
	Body    string         `json:"body"`
	Ask     []QuestionInfo `json:"ask,omitempty"`
	Urgency Urgency        `json:"urgency,omitempty"`
}

// DispatchResult is the tool's output payload.
type DispatchResult struct {
	Thread int    `json:"thread"`
	URL    string `json:"url"`
}

// ComputeRequestID hashes the (parent|subject|body|urgency|ask) tuple to
// identify duplicate dispatch attempts. ask is included so two otherwise
// identical dispatches that attach different structured questions do not
// collapse onto the same thread.
func ComputeRequestID(parent, subject, body string, urgency Urgency, ask []QuestionInfo) string {
	askJSON, _ := json.Marshal(ask)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s", parent, subject, body, urgency, askJSON)))
	return hex.EncodeToString(h[:])[:16]
}

var (
	ignorableSubIssue  = regexp.MustCompile(`(?i)already.*sub.?issue|already exists`)
	ignorableEditError = regexp.MustCompile(`(?i)already|duplicate|exists`)
	dispatchLabel      = "dispatch-thread"
)

// CreateThread executes the full dispatch orchestration: dedupe by request id,
// create issue if needed, link as sub-issue, and append a breadcrumb to the
// parent comment when applicable. Returns the resulting thread number + URL.
func CreateThread(ctx context.Context, client *github.Client, defaultRepo string, input DispatchInput) (DispatchResult, error) {
	urgency := input.Urgency
	if urgency == "" {
		urgency = UrgencyMed
	}
	parent, err := ParseParent(input.Parent)
	if err != nil {
		return DispatchResult{}, err
	}
	// A fully-qualified parent (e.g. "sjawhar/legion#42") overrides
	// dispatch.defaultRepo. Bare numbers fall back to the configured default.
	repo := parent.Repo
	if repo == "" {
		repo = defaultRepo
	}
	if repo == "" {
		return DispatchResult{}, fmt.Errorf("no repo: parent %q is bare and dispatch.defaultRepo is unset; pass <owner>/<repo>#<n> or configure a default", input.Parent)
	}
	owner, name, ok := githubapi.SplitRepo(repo)
	if !ok {
		return DispatchResult{}, fmt.Errorf("invalid repo slug: %s", repo)
	}

	requestID := ComputeRequestID(input.Parent, input.Subject, input.Body, urgency, input.Ask)
	existing, err := githubapi.SearchByRequestID(ctx, client, owner, name, requestID, dispatchLabel)
	if err != nil {
		return DispatchResult{}, err
	}
	var thread githubapi.IssueRef
	foundExisting := len(existing) > 0
	if foundExisting {
		thread = existing[0]
	} else {
		marker := BuildMetaMarker(MetaMarker{Urgency: urgency, RequestID: requestID, Ask: input.Ask})
		body := BuildThreadBody(marker, input.Subject, input.Body)
		thread, err = githubapi.IssueCreate(ctx, client, owner, name, input.Subject, body, []string{dispatchLabel})
		if err != nil {
			return DispatchResult{}, err
		}
	}

	if err := githubapi.AddSubIssue(ctx, client, owner, name, parent.IssueNumber, thread.Number); err != nil {
		if !(foundExisting && ignorableSubIssue.MatchString(err.Error())) {
			return DispatchResult{}, err
		}
	}

	if parent.CommentID != 0 {
		if err := updateBreadcrumb(ctx, client, owner, name, parent.CommentID, thread.Number); err != nil {
			if !(foundExisting && ignorableEditError.MatchString(err.Error())) {
				return DispatchResult{}, err
			}
		}
	}

	return DispatchResult{Thread: thread.Number, URL: thread.URL}, nil
}

func updateBreadcrumb(ctx context.Context, client *github.Client, owner, repo string, commentID int, thread int) error {
	body, err := githubapi.GetComment(ctx, client, owner, repo, int64(commentID))
	if err != nil {
		return err
	}
	next := breadcrumbBody(body, thread)
	if next == body {
		return nil
	}
	return githubapi.EditComment(ctx, client, owner, repo, int64(commentID), next)
}

func breadcrumbBody(body string, thread int) string {
	breadcrumb := fmt.Sprintf("→ #%d", thread)
	if strings.Contains(body, breadcrumb) {
		return body
	}
	return fmt.Sprintf("%s\n\n%s", body, breadcrumb)
}
