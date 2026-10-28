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
	err := s.applyLive(ctx, artifactID, mark.By, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := startTree(current, doc)
		if err != nil {
			return err
		}

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
// room looked up again once that Apply returned can have been evicted in between.
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
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, current *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		fragment := doc.GetXmlFragment(fragmentName)
		// Read before the Yjs transaction opens: the state vector takes the document lock.
		since := authoredClock(ctx, artifactID, doc)
		tree, err := startTree(current, doc)
		if err != nil {
			return err
		}
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
		next, err := pmdoc.Splice(tree, range_, replacement)
		if err != nil {
			return acceptSpliceRefusal(err)
		}
		// The browser editor's table plugin pads a table the accept cut to its width, as it does
		// after a reject, before anything reads the accept back, where the browser's accept writes
		// the replacement where Splice does (padsLikeTheBrowser).
		if padsLikeTheBrowser(tree, range_, at, inline) {
			if next, err = padCutTables(tree, next, range_, budget); err != nil {
				return acceptSpliceRefusal(err)
			}
		}
		if code {
			if err := refuseAcceptedCodeThatReshapes(tree, next, range_, at, with); err != nil {
				return err
			}
		}
		if next, err = settleAccepted(tree, next, range_, with); err != nil {
			return err
		}
		if err := refuseCodePipeInTableCell(tree, next, range_, "replace_with", "replace_with", with, "reject the suggestion, or reply asking for text the cell can hold"); err != nil {
			return err
		}
		if !insideAsk(at) {
			if err := refuseUnreadableAccept(tree, next, range_, at, with, replacement); err != nil {
				return err
			}
			if err := refuseReshapedAccept(tree, next, range_, at, with, replacement); err != nil {
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
	return s.applyLive(ctx, artifactID, actor, func(doc *crdt.Doc, _ *pmdoc.Node, transact func(func(*crdt.Transaction))) error {
		marks := doc.GetMap(marksMapName)
		transact(func(txn *crdt.Transaction) {
			marks.Set(txn, markID, plain)
		})
		return nil
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

// changedAnchor is an open anchor whose quote or orphaned flag its current tree mark changes.
type changedAnchor struct {
	mark      anchoredMark
	refreshed model.Anchor
}

// anchorTable is the table of mark's row: comments for a comment or suggestion, asks otherwise.
func anchorTable(mark anchoredMark) string {
	if mark.markType == string(MarkComment) || mark.markType == string(MarkSuggestion) {
		return "comments"
	}
	return "asks"
}

// refreshAnchors updates each open anchor from its current tree mark. Each
// persisted change appends the affected row's own full refresh event in this
// transaction; callers publish the collected committed events afterward.
//
// Every changed anchor is found before any is written, and then all of them are written
// together: one update of each table, one reload of the refreshed asks and one of the refreshed
// comments, and one AppendAll of their events. Once the first event is appended the transaction
// holds the commit-order lock every event writer on the database waits for (events.Broker), so
// the round trips spent under it stay the same however many anchors changed.
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
		changed = append(changed, changedAnchor{mark: mark, refreshed: refreshed})
	}
	if len(changed) == 0 {
		return nil
	}
	ids := map[string][]string{}
	quotes := map[string][]string{}
	orphaned := map[string][]bool{}
	for _, anchor := range changed {
		table := anchorTable(anchor.mark)
		ids[table] = append(ids[table], anchor.mark.id)
		quotes[table] = append(quotes[table], anchor.refreshed.Quote)
		orphaned[table] = append(orphaned[table], anchor.refreshed.Orphaned)
	}
	for _, table := range []string{"asks", "comments"} {
		if len(ids[table]) == 0 {
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
		`, table), ids[table], quotes[table], orphaned[table]); err != nil {
			return fmt.Errorf("update %s anchor: %w", table, err)
		}
	}
	asks, err := loadAnchorRefreshedAsks(ctx, tx, ids["asks"])
	if err != nil {
		return err
	}
	comments, err := loadAnchorRefreshedCommentPayloads(ctx, tx, ids["comments"])
	if err != nil {
		return err
	}
	refreshEvents := make([]model.Event, 0, len(changed))
	for _, anchor := range changed {
		if anchorTable(anchor.mark) == "asks" {
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
		left join artifacts aa on aa.id = (a.anchor->>'artifact_id')::uuid
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
		left join artifacts aa on aa.id = (c.anchor->>'artifact_id')::uuid
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
			if err := json.Unmarshal(anchor, &value); err != nil {
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
	now := s.now()
	seen := make(map[pmdoc.MarkRef]struct{})
	var expired []pmdoc.MarkRef
	var next time.Duration
	for _, mark := range pmdoc.ListMarks(tree) {
		if mark.Type != string(MarkAsk) && mark.Type != string(MarkComment) && mark.Type != string(MarkSuggestion) {
			continue
		}
		seen[mark] = struct{}{}
		if _, found := recordedRefs[mark]; found {
			state := s.lockState(room)
			delete(state.unrecorded, mark)
			s.unlockState(room, state)
			continue
		}
		state := s.lockState(room)
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
		s.unlockState(room, state)
	}
	state := s.lockState(room)
	for mark := range state.unrecorded {
		if _, found := seen[mark]; !found {
			delete(state.unrecorded, mark)
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
