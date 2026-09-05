package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/go-github/v66/github"

	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
)

// Prose caps. The reader is a human on a dashboard; longer than this is a
// transcript, not a question. Enforced before any GitHub call, never truncated.
const (
	ContextMaxChars  = 1200
	QuestionMaxChars = 800
)

// DispatchInput captures every parameter the dispatch tool accepts. Thread
// selects the mode: empty opens a thread, otherwise the call continues one.
type DispatchInput struct {
	Repo     string         `json:"repo,omitempty"`
	Parent   string         `json:"parent,omitempty"`
	Thread   string         `json:"thread,omitempty"`
	Subject  string         `json:"subject,omitempty"`
	Context  string         `json:"context"`
	Question string         `json:"question"`
	Origin   *Origin        `json:"origin,omitempty"`
	Ask      []QuestionInfo `json:"ask,omitempty"`
	Urgency  Urgency        `json:"urgency,omitempty"`
}

// DispatchResult is the tool's output payload. URL is always the issue URL;
// Comment is set when the call posted (or found) a follow-up comment.
type DispatchResult struct {
	Thread  int    `json:"thread"`
	URL     string `json:"url"`
	Comment string `json:"comment,omitempty"`
}

// Dispatch routes a call to CreateThread or ContinueThread by the presence of
// Thread. Both share the prose validation.
func Dispatch(ctx context.Context, client *github.Client, input DispatchInput) (DispatchResult, error) {
	if input.Thread != "" {
		return ContinueThread(ctx, client, input)
	}
	return CreateThread(ctx, client, input)
}

// ComputeRequestID hashes the (repo|parent|subject|context|question|urgency|ask)
// tuple to identify duplicate opening attempts. ask is included so two
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

// ComputeFollowUpRequestID hashes (repo|thread|context|question|ask) to
// identify duplicate follow-up attempts on one thread.
func ComputeFollowUpRequestID(repo string, thread int, context, question string, ask []QuestionInfo) string {
	if len(ask) == 0 {
		ask = nil
	}
	askJSON, _ := json.Marshal(ask)
	h := sha256.Sum256([]byte(fmt.Sprintf("follow-up|%s|%d|%s|%s|%s", repo, thread, context, question, askJSON)))
	return hex.EncodeToString(h[:])[:16]
}

// validateProse rejects blank or over-cap context/question, naming the field.
func validateProse(context, question string) error {
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{{"context", context, ContextMaxChars}, {"question", question, QuestionMaxChars}} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required and must not be blank", field.name)
		}
		if n := utf8.RuneCountInString(field.value); n > field.max {
			return fmt.Errorf("%s is %d characters; the limit is %d", field.name, n, field.max)
		}
	}
	return nil
}

// validateInput rejects an opening call whose rendered thread the dashboard
// could not use: blank subject (the reader has no transcript to fall back
// on), bad prose, or an urgency the marker parsers on both sides refuse.
func validateInput(input DispatchInput, urgency Urgency) error {
	if strings.TrimSpace(input.Subject) == "" {
		return fmt.Errorf("subject is required and must not be blank")
	}
	if err := validateProse(input.Context, input.Question); err != nil {
		return err
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

func splitValidRepo(repo string) (owner, name string, err error) {
	owner, name, ok := githubapi.SplitRepo(repo)
	if !ok || !repoSlugPattern.MatchString(repo) {
		return "", "", fmt.Errorf("invalid repo slug %q: expected owner/name", repo)
	}
	return owner, name, nil
}

// CreateThread opens a thread: resolve the target repo, dedupe by request id,
// create the issue if needed, and — when a parent was given — link it as a
// sub-issue and append a breadcrumb to the parent comment.
//
// Repo resolution, first hit wins: a qualified parent ("owner/name#n") names
// its own repo; otherwise input.Repo is used. Neither present is an error —
// the calling plugin is expected to fill Repo from the working directory.
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
		parsed, err := ParseParent(input.Parent)
		if err != nil {
			return DispatchResult{}, err
		}
		parent = parsed
	}

	repo := parent.Repo
	if repo == "" {
		repo = input.Repo
	}
	if repo == "" {
		return DispatchResult{}, fmt.Errorf("no repo: pass repo=owner/name (the plugin fills it from the working directory when one is a GitHub repo)")
	}
	owner, name, err := splitValidRepo(repo)
	if err != nil {
		return DispatchResult{}, err
	}

	requestID := ComputeRequestID(repo, input.Parent, input.Subject, input.Context, input.Question, urgency, input.Ask)
	existing, err := githubapi.SearchByRequestID(ctx, client, owner, name, requestID, dispatchLabel)
	if err != nil {
		return DispatchResult{}, err
	}
	foundExisting := len(existing) > 0
	var thread githubapi.IssueRef
	if foundExisting {
		thread = existing[0]
	} else if thread, err = openThread(ctx, client, owner, name, requestID, urgency, input); err != nil {
		return DispatchResult{}, err
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

// openThread renders the thread body under a dispatch:thread marker and
// creates the labelled issue.
func openThread(ctx context.Context, client *github.Client, owner, name, requestID string, urgency Urgency, input DispatchInput) (githubapi.IssueRef, error) {
	marker, err := BuildMetaMarker(MetaMarker{RequestID: requestID, Urgency: urgency, Origin: input.Origin, Ask: WithAskIDs(input.Ask, requestID)})
	if err != nil {
		return githubapi.IssueRef{}, err
	}
	body := BuildThreadBody(marker, input.Subject, input.Context, input.Question)
	return githubapi.IssueCreate(ctx, client, owner, name, input.Subject, body, []string{dispatchLabel})
}

// ContinueThread posts a follow-up turn on an existing open dispatch thread as
// a comment carrying a dispatch:ask marker. The same request id mechanism as
// CreateThread applies, searched across the thread's comments, so a retried
// follow-up posts once.
//
// Repo resolution, first hit wins: a qualified thread ("owner/name#n") names
// its own repo; otherwise input.Repo is used (the calling plugin fills it from
// the working directory for a bare-number thread).
func ContinueThread(ctx context.Context, client *github.Client, input DispatchInput) (DispatchResult, error) {
	if input.Subject != "" || input.Urgency != "" || input.Parent != "" {
		return DispatchResult{}, fmt.Errorf("thread cannot be combined with subject, urgency, or parent")
	}
	if err := validateProse(input.Context, input.Question); err != nil {
		return DispatchResult{}, err
	}
	ref, err := ParseThread(input.Thread)
	if err != nil {
		return DispatchResult{}, err
	}
	repo := ref.Repo
	if repo == "" {
		repo = input.Repo
	}
	if repo == "" {
		return DispatchResult{}, fmt.Errorf("no repo for thread #%d: pass thread=owner/name#%d (the plugin fills repo from the working directory when one is a GitHub repo)", ref.IssueNumber, ref.IssueNumber)
	}
	owner, name, err := splitValidRepo(repo)
	if err != nil {
		return DispatchResult{}, err
	}

	issue, err := githubapi.GetIssue(ctx, client, owner, name, ref.IssueNumber)
	if err != nil {
		return DispatchResult{}, err
	}
	if issue.PullRequest || ParseMetaMarker(issue.Body) == nil {
		return DispatchResult{}, fmt.Errorf("#%d is not a dispatch thread", ref.IssueNumber)
	}
	if issue.State != "open" {
		return DispatchResult{}, fmt.Errorf("#%d is closed; open a new thread", ref.IssueNumber)
	}

	requestID := ComputeFollowUpRequestID(repo, ref.IssueNumber, input.Context, input.Question, input.Ask)
	comments, err := githubapi.ListComments(ctx, client, owner, name, ref.IssueNumber)
	if err != nil {
		return DispatchResult{}, err
	}
	for _, c := range comments {
		if m := ParseAskMarker(c.Body); m != nil && m.RequestID == requestID {
			return DispatchResult{Thread: issue.Number, URL: issue.URL, Comment: c.URL}, nil
		}
	}

	marker, err := BuildAskMarker(AskMarker{RequestID: requestID, Origin: input.Origin, Ask: WithAskIDs(input.Ask, requestID)})
	if err != nil {
		return DispatchResult{}, err
	}
	comment, err := githubapi.CreateComment(ctx, client, owner, name, ref.IssueNumber, BuildFollowUpBody(marker, input.Context, input.Question))
	if err != nil {
		return DispatchResult{}, err
	}
	return DispatchResult{Thread: issue.Number, URL: issue.URL, Comment: comment.URL}, nil
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
