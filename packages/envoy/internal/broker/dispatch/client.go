// Package dispatch is the broker's thin client for the Dispatch routes the broker needs: open an
// ask on an issue, read an ask, resolve a human's bearer, and find or create an operator's
// standing secrets issue. It never answers, edits or resolves.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	base  string
	token string
	http  *http.Client
}

type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Answer struct {
	User     string    `json:"user"`
	Selected []string  `json:"selected"`
	At       time.Time `json:"at"`
}

type Ask struct {
	ID       string  `json:"id"`
	State    string  `json:"state"`
	EditedAt *string `json:"edited_at"`
	Answer   *Answer `json:"answer"`
}

type Identity struct {
	Kind  string  `json:"kind"`
	Login string  `json:"login"`
	Owner *string `json:"owner"`
}

type IssueSummary struct {
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Assignee *string  `json:"assignee"`
	Labels   []string `json:"labels"`
}

func New(baseURL, token string, client *http.Client) *Client {
	return &Client{base: baseURL, token: token, http: client}
}

func (c *Client) CreateAsk(ctx context.Context, issue, question string, options []Option, urgency string) (Ask, error) {
	body, _ := json.Marshal(map[string]any{
		"question": question,
		"options":  options,
		"urgency":  urgency,
		"multiple": false,
		"actor":    map[string]string{"kind": "session", "id": "agent-secrets-broker"},
	})
	var ask Ask
	err := c.do(ctx, http.MethodPost, "/api/v1/issues/"+issue+"/asks", c.token, bytes.NewReader(body), &ask)
	return ask, err
}

func (c *Client) GetAsk(ctx context.Context, id string) (Ask, error) {
	var ask Ask
	err := c.do(ctx, http.MethodGet, "/api/v1/asks/"+id, c.token, nil, &ask)
	return ask, err
}

// ListIssues finds issues in project carrying label, repeating the label query parameter (a
// single value is fine) the way Dispatch's GET /api/v1/issues?label= expects.
func (c *Client) ListIssues(ctx context.Context, project, label string) ([]IssueSummary, error) {
	var issues []IssueSummary
	path := "/api/v1/issues?project=" + url.QueryEscape(project) + "&label=" + url.QueryEscape(label)
	err := c.do(ctx, http.MethodGet, path, c.token, nil, &issues)
	return issues, err
}

type createIssueBody struct {
	Project  string   `json:"project"`
	Title    string   `json:"title"`
	Assignee *string  `json:"assignee"`
	Labels   []string `json:"labels"`
}

type createIssueResult struct {
	Key string `json:"key"`
}

// CreateIssue opens an issue and returns its key.
func (c *Client) CreateIssue(ctx context.Context, project, title string, assignee *string, labels []string) (string, error) {
	body, err := json.Marshal(createIssueBody{Project: project, Title: title, Assignee: assignee, Labels: labels})
	if err != nil {
		return "", err
	}
	var result createIssueResult
	err = c.do(ctx, http.MethodPost, "/api/v1/issues", c.token, bytes.NewReader(body), &result)
	return result.Key, err
}

func (c *Client) Whoami(ctx context.Context, bearer string) (Identity, error) {
	var id Identity
	err := c.do(ctx, http.MethodGet, "/api/v1/whoami", bearer, nil, &id)
	return id, err
}

// issueDetail is the small slice of GET /api/v1/issues/{key}'s response IssueAssignee needs.
// That route's full response carries additional fields (title, status, labels, ...) this
// package has no use for, so it decodes into this local struct rather than IssueSummary, whose
// shape belongs to ListIssues's own contract.
type issueDetail struct {
	Assignee *string `json:"assignee"`
}

// IssueAssignee answers issue's current assignee login, or "" when the issue has none.
func (c *Client) IssueAssignee(ctx context.Context, issue string) (string, error) {
	var detail issueDetail
	if err := c.do(ctx, http.MethodGet, "/api/v1/issues/"+issue, c.token, nil, &detail); err != nil {
		return "", err
	}
	if detail.Assignee == nil {
		return "", nil
	}
	return *detail.Assignee, nil
}

func (c *Client) do(ctx context.Context, method, path, bearer string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("dispatch %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		slice, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("dispatch %s %s: %d %s", method, path, resp.StatusCode, slice)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
