package events

import (
	"context"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// AskEventPayload is the payload of an ask.opened, ask.answered or ask.resolved, carrying the
// ask's thread_logins read in q after the write that moved it.
func AskEventPayload(ctx context.Context, q Queryer, ask model.Ask, changes model.ReferenceChanges) (model.AskEventPayload, error) {
	payload := model.NewAskEventPayload(ask, changes)
	logins, err := ThreadLogins(ctx, q, ask.ID)
	if err != nil {
		return model.AskEventPayload{}, err
	}
	payload.ThreadLogins = logins
	return payload, nil
}

// ThreadLogins is the thread_logins an event names: every login with a Replies row
// (user_threads, migrations 0095-0097) on threadID, sorted. The rows are kept by triggers on the
// comment, message and ask writes, so a writer reads this in its own transaction after the write,
// and the list says whose Replies count the write can have moved. It is never nil, so a thread
// nobody has a row on is named as [] rather than left out. threadID is the thread as the
// triggers key it: an ask's id; a comment's ask, else the root it replies to, else itself; an
// issue message's root up in_reply_to. A writer whose write phase is one batch has the event's
// insert read them instead (ThreadLoginsOf).
func ThreadLogins(ctx context.Context, q Queryer, threadID string) ([]string, error) {
	logins := []string{}
	if err := q.QueryRow(ctx, threadLoginsQuery("$1"), threadID).Scan(&logins); err != nil {
		return nil, fmt.Errorf("read the logins of thread %s: %w", threadID, err)
	}
	return logins, nil
}

// IssueThreadLogins is the thread_logins of an issue event that closed or reopened the issue: every
// login with a Replies row on one of issueKey's open asks, sorted and never nil. The Replies count
// leaves out an open ask on an open issue (api/replies.go, myRepliesUnread), so a close or a reopen
// moves exactly those rows and no other. It reads the asks through asks_open and their rows through
// user_threads_thread, so it never scans user_threads.
func IssueThreadLogins(ctx context.Context, q Queryer, issueKey string) ([]string, error) {
	logins := []string{}
	if err := q.QueryRow(ctx, issueLoginsQuery("$1"), issueKey).Scan(&logins); err != nil {
		return nil, fmt.Errorf("read the logins of issue %s's open asks: %w", issueKey, err)
	}
	return logins, nil
}

// threadLoginsQuery is ThreadLogins' query, its thread id the parameter param.
func threadLoginsQuery(param string) string {
	return `select coalesce(array_agg(login order by login), '{}') from user_threads where thread_id = ` + param + `::uuid`
}

// issueLoginsQuery is IssueThreadLogins' query, its issue key the parameter param.
func issueLoginsQuery(param string) string {
	return `
		select coalesce(array_agg(distinct u.login order by u.login), '{}')
		from user_threads u
		where u.thread_id in (select a.id from asks a where a.issue_key = ` + param + ` and a.state = 'open')
	`
}

// Logins is the thread_logins an event's payload carries, read by the event's own insert
// (QueueAppendWithLogins) from user_threads as the transaction's earlier statements, and the
// triggers they fired, left it: the read ThreadLogins or IssueThreadLogins makes after the write,
// for a writer whose write phase is one batch, which reads nothing in Go between its statements.
type Logins struct {
	query func(param string) string
	key   string
}

// ThreadLoginsOf is ThreadLogins of threadID, read by the event's insert.
func ThreadLoginsOf(threadID string) *Logins {
	return &Logins{query: threadLoginsQuery, key: threadID}
}

// IssueLoginsOf is IssueThreadLogins of issueKey, read by the event's insert: after the asks a
// close withdrew or a reopen restored earlier in the same batch.
func IssueLoginsOf(issueKey string) *Logins {
	return &Logins{query: issueLoginsQuery, key: issueKey}
}

// withThreadLogins is payload, the payload of an event that names thread_logins, carrying logins.
func withThreadLogins(payload any, logins []string) (any, error) {
	switch typed := payload.(type) {
	case model.AskEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.AskAnsweredEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.CommentEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.MessageEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	case model.IssueEventPayload:
		typed.ThreadLogins = logins
		return typed, nil
	}
	return nil, fmt.Errorf("event payload %T names no thread_logins", payload)
}
