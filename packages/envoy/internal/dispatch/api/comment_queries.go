package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) listComments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	comments, err := s.loadOwnerComments(
		r.Context(),
		s.deps.Store.Pool,
		issueOwner(r.PathValue("key")),
		strings.TrimSpace(r.URL.Query().Get("artifact")),
	)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, comments)
}

func (s *server) loadOwnerComments(ctx context.Context, q queryer, owner owner, artifactFilter string) ([]model.Comment, error) {
	var ownerColumn, ownerValue string
	switch {
	case owner.IssueKey != nil:
		ownerColumn, ownerValue = "issue_key", *owner.IssueKey
	case owner.ArtifactID != nil:
		ownerColumn, ownerValue = "artifact_id", *owner.ArtifactID
	default:
		return nil, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	rows, err := q.Query(ctx, fmt.Sprintf(`
		select `+commentColumns+`
		from comments
		where %s = $1 and ($2 = '' or anchor->>'artifact_id' = $2)
		order by created_at, id
	`, ownerColumn), ownerValue, artifactFilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	comments := []model.Comment{}
	for rows.Next() {
		comment, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.hydrateCommentSideTables(ctx, q, comments)
}

func commentHasOwner(comment model.Comment, owner owner) bool {
	if owner.IssueKey != nil {
		return comment.IssueKey != nil && *comment.IssueKey == *owner.IssueKey
	}
	return owner.ArtifactID != nil && comment.ArtifactID != nil && *comment.ArtifactID == *owner.ArtifactID
}

func (s *server) getComment(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "comment") {
		return
	}
	comment, err := s.loadComment(r.Context(), s.deps.Store.Pool, r.PathValue("id"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	comments, err := s.hydrateCommentSideTables(r.Context(), s.deps.Store.Pool, []model.Comment{comment})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadReplyChain(r.Context(), s.deps.Store.Pool, "reply_to", comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, struct {
		Comment model.Comment   `json:"comment"`
		Replies []model.Comment `json:"replies"`
	}{Comment: comments[0], Replies: replies})
}

// loadReplyChain returns every comment transitively replying to the row(s)
// matching "<seedColumn> = seedValue", ordered oldest first. seedColumn
// selects the thread root: "reply_to" for a comment's own replies, "ask_id"
// for an ask's directly-replying comments (plus, either way, every comment
// that replies to one of those via reply_to). seedColumn is caller-controlled
// and never derived from request input.
func (s *server) loadReplyChain(ctx context.Context, q queryer, seedColumn, seedValue string) ([]model.Comment, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(`
		with recursive replies as (
			select id, issue_key, artifact_id, author, body, anchor, reply_to, ask_id, turn, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at
			from comments where %s = $1
			union all
			select c.id, c.issue_key, c.artifact_id, c.author, c.body, c.anchor, c.reply_to, c.ask_id, c.turn, c.resolved, c.resolved_by, c.resolved_at, c.edited_at, c.suggestion, c.created_at
			from comments c join replies r on c.reply_to = r.id
		)
		select `+commentColumns+`
		from replies
		order by created_at, id
	`, seedColumn), seedValue)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	replies := []model.Comment{}
	for rows.Next() {
		reply, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		replies = append(replies, reply)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.hydrateCommentSideTables(ctx, q, replies)
}
func (s *server) loadComment(ctx context.Context, q queryer, id string) (model.Comment, error) {
	return scanComment(q.QueryRow(ctx, `
		select `+commentColumns+`
		from comments where id = $1
	`, id))
}
func (s *server) loadCommentForUpdate(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	return scanComment(tx.QueryRow(ctx, `
		select `+commentColumns+`
		from comments where id = $1 for update
	`, id))
}

// commentColumns is the comments select list scanComment reads, in scan order.
const commentColumns = `id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, turn, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at`

func scanComment(row pgx.Row, extra ...any) (model.Comment, error) {
	var comment model.Comment
	var author, anchor, resolvedBy, suggestion []byte
	var resolvedAt, editedAt *time.Time
	destinations := []any{
		&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.AskID, &comment.Turn, &comment.Resolved,
		&resolvedBy, &resolvedAt, &editedAt, &suggestion, &comment.CreatedAt,
	}
	destinations = append(destinations, extra...)
	if err := row.Scan(destinations...); err != nil {
		return model.Comment{}, err
	}
	if err := json.Unmarshal(author, &comment.Author); err != nil {
		return model.Comment{}, fmt.Errorf("decode comment author: %w", err)
	}
	if len(anchor) > 0 {
		var value model.Anchor
		if err := json.Unmarshal(anchor, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment anchor: %w", err)
		}
		comment.Anchor = &value
	}
	if len(resolvedBy) > 0 {
		var value model.Actor
		if err := json.Unmarshal(resolvedBy, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment resolved_by: %w", err)
		}
		comment.ResolvedBy = &value
	}
	comment.ResolvedAt = timestampPtr(resolvedAt)
	comment.EditedAt = timestampPtr(editedAt)
	if len(suggestion) > 0 {
		var value model.Suggestion
		if err := json.Unmarshal(suggestion, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode comment suggestion: %w", err)
		}
		comment.Suggestion = &value
	}
	comment.Mentions = []model.Mention{}
	comment.Deliveries = []model.CommentDelivery{}
	return comment, nil
}

// hydrateCommentSideTables loads mentions and delivery attempts once per comment page, avoiding
// a per-comment query while keeping all reads of a Comment's visible delivery state consistent.
func (s *server) hydrateCommentSideTables(ctx context.Context, q queryer, comments []model.Comment) ([]model.Comment, error) {
	ids := make([]string, len(comments))
	for index := range comments {
		ids[index] = comments[index].ID
	}
	mentions, err := s.loadCommentMentions(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	deliveries, err := s.loadCommentDeliveries(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for index := range comments {
		comments[index].Mentions = mentions[comments[index].ID]
		comments[index].Deliveries = deliveries[comments[index].ID]
	}
	return comments, nil
}

func (s *server) loadCommentMentions(ctx context.Context, q queryer, commentIDs []string) (map[string][]model.Mention, error) {
	mentions := make(map[string][]model.Mention, len(commentIDs))
	for _, id := range commentIDs {
		mentions[id] = []model.Mention{}
	}
	if len(commentIDs) == 0 {
		return mentions, nil
	}
	rows, err := q.Query(ctx, `
		select comment_id::text, target, delivery, resolved_session_id
		from comment_mentions
		where comment_id = any($1::uuid[])
		order by comment_id, target
	`, commentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var commentID string
		var mention model.Mention
		if err := rows.Scan(&commentID, &mention.Target, &mention.Delivery, &mention.SessionID); err != nil {
			return nil, err
		}
		mentions[commentID] = append(mentions[commentID], mention)
	}
	return mentions, rows.Err()
}

func (s *server) loadCommentDeliveries(ctx context.Context, q queryer, commentIDs []string) (map[string][]model.CommentDelivery, error) {
	deliveries := make(map[string][]model.CommentDelivery, len(commentIDs))
	for _, id := range commentIDs {
		deliveries[id] = []model.CommentDelivery{}
	}
	if len(commentIDs) == 0 {
		return deliveries, nil
	}
	rows, err := q.Query(ctx, `
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
		from comment_deliveries
		where comment_id = any($1::uuid[])
		order by comment_id, target, attempt
	`, commentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var delivery model.CommentDelivery
		if err := rows.Scan(
			&delivery.CommentID, &delivery.Target, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID,
			&delivery.EnvelopeID, &delivery.State, &delivery.Error, &delivery.ResolveError, &delivery.ReplyID, &delivery.CreatedAt,
		); err != nil {
			return nil, err
		}
		deliveries[delivery.CommentID] = append(deliveries[delivery.CommentID], delivery)
	}
	return deliveries, rows.Err()
}
