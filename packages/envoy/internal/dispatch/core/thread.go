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

// DispatchInput captures every parameter the dispatch tool accepts.
type DispatchInput struct {
	Repo     string         `json:"repo,omitempty"`
	Parent   string         `json:"parent,omitempty"`
	Subject  string         `json:"subject"`
	Context  string         `json:"context"`
	Question string         `json:"question"`
	Origin   *Origin        `json:"origin,omitempty"`
	Ask      []QuestionInfo `json:"ask,omitempty"`
	Urgency  Urgency        `json:"urgency,omitempty"`
}

// DispatchResult is the tool's output payload.
type DispatchResult struct {
	Thread int    `json:"thread"`
	URL    string `json:"url"`
}

// ComputeRequestID hashes the (repo|parent|subject|context|question|urgency|ask)
// tuple to identify duplicate dispatch attempts. ask is included so two
// otherwise identical dispatches that attach different structured questions
// do not collapse onto the same thread; an empty ask hashes the same whether
// the caller omitted it or sent `[]`.
func ComputeRequestID(repo, parent, subject, context, question string, urgency Urgency, ask []QuestionInfo) string {
	if len(ask) == 0 {
		ask = nil
	}
	askJSON, _ := json.Marshal(ask)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", repo, parent, subject, context, question, urgency, askJSON)))
	return hex.EncodeToString(h[:])[:16]
}

// validateInput rejects a dispatch whose rendered thread the dashboard could
// not use: blank subject/context/question (the reader has no transcript to
// fall back on) or an urgency the marker parsers on both sides refuse — a
// thread written with one would exist on GitHub yet never appear on the
// dashboard.
func validateInput(input DispatchInput, urgency Urgency) error {
	for _, field := range []struct{ name, value string }{
		{"subject", input.Subject},
		{"context", input.Context},
		{"question", input.Question},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required and must not be blank", field.name)
		}
	}
	switch urgency {
	case UrgencyLow, UrgencyMed, UrgencyHigh, UrgencyBlocking:
		return nil
	default:
		return fmt.Errorf("invalid urgency %q: use low, med, high, or blocking", urgency)
	}
}

var (
	ignorableSubIssue  = regexp.MustCompile(`(?i)already.*sub.?issue|already exists`)
	ignorableEditError = regexp.MustCompile(`(?i)already|duplicate|exists`)
	dispatchLabel      = "dispatch-thread"
	// GitHub's own owner/name alphabet. Anything else (spaces, quotes,
	// search qualifiers) would be spliced verbatim into the dedupe search
	// query, so it is refused up front.
	repoSlugPattern = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)
)

// CreateThread executes the full dispatch orchestration: resolve the target
// repo, dedupe by request id, create the issue if needed, and — when a
// parent was given — link it as a sub-issue and append a breadcrumb to the
// parent comment. Returns the resulting thread number + URL.
//
// Repo resolution, first hit wins: a qualified parent ("owner/name#n")
// names its own repo; otherwise input.Repo is used. Neither present is an
// error — the caller (the MCP shim) is expected to fill Repo from the
// working directory.
func CreateThread(ctx context.Context, client *github.Client, input DispatchInput) (DispatchResult, error) {
	urgency := input.Urgency
	if urgency == "" {
		urgency = UrgencyMed
	}
	if err := validateInput(input, urgency); err != nil {
		return DispatchResult{}, err
	}

	var parent ParsedParent
	if input.Parent != "" {
		var err error
		parent, err = ParseParent(input.Parent)
		if err != nil {
			return DispatchResult{}, err
		}
	}

	repo := parent.Repo
	if repo == "" {
		repo = input.Repo
	}
	if repo == "" {
		return DispatchResult{}, fmt.Errorf("no repo: pass repo=owner/name (the shim fills it from the working directory when one is a GitHub repo)")
	}
	owner, name, ok := githubapi.SplitRepo(repo)
	if !ok || !repoSlugPattern.MatchString(repo) {
		return DispatchResult{}, fmt.Errorf("invalid repo slug %q: expected owner/name", repo)
	}

	requestID := ComputeRequestID(repo, input.Parent, input.Subject, input.Context, input.Question, urgency, input.Ask)
	existing, err := githubapi.SearchByRequestID(ctx, client, owner, name, requestID, dispatchLabel)
	if err != nil {
		return DispatchResult{}, err
	}
	var thread githubapi.IssueRef
	foundExisting := len(existing) > 0
	if foundExisting {
		thread = existing[0]
	} else {
		marker, err := BuildMetaMarker(MetaMarker{RequestID: requestID, Urgency: urgency, Origin: input.Origin, Ask: WithAskIDs(input.Ask, requestID)})
		if err != nil {
			return DispatchResult{}, err
		}
		body := BuildThreadBody(marker, input.Subject, input.Context, input.Question)
		thread, err = githubapi.IssueCreate(ctx, client, owner, name, input.Subject, body, []string{dispatchLabel})
		if err != nil {
			return DispatchResult{}, err
		}
	}

	if input.Parent != "" {
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
