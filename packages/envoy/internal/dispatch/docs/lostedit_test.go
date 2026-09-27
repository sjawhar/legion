package docs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

const lostEditDocument = "Alpha paragraph.\n\nBravo paragraph.\n"

var lostEditAgent = model.Actor{Kind: "session", ID: "agent"}

// editInFlight is one agent edit held open between the operations and the version render, the
// window a browser's concurrent change merges into (LEGION-269's first window). The browser write
// is driven explicitly rather than raced: the transaction's fork is only brought up to date with
// the room when the version is captured, so a write made at any point before that lands in the
// same merge.
type editInFlight struct {
	service    *Service
	artifactID string
	ctx        context.Context
	tx         pgx.Tx
	ledger     *Ledger
}

func startEditInFlight(t *testing.T, service *Service, artifactID string, ops ...model.EditOp) *editInFlight {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
		t.Fatalf("lock document owner: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)
	if _, err := service.ApplyOps(joined, artifactID, ops, lostEditAgent, nil); err != nil {
		t.Fatalf("apply agent operations: %v", err)
	}
	return &editInFlight{service: service, artifactID: artifactID, ctx: joined, tx: tx, ledger: ledger}
}

// browserWrites applies edit to the room exactly as a connected browser's update does, while the
// agent's transaction is open.
func (e *editInFlight) browserWrites(t *testing.T, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	if err := browserStyleWrite(e.service, e.artifactID, edit, make(chan struct{})); err != nil {
		t.Fatalf("browser write: %v", err)
	}
}

func lostEditService(t *testing.T) (*Service, string) {
	t.Helper()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, lostEditDocument)
	return service, artifactID
}

func blockOwnText(node *pmdoc.Node) string {
	own := ""
	for _, child := range node.Children {
		if child.Type == "text" {
			own += child.Text
		}
	}
	return own
}

// withoutTopLevelBlock removes the top-level block whose own text is text, which is what a
// browser's paragraph deletion writes.
func withoutTopLevelBlock(text string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		out := &pmdoc.Node{Type: tree.Type, Attrs: tree.Attrs}
		for _, child := range tree.Children {
			if blockOwnText(child) == text {
				continue
			}
			out.Children = append(out.Children, child)
		}
		return out
	}
}

// movingTopLevelBlockLast relocates the top-level block whose own text is text to the document's
// end, which pmdoc.Update writes as an in-place rewrite of the elements it passes.
func movingTopLevelBlockLast(text string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		out := &pmdoc.Node{Type: tree.Type, Attrs: tree.Attrs}
		var moved *pmdoc.Node
		for _, child := range tree.Children {
			if blockOwnText(child) == text {
				moved = child
				continue
			}
			out.Children = append(out.Children, child)
		}
		if moved != nil {
			out.Children = append(out.Children, moved)
		}
		return out
	}
}

// rewritingTopLevelBlock replaces the own text of the top-level block whose text is text, which is
// what a keystroke in that paragraph writes.
func rewritingTopLevelBlock(text, want string) func(*pmdoc.Node) *pmdoc.Node {
	return func(tree *pmdoc.Node) *pmdoc.Node {
		out := &pmdoc.Node{Type: tree.Type, Attrs: tree.Attrs}
		for _, child := range tree.Children {
			if blockOwnText(child) != text {
				out.Children = append(out.Children, child)
				continue
			}
			rewritten := &pmdoc.Node{
				Type:     child.Type,
				Attrs:    child.Attrs,
				Children: []*pmdoc.Node{{Type: "text", Text: want}},
			}
			out.Children = append(out.Children, rewritten)
		}
		return out
	}
}

// An edit whose text a concurrent browser change removes before the edit's version is rendered is
// refused rather than reported applied: the merge annihilates the change, the version would record
// the browser's document while naming the agent as its author, and the agent would move on
// believing its edit landed (LEGION-269).
func TestAnEditIsRefusedWhenAConcurrentChangeRemovesItsTextBeforeTheVersion(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*pmdoc.Node) *pmdoc.Node
	}{
		{name: "the browser deletes the paragraph", edit: withoutTopLevelBlock("Alpha paragraph.")},
		{name: "the browser moves the paragraph", edit: movingTopLevelBlockLast("Alpha paragraph.")},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := lostEditService(t)
			browser := model.Actor{Kind: "user", ID: "alice"}
			service.addConnection(artifactID, service.nextConnection.Add(1), browser)

			edit := startEditInFlight(t, service, artifactID, model.EditOp{
				Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited.",
			})
			edit.browserWrites(t, test.edit)

			_, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent)
			var lost *ErrEditLost
			if !errors.As(err, &lost) {
				t.Fatalf("snapshot after the concurrent change = %v, want the edit refused as lost", err)
			}
			if want := []int{0}; !reflect.DeepEqual(lost.Ops, want) {
				t.Fatalf("lost operations = %v, want %v", lost.Ops, want)
			}
			if !reflect.DeepEqual(lost.Participants, []model.Actor{browser}) {
				t.Fatalf("participants = %#v, want the connected browser %v", lost.Participants, browser)
			}
			if !strings.Contains(lost.Error(), "operation 0") {
				t.Fatalf("refusal = %q, want it to name operation 0", lost.Error())
			}

			// The refused transaction leaves nothing: no version, and the document the browser
			// wrote stands.
			edit.ledger.Discard()
			if err := edit.tx.Rollback(context.Background()); err != nil {
				t.Fatalf("roll back the refused edit: %v", err)
			}
			var versions int
			if err := service.store.Pool.QueryRow(context.Background(),
				`select count(*) from artifact_versions where artifact_id = $1`, artifactID,
			).Scan(&versions); err != nil {
				t.Fatalf("count versions: %v", err)
			}
			if versions != 1 {
				t.Fatalf("versions = %d, want only the seeded one: a refused edit writes none", versions)
			}
			live, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read live document: %v", err)
			}
			if strings.Contains(live, "Alpha paragraph edited.") {
				t.Fatalf("live document = %q, want the refused edit absent from it", live)
			}
		})
	}
}

// A concurrent change the merge does not annihilate leaves the edit reported exactly as before:
// text typed into the same paragraph, text removed around the agent's own insertion, and a
// deletion elsewhere in the document all merge with it.
func TestAnEditThatSurvivesAConcurrentChangeStillReportsSuccess(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*pmdoc.Node) *pmdoc.Node
	}{
		{
			name: "a keystroke in the same paragraph",
			edit: rewritingTopLevelBlock("Alpha paragraph.", "Alpha paragraph.!"),
		},
		{
			name: "text removed around the agent's insertion",
			edit: rewritingTopLevelBlock("Alpha paragraph.", "Alpha."),
		},
		{
			name: "a paragraph deleted elsewhere",
			edit: withoutTopLevelBlock("Bravo paragraph."),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := lostEditService(t)
			edit := startEditInFlight(t, service, artifactID, model.EditOp{
				Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited.",
			})
			edit.browserWrites(t, test.edit)

			if _, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent); err != nil {
				t.Fatalf("snapshot a surviving edit: %v", err)
			}
			if err := edit.ledger.commit(context.Background()); err != nil {
				t.Fatalf("commit a surviving edit: %v", err)
			}
			edit.ledger.publish()
			lost, known := edit.ledger.LostOps(artifactID)
			if !known || len(lost) != 0 {
				t.Fatalf("published verdict = %v (known %t), want no operation lost", lost, known)
			}
			live, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read live document: %v", err)
			}
			if !strings.Contains(live, "edited") {
				t.Fatalf("live document = %q, want the agent's insertion in it", live)
			}
		})
	}
}

// An operation whose only change is a removal inserts nothing a concurrent deletion could undo, so
// a browser deleting the same paragraph never refuses it.
func TestADeletingOperationIsNeverReportedLost(t *testing.T) {
	service, artifactID := lostEditService(t)
	edit := startEditInFlight(t, service, artifactID, model.EditOp{
		Op: "delete", Find: "Alpha paragraph.",
	})
	edit.browserWrites(t, withoutTopLevelBlock("Alpha paragraph."))

	if _, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent); err != nil {
		t.Fatalf("snapshot a deleting edit met by the same deletion: %v", err)
	}
	if err := edit.ledger.commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	edit.ledger.publish()
	if lost, known := edit.ledger.LostOps(artifactID); known && len(lost) > 0 {
		t.Fatalf("published verdict = %v, want a removal never reported lost", lost)
	}
}

// A concurrent change that lands after the version is rendered and before the committed write
// reaches the room cannot be rolled back: the update is durable and the version written. The edit
// reports it instead, so the agent learns the live document does not carry its change
// (LEGION-269's second window).
func TestAnEditReportsTextTheRoomLostBetweenTheVersionAndThePublish(t *testing.T) {
	service, artifactID := lostEditService(t)
	edit := startEditInFlight(t, service, artifactID, model.EditOp{
		Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited.",
	})
	version, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent)
	if err != nil {
		t.Fatalf("snapshot the uncontended edit: %v", err)
	}
	if !version.Wrote {
		t.Fatal("snapshot wrote no version, so there is no second window to test")
	}
	if err := edit.ledger.commit(context.Background()); err != nil {
		t.Fatalf("commit the edit: %v", err)
	}
	edit.browserWrites(t, withoutTopLevelBlock("Alpha paragraph."))
	edit.ledger.publish()

	lost, known := edit.ledger.LostOps(artifactID)
	if !known {
		t.Fatal("no published verdict, want the check to have run")
	}
	if want := []int{0}; !reflect.DeepEqual(lost, want) {
		t.Fatalf("published verdict = %v, want %v: the version carries text the live document no longer has", lost, want)
	}
	live, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read live document: %v", err)
	}
	if strings.Contains(live, "Alpha paragraph edited.") {
		t.Fatalf("live document = %q, want the agent's text absent, which is what the verdict reports", live)
	}
}

// Nothing concurrent, nothing lost: the uncontended edit reaches a verdict of its own rather than
// leaving the caller unable to tell survival from an unanswered check.
func TestAnUncontendedEditPublishesAnEmptyLossVerdict(t *testing.T) {
	service, artifactID := lostEditService(t)
	edit := startEditInFlight(t, service, artifactID, model.EditOp{
		Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited.",
	})
	if _, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := edit.ledger.commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	edit.ledger.publish()
	lost, known := edit.ledger.LostOps(artifactID)
	if !known || len(lost) != 0 {
		t.Fatalf("published verdict = %v (known %t), want an empty verdict", lost, known)
	}
}

// Accepting a suggestion writes the document the same way an edit does and loses its text the
// same way. It has no operation index, so its refusal names the suggestion, and a loss in the
// second window reports on the accept rather than on an operation.
func TestAnAcceptedSuggestionIsRefusedWhenAConcurrentChangeRemovesItsText(t *testing.T) {
	for _, test := range []struct {
		name     string
		lateEdit bool
	}{
		{name: "before the version is rendered"},
		{name: "between the version and the publish", lateEdit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := lostEditService(t)
			ctx := context.Background()
			if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{
				Kind: MarkSuggestion, ID: "s1", By: lostEditAgent,
			}, "Alpha", nil); err != nil {
				t.Fatalf("mark the suggestion: %v", err)
			}

			tx, err := service.store.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback(context.Background())
			if _, _, err := lockArtifactOwner(ctx, tx, artifactID); err != nil {
				t.Fatalf("lock owner: %v", err)
			}
			joined, ledger := service.Join(ctx, tx)
			defer ledger.Discard()
			human := model.Actor{Kind: "user", ID: "alice"}
			if err := service.AcceptSuggestion(joined, artifactID, "s1", "Omega", human); err != nil {
				t.Fatalf("accept the suggestion: %v", err)
			}
			deleteTheParagraph := func() {
				if err := browserStyleWrite(service, artifactID, withoutTopLevelBlock("Alpha paragraph."), make(chan struct{})); err != nil {
					t.Fatalf("browser write: %v", err)
				}
			}

			if !test.lateEdit {
				deleteTheParagraph()
				_, err := service.NamedVersion(joined, artifactID, "Accepted suggestion", human)
				var lost *ErrEditLost
				if !errors.As(err, &lost) {
					t.Fatalf("named version after the concurrent change = %v, want the accept refused", err)
				}
				if lost.Suggestion != "s1" {
					t.Fatalf("refusal names suggestion %q, want %q", lost.Suggestion, "s1")
				}
				if len(lost.Ops) != 0 {
					t.Fatalf("refusal names operations %v, want none: an accept has no operation index", lost.Ops)
				}
				return
			}
			if _, err := service.NamedVersion(joined, artifactID, "Accepted suggestion", human); err != nil {
				t.Fatalf("named version: %v", err)
			}
			if err := ledger.commit(context.Background()); err != nil {
				t.Fatalf("commit: %v", err)
			}
			deleteTheParagraph()
			ledger.publish()
			lost, known := ledger.LostOps(artifactID)
			if !known || len(lost) == 0 {
				t.Fatalf("published verdict = %v (known %t), want the accept reported lost", lost, known)
			}
		})
	}
}

// A batch of several operations names the ones that went, not the batch: the refusal is
// all-or-nothing, but the caller has to know which of its operations the document lost.
func TestABatchNamesOnlyTheOperationsWhoseTextWentMissing(t *testing.T) {
	service, artifactID := lostEditService(t)
	edit := startEditInFlight(t, service, artifactID,
		model.EditOp{Op: "replace", Find: "Alpha paragraph.", With: "Alpha paragraph edited."},
		model.EditOp{Op: "replace", Find: "Bravo paragraph.", With: "Bravo paragraph edited."},
	)
	edit.browserWrites(t, withoutTopLevelBlock("Bravo paragraph."))

	_, err := service.SnapshotVersion(edit.ctx, artifactID, lostEditAgent)
	var lost *ErrEditLost
	if !errors.As(err, &lost) {
		t.Fatalf("snapshot = %v, want the batch refused", err)
	}
	if want := []int{1}; !reflect.DeepEqual(lost.Ops, want) {
		t.Fatalf("lost operations = %v, want %v: only the second operation's paragraph went", lost.Ops, want)
	}
}
