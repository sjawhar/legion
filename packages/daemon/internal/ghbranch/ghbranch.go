// Package ghbranch creates a branch on GitHub through its REST API, the way the daemon makes an
// issue's branch exist before any role of the issue pushes to it.
package ghbranch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// Create creates branch on repo, whose REST API github calls, at main's current commit. A branch
// GitHub already has is left where it is, and Create reports success: GitHub answers its create
// with 422 "Reference already exists". Every other answer, to the read of main or to the create,
// is an error naming the repository, the ref and GitHub's answer (a *githubrest.Answer, its status
// and body).
func Create(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, branch string) error {
	var main struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := github.Get(ctx, "/git/ref/heads/main", &main); err != nil {
		return fmt.Errorf("read refs/heads/main of %s: %w", repo, err)
	}
	if main.Object.SHA == "" {
		return fmt.Errorf("read refs/heads/main of %s: GitHub named no commit", repo)
	}

	ref := "refs/heads/" + branch
	err := github.Post(ctx, "/git/refs", map[string]string{"ref": ref, "sha": main.Object.SHA}, nil)
	var answer *githubrest.Answer
	if err == nil || errors.As(err, &answer) && answer.Status == http.StatusUnprocessableEntity && alreadyExists(answer.Body) {
		return nil
	}
	return fmt.Errorf("create %s on %s at %s: %w", ref, repo, main.Object.SHA, err)
}

// alreadyExists says whether a 422's body is GitHub's answer to a ref it already has. GitHub answers
// other refusals of a create with 422 too, such as a malformed ref name or an unknown commit.
func alreadyExists(body string) bool {
	var refusal struct {
		Message string `json:"message"`
	}
	return json.Unmarshal([]byte(body), &refusal) == nil && refusal.Message == "Reference already exists"
}
