// Package githubrest is READY's and the daemon's client for one repository's GitHub REST API,
// called with an installation token. appauth mints those tokens with its own client.
package githubrest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// RepositoryAPI is repository's REST base under endpoint, GitHub's API root; empty is
// https://api.github.com.
func RepositoryAPI(endpoint string, repository ghrepo.Repository) string {
	if endpoint == "" {
		endpoint = "https://api.github.com"
	}
	return strings.TrimRight(endpoint, "/") + "/repos/" + repository.String()
}

// Client calls one repository's REST API: API is its REST base (RepositoryAPI, whose empty
// endpoint is api.github.com).
type Client struct {
	Token string
	API   string
}

// Get reads path under the repository's REST base into into. An answer other than 2xx is an
// *Answer.
func (c Client) Get(ctx context.Context, path string, into any) error {
	_, err := c.call(ctx, http.MethodGet, c.API+path, nil, into)
	return err
}

// Post sends body, as JSON, to path under the repository's REST base, and reads GitHub's answer
// into into when into is not nil. An answer other than 2xx is an *Answer.
func (c Client) Post(ctx context.Context, path string, body, into any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode POST %s: %w", path, err)
	}
	_, err = c.call(ctx, http.MethodPost, c.API+path, encoded, into)
	return err
}

// GetPages reads every page of the JSON array GitHub answers at path, a hundred items to a page,
// following the Link header's rel="next" until GitHub names none.
func GetPages[T any](ctx context.Context, c Client, path string) ([]T, error) {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	var all []T
	for url := c.API + path + separator + "per_page=100"; url != ""; {
		var page []T
		next, err := c.call(ctx, http.MethodGet, url, nil, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		url = next
	}
	return all, nil
}

// call sends method to url, with body as JSON when it is not nil, reads a 2xx answer into into
// when into is not nil, and returns the next page's URL its Link header names, "" for none.
func (c Client) call(ctx context.Context, method, url string, body []byte, into any) (string, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, payload)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	request.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		path, _, _ := strings.Cut(strings.TrimPrefix(url, c.API), "?")
		return "", &Answer{Method: method, Path: path, Status: response.StatusCode, Body: strings.TrimSpace(string(answer)), RateLimited: RateLimited(response)}
	}
	if into == nil {
		return nextPage(response.Header.Get("Link")), nil
	}
	return nextPage(response.Header.Get("Link")), json.Unmarshal(answer, into)
}

// RateLimited says whether response is GitHub's rate limit: a 429, or a 403 carrying
// x-ratelimit-remaining: 0 or a retry-after.
func RateLimited(response *http.Response) bool {
	return response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusForbidden &&
		(response.Header.Get("X-RateLimit-Remaining") == "0" || response.Header.Get("Retry-After") != "")
}

// nextPage is the URL a Link header names rel="next", "" when it names none.
func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		target, params, found := strings.Cut(strings.TrimSpace(part), ";")
		if found && strings.Contains(params, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(target), "<>")
		}
	}
	return ""
}

// Answer is a GitHub REST answer other than 2xx: the request's method and path (its query left
// out), GitHub's HTTP status, the body GitHub sent with it, and whether it is a rate limit, which
// every further call on the same token meets until the limit resets.
type Answer struct {
	Method      string
	Path        string
	Status      int
	Body        string
	RateLimited bool
}

func (a *Answer) Error() string {
	return fmt.Sprintf("GitHub answered %s %s with %d: %s", a.Method, a.Path, a.Status, a.Body)
}
