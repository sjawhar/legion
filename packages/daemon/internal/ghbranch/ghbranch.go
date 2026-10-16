// Package ghbranch creates a branch on GitHub through its REST API, the way the daemon makes an
// issue's branch exist before any role of the issue pushes to it, and reads the repository's
// default branch, the base every issue's branch starts at.
package ghbranch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// Create creates branch on repo, whose REST API github calls, at the current commit of base, a
// branch name used verbatim (release/v7.1 is the ref refs/heads/release/v7.1, release#stable is
// itself and never release). A branch GitHub already has is left where it is, and Create reports
// success: GitHub answers its create with 422 "Reference already exists". Every other answer, to
// the read of base or to the create, is an error naming the repository, the ref and GitHub's
// answer (a *githubrest.Answer, its status and body).
func Create(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, branch, base string) error {
	var head struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := github.Get(ctx, "/git/ref/heads/"+refPath(base), &head); err != nil {
		return fmt.Errorf("read refs/heads/%s of %s: %w", base, repo, err)
	}
	if head.Object.SHA == "" {
		return fmt.Errorf("read refs/heads/%s of %s: GitHub named no commit", base, repo)
	}

	ref := "refs/heads/" + branch
	err := github.Post(ctx, "/git/refs", map[string]string{"ref": ref, "sha": head.Object.SHA}, nil)
	var answer *githubrest.Answer
	if err == nil || errors.As(err, &answer) && answer.Status == http.StatusUnprocessableEntity && alreadyExists(answer.Body) {
		return nil
	}
	return fmt.Errorf("create %s on %s at %s: %w", ref, repo, head.Object.SHA, err)
}

// refPath is branch as the path of its ref under /git/ref/heads/: every character a URL path would
// read as its own syntax (`#`, `?`, `%`, and the rest url.PathEscape escapes) escaped, and each `/`
// kept, since it separates the ref's own components, so GitHub reads the one ref branch names.
func refPath(branch string) string {
	return strings.ReplaceAll(url.PathEscape(branch), "%2F", "/")
}

// DefaultBranch is repo's default branch as GitHub names it (`default_branch` of `GET
// /repos/{owner}/{repo}`), read through github, the repository's REST API. An answer naming no
// branch is refused by name, as is every answer other than 2xx.
func DefaultBranch(ctx context.Context, github githubrest.Client, repo ghrepo.Repository) (string, error) {
	var info struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := github.Get(ctx, "", &info); err != nil {
		return "", fmt.Errorf("read the default branch of %s: %w", repo, err)
	}
	if info.DefaultBranch == "" {
		return "", fmt.Errorf("read the default branch of %s: GitHub named no default branch", repo)
	}
	return info.DefaultBranch, nil
}

// alreadyExists says whether a 422's body is GitHub's answer to a ref it already has. GitHub answers
// other refusals of a create with 422 too, such as a malformed ref name or an unknown commit.
func alreadyExists(body string) bool {
	var refusal struct {
		Message string `json:"message"`
	}
	return json.Unmarshal([]byte(body), &refusal) == nil && refusal.Message == "Reference already exists"
}
