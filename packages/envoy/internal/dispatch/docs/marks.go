package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// MarkSpec identifies a mark and the actor that created it.
type MarkSpec struct {
	Kind MarkKind
	ID   string
	By   model.Actor
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

// ActorRef returns the Proof actor reference for an actor.
func ActorRef(actor model.Actor) string {
	return actor.Kind + ":" + actor.ID
}

func (m MarkSpec) pmMark() pmdoc.Mark {
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
			_, markErr = markQuoteInTxn(txn, fragment, tree, quote, occurrence, mark)
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
func markQuoteInTxn(txn *crdt.Transaction, fragment *crdt.YXmlFragment, doc *pmdoc.Node, quote string, occurrence *int, spec MarkSpec) (pmdoc.Range, error) {
	range_, err := pmdoc.FindQuote(doc, quote, occurrence, nil)
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

// AcceptSuggestion applies the replacement for a suggestion mark.
func (s *Service) AcceptSuggestion(ctx context.Context, artifactID, id, replaceWith string, actor model.Actor) error {
	return s.applySuggestion(ctx, artifactID, id, replaceWith, actor, true)
}

// RejectSuggestion removes a suggestion mark and its inserted text when necessary.
func (s *Service) RejectSuggestion(ctx context.Context, artifactID, id string, actor model.Actor) error {
	return s.applySuggestion(ctx, artifactID, id, "", actor, false)
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
