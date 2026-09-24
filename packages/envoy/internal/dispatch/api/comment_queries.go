package api

import (
	"context"
	"encoding/json"
	"errors"
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

// replyChainQuery is every comment transitively replying to the row(s) matching
// "<seedColumn> = $1", oldest first; %s is that seed column. comments.reply_to has no
// acyclicity constraint, so the walk unions on the visited row: a cycle repeats a row already
// in the set and adds nothing, rather than spinning the request. query_plan_test.go plans
// this exact text, so the plan it checks is the one the server runs.
const replyChainQuery = `
		with recursive replies as (
			select ` + commentThreadColumns + `
			from comments c where c.%s = $1
			union
			select ` + commentThreadColumns + `
			from comments c join replies r on c.reply_to = r.id
		)
		select ` + commentColumns + `
		from replies
		order by created_at, id`

// inboxAskReplyChainsQuery hydrates every Inbox card's thread in one walk: the same recursion
// as replyChainQuery, seeded from a set of asks and carrying the ask each row belongs to, so a
// comment under two asks yields one row per ask. Planned by query_plan_test.go too.
const inboxAskReplyChainsQuery = `
		with recursive replies as (
			select c.ask_id as thread_id, ` + commentThreadColumns + `
			from comments c
			where c.ask_id = any($1::uuid[])
			union
			select r.thread_id, ` + commentThreadColumns + `
			from comments c join replies r on c.reply_to = r.id
		)
		select ` + commentColumns + `, thread_id::text
		from replies
		order by thread_id, created_at, id`

// loadReplyChain returns every comment transitively replying to the row(s)
// matching "<seedColumn> = seedValue", ordered oldest first. seedColumn
// selects the thread root: "reply_to" for a comment's own replies, "ask_id"
// for an ask's directly-replying comments (plus, either way, every comment
// that replies to one of those via reply_to). seedColumn is caller-controlled
// and never derived from request input.
func (s *server) loadReplyChain(ctx context.Context, q queryer, seedColumn, seedValue string) ([]model.Comment, error) {
	rows, err := q.Query(ctx, fmt.Sprintf(replyChainQuery, seedColumn), seedValue)
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

// loadInboxAskThreads batches the initial data each Inbox card renders. Both write paths
// normalise a reply under an ask to that ask's ask_id, so the recursive reply_to walk only
// picks up rows a pre-0043 server wrote or is still draining; it matches loadReplyChain
// exactly either way.
func (s *server) loadInboxAskThreads(ctx context.Context, q queryer, askIDs []string) (map[string]inboxAskThread, error) {
	threads := make(map[string]inboxAskThread, len(askIDs))
	for _, askID := range askIDs {
		threads[askID] = inboxAskThread{
			Replies:   []model.Comment{},
			Edits:     []model.AskEdit{},
			Followers: []model.AskFollower{},
		}
	}
	if len(askIDs) == 0 {
		return threads, nil
	}

	replies, err := s.loadInboxAskReplyChains(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	edits, err := s.loadInboxAskEdits(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	followers, err := s.loadInboxAskFollowers(ctx, q, askIDs)
	if err != nil {
		return nil, err
	}
	for askID, thread := range threads {
		thread.Replies = replies[askID]
		thread.Edits = edits[askID]
		thread.Followers = followers[askID]
		threads[askID] = thread
	}
	return threads, nil
}

// loadInboxAskReplyChains groups inboxAskReplyChainsQuery's rows by the ask each belongs to,
// preserving the query's order within a thread.
func (s *server) loadInboxAskReplyChains(ctx context.Context, q queryer, askIDs []string) (map[string][]model.Comment, error) {
	replies := make(map[string][]model.Comment, len(askIDs))
	for _, askID := range askIDs {
		replies[askID] = []model.Comment{}
	}
	rows, err := q.Query(ctx, inboxAskReplyChainsQuery, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask replies: %w", err)
	}
	defer rows.Close()
	type threadedComment struct {
		askID   string
		comment model.Comment
	}
	comments := []threadedComment{}
	for rows.Next() {
		var askID string
		comment, err := scanComment(rows, &askID)
		if err != nil {
			return nil, fmt.Errorf("scan inbox ask reply: %w", err)
		}
		comments = append(comments, threadedComment{askID: askID, comment: comment})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask replies: %w", err)
	}
	unhydrated := make([]model.Comment, len(comments))
	for index := range comments {
		unhydrated[index] = comments[index].comment
	}
	hydrated, err := s.hydrateCommentSideTables(ctx, q, unhydrated)
	if err != nil {
		return nil, fmt.Errorf("hydrate inbox ask replies: %w", err)
	}
	for index := range hydrated {
		replies[comments[index].askID] = append(replies[comments[index].askID], hydrated[index])
	}
	return replies, nil
}

func (s *server) loadInboxAskEdits(ctx context.Context, q queryer, askIDs []string) (map[string][]model.AskEdit, error) {
	edits := make(map[string][]model.AskEdit, len(askIDs))
	for _, askID := range askIDs {
		edits[askID] = []model.AskEdit{}
	}
	rows, err := q.Query(ctx, `
		select payload->>'id', payload->'previous', payload->'edited_by', created_at
		from events
		where type = 'ask.edited' and payload->>'id' = any($1::text[])
		order by payload->>'id', id
	`, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask edits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var askID string
		var previous, editedBy []byte
		var at time.Time
		if err := rows.Scan(&askID, &previous, &editedBy, &at); err != nil {
			return nil, fmt.Errorf("scan inbox ask edit: %w", err)
		}
		var edit model.AskEdit
		if err := json.Unmarshal(previous, &edit.Previous); err != nil {
			return nil, fmt.Errorf("decode inbox ask edit previous: %w", err)
		}
		if err := json.Unmarshal(editedBy, &edit.EditedBy); err != nil {
			return nil, fmt.Errorf("decode inbox ask edit editor: %w", err)
		}
		edit.At = timestampValue(at)
		edits[askID] = append(edits[askID], edit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask edits: %w", err)
	}
	return edits, nil
}

func (s *server) loadInboxAskFollowers(ctx context.Context, q queryer, askIDs []string) (map[string][]model.AskFollower, error) {
	followers := make(map[string][]model.AskFollower, len(askIDs))
	for _, askID := range askIDs {
		followers[askID] = []model.AskFollower{}
	}
	rows, err := q.Query(ctx, `
		select ask_id::text, session_id, since
		from ask_followers
		where ask_id = any($1::uuid[])
		order by ask_id, since, session_id
	`, askIDs)
	if err != nil {
		return nil, fmt.Errorf("load inbox ask followers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var askID string
		var follower model.AskFollower
		if err := rows.Scan(&askID, &follower.SessionID, &follower.Since); err != nil {
			return nil, fmt.Errorf("scan inbox ask follower: %w", err)
		}
		followers[askID] = append(followers[askID], follower)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load inbox ask followers: %w", err)
	}
	return followers, nil
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

// loadOwnedCommentForUpdate locks a comment and refuses one that is not on this owner, with
// the 400 every reply_to walk answers.
func (s *server) loadOwnedCommentForUpdate(
	ctx context.Context, tx pgx.Tx, owner owner, id string,
) (model.Comment, error) {
	comment, err := s.loadCommentForUpdate(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !commentHasOwner(comment, owner)) {
		return model.Comment{}, errorf(http.StatusBadRequest, "INVALID_COMMENT", "reply_to must identify a comment on this owner")
	}
	if err != nil {
		return model.Comment{}, err
	}
	return comment, nil
}

// commentColumns is the comments select list scanComment reads, in scan order.
const commentColumns = `id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, turn, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at`

// commentThreadColumns is commentColumns without the ::text casts, qualified to the comments
// alias `c`. A recursive walk joins c.reply_to to the visited row's id, so those columns have
// to stay uuid inside the CTE; and a recursive union cannot take c.* because comments.search
// is a tsvector, which has no hash operator.
const commentThreadColumns = `c.id, c.issue_key, c.artifact_id, c.author, c.body, c.anchor, c.reply_to,
		c.ask_id, c.turn, c.resolved, c.resolved_by, c.resolved_at, c.edited_at, c.suggestion, c.created_at`

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
