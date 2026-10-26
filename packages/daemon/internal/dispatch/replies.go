package dispatch

import (
	"context"
	"net/http"
	"net/url"
)

// The answer loop's writes, each made as the stopped agent's own session (sessionActor) rather than
// the daemon's (daemonActor): Dispatch accepts a reply to a delivery only from the session the
// delivery went to (packages/envoy/internal/dispatch/api message_delivery.go replyMessage,
// comment_delivery.go replyComment), and an answer reads as the agent's.

// Actor is a Dispatch writer as a read names it: a person ("user", their login) or an agent
// ("session", its session id).
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Comment is a comment as an ask read lists it: its id, its author and its body.
type Comment struct {
	ID     string `json:"id"`
	Author Actor  `json:"author"`
	Body   string `json:"body"`
}

// Ask is an ask as GET /api/v1/asks/{id} reads it: its id, its issue (nil for a document's ask),
// who opened it, and every reply on its thread, oldest first.
type Ask struct {
	ID       string
	IssueKey *string
	Author   Actor
	Replies  []Comment
}

// ReplyToMessage answers a targeted message's delivery attempt as session, the attempt's own
// (POST /api/v1/messages/{id}/reply). Dispatch keeps one reply per attempt: a second call for an
// answered attempt posts nothing and answers the stored reply, so a re-post after a restart is
// safe.
func (c *HTTPClient) ReplyToMessage(ctx context.Context, messageID string, attempt int, session, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/messages/"+url.PathEscape(messageID)+"/reply", struct {
		Actor   actor  `json:"actor"`
		Attempt int    `json:"attempt"`
		Body    string `json:"body"`
	}{Actor: sessionActor(session), Attempt: attempt, Body: body}, nil)
}

// ReplyToComment answers a mention's delivery attempt as session, the attempt's own (POST
// /api/v1/comments/{id}/reply), naming the mention it answers (target). Dispatch keeps one reply
// per attempt, so a re-post after a restart posts nothing.
func (c *HTTPClient) ReplyToComment(ctx context.Context, commentID, target string, attempt int, session, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/comments/"+url.PathEscape(commentID)+"/reply", struct {
		Actor   actor  `json:"actor"`
		Target  string `json:"target"`
		Attempt int    `json:"attempt"`
		Body    string `json:"body"`
	}{Actor: sessionActor(session), Target: target, Attempt: attempt, Body: body}, nil)
}

// ReplyOnAsk comments on the ask's thread of issue as session (POST /api/v1/issues/{key}/comments
// with ask_id). Dispatch keeps no key for it, so a caller reads the thread first (Ask) to post it
// once.
func (c *HTTPClient) ReplyOnAsk(ctx context.Context, issue, askID, session, body string) error {
	return c.request(ctx, http.MethodPost, "/api/v1/issues/"+url.PathEscape(issue)+"/comments", struct {
		Actor actor  `json:"actor"`
		Body  string `json:"body"`
		AskID string `json:"ask_id"`
	}{Actor: sessionActor(session), Body: body, AskID: askID}, nil)
}

// Ask reads an ask with its replies (GET /api/v1/asks/{id}).
func (c *HTTPClient) Ask(ctx context.Context, askID string) (Ask, error) {
	var response struct {
		Ask struct {
			ID       string  `json:"id"`
			IssueKey *string `json:"issue_key"`
			Author   Actor   `json:"author"`
		} `json:"ask"`
		Replies []Comment `json:"replies"`
	}
	if err := c.request(ctx, http.MethodGet, "/api/v1/asks/"+url.PathEscape(askID), nil, &response); err != nil {
		return Ask{}, err
	}
	return Ask{ID: response.Ask.ID, IssueKey: response.Ask.IssueKey, Author: response.Ask.Author, Replies: response.Replies}, nil
}
