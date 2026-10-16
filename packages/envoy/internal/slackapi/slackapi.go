// Package slackapi calls Slack's Web API with a bot token. Its one call is chat.postMessage, which
// the listener's POST /v1/slack/post makes to reply in a thread; the token never leaves the
// listener.
package slackapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DefaultBaseURL is Slack's Web API, which ENVOY_SLACK_API_URL replaces (another Slack
// deployment's, or a stand-in a test serves).
const DefaultBaseURL = "https://slack.com/api"

// maxAnswerBytes bounds how much of Slack's answer is read; chat.postMessage answers a few KiB.
const maxAnswerBytes = 1 << 20

// ErrUnanswered is a call Slack did not answer: the request may have reached it, so what it asked
// for may have happened.
var ErrUnanswered = errors.New("Slack did not answer")

// Client calls Slack's Web API under baseURL with the bot token as its bearer.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New is a client of the Web API at baseURL (DefaultBaseURL for Slack's own), authenticating with
// the bot token token, over httpClient.
func New(baseURL, token string, httpClient *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: httpClient}
}

// Message is one reply chat.postMessage posts into a thread: the channel's id, the thread's root
// ts, and the text.
type Message struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
}

// Posted is what Slack answered a post with: the channel it posted in and the new message's ts.
type Posted struct {
	Channel string
	TS      string
}

// Error is Slack's refusal of a call: the method and the `error` code Slack answered, as Slack
// spells it (`not_in_channel`, `channel_not_found`, `ratelimited`).
type Error struct {
	Method string
	Code   string
}

func (e *Error) Error() string { return "Slack refused " + e.Method + ": " + e.Code }

// PostMessage posts m with chat.postMessage and answers the channel and ts Slack posted it at. A
// refusal is an *Error carrying Slack's code; a call Slack did not answer wraps ErrUnanswered; an
// answer that is not Slack's JSON names its status and the start of its body.
func (c *Client) PostMessage(ctx context.Context, m Message) (Posted, error) {
	const method = "chat.postMessage"
	body, err := json.Marshal(m)
	if err != nil {
		return Posted{}, fmt.Errorf("encode %s: %w", method, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, bytes.NewReader(body))
	if err != nil {
		return Posted{}, fmt.Errorf("build %s: %w", method, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response, err := c.http.Do(request)
	if err != nil {
		return Posted{}, fmt.Errorf("%w %s (%v): the reply may have been posted, and posting it again may put it in the thread twice", ErrUnanswered, method, err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes))
	if err != nil {
		return Posted{}, fmt.Errorf("%w %s whole (%v): the reply may have been posted, and posting it again may put it in the thread twice", ErrUnanswered, method, err)
	}
	var answer struct {
		OK      *bool  `json:"ok"`
		Error   string `json:"error"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || answer.OK == nil {
		return Posted{}, fmt.Errorf("%s answered HTTP %d with a body that is not Slack's JSON: %s", method, response.StatusCode, snippet(raw))
	}
	if !*answer.OK {
		return Posted{}, &Error{Method: method, Code: answer.Error}
	}
	if answer.TS == "" {
		return Posted{}, fmt.Errorf("%s answered ok with no ts", method)
	}
	return Posted{Channel: answer.Channel, TS: answer.TS}, nil
}

// snippet is the start of a body, on one line and at most 200 characters, for an error that quotes
// it.
func snippet(raw []byte) string {
	text := []rune(strings.Join(strings.Fields(string(raw)), " "))
	if len(text) > 200 {
		return string(text[:200]) + "…"
	}
	return string(text)
}
