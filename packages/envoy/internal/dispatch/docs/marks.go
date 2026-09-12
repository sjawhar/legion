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

// MarkQuote marks one matching quote and returns the text covered by the new mark.
func (s *Service) MarkQuote(ctx context.Context, artifactID string, mark MarkSpec, quote string, occurrence *int) (string, error) {
	var covered string
	err := s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
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
		var found bool
		_, covered, found = pmdoc.FindMark(tree, string(mark.Kind), mark.ID)
		if !found {
			return false, fmt.Errorf("%w: written mark %q is missing", ErrDocSchema, mark.ID)
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	return covered, nil
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

// VerifyMark returns a browser-written mark now or after its next document update.
func (s *Service) VerifyMark(ctx context.Context, artifactID string, kind MarkKind, id string) (string, error) {
	if err := s.srv.Apply(ctx, artifactID, func(_ *crdt.Doc, _ func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return "", err
	}
	doc := s.srv.GetDoc(artifactID)
	if doc == nil {
		return "", errors.New("warm live document did not retain room")
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
			return "", readErr
		}
		if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
			return "", err
		}
		if _, quote, ok := pmdoc.FindMark(tree, string(kind), id); ok {
			return quote, nil
		}

		select {
		case <-updates:
		case <-timer.C:
			return "", ErrAnchorMissing
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// BlockForMark returns the stable block that contains a persisted inline mark's
// full range. A mark spanning top-level siblings has no block identity.
func (s *Service) BlockForMark(ctx context.Context, artifactID string, kind MarkKind, id string) (string, error) {
	var blockID string
	var markErr error
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		tree, err := treeOf(doc)
		if err != nil {
			markErr = err
			return
		}
		r, _, found := pmdoc.FindMark(tree, string(kind), id)
		if !found {
			markErr = ErrAnchorMissing
			return
		}
		blockID, markErr = pmdoc.BlockIDForRange(tree, r)
	})
	if markErr != nil {
		return "", markErr
	}
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		return "", err
	}
	return blockID, nil
}

// BlockForQuote returns the stable block that contains the one matching quote.
// A quote spanning top-level siblings has no block identity.
func (s *Service) BlockForQuote(ctx context.Context, artifactID, quote string) (string, error) {
	var blockID string
	var quoteErr error
	err := s.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
		tree, err := treeOf(doc)
		if err != nil {
			quoteErr = err
			return
		}
		r, err := pmdoc.FindQuote(tree, quote, nil, nil)
		if err != nil {
			quoteErr = err
			return
		}
		blockID, quoteErr = pmdoc.BlockIDForRange(tree, r)
	})
	if quoteErr != nil {
		return "", quoteErr
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
	return s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
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
func (s *Service) ProjectMark(ctx context.Context, artifactID, markID string, record MarkRecord) error {
	plain, err := toPlain(record)
	if err != nil {
		return err
	}
	return s.applyLive(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) (bool, error) {
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

// refreshAnchors updates each open anchor from its current tree mark.
func (s *Service) refreshAnchors(ctx context.Context, tx pgx.Tx, artifactID string, tree *pmdoc.Node) error {
	anchors, err := s.openAnchoredMarks(WithTx(ctx, tx), artifactID)
	if err != nil {
		return err
	}
	skippedMarkID, skipping := ctx.Value(skippedAnchorRefreshKey{}).(string)
	for _, mark := range anchors {
		if skipping && mark.anchor.MarkID == skippedMarkID {
			continue
		}
		if _, quote, found := pmdoc.FindMark(tree, mark.markType, mark.anchor.MarkID); found {
			mark.anchor.Quote = quote
			mark.anchor.Orphaned = false
		} else {
			mark.anchor.Orphaned = true
		}
		encoded, err := json.Marshal(mark.anchor)
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
		rows, err := s.store.Pool.Query(ctx, fmt.Sprintf(`
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
