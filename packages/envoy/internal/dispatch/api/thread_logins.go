package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// askThreadLogins is an ask event's thread_logins: the logins with a Replies row on the ask's own
// thread (0096's user_threads_ask), read once the ask's insert or answer is in tx.
func askThreadLogins(ctx context.Context, tx pgx.Tx, askID string) ([]string, error) {
	return events.ThreadLogins(ctx, tx, askID)
}

// issueThreadLogins is the thread_logins of an issue event whose status moved: whose Replies count
// the move can change. Only a close or a reopen (closedMoved) changes one, for the logins with a
// Replies row on the issue's open asks (events.IssueThreadLogins); any other status move names
// nobody, as [] rather than left out, so a reader knows the event says so.
func issueThreadLogins(ctx context.Context, tx pgx.Tx, issueKey string, closedMoved bool) ([]string, error) {
	if !closedMoved {
		return []string{}, nil
	}
	return events.IssueThreadLogins(ctx, tx, issueKey)
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

// messageThreadLogins is a written message's thread_logins: the logins with a Replies row on its
// thread root, found up in_reply_to as the user_threads trigger finds it (0095's
// user_threads_message). An issue-less message is a direct conversation, which has no Replies
// thread, so it has none (nil, which the payload leaves out). tx must hold the message's insert.
func messageThreadLogins(ctx context.Context, tx pgx.Tx, message model.Message) ([]string, error) {
	if message.IssueKey == nil {
		return nil, nil
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
