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

// Create creates branch on repo, whose REST API github calls, at main's current commit, with any
// top-level .legion/ stripped from that commit first: before this branch exists, nothing has run
// any role of its issue, so a .legion/ directory already on main holds only another issue's merged
// handoffs, never this one's (dispatch://LEGION-565). main with no .legion/ entry is branched
// unchanged, so a repository that carries none never gets a needless extra commit. A branch GitHub
// already has is left where it is, and Create reports success: GitHub answers its create with 422
// "Reference already exists". Every other answer, to the read of main, the read of its tree, the
// strip, or the create, is an error naming the repository, the ref and GitHub's answer (a
// *githubrest.Answer, its status and body).
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

	sha, err := stripLegion(ctx, github, repo, main.Object.SHA)
	if err != nil {
		return err
	}

	ref := "refs/heads/" + branch
	err = github.Post(ctx, "/git/refs", map[string]string{"ref": ref, "sha": sha}, nil)
	var answer *githubrest.Answer
	if err == nil || errors.As(err, &answer) && answer.Status == http.StatusUnprocessableEntity && alreadyExists(answer.Body) {
		return nil
	}
	return fmt.Errorf("create %s on %s at %s: %w", ref, repo, sha, err)
}

// stripLegion is base unchanged, or a new commit on top of it with its top-level .legion/ entry
// deleted, when base's own top-level tree carries one. The deletion is a single git-tree-API entry
// (path ".legion", sha null) against base_tree base, so every other top-level entry of base's tree
// carries over untouched regardless of how many files or subdirectories .legion/ holds.
func stripLegion(ctx context.Context, github githubrest.Client, repo ghrepo.Repository, base string) (string, error) {
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
		} `json:"tree"`
	}
	if err := github.Get(ctx, "/git/trees/"+base, &tree); err != nil {
		return "", fmt.Errorf("read the top-level tree of %s at %s: %w", repo, base, err)
	}
	hasLegion := false
	for _, entry := range tree.Tree {
		if entry.Path == ".legion" {
			hasLegion = true
			break
		}
	}
	if !hasLegion {
		return base, nil
	}

	var stripped struct {
		SHA string `json:"sha"`
	}
	deletion := map[string]any{
		"base_tree": base,
		"tree":      []map[string]any{{"path": ".legion", "mode": "040000", "type": "tree", "sha": nil}},
	}
	if err := github.Post(ctx, "/git/trees", deletion, &stripped); err != nil {
		return "", fmt.Errorf("strip .legion/ from %s's tree at %s: %w", repo, base, err)
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	body := map[string]any{
		"message": "legion: start this branch without main's .legion/\n\n" +
			"Another issue's merged handoffs were still on main; this branch never carries them forward.\n",
		"tree":    stripped.SHA,
		"parents": []string{base},
	}
	if err := github.Post(ctx, "/git/commits", body, &commit); err != nil {
		return "", fmt.Errorf("commit %s without .legion/ for %s: %w", base, repo, err)
	}
	return commit.SHA, nil
}

// alreadyExists says whether a 422's body is GitHub's answer to a ref it already has. GitHub answers
// other refusals of a create with 422 too, such as a malformed ref name or an unknown commit.
func alreadyExists(body string) bool {
	var refusal struct {
		Message string `json:"message"`
	}
	return json.Unmarshal([]byte(body), &refusal) == nil && refusal.Message == "Reference already exists"
}
