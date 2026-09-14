package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) commentProjectionKind(ctx context.Context, comment model.Comment) (string, error) {
	if comment.Suggestion == nil || comment.Anchor == nil {
		return "", nil
	}
	return s.deps.Docs.SuggestionKind(ctx, comment.Anchor.ArtifactID, comment.Anchor.MarkID)
}

func commentMarkRecord(comment model.Comment, replies []model.Comment, suggestionKind string) docs.MarkRecord {
	record := docs.MarkRecord{
		Kind:      "comment",
		By:        docs.ActorRef(comment.Author),
		CreatedAt: timestampValue(comment.CreatedAt),
		Text:      comment.Body,
		Resolved:  comment.Resolved,
		Replies:   make([]docs.MarkReply, 0, len(replies)),
	}
	for _, reply := range replies {
		record.Replies = append(record.Replies, docs.MarkReply{
			By:   docs.ActorRef(reply.Author),
			Text: reply.Body,
			At:   timestampValue(reply.CreatedAt),
		})
	}
	if comment.Suggestion != nil {
		record.Kind = suggestionKind
		record.Content = comment.Suggestion.ReplaceWith
		record.Status = "pending"
		if comment.Suggestion.Accepted != nil {
			if *comment.Suggestion.Accepted {
				record.Status = "accepted"
			} else {
				record.Status = "rejected"
			}
		}
	}
	return record
}

// commentEventThread is what a comment.created payload says about the thread the
// comment joined: the ask it replies to (question, state, and whose turn it is once
// this comment is the newest reply) or the root of the comment thread. Zero for
// root comments and for every other comment.* event.
type commentEventThread struct {
	AskQuestion  string
	AskState     string
	AskWaitingOn string
	ThreadRootID string
}

func (s *server) commentEventPayload(ctx context.Context, tx pgx.Tx, comment model.Comment, artifactName string, thread commentEventThread) (model.CommentEventPayload, error) {
	payload := model.CommentEventPayload{
		Comment:      comment,
		ArtifactName: artifactName,
		AskQuestion:  thread.AskQuestion,
		AskState:     thread.AskState,
		AskWaitingOn: thread.AskWaitingOn,
		ThreadRootID: thread.ThreadRootID,
	}
	if comment.ArtifactID == nil {
		return payload, nil
	}
	if err := tx.QueryRow(ctx, `select project_key, slug from artifacts where id = $1`, *comment.ArtifactID).Scan(&payload.ProjectKey, &payload.ArtifactSlug); err != nil {
		return model.CommentEventPayload{}, fmt.Errorf("load artifact comment event owner: %w", err)
	}
	return payload, nil
}

func (s *server) commentArtifactName(ctx context.Context, tx pgx.Tx, comment model.Comment) (string, error) {
	if comment.Anchor == nil {
		return "", nil
	}
	var name string
	if err := tx.QueryRow(ctx, `select name from artifacts where id = $1`, comment.Anchor.ArtifactID).Scan(&name); err != nil {
		return "", err
	}
	return name, nil
}
