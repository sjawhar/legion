package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// listComments answers an issue's comment list, scoped by ?artifact= to the comments anchored in
// one of its documents.
func (s *server) listComments(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	s.answerCommentList(
		w, r,
		issueOwner(r.PathValue("key")),
		strings.TrimSpace(r.URL.Query().Get("artifact")),
		r.PathValue("key"),
	)
}

// answerCommentList answers one comment list - an issue's (listComments) or an unlinked
// document's (listArtifactComments) - in the order the activity feed answers a page of events
// (listIssueEvents): the validator is read first, in one statement over metadata, and the
// comments are loaded only for a reader that neither holds that validator nor can be written the
// bytes this server kept under it (list_answers.go). A warm read therefore builds nothing - no
// comment row, no mention, no delivery attempt, no display tree, no encode, no compression -
// which is what a validator over the answer's own bytes could never do, since the answer had to
// exist before its key was known.
//
// The validator stands on the list's watermark: a counter per comment-list owner that three
// deferred constraint triggers raise for every write of comments, comment_mentions and
// comment_deliveries (migration 20261106020619). It cannot stand on the issue's last_seq the way
// the feed's does, because a delivery claim and the resolution recorded before a send write a
// delivery row and append no event at all (comment_delivery.go), and a reader whose list now
// shows that attempt would be answered 304. Deferred to commit, the counter rises inside the
// writing transaction, so a reader sees the new rows and the new counter together or neither.
//
// last_seq is hashed in beside it for the one part of the answer those three tables do not
// decide: a reply that answers a block carries that block's reference, built from the owner's
// project key and the document's slug (refs.ReplyBlocks), and a rename of either moves the
// owner's event sequence without touching a comment row. The owner and the ?artifact= scope are
// in the hash too, so no two lists of one deploy can be given one validator.
//
// name is the owner as the request spells it: the key the kept answer is filed under beside its
// ETag, and the subject the display tree read is logged against.
func (s *server) answerCommentList(w http.ResponseWriter, r *http.Request, owner owner, artifactFilter, name string) {
	watermark, lastSeq, err := s.commentListWatermark(r.Context(), owner)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	etag := commentListETag(owner, artifactFilter, watermark, lastSeq, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	key := listAnswerKeyFor(r, commentListRoute, name, etag)
	if s.listAnswers.answer(w, key) {
		return
	}
	comments, err := s.loadOwnerComments(r.Context(), s.deps.Store.Pool, owner, artifactFilter)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var texts []string
	if trees {
		texts = commentDisplayTexts(nil, comments...)
	}
	defer s.readDisplayTrees(r.Context(), trees, name, texts)()
	encoded, err := encodeListAnswer(commentReads(comments, trees))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeListAnswer(w, key, encoded)
}

// commentListWatermark is the pair a comment list's validator is built on, read in one statement:
// the list's own watermark, and the newest event sequence of the owner holding it. Each reads 0
// where no row answers - an owner nothing has written carries no counter, and a key no issue
// holds answers the empty list rather than a refusal, as this route has always done - so a list
// that has never been written and one whose owner does not exist are one case here, as they are
// in the answer.
func (s *server) commentListWatermark(ctx context.Context, owner owner) (int64, int64, error) {
	var query, value string
	switch {
	case owner.IssueKey != nil:
		query, value = `
			select coalesce((select seq from comment_watermarks where issue_key = $1), 0),
			       coalesce((select last_seq from issues where key = $1), 0)
		`, *owner.IssueKey
	case owner.ArtifactID != nil:
		query, value = `
			select coalesce((select seq from comment_watermarks where artifact_id = $1), 0),
			       coalesce((select max(seq) from events where artifact_id = $1), 0)
		`, *owner.ArtifactID
	default:
		return 0, 0, errorf(http.StatusBadRequest, "OWNER_INVALID", "owner requires exactly one issue or artifact")
	}
	var watermark, lastSeq int64
	if err := s.deps.Store.Pool.QueryRow(ctx, query, value).Scan(&watermark, &lastSeq); err != nil {
		return 0, 0, fmt.Errorf("read the comment list's watermark: %w", err)
	}
	return watermark, lastSeq, nil
}

// commentListETag is a comment list answer's validator: the list it is of, the watermark every
// write the list shows raises, and the owner's event sequence, closed with the lines both lists'
// ETags carry (listETag). It is referencesETag's shape - one line per part, then the hash of them
// - and, unlike the feed's, it names its owner, so two lists standing at one count never share a
// validator.
func commentListETag(owner owner, artifactFilter string, watermark, lastSeq int64, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "issue=%s\nartifact=%s\nfilter=%s\nwatermark=%d\nlast_seq=%d\n",
		deref(owner.IssueKey), deref(owner.ArtifactID), artifactFilter, watermark, lastSeq)
	return listETag(hash, trees)
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
	comments[0].AnchorPosition, err = s.anchorBlock(r.Context(), comments[0].Anchor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	replies, err := s.loadReplyChain(r.Context(), s.deps.Store.Pool, "reply_to", comment.ID)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	trees := wantsDisplayTrees(r)
	var texts []string
	if trees {
		texts = commentDisplayTexts(commentDisplayTexts(nil, comments[0]), replies...)
	}
	defer s.readDisplayTrees(r.Context(), trees, comment.ID, texts)()
	WriteJSON(w, http.StatusOK, struct {
		Comment commentRead   `json:"comment"`
		Replies []commentRead `json:"replies"`
	}{Comment: newCommentRead(comments[0], trees), Replies: commentReads(replies, trees)})
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
const commentColumns = `id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, turn, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at, replies_to_block, answers::text`

// commentThreadColumns is commentColumns without the ::text casts, qualified to the comments
// alias `c`. A recursive walk joins c.reply_to to the visited row's id, so those columns have
// to stay uuid inside the CTE; and a recursive union cannot take c.* because comments.search
// is a tsvector, which has no hash operator.
const commentThreadColumns = `c.id, c.issue_key, c.artifact_id, c.author, c.body, c.anchor, c.reply_to,
		c.ask_id, c.turn, c.resolved, c.resolved_by, c.resolved_at, c.edited_at, c.suggestion, c.created_at, c.replies_to_block,
		c.answers`

func scanComment(row pgx.Row, extra ...any) (model.Comment, error) {
	var comment model.Comment
	var author, anchor, resolvedBy, suggestion []byte
	var resolvedAt, editedAt *time.Time
	destinations := []any{
		&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.AskID, &comment.Turn, &comment.Resolved,
		&resolvedBy, &resolvedAt, &editedAt, &suggestion, &comment.CreatedAt, &comment.RepliesToBlock, &comment.Answers,
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
		if err := model.DecodeAnchor(anchor, &value); err != nil {
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

// hydrateCommentSideTables loads mentions, delivery attempts and the block each reply answers once
// per comment page, avoiding a per-comment query while keeping all reads of a Comment's visible
// delivery state consistent.
func (s *server) hydrateCommentSideTables(ctx context.Context, q queryer, comments []model.Comment) ([]model.Comment, error) {
	ids := make([]string, len(comments))
	replies := make([]*model.Comment, len(comments))
	for index := range comments {
		ids[index] = comments[index].ID
		replies[index] = &comments[index]
	}
	mentions, err := s.loadCommentMentions(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	deliveries, err := s.loadCommentDeliveries(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	if err := hydrateCommentReplyBlocks(ctx, q, replies...); err != nil {
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
		select `+commentDeliveryColumns+`
		from comment_deliveries
		where comment_id = any($1::uuid[])
		order by comment_id, target, attempt
	`, commentIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		delivery, err := scanCommentDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries[delivery.CommentID] = append(deliveries[delivery.CommentID], delivery)
	}
	return deliveries, rows.Err()
}
