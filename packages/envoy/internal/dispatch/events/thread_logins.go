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
// issue message's root up in_reply_to. A direct (issue-less) message keeps no row here, and names
// its thread root's author instead (api/thread_logins.go, directThreadLogins).
func ThreadLogins(ctx context.Context, q Queryer, threadID string) ([]string, error) {
	logins := []string{}
	if err := q.QueryRow(ctx, `
		select coalesce(array_agg(login order by login), '{}') from user_threads where thread_id = $1::uuid
	`, threadID).Scan(&logins); err != nil {
		return nil, fmt.Errorf("read the logins of thread %s: %w", threadID, err)
	}
	return logins, nil
}

// IssueThreadLogins is the status half of an issue event's thread_logins, for an event that
// closed or reopened the issue: every login with a Replies row on one of issueKey's open asks,
// sorted and never nil. The Replies list leaves out an open ask on an open issue assigned to that
// login (api/replies.go, myRepliesOwner), so a close or a reopen moves exactly those rows and no
// other. It reads the asks through asks_open and their rows through user_threads_thread, so it
// never scans user_threads.
//
// An issue event carries logins on an assignee move too, the old and the new assignee, since the
// same rule moves an open ask's row out of one Inbox and into the other; the two sets are joined
// by api/thread_logins.go's issueWriteThreadLogins, which is the whole of what an issue event
// names.
func IssueThreadLogins(ctx context.Context, q Queryer, issueKey string) ([]string, error) {
	logins := []string{}
	if err := q.QueryRow(ctx, `
		select coalesce(array_agg(distinct u.login order by u.login), '{}')
		from user_threads u
		where u.thread_id in (select a.id from asks a where a.issue_key = $1 and a.state = 'open')
	`, issueKey).Scan(&logins); err != nil {
		return nil, fmt.Errorf("read the logins of issue %s's open asks: %w", issueKey, err)
	}
	return logins, nil
}
