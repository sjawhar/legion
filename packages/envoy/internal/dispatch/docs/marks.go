package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
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
	err := s.applyLive(ctx, artifactID, mark.By, func(doc *crdt.Doc, current pmdoc.ValidDocument, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		valid, err := startTree(current, doc)
		if err != nil {
			return err
		}
		tree := valid.Node()

		var markErr error
		transact(func(txn *crdt.Transaction) {
			_, markErr = markQuoteInTxn(txn, fragment, tree, quote, occurrence, nil, mark)
		})
		return markErr
	})
	if err != nil {
		return Anchored{}, err
	}
	// applyLive keeps the tree it read from the fork after this callback as the write's rendering,
	// so the anchor is read off it rather than off a second read of the fork.
	anchored, err := readAnchored(ledgerFrom(ctx).writtenRendering(artifactID).tree, mark.Kind, mark.ID)
	if errors.Is(err, ErrAnchorMissing) {
		return Anchored{}, fmt.Errorf("%w: written mark %q is missing", ErrDocSchema, mark.ID)
	}
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
		return pmdoc.Range{}, documentSchemaError(err)
	}
	return range_, nil
}

// VerifyMark returns what a browser-written mark anchors to, now or after its next document
// update. It subscribes to the room's updates inside the Apply that loads and holds the room: a
// room looked up again once that Apply returned can have been evicted in between. The wait ends
// with one last read rather than on the subscription alone, so a mark written into a room
// instance that replaced the subscribed one - which another process's eviction can leave behind -
// is still found.
func (s *Service) VerifyMark(ctx context.Context, artifactID string, kind MarkKind, id string) (Anchored, error) {
	updates := make(chan struct{}, 1)
	var unsubscribe func()
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		if s.afterReadWarm != nil {
			s.afterReadWarm(artifactID)
		}
		unsubscribe = doc.OnUpdate(func(_ []byte, _ any) {
			select {
			case updates <- struct{}{}:
			default:
			}
		})
	})
	if unsubscribe != nil {
		defer unsubscribe()
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return Anchored{}, err
	}

	timer := time.NewTimer(s.markWait)
	defer timer.Stop()
	final := false
	for {
		tree, err := s.docTree(ctx, artifactID)
		if err != nil {
			return Anchored{}, err
		}
		anchored, err := readAnchored(tree, kind, id)
		if err == nil {
			return anchored, nil
		}
		if !errors.Is(err, ErrAnchorMissing) {
			return Anchored{}, err
		}
		if final {
			return Anchored{}, ErrAnchorMissing
		}

		select {
		case <-updates:
		case <-timer.C:
			final = true
		case <-ctx.Done():
			return Anchored{}, ctx.Err()
		}
	}
}

// BlockForQuote returns the stable block that contains the one matching quote.
// A quote spanning top-level siblings has no block identity.
func (s *Service) BlockForQuote(ctx context.Context, artifactID, quote string) (string, error) {
	tree, err := s.docTree(ctx, artifactID)
	if err != nil {
		return "", err
	}
	r, err := pmdoc.FindQuote(tree, quote, nil, nil)
	if err != nil {
		return "", err
	}
	return pmdoc.BlockIDForRange(tree, r)
}

// SuggestionKind returns the kind recorded on a verified browser or server suggestion mark.
func (s *Service) SuggestionKind(ctx context.Context, artifactID, id string) (string, error) {
	tree, err := s.docTree(ctx, artifactID)
	if err != nil {
		return "", err
	}
	attrs, found := pmdoc.MarkAttrs(tree, string(MarkSuggestion), id)
	if !found {
		return "", ErrAnchorMissing
	}
	return suggestionKind(attrs, id)
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
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current pmdoc.ValidDocument, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		// Read before the Yjs transaction opens: the state vector takes the document lock.
		since := authoredClock(ctx, artifactID, doc)
		valid, err := startTree(current, doc)
		if err != nil {
			return err
		}
		tree := valid.Node()
		range_, _, ok := pmdoc.FindMark(tree, string(MarkSuggestion), id)
		if !ok {
			return ErrAnchorOrphaned
		}
		attrs, ok := pmdoc.MarkAttrs(tree, string(MarkSuggestion), id)
		if !ok {
			return fmt.Errorf("%w: suggestion mark %q has no attributes", ErrDocSchema, id)
		}
		kind, err := suggestionKind(attrs, id)
		if err != nil {
			return err
		}

		splice := (accept && (kind == "replace" || kind == "delete")) || (!accept && kind == "insert")
		if !splice {
			var unmarkErr error
			transact(func(txn *crdt.Transaction) {
				unmarkErr = documentSchemaError(pmdoc.Unmark(txn, fragment, string(MarkSuggestion), id))
			})
			if unmarkErr != nil {
				return unmarkErr
			}
			return nil
		}

		// An accept is one implicit operation, so every run its own update inserts is its own and
		// no tree diff tells them apart: recordInsertedText reads the blocks it inserted into and
		// this hands them straight back (editBatch.writes, LEGION-269). It stamps no block ids -
		// a replacement's blocks are minted by pmdoc.Parse already, and EnsureBlockIDs would also
		// repair a repeat the live document carries, which is settlement's to repair and a
		// write's to leave as it found it (TestWritesBesideALiveRepeatedBlockIDAreTaken). Text
		// left in a block with no id at all - the far half of a split, which no caller can
		// address either - is simply not tracked, so the check under-reports there rather than
		// naming a block nobody can name.
		write := func(next *pmdoc.Node) error {
			written := editBatch{tree: next, operations: 1}
			return recordInsertedText(ctx, artifactID, id, fragment, since, written.writes, func() error {
				var updateErr error
				transact(func(txn *crdt.Transaction) {
					updateErr = pmdoc.Update(txn, fragment, next)
				})
				return updateErr
			})
		}
		// A reject is not checked: it removes the text a browser insert added, which gives back
		// the document the insert started from.
		if !accept {
			next, err := rejectedInsert(tree, id)
			if err != nil {
				return err
			}
			return write(next)
		}

		// A suggestion's text reaches the document with line feeds alone (pmdoc.LineFeeds). It is
		// stored so when the suggestion is created, but one created before that holds its text as
		// sent.
		with := pmdoc.LineFeeds(replaceWith)
		// The replacement's markdown and the tables its splice cuts are this accept's to pad.
		budget := pmdoc.NewWriteBudget()
		at, _ := pmdoc.ContainingTextblock(tree, range_.From)
		code := at.Node.Type == "code_block"
		var replacement *pmdoc.Node
		if code {
			replacement = codeReplacement(acceptedCode(tree, with, at, range_))
		} else if replacement, err = inlineAware(with, edgesOf(at, range_), opensDocument(tree, range_.From), budget); err != nil {
			return err
		}
		// Accepted text stays inside every ask and comment anchor the suggestion lay wholly inside,
		// as an edit's replacement does, and the suggestion's own mark goes with the text it
		// replaced. Only text that stays in the textblock carries them: a block replacement can
		// land a code block, which an ask's mark cannot cover.
		inline := code || isInlineDocument(replacement)
		if inline {
			pmdoc.AddMarks(replacement, slices.DeleteFunc(pmdoc.AnchorMarksCovering(tree, range_), func(mark pmdoc.Mark) bool {
				return mark.Type == string(MarkSuggestion) && mark.Attrs["id"] == id
			}))
		}
		next, err := valid.Splice(range_, replacement)
		if err != nil {
			return acceptSpliceRefusal(err)
		}
		// Where the suggestion lies in the document the accept started from, which every refusal
		// below reads against the tree it holds then (matchedBlocks).
		found, err := matchedBlocks(valid, range_)
		if err != nil {
			return err
		}
		// The browser editor's table plugin pads a table the accept cut to its width, as it does
		// after a reject, before anything reads the accept back, where the browser's accept writes
		// the replacement where Splice does (padsLikeTheBrowser).
		if padsLikeTheBrowser(tree, range_, at, inline) {
			if next, err = padCutTables(next, found, budget); err != nil {
				return acceptSpliceRefusal(err)
			}
		}
		if code {
			if err := refuseAcceptedCodeThatReshapes(valid, next, found, at, with); err != nil {
				return err
			}
		}
		next = settleAccepted(valid, next, found, with)
		if err := refuseCodePipeInTableCell(valid, next, found, "replace_with", "replace_with", with, "reject the suggestion, or reply asking for text the cell can hold"); err != nil {
			return err
		}
		if !insideAsk(at) {
			if err := refuseUnreadableAccept(valid, next, found, range_, at, with, replacement); err != nil {
				return err
			}
			if err := refuseReshapedAccept(valid, next, found, range_, at, with, replacement); err != nil {
				return err
			}
		}
		if err := pmdoc.RepeatedBlockID(tree, next, replacement); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidMarkdown, err)
		}
		if err := refuseBrokenAsks(tree, next); err != nil {
			return err
		}
		if err := refuseMisreadAccept(tree, next, with); err != nil {
			return err
		}
		return write(next)
	})
}

// refuseBrokenAsks refuses the first ask a write left unreadable whose id the document could read
// before it, with settlement's reason (the one the edit route's ApplyOps gives), so a replacement
// an ask cannot hold, such as a question given a code block, writes nothing. An id the document
// already held unreadable, malformed or repeated, is not the write's to refuse: a browser edit can
// leave one and an upload can carry it on, settlement flags it, and a write elsewhere must not fail
// over it. An id that gains an ask is refused whatever it held, since settlement's id repair keeps
// the id for the first ask in document order and would hand the held ask's row and answer to it.
func refuseBrokenAsks(before, after *pmdoc.Node) error {
	unreadable, held := map[string]bool{}, map[string]int{}
	askReadability(before, func(id string, reason error) bool {
		unreadable[id] = unreadable[id] || reason != nil
		held[id]++
		return true
	})
	var refusal error
	written := map[string]int{}
	askReadability(after, func(id string, reason error) bool {
		written[id]++
		if reason != nil && (!unreadable[id] || written[id] > held[id]) {
			refusal = reason
		}
		return refusal == nil
	})
	if refusal != nil {
		return &ErrInvalidAskBlock{Reason: refusal}
	}
	return nil
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
	return s.applyMargin(ctx, artifactID, actor, markID, plain)
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

type anchoredMark struct {
	id       string
	markType string
	anchor   model.Anchor
}

// markRef is the mark identity the row anchors to.
func (m anchoredMark) markRef() pmdoc.MarkRef {
	return pmdoc.MarkRef{Type: m.markType, ID: m.anchor.MarkID}
}

// openAnchoredMarksQuery is the statement openAnchoredMarks runs over target's table: every row
// anchored on the document bound to $1 whose mark is still open, oldest first. The open filter
// is the target's own (anchorTarget.open), so a table added to anchorTargets brings the
// predicate that holds its rows open with it.
func openAnchoredMarksQuery(target anchorTarget) string {
	return fmt.Sprintf(`
			select id::text, anchor, %s
			from %s
			where %s and %s
			order by created_at, id
		`, commentMarkType(target.table), target.table, target.anchoredOn(), target.open)
}

// openAnchoredMarks lists the open anchored asks, then the unresolved anchored comments, each by
// creation time and id, so a refresh announces its rows, and an upload re-anchors them, in the same
// order every time it runs over the same rows.
func (s *Service) openAnchoredMarks(ctx context.Context, source Queryer, artifactID string) ([]anchoredMark, error) {
	var marks []anchoredMark
	for _, target := range anchorTargets {
		rows, err := source.Query(ctx, openAnchoredMarksQuery(target), artifactID)
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

// refreshAnchors updates each open anchor from its current tree mark. Each
// persisted change appends the affected row's own full refresh event in this
// transaction; callers publish the collected committed events afterward.
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
		table := "asks"
		if mark.markType == string(MarkComment) || mark.markType == string(MarkSuggestion) {
			table = "comments"
		}
		// Only the two fields this refresh owns are written. The row was read before the
		// events and lookups below, so writing the whole anchor column back would erase what
		// another writer - the one-time block-id backfill - put in it in between (LEGION-149).
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			update %s
			set anchor = jsonb_set(
				jsonb_set(anchor, '{quote}', to_jsonb($2::text)),
				'{orphaned}', to_jsonb($3::boolean))
			where id = $1
		`, table), mark.id, refreshed.Quote, refreshed.Orphaned); err != nil {
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
		left join artifacts aa on aa.id = `+store.CastableUUID(`a.anchor->>'artifact_id'`)+`
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
		select name, coalesce(project_key, ''), slug
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
		if err := model.DecodeAnchor(anchor, &value); err != nil {
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
		select comment_id::text, target, attempt, delivery, session_id, envelope_id, duplicate, state, error, resolve_error, reply_id::text, created_at
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
			&delivery.EnvelopeID, &delivery.Duplicate, &delivery.State, &delivery.Error, &delivery.ResolveError,
			&delivery.ReplyID, &delivery.CreatedAt,
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

// anchorTarget is one table a document's anchored rows live in: its name, the SQL expression
// that reads an anchor's artifact_id as a uuid there, the anchor index that carries exactly
// that expression, and the predicate that holds one of its rows open. The expression differs
// per table because the index does: an ask's is store.CanonicalUUID's guarded cast, written out
// in migration 0148, a comment's the plain cast of comments_anchor_artifact (0032). The open
// predicate differs because the state does: an ask carries a state column, a comment a resolved
// flag.
type anchorTarget struct {
	table      string
	artifactID string
	index      string
	open       string
}

// asksAnchorTarget and commentsAnchorTarget are the two tables every anchor read of this package
// finds a document's rows in. Each table's facts are written once, here: the uuid expression,
// which must stay byte-identical to the one its index is built on, that index's name, which the
// plan tests hold each read to, and the predicate that holds a row of the table open. A read
// names its target instead of spelling any of them again.
var (
	asksAnchorTarget     = anchorTarget{table: "asks", artifactID: store.CanonicalUUID(`anchor->>'artifact_id'`), index: "asks_anchor_artifact_id", open: "state = 'open'"}
	commentsAnchorTarget = anchorTarget{table: "comments", artifactID: `(anchor->>'artifact_id')::uuid`, index: "comments_anchor_artifact", open: "not resolved"}

	// anchorTargets are both, in the order the reads that read both tables read them.
	anchorTargets = []anchorTarget{asksAnchorTarget, commentsAnchorTarget}
)

// anchoredOn is the predicate that finds the rows of the target's table anchored on the document
// bound to $1: `anchor is not null`, the text comparison the anchor reads of the release before
// migration 20261101182419 ran, and the uuid comparison this table's anchor index is built on,
// in that order.
//
// `anchor is not null` is the partial predicate of asks_anchor_artifact_id (0148), which Postgres
// cannot prove from a clause on anchor->>'artifact_id' alone, so without it that index is not
// eligible at all; ->> is strict, so a row either comparison matches carries a non-null anchor
// already and no read loses a row to it.
//
// The text comparison keeps the row set exactly the one that release returned, and it is what
// docs/write_hold_test.go's counter sees a refresh pass by, so a read that drops it both widens
// its answer and goes uncounted. A row whose artifact_id is not a lower-case document id - an
// upper-case spelling of a real id, or, on asks, a value that is no uuid at all - is left out of
// asks_anchor_artifact_id by migration 0148's guard and was never matched by the text comparison
// either, while the uuid comparison alone would return the upper-case one. On comments the uuid
// side is comments_anchor_artifact's plain (anchor->>'artifact_id')::uuid (0032), which raises
// 22P02 on an artifact_id that is no uuid where the text comparison returns no row; only that
// index's own cast keeps such a row out of comments today, and the text comparison is what holds
// the read to that release's answer whatever the table carries.
//
// The uuid comparison is written out as its index's expression, byte for byte, so the read is an
// index scan rather than a scan of a table holding every ask or comment of the instance. Every
// read that binds $1 is passed a document id the server holds, never a caller's text, so the cast
// of the parameter raises on no input.
func (t anchorTarget) anchoredOn() string {
	return `anchor is not null and anchor->>'artifact_id' = $1 and ` + t.artifactID + ` = $1::uuid`
}

// recordedMarksQuery is the statement that lists the anchor and mark type of every row of
// target's table whose anchor names the artifact $1.
func recordedMarksQuery(target anchorTarget) string {
	return fmt.Sprintf(`
		select anchor, %s
		from %s
		where %s
	`, commentMarkType(target.table), target.table, target.anchoredOn())
}

// recordedMarkRefs is every mark an ask or comment row anchored on artifactID records, read by the
// anchor's artifact alone whatever an ask's block_artifact_id holds: an ask that also names an ask
// block on another document keeps its mark on its anchor's document, as refreshAnchors and the
// other anchor readers read it.
func (s *Service) recordedMarkRefs(ctx context.Context, artifactID string) (map[pmdoc.MarkRef]struct{}, error) {
	recorded := make(map[pmdoc.MarkRef]struct{})
	for _, target := range anchorTargets {
		rows, err := s.queryFrom(ctx).Query(ctx, recordedMarksQuery(target), artifactID)
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
			if err := model.DecodeAnchor(encoded, &anchor); err != nil {
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

// sweepUnrecordedMarks removes every ask, comment or suggestion mark of this tree that no row
// records and that has been unrecorded for unrecordedMarkTTL, counted from the first settlement
// that saw it: the provisional mark a composer the reader abandoned left behind.
//
// A mark whose author still has a browser on the room is the composer still open in front of
// somebody, whose Send would be refused as a missing anchor were the mark taken from under it, so
// it is kept and counted from this sweep instead. Once that browser has gone the mark ages from
// the author's last presence (state.departed), not from the sweep that last saw them: the sweeps
// of a quiet document are a TTL apart, so a drop a second before one - an idle reconnect - would
// otherwise lose the mark a second after it rather than a TTL after it. A tab that closes or goes
// offline therefore loses its stray mark one TTL later, as every other unrecorded mark does.
func (s *Service) sweepUnrecordedMarks(room string, tree *pmdoc.Node) {
	recordedRefs, err := s.recordedMarkRefs(context.Background(), room)
	if err != nil {
		slog.Error("dispatch: list recorded marks for sweep", "room", room, "error", err)
		return
	}
	now := s.now()
	marks := pmdoc.ListMarks(tree)
	seen := make(map[pmdoc.MarkRef]struct{})
	var expired []pmdoc.MarkRef
	var next time.Duration
	state := s.lockState(room)
	// With nothing unrecorded, every mark here is seen for the first time and its author cannot
	// change the answer, so the settlement of a room with no stray mark - the steady state - builds
	// no set and concatenates no reference.
	var authors map[string]struct{}
	if len(state.unrecorded) > 0 {
		authors = connectedMarkAuthors(state.connected)
	}
	for _, mark := range marks {
		if mark.Type != string(MarkAsk) && mark.Type != string(MarkComment) && mark.Type != string(MarkSuggestion) {
			continue
		}
		seen[mark.MarkRef] = struct{}{}
		if _, found := recordedRefs[mark.MarkRef]; found {
			delete(state.unrecorded, mark.MarkRef)
			continue
		}
		firstSeen, seenBefore := state.unrecorded[mark.MarkRef]
		_, writing := authors[mark.By]
		if writing || !seenBefore {
			firstSeen = now
			state.unrecorded[mark.MarkRef] = firstSeen
		}
		since := firstSeen
		// A mark written while its author was already away ages from its own first sighting: the
		// later of the two is when it was last anybody's open composer. A mark with no author
		// matches no departure, as it matches no connected browser.
		if left, away := state.departed[mark.By]; away && left.After(since) {
			since = left
		}
		age := now.Sub(since)
		if age >= s.unrecordedMarkTTL {
			expired = append(expired, mark.MarkRef)
		} else if wait := s.unrecordedMarkTTL - age; next == 0 || wait < next {
			next = wait
		}
	}
	for mark := range state.unrecorded {
		if _, found := seen[mark]; !found {
			delete(state.unrecorded, mark)
		}
	}
	// A departure a TTL old answers nothing this sweep has not already decided: every mark of that
	// author is at least that old too, so the first sighting alone expires it from here on.
	for author, left := range state.departed {
		if now.Sub(left) >= s.unrecordedMarkTTL {
			delete(state.departed, author)
		}
	}
	if next > 0 {
		s.scheduleSettleAfterLocked(room, state, next)
	}
	s.unlockState(room, state)
	if len(expired) == 0 {
		return
	}
	if err := s.unmarkExpired(room, expired); err != nil {
		slog.Error("dispatch: sweep unrecorded marks", "room", room, "error", err)
	}
}

// connectedMarkAuthors is every spelling of a mark's `by` attribute that names a user with a
// browser on the room: the bare login a browser's own mark carries, and the user:<login> of a
// mark the server wrote (ActorRef). It is nil when no such browser is there, which answers every
// mark the same way. An actor with no id names nobody, so a mark with no author matches nothing
// here. Its caller holds state.mu.
func connectedMarkAuthors(connected map[uint64]model.Actor) map[string]struct{} {
	var authors map[string]struct{}
	for _, actor := range connected {
		if actor.Kind != "user" || actor.ID == "" {
			continue
		}
		if authors == nil {
			authors = make(map[string]struct{}, 2*len(connected))
		}
		authors[actor.ID] = struct{}{}
		authors[ActorRef(actor)] = struct{}{}
	}
	return authors
}

// recordMarkAuthorDeparture records in state that actor's last browser left the room at now, from
// when an unrecorded mark of theirs ages (sweepUnrecordedMarks). Another browser of the same
// actor still on the room leaves nothing to record: the composer is still open in front of them.
// Both spellings of a mark's `by` are recorded, the two connectedMarkAuthors answers, so the
// sweep reads a departure with the one lookup it already makes. Its caller holds state.mu.
func recordMarkAuthorDeparture(state *roomState, actor model.Actor, now time.Time) {
	if actor.Kind != "user" || actor.ID == "" {
		return
	}
	for _, other := range state.connected {
		if other.Kind == actor.Kind && other.ID == actor.ID {
			return
		}
	}
	state.departed[actor.ID] = now
	state.departed[ActorRef(actor)] = now
}

// forgetMarkAuthorDeparture takes actor's departure off state: a browser of theirs is on the room
// again, so their marks are open composers once more and age from no earlier absence. Its caller
// holds state.mu.
func forgetMarkAuthorDeparture(state *roomState, actor model.Actor) {
	if actor.Kind != "user" || actor.ID == "" {
		return
	}
	delete(state.departed, actor.ID)
	delete(state.departed, ActorRef(actor))
}

// unmarkExpired removes from room's live document each of expired that it still holds.
func (s *Service) unmarkExpired(room string, expired []pmdoc.MarkRef) error {
	var sweepErr error
	err := s.srv.Apply(context.Background(), room, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		transact, release := s.serviceTransact(transact)
		defer release()
		fragment := doc.GetXmlFragment(fragmentName)
		// The tree is read inside the transaction that unmarks it, under the document's lock: a
		// walk of the live tree outside one takes no lock and can read a peer's write halfway.
		transact(func(txn *crdt.Transaction) {
			fresh, readErr := treeOfTransaction(txn, fragment)
			if readErr != nil {
				sweepErr = readErr
				return
			}
			for _, mark := range expired {
				if _, _, found := pmdoc.FindMark(fresh, mark.Type, mark.ID); !found {
					continue
				}
				if err := pmdoc.Unmark(txn, fragment, mark.Type, mark.ID); err != nil {
					sweepErr = documentSchemaError(err)
					return
				}
			}
		})
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		sweepErr = err
	}
	return sweepErr
}
