package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// commentProjectionKind reads the suggestion's kind from the live document. ctx is the handler's
// document context (Docs.Join), so the read joins the handler's transaction: it runs while that
// transaction is open, where a second pooled connection is what deadlocks the pool
// (store.ErrNestedAcquire), and it reads the document as the transaction's own writes left it.
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

// commentEventThread is what a comment payload says about the thread the comment joined:
// the ask it replies to (question, state, and whose turn it is once this comment is the
// newest reply) or the root of the comment thread. Populated on comment.created and on the
// comment.answered a delivery callback appends; zero for root comments and for every other
// comment.* event.
type commentEventThread struct {
	AskQuestion  string
	AskState     string
	AskWaitingOn string
	ThreadRootID string
}

// commentWriteResponse is what a route that posts a comment answers: the comment row, and for a
// reply to an open ask whom that ask waits on now that the reply is its newest
// (commentEventThread.AskWaitingOn, under the same name the comment's event carries it).
type commentWriteResponse struct {
	model.Comment
	AskWaitingOn string `json:"ask_waiting_on,omitempty"`
}

// commentEventPayload builds the payload of every comment.* event. It takes what the write moved
// in the reference graph — an empty ReferenceChanges on a transition that writes no body — so a
// producer whose comment text can cite something cannot append an event that stays silent. It
// reads the block the comment answers (reply_block) as the transaction sees it into comment
// itself, so a route that answers with the comment answers that block too.
func (s *server) commentEventPayload(ctx context.Context, tx pgx.Tx, comment *model.Comment, artifactName string, thread commentEventThread, changes model.ReferenceChanges) (model.CommentEventPayload, error) {
	if err := hydrateCommentReplyBlocks(ctx, tx, comment); err != nil {
		return model.CommentEventPayload{}, fmt.Errorf("load the block comment %s answers: %w", comment.ID, err)
	}
	payload := model.CommentEventPayload{
		Comment:                 *comment,
		ArtifactName:            artifactName,
		AskQuestion:             thread.AskQuestion,
		AskState:                thread.AskState,
		AskWaitingOn:            thread.AskWaitingOn,
		ThreadRootID:            thread.ThreadRootID,
		ReferenceChangesPayload: model.NewReferenceChangesPayload(changes),
	}
	if comment.ArtifactID == nil {
		return payload, nil
	}
	if err := tx.QueryRow(ctx, `select coalesce(project_key, ''), slug from artifacts where id = $1`, *comment.ArtifactID).Scan(&payload.ProjectKey, &payload.ArtifactSlug); err != nil {
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

// registerCommentAnchor records comment's anchored row on ledger as the route's queued write of it
// leaves it (docs.Ledger.RegisterPendingAnchor): the anchor refreshes the transaction runs after
// that write - ProjectMark's - read the open anchors from the table, which holds the write only
// once Commit has run it, so they take this row from the ledger, as they would have read it after
// the write. An unresolved row is refreshed, and a refresh that moves its anchor prepares its
// event (commentAnchorRefreshEvent); a resolved one no longer is. A comment without an anchor has
// nothing to register. stored says the table already holds the row (an update rather than a new
// comment's insert).
func (s *server) registerCommentAnchor(ledger *docs.Ledger, comment model.Comment, actor model.Actor, stored bool) {
	if comment.Anchor == nil {
		return
	}
	kind := docs.MarkComment
	if comment.Suggestion != nil {
		kind = docs.MarkSuggestion
	}
	row := docs.PendingAnchor{ID: comment.ID, Kind: kind, Anchor: *comment.Anchor, Open: !comment.Resolved}
	if row.Open {
		row.Event = func(ctx context.Context, tx pgx.Tx) (func(model.Anchor) (model.Event, error), error) {
			return s.commentAnchorRefreshEvent(ctx, tx, comment, actor, stored)
		}
	}
	ledger.RegisterPendingAnchor(row)
}

// commentAnchorRefreshEvent builds comment's comment.anchor_refreshed event for a refresh that
// moves its anchor (docs.PendingAnchor.Event): the row as the route's write leaves it, with the
// quote and orphaned flag the refresh writes, as the refresh reads a stored row back. What it
// reads - the anchor document's name and key, and a stored row's mention and delivery rows - it
// reads here, in the read phase, once the refresh has found the anchor moved; a new comment's
// mention rows are inserted after its projection, so the refresh read none. The builder only
// assembles the event.
func (s *server) commentAnchorRefreshEvent(ctx context.Context, tx pgx.Tx, comment model.Comment, actor model.Actor, stored bool) (func(model.Anchor) (model.Event, error), error) {
	row := comment
	row.Mentions, row.Deliveries = []model.Mention{}, []model.CommentDelivery{}
	if stored {
		hydrated, err := s.hydrateCommentSideTables(ctx, tx, []model.Comment{row})
		if err != nil {
			return nil, fmt.Errorf("load refreshed comment %q: %w", comment.ID, err)
		}
		row = hydrated[0]
	}
	anchor := *comment.Anchor
	var payload model.CommentEventPayload
	if err := tx.QueryRow(ctx, `
		select name, coalesce(project_key, ''), slug
		from artifacts where id = $1
	`, anchor.ArtifactID).Scan(&payload.ArtifactName, &payload.ProjectKey, &payload.ArtifactSlug); err != nil {
		return nil, fmt.Errorf("load refreshed comment %q artifact: %w", comment.ID, err)
	}
	return func(refreshed model.Anchor) (model.Event, error) {
		moved := anchor
		moved.Quote, moved.Orphaned = refreshed.Quote, refreshed.Orphaned
		payload := payload
		payload.Comment = row
		payload.Comment.Anchor = &moved
		return model.Event{
			IssueKey: row.IssueKey, ArtifactID: row.ArtifactID,
			Type: "comment.anchor_refreshed", Actor: actor, Payload: payload,
		}, nil
	}, nil
}
