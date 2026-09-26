// Package dispatch is the broker's thin client for the three Dispatch routes it needs: open an
// ask on an issue, read an ask, and resolve a human's bearer. It never answers, edits or resolves.
package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func New(baseURL, token string, client *http.Client) *Client {
	return &Client{base: baseURL, token: token, http: client}
}

func (c *Client) CreateAsk(ctx context.Context, issue, question string, options []Option, urgency string) (Ask, error) {
	body, _ := json.Marshal(map[string]any{"question": question, "options": options, "urgency": urgency, "multiple": false})
	var ask Ask
	err := c.do(ctx, http.MethodPost, "/api/v1/issues/"+issue+"/asks", c.token, bytes.NewReader(body), &ask)
	return ask, err
}

func (c *Client) GetAsk(ctx context.Context, id string) (Ask, error) {
	var ask Ask
	err := c.do(ctx, http.MethodGet, "/api/v1/asks/"+id, c.token, nil, &ask)
	return ask, err
}

func (c *Client) Whoami(ctx context.Context, bearer string) (Identity, error) {
	var id Identity
	err := c.do(ctx, http.MethodGet, "/api/v1/whoami", bearer, nil, &id)
	return id, err
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
