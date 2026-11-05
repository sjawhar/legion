package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// issueThreadLogins is the status half of an issue event's thread_logins: whose Replies count a
// status move can change. Only a close or a reopen (closedMoved) changes one, for the logins with
// a Replies row on the issue's open asks (events.IssueThreadLogins); any other status move names
// nobody, as [] rather than left out, so a reader knows the event says so. A close withdraws the
// issue's open asks and a reopen restores them before this read, and each of those ask events
// names its own thread's logins, so after a close this names only an open ask a close by an older
// release left behind.
func issueThreadLogins(ctx context.Context, tx pgx.Tx, issueKey string, closedMoved bool) ([]string, error) {
	if !closedMoved {
		return []string{}, nil
	}
	return events.IssueThreadLogins(ctx, tx, issueKey)
}

// issueWriteThreadLogins is the thread_logins of an issue.updated or issue.closed: every login
// whose Inbox rows the write can have moved, sorted and without a duplicate.
//
// A status move names issueThreadLogins' logins. An assignee move names the assignee it took the
// issue from and the one it gave it to (movedAssignees, either of which may be nil for an issue
// nobody held or nobody holds): the Replies list leaves out an open ask on an open issue assigned
// to the reader (api/replies.go, myRepliesOwner), so moving the assignee takes those asks out of
// the new assignee's list and puts them into the old one's. A write that moved both names the
// union of the two, which is why the result is sorted here rather than taken as
// events.IssueThreadLogins sorted it.
//
// movedAssignees is nil on a write that left the assignee alone, so a status-only move still
// answers exactly what it answered before.
func issueWriteThreadLogins(
	ctx context.Context, tx pgx.Tx, issueKey string, statusMoved, closedMoved bool, movedAssignees []*string,
) ([]string, error) {
	logins := []string{}
	if statusMoved {
		moved, err := issueThreadLogins(ctx, tx, issueKey, closedMoved)
		if err != nil {
			return nil, err
		}
		logins = moved
	}
	for _, assignee := range movedAssignees {
		if assignee == nil {
			continue
		}
		login := model.CanonicalLogin(*assignee)
		if login == "" || slices.Contains(logins, login) {
			continue
		}
		logins = append(logins, login)
	}
	slices.Sort(logins)
	return logins, nil
}

// commentThreadLogins is a written comment's thread_logins: the logins with a Replies row on the
// thread the user_threads trigger files it under (0095's user_threads_comment): its ask, else the
// root it replies to, else the comment itself. tx must hold the comment's insert.
func commentThreadLogins(ctx context.Context, tx pgx.Tx, comment model.Comment) ([]string, error) {
	thread := comment.ID
	switch {
	case comment.AskID != nil:
		thread = *comment.AskID
	case comment.ReplyTo != nil:
		thread = *comment.ReplyTo
	}
	return events.ThreadLogins(ctx, tx, thread)
}

// messageThreadLogins is a written message's thread_logins: every login whose Inbox the message
// can have moved. An issue message moves a Replies row, so it names the logins with a row on its
// thread root, found up in_reply_to as the user_threads trigger finds it (0095's
// user_threads_message). An issue-less message is a direct conversation, which keeps no
// user_threads row: it moves the Agent row of the person who opened the thread (api/replies.go,
// myAgentRootFilter), so it names that root author's canonical login, and [] when a session or the
// system opened the thread and no Agent row holds it. tx must hold the message's insert.
func messageThreadLogins(ctx context.Context, tx pgx.Tx, message model.Message) ([]string, error) {
	if message.IssueKey == nil {
		return directThreadLogins(ctx, tx, message)
	}
	root := message.ID
	if message.InReplyTo != nil {
		if err := tx.QueryRow(ctx, messageThreadCTE+`
			select id::text from thread where in_reply_to is null
		`, message.ID).Scan(&root); err != nil {
			return nil, fmt.Errorf("find the thread root of message %s: %w", message.ID, err)
		}
	}
	return events.ThreadLogins(ctx, tx, root)
}

// directThreadLogins is a direct (issue-less) message's thread_logins: the canonical login of its
// thread root's author, which is the one Inbox an Agent row of this thread can sit in, and [] when
// that author is not a person. The root is found with messageThreadCTE, the walk the issue
// messages take; a message that answers nothing is its own root, so its author needs no query.
func directThreadLogins(ctx context.Context, tx pgx.Tx, message model.Message) ([]string, error) {
	kind, id := message.Author.Kind, message.Author.ID
	if message.InReplyTo != nil {
		if err := tx.QueryRow(ctx, messageThreadCTE+`
			select coalesce(m.author ->> 'kind', ''), coalesce(m.author ->> 'id', '')
			from messages m join thread t on t.id = m.id
			where t.in_reply_to is null
		`, message.ID).Scan(&kind, &id); err != nil {
			return nil, fmt.Errorf("find the thread root's author of message %s: %w", message.ID, err)
		}
	}
	login := model.CanonicalLogin(id)
	if kind != "user" || login == "" {
		return []string{}, nil
	}
	return []string{login}, nil
}
