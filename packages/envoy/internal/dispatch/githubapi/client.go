// Package githubapi wraps go-github operations used by the Dispatch server
// and reverse-proxies GitHub API calls for the dashboard SPA.
package githubapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/go-github/v66/github"
)

// NewClient returns a go-github client authenticated with the given bearer
// token. The token is used verbatim in `Authorization: Bearer <token>`.
func NewClient(ctx context.Context, token string) *github.Client {
	httpClient := &http.Client{Transport: &bearerTransport{token: token, base: http.DefaultTransport}}
	return github.NewClient(httpClient)
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if clone.Header.Get("Authorization") == "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	if clone.Header.Get("Accept") == "" {
		clone.Header.Set("Accept", "application/vnd.github+json")
	}
	return t.base.RoundTrip(clone)
}

// SplitRepo turns "owner/repo" into its parts. Returns ("", "", false) on
// invalid input.
func SplitRepo(slug string) (string, string, bool) {
	idx := strings.Index(slug, "/")
	if idx <= 0 || idx == len(slug)-1 {
		return "", "", false
	}
	owner, name := slug[:idx], slug[idx+1:]
	if strings.Contains(name, "/") {
		return "", "", false
	}
	return owner, name, true
}
