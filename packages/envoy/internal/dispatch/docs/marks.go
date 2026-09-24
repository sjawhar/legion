package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// MarkKind identifies the Proof or Dispatch mark type written into a document tree.
type MarkKind string

const (
	MarkAsk        MarkKind = "dispatchAsk"
	MarkComment    MarkKind = "proofComment"
	MarkSuggestion MarkKind = "proofSuggestion"
)

type MarkSpec struct {
	Kind  MarkKind
	ID    string
	By    model.Actor
	Attrs pmdoc.Attrs
}

// MarkReply is a reply projected to Proof's stored-mark format.
type MarkReply struct {
	By   string `json:"by"`
	Text string `json:"text"`
	At   string `json:"at"`
}

// MarkRecord is the server-owned projection of a comment or suggestion for Proof.
type MarkRecord struct {
	Kind      string      `json:"kind"`
	By        string      `json:"by"`
	CreatedAt string      `json:"createdAt"`
	Text      string      `json:"text"`
	Resolved  bool        `json:"resolved"`
	Replies   []MarkReply `json:"replies"`
	Content   string      `json:"content,omitempty"`
	Status    string      `json:"status,omitempty"`
}

var (
	// ErrAnchorMissing reports a mark that did not appear before verification timed out.
	ErrAnchorMissing = errors.New("anchor mark is not in the document")
	// ErrAnchorOrphaned reports a suggestion mark that no longer exists.
	ErrAnchorOrphaned = errors.New("anchor mark no longer exists")
)

type skippedAnchorRefreshKey struct{}

// ActorRef returns the Proof actor reference for an actor.
func ActorRef(actor model.Actor) string {
	return actor.Kind + ":" + actor.ID
}

func (m MarkSpec) pmMark() pmdoc.Mark {
	if m.Attrs != nil {
		return pmdoc.Mark{Type: string(m.Kind), Attrs: m.Attrs}
	}
	attrs := pmdoc.Attrs{"id": m.ID, "by": ActorRef(m.By)}
	if m.Kind == MarkSuggestion {
		attrs["kind"] = "replace"
	}
	return pmdoc.Mark{Type: string(m.Kind), Attrs: attrs}
}

// Anchored is what an inline mark anchors to: the text it covers and the stable block that
// contains its full range — empty when the range spans top-level siblings, which have only
// the document root in common.
type Anchored struct {
	Quote   string
	BlockID string
}

// MarkQuote marks one matching quote and returns what the new mark anchors to.
func (s *Service) MarkQuote(ctx context.Context, artifactID string, mark MarkSpec, quote string, occurrence *int) (Anchored, error) {
	var anchored Anchored
	err := s.applyLive(ctx, artifactID, mark.By, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			return false, err
		}

		var markErr error
		transact(func(txn *crdt.Transaction) {
			_, markErr = markQuoteInTxn(txn, fragment, tree, quote, occurrence, nil, mark)
		})
		if markErr != nil {
			return false, markErr
		}

		tree, err = treeOf(doc)
		if err != nil {
			return false, err
		}
		anchored, err = readAnchored(tree, mark.Kind, mark.ID)
		if errors.Is(err, ErrAnchorMissing) {
			return false, fmt.Errorf("%w: written mark %q is missing", ErrDocSchema, mark.ID)
		}
		if err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return Anchored{}, err
	}
	return anchored, nil
}

// readAnchored reads a persisted mark's covered text and containing block from tree;
// ErrAnchorMissing when the mark is not there.
func readAnchored(tree *pmdoc.Node, kind MarkKind, id string) (Anchored, error) {
	r, quote, found := pmdoc.FindMark(tree, string(kind), id)
	if !found {
		return Anchored{}, ErrAnchorMissing
	}
	blockID, err := pmdoc.BlockIDForRange(tree, r)
	if err != nil {
		return Anchored{}, err
	}
	return Anchored{Quote: quote, BlockID: blockID}, nil
}

// markQuoteInTxn finds quote in doc and writes the supplied mark within txn.
func markQuoteInTxn(txn *crdt.Transaction, fragment *crdt.YXmlFragment, doc *pmdoc.Node, quote string, occurrence, near *int, spec MarkSpec) (pmdoc.Range, error) {
	range_, err := pmdoc.FindQuote(doc, quote, occurrence, near)
	if err != nil {
		return pmdoc.Range{}, err
	}
	if err := pmdoc.MarkRange(txn, fragment, range_, spec.pmMark()); err != nil {
		return pmdoc.Range{}, err
	}
	return range_, nil
}

// VerifyMark returns what a browser-written mark anchors to, now or after its next document
// update.
func (s *Service) VerifyMark(ctx context.Context, artifactID string, kind MarkKind, id string) (Anchored, error) {
	if err := s.srv.Apply(ctx, artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return Anchored{}, err
	}
	doc := s.srv.GetDoc(artifactID)
	if doc == nil {
		return Anchored{}, errors.New("warm live document did not retain room")
	}

	updates := make(chan struct{}, 1)
	unsubscribe := doc.OnUpdate(func(_ []byte, _ any) {
		select {
		case updates <- struct{}{}:
		default:
		}
	})
	defer unsubscribe()

	timer := time.NewTimer(s.markWait)
	defer timer.Stop()
	for {
		var tree *pmdoc.Node
		var readErr error
		err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
			tree, readErr = treeOf(doc)
		})
		if readErr != nil {
			return Anchored{}, readErr
		}
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return Anchored{}, err
		}
		anchored, err := readAnchored(tree, kind, id)
		if err == nil {
			return anchored, nil
		}
		if !errors.Is(err, ErrAnchorMissing) {
			return Anchored{}, err
		}

		select {
		case <-updates:
		case <-timer.C:
			return Anchored{}, ErrAnchorMissing
		case <-ctx.Done():
			return Anchored{}, ctx.Err()
		}
	}
}

// BlockForQuote returns the stable block that contains the one matching quote.
// A quote spanning top-level siblings has no block identity.
func (s *Service) BlockForQuote(ctx context.Context, artifactID, quote string) (string, error) {
	var blockID string
	var blockErr error
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		tree, err := treeOf(doc)
		if err != nil {
			blockErr = err
			return
		}
		r, err := pmdoc.FindQuote(tree, quote, nil, nil)
		if err != nil {
			blockErr = err
			return
		}
		blockID, blockErr = pmdoc.BlockIDForRange(tree, r)
	})
	if blockErr != nil {
		return "", blockErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return "", err
	}
	return blockID, nil
}

// SuggestionKind returns the kind recorded on a verified browser or server suggestion mark.
func (s *Service) SuggestionKind(ctx context.Context, artifactID, id string) (string, error) {
	var kind string
	var markErr error
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		tree, readErr := treeOf(doc)
		if readErr != nil {
			markErr = readErr
			return
		}
		attrs, found := pmdoc.MarkAttrs(tree, string(MarkSuggestion), id)
		if !found {
			markErr = ErrAnchorMissing
			return
		}
		kind, markErr = suggestionKind(attrs, id)
	})
	if markErr != nil {
		return "", markErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return "", err
	}
	return kind, nil
}

// AcceptSuggestion applies the replacement for a suggestion mark.
func (s *Service) AcceptSuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor) error {
	return s.applySuggestion(context.WithValue(ctx, skippedAnchorRefreshKey{}, id), artifactID, id, replaceWith, actor, true)
}

// RejectSuggestion removes a suggestion mark and its inserted text when necessary.
func (s *Service) RejectSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) error {
	return s.applySuggestion(context.WithValue(ctx, skippedAnchorRefreshKey{}, id), artifactID, id, "", actor, false)
}

func (s *Service) applySuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor, accept bool) error {
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			return false, err
		}
		range_, _, ok := pmdoc.FindMark(tree, string(MarkSuggestion), id)
		if !ok {
			return false, ErrAnchorOrphaned
		}
		attrs, ok := pmdoc.MarkAttrs(tree, string(MarkSuggestion), id)
		if !ok {
			return false, fmt.Errorf("%w: suggestion mark %q has no attributes", ErrDocSchema, id)
		}
		kind, err := suggestionKind(attrs, id)
		if err != nil {
			return false, err
		}

		splice := (accept && (kind == "replace" || kind == "delete")) || (!accept && kind == "insert")
		if !splice {
			var unmarkErr error
			transact(func(txn *crdt.Transaction) {
				unmarkErr = pmdoc.Unmark(txn, fragment, string(MarkSuggestion), id)
			})
			if unmarkErr != nil {
				return false, unmarkErr
			}
			return true, nil
		}

		with := replaceWith
		if !accept {
			with = ""
		}
		replacement, err := inlineAware(with)
		if err != nil {
			return false, err
		}
		next, err := pmdoc.Splice(tree, range_, replacement)
		if err != nil {
			return false, err
		}
		s.recordActor(artifactID, actor)
		var updateErr error
		transact(func(txn *crdt.Transaction) {
			updateErr = pmdoc.Update(txn, fragment, next)
		})
		if updateErr != nil {
			return false, updateErr
		}
		return true, nil
	})
}

func suggestionKind(attrs pmdoc.Attrs, id string) (string, error) {
	value, ok := attrs["kind"]
	if !ok {
		return "replace", nil
	}
	kind, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: suggestion mark %q kind is %T", ErrDocSchema, id, value)
	}
	switch kind {
	case "replace", "delete", "insert":
		return kind, nil
	default:
		return "", fmt.Errorf("%w: suggestion mark %q has kind %q", ErrDocSchema, id, kind)
	}
}

// ProjectMark writes a comment or suggestion record for Proof's margin projection.
func (s *Service) ProjectMark(ctx context.Context, artifactID, markID string, record MarkRecord, actor model.Actor) error {
	plain, err := toPlain(record)
	if err != nil {
		return err
	}
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
		marks := doc.GetMap(marksMapName)
		transact(func(txn *crdt.Transaction) {
			marks.Set(txn, markID, plain)
		})
		return true, nil
	})
}

func toPlain(record MarkRecord) (map[string]any, error) {
	if record.Replies == nil {
		record.Replies = []MarkReply{}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	var plain map[string]any
	if err := json.Unmarshal(encoded, &plain); err != nil {
		return nil, err
	}
	return plain, nil
}

type anchorQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type anchoredMark struct {
	id       string
	markType string
	anchor   model.Anchor
}

func (s *Service) openAnchoredMarks(ctx context.Context, artifactID string) ([]anchoredMark, error) {
	var source anchorQueryer = s.store.Pool
	if tx, ok := txFromContext(ctx); ok {
		source = tx
	}
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
			if err := json.Unmarshal(encoded, &mark.anchor); err != nil {
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

// refreshAnchors updates each open anchor from its current tree mark. Each
// persisted change appends the affected row's own full refresh event in this
// transaction; callers publish the collected committed events afterward.
func (s *Service) refreshAnchors(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node, actor model.Actor) error {
	anchors, err := s.openAnchoredMarks(WithTx(ctx, tx), artifactID)
	if err != nil {
		return err
	}
	skippedMarkID, skipping := ctx.Value(skippedAnchorRefreshKey{}).(string)
	for _, mark := range anchors {
		if skipping && mark.anchor.MarkID == skippedMarkID {
			continue
		}
		refreshed := mark.anchor
		if _, quote, found := pmdoc.FindMark(tree, mark.markType, mark.anchor.MarkID); found {
			refreshed.Quote = quote
			refreshed.Orphaned = false
		} else {
			refreshed.Orphaned = true
		}
		if refreshed == mark.anchor {
			continue
		}
		encoded, err := json.Marshal(refreshed)
		if err != nil {
			return fmt.Errorf("encode anchor: %w", err)
		}
		table := "asks"
		if mark.markType == string(MarkComment) || mark.markType == string(MarkSuggestion) {
			table = "comments"
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`update %s set anchor = $2 where id = $1`, table), mark.id, encoded); err != nil {
			return fmt.Errorf("update %s anchor: %w", table, err)
		}
		event, err := s.anchorRefreshEvent(ctx, tx, mark, actor)
		if err != nil {
			return err
		}
		event, err = s.events.Append(ctx, tx, event)
		if err != nil {
			return fmt.Errorf("append anchor refresh event: %w", err)
		}
		collectEvent(ctx, event)
	}
	return nil
}

func (s *Service) anchorRefreshEvent(ctx context.Context, tx pgx.Tx, mark anchoredMark, actor model.Actor) (model.Event, error) {
	if mark.markType == string(MarkAsk) {
		ask, err := loadAnchorRefreshedAsk(ctx, tx, mark.id)
		if err != nil {
			return model.Event{}, err
		}
		// Re-anchoring moves the mark, never the question text, so it cites nothing new.
		return model.Event{
			IssueKey: ask.IssueKey, ArtifactID: ask.ArtifactID,
			Type:    "ask.anchor_refreshed",
			Actor:   actor,
			Payload: model.NewAskEventPayload(ask, model.ReferenceChanges{}),
		}, nil
	}
	payload, err := loadAnchorRefreshedCommentPayload(ctx, tx, mark.id)
	if err != nil {
		return model.Event{}, err
	}
	return model.Event{
		IssueKey: payload.IssueKey, ArtifactID: payload.ArtifactID,
		Type: "comment.anchor_refreshed", Actor: actor, Payload: payload,
	}, nil
}

func loadAnchorRefreshedAsk(ctx context.Context, tx pgx.Tx, id string) (model.Ask, error) {
	var blockArtifactID, blockArtifactSlug *string
	var blockArtifactPrimary *bool
	var anchorProject, anchorSlug, anchorName *string
	var anchorPrimary *bool
	ask, err := ScanAsk(tx.QueryRow(ctx, `
		select `+AskColumns+`, ba.id::text, ba.slug, ba.is_primary,
			aa.project_key, aa.slug, aa.name, aa.is_primary
		from asks a
		left join artifacts ba on ba.id = a.block_artifact_id
		left join artifacts aa on aa.id = (a.anchor->>'artifact_id')::uuid
		where a.id = $1
	`, id), &blockArtifactID, &blockArtifactSlug, &blockArtifactPrimary, &anchorProject, &anchorSlug, &anchorName, &anchorPrimary)
	if err != nil {
		return model.Ask{}, fmt.Errorf("load refreshed ask %q: %w", id, err)
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
	openedEventIDs, err := events.OpenedEventIDs(ctx, tx, []string{ask.ID})
	if err != nil {
		return model.Ask{}, fmt.Errorf("load refreshed ask %q opened event: %w", id, err)
	}
	openedEventID, ok := openedEventIDs[ask.ID]
	if !ok {
		return model.Ask{}, fmt.Errorf("load refreshed ask %q opened event: ask has no event", id)
	}
	ask.OpenedEventID = &openedEventID
	return ask, nil
}

func loadAnchorRefreshedCommentPayload(ctx context.Context, tx pgx.Tx, id string) (model.CommentEventPayload, error) {
	comment, err := loadAnchorRefreshedComment(ctx, tx, id)
	if err != nil {
		return model.CommentEventPayload{}, err
	}
	if comment.Anchor == nil {
		return model.CommentEventPayload{}, fmt.Errorf("refreshed comment %q has no anchor", id)
	}
	var payload model.CommentEventPayload
	if err := tx.QueryRow(ctx, `
		select name, project_key, slug
		from artifacts where id = $1
	`, comment.Anchor.ArtifactID).Scan(&payload.ArtifactName, &payload.ProjectKey, &payload.ArtifactSlug); err != nil {
		return model.CommentEventPayload{}, fmt.Errorf("load refreshed comment %q artifact: %w", id, err)
	}
	payload.Comment = comment
	return payload, nil
}

func loadAnchorRefreshedComment(ctx context.Context, tx pgx.Tx, id string) (model.Comment, error) {
	var comment model.Comment
	var author, anchor, resolvedBy, suggestion []byte
	var resolvedAt, editedAt *time.Time
	if err := tx.QueryRow(ctx, `
		select id::text, issue_key, artifact_id::text, author, body, anchor, reply_to::text, ask_id::text, turn, resolved, resolved_by, resolved_at, edited_at, suggestion, created_at
		from comments where id = $1
	`, id).Scan(
		&comment.ID, &comment.IssueKey, &comment.ArtifactID, &author, &comment.Body, &anchor, &comment.ReplyTo, &comment.AskID, &comment.Turn, &comment.Resolved,
		&resolvedBy, &resolvedAt, &editedAt, &suggestion, &comment.CreatedAt,
	); err != nil {
		return model.Comment{}, fmt.Errorf("load refreshed comment %q: %w", id, err)
	}
	if err := json.Unmarshal(author, &comment.Author); err != nil {
		return model.Comment{}, fmt.Errorf("decode refreshed comment %q author: %w", id, err)
	}
	if len(anchor) > 0 {
		value := model.Anchor{}
		if err := json.Unmarshal(anchor, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode refreshed comment %q anchor: %w", id, err)
		}
		comment.Anchor = &value
	}
	if len(resolvedBy) > 0 {
		value := model.Actor{}
		if err := json.Unmarshal(resolvedBy, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode refreshed comment %q resolver: %w", id, err)
		}
		comment.ResolvedBy = &value
	}
	if resolvedAt != nil {
		value := resolvedAt.UTC().Format(time.RFC3339Nano)
		comment.ResolvedAt = &value
	}
	if editedAt != nil {
		value := editedAt.UTC().Format(time.RFC3339Nano)
		comment.EditedAt = &value
	}
	if len(suggestion) > 0 {
		value := model.Suggestion{}
		if err := json.Unmarshal(suggestion, &value); err != nil {
			return model.Comment{}, fmt.Errorf("decode refreshed comment %q suggestion: %w", id, err)
		}
		comment.Suggestion = &value
	}
	mentions, err := loadAnchorRefreshedCommentMentions(ctx, tx, id)
	if err != nil {
		return model.Comment{}, err
	}
	deliveries, err := loadAnchorRefreshedCommentDeliveries(ctx, tx, id)
	if err != nil {
		return model.Comment{}, err
	}
	comment.Mentions = mentions
	comment.Deliveries = deliveries
	return comment, nil
}

func loadAnchorRefreshedCommentMentions(ctx context.Context, tx pgx.Tx, id string) ([]model.Mention, error) {
	rows, err := tx.Query(ctx, `
		select target, delivery, resolved_session_id
		from comment_mentions
		where comment_id = $1
		order by target
	`, id)
	if err != nil {
		return nil, fmt.Errorf("load refreshed comment %q mentions: %w", id, err)
	}
	defer rows.Close()
	mentions := []model.Mention{}
	for rows.Next() {
		mention := model.Mention{}
		if err := rows.Scan(&mention.Target, &mention.Delivery, &mention.SessionID); err != nil {
			return nil, fmt.Errorf("scan refreshed comment %q mention: %w", id, err)
		}
		mentions = append(mentions, mention)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate refreshed comment %q mentions: %w", id, err)
	}
	return mentions, nil
}

func loadAnchorRefreshedCommentDeliveries(ctx context.Context, tx pgx.Tx, id string) ([]model.CommentDelivery, error) {
	rows, err := tx.Query(ctx, `
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, state, error, resolve_error, reply_id::text, created_at
		from comment_deliveries
		where comment_id = $1
		order by target, attempt
	`, id)
	if err != nil {
		return nil, fmt.Errorf("load refreshed comment %q deliveries: %w", id, err)
	}
	defer rows.Close()
	deliveries := []model.CommentDelivery{}
	for rows.Next() {
		delivery := model.CommentDelivery{}
		if err := rows.Scan(
			&delivery.CommentID, &delivery.Target, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID,
			&delivery.EnvelopeID, &delivery.State, &delivery.Error, &delivery.ResolveError, &delivery.ReplyID, &delivery.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan refreshed comment %q delivery: %w", id, err)
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate refreshed comment %q deliveries: %w", id, err)
	}
	return deliveries, nil
}

func (s *Service) recordedMarkRefs(ctx context.Context, artifactID string) (map[pmdoc.MarkRef]struct{}, error) {
	recorded := make(map[pmdoc.MarkRef]struct{})
	for _, target := range []struct {
		table string
	}{
		{table: "asks"},
		{table: "comments"},
	} {
		rows, err := s.queryFrom(ctx).Query(ctx, fmt.Sprintf(`
			select anchor, %s
			from %s
			where anchor is not null and anchor->>'artifact_id' = $1
		`, commentMarkType(target.table), target.table), artifactID)
		if err != nil {
			return nil, fmt.Errorf("list recorded %s marks: %w", target.table, err)
		}
		for rows.Next() {
			var encoded []byte
			var markType string
			if err := rows.Scan(&encoded, &markType); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan recorded %s mark: %w", target.table, err)
			}
			var anchor model.Anchor
			if err := json.Unmarshal(encoded, &anchor); err != nil {
				rows.Close()
				return nil, fmt.Errorf("decode recorded %s mark: %w", target.table, err)
			}
			if anchor.MarkID != "" {
				recorded[pmdoc.MarkRef{Type: markType, ID: anchor.MarkID}] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list recorded %s marks: %w", target.table, err)
		}
		rows.Close()
	}
	return recorded, nil
}

func (s *Service) sweepUnrecordedMarks(room string, tree *pmdoc.Node) {
	recordedRefs, err := s.recordedMarkRefs(context.Background(), room)
	if err != nil {
		slog.Error("dispatch: list recorded marks for sweep", "room", room, "error", err)
		return
	}
	now := time.Now()
	seen := make(map[pmdoc.MarkRef]struct{})
	var expired []pmdoc.MarkRef
	var next time.Duration
	for _, mark := range pmdoc.ListMarks(tree) {
		if mark.Type != string(MarkAsk) && mark.Type != string(MarkComment) && mark.Type != string(MarkSuggestion) {
			continue
		}
		seen[mark] = struct{}{}
		if _, found := recordedRefs[mark]; found {
			state := s.room(room)
			state.mu.Lock()
			delete(state.unrecorded, mark)
			state.mu.Unlock()
			continue
		}
		state := s.room(room)
		state.mu.Lock()
		firstSeen, found := state.unrecorded[mark]
		if !found {
			firstSeen = now
			state.unrecorded[mark] = firstSeen
		}
		age := now.Sub(firstSeen)
		if age >= s.unrecordedMarkTTL {
			expired = append(expired, mark)
		} else if wait := s.unrecordedMarkTTL - age; next == 0 || wait < next {
			next = wait
		}
		state.mu.Unlock()
	}
	state := s.room(room)
	state.mu.Lock()
	for mark := range state.unrecorded {
		if _, found := seen[mark]; !found {
			delete(state.unrecorded, mark)
		}
	}
	if next > 0 {
		s.scheduleSettleAfterLocked(room, state, next)
	}
	state.mu.Unlock()
	if len(expired) == 0 {
		return
	}

	var sweepErr error
	err = s.srv.Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact, release := s.serviceTransact(transact)
		defer release()
		fragment := doc.GetXmlFragment(fragmentName)
		fresh, readErr := treeOf(doc)
		if readErr != nil {
			sweepErr = readErr
			return
		}
		transact(func(txn *crdt.Transaction) {
			for _, mark := range expired {
				if _, _, found := pmdoc.FindMark(fresh, mark.Type, mark.ID); !found {
					continue
				}
				if err := pmdoc.Unmark(txn, fragment, mark.Type, mark.ID); err != nil {
					sweepErr = err
					return
				}
			}
		})
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		sweepErr = err
	}
	if sweepErr != nil {
		slog.Error("dispatch: sweep unrecorded marks", "room", room, "error", sweepErr)
	}
}
