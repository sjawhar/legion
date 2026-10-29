package docs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// equivalenceFlow is one route's document calls, run in a joined transaction of its own.
type equivalenceFlow struct {
	name string
	// corrupt gives the document a stored tree outside the schema, written while no room holds
	// it, in place of the browser edit every other flow starts from.
	corrupt func(context.Context, *store.Store, string) error
	// wantErr is the refusal the flow ends in, nil for one that commits.
	wantErr error
	// refreshing names the operations whose anchor refresh must write: the browser edit before
	// the flow moved anchors for them to refresh, so a refresh there that writes nothing tests
	// nothing.
	refreshing []string
	run        func(*flowRun) error
}

// equivalenceFlows are the flows each document is put through: the comment routes that write a
// margin record (api/comment_create.go, api/comment_actions.go), both after an operation of their
// own transaction left a rendering to reuse and as the transaction's first document operation,
// the record bound's refusal on each of those two paths, and a margin write on each stored tree
// outside the schema.
func equivalenceFlows() []equivalenceFlow {
	longBody := strings.Repeat("A margin record past its bound. ", maxMarginRecordBytes/32+8)
	flows := []equivalenceFlow{
		{name: "an anchored comment", refreshing: []string{"MarkQuote"}, run: func(r *flowRun) error {
			return r.anchoredComment("A new comment on this sentence.", false)
		}},
		{name: "an anchored comment beside a browser edit", refreshing: []string{"MarkQuote", "ProjectMark"}, run: func(r *flowRun) error {
			return r.anchoredComment("A new comment written while a browser edits.", true)
		}},
		{name: "a reply", refreshing: []string{"ProjectMark"}, run: func(r *flowRun) error {
			return r.reply("A reply to the seeded comment.")
		}},
		{name: "a resolve", refreshing: []string{"ProjectMark"}, run: (*flowRun).resolve},
		{name: "a reopen", refreshing: []string{"ProjectMark"}, run: (*flowRun).reopen},
		{name: "an accepted suggestion", refreshing: []string{"AcceptSuggestion"}, run: (*flowRun).accept},
		{name: "a reply past the record bound", wantErr: ErrDocumentTooLarge, run: func(r *flowRun) error {
			return r.reply(longBody)
		}},
		{name: "an anchored comment past the record bound", wantErr: ErrDocumentTooLarge, refreshing: []string{"MarkQuote"}, run: func(r *flowRun) error {
			return r.anchoredComment(longBody, false)
		}},
	}
	for _, corruption := range OutsideSchemaCorruptionsForTest {
		flows = append(flows, equivalenceFlow{
			name: "a reply on " + corruption.Name, corrupt: corruption.Corrupt, wantErr: ErrDocSchema,
			run: func(r *flowRun) error { return r.reply("A reply to the seeded comment.") },
		})
	}
	return flows
}

// marginPath is how a margin write came by the rendering it records: reused from the
// transaction's previous operation, or read and rendered from its fork.
type marginPath string

const (
	reusedRendering marginPath = "reused the rendering its transaction kept"
	readRendering   marginPath = "read and rendered the document"
)

// flowRun is one flow on one side: the transaction it runs in and what it observes.
type flowRun struct {
	t            *testing.T
	side         *equivalenceSide
	document     *equivalenceDocument
	ids          flowIDs
	ctx          context.Context
	tx           pgx.Tx
	ledger       *Ledger
	observed     *flowObservation
	marginWrites int
}

// operation runs one document call of the flow, recording its result, the rows its anchor
// refreshes refreshed and where in the write phase its events fall. A transaction's events are
// appended when its write phase runs (Ledger.Commit, INFRA-636), so the events themselves are
// read off the ledger afterwards (flowObservation.readEvents).
func (r *flowRun) operation(name string, call func() (any, error)) error {
	operation := observedOperation{Name: name, from: r.eventBoundary()}
	refreshed := r.side.refreshes.Load()
	result, err := call()
	operation.Result = encodeObserved(result)
	operation.Refreshed = int(r.side.refreshes.Load() - refreshed)
	operation.to = r.eventBoundary()
	r.observed.Operations = append(r.observed.Operations, operation)
	return err
}

// eventBoundary marks how many events the transaction had appended at this point of its write
// phase: a statement of the write phase, queued here, writing nothing, whose callback the batch
// runs in statement order, as the appends' own callbacks run in theirs. Both sides queue one
// before and one after every operation alike. A flow that is refused never runs its write phase,
// so its boundaries stay unreached (-1) and it appended nothing at all.
func (r *flowRun) eventBoundary() *int {
	at := -1
	if err := r.ledger.Queue(func(b *store.WriteBatch) error {
		b.Queue("", `select 1`).Exec(func(pgconn.CommandTag) error {
			at = len(r.ledger.events)
			return nil
		})
		return nil
	}); err != nil {
		r.t.Fatalf("queue an event boundary: %v", err)
	}
	return &at
}

// project writes the margin record as the side does, and holds the margin write to the path the
// flow expects it to take.
func (r *flowRun) project(markID string, record MarkRecord, path marginPath) error {
	return r.operation("ProjectMark", func() (any, error) {
		var kept *pmdoc.Node
		if write := joinedLiveWrite(r.ctx, r.ids.artifact); write != nil {
			kept = write.tree
		}
		err := r.side.write(r.ctx, r.side.service, r.ids.artifact, markID, record, flowActor)
		if err != nil {
			return nil, err
		}
		r.marginWrites++
		if r.side.margin {
			took := readRendering
			if kept != nil && joinedLiveWrite(r.ctx, r.ids.artifact).tree == kept {
				took = reusedRendering
			}
			if took != path {
				r.t.Errorf("the margin write %s, want it to have %s", took, path)
			}
		}
		return nil, nil
	})
}

// anchoredComment is a new anchored comment as the create route writes it: the anchor
// (resolveAnchor's MarkQuote and SnapshotVersion), the comment's row, and its margin record, which
// reuses the rendering MarkQuote kept. With lateEdit a browser edit lands in the room between the
// row and the margin record, so the fork moves and the kept rendering no longer describes it.
func (r *flowRun) anchoredComment(body string, lateEdit bool) error {
	var anchored Anchored
	if err := r.operation("MarkQuote", func() (any, error) {
		var err error
		anchored, err = r.side.service.MarkQuote(r.ctx, r.ids.artifact, MarkSpec{Kind: MarkComment, ID: newCommentID, By: flowActor}, r.document.newQuote, nil)
		return anchored, err
	}); err != nil {
		return err
	}
	var snapshot VersionResult
	if err := r.operation("SnapshotVersion", func() (any, error) {
		var err error
		snapshot, err = r.side.service.SnapshotVersion(r.ctx, r.ids.artifact, flowActor)
		return comparableVersion(snapshot), err
	}); err != nil {
		return err
	}
	anchor := model.Anchor{ArtifactID: r.ids.artifact, MarkID: newCommentID, Version: snapshot.Version.Number, Quote: anchored.Quote}
	if anchored.BlockID != "" {
		anchor.BlockID = new(anchored.BlockID)
	}
	if err := insertComment(r.ctx, r.tx, r.ids, newCommentID, nil, flowActor, body, &anchor, nil, nil, nil, flowAt); err != nil {
		return err
	}
	path := reusedRendering
	if lateEdit {
		r.side.applyBrowserEdit(r.t, r.ids.artifact, r.document.lateEdit, lateEditor)
		path = readRendering
	}
	return r.project(newCommentID, MarkRecord{
		Kind: "comment", By: ActorRef(flowActor), CreatedAt: flowAt.Format(time.RFC3339Nano), Text: body, Replies: []MarkReply{},
	}, path)
}

// reply is a reply to an anchored comment as the create route writes it: the reply's row, then
// the root's margin record with the reply in it, the transaction's first document operation.
func (r *flowRun) reply(body string) error {
	root := r.document.anchors[replyRoot]
	if err := insertComment(r.ctx, r.tx, r.ids, replyCommentID, &root.id, flowActor, body, nil, nil, nil, nil, repliedAt); err != nil {
		return err
	}
	record := root.record()
	record.Replies = []MarkReply{{By: ActorRef(flowActor), Text: body, At: repliedAt.Format(time.RFC3339Nano)}}
	return r.project(root.id, record, readRendering)
}

// resolve is the resolve route on an open anchored comment.
func (r *flowRun) resolve() error {
	target := r.document.anchors[resolveTarget]
	if err := r.setResolved(target.id, true); err != nil {
		return err
	}
	record := target.record()
	record.Resolved = true
	return r.project(target.id, record, readRendering)
}

// reopen is the reopen route on a resolved anchored comment, whose anchor the browser edit moved
// while it was resolved, so only the reopen's refresh brings it up to date.
func (r *flowRun) reopen() error {
	target := r.document.anchors[reopenTarget]
	if err := r.setResolved(target.id, false); err != nil {
		return err
	}
	record := target.record()
	record.Resolved = false
	return r.project(target.id, record, readRendering)
}

func (r *flowRun) setResolved(id string, resolved bool) error {
	if resolved {
		resolver, err := json.Marshal(flowActor)
		if err != nil {
			return err
		}
		_, err = r.tx.Exec(r.ctx, `update comments set resolved = true, resolved_by = $2, resolved_at = $3 where id = $1`, id, resolver, flowAt)
		return err
	}
	_, err := r.tx.Exec(r.ctx, `update comments set resolved = false, resolved_by = null, resolved_at = null where id = $1`, id)
	return err
}

// accept is the accept route on the seeded suggestion: its kind (SuggestionKind), the text change
// (AcceptSuggestion), the row, the margin record, which reuses the rendering the text change kept,
// and the named version the decision produces.
func (r *flowRun) accept() error {
	target := r.document.anchors[suggestionAnchor]
	var kind string
	if err := r.operation("SuggestionKind", func() (any, error) {
		var err error
		kind, err = r.side.service.SuggestionKind(r.ctx, r.ids.artifact, target.id)
		return kind, err
	}); err != nil {
		return err
	}
	if err := r.operation("AcceptSuggestion", func() (any, error) {
		return nil, r.side.service.AcceptSuggestion(r.ctx, r.ids.artifact, target.id, suggestedWording, flowActor)
	}); err != nil {
		return err
	}
	accepted := true
	suggestion, err := json.Marshal(model.Suggestion{ReplaceWith: suggestedWording, Accepted: &accepted})
	if err != nil {
		return err
	}
	resolver, err := json.Marshal(flowActor)
	if err != nil {
		return err
	}
	if _, err := r.tx.Exec(r.ctx, `
		update comments set resolved = true, resolved_by = $2, resolved_at = $3, suggestion = $4
		where id = $1 and suggestion->>'accepted' is null
	`, target.id, resolver, flowAt, suggestion); err != nil {
		return err
	}
	record := target.record()
	record.Kind, record.Resolved, record.Status = kind, true, "accepted"
	if err := r.project(target.id, record, reusedRendering); err != nil {
		return err
	}
	return r.operation("NamedVersion", func() (any, error) {
		named, err := r.side.service.NamedVersion(r.ctx, r.ids.artifact, target.body, flowActor)
		return comparableVersion(named), err
	})
}

// comparableVersion is result without the clock its row took from the database.
func comparableVersion(result VersionResult) VersionResult {
	result.Version.CreatedAt = time.Time{}
	return result
}
