package events

import (
	"context"
	"fmt"
)

// ThreadLogins is the thread_logins an event names: every login with a Replies row
// (user_threads, migrations 0095-0097) on threadID, sorted. The rows are kept by triggers on the
// comment, message and ask writes, so a writer reads this in its own transaction after the write,
// and the list says whose Replies count the write can have moved. It is never nil, so a thread
// nobody has a row on is named as [] rather than left out. threadID is the thread as the
// triggers key it: an ask's id; a comment's ask, else the root it replies to, else itself; an
// issue message's root up in_reply_to.
func ThreadLogins(ctx context.Context, q Queryer, threadID string) ([]string, error) {
	logins := []string{}
	if err := q.QueryRow(ctx, `
		select coalesce(array_agg(login order by login), '{}') from user_threads where thread_id = $1::uuid
	`, threadID).Scan(&logins); err != nil {
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
	if err := q.QueryRow(ctx, `
		select coalesce(array_agg(distinct u.login order by u.login), '{}')
		from user_threads u
		where u.thread_id in (select a.id from asks a where a.issue_key = $1 and a.state = 'open')
	`, issueKey).Scan(&logins); err != nil {
		return nil, fmt.Errorf("read the logins of issue %s's open asks: %w", issueKey, err)
	}
	return logins, nil
}
