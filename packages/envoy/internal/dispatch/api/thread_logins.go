package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// commentThread is the thread the user_threads trigger files comment under (0095's
// user_threads_comment): its ask, else the root it replies to, else the comment itself.
func commentThread(comment model.Comment) string {
	switch {
	case comment.AskID != nil:
		return *comment.AskID
	case comment.ReplyTo != nil:
		return *comment.ReplyTo
	}
	return comment.ID
}

// commentThreadLogins is a written comment's thread_logins: the logins with a Replies row on its
// thread (commentThread). tx must hold the comment's insert.
func commentThreadLogins(ctx context.Context, tx pgx.Tx, comment model.Comment) ([]string, error) {
	return events.ThreadLogins(ctx, tx, commentThread(comment))
}

// messageThreadRoot is the thread root of message messageID, which replies to inReplyTo (nil: a
// root, the message itself), found up in_reply_to as the user_threads trigger finds it (0095's
// user_threads_message). The walk starts at inReplyTo, whose chain is the message's own above it,
// so it finds the root before the message is written.
func messageThreadRoot(ctx context.Context, tx pgx.Tx, messageID string, inReplyTo *string) (string, error) {
	if inReplyTo == nil {
		return messageID, nil
	}
	var root string
	if err := tx.QueryRow(ctx, messageThreadCTE+`
		select id::text from thread where in_reply_to is null
	`, *inReplyTo).Scan(&root); err != nil {
		return "", fmt.Errorf("find the thread root of message %s: %w", messageID, err)
	}
	return root, nil
}

// messageThreadLogins is a written message's thread_logins: the logins with a Replies row on its
// thread root (messageThreadRoot). An issue-less message is a direct conversation, which has no
// Replies thread, so it has none (nil, which the payload leaves out). tx must hold the message's
// insert.
func messageThreadLogins(ctx context.Context, tx pgx.Tx, message model.Message) ([]string, error) {
	if message.IssueKey == nil {
		return nil, nil
	}
	root, err := messageThreadRoot(ctx, tx, message.ID, message.InReplyTo)
	if err != nil {
		return nil, err
	}
	return events.ThreadLogins(ctx, tx, root)
}
