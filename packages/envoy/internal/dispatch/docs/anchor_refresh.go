package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

type anchoredMark struct {
	id       string
	markType string
	anchor   model.Anchor
}

// markRef is the mark identity the row anchors to.
func (m anchoredMark) markRef() pmdoc.MarkRef {
	return pmdoc.MarkRef{Type: m.markType, ID: m.anchor.MarkID}
}

// openAnchoredMarks lists the open anchored asks, then the unresolved anchored comments, each by
// creation time and id, so a refresh announces its rows, and an upload re-anchors them, in the same
// order every time it runs over the same rows.
func (s *Service) openAnchoredMarks(ctx context.Context, source Queryer, artifactID string) ([]anchoredMark, error) {
	var marks []anchoredMark
	for _, target := range []struct {
		table string
		where string
	}{
		{table: "asks", where: "state = 'open'"},
		{table: "comments", where: "not resolved"},
	} {
		rows, err := source.Query(ctx, fmt.Sprintf(`
			select id::text, anchor, %s
			from %s
			where anchor is not null and anchor->>'artifact_id' = $1 and %s
			order by created_at, id
		`, commentMarkType(target.table), target.table, target.where), artifactID)
		if err != nil {
			return nil, fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		for rows.Next() {
			var mark anchoredMark
			var encoded []byte
			var typeFromRow string
			if err := rows.Scan(&mark.id, &encoded, &typeFromRow); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s anchor: %w", target.table, err)
			}
			if err := model.DecodeAnchor(encoded, &mark.anchor); err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode %s anchor: %w", target.table, err)
			}
			mark.markType = typeFromRow
			marks = append(marks, mark)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list %s anchors: %w", target.table, err)
		}
		rows.Close()
	}
	return marks, nil
}

func commentMarkType(table string) string {
	if table == "comments" {
		return `case when suggestion is null then 'proofComment' else 'proofSuggestion' end`
	}
	return `'dispatchAsk'`
}

// changedAnchor is an open anchor whose quote or orphaned flag its current tree mark changes.
// comment is whether its row is a comment's (a comment or a suggestion) rather than an ask's.
type changedAnchor struct {
	mark      anchoredMark
	refreshed model.Anchor
	comment   bool
}

// refreshedAnchors is the changed anchors of one table as the columns its update writes, one
// entry per anchor, in the order found.
type refreshedAnchors struct {
	table    string
	ids      []string
	quotes   []string
	orphaned []bool
}

// refreshAnchors updates each open anchor from its current tree mark. Each
// persisted change appends the affected row's own full refresh event in this
// transaction; callers publish the collected committed events afterward.
//
// Every changed anchor is found before any is written, and then all of them are written
// together: one update of each table, one reload of the refreshed asks and one of the refreshed
// comments, and one AppendAll of their events. The transaction holds its owner's row lock, which
// every event writer of that owner waits for (events.Broker), so the round trips spent under it
// stay the same however many anchors changed.
func (s *Service) refreshAnchors(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node, actor model.Actor) error {
	anchors, err := s.openAnchoredMarks(ctx, tx, artifactID)
	if err != nil {
		return err
	}
	skippedMarkID, skipping := ctx.Value(skippedAnchorRefreshKey{}).(string)
	skipped := func(mark anchoredMark) bool { return skipping && mark.anchor.MarkID == skippedMarkID }
	// One walk finds every anchor: a walk per row would make each write quadratic in a document
	// carrying many anchors. Rows sharing a key look the same result up, each refreshed in turn.
	keys := make([]pmdoc.MarkRef, 0, len(anchors))
	for _, mark := range anchors {
		if !skipped(mark) {
			keys = append(keys, mark.markRef())
		}
	}
	found := pmdoc.FindMarks(tree, keys)
	var changed []changedAnchor
	for _, mark := range anchors {
		if skipped(mark) {
			continue
		}
		refreshed := mark.anchor
		if hit, ok := found[mark.markRef()]; ok {
			refreshed.Quote = hit.Quote
			refreshed.Orphaned = false
		} else {
			refreshed.Orphaned = true
		}
		if refreshed == mark.anchor {
			continue
		}
		comment := mark.markType == string(MarkComment) || mark.markType == string(MarkSuggestion)
		changed = append(changed, changedAnchor{mark: mark, refreshed: refreshed, comment: comment})
	}
	if len(changed) == 0 {
		return nil
	}
	changedAsks, changedComments := refreshedAnchors{table: "asks"}, refreshedAnchors{table: "comments"}
	for _, anchor := range changed {
		into := &changedAsks
		if anchor.comment {
			into = &changedComments
		}
		into.ids = append(into.ids, anchor.mark.id)
		into.quotes = append(into.quotes, anchor.refreshed.Quote)
		into.orphaned = append(into.orphaned, anchor.refreshed.Orphaned)
	}
	for _, update := range []refreshedAnchors{changedAsks, changedComments} {
		if len(update.ids) == 0 {
			continue
		}
		// Only the two fields this refresh owns are written. The rows were read before the
		// events and lookups below, so writing the whole anchor column back would erase what
		// another writer - the one-time block-id backfill - put in it in between (LEGION-149).
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			update %s as target
			set anchor = jsonb_set(
				jsonb_set(target.anchor, '{quote}', to_jsonb(refreshed.quote)),
				'{orphaned}', to_jsonb(refreshed.orphaned))
			from unnest($1::uuid[], $2::text[], $3::boolean[]) as refreshed(id, quote, orphaned)
			where target.id = refreshed.id
		`, update.table), update.ids, update.quotes, update.orphaned); err != nil {
			return fmt.Errorf("update %s anchor: %w", update.table, err)
		}
	}
	asks, err := loadAnchorRefreshedAsks(ctx, tx, changedAsks.ids)
	if err != nil {
		return err
	}
	comments, err := loadAnchorRefreshedCommentPayloads(ctx, tx, changedComments.ids)
	if err != nil {
		return err
	}
	refreshEvents := make([]model.Event, 0, len(changed))
	for _, anchor := range changed {
		if !anchor.comment {
			ask := asks[anchor.mark.id]
			// Re-anchoring moves the mark, never the question text, so it cites nothing new.
			refreshEvents = append(refreshEvents, model.Event{
				IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
				Type:    "ask.anchor_refreshed",
				Actor:   actor,
				Payload: model.NewAskEventPayload(ask, model.ReferenceChanges{}),
			})
			continue
		}
		payload := comments[anchor.mark.id]
		refreshEvents = append(refreshEvents, model.Event{
			IssueKey: payload.IssueKey, ArtifactID: payload.ArtifactID,
			Type: "comment.anchor_refreshed", Actor: actor, Payload: payload,
		})
	}
	appended, err := s.events.AppendAll(ctx, tx, refreshEvents)
	if err != nil {
		return fmt.Errorf("append anchor refresh event: %w", err)
	}
	for _, event := range appended {
		collectEvent(ctx, event)
	}
	return nil
}

// loadAnchorRefreshedAsks reads each ask of ids as its refresh event carries it, by id, in one
// read of the rows and one of their opened events. Every id must name an ask.
func loadAnchorRefreshedAsks(ctx context.Context, tx pgx.Tx, ids []string) (map[string]model.Ask, error) {
	asks := make(map[string]model.Ask, len(ids))
	if len(ids) == 0 {
		return asks, nil
	}
	rows, err := tx.Query(ctx, `
		select `+AskColumns+`, ba.id::text, ba.slug, ba.is_primary,
			aa.project_key, aa.slug, aa.name, aa.is_primary
		from asks a
		left join artifacts ba on ba.id = a.block_artifact_id
		left join artifacts aa on aa.id = `+store.CastableUUID(`a.anchor->>'artifact_id'`)+`
		where a.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("load refreshed asks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var blockArtifactID, blockArtifactSlug *string
		var blockArtifactPrimary *bool
		var anchorProject, anchorSlug, anchorName *string
		var anchorPrimary *bool
		ask, err := ScanAsk(rows, &blockArtifactID, &blockArtifactSlug, &blockArtifactPrimary, &anchorProject, &anchorSlug, &anchorName, &anchorPrimary)
		if err != nil {
			return nil, fmt.Errorf("load refreshed asks: %w", err)
		}
		if blockArtifactID != nil {
			ask.BlockArtifact = &model.AskBlockArtifact{
				ID: *blockArtifactID, Slug: *blockArtifactSlug, Primary: *blockArtifactPrimary,
			}
		}
		if anchorProject != nil {
			ask.AnchorArtifact = &model.AskAnchorArtifact{
				Project: *anchorProject, Slug: *anchorSlug, Name: *anchorName, Primary: *anchorPrimary,
			}
		}
		asks[ask.ID] = ask
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load refreshed asks: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		if _, found := asks[id]; !found {
			return nil, fmt.Errorf("load refreshed ask %q: %w", id, pgx.ErrNoRows)
		}
	}
	openedEventIDs, err := events.OpenedEventIDs(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("load refreshed asks' opened events: %w", err)
	}
	for _, id := range ids {
		openedEventID, ok := openedEventIDs[id]
		if !ok {
			return nil, fmt.Errorf("load refreshed ask %q opened event: ask has no event", id)
		}
		ask := asks[id]
		ask.OpenedEventID = &openedEventID
		asks[id] = ask
	}
	return asks, nil
}

// loadAnchorRefreshedCommentPayloads reads each comment of ids as its refresh event carries it,
// with the name and place of the artifact its anchor is on, by id: one read each of the rows with
// their anchors' artifacts, their mentions and their deliveries. Every id must name an anchored
// comment.
func loadAnchorRefreshedCommentPayloads(ctx context.Context, tx pgx.Tx, ids []string) (map[string]model.CommentEventPayload, error) {
	payloads := make(map[string]model.CommentEventPayload, len(ids))
	if len(ids) == 0 {
		return payloads, nil
	}
	comments, places, err := loadAnchorRefreshedComments(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if comments[id].Anchor == nil {
			return nil, fmt.Errorf("refreshed comment %q has no anchor", id)
		}
	}
	for _, id := range ids {
		place, found := places[id]
		if !found {
			return nil, fmt.Errorf("load refreshed comment %q artifact: %w", id, pgx.ErrNoRows)
		}
		payloads[id] = model.CommentEventPayload{
			Comment: comments[id], ArtifactName: place.name, ProjectKey: place.projectKey, ArtifactSlug: place.slug,
		}
	}
	return payloads, nil
}

// anchorArtifactPlace is the name and place of the artifact a comment's anchor is on.
type anchorArtifactPlace struct{ name, projectKey, slug string }

// loadAnchorRefreshedComments reads each comment of ids with its mentions and deliveries, by id,
// and the place of the artifact each anchored comment's anchor is on, by comment id, for the
// comments whose anchor names an artifact that exists. Every id must name a comment.
func loadAnchorRefreshedComments(ctx context.Context, tx pgx.Tx, ids []string) (map[string]model.Comment, map[string]anchorArtifactPlace, error) {
	comments := make(map[string]model.Comment, len(ids))
	places := make(map[string]anchorArtifactPlace, len(ids))
	rows, err := tx.Query(ctx, `
		select c.id::text, c.issue_key, c.artifact_id::text, c.author, c.body, c.anchor, c.reply_to::text, c.ask_id::text, c.turn, c.resolved, c.resolved_by, c.resolved_at, c.edited_at, c.suggestion, c.created_at,
			aa.name, coalesce(aa.project_key, ''), aa.slug
		from comments c
		left join artifacts aa on aa.id = `+store.CastableUUID(`c.anchor->>'artifact_id'`)+`
		where c.id = any($1::uuid[])
	`, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("load refreshed comments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var comment model.Comment
		var author, anchor, resolvedBy, suggestion []byte
		var resolvedAt, editedAt *time.Time
		var anchorName, anchorSlug *string
		var anchorProject string
		if err := rows.Scan(
			&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.AskID, &comment.Turn, &comment.Resolved,
			&resolvedBy, &resolvedAt, &editedAt, &suggestion, &comment.CreatedAt,
			&anchorName, &anchorProject, &anchorSlug,
		); err != nil {
			return nil, nil, fmt.Errorf("load refreshed comments: %w", err)
		}
		id := comment.ID
		if err := json.Unmarshal(author, &comment.Author); err != nil {
			return nil, nil, fmt.Errorf("decode refreshed comment %q author: %w", id, err)
		}
		if len(anchor) > 0 {
			value := model.Anchor{}
			if err := model.DecodeAnchor(anchor, &value); err != nil {
				return nil, nil, fmt.Errorf("decode refreshed comment %q anchor: %w", id, err)
			}
			comment.Anchor = &value
		}
		if len(resolvedBy) > 0 {
			value := model.Actor{}
			if err := json.Unmarshal(resolvedBy, &value); err != nil {
				return nil, nil, fmt.Errorf("decode refreshed comment %q resolver: %w", id, err)
			}
			comment.ResolvedBy = &value
		}
		comment.ResolvedAt = askTimestamp(resolvedAt)
		comment.EditedAt = askTimestamp(editedAt)
		if len(suggestion) > 0 {
			value := model.Suggestion{}
			if err := json.Unmarshal(suggestion, &value); err != nil {
				return nil, nil, fmt.Errorf("decode refreshed comment %q suggestion: %w", id, err)
			}
			comment.Suggestion = &value
		}
		comment.Mentions = []model.Mention{}
		comment.Deliveries = []model.CommentDelivery{}
		comments[id] = comment
		if anchorName != nil {
			places[id] = anchorArtifactPlace{name: *anchorName, projectKey: anchorProject, slug: *anchorSlug}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("load refreshed comments: %w", err)
	}
	rows.Close()
	for _, id := range ids {
		if _, found := comments[id]; !found {
			return nil, nil, fmt.Errorf("load refreshed comment %q: %w", id, pgx.ErrNoRows)
		}
	}
	if err := loadAnchorRefreshedCommentMentions(ctx, tx, ids, comments); err != nil {
		return nil, nil, err
	}
	if err := loadAnchorRefreshedCommentDeliveries(ctx, tx, ids, comments); err != nil {
		return nil, nil, err
	}
	return comments, places, nil
}

// loadAnchorRefreshedCommentMentions appends each mention of the comments of ids to its comment
// in comments, in target order.
func loadAnchorRefreshedCommentMentions(ctx context.Context, tx pgx.Tx, ids []string, comments map[string]model.Comment) error {
	rows, err := tx.Query(ctx, `
		select comment_id::text, target, delivery, resolved_session_id
		from comment_mentions
		where comment_id = any($1::uuid[])
		order by comment_id, target
	`, ids)
	if err != nil {
		return fmt.Errorf("load refreshed comments' mentions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var commentID string
		mention := model.Mention{}
		if err := rows.Scan(&commentID, &mention.Target, &mention.Delivery, &mention.SessionID); err != nil {
			return fmt.Errorf("scan refreshed comment mention: %w", err)
		}
		comment := comments[commentID]
		comment.Mentions = append(comment.Mentions, mention)
		comments[commentID] = comment
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate refreshed comments' mentions: %w", err)
	}
	return nil
}

// loadAnchorRefreshedCommentDeliveries appends each delivery of the comments of ids to its
// comment in comments, in target and attempt order.
func loadAnchorRefreshedCommentDeliveries(ctx context.Context, tx pgx.Tx, ids []string, comments map[string]model.Comment) error {
	rows, err := tx.Query(ctx, `
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, duplicate, state, error, resolve_error, reply_id::text, created_at
		from comment_deliveries
		where comment_id = any($1::uuid[])
		order by comment_id, target, attempt
	`, ids)
	if err != nil {
		return fmt.Errorf("load refreshed comments' deliveries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		delivery := model.CommentDelivery{}
		if err := rows.Scan(
			&delivery.CommentID, &delivery.Target, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID,
			&delivery.EnvelopeID, &delivery.Duplicate, &delivery.State, &delivery.Error, &delivery.ResolveError,
			&delivery.ReplyID, &delivery.CreatedAt,
		); err != nil {
			return fmt.Errorf("scan refreshed comment delivery: %w", err)
		}
		comment := comments[delivery.CommentID]
		comment.Deliveries = append(comment.Deliveries, delivery)
		comments[delivery.CommentID] = comment
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate refreshed comments' deliveries: %w", err)
	}
	return nil
}
