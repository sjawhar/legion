// Package dispatch is the broker's thin client for the Dispatch routes the broker needs: open an
// ask on an issue, read an ask, resolve a human's bearer, and find or create an operator's
// standing secrets issue. It never answers, edits or resolves.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// actor is the session every broker write names. Dispatch refuses a bearer-authenticated write
// that names no session actor (400 ACTOR_KIND), so every write body embeds brokerActor through
// this one type rather than each route spelling the field itself.
type actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

var brokerActor = actor{Kind: "session", ID: "agent-secrets-broker"}

// Error is a Dispatch call that did not succeed. Status is the HTTP status Dispatch answered
// with, or 0 when no response arrived (a transport failure or timeout, carried in Err). Code is
// Dispatch's own error code when its body carried one.
type Error struct {
	Method, Path string
	Status       int
	Code         string
	Message      string
	Err          error
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("dispatch %s %s: %v", e.Method, e.Path, e.Err)
	}
	return fmt.Sprintf("dispatch %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Unavailable reports whether Dispatch could not answer at all: no response, or a 5xx.
func (e *Error) Unavailable() bool { return e.Status == 0 || e.Status >= 500 }

// AsError returns err's *Error when it carries one.
func AsError(err error) (*Error, bool) {
	var dispatchErr *Error
	ok := errors.As(err, &dispatchErr)
	return dispatchErr, ok
}

func New(baseURL, token string, client *http.Client) *Client {
	return &Client{base: baseURL, token: token, http: client}
}

type createAskBody struct {
	Question string   `json:"question"`
	Options  []Option `json:"options"`
	Urgency  string   `json:"urgency"`
	Multiple bool     `json:"multiple"`
	Actor    actor    `json:"actor"`
}

func (c *Client) CreateAsk(ctx context.Context, issue, question string, options []Option, urgency string) (Ask, error) {
	body, err := json.Marshal(createAskBody{Question: question, Options: options, Urgency: urgency, Actor: brokerActor})
	if err != nil {
		return Ask{}, err
	}
	var ask Ask
	err = c.do(ctx, http.MethodPost, "/api/v1/issues/"+url.PathEscape(issue)+"/asks", c.token, bytes.NewReader(body), &ask)
	return ask, err
}

// GetAsk reads one ask. Dispatch answers GET /api/v1/asks/{id} with the ask nested under "ask"
// beside its replies, edits and followers; an answer naming any other ask is refused rather than
// trusted, so a zero or foreign ask can never reach a caller deciding a request on it.
func (c *Client) GetAsk(ctx context.Context, id string) (Ask, error) {
	var body struct {
		Ask Ask `json:"ask"`
	}
	path := "/api/v1/asks/" + url.PathEscape(id)
	if err := c.do(ctx, http.MethodGet, path, c.token, nil, &body); err != nil {
		return Ask{}, err
	}
	if body.Ask.ID != id {
		return Ask{}, fmt.Errorf("dispatch GET %s: response names ask %q", path, body.Ask.ID)
	}
	return body.Ask, nil
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
	Actor    actor    `json:"actor"`
}

type createIssueResult struct {
	Key string `json:"key"`
}

// CreateIssue opens an issue and returns its key.
func (c *Client) CreateIssue(ctx context.Context, project, title string, assignee *string, labels []string) (string, error) {
	body, err := json.Marshal(createIssueBody{Project: project, Title: title, Assignee: assignee, Labels: labels, Actor: brokerActor})
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
	if err := c.do(ctx, http.MethodGet, "/api/v1/issues/"+url.PathEscape(issue), c.token, nil, &detail); err != nil {
		return "", err
	}
	if detail.Assignee == nil {
		return "", nil
	}
	return *detail.Assignee, nil
}

// do sends one request. A nil out discards a successful response's body.
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
		return &Error{Method: method, Path: path, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		slice, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var envelope struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(slice, &envelope)
		return &Error{Method: method, Path: path, Status: resp.StatusCode, Code: envelope.Code, Message: string(slice)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{Method: method, Path: path, Status: resp.StatusCode, Message: "decode response: " + err.Error()}
	}
	return nil
}
