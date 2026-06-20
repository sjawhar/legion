package githubapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v66/github"
)

// IssueRef is a minimal pointer to a GitHub issue.
type IssueRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// IssueCreate creates a new issue with optional labels. Returns the issue
// number and HTML URL of the created issue.
func IssueCreate(ctx context.Context, client *github.Client, owner, repo, title, body string, labels []string) (IssueRef, error) {
	req := &github.IssueRequest{
		Title:  github.String(title),
		Body:   github.String(body),
		Labels: &labels,
	}
	issue, _, err := client.Issues.Create(ctx, owner, repo, req)
	if err != nil {
		return IssueRef{}, fmt.Errorf("create issue: %w", err)
	}
	return IssueRef{Number: issue.GetNumber(), URL: issue.GetHTMLURL()}, nil
}

// GetNodeID fetches the GraphQL node id of an issue. go-github populates
// node_id on issue responses.
func GetNodeID(ctx context.Context, client *github.Client, owner, repo string, number int) (string, error) {
	issue, _, err := client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("get issue node id: %w", err)
	}
	if issue.GetNodeID() == "" {
		return "", fmt.Errorf("issue %d has empty node id", number)
	}
	return issue.GetNodeID(), nil
}

// AddSubIssue links child as a sub-issue of parent via the GraphQL
// `addSubIssue` mutation. Both arguments are issue numbers.
func AddSubIssue(ctx context.Context, client *github.Client, owner, repo string, parent, child int) error {
	parentID, err := GetNodeID(ctx, client, owner, repo, parent)
	if err != nil {
		return err
	}
	childID, err := GetNodeID(ctx, client, owner, repo, child)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(
		`mutation { addSubIssue(input: {issueId: "%s", subIssueId: "%s"}) { issue { id } subIssue { id } } }`,
		parentID, childID,
	)
	// Use raw GraphQL via the client transport.
	body := map[string]any{"query": query}
	req, err := client.NewRequest("POST", "graphql", body)
	if err != nil {
		return fmt.Errorf("build graphql request: %w", err)
	}
	var resp struct {
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if _, err := client.Do(ctx, req, &resp); err != nil {
		return fmt.Errorf("add sub-issue: %w", err)
	}
	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("add sub-issue: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// GetComment fetches an issue comment body.
func GetComment(ctx context.Context, client *github.Client, owner, repo string, commentID int64) (string, error) {
	comment, _, err := client.Issues.GetComment(ctx, owner, repo, commentID)
	if err != nil {
		return "", fmt.Errorf("get comment: %w", err)
	}
	return comment.GetBody(), nil
}

// EditComment replaces an issue comment body.
func EditComment(ctx context.Context, client *github.Client, owner, repo string, commentID int64, body string) error {
	_, _, err := client.Issues.EditComment(ctx, owner, repo, commentID, &github.IssueComment{Body: github.String(body)})
	if err != nil {
		return fmt.Errorf("edit comment: %w", err)
	}
	return nil
}

// BuildRequestIDQuery builds the GitHub issue-search query used to find an
// existing dispatch thread by its request id. It searches for the raw request
// id token that core.BuildMetaMarker embeds in the issue body (the `requestId:
// <id>` frontmatter line), scoped to the dispatch label so unrelated issues
// that merely mention the id are ignored. The token must stay in sync with the
// marker writer — see TestRequestIDQueryMatchesMarker.
func BuildRequestIDQuery(owner, repo, requestID, label string) string {
	return fmt.Sprintf(`repo:%s/%s label:%s in:body "%s"`, owner, repo, label, requestID)
}

// SearchByRequestID returns dispatch threads whose body embeds the given
// request id, as written by core.BuildMetaMarker.
func SearchByRequestID(ctx context.Context, client *github.Client, owner, repo, requestID, label string) ([]IssueRef, error) {
	result, _, err := client.Search.Issues(ctx, BuildRequestIDQuery(owner, repo, requestID, label), nil)
	if err != nil {
		return nil, fmt.Errorf("search issues: %w", err)
	}
	refs := make([]IssueRef, 0, len(result.Issues))
	for _, issue := range result.Issues {
		refs = append(refs, IssueRef{Number: issue.GetNumber(), URL: issue.GetHTMLURL()})
	}
	return refs, nil
}
