// Package ghbranch creates a branch on GitHub through its REST API, the way the daemon makes an
// issue's branch exist before any role of the issue pushes to it.
package ghbranch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// API is GitHub's REST API, which the daemon creates branches through.
const API = "https://api.github.com"

// bodyLimit bounds how much of an answer an error quotes.
const bodyLimit = 4096

// Create creates branch on repo at base's current commit, as token, against api. A branch GitHub
// already has is left where it is, and Create reports success: GitHub answers its create with 422
// "Reference already exists". Every other answer, to the read of base or to the create, is an error
// naming the repository, the ref, GitHub's status and its body.
func Create(ctx context.Context, client *http.Client, api, token string, repo ghrepo.Repository, branch, base string) error {
	endpoint := strings.TrimRight(api, "/") + "/repos/" + repo.String() + "/git"
	baseRef := "refs/heads/" + base
	status, body, err := call(ctx, client, token, http.MethodGet, endpoint+"/ref/heads/"+escapeRef(base), nil)
	if err != nil {
		return fmt.Errorf("read %s of %s: %w", baseRef, repo, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("read %s of %s: GitHub answered %d: %s", baseRef, repo, status, body)
	}
	var head struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &head); err != nil || head.Object.SHA == "" {
		return fmt.Errorf("read %s of %s: GitHub answered %d with no commit: %s", baseRef, repo, status, body)
	}

	ref := "refs/heads/" + branch
	create, err := json.Marshal(map[string]string{"ref": ref, "sha": head.Object.SHA})
	if err != nil {
		return err
	}
	status, body, err = call(ctx, client, token, http.MethodPost, endpoint+"/refs", create)
	if err != nil {
		return fmt.Errorf("create %s on %s at %s: %w", ref, repo, head.Object.SHA, err)
	}
	if status == http.StatusCreated || status == http.StatusUnprocessableEntity && alreadyExists(body) {
		return nil
	}
	return fmt.Errorf("create %s on %s at %s: GitHub answered %d: %s", ref, repo, head.Object.SHA, status, body)
}

// escapeRef escapes each segment of a ref name for a URL path, keeping the slashes between them.
func escapeRef(name string) string {
	segments := strings.Split(name, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// alreadyExists says whether a 422's body is GitHub's answer to a ref it already has. GitHub answers
// other refusals of a create with 422 too, such as a malformed ref name or an unknown commit.
func alreadyExists(body []byte) bool {
	var refusal struct {
		Message string `json:"message"`
	}
	return json.Unmarshal(body, &refusal) == nil && refusal.Message == "Reference already exists"
}

// call makes one request and returns GitHub's status and the start of its body, trimmed.
func call(ctx context.Context, client *http.Client, token, method, url string, body []byte) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "legion-daemon")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, bodyLimit))
	if err != nil {
		return 0, nil, fmt.Errorf("read GitHub's %d answer: %w", response.StatusCode, err)
	}
	return response.StatusCode, bytes.TrimSpace(answer), nil
}
